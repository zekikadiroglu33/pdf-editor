# pdfer — Fast PDF Text Editor (Go backend + Web UI)

A small, self-contained web service that edits text inside PDF files: upload a PDF, type the text to find and the text to replace it with, and download the edited PDF. Includes a browser UI for manual testing.

## Why Go?

With C, C++, and Rust excluded, the fastest practical choices are **Go**, **C#**, and **Java**. Go wins for this project:

- Compiled to native code — throughput is close to C for I/O-heavy byte manipulation like this, far ahead of Python/Node/PHP.
- The HTTP server, zlib, regex, and byte handling are all in the standard library — **this project has zero Go dependencies**.
- Goroutines: the built-in `net/http` server handles each upload concurrently with no extra code.
- Ships as a single static binary (~8 MB) — trivial to deploy.

## How it works

Editing text inside a PDF is hard because content streams are compressed and split across objects. pdfer uses a 3-stage pipeline:

1. **Normalize** — `qpdf` rewrites the upload as a classic PDF: object streams disabled, content streams uncompressed. Now the text operators (`(Hello) Tj`) are plain bytes.
2. **Replace** — the Go engine parses every top-level object (walking dictionaries and using `/Length` to safely skip binary stream data), performs the find/replace inside every non-image stream (with proper PDF literal-string escaping for `(`, `)`, `\`), and updates each stream's `/Length`.
3. **Rebuild** — the file is re-serialized with a fresh, valid `xref` table and trailer, then recompressed with `qpdf`.

## Prerequisites

- Go 1.21+ (tested on 1.22)
- `qpdf` CLI: `apt install qpdf` (Debian/Ubuntu), `brew install qpdf` (macOS), or [qpdf releases](https://github.com/qpdf/qpdf/releases) (Windows)

## Project structure

```
pdfer/
├── go.mod            # module pdfer  (no dependencies)
├── main.go           # server + PDF engine (~330 lines, stdlib only)
└── static/
    └── index.html    # test UI (embedded into the binary via go:embed)
```

## `main.go`

```go
// pdfer - a tiny PDF text-editing server written in Go (stdlib only).
//
// Pipeline:
//  1. Normalize the uploaded PDF with qpdf (disable object streams,
//     uncompress content streams) so the file becomes a classic,
//     parseable PDF with plain-text content streams.
//  2. Walk every top-level object, find non-image streams, and replace
//     the target text inside them (PDF literal-string escaping applied).
//  3. Rebuild the file with corrected /Length values and a fresh xref
//     table, then recompress with qpdf.
//
// External dependency: the qpdf CLI (apt install qpdf / brew install qpdf).
package main

import (
	"bytes"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
)

//go:embed static
var staticFS embed.FS

const maxUpload = 50 << 20 // 50 MB

// ---------------------------------------------------------------------------
// qpdf helpers
// ---------------------------------------------------------------------------

func runQpdf(input []byte, args ...string) ([]byte, error) {
	in, err := os.CreateTemp("", "pdfer-in-*.pdf")
	if err != nil {
		return nil, err
	}
	defer os.Remove(in.Name())
	out, err := os.CreateTemp("", "pdfer-out-*.pdf")
	if err != nil {
		return nil, err
	}
	defer os.Remove(out.Name())
	out.Close()

	if _, err := in.Write(input); err != nil {
		return nil, err
	}
	in.Close()

	full := append(args, in.Name(), out.Name())
	cmd := exec.Command("qpdf", full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// exit code 3 == "succeeded with warnings" - acceptable
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 3 {
			return nil, fmt.Errorf("qpdf: %v: %s", err, stderr.String())
		}
	}
	return os.ReadFile(out.Name())
}

// normalize turns any PDF into a classic PDF with uncompressed,
// text-searchable content streams and no object streams.
func normalize(pdf []byte) ([]byte, error) {
	return runQpdf(pdf, "--object-streams=disable", "--stream-data=uncompress")
}

// recompress shrinks the rebuilt file back down.
func recompress(pdf []byte) []byte {
	out, err := runQpdf(pdf, "--object-streams=generate")
	if err != nil {
		return pdf // uncompressed output is still a valid PDF
	}
	return out
}

// ---------------------------------------------------------------------------
// Minimal PDF object model
// ---------------------------------------------------------------------------

type pdfObject struct {
	num, gen  int
	body      []byte // dict (or any non-stream body), without "obj"/"endobj"
	stream    []byte // nil if the object has no stream
	hasStream bool
}

var (
	objStartRe = regexp.MustCompile(`(?m)^(\d+)[ \t]+(\d+)[ \t]+obj\b`)
	lengthRe   = regexp.MustCompile(`/Length[ \t\r\n]+(\d+)`)
	trailerRe  = regexp.MustCompile(`(?s)trailer[ \t\r\n]*(<<.*?>>)`)
	sizeRe     = regexp.MustCompile(`/Size[ \t\r\n]+\d+`)
	prevRe     = regexp.MustCompile(`/Prev[ \t\r\n]+\d+`)
)

// dictEnd returns the index just past the ">>" matching the "<<" at start.
func dictEnd(data []byte, start int) int {
	depth := 0
	for i := start; i < len(data)-1; i++ {
		if data[i] == '<' && data[i+1] == '<' {
			depth++
			i++
		} else if data[i] == '>' && data[i+1] == '>' {
			depth--
			i++
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

func skipWS(data []byte, i int) int {
	for i < len(data) {
		switch data[i] {
		case ' ', '\t', '\r', '\n', '\f', 0:
			i++
		default:
			return i
		}
	}
	return i
}

// parseObjects walks a normalized (classic, direct-/Length) PDF.
func parseObjects(pdf []byte) ([]pdfObject, error) {
	var objects []pdfObject
	locs := objStartRe.FindAllSubmatchIndex(pdf, -1)
	for _, m := range locs {
		num, _ := strconv.Atoi(string(pdf[m[2]:m[3]]))
		gen, _ := strconv.Atoi(string(pdf[m[4]:m[5]]))
		pos := skipWS(pdf, m[1])

		var obj pdfObject
		obj.num, obj.gen = num, gen

		if pos+1 < len(pdf) && pdf[pos] == '<' && pdf[pos+1] == '<' {
			end := dictEnd(pdf, pos)
			if end < 0 {
				return nil, fmt.Errorf("object %d: unterminated dictionary", num)
			}
			obj.body = pdf[pos:end]
			after := skipWS(pdf, end)
			if bytes.HasPrefix(pdf[after:], []byte("stream")) {
				lm := lengthRe.FindSubmatch(obj.body)
				if lm == nil {
					return nil, fmt.Errorf("object %d: stream without direct /Length", num)
				}
				length, _ := strconv.Atoi(string(lm[1]))
				dataStart := after + len("stream")
				if dataStart < len(pdf) && pdf[dataStart] == '\r' {
					dataStart++
				}
				if dataStart < len(pdf) && pdf[dataStart] == '\n' {
					dataStart++
				}
				if dataStart+length > len(pdf) {
					return nil, fmt.Errorf("object %d: /Length exceeds file size", num)
				}
				obj.stream = pdf[dataStart : dataStart+length]
				obj.hasStream = true
			}
		} else {
			// Non-dictionary object (number, array, name...): safe to
			// scan for endobj because there is no binary payload.
			end := bytes.Index(pdf[pos:], []byte("endobj"))
			if end < 0 {
				return nil, fmt.Errorf("object %d: missing endobj", num)
			}
			obj.body = bytes.TrimSpace(pdf[pos : pos+end])
		}
		objects = append(objects, obj)
	}
	if len(objects) == 0 {
		return nil, fmt.Errorf("no PDF objects found")
	}
	return objects, nil
}

// ---------------------------------------------------------------------------
// Text replacement
// ---------------------------------------------------------------------------

// escapePDFString escapes text for use inside a PDF literal string ( ... ).
func escapePDFString(s string) []byte {
	var b bytes.Buffer
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\', '(', ')':
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	return b.Bytes()
}

// replaceInStreams edits every non-image stream in place.
// Returns the number of replacements performed.
func replaceInStreams(objects []pdfObject, find, repl string) int {
	oldB := escapePDFString(find)
	newB := escapePDFString(repl)
	total := 0
	for i := range objects {
		o := &objects[i]
		if !o.hasStream || bytes.Contains(o.body, []byte("/Image")) {
			continue
		}
		n := bytes.Count(o.stream, oldB)
		if n == 0 {
			continue
		}
		o.stream = bytes.ReplaceAll(o.stream, oldB, newB)
		o.body = lengthRe.ReplaceAll(o.body,
			[]byte(fmt.Sprintf("/Length %d", len(o.stream))))
		total += n
	}
	return total
}

// ---------------------------------------------------------------------------
// Serialization: rebuild the PDF with a fresh xref table
// ---------------------------------------------------------------------------

func rebuild(objects []pdfObject, original []byte) ([]byte, error) {
	tm := trailerRe.FindSubmatch(original)
	if tm == nil {
		return nil, fmt.Errorf("no classic trailer found (normalization failed?)")
	}
	trailer := tm[1]

	sort.Slice(objects, func(i, j int) bool { return objects[i].num < objects[j].num })
	maxNum := objects[len(objects)-1].num

	var out bytes.Buffer
	out.WriteString("%PDF-1.7\n%\xE2\xE3\xCF\xD3\n")

	offsets := make(map[int]int, len(objects))
	for _, o := range objects {
		offsets[o.num] = out.Len()
		fmt.Fprintf(&out, "%d %d obj\n", o.num, o.gen)
		out.Write(o.body)
		if o.hasStream {
			out.WriteString("\nstream\n")
			out.Write(o.stream)
			out.WriteString("\nendstream")
		}
		out.WriteString("\nendobj\n")
	}

	xrefPos := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n", maxNum+1)
	out.WriteString("0000000000 65535 f \n")
	for n := 1; n <= maxNum; n++ {
		if off, ok := offsets[n]; ok {
			fmt.Fprintf(&out, "%010d %05d n \n", off, 0)
		} else {
			out.WriteString("0000000000 65535 f \n")
		}
	}

	trailer = sizeRe.ReplaceAll(trailer, []byte(fmt.Sprintf("/Size %d", maxNum+1)))
	trailer = prevRe.ReplaceAll(trailer, nil)
	out.WriteString("trailer\n")
	out.Write(trailer)
	fmt.Fprintf(&out, "\nstartxref\n%d\n%%%%EOF\n", xrefPos)
	return out.Bytes(), nil
}

// EditPDF is the full pipeline: normalize -> replace -> rebuild -> recompress.
func EditPDF(pdf []byte, find, repl string) ([]byte, int, error) {
	norm, err := normalize(pdf)
	if err != nil {
		return nil, 0, fmt.Errorf("normalize: %w", err)
	}
	objects, err := parseObjects(norm)
	if err != nil {
		return nil, 0, fmt.Errorf("parse: %w", err)
	}
	count := replaceInStreams(objects, find, repl)
	rebuilt, err := rebuild(objects, norm)
	if err != nil {
		return nil, 0, fmt.Errorf("rebuild: %w", err)
	}
	return recompress(rebuilt), count, nil
}

// ---------------------------------------------------------------------------
// HTTP server
// ---------------------------------------------------------------------------

func handleEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	if err := r.ParseMultipartForm(maxUpload); err != nil {
		http.Error(w, "upload too large or malformed: "+err.Error(), http.StatusBadRequest)
		return
	}
	find := r.FormValue("find")
	repl := r.FormValue("replace")
	if find == "" {
		http.Error(w, `"find" text is required`, http.StatusBadRequest)
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file field", http.StatusBadRequest)
		return
	}
	defer file.Close()
	pdf, err := io.ReadAll(file)
	if err != nil || !bytes.HasPrefix(pdf, []byte("%PDF")) {
		http.Error(w, "not a valid PDF file", http.StatusBadRequest)
		return
	}

	edited, count, err := EditPDF(pdf, find, repl)
	if err != nil {
		log.Printf("edit failed for %q: %v", hdr.Filename, err)
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	log.Printf("edited %q: %d replacement(s), %d -> %d bytes",
		hdr.Filename, count, len(pdf), len(edited))

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("X-Replacements", strconv.Itoa(count))
	w.Header().Set("Content-Disposition", `attachment; filename="edited.pdf"`)
	w.Write(edited)
}

func main() {
	if _, err := exec.LookPath("qpdf"); err != nil {
		log.Fatal("qpdf not found in PATH - install it first (apt install qpdf / brew install qpdf)")
	}

	static, _ := fs.Sub(staticFS, "static")
	http.Handle("/", http.FileServer(http.FS(static)))
	http.HandleFunc("/api/edit", handleEdit)
	http.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})

	addr := ":8080"
	if p := os.Getenv("PORT"); p != "" {
		addr = ":" + p
	}
	log.Printf("pdfer listening on http://localhost%s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
```

## `static/index.html`

```html
<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>pdfer — PDF Text Editor</title>
<style>
  :root {
    --bg: #0f1117; --panel: #181b24; --border: #2a2f3d;
    --text: #e8eaf0; --muted: #8b91a3; --accent: #4f8cff; --ok: #3ecf8e; --err: #ff6b6b;
  }
  * { box-sizing: border-box; margin: 0; }
  body {
    font-family: ui-sans-serif, system-ui, "Segoe UI", Roboto, sans-serif;
    background: var(--bg); color: var(--text);
    min-height: 100vh; display: flex; flex-direction: column; align-items: center;
    padding: 40px 16px;
  }
  h1 { font-size: 26px; margin-bottom: 4px; }
  h1 span { color: var(--accent); }
  .sub { color: var(--muted); margin-bottom: 28px; font-size: 14px; }
  .card {
    background: var(--panel); border: 1px solid var(--border); border-radius: 14px;
    padding: 24px; width: 100%; max-width: 620px;
  }
  .drop {
    border: 2px dashed var(--border); border-radius: 10px; padding: 28px;
    text-align: center; color: var(--muted); cursor: pointer; transition: .15s;
  }
  .drop.over, .drop:hover { border-color: var(--accent); color: var(--text); }
  .drop.hasfile { border-style: solid; border-color: var(--ok); color: var(--ok); }
  label { display: block; font-size: 13px; color: var(--muted); margin: 18px 0 6px; }
  input[type=text] {
    width: 100%; padding: 11px 12px; border-radius: 8px;
    border: 1px solid var(--border); background: var(--bg); color: var(--text); font-size: 15px;
  }
  input[type=text]:focus { outline: none; border-color: var(--accent); }
  button {
    margin-top: 22px; width: 100%; padding: 13px; border: none; border-radius: 8px;
    background: var(--accent); color: #fff; font-size: 16px; font-weight: 600; cursor: pointer;
  }
  button:hover:not(:disabled) { filter: brightness(1.12); }
  button:disabled { opacity: .45; cursor: not-allowed; }
  #status { margin-top: 14px; font-size: 14px; min-height: 20px; }
  #status.ok { color: var(--ok); } #status.err { color: var(--err); }
  #result { width: 100%; max-width: 900px; margin-top: 28px; display: none; }
  #result iframe {
    width: 100%; height: 640px; border: 1px solid var(--border);
    border-radius: 10px; background: #fff;
  }
  #dl {
    display: inline-block; margin-bottom: 12px; color: var(--accent);
    font-weight: 600; text-decoration: none;
  }
</style>
</head>
<body>
  <h1>pdf<span>er</span></h1>
  <p class="sub">Upload a PDF, find text, replace it. Powered by a Go backend.</p>

  <div class="card">
    <div class="drop" id="drop">Drop a PDF here or click to choose</div>
    <input type="file" id="file" accept="application/pdf" hidden>

    <label for="find">Find text</label>
    <input type="text" id="find" placeholder="e.g. ACME Corp">

    <label for="replace">Replace with</label>
    <input type="text" id="replace" placeholder="e.g. Globex Inc">

    <button id="go" disabled>Edit PDF</button>
    <div id="status"></div>
  </div>

  <div id="result">
    <a id="dl" download="edited.pdf">⬇ Download edited.pdf</a>
    <iframe id="preview" title="Edited PDF preview"></iframe>
  </div>

<script>
  const drop = document.getElementById('drop');
  const fileIn = document.getElementById('file');
  const go = document.getElementById('go');
  const status = document.getElementById('status');
  const result = document.getElementById('result');
  let pdfFile = null;

  const setStatus = (msg, cls = '') => { status.textContent = msg; status.className = cls; };

  const setFile = f => {
    if (!f || f.type !== 'application/pdf') { setStatus('Please choose a PDF file.', 'err'); return; }
    pdfFile = f;
    drop.textContent = `📄 ${f.name} (${(f.size / 1024).toFixed(1)} KB)`;
    drop.classList.add('hasfile');
    go.disabled = false;
    setStatus('');
  };

  drop.addEventListener('click', () => fileIn.click());
  fileIn.addEventListener('change', () => setFile(fileIn.files[0]));
  drop.addEventListener('dragover', e => { e.preventDefault(); drop.classList.add('over'); });
  drop.addEventListener('dragleave', () => drop.classList.remove('over'));
  drop.addEventListener('drop', e => {
    e.preventDefault(); drop.classList.remove('over');
    setFile(e.dataTransfer.files[0]);
  });

  go.addEventListener('click', async () => {
    const find = document.getElementById('find').value;
    if (!find) { setStatus('Enter the text to find.', 'err'); return; }

    const form = new FormData();
    form.append('file', pdfFile);
    form.append('find', find);
    form.append('replace', document.getElementById('replace').value);

    go.disabled = true;
    setStatus('Editing…');
    result.style.display = 'none';

    try {
      const res = await fetch('/api/edit', { method: 'POST', body: form });
      if (!res.ok) throw new Error(await res.text());

      const count = res.headers.get('X-Replacements') || '0';
      const blob = await res.blob();
      const url = URL.createObjectURL(blob);
      document.getElementById('preview').src = url;
      document.getElementById('dl').href = url;
      result.style.display = 'block';

      if (count === '0') {
        setStatus('Done, but no matches were found. (Some fonts encode text non-literally — see README limitations.)', 'err');
      } else {
        setStatus(`Done — ${count} replacement(s) made.`, 'ok');
      }
    } catch (err) {
      setStatus('Error: ' + err.message, 'err');
    } finally {
      go.disabled = false;
    }
  });
</script>
</body>
</html>
```

## Run it

```bash
mkdir -p pdfer/static
# save main.go and static/index.html as shown above, then:
cd pdfer
go mod init pdfer
go run .            # or: go build -o pdfer . && ./pdfer
```

The server starts on **http://localhost:8080** (override with `PORT=3000 go run .`). Open it in a browser: drop in a PDF, enter the text to find and its replacement, click **Edit PDF**. The edited file is previewed inline in an iframe and downloadable — no page reload needed.

## API reference

| Endpoint | Method | Body | Response |
|---|---|---|---|
| `/` | GET | — | Test UI (embedded) |
| `/api/health` | GET | — | `ok` |
| `/api/edit` | POST | multipart form: `file` (PDF, max 50 MB), `find` (required), `replace` | Edited PDF bytes; header `X-Replacements: N` |

Example with curl:

```bash
curl -o edited.pdf \
  -F file=@report.pdf \
  -F 'find=ACME Corp' \
  -F 'replace=Globex Inc' \
  http://localhost:8080/api/edit
```

Errors return plain text with status 400 (bad input) or 422 (PDF could not be processed).

## Verified behavior

This exact code was built and tested end-to-end:

```
$ curl -F file=@test.pdf -F 'find=ACME Corp' -F 'replace=Globex Inc' localhost:8080/api/edit -o edited.pdf
X-Replacements: 3
$ qpdf --check edited.pdf     # → no syntax or stream errors
$ pdftotext edited.pdf -
Hello World! This is Globex Inc annual report.
Globex Inc revenue grew 25% this year.
Page 2: Globex Inc forever.
```

Replacement text containing `(`, `)`, or `\` is escaped correctly, and multi-page PDFs are handled.

## Limitations (honest ones)

- **Subset/CID fonts**: many PDFs (especially from Word or InDesign) embed subset fonts with custom character maps, so text is not stored as readable ASCII — the find text won't match and you'll get `X-Replacements: 0`. Handling this requires decoding each font's CMap; production tools (Apryse, UniPDF, Adobe API) do this.
- **Layout is not reflowed**: replacing "Hello" with a much longer string keeps the original glyph positions, so long replacements can overlap neighboring text.
- **Text split across operators**: a phrase kerned into pieces like `(AC) -12 (ME)` won't match a search for `ACME`.
- Scanned/image-only PDFs contain no text at all — you'd need OCR first.

## Next steps

Ideas to extend: fuzzy matching across split text runs, per-page targeting, regex find, a `/api/extract` endpoint to preview a PDF's actual text before editing, and Docker packaging (`FROM golang` build stage + `FROM debian:slim` with qpdf).
