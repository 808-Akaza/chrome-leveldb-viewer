package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// decodeSSV turns a Blink SerializedScriptValue (the bytes Chrome stores as an
// IndexedDB record value, after the record-version varint) into a Go value
// that formats as plain text or JSON.
//
// The wire format is Blink's envelope around V8's structured-clone serializer:
//
//	FF <blink version> [FE <trailer offset:u64 BE> <trailer size:u32 BE>]
//	FF <v8 version> <value…>
//
// Blink versions before 16 use a single shared version tag.
func decodeSSV(b []byte) (any, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("empty script value")
	}
	r := &v8Reader{b: b}
	if b[0] == 0xFF {
		r.i = 1
		ver, ok := r.uvarint()
		if !ok {
			return nil, fmt.Errorf("bad blink version")
		}
		r.blink = uint32(ver)
		if r.i < len(r.b) && r.b[r.i] == 0xFE {
			r.i++
			if r.i+12 > len(r.b) {
				return nil, fmt.Errorf("truncated trailer pointer")
			}
			off := binary.BigEndian.Uint64(r.b[r.i : r.i+8])
			size := binary.BigEndian.Uint32(r.b[r.i+8 : r.i+12])
			r.i += 12
			if size > 0 && off <= uint64(len(r.b)) && off >= uint64(r.i) {
				r.b = r.b[:off]
			}
		}
		if ver >= 16 && r.i < len(r.b) && r.b[r.i] == 0xFF {
			r.i++
			v8ver, ok := r.uvarint()
			if !ok {
				return nil, fmt.Errorf("bad v8 version")
			}
			r.version = uint32(v8ver)
		} else {
			r.version = uint32(ver)
		}
	}
	v, err := r.readObject()
	if err != nil {
		return nil, err
	}
	return normalizeValue(v), nil
}

type arrayBuf struct{ data []byte }

type jsArray struct {
	items []any
	props map[string]any
}

type jsList struct{ items []any }

type v8Reader struct {
	b       []byte
	i       int
	version uint32
	blink   uint32
	objs    []any
	depth   int
}

func (r *v8Reader) uvarint() (uint64, bool) {
	var x uint64
	var s uint
	for n := 0; n < 10; n++ {
		if r.i >= len(r.b) {
			return 0, false
		}
		c := r.b[r.i]
		r.i++
		if c < 0x80 {
			if n == 9 && c > 1 {
				return 0, false
			}
			return x | uint64(c)<<s, true
		}
		x |= uint64(c&0x7f) << s
		s += 7
	}
	return 0, false
}

func (r *v8Reader) raw(n int) ([]byte, bool) {
	if n < 0 || r.i+n > len(r.b) {
		return nil, false
	}
	out := r.b[r.i : r.i+n]
	r.i += n
	return out, true
}

func (r *v8Reader) peekTag() (byte, bool) {
	i := r.i
	for i < len(r.b) {
		if r.b[i] != 0 {
			return r.b[i], true
		}
		i++
	}
	return 0, false
}

func (r *v8Reader) readTag() (byte, bool) {
	for r.i < len(r.b) {
		c := r.b[r.i]
		r.i++
		if c != 0 {
			return c, true
		}
	}
	return 0, false
}

func (r *v8Reader) allocID() int {
	id := len(r.objs)
	r.objs = append(r.objs, nil)
	return id
}

func (r *v8Reader) readObject() (any, error) {
	r.depth++
	if r.depth > 80 {
		return nil, fmt.Errorf("value nested too deeply")
	}
	defer func() { r.depth-- }()

	v, err := r.readObjectInternal()
	if err != nil {
		return nil, err
	}
	if buf, ok := v.(*arrayBuf); ok {
		if tag, ok := r.peekTag(); ok && tag == 'V' {
			r.readTag()
			return r.readArrayBufferView(buf)
		}
	}
	return v, nil
}

