package main

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
)

// enrichIndexedDB rewrites records that use Chrome's IndexedDB LevelDB coding
// (the format inside an IndexedDB .ldb file) so keys and values show up as
// text instead of hex. Other databases are left untouched.
func enrichIndexedDB(entries []Entry) {
	if len(entries) == 0 {
		return
	}
	recs := make([]*idbRec, len(entries))
	parsed, markers := 0, 0
	for i := range entries {
		rec, ok := parseIDBRecord(entries[i].Key)
		if !ok {
			continue
		}
		recs[i] = &rec
		parsed++
		if rec.kind == "data" || (rec.kind == "global" && (rec.meta == 0 || rec.meta == 201)) {
			markers++
		}
	}
	if markers == 0 || parsed*5 < len(entries)*4 {
		return
	}

	names := collectIDBNames(entries, recs)
	for i := range entries {
		e := &entries[i]
		rec := recs[i]
		if rec == nil {
			if text := readableBytes(e.Key); text != "" {
				e.KeyDisplay, e.KeyEnc = text, "text"
				e.Chrome.KeyDisplay, e.Chrome.KeyEnc = text, "text"
			}
			if text := readableBytes(e.Value); text != "" {
				e.ValText, e.ValEnc = text, "text"
			}
			e.Chrome.Type = "idb-meta"
			continue
		}
		info := rec.describe(names)
		text, enc := decodeIDBValue(*rec, e.Value)
		e.Chrome = info
		e.KeyDisplay, e.KeyEnc = info.KeyDisplay, info.KeyEnc
		e.ValText, e.ValEnc = text, enc
	}
}

type idbRec struct {
	kind       string
	db, os, ix int64
	meta       byte
	osID, ixID int64
	userKey    string
	indexKey   string
	primaryKey string
	origin     string
	dbName     string
	storeName  string
	indexName  string
}

type idbNames struct {
	origin map[int64]string
	db     map[int64]string
	store  map[[2]int64]string
	index  map[[3]int64]string
}

func collectIDBNames(entries []Entry, recs []*idbRec) idbNames {
	n := idbNames{
		origin: map[int64]string{},
		db:     map[int64]string{},
		store:  map[[2]int64]string{},
		index:  map[[3]int64]string{},
	}
	for i, rec := range recs {
		if rec == nil {
			continue
		}
		val := entries[i].Value
		switch rec.kind {
		case "dbname":
			if id, ok := parseCompactInt(val); ok {
				n.db[id] = rec.dbName
				n.origin[id] = prettyOrigin(rec.origin)
			}
		case "dbmeta":
			s, ok := decodeUTF16BE(val)
			if !ok {
				continue
			}
			switch rec.meta {
			case 0:
				n.origin[rec.db] = prettyOrigin(s)
			case 1:
				n.db[rec.db] = s
			}
		case "osmeta":
			if rec.meta == 0 {
				if s, ok := decodeUTF16BE(val); ok {
					n.store[[2]int64{rec.db, rec.osID}] = s
				}
			}
		case "ixmeta":
			if rec.meta == 0 {
				if s, ok := decodeUTF16BE(val); ok {
					n.index[[3]int64{rec.db, rec.osID, rec.ixID}] = s
				}
			}
		case "osnames":
			if id, ok := parseCompactInt(val); ok {
				n.store[[2]int64{rec.db, id}] = rec.storeName
			}
		case "ixnames":
			if id, ok := parseCompactInt(val); ok {
				n.index[[3]int64{rec.db, rec.osID, id}] = rec.indexName
			}
		}
	}
	return n
}

