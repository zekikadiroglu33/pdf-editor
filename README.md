# pdfer — PDF Text Editor

A small, self-contained web service that edits text inside PDF files: upload a PDF, select or type the text to find, type its replacement, and download the edited PDF. Includes a browser UI with a PDF.js-powered preview — drag-select text directly on the rendered page instead of retyping it.

Backend is Go (stdlib only, zero dependencies); the only external tool required is `qpdf`.

## Features

- Find/replace text inside PDF content streams, with correct `/Length` and xref rebuilding.
- Browser UI renders the uploaded PDF (via PDF.js) with a selectable text layer — drag to select text on the page and it fills the "Find text" box automatically, similar to Adobe's editor.
- Zoom controls (50%–500%) for precisely selecting small text.
- Language-aware character encoding: PDF content streams store text as raw bytes in a single-byte code page (usually WinAnsiEncoding/Windows-1252), not UTF-8. Pick "Turkish" from the language dropdown to correctly match/replace Turkish-specific letters (ş, Ş, ğ, Ğ, ı, İ) that don't exist in the default Latin table.

## Prerequisites

- Go 1.21+ (tested on 1.22+)
- `qpdf` CLI:
  - macOS: `brew install qpdf`
  - Debian/Ubuntu: `apt install qpdf`
  - Windows: [qpdf releases](https://github.com/qpdf/qpdf/releases)

## How to run

```bash
git clone https://github.com/zekikadiroglu33/pdf-editor.git
cd pdf-editor
go run .
```

The server starts on **http://localhost:8080**. Override the port with `PORT`:

```bash
PORT=3000 go run .
```

Or build a standalone binary:

```bash
go build -o pdfer .
./pdfer
```

### Testing

```bash
go test ./...
```

Fixture PDFs are built programmatically in `pdfer_test.go` (no binary `.pdf` files checked into the repo), covering the sharper edges of the format: kerning/structure preservation on a same-font replacement, `q`/`Q` graphics-state font restoration, zero-width subset-font fallback routing, and Form XObject content resolution.

Open the URL in a browser: drop in a PDF, drag-select the text you want to change directly on the rendered page (or type it manually), choose the replacement text and language, then click **Edit PDF**. The edited file previews inline and is downloadable — no page reload needed. Selecting a specific occurrence only edits that one instance; each edit builds on the last, so you can make as many changes as you want in one session.

### Docker

```bash
docker build -t pdfer .
docker run --rm -p 8080:8080 pdfer
```

The image is a multi-stage build (Go build stage + a `debian:bookworm-slim` runtime with `qpdf` installed), runs as a non-root user, and exposes `/api/health` as its `HEALTHCHECK`. Override the port with `-e PORT=3000 -p 3000:3000`.

### Production hardening

The server is meant to be safe to expose directly to the public internet with no authentication in front of it:

- Per-IP rate limiting and a process-wide concurrency cap on `/api/edit` (each request spawns two `qpdf` subprocesses).
- Request timeouts throughout: HTTP server read/write/idle timeouts, and a bounded context on every `qpdf` call and the overall edit pipeline, so a hung or adversarial upload can't tie up a worker indefinitely.
- Panic recovery, structured request logging, and graceful shutdown on `SIGTERM`.
- Strict CSP (`script-src 'self'`, no third-party CDNs — PDF.js is self-hosted under `static/vendor/`) plus standard security headers.
- TLS is expected to be terminated in front of the app (reverse proxy / platform load balancer) — the binary itself serves plain HTTP.

## API reference

| Endpoint | Method | Body | Response |
|---|---|---|---|
| `/` | GET | — | Web UI |
| `/api/health` | GET | — | `ok` |
| `/api/edit` | POST | multipart form: `file` (PDF, max 50 MB), `find` (required), `replace`, `lang` (optional: `latin` default, or `turkish`) | Edited PDF bytes; header `X-Replacements: N` |

Example with curl:

```bash
curl -o edited.pdf \
  -F file=@report.pdf \
  -F 'find=ACME Corp' \
  -F 'replace=Globex Inc' \
  http://localhost:8080/api/edit
```

Turkish text needs the matching language table:

```bash
curl -o edited.pdf \
  -F file=@rapor.pdf \
  -F 'find=çalışma' \
  -F 'replace=ofis' \
  -F 'lang=turkish' \
  http://localhost:8080/api/edit
```

Errors return plain text with status 400 (bad input — including an unknown `lang` or a character unsupported by the selected language) or 422 (PDF could not be processed).

## How it works

1. **Normalize** — `qpdf` rewrites the upload as a classic PDF: object streams disabled, content streams uncompressed, so text operators (`(Hello) Tj`) become plain bytes.
2. **Replace** — the `find`/`replace` strings are transcoded from UTF-8 into the selected language's single-byte encoding (see `encoding.go`), then matched and replaced inside every non-image stream, with `/Length` updated.
3. **Rebuild** — the file is re-serialized with a fresh `xref` table and trailer, then recompressed with `qpdf`.

## Limitations (honest ones)

- **CID/subset fonts are matched via `/ToUnicode`, not their embedded CMap**: text rendered through a CID/Type0 font is decoded using that font's `/ToUnicode` map, which is present in the vast majority of real-world PDFs (Word, Google Docs, LibreOffice, browser print-to-PDF) - but if a document omits it, that text won't match.
- **Font `/Differences` isn't parsed for matching**: the `lang` option assumes WinAnsiEncoding (`latin`) or Windows-1254 (`turkish`) for *decoding* a simple font's bytes. Replacement text that the matched font can't safely render (wrong glyph, or a byte with no `/Widths` entry in that specific subset font) is automatically routed through a bundled fallback font instead of guessing.
- **Layout is not reflowed**: replacing "Hello" with a much longer string keeps the original glyph positions, so long replacements can overlap neighboring text.
- **Text split across operators**: a phrase kerned into pieces like `(AC) -12 (ME)` won't match a search for `ACME`.
- Scanned/image-only PDFs contain no text at all — you'd need OCR first.

## Project structure

```
pdf-editor/
├── go.mod                  # module pdfer (no dependencies)
├── main.go                 # HTTP server + PDF parse/rebuild/qpdf pipeline
├── middleware.go            # panic recovery, logging, security headers, rate/concurrency limiting
├── pdftext.go               # font-aware content-stream tokenizer, decoder, and find/replace engine
├── encoding.go              # rune <-> byte tables per language (latin, turkish, ...)
├── fallbackfont.go           # bundled fallback-font PDF object builder
├── fallbackfont_data.go      # generated widths/metrics for the fallback font
├── pdfer_test.go             # regression tests (programmatically-built fixture PDFs)
├── Dockerfile
├── assets/
│   └── pdfer-fallback.ttf   # subsetted DejaVu Sans, embedded for fallback rendering
└── static/
    ├── index.html           # browser UI markup/styles
    ├── app.js                # browser UI logic: PDF.js preview, drag-to-select, zoom, language picker
    └── vendor/                # self-hosted PDF.js (required for a strict CSP - no third-party CDN)
```