func (r *v8Reader) readObjectInternal() (any, error) {
	tag, ok := r.readTag()
	if !ok {
		return nil, fmt.Errorf("truncated value")
	}
	switch tag {
	case '?': // kVerifyObjectCount
		if _, ok := r.uvarint(); !ok {
			return nil, fmt.Errorf("bad object count")
		}
		return r.readObject()
	case '_':
		return "undefined", nil
	case '0':
		return nil, nil
	case 'T':
		return true, nil
	case 'F':
		return false, nil
	case '-':
		return nil, nil
	case 'I':
		n, ok := r.uvarint()
		if !ok {
			return nil, fmt.Errorf("bad int")
		}
		z := int64(n >> 1)
		if n&1 != 0 {
			z = ^z
		}
		return z, nil
	case 'U':
		n, ok := r.uvarint()
		if !ok {
			return nil, fmt.Errorf("bad uint")
		}
		return n, nil
	case 'N':
		d, ok := r.float64()
		if !ok {
			return nil, fmt.Errorf("bad double")
		}
		return numberValue(d), nil
	case 'Z':
		return r.readBigInt()
	case 'z':
		id := r.allocID()
		s, err := r.readBigInt()
		if err != nil {
			return nil, err
		}
		r.objs[id] = s
		return s, nil
	case 'S':
		return r.readCountedString(false)
	case '"':
		return r.readCountedBytes()
	case 'c':
		return r.readUTF16LE()
	case '^':
		id, ok := r.uvarint()
		if !ok || id >= uint64(len(r.objs)) {
			return nil, fmt.Errorf("bad object reference")
		}
		return r.objs[id], nil
	case 'o':
		return r.readJSObject()
	case 'a':
		return r.readSparseArray()
	case 'A':
		return r.readDenseArray()
	case 'D':
		return r.readDate()
	case 'y':
		id := r.allocID()
		r.objs[id] = true
		return true, nil
	case 'x':
		id := r.allocID()
		r.objs[id] = false
		return false, nil
	case 'n':
		id := r.allocID()
		d, ok := r.float64()
		if !ok {
			return nil, fmt.Errorf("bad number object")
		}
		v := numberValue(d)
		r.objs[id] = v
		return v, nil
	case 's':
		id := r.allocID()
		s, err := r.readString()
		if err != nil {
			return nil, err
		}
		r.objs[id] = s
		return s, nil
	case 'R':
		return r.readRegExp()
	case ';':
		return r.readMap()
	case '\'':
		return r.readSet()
	case 'B', 'C':
		return r.readArrayBuffer(false)
	case '~':
		return r.readArrayBuffer(true)
	case 'r':
		return r.readError()
	case '\\':
		return r.readHostObject()
	case 't', 'u', 'p', 'w', 'm', 'E':
		// Transfer / shared / wasm handles have no inline bytes we can recover.
		r.allocID()
		if _, ok := r.uvarint(); !ok {
			return nil, fmt.Errorf("bad transfer id")
		}
		return fmt.Sprintf("[%s]", hostTagName(tag)), nil
	default:
		return nil, fmt.Errorf("unsupported value tag 0x%02x", tag)
	}
}

func hostTagName(tag byte) string {
	switch tag {
	case 't':
		return "transferred ArrayBuffer"
	case 'u':
		return "SharedArrayBuffer"
	case 'p':
		return "shared object"
	case 'w':
		return "Wasm module"
	case 'm':
		return "Wasm memory"
	case 'E':
		return "shared immutable ArrayBuffer"
	default:
		return "host object"
	}
}

func (r *v8Reader) readCountedBytes() (string, error) {
	n, ok := r.uvarint()
	if !ok || n > uint64(len(r.b)-r.i) {
		return "", fmt.Errorf("bad string length")
	}
	buf, ok := r.raw(int(n))
	if !ok {
		return "", fmt.Errorf("truncated string")
	}
	return string(buf), nil
}

