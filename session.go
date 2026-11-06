package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/golang/leveldb/db"
	"github.com/golang/leveldb/record"
	"github.com/golang/leveldb/table"
)

const (
	maxEntries       = 200_000
	maxStoredValue   = 1 << 20 // 1 MiB kept per value; larger values are truncated
	previewRuneLimit = 160
)

// Entry is one key/value record loaded from the database.
type Entry struct {
	Key       []byte
	Value     []byte
	ValueSize int  // original size before truncation
	Deleted   bool // record is a deletion tombstone (forensically interesting)

	// Precomputed at load time for fast search and listing.
	KeyDisplay string
	KeyEnc     string
	ValText    string
	ValEnc     string
	Chrome     ChromeInfo
}

// Session holds a fully loaded snapshot of a database.
type Session struct {
	Source    string   // user-facing description of what was opened
	Mode      string   // "db" (full database scan) or "table" (raw table files)
	Files     []string // files that were parsed
	Entries   []Entry
	Truncated bool // true if maxEntries was hit
}

type UploadedFile struct {
	Name string
	Data []byte
}

var leveldbFilePattern = regexp.MustCompile(`^(CURRENT|LOCK|LOG(\.old)?|MANIFEST-\d+|\d+\.(ldb|sst|log))$`)

// OpenPath opens a LevelDB directory or a single table file from disk. The
// original files are never written to: everything is copied into a temp
// workspace and parsed there, so pointing this tool at a live Chrome profile
// is safe.
func OpenPath(path string) (*Session, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("cannot access %q: %w", path, err)
	}
	if !info.IsDir() {
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".ldb" && ext != ".sst" && ext != ".log" {
			return nil, fmt.Errorf("%q is not a directory or a .ldb/.sst/.log file", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return OpenUploadedAs([]UploadedFile{{Name: filepath.Base(path), Data: data}}, path)
	}

	// Chrome convenience: if the user points at e.g. "Local Storage", descend
	// into the "leveldb" subdirectory when the directory itself has no
	// LevelDB files.
	if _, err := os.Stat(filepath.Join(path, "CURRENT")); os.IsNotExist(err) {
		if _, err2 := os.Stat(filepath.Join(path, "leveldb", "CURRENT")); err2 == nil {
			path = filepath.Join(path, "leveldb")
		}
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	var files []UploadedFile
	for _, e := range entries {
		if e.IsDir() || !leveldbFilePattern.MatchString(e.Name()) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(path, e.Name()))
		if err != nil {
			return nil, err
		}
		files = append(files, UploadedFile{Name: e.Name(), Data: data})
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no LevelDB files (CURRENT, MANIFEST-*, *.ldb, *.log) found in %q", path)
	}
	return OpenUploadedAs(files, path)
}

// OpenUploaded opens a set of uploaded LevelDB files.
func OpenUploaded(files []UploadedFile) (*Session, error) {
	names := make([]string, 0, len(files))
	for _, f := range files {
		if strings.HasSuffix(f.Name, ".ldb") || strings.HasSuffix(f.Name, ".sst") || strings.HasSuffix(f.Name, ".log") {
			names = append(names, f.Name)
		}
	}
	source := fmt.Sprintf("%d uploaded file(s)", len(files))
	if len(names) == 1 {
		source = names[0]
	}
	return OpenUploadedAs(files, source)
}

// versioned is one (key, seq, kind, value) tuple read from a table or log.
type versioned struct {
	key     []byte
	seq     uint64
	deleted bool
	value   []byte
}

// OpenUploadedAs parses the given LevelDB files and builds a session.
//
// Note on approach: github.com/golang/leveldb is an incomplete port — its
// DB.Find iterator is unimplemented — so instead of opening the database
// through the high-level API we scan the files directly, the same way
// forensic tools do: every sorted-table file (*.ldb / *.sst) is read with the
// library's table reader, every write-ahead log (*.log) is replayed with the
// library's record reader, and the results are merged by sequence number so
// that the newest version of each key wins. Deletion tombstones are kept and
// shown, which is usually what you want when inspecting Chrome data.
func OpenUploadedAs(files []UploadedFile, source string) (*Session, error) {
	tmp, err := os.MkdirTemp("", "leveldb-viewer-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	// Sort so logs replay in file-number order and results are deterministic.
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })

	s := &Session{Source: source}
	var records []versioned
	hasTable, hasLog := false, false

	var parseErrs []string
	for _, uf := range files {
		switch {
		case strings.HasSuffix(uf.Name, ".ldb") || strings.HasSuffix(uf.Name, ".sst"):
			rs, err := readTableFile(tmp, uf)
			if err != nil {
				parseErrs = append(parseErrs, fmt.Sprintf("%s: %v", uf.Name, err))
				continue
			}
			records = append(records, rs...)
			hasTable = true
			s.Files = append(s.Files, uf.Name)
		case strings.HasSuffix(uf.Name, ".log"):
			rs, err := readLogFile(uf)
			if err != nil && len(rs) == 0 {
				parseErrs = append(parseErrs, fmt.Sprintf("%s: %v", uf.Name, err))
				continue
			}
			records = append(records, rs...)
			hasLog = true
			s.Files = append(s.Files, uf.Name)
		}
	}
	if len(s.Files) == 0 {
		if len(parseErrs) > 0 {
			return nil, fmt.Errorf("could not parse any file: %s", strings.Join(parseErrs, "; "))
		}
		return nil, fmt.Errorf("no LevelDB data files found — expected *.ldb, *.sst or *.log files")
	}

	s.Mode = "table"
	if hasLog || (hasTable && len(s.Files) > 1) {
		s.Mode = "db"
	}

	// Keep only the newest version of each key.
	best := map[string]*versioned{}
	for i := range records {
		r := &records[i]
		k := string(r.key)
		if cur, ok := best[k]; !ok || r.seq >= cur.seq {
			best[k] = r
		}
	}
	keys := make([]string, 0, len(best))
	for k := range best {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if len(s.Entries) >= maxEntries {
			s.Truncated = true
			break
		}
		r := best[k]
		s.Entries = append(s.Entries, newEntry(r.key, r.value, r.deleted))
	}
	enrichIndexedDB(s.Entries)
	return s, nil
}

