package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/golang/leveldb"
	"github.com/golang/leveldb/db"
)

func writeTestDB(t *testing.T, dir string) {
	t.Helper()
	d, err := leveldb.Open(dir, &db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	set := func(k, v []byte) {
		if err := d.Set(k, v, nil); err != nil {
			t.Fatal(err)
		}
	}
	set([]byte("VERSION"), []byte("1"))
	set([]byte("META:https://example.com"), []byte{0x08, 0x01})
	// Local Storage item: latin-1 key "theme", latin-1 value "dark"
	set(append(append([]byte("_https://example.com"), 0x00, 0x01), []byte("theme")...),
		append([]byte{0x01}, []byte("dark")...))
	// Local Storage item with UTF-16LE value "héllo" (via 0x00 prefix)
	set(append(append([]byte("_https://example.com"), 0x00, 0x01), []byte("msg")...),
		append([]byte{0x00}, 0x68, 0x00, 0xe9, 0x00, 0x6c, 0x00, 0x6c, 0x00, 0x6f, 0x00))
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenPathAndDecode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "leveldb")
	writeTestDB(t, dir)

	s, err := OpenPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Mode != "db" {
		t.Fatalf("mode = %q, want db", s.Mode)
	}
	if len(s.Entries) != 4 {
		t.Fatalf("got %d entries, want 4", len(s.Entries))
	}

	byKey := map[string]*Entry{}
	for i := range s.Entries {
		byKey[s.Entries[i].KeyDisplay] = &s.Entries[i]
	}

	if e := byKey["VERSION"]; e == nil || e.Chrome.Type != "version" {
		t.Errorf("VERSION record missing or misclassified: %+v", e)
	}
	e := byKey["https://example.com » theme"]
	if e == nil {
		t.Fatalf("theme record not found; keys: %v", keys(byKey))
	}
	if e.Chrome.Type != "ls-item" || e.Chrome.Origin != "https://example.com" ||
		e.Chrome.StorageKey != "theme" || e.ValText != "dark" || e.ValEnc != "latin-1" {
		t.Errorf("theme record decoded wrong: %+v valText=%q valEnc=%q", e.Chrome, e.ValText, e.ValEnc)
	}
	e = byKey["https://example.com » msg"]
	if e == nil || e.ValText != "héllo" || e.ValEnc != "utf-16le" {
		t.Errorf("msg record decoded wrong: %+v", e)
	}
}

func TestQueryAndSummary(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "leveldb")
	writeTestDB(t, dir)
	s, err := OpenPath(dir)
	if err != nil {
		t.Fatal(err)
	}

	res := s.Query("dark", "", "", 0, 100, true)
	if res.Matched != 1 || len(res.Rows) != 1 || len(res.Indexes) != 1 {
		t.Fatalf("query 'dark': matched=%d rows=%d indexes=%d, want 1/1/1", res.Matched, len(res.Rows), len(res.Indexes))
	}
	res = s.Query("", "ls-item", "", 0, 100, false)
	if res.Matched != 2 || res.Indexes != nil {
		t.Fatalf("type filter ls-item: matched=%d indexes=%v, want 2 and no index list", res.Matched, res.Indexes)
	}

	sum := s.Summary("x")
	if sum.Types["ls-item"] != 2 || sum.Types["ls-meta"] != 1 || sum.Types["version"] != 1 {
		t.Fatalf("summary types wrong: %+v", sum.Types)
	}
	if len(sum.Origins) != 1 || sum.Origins[0].Origin != "https://example.com" {
		t.Fatalf("summary origins wrong: %+v", sum.Origins)
	}
}

// TestChromeOriginalsUntouched ensures opening never mutates the source dir.
func TestChromeOriginalsUntouched(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "leveldb")
	writeTestDB(t, dir)
	before := dirListing(t, dir)
	if _, err := OpenPath(dir); err != nil {
		t.Fatal(err)
	}
	after := dirListing(t, dir)
	if before != after {
		t.Fatalf("source directory changed:\nbefore: %s\nafter:  %s", before, after)
	}
}

func dirListing(t *testing.T, dir string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	out := ""
	for _, m := range matches {
		out += filepath.Base(m) + ";"
	}
	return out
}

func TestRenderReport(t *testing.T) {
	s := &Session{
		Source: "unit <db>",
		Mode:   "db",
		Files:  []string{"000003.log"},
		Entries: []Entry{
			{
				KeyDisplay: "theme",
				KeyEnc:     "latin-1",
				ValText:    "dark",
				ValEnc:     "latin-1",
				ValueSize:  4,
				Value:      []byte("dark"),
				Chrome:     ChromeInfo{Type: "ls-item", Origin: "https://example.com", StorageKey: "theme"},
			},
			{
				KeyDisplay: `say <script>alert("x")</script>`,
				KeyEnc:     "text",
				ValText:    `{"ok":true}`,
				ValEnc:     "text",
				ValueSize:  11,
				Value:      []byte(`{"ok":true}`),
				Chrome:     ChromeInfo{Type: "other"},
			},
		},
	}

	name, body, err := s.RenderReport([]int{1, 0, 1, 99})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(name, "leveldb-report-") || !strings.HasSuffix(name, ".html") {
		t.Fatalf("filename %q", name)
	}
	html := string(body)
	if strings.Contains(html, "<script>") {
		t.Fatalf("report did not escape HTML: %s", html)
	}
	if !strings.Contains(html, "&lt;script&gt;") || !strings.Contains(html, "theme") || !strings.Contains(html, "dark") {
		t.Fatalf("report missing expected records:\n%s", html)
	}
	if !strings.Contains(html, "&#34;ok&#34;: true") {
		t.Fatalf("JSON value was not pretty-printed:\n%s", html)
	}
	// Request order is kept, invalid and duplicate indexes are dropped.
	themeAt := strings.Index(html, ">theme<")
	scriptAt := strings.Index(html, "&lt;script&gt;")
	if scriptAt < 0 || themeAt < 0 || scriptAt > themeAt {
		t.Fatalf("records out of order: script %d theme %d", scriptAt, themeAt)
	}

	if _, _, err := s.RenderReport(nil); err == nil {
		t.Fatal("empty selection should fail")
	}
	big := &Session{Entries: make([]Entry, exportLimit+1)}
	ids := make([]int, len(big.Entries))
	for i := range ids {
		ids[i] = i
	}
	if _, _, err := big.RenderReport(ids); err == nil {
		t.Fatal("over-limit selection should fail")
	}
}

func TestReportHTTP(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "leveldb")
	writeTestDB(t, dir)
	s, err := OpenPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := registerSession(s)

	req := httptest.NewRequest(http.MethodPost, "/api/session/"+id+"/report", bytes.NewBufferString(`{"indexes":[0,1,2,3]}`))
	rr := httptest.NewRecorder()
	handleSession(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	disp := rr.Header().Get("Content-Disposition")
	if !strings.Contains(disp, "leveldb-report-") || !strings.Contains(disp, ".html") {
		t.Fatalf("content-disposition %q", disp)
	}
	html := rr.Body.String()
	if !strings.Contains(html, "LevelDB Viewer report") || !strings.Contains(html, "theme") || !strings.Contains(html, "dark") {
		t.Fatalf("report missing decoded records:\n%s", html)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/session/"+id+"/report", bytes.NewBufferString(`{"indexes":[]}`))
	rr = httptest.NewRecorder()
	handleSession(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty selection status %d", rr.Code)
	}
}

func keys(m map[string]*Entry) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