func (rec idbRec) describe(n idbNames) ChromeInfo {
	info := ChromeInfo{Type: "idb-meta", KeyEnc: "indexeddb"}
	origin := n.origin[rec.db]
	dbName := n.db[rec.db]
	if origin == "" && rec.db != 0 {
		origin = fmt.Sprintf("database %d", rec.db)
	}
	info.Origin = origin
	where := joinNonEmpty(" / ", origin, dbName)

	switch rec.kind {
	case "global":
		info.KeyDisplay = globalMetaName(rec.meta)
	case "dbname":
		info.Origin = prettyOrigin(rec.origin)
		info.KeyDisplay = fmt.Sprintf("database %s — %s", prettyOrigin(rec.origin), rec.dbName)
		info.StorageKey = rec.dbName
	case "dbmeta":
		info.KeyDisplay = joinNonEmpty(" / ", where, dbMetaName(rec.meta))
	case "osmeta":
		store := n.store[[2]int64{rec.db, rec.osID}]
		if store == "" {
			store = fmt.Sprintf("store %d", rec.osID)
		}
		info.KeyDisplay = joinNonEmpty(" / ", where, store, osMetaName(rec.meta))
		info.StorageKey = store
	case "ixmeta":
		store := storeLabel(n, rec.db, rec.osID)
		ix := n.index[[3]int64{rec.db, rec.osID, rec.ixID}]
		if ix == "" {
			ix = fmt.Sprintf("index %d", rec.ixID)
		}
		info.KeyDisplay = joinNonEmpty(" / ", where, store, ix, ixMetaName(rec.meta))
	case "osnames":
		info.KeyDisplay = joinNonEmpty(" / ", where, "object store "+rec.storeName)
		info.StorageKey = rec.storeName
	case "ixnames":
		info.KeyDisplay = joinNonEmpty(" / ", where, storeLabel(n, rec.db, rec.osID), "index "+rec.indexName)
		info.StorageKey = rec.indexName
	case "data":
		info.Type = "idb-data"
		store := storeLabel(n, rec.db, rec.os)
		info.KeyDisplay = joinNonEmpty(" / ", where, store, rec.userKey)
		info.StorageKey = unquoteKey(rec.userKey)
	case "exists":
		info.KeyDisplay = joinNonEmpty(" / ", where, storeLabel(n, rec.db, rec.os), rec.userKey, "exists")
		info.StorageKey = unquoteKey(rec.userKey)
	case "blob":
		info.KeyDisplay = joinNonEmpty(" / ", where, storeLabel(n, rec.db, rec.os), rec.userKey, "blob")
		info.StorageKey = unquoteKey(rec.userKey)
	case "index":
		info.Type = "idb-index"
		ix := n.index[[3]int64{rec.db, rec.os, rec.ix}]
		if ix == "" {
			ix = fmt.Sprintf("index %d", rec.ix)
		}
		label := joinNonEmpty(" / ", where, storeLabel(n, rec.db, rec.os), ix, rec.indexKey)
		if rec.primaryKey != "" {
			label += " → " + rec.primaryKey
		}
		info.KeyDisplay = label
		info.StorageKey = unquoteKey(rec.indexKey)
	default:
		info.KeyDisplay = "IndexedDB record"
	}
	return info
}

func storeLabel(n idbNames, db, os int64) string {
	if s := n.store[[2]int64{db, os}]; s != "" {
		return s
	}
	return fmt.Sprintf("store %d", os)
}

func globalMetaName(meta byte) string {
	switch meta {
	case 0:
		return "IndexedDB schema version"
	case 1:
		return "IndexedDB max database id"
	case 2:
		return "IndexedDB data format version"
	case 3:
		return "IndexedDB recovery blob journal"
	case 4:
		return "IndexedDB active blob journal"
	case 5:
		return "IndexedDB earliest sweep time"
	case 6:
		return "IndexedDB earliest compaction time"
	case 100:
		return "IndexedDB database free list"
	default:
		return fmt.Sprintf("IndexedDB global metadata %d", meta)
	}
}

func dbMetaName(meta byte) string {
	switch meta {
	case 0:
		return "origin"
	case 1:
		return "database name"
	case 2:
		return "string version"
	case 3:
		return "max object store id"
	case 4:
		return "database version"
	case 5:
		return "blob number generator"
	default:
		return fmt.Sprintf("database metadata %d", meta)
	}
}

func osMetaName(meta byte) string {
	switch meta {
	case 0:
		return "object store name"
	case 1:
		return "key path"
	case 2:
		return "auto increment"
	case 4:
		return "last version"
	case 5:
		return "max index id"
	case 7:
		return "key generator"
	default:
		return fmt.Sprintf("object store metadata %d", meta)
	}
}

func ixMetaName(meta byte) string {
	switch meta {
	case 0:
		return "index name"
	case 1:
		return "unique"
	case 2:
		return "key path"
	case 3:
		return "multi-entry"
	default:
		return fmt.Sprintf("index metadata %d", meta)
	}
}