// readTableFile reads every record of one sorted-table file. Keys inside
// tables are internal keys: user key + 8-byte little-endian (seq<<8 | kind)
// trailer, where kind 0 is a deletion tombstone.
func readTableFile(tmpDir string, uf UploadedFile) ([]versioned, error) {
	p := filepath.Join(tmpDir, "scan-"+uf.Name)
	if err := os.WriteFile(p, uf.Data, 0o600); err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	r := table.NewReader(f, &db.Options{})
	defer r.Close() // also closes f

	var out []versioned
	it := r.Find(nil, nil)
	for it.Next() {
		ikey := it.Key()
		if len(ikey) < 8 {
			continue
		}
		trailer := binary.LittleEndian.Uint64(ikey[len(ikey)-8:])
		kind := trailer & 0xff
		if kind > 1 {
			continue
		}
		out = append(out, versioned{
			key:     append([]byte(nil), ikey[:len(ikey)-8]...),
			seq:     trailer >> 8,
			deleted: kind == 0,
			value:   append([]byte(nil), it.Value()...),
		})
	}
	if err := it.Close(); err != nil && len(out) == 0 {
		return nil, fmt.Errorf("not a valid LevelDB table: %w", err)
	}
	return out, nil
}

// readLogFile replays a write-ahead log. Each log record is a batch:
// 8-byte seq, 4-byte count, then count × (kind byte, varint key, varint
// value when kind is "set"). Partial/corrupt tails are tolerated — whatever
// parsed cleanly is returned.
func readLogFile(uf UploadedFile) ([]versioned, error) {
	var out []versioned
	rr := record.NewReader(bytes.NewReader(uf.Data))
	for {
		rec, err := rr.Next()
		if err != nil {
			if err == io.EOF {
				return out, nil
			}
			return out, fmt.Errorf("not a valid LevelDB log: %w", err)
		}
		buf, err := io.ReadAll(rec)
		if err != nil {
			return out, nil // corrupt tail; keep what we have
		}
		out = append(out, parseBatch(buf)...)
	}
}

func parseBatch(b []byte) []versioned {
	if len(b) < 12 {
		return nil
	}
	seq := binary.LittleEndian.Uint64(b[:8])
	count := binary.LittleEndian.Uint32(b[8:12])
	b = b[12:]
	var out []versioned
	for i := uint32(0); i < count && len(b) > 0; i++ {
		kind := b[0]
		b = b[1:]
		key, rest, ok := readLenPrefixed(b)
		if !ok || kind > 1 {
			break
		}
		b = rest
		var value []byte
		if kind == 1 {
			value, rest, ok = readLenPrefixed(b)
			if !ok {
				break
			}
			b = rest
		}
		out = append(out, versioned{
			key:     append([]byte(nil), key...),
			seq:     seq + uint64(i),
			deleted: kind == 0,
			value:   append([]byte(nil), value...),
		})
	}
	return out
}

func readLenPrefixed(b []byte) (s, rest []byte, ok bool) {
	n, w := binary.Uvarint(b)
	if w <= 0 || uint64(len(b)-w) < n {
		return nil, nil, false
	}
	return b[w : w+int(n)], b[w+int(n):], true
}

func newEntry(key, value []byte, deleted bool) Entry {
	k := append([]byte(nil), key...)
	origSize := len(value)
	v := value
	if len(v) > maxStoredValue {
		v = v[:maxStoredValue]
	}
	v = append([]byte(nil), v...)

	e := Entry{Key: k, Value: v, ValueSize: origSize, Deleted: deleted}
	e.Chrome = ClassifyChromeKey(k)
	if e.Chrome.KeyDisplay != "" {
		e.KeyDisplay, e.KeyEnc = e.Chrome.KeyDisplay, e.Chrome.KeyEnc
	} else {
		e.KeyDisplay, e.KeyEnc = SmartDecode(k)
	}
	e.ValText, e.ValEnc = DecodeChromeValue(e.Chrome, v)
	return e
}

