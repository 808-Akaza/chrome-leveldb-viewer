# LevelDB Viewer

A GUI application for inspecting LevelDB databases, built on
[`github.com/golang/leveldb`](https://github.com/golang/leveldb) — the Go
implementation of LevelDB. It has first-class support for the databases
Chrome keeps inside its profile directory: **Local Storage**, **Session
Storage**, and other LevelDB-backed stores (IndexedDB, Extension State,
Sync Data, …).

On Windows the portable `.exe` opens a native application window with the
full viewer inside. The same UI is also available from source, where a local
server renders it in the browser.

## Features

- **Open Chrome LevelDB folders** three ways: upload a whole folder, upload
  individual files (`*.ldb`, `MANIFEST-*`, `*.log`), or type a path on the
  machine running the server.
- **Forensically safe** — source files are copied before parsing and are
  never written to, so you can point it at a live Chrome profile.
- **IndexedDB decoding** for Chrome `.ldb` files: keys are read with Chrome's
  IndexedDB layout (database, object store, index, and the JavaScript key)
  and values are decoded from Blink/V8 structured clone into text and JSON.
- **Chrome-aware decoding**:
  - Local Storage keys (`_<origin>\x00<fmt><key>`) and values, including the
    one-byte UTF-16LE / Latin-1 format prefixes Chrome uses.
  - Local Storage `META:<origin>` and `VERSION` records.
  - Session Storage `map-<id>-<key>` and `namespace-…` records.
- **Smart fallback decoding** for everything else: UTF-8 → UTF-16LE → hex.
- **Detail panel** with every rendering of a key/value: decoded text, UTF-8,
  UTF-16LE, Latin-1, a classic hex dump, and pretty-printed JSON when the
  value parses as JSON.
- **Search and filters** across decoded keys, values, and origins, with
  per-origin and per-record-type filtering and pagination.
- **Multi-select and export.** Click selects one record. Ctrl+click
  (Cmd+click on macOS) toggles records, including across pages.
  Shift+click selects the range between the last anchor and the record
  you click. Ctrl+A selects every record matching the current filter.
  **Export report** writes the selection to a self-contained HTML file
  (decoded keys and values, pretty-printed when the value is JSON).
- **Deleted records** — deletion tombstones found in table and log files are
  shown (struck through) rather than hidden.
- Reads standalone `.ldb` / `.sst` table files and `.log` write-ahead logs,
  even without a complete database directory.

## How it reads databases

`golang/leveldb` is an incomplete port (its high-level `DB.Find` iterator is
unimplemented), so the viewer reads files the way forensic tools do, using
the library's lower-level packages:

- every sorted-table file (`*.ldb`, `*.sst`) is read with the
  [`table`](https://pkg.go.dev/github.com/golang/leveldb/table) package
  (snappy-compressed blocks included),
- every write-ahead log (`*.log`) is replayed with the
  [`record`](https://pkg.go.dev/github.com/golang/leveldb/record) package and
  its batches decoded,
- all versions are merged by sequence number so the newest version of each
  key wins, and deletion tombstones are kept and labeled.

This also sidesteps Chrome's custom comparators (e.g. IndexedDB's
`idb_cmp1`), which would prevent a normal `Open` of those databases.

## Portable Windows executable

Prebuilt, fully self-contained executables are committed under [`dist/`](dist/):

- `dist/leveldb-viewer-windows-amd64.exe` — Windows x64 (also as `.zip`)
- `dist/leveldb-viewer-windows-arm64.exe` — Windows on ARM (also as `.zip`)

Note: repository web viewers cannot preview binaries (clicking the file may
say "binary file not supported") — use the raw-file **Download** option, grab
the `.zip`, or clone/check out the repo locally to get the file.

SHA-256 checksums:

```
5d4f1e3c2b6235b87cf4c18feff58e650a32dfd85e834df315421598813fd6e0  leveldb-viewer-windows-amd64.exe
15ca04703ee72b7ce6b7e45689946e7301da235758a42e9e3d28345273ccb2e0  leveldb-viewer-windows-arm64.exe
```

No installation and no extra files: double-click the `.exe` and a native
application window opens with the full viewer inside. There is no console
window and nothing to open in a browser. The UI and the sample databases are
embedded in that single binary. Closing the window quits the app.

The window is drawn with WebView2, the component that ships with Windows 11
and with current Windows 10 (it comes with Microsoft Edge). If that component
is missing, the app falls back to your default browser and writes
`%TEMP%\leveldb-viewer.log`. Startup failures show a message box.

Optional flags, from a terminal: `-browser` opens the UI in the default
browser instead of a window; `-no-browser` runs headless; `-port N` picks the
internal port (a free port is chosen automatically if it is taken).

To rebuild the Windows executable:

```bash
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -H windowsgui" -o dist/leveldb-viewer-windows-amd64.exe .
```

With `GOOS=darwin` or `GOOS=linux` (and without `-H windowsgui`) the same
command produces a portable build for macOS or Linux. Those builds open the
UI in the default browser, because the native window is Windows-only.

## Running from source

Requires Go 1.22+.

```bash
go run ./cmd/gensample   # optional: regenerate sample Chrome-format databases
go run . -browser        # serves http://127.0.0.1:44517 and opens the browser
```

On Windows, `go run .` opens the native window instead. Flags: `-port`,
`-browser`, `-no-browser`, or the `PORT` environment variable.

```bash
go test ./...            # run the test suite
```

## Where Chrome keeps its LevelDB data

| Store | Path inside the profile directory |
| --- | --- |
| Local Storage | `Default/Local Storage/leveldb/` |
| Session Storage | `Default/Session Storage/` |
| IndexedDB | `Default/IndexedDB/https_<site>_0.indexeddb.leveldb/` |
| Extension state | `Default/Local Extension Settings/<extension id>/` |
| Sync data | `Default/Sync Data/LevelDB/` |

Profile directories live at e.g. `~/.config/google-chrome/` (Linux),
`~/Library/Application Support/Google/Chrome/` (macOS), or
`%LocalAppData%\Google\Chrome\User Data\` (Windows). Close Chrome first, or
copy the folder — the viewer never writes to the source either way.

## Project layout

```
main.go               HTTP server and API endpoints
session.go            file scanning, version merging, querying
report.go             HTML report export for the selected records
decode.go             Chrome key/value classification and text decoding
web/                  embedded frontend (HTML/CSS/JS)
cmd/gensample/        writes Chrome-format sample databases to ./sample-data
sample-data/          generated sample databases used by the "Sample databases" panel
```
