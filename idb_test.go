package main

import (
	"os"
	"strings"
	"testing"
)

func TestBase92Vector(t *testing.T) {
	// Known vector from the pixa-pics Base92 library.
	out, err := decodeBase92("  ,@%7 sBv&]1")
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	if string(out) != string(want) {
		t.Fatalf("got %v", out)
	}
}

func TestDecodeStructuredClone(t *testing.T) {
	// Blink v21 envelope around a V8 object {"a": "hi"}.
	raw := []byte{
		0xff, 0x15,
		0xfe, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0xff, 0x10,
		'o',
		'"', 0x01, 'a',
		'"', 0x02, 'h', 'i',
		'{', 0x01,
	}
	v, err := decodeSSV(raw)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := v.(map[string]any)
	if !ok || m["a"] != "hi" {
		t.Fatalf("decoded %#v", v)
	}
}

func TestIndexedDBKeyRoundTripDisplay(t *testing.T) {
	// database 1, object store 1, data record, user key "theme".
	key := []byte{0x00, 0x01, 0x01, 0x01, 0x01, 0x05,
		0x00, 't', 0x00, 'h', 0x00, 'e', 0x00, 'm', 0x00, 'e'}
	rec, ok := parseIDBRecord(key)
	if !ok || rec.kind != "data" || rec.userKey != `"theme"` {
		t.Fatalf("record %+v ok=%v", rec, ok)
	}
	ssv := []byte{
		0xff, 0x15,
		0xfe, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0xff, 0x10,
		'"', 0x04, 'd', 'a', 'r', 'k',
	}
	val := append([]byte{0x01}, ssv...)
	entries := []Entry{newEntry(key, val, false)}
	enrichIndexedDB(entries)
	if entries[0].Chrome.Type != "idb-data" || entries[0].ValText != "dark" {
		t.Fatalf("enriched type=%s val=%q key=%q", entries[0].Chrome.Type, entries[0].ValText, entries[0].KeyDisplay)
	}
}

func TestUploadedIndexedDBPlainText(t *testing.T) {
	path := "/home/ubuntu/.cursor/projects/workspace/uploads/000274_e277.ldb"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skip(path)
	}
	s, err := OpenUploaded([]UploadedFile{{Name: "000274.ldb", Data: data}})
	if err != nil {
		t.Fatal(err)
	}
	var dataRows, hexRows int
	var sample string
	for i := range s.Entries {
		e := &s.Entries[i]
		if e.ValEnc == "hex" && e.KeyEnc == "hex" {
			hexRows++
		}
		if e.Chrome.Type == "idb-data" {
			dataRows++
			if sample == "" && strings.Contains(e.ValText, "name") {
				sample = e.KeyDisplay + "\n" + clip(e.ValText, 500)
			}
		}
	}
	t.Logf("entries=%d data=%d stillHex=%d\n%s", len(s.Entries), dataRows, hexRows, sample)
	if dataRows < 10 {
		t.Fatalf("expected decoded IndexedDB records, got %d", dataRows)
	}
	if hexRows > len(s.Entries)/10 {
		t.Fatalf("too many undecoded records: %d of %d", hexRows, len(s.Entries))
	}
	if !strings.Contains(sample, "name") {
		t.Fatal("decoded record did not contain readable text")
	}
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