// --- Querying ------------------------------------------------------------

type entryRow struct {
	Index        int    `json:"index"`
	KeyDisplay   string `json:"keyDisplay"`
	KeyEnc       string `json:"keyEnc"`
	ValuePreview string `json:"valuePreview"`
	ValueEnc     string `json:"valueEnc"`
	ValueSize    int    `json:"valueSize"`
	Type         string `json:"type"`
	Origin       string `json:"origin,omitempty"`
	Deleted      bool   `json:"deleted"`
}

type queryResult struct {
	Total   int        `json:"total"`
	Matched int        `json:"matched"`
	Offset  int        `json:"offset"`
	Rows    []entryRow `json:"rows"`
	// Indexes is every matching entry index, in display order. It is filled
	// only when withIDs is set, so ordinary page loads stay small while
	// Shift+click can still select a range that spans pages.
	Indexes []int `json:"indexes,omitempty"`
}

func (s *Session) Query(q, typ, origin string, offset, limit int, withIDs bool) queryResult {
	q = strings.ToLower(strings.TrimSpace(q))
	res := queryResult{Total: len(s.Entries), Offset: offset}
	if withIDs {
		res.Indexes = []int{}
	}
	for i := range s.Entries {
		e := &s.Entries[i]
		if typ != "" && typ != "all" && e.Chrome.Type != typ {
			continue
		}
		if origin != "" && e.Chrome.Origin != origin {
			continue
		}
		if q != "" &&
			!strings.Contains(strings.ToLower(e.KeyDisplay), q) &&
			!strings.Contains(strings.ToLower(e.ValText), q) &&
			!strings.Contains(strings.ToLower(e.Chrome.Origin), q) {
			continue
		}
		if withIDs {
			res.Indexes = append(res.Indexes, i)
		}
		if res.Matched >= offset && len(res.Rows) < limit {
			res.Rows = append(res.Rows, entryRow{
				Index:        i,
				KeyDisplay:   truncateRunes(e.KeyDisplay, previewRuneLimit),
				KeyEnc:       e.KeyEnc,
				ValuePreview: truncateRunes(e.ValText, previewRuneLimit),
				ValueEnc:     e.ValEnc,
				ValueSize:    e.ValueSize,
				Type:         e.Chrome.Type,
				Origin:       e.Chrome.Origin,
				Deleted:      e.Deleted,
			})
		}
		res.Matched++
	}
	return res
}

type originCount struct {
	Origin string `json:"origin"`
	Count  int    `json:"count"`
}

type summary struct {
	ID        string         `json:"id"`
	Source    string         `json:"source"`
	Mode      string         `json:"mode"`
	Files     []string       `json:"files"`
	Total     int            `json:"total"`
	Truncated bool           `json:"truncated"`
	Types     map[string]int `json:"types"`
	Origins   []originCount  `json:"origins"`
}

func (s *Session) Summary(id string) summary {
	sum := summary{
		ID: id, Source: s.Source, Mode: s.Mode, Files: s.Files,
		Total: len(s.Entries), Truncated: s.Truncated, Types: map[string]int{},
	}
	origins := map[string]int{}
	for i := range s.Entries {
		sum.Types[s.Entries[i].Chrome.Type]++
		if o := s.Entries[i].Chrome.Origin; o != "" {
			origins[o]++
		}
	}
	for o, c := range origins {
		sum.Origins = append(sum.Origins, originCount{o, c})
	}
	sort.Slice(sum.Origins, func(i, j int) bool { return sum.Origins[i].Origin < sum.Origins[j].Origin })
	return sum
}

type entryDetail struct {
	Index      int        `json:"index"`
	Type       string     `json:"type"`
	TypeLabel  string     `json:"typeLabel"`
	Origin     string     `json:"origin,omitempty"`
	StorageKey string     `json:"storageKey,omitempty"`
	Deleted    bool       `json:"deleted"`
	Key        valueViews `json:"key"`
	Value      valueViews `json:"value"`
	Truncated  bool       `json:"truncated"`
}

type valueViews struct {
	Size    int    `json:"size"`
	Best    string `json:"best"`
	BestEnc string `json:"bestEnc"`
	UTF8    string `json:"utf8"`
	UTF16LE string `json:"utf16le"`
	Latin1  string `json:"latin1"`
	HexDump string `json:"hexDump"`
	JSON    string `json:"json,omitempty"`
}

func (s *Session) EntryDetail(i int) entryDetail {
	e := &s.Entries[i]
	d := entryDetail{
		Index:      i,
		Type:       e.Chrome.Type,
		TypeLabel:  TypeLabel(e.Chrome.Type),
		Origin:     e.Chrome.Origin,
		StorageKey: e.Chrome.StorageKey,
		Deleted:    e.Deleted,
		Key:        makeViews(e.Key, e.KeyDisplay, e.KeyEnc),
		Value:      makeViews(e.Value, e.ValText, e.ValEnc),
		Truncated:  e.ValueSize > len(e.Value),
	}
	d.Value.Size = e.ValueSize
	return d
}