func (r *v8Reader) readCountedString(utf8 bool) (string, error) {
	s, err := r.readCountedBytes()
	if err != nil {
		return "", err
	}
	if utf8 {
		return strings.ToValidUTF8(s, "\uFFFD"), nil
	}
	return s, nil
}

func (r *v8Reader) readUTF16LE() (string, error) {
	n, ok := r.uvarint()
	if !ok || n%2 != 0 || n > uint64(len(r.b)-r.i) {
		return "", fmt.Errorf("bad utf-16 string")
	}
	buf, ok := r.raw(int(n))
	if !ok {
		return "", fmt.Errorf("truncated utf-16 string")
	}
	u := make([]uint16, len(buf)/2)
	for i := range u {
		u[i] = uint16(buf[2*i]) | uint16(buf[2*i+1])<<8
	}
	return string(utf16.Decode(u)), nil
}

func (r *v8Reader) readString() (string, error) {
	if r.version < 12 {
		return r.readCountedString(true)
	}
	v, err := r.readObject()
	if err != nil {
		return "", err
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("expected string, got %T", v)
	}
	return s, nil
}

func (r *v8Reader) float64() (float64, bool) {
	buf, ok := r.raw(8)
	if !ok {
		return 0, false
	}
	return math.Float64frombits(binary.LittleEndian.Uint64(buf)), true
}

func (r *v8Reader) readBigInt() (string, error) {
	bitfield, ok := r.uvarint()
	if !ok {
		return "", fmt.Errorf("bad bigint")
	}
	n := int(bitfield >> 1)
	neg := bitfield&1 != 0
	buf, ok := r.raw(n)
	if !ok {
		return "", fmt.Errorf("truncated bigint")
	}
	be := make([]byte, len(buf))
	for i := range buf {
		be[len(buf)-1-i] = buf[i]
	}
	z := new(big.Int).SetBytes(be)
	if neg {
		z.Neg(z)
	}
	return z.String(), nil
}

func (r *v8Reader) readJSObject() (any, error) {
	id := r.allocID()
	m := map[string]any{}
	r.objs[id] = m
	n, err := r.readProps(m, '{')
	if err != nil {
		return nil, err
	}
	expect, ok := r.uvarint()
	if !ok || expect != uint64(n) {
		return nil, fmt.Errorf("object property count mismatch")
	}
	return m, nil
}

func (r *v8Reader) readProps(m map[string]any, end byte) (int, error) {
	n := 0
	for {
		tag, ok := r.peekTag()
		if !ok {
			return 0, fmt.Errorf("truncated object")
		}
		if tag == end {
			r.readTag()
			return n, nil
		}
		k, err := r.readObject()
		if err != nil {
			return 0, err
		}
		v, err := r.readObject()
		if err != nil {
			return 0, err
		}
		m[propKey(k)] = v
		n++
		if n > 1_000_000 {
			return 0, fmt.Errorf("object too large")
		}
	}
}

func (r *v8Reader) readDenseArray() (any, error) {
	length, ok := r.uvarint()
	if !ok || length > 5_000_000 || int(length) > len(r.b)-r.i {
		return nil, fmt.Errorf("bad array length")
	}
	id := r.allocID()
	arr := &jsArray{items: make([]any, length)}
	r.objs[id] = arr
	for i := uint64(0); i < length; i++ {
		if tag, ok := r.peekTag(); ok && tag == '-' {
			r.readTag()
			continue
		}
		v, err := r.readObject()
		if err != nil {
			return nil, err
		}
		if r.version < 11 {
			if s, ok := v.(string); ok && s == "undefined" {
				continue
			}
		}
		arr.items[i] = v
	}
	arr.props = map[string]any{}
	n, err := r.readProps(arr.props, '$')
	if err != nil {
		return nil, err
	}
	expectN, ok1 := r.uvarint()
	expectL, ok2 := r.uvarint()
	if !ok1 || !ok2 || expectN != uint64(n) || expectL != length {
		return nil, fmt.Errorf("array trailer mismatch")
	}
	return arr, nil
}

