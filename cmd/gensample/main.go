// Command gensample writes sample LevelDB databases into ./sample-data using
// the exact on-disk key/value formats Chrome uses for Local Storage and
// Session Storage, so the viewer can be demoed without a real Chrome profile.
package main

import (
	"encoding/binary"
	"log"
	"os"
	"path/filepath"
	"unicode/utf16"

	"github.com/golang/leveldb"
	"github.com/golang/leveldb/db"
)

func utf16le(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2*i:], c)
	}
	return b
}

// lsKey builds a Chrome Local Storage record key: "_" + origin + "\x00" +
// format byte (0x01 = Latin-1 key) + key bytes.
func lsKey(origin, key string) []byte {
	out := append([]byte("_"+origin), 0x00, 0x01)
	return append(out, key...)
}

// lsValueLatin1 builds a Local Storage value with the 0x01 (Latin-1) prefix.
func lsValueLatin1(v string) []byte { return append([]byte{0x01}, v...) }

// lsValueUTF16 builds a Local Storage value with the 0x00 (UTF-16LE) prefix.
func lsValueUTF16(v string) []byte { return append([]byte{0x00}, utf16le(v)...) }

func main() {
	if err := os.MkdirAll("sample-data", 0o755); err != nil {
		log.Fatal(err)
	}
	writeLocalStorage(filepath.Join("sample-data", "chrome-local-storage"))
	writeSessionStorage(filepath.Join("sample-data", "chrome-session-storage"))
	log.Println("sample databases written to ./sample-data")
}

func writeLocalStorage(dir string) {
	os.RemoveAll(dir)
	d, err := leveldb.Open(dir, &db.Options{})
	if err != nil {
		log.Fatal(err)
	}
	set := func(k, v []byte) {
		if err := d.Set(k, v, nil); err != nil {
			log.Fatal(err)
		}
	}

	set([]byte("VERSION"), []byte("1"))

	type item struct{ origin, key, value string }
	latin1Items := []item{
		{"https://github.com", "color_mode", "dark"},
		{"https://github.com", "preferred_editor", `{"name":"vim","tabSize":4,"wordWrap":true}`},
		{"https://github.com", "last_visited_repo", "golang/leveldb"},
		{"https://news.ycombinator.com", "theme", "default"},
		{"https://news.ycombinator.com", "hidden_ids", "[41234991,41235102,41237745]"},
		{"https://mail.example.com", "session_prefs", `{"density":"compact","previewPane":"right","unreadFirst":true}`},
		{"https://mail.example.com", "draft_autosave", `{"id":"draft-7f3a","subject":"Quarterly report","savedAt":1730834422000}`},
		{"https://shop.example.com", "cart", `{"items":[{"sku":"KB-0042","qty":1,"price":129.99},{"sku":"MS-0007","qty":2,"price":24.5}],"currency":"USD"}`},
		{"https://shop.example.com", "recently_viewed", `["KB-0042","MN-2210","MS-0007","HD-9931"]`},
	}
	for _, it := range latin1Items {
		set(lsKey(it.origin, it.key), lsValueLatin1(it.value))
	}

	// UTF-16 values, as Chrome stores any string containing non-Latin-1 text.
	set(lsKey("https://translate.example.org", "greeting"), lsValueUTF16("こんにちは世界 — hello, world 🌍"))
	set(lsKey("https://translate.example.org", "recent_phrases"), lsValueUTF16(`["Привет","你好","مرحبا","हेलो"]`))

	// Per-origin META records (Chrome stores a small protobuf here; these
	// bytes are representative, not meaningful).
	for _, origin := range []string{
		"https://github.com", "https://news.ycombinator.com",
		"https://mail.example.com", "https://shop.example.com",
		"https://translate.example.org",
	} {
		set([]byte("META:"+origin), []byte{0x08, 0xd2, 0x85, 0xe2, 0xb0, 0x06, 0x10, 0x80, 0x02})
	}

	if err := d.Close(); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %s", dir)
}

func writeSessionStorage(dir string) {
	os.RemoveAll(dir)
	d, err := leveldb.Open(dir, &db.Options{})
	if err != nil {
		log.Fatal(err)
	}
	set := func(k, v string) {
		if err := d.Set([]byte(k), []byte(v), nil); err != nil {
			log.Fatal(err)
		}
	}

	set("version", "1")
	set("namespace-8c4f6a2e-1b7d-4c3a-9e5f-2a8b7c6d5e4f-https://github.com", "1")
	set("namespace-8c4f6a2e-1b7d-4c3a-9e5f-2a8b7c6d5e4f-https://docs.example.com", "2")
	set("next-map-id", "3")

	set("map-1-scroll_position", "1284")
	set("map-1-open_tab", "pull-requests")
	set("map-1-filter_state", `{"state":"open","sort":"updated-desc","labels":["bug"]}`)
	set("map-2-wizard_step", "3")
	set("map-2-form_draft", `{"title":"Migration plan","body":"Step one: snapshot the database..."}`)

	if err := d.Close(); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %s", dir)
}