func parseIDBRecord(key []byte) (idbRec, bool) {
	p, rest, ok := parseIDBPrefix(key)
	if !ok {
		return idbRec{}, false
	}
	rec := idbRec{db: p.db, os: p.os, ix: p.idx}
	switch {
	case p.db == 0 && p.os == 0 && p.idx == 0:
		if len(rest) < 1 {
			return idbRec{}, false
		}
		rec.kind = "global"
		rec.meta = rest[0]
		rest = rest[1:]
		switch rec.meta {
		case 0, 1, 2, 3, 4, 5, 6:
			if len(rest) == 0 {
				return rec, true
			}
		case 100:
			if _, r, ok := takeVarint(rest); ok && len(r) == 0 {
				return rec, true
			}
		case 201:
			origin, r1, ok1 := takeStringWithLength(rest)
			name, r2, ok2 := takeStringWithLength(r1)
			if ok1 && ok2 && len(r2) == 0 {
				rec.kind = "dbname"
				rec.origin = origin
				rec.dbName = name
				return rec, true
			}
		}
		return idbRec{}, false

	case p.os == 0 && p.idx == 0:
		if len(rest) < 1 {
			return idbRec{}, false
		}
		meta := rest[0]
		rest = rest[1:]
		switch meta {
		case 0, 1, 2, 3, 4, 5:
			if len(rest) == 0 {
				rec.kind, rec.meta = "dbmeta", meta
				return rec, true
			}
		case 50:
			osID, r, ok := takeVarint(rest)
			if ok && len(r) == 1 {
				rec.kind, rec.osID, rec.meta = "osmeta", int64(osID), r[0]
				return rec, true
			}
		case 100:
			osID, r, ok1 := takeVarint(rest)
			ixID, r2, ok2 := takeVarint(r)
			if ok1 && ok2 && len(r2) == 1 {
				rec.kind, rec.osID, rec.ixID, rec.meta = "ixmeta", int64(osID), int64(ixID), r2[0]
				return rec, true
			}
		case 150, 151:
			if _, r, ok := takeVarint(rest); ok && (meta == 150 && len(r) == 0 || meta == 151) {
				if meta == 151 {
					if _, r2, ok2 := takeVarint(r); ok2 && len(r2) == 0 {
						rec.kind = "dbmeta"
						rec.meta = meta
						return rec, true
					}
				} else {
					rec.kind, rec.meta = "dbmeta", meta
					return rec, true
				}
			}
		case 200:
			name, r, ok := takeStringWithLength(rest)
			if ok && len(r) == 0 {
				rec.kind, rec.storeName = "osnames", name
				return rec, true
			}
		case 201:
			osID, r, ok1 := takeVarint(rest)
			name, r2, ok2 := takeStringWithLength(r)
			if ok1 && ok2 && len(r2) == 0 {
				rec.kind, rec.osID, rec.indexName = "ixnames", int64(osID), name
				return rec, true
			}
		}
		return idbRec{}, false

	case p.idx == 1 || p.idx == 2 || p.idx == 3:
		user, r, ok := parseIDBKey(rest)
		if !ok || len(r) != 0 {
			return idbRec{}, false
		}
		rec.userKey = user
		switch p.idx {
		case 1:
			rec.kind = "data"
		case 2:
			rec.kind = "exists"
		default:
			rec.kind = "blob"
		}
		return rec, true

	case p.idx >= 30:
		indexKey, r, ok := parseIDBKey(rest)
		if !ok {
			return idbRec{}, false
		}
		rec.kind = "index"
		rec.indexKey = indexKey
		if len(r) == 0 {
			return rec, true
		}
		seq, r2, ok := takeVarint(r)
		if !ok {
			return idbRec{}, false
		}
		_ = seq
		if len(r2) == 0 {
			return rec, true
		}
		primary, r3, ok := parseIDBKey(r2)
		if !ok || len(r3) != 0 {
			return idbRec{}, false
		}
		rec.primaryKey = primary
		return rec, true
	}
	return idbRec{}, false
}

type idbPrefix struct{ db, os, idx int64 }

