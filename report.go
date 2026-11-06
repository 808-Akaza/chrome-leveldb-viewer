package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"strings"
	"time"
)

// exportLimit caps one report. Past this, the file is too large to open
// comfortably and the request is rejected instead of being truncated quietly.
const exportLimit = 10000

// reportTextLimit caps a single decoded field inside the report.
const reportTextLimit = 256 << 10

type reportDoc struct {
	Source    string
	Mode      string
	Files     string
	Generated string
	Selected  int
	Total     int
	Truncated bool
	Records   []reportRecord
}

type reportRecord struct {
	N          int
	Index      int
	Type       string
	Origin     string
	StorageKey string
	Deleted    bool
	Key        string
	KeyEnc     string
	TOCKey     string
	Value      string
	ValueEnc   string
	ValueSize  string
	Truncated  bool
}

// RenderReport builds a self-contained HTML report for the given entry
// indexes, in the order they were requested. Invalid indexes are skipped.
func (s *Session) RenderReport(indexes []int) (string, []byte, error) {
	if len(indexes) == 0 {
		return "", nil, fmt.Errorf("select at least one record")
	}
	seen := make(map[int]struct{}, len(indexes))
	ordered := make([]int, 0, len(indexes))
	for _, i := range indexes {
		if i < 0 || i >= len(s.Entries) {
			continue
		}
		if _, ok := seen[i]; ok {
			continue
		}
		seen[i] = struct{}{}
		ordered = append(ordered, i)
	}
	if len(ordered) == 0 {
		return "", nil, fmt.Errorf("none of the selected records are in this database")
	}
	if len(ordered) > exportLimit {
		return "", nil, fmt.Errorf("a report can include at most %d records (%d selected)", exportLimit, len(ordered))
	}

	doc := reportDoc{
		Source:    s.Source,
		Mode:      "raw table scan",
		Files:     strings.Join(s.Files, ", "),
		Generated: time.Now().Format("2006-01-02 15:04:05 MST"),
		Selected:  len(ordered),
		Total:     len(s.Entries),
		Truncated: s.Truncated,
		Records:   make([]reportRecord, 0, len(ordered)),
	}
	if s.Mode == "db" {
		doc.Mode = "database scan"
	}
	if doc.Files == "" {
		doc.Files = "—"
	}

	for n, i := range ordered {
		e := &s.Entries[i]
		value, valueEnc, valueCut := reportValue(e)
		key := e.KeyDisplay
		if key == "" {
			key = "(empty)"
		}
		rec := reportRecord{
			N:          n + 1,
			Index:      i,
			Type:       TypeLabel(e.Chrome.Type),
			Origin:     e.Chrome.Origin,
			StorageKey: e.Chrome.StorageKey,
			Deleted:    e.Deleted,
			Key:        key,
			KeyEnc:     e.KeyEnc,
			TOCKey:     truncateRunes(key, 90),
			Value:      value,
			ValueEnc:   valueEnc,
			ValueSize:  formatSize(e.ValueSize),
			Truncated:  valueCut || e.ValueSize > len(e.Value),
		}
		doc.Records = append(doc.Records, rec)
	}

	var buf bytes.Buffer
	if err := reportTmpl.Execute(&buf, doc); err != nil {
		return "", nil, err
	}
	name := "leveldb-report-" + time.Now().Format("20060102-150405") + ".html"
	return name, buf.Bytes(), nil
}

// reportValue returns the decoded value, pretty-printed when it is JSON.
func reportValue(e *Entry) (text, enc string, truncated bool) {
	enc = e.ValEnc
	text = e.ValText
	trimmed := strings.TrimSpace(text)
	if len(trimmed) > 1 && (trimmed[0] == '{' || trimmed[0] == '[') {
		var parsed any
		if json.Unmarshal([]byte(trimmed), &parsed) == nil {
			if pretty, err := json.MarshalIndent(parsed, "", "  "); err == nil {
				text = string(pretty)
				enc = "json"
			}
		}
	}
	if text == "" {
		text = "(empty)"
	}
	if len(text) > reportTextLimit {
		text = text[:reportTextLimit] + "\n… (truncated for the report)"
		truncated = true
	}
	return text, enc, truncated
}

func formatSize(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}