func (r *v8Reader) readSparseArray() (any, error) {
	length, ok := r.uvarint()
	if !ok {
		return nil, fmt.Errorf("bad sparse array")
	}
	id := r.allocID()
	props := map[string]any{}
	r.objs[id] = props
	n, err := r.readProps(props, '@')
	if err != nil {
		return nil, err
	}
	expectN, ok1 := r.uvarint()
	expectL, ok2 := r.uvarint()
	if !ok1 || !ok2 || expectN != uint64(n) || expectL != length {
		return nil, fmt.Errorf("sparse array trailer mismatch")
	}
	props["length"] = length
	return props, nil
}

func (r *v8Reader) readDate() (any, error) {
	d, ok := r.float64()
	if !ok {
		return nil, fmt.Errorf("bad date")
	}
	id := r.allocID()
	var v any = "Invalid Date"
	if !math.IsNaN(d) && !math.IsInf(d, 0) {
		v = formatJSDate(d)
	}
	r.objs[id] = v
	return v, nil
}

func (r *v8Reader) readRegExp() (any, error) {
	id := r.allocID()
	src, err := r.readString()
	if err != nil {
		return nil, err
	}
	flags, ok := r.uvarint()
	if !ok {
		return nil, fmt.Errorf("bad regexp")
	}
	s := fmt.Sprintf("/%s/%s", src, regexpFlags(uint32(flags)))
	r.objs[id] = s
	return s, nil
}

func regexpFlags(f uint32) string {
	var b strings.Builder
	order := []struct {
		bit  uint32
		char byte
	}{{1, 'g'}, {2, 'i'}, {4, 'm'}, {8, 'y'}, {16, 'u'}, {32, 's'}, {128, 'd'}, {256, 'v'}}
	for _, o := range order {
		if f&o.bit != 0 {
			b.WriteByte(o.char)
		}
	}
	return b.String()
}

func (r *v8Reader) readMap() (any, error) {
	id := r.allocID()
	list := &jsList{}
	r.objs[id] = list
	n := 0
	for {
		tag, ok := r.peekTag()
		if !ok {
			return nil, fmt.Errorf("truncated map")
		}
		if tag == ':' {
			r.readTag()
			break
		}
		k, err := r.readObject()
		if err != nil {
			return nil, err
		}
		v, err := r.readObject()
		if err != nil {
			return nil, err
		}
		list.items = append(list.items, map[string]any{"key": k, "value": v})
		n += 2
	}
	expect, ok := r.uvarint()
	if !ok || expect != uint64(n) {
		return nil, fmt.Errorf("map length mismatch")
	}
	return list, nil
}

func (r *v8Reader) readSet() (any, error) {
	id := r.allocID()
	list := &jsList{}
	r.objs[id] = list
	for {
		tag, ok := r.peekTag()
		if !ok {
			return nil, fmt.Errorf("truncated set")
		}
		if tag == ',' {
			r.readTag()
			break
		}
		v, err := r.readObject()
		if err != nil {
			return nil, err
		}
		list.items = append(list.items, v)
	}
	expect, ok := r.uvarint()
	if !ok || expect != uint64(len(list.items)) {
		return nil, fmt.Errorf("set length mismatch")
	}
	return list, nil
}

func (r *v8Reader) readArrayBuffer(resizable bool) (*arrayBuf, error) {
	id := r.allocID()
	n, ok := r.uvarint()
	if !ok {
		return nil, fmt.Errorf("bad array buffer")
	}
	if resizable {
		if _, ok := r.uvarint(); !ok {
			return nil, fmt.Errorf("bad resizable array buffer")
		}
	}
	if n > uint64(len(r.b)-r.i) {
		return nil, fmt.Errorf("truncated array buffer")
	}
	buf, ok := r.raw(int(n))
	if !ok {
		return nil, fmt.Errorf("truncated array buffer")
	}
	ab := &arrayBuf{data: append([]byte(nil), buf...)}
	r.objs[id] = ab
	return ab, nil
}