func parseIDBPrefix(k []byte) (idbPrefix, []byte, bool) {
	if len(k) < 1 {
		return idbPrefix{}, nil, false
	}
	b := k[0]
	dbLen := int(b>>5) + 1
	osLen := int((b>>2)&7) + 1
	ixLen := int(b&3) + 1
	need := 1 + dbLen + osLen + ixLen
	if dbLen > 8 || osLen > 8 || ixLen > 4 || len(k) < need {
		return idbPrefix{}, nil, false
	}
	p := idbPrefix{
		db:  readLEInt(k[1 : 1+dbLen]),
		os:  readLEInt(k[1+dbLen : 1+dbLen+osLen]),
		idx: readLEInt(k[1+dbLen+osLen : need]),
	}
	return p, k[need:], true
}

func readLEInt(b []byte) int64 {
	var buf [8]byte
	copy(buf[:], b)
	return int64(binary.LittleEndian.Uint64(buf[:]))
}

func parseIDBKey(b []byte) (string, []byte, bool) {
	if len(b) == 0 {
		return "", nil, false
	}
	typ := b[0]
	b = b[1:]
	switch typ {
	case 0:
		return "null", b, true
	case 1:
		s, rest, ok := takeStringWithLength(b)
		if !ok {
			return "", nil, false
		}
		return strconv.Quote(s), rest, true
	case 2, 3:
		if len(b) < 8 {
			return "", nil, false
		}
		d := float64FromLE(b[:8])
		b = b[8:]
		if typ == 2 {
			return "Date(" + formatJSDate(d) + ")", b, true
		}
		return fmt.Sprint(numberValue(d)), b, true
	case 4:
		n, rest, ok := takeVarint(b)
		if !ok || n > 100000 {
			return "", nil, false
		}
		parts := make([]string, 0, n)
		for i := uint64(0); i < n; i++ {
			s, r, ok := parseIDBKey(rest)
			if !ok {
				return "", nil, false
			}
			parts = append(parts, s)
			rest = r
		}
		return "[" + strings.Join(parts, ", ") + "]", rest, true
	case 5:
		return "MinKey", b, true
	case 6:
		n, rest, ok := takeVarint(b)
		if !ok || n > uint64(len(rest)) {
			return "", nil, false
		}
		raw := rest[:n]
		rest = rest[n:]
		if s := string(raw); utf8MostlyText(s) {
			return strconv.Quote(s), rest, true
		}
		return "0x" + fmt.Sprintf("%x", raw), rest, true
	default:
		return "", nil, false
	}
}

func takeVarint(b []byte) (uint64, []byte, bool) {
	var x uint64
	var s uint
	for n := 0; n < 10 && n < len(b); n++ {
		c := b[n]
		if c < 0x80 {
			if n == 9 && c > 1 {
				return 0, nil, false
			}
			return x | uint64(c)<<s, b[n+1:], true
		}
		x |= uint64(c&0x7f) << s
		s += 7
	}
	return 0, nil, false
}

func takeStringWithLength(b []byte) (string, []byte, bool) {
	n, rest, ok := takeVarint(b)
	if !ok || n > uint64(len(rest)/2) {
		return "", nil, false
	}
	raw := rest[:n*2]
	s, ok := decodeUTF16BE(raw)
	if !ok {
		return "", nil, false
	}
	return s, rest[n*2:], true
}

func decodeUTF16BE(b []byte) (string, bool) {
	if len(b)%2 != 0 {
		return "", false
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
	}
	return string(utf16.Decode(u)), true
}

func float64FromLE(b []byte) float64 {
	return math.Float64frombits(binary.LittleEndian.Uint64(b))
}

