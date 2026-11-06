package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// ChromeInfo describes how a record maps onto a known Chrome storage schema.
type ChromeInfo struct {
	Type       string // ls-item, ls-meta, ss-item, ss-namespace, ss-meta, version, other
	Origin     string
	StorageKey string // the JavaScript-visible key, when known

	// Optional override for how the raw key should be displayed.
	KeyDisplay string
	KeyEnc     string
}

// TypeLabel returns a human-readable name for a record type.
func TypeLabel(t string) string {
	switch t {
	case "ls-item":
		return "Local Storage item"
	case "ls-meta":
		return "Local Storage metadata"
	case "ss-item":
		return "Session Storage item"
	case "ss-namespace":
		return "Session Storage namespace"
	case "ss-meta":
		return "Session Storage metadata"
	case "version":
		return "Schema version"
	case "idb-data":
		return "IndexedDB record"
	case "idb-index":
		return "IndexedDB index"
	case "idb-meta":
		return "IndexedDB metadata"
	default:
		return "Key/value record"
	}
}

// ClassifyChromeKey recognizes the key layouts Chrome uses in its LevelDB
// databases.
//
// Local Storage ("Local Storage/leveldb" in a Chrome profile):
//
//	"VERSION"                              schema version
//	"META:" + origin                       per-origin metadata (protobuf)
//	"_" + origin + "\x00" + fmt + key      one stored item; fmt 0x00 = the key
//	                                       is UTF-16LE, 0x01 = Latin-1
//
// Session Storage ("Session Storage" in a Chrome profile):
//
//	"version"                              schema version
//	"namespace-<guid>-<origin>"            maps a tab namespace to a map id
//	"map-<id>-<key>"                       one stored item (key is UTF-8/UTF-16)
func ClassifyChromeKey(key []byte) ChromeInfo {
	switch {
	case bytes.Equal(key, []byte("VERSION")) || bytes.Equal(key, []byte("version")):
		return ChromeInfo{Type: "version"}

	case bytes.HasPrefix(key, []byte("META:")):
		origin := string(key[len("META:"):])
		return ChromeInfo{Type: "ls-meta", Origin: origin}

	case len(key) > 1 && key[0] == '_' && bytes.IndexByte(key, 0) > 0:
		sep := bytes.IndexByte(key, 0)
		origin := string(key[1:sep])
		rest := key[sep+1:]
		info := ChromeInfo{Type: "ls-item", Origin: origin}
		if len(rest) > 0 {
			switch rest[0] {
			case 0x00:
				info.StorageKey = decodeUTF16LELossy(rest[1:])
				info.KeyEnc = "utf-16le"
			case 0x01:
				info.StorageKey = decodeLatin1(rest[1:])
				info.KeyEnc = "latin-1"
			default:
				info.StorageKey, info.KeyEnc = SmartDecode(rest)
			}
			info.KeyDisplay = origin + " » " + info.StorageKey
		}
		return info

	case bytes.HasPrefix(key, []byte("namespace-")):
		rest := string(key[len("namespace-"):])
		info := ChromeInfo{Type: "ss-namespace"}
		if i := strings.LastIndex(rest, "-"); i > 0 && strings.Contains(rest[i+1:], "://") {
			info.Origin = rest[i+1:]
		}
		return info

	case bytes.HasPrefix(key, []byte("map-")):
		rest := string(key[len("map-"):])
		info := ChromeInfo{Type: "ss-item"}
		if i := strings.Index(rest, "-"); i > 0 {
			info.StorageKey = rest[i+1:]
		}
		return info

	case bytes.HasPrefix(key, []byte("meta:")) || bytes.HasPrefix(key, []byte("next-map-id")):
		return ChromeInfo{Type: "ss-meta"}
	}
	return ChromeInfo{Type: "other"}
}