func (r *v8Reader) readArrayBufferView(buf *arrayBuf) (any, error) {
	tag, ok1 := r.uvarint()
	off, ok2 := r.uvarint()
	n, ok3 := r.uvarint()
	if !ok1 || !ok2 || !ok3 || off > uint64(len(buf.data)) || n > uint64(len(buf.data))-off {
		return nil, fmt.Errorf("bad array buffer view")
	}
	if r.version >= 14 {
		if _, ok := r.uvarint(); !ok {
			return nil, fmt.Errorf("bad array buffer view flags")
		}
	}
	r.allocID()
	slice := buf.data[off : off+n]
	names := map[uint64]string{
		'b': "Int8Array", 'B': "Uint8Array", 'C': "Uint8ClampedArray",
		'w': "Int16Array", 'W': "Uint16Array", 'd': "Int32Array", 'D': "Uint32Array",
		'h': "Float16Array", 'f': "Float32Array", 'F': "Float64Array",
		'q': "BigInt64Array", 'Q': "BigUint64Array", '?': "DataView",
	}
	return map[string]any{
		"typedArray": names[tag],
		"data":       bytesToText(slice),
	}, nil
}

func (r *v8Reader) readError() (any, error) {
	r.allocID()
	out := map[string]any{}
	for {
		if r.i >= len(r.b) {
			return nil, fmt.Errorf("truncated error")
		}
		tag := r.b[r.i]
		r.i++
		switch tag {
		case 'E':
			out["name"] = "EvalError"
		case 'R':
			out["name"] = "RangeError"
		case 'F':
			out["name"] = "ReferenceError"
		case 'S':
			out["name"] = "SyntaxError"
		case 'T':
			out["name"] = "TypeError"
		case 'U':
			out["name"] = "URIError"
		case 'm':
			s, err := r.readString()
			if err != nil {
				return nil, err
			}
			out["message"] = s
		case 's':
			s, err := r.readString()
			if err != nil {
				return nil, err
			}
			out["stack"] = s
		case 'c':
			v, err := r.readObject()
			if err != nil {
				return nil, err
			}
			out["cause"] = v
		case '.':
			if _, ok := out["name"]; !ok {
				out["name"] = "Error"
			}
			return out, nil
		default:
			return nil, fmt.Errorf("bad error subtag 0x%02x", tag)
		}
	}
}

func (r *v8Reader) readHostObject() (any, error) {
	if r.i >= len(r.b) {
		return nil, fmt.Errorf("truncated host object")
	}
	tag := r.b[r.i]
	r.i++
	r.allocID()
	utf8s := func() (string, error) {
		n, ok := r.uvarint()
		if !ok || n > uint64(len(r.b)-r.i) {
			return "", fmt.Errorf("bad host string")
		}
		buf, ok := r.raw(int(n))
		if !ok {
			return "", fmt.Errorf("truncated host string")
		}
		return string(buf), nil
	}
	switch tag {
	case 'b':
		uuid, err := utf8s()
		if err != nil {
			return nil, err
		}
		mime, err := utf8s()
		if err != nil {
			return nil, err
		}
		size, ok := r.uvarint()
		if !ok {
			return nil, fmt.Errorf("bad blob")
		}
		return map[string]any{"blob": mime, "uuid": uuid, "size": size}, nil
	case 'i', 'e', 'G', 'J', 'M', 'h':
		n, ok := r.uvarint()
		if !ok {
			return nil, fmt.Errorf("bad host index")
		}
		return map[string]any{"hostObject": string(tag), "index": n}, nil
	case 'x':
		name, err := utf8s()
		if err != nil {
			return nil, err
		}
		msg, err := utf8s()
		if err != nil {
			return nil, err
		}
		stack, err := utf8s()
		if err != nil {
			return nil, err
		}
		return map[string]any{"DOMException": name, "message": msg, "stack": stack}, nil
	case 'f':
		return r.readFile(utf8s)
	default:
		return nil, fmt.Errorf("unsupported host object %q", tag)
	}
}