func decodeIDBValue(rec idbRec, val []byte) (string, string) {
	switch rec.kind {
	case "data":
		if text, enc, ok := decodeObjectStoreValue(val); ok {
			return text, enc
		}
	case "exists", "index":
		if n, rest, ok := takeVarint(val); ok {
			if rec.kind == "exists" && len(rest) == 0 {
				return strconv.FormatUint(n, 10), "varint"
			}
			if rec.kind == "index" {
				if key, r2, ok := parseIDBKey(rest); ok && len(r2) == 0 {
					return fmt.Sprintf("version %d, primary key %s", n, key), "indexeddb"
				}
			}
		}
	case "global":
		switch rec.meta {
		case 2:
			if n, ok := parseCompactInt(val); ok {
				return fmt.Sprintf("%d (Blink %d, V8 %d)", n, uint32(n), uint32(n>>32)), "int"
			}
		case 0, 1, 5, 6:
			if n, ok := parseCompactInt(val); ok {
				return strconv.FormatInt(n, 10), "int"
			}
		}
	case "dbname":
		if n, ok := parseCompactInt(val); ok {
			return fmt.Sprintf("database id %d", n), "int"
		}
	case "dbmeta":
		switch rec.meta {
		case 0, 1, 2:
			if s, ok := decodeUTF16BE(val); ok {
				return s, "utf-16be"
			}
		default:
			if n, ok := parseCompactInt(val); ok {
				return strconv.FormatInt(n, 10), "int"
			}
		}
	case "osmeta":
		switch rec.meta {
		case 0:
			if s, ok := decodeUTF16BE(val); ok {
				return s, "utf-16be"
			}
		case 1:
			if s, ok := formatKeyPath(val); ok {
				return s, "text"
			}
		case 2, 3, 6:
			if len(val) == 1 {
				if val[0] == 0 {
					return "false", "bool"
				}
				return "true", "bool"
			}
		default:
			if n, ok := parseCompactInt(val); ok {
				return strconv.FormatInt(n, 10), "int"
			}
		}
	case "ixmeta":
		switch rec.meta {
		case 0:
			if s, ok := decodeUTF16BE(val); ok {
				return s, "utf-16be"
			}
		case 1, 3:
			if len(val) == 1 {
				if val[0] == 0 {
					return "false", "bool"
				}
				return "true", "bool"
			}
		case 2:
			if s, ok := formatKeyPath(val); ok {
				return s, "text"
			}
		}
	case "osnames", "ixnames":
		if n, ok := parseCompactInt(val); ok {
			return strconv.FormatInt(n, 10), "int"
		}
	}
	if text := readableBytes(val); text != "" {
		return text, "text"
	}
	t, enc := SmartDecode(val)
	return t, enc
}

func decodeObjectStoreValue(val []byte) (string, string, bool) {
	_, rest, ok := takeVarint(val)
	if !ok || len(rest) == 0 || rest[0] != 0xFF {
		rest = val
	}
	v, err := decodeSSV(rest)
	if err != nil {
		return "", "", false
	}
	switch t := v.(type) {
	case string:
		return t, "text", true
	case nil:
		return "null", "json", true
	default:
		pretty, err := json.MarshalIndent(t, "", "  ")
		if err != nil {
			return "", "", false
		}
		return string(pretty), "json", true
	}
}

func formatKeyPath(b []byte) (string, bool) {
	if len(b) >= 3 && b[0] == 0 && b[1] == 0 {
		switch b[2] {
		case 0:
			if len(b) == 3 {
				return "(empty key path)", true
			}
		case 1:
			s, rest, ok := takeStringWithLength(b[3:])
			if ok && len(rest) == 0 {
				return s, true
			}
		case 2:
			n, rest, ok := takeVarint(b[3:])
			if !ok {
				return "", false
			}
			var parts []string
			for i := uint64(0); i < n; i++ {
				s, r, ok := takeStringWithLength(rest)
				if !ok {
					return "", false
				}
				parts = append(parts, s)
				rest = r
			}
			if len(rest) == 0 {
				return strings.Join(parts, "."), true
			}
		}
	}
	if s, ok := decodeUTF16BE(b); ok {
		return s, true
	}
	return "", false
}

func parseCompactInt(b []byte) (int64, bool) {
	if len(b) == 0 || len(b) > 8 {
		return 0, false
	}
	return readLEInt(b), true
}

func prettyOrigin(s string) string {
	base := s
	if i := strings.LastIndex(s, "@"); i > 0 && strings.Contains(s[:i], "_") {
		base = s[:i]
	}
	scheme, rest, ok := strings.Cut(base, "_")
	if !ok || (scheme != "https" && scheme != "http" && scheme != "chrome" && scheme != "file") {
		return s
	}
	host, port, ok := strings.Cut(rest, "_")
	if !ok || host == "" {
		return s
	}
	if port == "0" || (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		return scheme + "://" + host
	}
	if _, err := strconv.Atoi(port); err == nil {
		return scheme + "://" + host + ":" + port
	}
	return s
}

func joinNonEmpty(sep string, parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}

func unquoteKey(s string) string {
	if len(s) >= 2 && s[0] == '"' {
		if u, err := strconv.Unquote(s); err == nil {
			return u
		}
	}
	return s
}

