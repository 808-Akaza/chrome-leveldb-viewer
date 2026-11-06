package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/golang/leveldb/db"
	"github.com/golang/leveldb/table"
)

// makeInternalKey builds a table-file internal key: user key + 8-byte
// little-endian (seq<<8 | kind) trailer.
func makeInternalKey(userKey []byte, seq uint64, kind byte) []byte {
	out := append([]byte(nil), userKey...)
	var tr [8]byte
	binary.LittleEndian.PutUint64(tr[:], seq<<8|uint64(kind))
	return append(out, tr[:]...)
}

// TestRawTableScan writes a real sorted-table file (as found inside Chrome
// LevelDB folders) and reads it back through the raw-table path, including a
// deletion tombstone and a newer version of a key superseding an older one.
func TestRawTableScan(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "000005.ldb")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	w := table.NewWriter(f, &db.Options{})
	type rec struct {
		key  string
		seq  uint64
		kind byte
		val  string
	}
	// The writer validates bytewise key order, so the lower sequence number
	// comes first here; the scanner must still surface the newest version.
	recs := []rec{
		{"alpha", 7, 1, "old-value"},
		{"alpha", 12, 1, "new-value"},
		{"beta", 9, 0, ""}, // tombstone
		{"gamma", 3, 1, `{"n":1}`},
	}
	for _, r := range recs {
		if err := w.Set(makeInternalKey([]byte(r.key), r.seq, r.kind), []byte(r.val), nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s, err := OpenUploaded([]UploadedFile{{Name: "000005.ldb", Data: data}})
	if err != nil {
		t.Fatal(err)
	}
	if s.Mode != "table" {
		t.Errorf("mode = %q, want table", s.Mode)
	}
	if len(s.Entries) != 3 {
		t.Fatalf("got %d entries, want 3 (versions merged): %+v", len(s.Entries), s.Entries)
	}
	if e := s.Entries[0]; string(e.Key) != "alpha" || e.ValText != "new-value" || e.Deleted {
		t.Errorf("alpha: got value %q deleted=%v, want newest version", e.ValText, e.Deleted)
	}
	if e := s.Entries[1]; string(e.Key) != "beta" || !e.Deleted {
		t.Errorf("beta: expected deletion tombstone, got %+v", e)
	}
	if e := s.Entries[2]; string(e.Key) != "gamma" || e.ValText != `{"n":1}` {
		t.Errorf("gamma: got %q", e.ValText)
	}
}

// TestGarbageFileRejected ensures a non-LevelDB file produces a clear error.
func TestGarbageFileRejected(t *testing.T) {
	_, err := OpenUploaded([]UploadedFile{{Name: "junk.ldb", Data: []byte("this is not a leveldb table file at all")}})
	if err == nil {
		t.Fatal("expected an error for a garbage .ldb file")
	}
}
