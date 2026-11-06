// Command leveldb-viewer is a GUI application for inspecting LevelDB
// databases, with first-class support for the databases Chrome keeps in its
// profile directory (Local Storage, Session Storage, IndexedDB, ...).
//
// It is built on github.com/golang/leveldb, the Go implementation of LevelDB.
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

// The sample databases are compiled into the binary so a single portable
// executable can demo itself anywhere; they are extracted to a temp folder
// on first use when no ./sample-data directory exists.
//
//go:embed all:sample-data
var samplesFS embed.FS

var (
	sessionsMu sync.Mutex
	sessions   = map[string]*Session{}
	nextID     = 1
)

func main() {
	port := flag.Int("port", 44517, "port to listen on")
	browserMode := flag.Bool("browser", false, "serve the UI to the default browser instead of opening a native window")
	noBrowser := flag.Bool("no-browser", false, "headless: serve only, open neither a window nor a browser")
	flag.Parse()
	if env := os.Getenv("PORT"); env != "" {
		if p, err := strconv.Atoi(env); err == nil {
			*port = p
		}
	}

	// Mirror all output into a log file so problems are diagnosable even if
	// the console window closes (e.g. when double-clicked on Windows).
	logPath := filepath.Join(os.TempDir(), "leveldb-viewer.log")
	if lf, err := os.Create(logPath); err == nil {
		defer lf.Close()
		log.SetOutput(io.MultiWriter(os.Stderr, lf))
	}

	staticFS, err := fs.Sub(webFS, "web")
	if err != nil {
		fatal(err, logPath)
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(staticFS)))
	mux.HandleFunc("/api/open", handleOpen)
	mux.HandleFunc("/api/upload", handleUpload)
	mux.HandleFunc("/api/samples", handleSamples)
	mux.HandleFunc("/api/session/", handleSession)

	// Bind the preferred port, but fall back to any free port rather than
	// exiting — the chosen port may be taken by another program.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		log.Printf("port %d is unavailable (%v); picking a free port instead", *port, err)
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fatal(fmt.Errorf("could not listen on any port: %w", err), logPath)
		}
	}

	url := "http://" + ln.Addr().String()
	log.Printf("LevelDB Viewer running at %s", url)

	// The HTTP server is internal plumbing; it runs in the background while
	// the native window (or the fallback browser session) is in charge.
	go func() {
		fatal(http.Serve(ln, mux), logPath)
	}()

	if !*browserMode && !*noBrowser {
		if err := runGUI(url); err == nil {
			log.Printf("window closed; exiting")
			return
		} else {
			log.Printf("native window unavailable (%v); falling back to the browser", err)
		}
	}
	if !*noBrowser {
		time.Sleep(100 * time.Millisecond)
		openBrowser(url)
	}
	log.Printf("Press Ctrl+C to quit.")
	select {}
}

// fatal reports a fatal error: logged (also to the log file), surfaced in a
// native dialog on Windows (the exe has no console window), then exit.
func fatal(err error, logPath string) {
	log.Printf("fatal: %v", err)
	log.Printf("(this log is also saved to %s)", logPath)
	notifyFatal(fmt.Sprintf("LevelDB Viewer could not start:\n\n%v\n\nA log was saved to:\n%s", err, logPath))
	os.Exit(1)
}

// openBrowser opens the default browser at url; failures are only logged
// since the URL is printed anyway.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("could not open browser: %v", err)
	}
}

// samplesDir returns a directory containing the sample databases: the local
// ./sample-data checkout when present, otherwise the embedded copies
// extracted into the OS temp directory.
func samplesDir() (string, error) {
	if info, err := os.Stat("sample-data"); err == nil && info.IsDir() {
		return "sample-data", nil
	}
	dir := filepath.Join(os.TempDir(), "leveldb-viewer-samples")
	err := fs.WalkDir(samplesFS, "sample-data", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(path, "sample-data")
		target := filepath.Join(dir, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := samplesFS.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		return "", err
	}
	return dir, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func registerSession(s *Session) string {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	id := strconv.Itoa(nextID)
	nextID++
	sessions[id] = s
	return id
}

func getSession(id string) *Session {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	return sessions[id]
}

// handleOpen opens a LevelDB database (or a single .ldb/.sst table file) from
// a path on the machine the server runs on.
func handleOpen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	req.Path = strings.TrimSpace(req.Path)
	if req.Path == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("path is required"))
		return
	}
	s, err := OpenPath(req.Path)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	id := registerSession(s)
	writeJSON(w, http.StatusOK, s.Summary(id))
}

// handleUpload accepts LevelDB files (the contents of e.g. a "Local Storage/
// leveldb" folder, or individual .ldb files) and opens them.
func handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	if err := r.ParseMultipartForm(512 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var files []UploadedFile
	for _, fhs := range r.MultipartForm.File {
		for _, fh := range fhs {
			f, err := fh.Open()
			if err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			data, err := io.ReadAll(f)
			f.Close()
			if err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			files = append(files, UploadedFile{Name: filepath.Base(fh.Filename), Data: data})
		}
	}
	if len(files) == 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("no files uploaded"))
		return
	}
	s, err := OpenUploaded(files)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	id := registerSession(s)
	writeJSON(w, http.StatusOK, s.Summary(id))
}

// handleSamples lists bundled sample databases from ./sample-data.
func handleSamples(w http.ResponseWriter, r *http.Request) {
	type sample struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	var out []sample
	if dir, err := samplesDir(); err == nil {
		entries, err := os.ReadDir(dir)
		if err == nil {
			for _, e := range entries {
				if e.IsDir() {
					abs, _ := filepath.Abs(filepath.Join(dir, e.Name()))
					out = append(out, sample{Name: e.Name(), Path: abs})
				}
			}
			sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"samples": out})
}

// handleSession serves /api/session/{id}/entries, /entry, and /report.
func handleSession(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/session/")
	parts := strings.SplitN(rest, "/", 2)
	s := getSession(parts[0])
	if s == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("unknown session"))
		return
	}
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	switch action {
	case "", "summary":
		writeJSON(w, http.StatusOK, s.Summary(parts[0]))
	case "entries":
		q := r.URL.Query()
		offset, _ := strconv.Atoi(q.Get("offset"))
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit <= 0 || limit > 500 {
			limit = 100
		}
		res := s.Query(q.Get("q"), q.Get("type"), q.Get("origin"), offset, limit, q.Get("ids") == "1")
		writeJSON(w, http.StatusOK, res)
	case "report":
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
			return
		}
		var req struct {
			Indexes []int `json:"indexes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid report request"))
			return
		}
		name, body, err := s.RenderReport(req.Indexes)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	case "entry":
		idx, err := strconv.Atoi(r.URL.Query().Get("i"))
		if err != nil || idx < 0 || idx >= len(s.Entries) {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("bad entry index"))
			return
		}
		writeJSON(w, http.StatusOK, s.EntryDetail(idx))
	default:
		writeErr(w, http.StatusNotFound, fmt.Errorf("unknown action %q", action))
	}
}