func readableBytes(b []byte) string {
	if s, ok := decodeUTF16BE(b); ok && s != "" && printableRatio(s) >= 0.9 && !strings.ContainsRune(s, 0) {
		return s
	}
	var parts []string
	var run []byte
	flush := func() {
		if len(run) >= 4 {
			parts = append(parts, string(run))
		}
		run = run[:0]
	}
	for _, c := range b {
		if c >= 32 && c < 127 {
			run = append(run, c)
		} else {
			flush()
		}
	}
	flush()
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n")
}

// base92Decode maps the pixa-pics Base92 alphabet (printable ASCII except
// quotation mark, backslash, and backtick) onto 0..91.
var base92Decode = func() [256]byte {
	var table [256]byte
	for i := range table {
		table[i] = 0xFF
	}
	n := byte(0)
	for c := 32; c <= 126; c++ {
		if c == '"' || c == '\\' || c == '`' {
			continue
		}
		table[c] = n
		n++
	}
	return table
}()

// decodeBase92 reverses the 13-bit-pair Base92 coding used by some Chrome
// apps to stash a zip inside a JSON string.
func decodeBase92(s string) ([]byte, error) {
	if s == "" {
		return nil, fmt.Errorf("empty base92")
	}
	var workspace uint32
	var bits int
	out := make([]byte, 0, len(s))
	emit := func() {
		for bits >= 8 {
			out = append(out, byte(workspace>>uint(bits-8)))
			bits -= 8
		}
	}
	i := 0
	for ; i+1 < len(s); i += 2 {
		a := base92Decode[s[i]]
		b := base92Decode[s[i+1]]
		if a == 0xFF || b == 0xFF {
			return nil, fmt.Errorf("invalid base92")
		}
		workspace = workspace<<13 | (uint32(a)*91 + uint32(b))
		bits += 13
		emit()
	}
	if i < len(s) {
		a := base92Decode[s[i]]
		if a == 0xFF {
			return nil, fmt.Errorf("invalid base92")
		}
		workspace = workspace<<6 | uint32(a)
		bits += 6
		emit()
	}
	return out, nil
}

// expandEmbedded unpacks data URLs and zip payloads so stored documents show
// up as text instead of a base64 blob.
func expandEmbedded(s string) (any, bool) {
	lower := strings.ToLower(s)
	if !strings.HasPrefix(lower, "data:") {
		return nil, false
	}
	comma := strings.IndexByte(s, ',')
	if comma < 0 {
		return nil, false
	}
	meta, payload := s[:comma], s[comma+1:]
	var raw []byte
	var err error
	switch {
	case strings.Contains(strings.ToLower(meta), "base64"):
		raw, err = base64.StdEncoding.DecodeString(payload)
		if err != nil {
			raw, err = base64.RawStdEncoding.DecodeString(payload)
		}
	case strings.Contains(strings.ToLower(meta), "base92"):
		raw, err = decodeBase92(payload)
	default:
		raw = []byte(payload)
	}
	if err != nil || len(raw) == 0 {
		return nil, false
	}
	return expandBytes(raw)
}

func expandBytes(b []byte) (any, bool) {
	if len(b) < 4 || b[0] != 'P' || b[1] != 'K' {
		return nil, false
	}
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil || len(zr.File) == 0 {
		return nil, false
	}
	files := map[string]any{}
	shown := 0
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if shown >= 20 {
			files[f.Name] = fmt.Sprintf("(%d more bytes not shown)", f.UncompressedSize64)
			continue
		}
		rc, err := f.Open()
		if err != nil {
			files[f.Name] = "(unreadable)"
			continue
		}
		limit := int64(256 << 10)
		buf, err := io.ReadAll(io.LimitReader(rc, limit+1))
		rc.Close()
		if err != nil {
			files[f.Name] = "(unreadable)"
			continue
		}
		truncated := int64(len(buf)) > limit
		if truncated {
			buf = buf[:limit]
		}
		if s := string(buf); utf8MostlyText(s) {
			if truncated {
				s += "\n… (truncated)"
			}
			files[f.Name] = s
		} else {
			files[f.Name] = fmt.Sprintf("(binary, %d bytes)", f.UncompressedSize64)
		}
		shown++
	}
	if len(files) == 0 {
		return nil, false
	}
	return map[string]any{"zip": files}, true
}

// mathFloat is split out so the idb file can convert bits without importing
// math under two names. Implemented in terms of the encoding already used by
// the V8 decoder.