// DecodeChromeValue decodes a record value using Chrome's conventions when
// the record type is known, and falls back to smart detection otherwise.
//
// Local Storage values carry a one-byte format prefix: 0x00 means the rest is
// UTF-16LE, 0x01 means Latin-1.
func DecodeChromeValue(info ChromeInfo, value []byte) (text, enc string) {
	if info.Type == "ls-item" && len(value) > 0 {
		switch value[0] {
		case 0x00:
			return decodeUTF16LELossy(value[1:]), "utf-16le"
		case 0x01:
			return decodeLatin1(value[1:]), "latin-1"
		}
	}
	return SmartDecode(value)
}

// SmartDecode guesses the best human-readable rendering of raw bytes:
// UTF-8 when valid and mostly printable, then UTF-16LE, then hex.
func SmartDecode(b []byte) (text, enc string) {
	if len(b) == 0 {
		return "", "empty"
	}
	if utf8.Valid(b) {
		s := string(b)
		if printableRatio(s) >= 0.85 {
			return s, "utf-8"
		}
	}
	if len(b)%2 == 0 {
		s := decodeUTF16LELossy(b)
		if s != "" && printableRatio(s) >= 0.85 && !strings.ContainsRune(s, 0xFFFD) {
			return s, "utf-16le"
		}
	}
	const hexLimit = 512
	if len(b) > hexLimit {
		return hex.EncodeToString(b[:hexLimit]) + "…", "hex"
	}
	return hex.EncodeToString(b), "hex"
}

func printableRatio(s string) float64 {
	if s == "" {
		return 0
	}
	printable, total := 0, 0
	for _, r := range s {
		total++
		if unicode.IsPrint(r) || r == '\n' || r == '\r' || r == '\t' {
			printable++
		}
	}
	return float64(printable) / float64(total)
}

func decodeUTF16LELossy(b []byte) string {
	if len(b)%2 != 0 {
		b = b[:len(b)-len(b)%2]
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return string(utf16.Decode(u))
}

func decodeLatin1(b []byte) string {
	rs := make([]rune, len(b))
	for i, c := range b {
		rs[i] = rune(c)
	}
	return string(rs)
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	rs := []rune(s)
	return string(rs[:n]) + "…"
}

// makeViews builds every rendering of a byte string for the detail panel.
func makeViews(b []byte, best, bestEnc string) valueViews {
	const textLimit = 256 << 10 // cap text renderings at 256 KiB
	v := valueViews{Size: len(b), Best: capString(best, textLimit), BestEnc: bestEnc}
	v.UTF8 = capString(strings.ToValidUTF8(string(b), "\uFFFD"), textLimit)
	v.UTF16LE = capString(decodeUTF16LELossy(b), textLimit)
	v.Latin1 = capString(decodeLatin1(b), textLimit)
	v.HexDump = hexDump(b, 4096)

	trimmed := strings.TrimSpace(best)
	if len(trimmed) > 1 && (trimmed[0] == '{' || trimmed[0] == '[') {
		var parsed any
		if err := json.Unmarshal([]byte(trimmed), &parsed); err == nil {
			if pretty, err := json.MarshalIndent(parsed, "", "  "); err == nil {
				v.JSON = capString(string(pretty), textLimit)
			}
		}
	}
	return v
}

func capString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n… (truncated)"
}

// hexDump renders a classic 16-bytes-per-row hex dump, capped at maxBytes.
func hexDump(b []byte, maxBytes int) string {
	truncated := false
	if len(b) > maxBytes {
		b = b[:maxBytes]
		truncated = true
	}
	var sb strings.Builder
	for off := 0; off < len(b); off += 16 {
		end := off + 16
		if end > len(b) {
			end = len(b)
		}
		row := b[off:end]
		fmt.Fprintf(&sb, "%08x  ", off)
		for i := 0; i < 16; i++ {
			if i < len(row) {
				fmt.Fprintf(&sb, "%02x ", row[i])
			} else {
				sb.WriteString("   ")
			}
			if i == 7 {
				sb.WriteByte(' ')
			}
		}
		sb.WriteString(" |")
		for _, c := range row {
			if c >= 0x20 && c < 0x7f {
				sb.WriteByte(c)
			} else {
				sb.WriteByte('.')
			}
		}
		sb.WriteString("|\n")
	}
	if truncated {
		sb.WriteString("… (truncated)\n")
	}
	return sb.String()
}