func (r *v8Reader) readFile(utf8s func() (string, error)) (any, error) {
	path, err := utf8s()
	if err != nil {
		return nil, err
	}
	out := map[string]any{"file": path}
	if r.blink >= 4 {
		name, err := utf8s()
		if err != nil {
			return nil, err
		}
		rel, err := utf8s()
		if err != nil {
			return nil, err
		}
		out["name"] = name
		out["relativePath"] = rel
	}
	uuid, err := utf8s()
	if err != nil {
		return nil, err
	}
	mime, err := utf8s()
	if err != nil {
		return nil, err
	}
	out["uuid"] = uuid
	out["type"] = mime
	if r.blink >= 4 {
		has, ok := r.uvarint()
		if !ok {
			return nil, fmt.Errorf("bad file snapshot flag")
		}
		if has != 0 {
			size, ok := r.uvarint()
			if !ok {
				return nil, fmt.Errorf("bad file size")
			}
			buf, ok := r.raw(8)
			if !ok {
				return nil, fmt.Errorf("bad file mtime")
			}
			out["size"] = size
			out["lastModified"] = math.Float64frombits(binary.LittleEndian.Uint64(buf))
		}
	}
	if r.blink >= 7 {
		if _, ok := r.uvarint(); !ok {
			return nil, fmt.Errorf("bad file visibility")
		}
	}
	return out, nil
}

func numberValue(d float64) any {
	if math.IsNaN(d) {
		return "NaN"
	}
	if math.IsInf(d, 1) {
		return "Infinity"
	}
	if math.IsInf(d, -1) {
		return "-Infinity"
	}
	if d == math.Trunc(d) && d >= math.MinInt64 && d <= math.MaxInt64 {
		return int64(d)
	}
	return d
}

func propKey(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return "null"
	default:
		b, err := json.Marshal(normalizeValue(t))
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(b)
	}
}

func normalizeValue(v any) any {
	switch t := v.(type) {
	case *arrayBuf:
		return bytesToText(t.data)
	case *jsList:
		out := make([]any, len(t.items))
		for i, item := range t.items {
			out[i] = normalizeValue(item)
		}
		return out
	case *jsArray:
		items := make([]any, len(t.items))
		for i, item := range t.items {
			items[i] = normalizeValue(item)
		}
		if len(t.props) == 0 {
			return items
		}
		props := map[string]any{}
		for k, val := range t.props {
			props[k] = normalizeValue(val)
		}
		return map[string]any{"items": items, "properties": props}
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[k] = normalizeValue(val)
		}
		return m
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = normalizeValue(item)
		}
		return out
	case string:
		if expanded, ok := expandEmbedded(t); ok {
			return expanded
		}
		return t
	default:
		return v
	}
}

func bytesToText(b []byte) any {
	if len(b) == 0 {
		return ""
	}
	if s := string(b); utf8MostlyText(s) {
		if expanded, ok := expandEmbedded(s); ok {
			return expanded
		}
		return s
	}
	if expanded, ok := expandBytes(b); ok {
		return expanded
	}
	return base64.StdEncoding.EncodeToString(b)
}

func utf8MostlyText(s string) bool {
	return utf8.ValidString(s) && printableRatio(s) >= 0.9
}

func formatJSDate(ms float64) string {
	sec := int64(ms / 1000)
	nsec := int64((ms - float64(sec*1000)) * 1e6)
	return time.Unix(sec, nsec).UTC().Format(time.RFC3339Nano)
}