var reportTmpl = template.Must(template.New("report").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>LevelDB report — {{.Source}}</title>
<style>
  :root { color-scheme: light; }
  body { margin: 0; background: #f4f1ea; color: #1c1915; font: 15px/1.5 "Segoe UI", Helvetica, Arial, sans-serif; }
  main { max-width: 880px; margin: 0 auto; padding: 32px 20px 64px; }
  h1 { font-size: 26px; margin: 0 0 4px; }
  h2 { font-size: 18px; margin: 0 0 8px; }
  .sub { color: #5c564c; margin: 0 0 20px; }
  .meta { display: grid; grid-template-columns: 140px 1fr; gap: 4px 12px; background: #fff; border: 1px solid #e2d9cc; border-radius: 10px; padding: 14px 16px; }
  .meta dt { color: #6d655b; }
  .meta dd { margin: 0; font-family: ui-monospace, "Cascadia Code", Consolas, monospace; word-break: break-word; }
  .warn { margin-top: 12px; padding: 8px 12px; background: #fff6e5; border: 1px solid #e6c98a; border-radius: 8px; color: #6a4b05; }
  ol.toc { background: #fff; border: 1px solid #e2d9cc; border-radius: 10px; padding: 12px 16px 12px 36px; }
  ol.toc li { margin: 3px 0; }
  ol.toc a { color: #1d4e89; text-decoration: none; }
  ol.toc a:hover { text-decoration: underline; }
  article { background: #fff; border: 1px solid #e2d9cc; border-radius: 10px; padding: 16px 18px; margin: 16px 0; }
  article.deleted { opacity: 0.75; }
  .k { color: #6d655b; font-size: 12px; text-transform: uppercase; letter-spacing: 0.04em; margin: 12px 0 4px; }
  pre { margin: 0; white-space: pre-wrap; word-break: break-word; font: 13px/1.45 ui-monospace, "Cascadia Code", Consolas, monospace; background: #f7f4ee; border-radius: 8px; padding: 10px 12px; }
  .tag { display: inline-block; font-size: 12px; font-weight: 650; background: #e7f0fa; color: #1d4e89; border-radius: 999px; padding: 2px 8px; margin-right: 6px; }
  .tag.del { background: #fde8e6; color: #9d2c27; }
  .enc { color: #6d655b; font-size: 12px; margin-left: 6px; }
  .note { color: #8a5a00; font-size: 12px; margin-top: 6px; }
  footer { color: #6d655b; font-size: 12px; margin-top: 24px; }
</style>
</head>
<body>
<main>
  <h1>LevelDB Viewer report</h1>
  <p class="sub">{{.Selected}} selected record{{if ne .Selected 1}}s{{end}} from {{.Source}}</p>
  <dl class="meta">
    <dt>Source</dt><dd>{{.Source}}</dd>
    <dt>Mode</dt><dd>{{.Mode}}</dd>
    <dt>Files</dt><dd>{{.Files}}</dd>
    <dt>Database records</dt><dd>{{.Total}}</dd>
    <dt>In this report</dt><dd>{{.Selected}}</dd>
    <dt>Generated</dt><dd>{{.Generated}}</dd>
  </dl>
  {{if .Truncated}}<p class="warn">The database listing itself was truncated before this report was built. Some records may be missing.</p>{{end}}

  <h2>Contents</h2>
  <ol class="toc">
    {{range .Records}}<li><a href="#r{{.N}}">{{.TOCKey}}</a></li>{{end}}
  </ol>

  {{range .Records}}
  <article id="r{{.N}}"{{if .Deleted}} class="deleted"{{end}}>
    <h2>Record {{.N}}</h2>
    <span class="tag">{{.Type}}</span>
    {{if .Deleted}}<span class="tag del">deleted</span>{{end}}
    <dl class="meta">
      <dt>Origin</dt><dd>{{if .Origin}}{{.Origin}}{{else}}—{{end}}</dd>
      <dt>Storage key</dt><dd>{{if .StorageKey}}{{.StorageKey}}{{else}}—{{end}}</dd>
      <dt>Value size</dt><dd>{{.ValueSize}}</dd>
      <dt>Database index</dt><dd>{{.Index}}</dd>
    </dl>
    <div class="k">Key <span class="enc">{{.KeyEnc}}</span></div>
    <pre>{{.Key}}</pre>
    <div class="k">Value <span class="enc">{{.ValueEnc}}</span></div>
    <pre>{{.Value}}</pre>
    {{if .Truncated}}<p class="note">This value was shortened. The size above is the original size.</p>{{end}}
  </article>
  {{end}}
  <footer>Generated by LevelDB Viewer. Source files were not modified.</footer>
</main>
</body>
</html>
`))
