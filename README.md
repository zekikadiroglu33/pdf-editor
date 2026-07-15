<p align="center"><img src="static/logo.svg" width="64" height="64" alt="pdfer logo"></p>

# pdfer — PDF Text Editor

A small, self-contained web service that edits text inside PDF files: upload a PDF, select or type the text to find, type its replacement, and download the edited PDF. Includes a browser UI with a PDF.js-powered preview — drag-select text directly on the rendered page instead of retyping it.

Backend is Go (stdlib only, zero Go dependencies); it shells out to a small number of external CLI tools, the same way it always has for `qpdf` (see Prerequisites).

## Features

- Find/replace text inside PDF content streams, with correct `/Length` and xref rebuilding.
- Browser UI renders the uploaded PDF (via PDF.js) with a selectable text layer — drag to select text on the page and it fills the "Find text" box automatically, similar to Adobe's editor.
- Zoom controls (50%–500%) for precisely selecting small text.
- Language-aware character encoding: PDF content streams store text as raw bytes in a single-byte code page (usually WinAnsiEncoding/Windows-1252), not UTF-8. Pick "Turkish" from the language dropdown to correctly match/replace Turkish-specific letters (ş, Ş, ğ, Ğ, ı, İ) that don't exist in the default Latin table.
- Optional regex find/replace: check "Use regex" to treat "Find text" as a Go regexp (RE2 — immune to catastrophic backtracking regardless of the pattern) matched against the document's actual text, with capture-group expansion in the replacement (`$1`, `${name}`, ...). Drag-to-select is literal-only and is disabled while regex mode is on, since occurrence targeting doesn't carry over to arbitrary patterns.
- Undo: each successful edit is chainable (see below) and reversible — the "Undo" button in the toolbar reverts to the pre-edit version, up to 5 steps back in the current session.
- Make as many edits as you want in one session without the page (or the download) refreshing on every single change: every completed edit's replacement text is highlighted in yellow directly on the rendered preview so you can review everything you've changed so far, and the Download link only appears once you click "Confirm changes" — so you always review before a file is handed to you, and any further edit or undo after confirming hides the stale download again until you re-confirm.
- **PDF converter** (`convert*.go`, a separate module from the editor above): a "Convert" tab with its own upload/download flow converts a PDF to **Word (.docx), PowerPoint (.pptx), Excel (.xlsx), HTML, plain text, SVG, PNG, JPEG, TIFF, or WEBP**. The document formats are best-effort text extraction, not a layout-faithful conversion; the image formats are actual rendered pages. See [Converting to other formats](#converting-to-other-formats) and [Limitations](#limitations-honest-ones) below.

## Prerequisites

- Go 1.21+ (tested on 1.22+)
- `qpdf` CLI (required — the app won't start without it):
  - macOS: `brew install qpdf`
  - Debian/Ubuntu: `apt install qpdf`
  - Windows: [qpdf releases](https://github.com/qpdf/qpdf/releases)
- `pdftoppm` (from poppler), optional — only needed for the PNG/JPEG/TIFF/WEBP conversion formats; every other feature (editing, and the docx/pptx/xlsx/html/txt/svg conversions) works without it. Its absence only disables those four formats (a clear error is returned if you request one), it doesn't stop the server from starting.
  - macOS: `brew install poppler`
  - Debian/Ubuntu: `apt install poppler-utils`
- `cwebp` (from webp), optional — only needed for the WEBP conversion format specifically (poppler can rasterize directly to PNG/JPEG/TIFF, but not WEBP, so WEBP output is produced by rendering to PNG and re-encoding with `cwebp`).
  - macOS: `brew install webp`
  - Debian/Ubuntu: `apt install webp`

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

Fixture PDFs are built programmatically in `pdfer_test.go` (no binary `.pdf` files checked into the repo), covering the sharper edges of the format: kerning/structure preservation on a same-font replacement, `q`/`Q` graphics-state font restoration, zero-width subset-font fallback routing, Form XObject content resolution, and regex find/replace (capture-group expansion, invalid-pattern rejection).

Open the URL in a browser: drop in a PDF, drag-select the text you want to change directly on the rendered page (or type it manually, optionally as a regex), choose the replacement text and language, then click **Edit PDF**. Selecting a specific occurrence only edits that one instance; each edit builds on the last, so you can make as many changes as you want in one session — each one is highlighted in yellow on the preview as you go, and **Undo** in the toolbar steps back through up to 5 prior versions (highlights included) if an edit didn't go as planned. Nothing is downloadable yet at this point: once you're happy with the highlighted changes, click **Confirm changes & prepare download** to reveal the inline preview and Download link for that exact state — make another edit afterward and it hides again until you re-confirm.

### Docker

```bash
docker build -t pdfer .
docker run --rm -p 8080:8080 pdfer
```

The image is a multi-stage build (Go build stage + a `debian:bookworm-slim` runtime with `qpdf` installed), runs as a non-root user, and exposes `/api/health` as its `HEALTHCHECK`. Override the port with `-e PORT=3000 -p 3000:3000`.

### Deploying on a VPS (Caddy + automatic HTTPS)

`docker-compose.yml` + `Caddyfile` bundle the app with [Caddy](https://caddyserver.com) as a reverse proxy, which gets you free, automatic HTTPS (Let's Encrypt) with no manual certificate handling. `pdfer` itself is not published to the host — only Caddy is reachable from outside, on 80/443.

1. Point a DNS A/AAAA record for your domain at the server's IP.
2. `cp .env.example .env` and set `DOMAIN=your-domain.com`.
3. `docker compose up -d --build`

Caddy issues and renews the certificate automatically on first request. Logs: `docker compose logs -f`.

### Production hardening

The server is meant to be safe to expose directly to the public internet with no authentication in front of it:

- Per-IP rate limiting and a process-wide concurrency cap on `/api/edit` (each request spawns two `qpdf` subprocesses).
- Request timeouts throughout: HTTP server read/write/idle timeouts, and a bounded context on every `qpdf` call and the overall edit pipeline, so a hung or adversarial upload can't tie up a worker indefinitely.
- Panic recovery, structured request logging, and graceful shutdown on `SIGTERM`.
- Strict CSP (`script-src 'self'`, no third-party CDNs — PDF.js is self-hosted under `static/vendor/`) plus standard security headers.
- TLS is expected to be terminated in front of the app (reverse proxy / platform load balancer) — the binary itself serves plain HTTP.

### Sizing

Measured with a synthetic ~46 MB PDF (near the 50 MB upload cap): a single edit peaks around **400 MB** RSS (`pdfer` + its two sequential `qpdf` subprocesses) and takes ~5-6s. Eight concurrent worst-case-sized edits (the default concurrency cap) peaked around **1.5 GB** combined RSS with no failures or timeouts, just slower per-request latency under CPU contention (timeouts have comfortable headroom even then). Real-world documents are almost always far smaller than 46 MB, so this is a deliberately pessimistic ceiling, not the expected case.

- **Memory**: 2 GB minimum, 4 GB recommended for headroom.
- **CPU**: 2 vCPUs minimum; more improves worst-case latency under concurrent large uploads, not correctness.
- Running on a 1 GB or smaller instance: lower the concurrency limit in `main.go` (`newConcurrencyLimiter(8)`) to something like 3-4 to stay inside that budget.

## API reference

| Endpoint | Method | Body | Response |
|---|---|---|---|
| `/` | GET | — | Web UI |
| `/api/health` | GET | — | `ok` |
| `/api/edit` | POST | multipart form: `file` (PDF, max 50 MB), `find` (required), `replace`, `lang` (optional: `latin` default, or `turkish`), `regex` (optional: `true` to treat `find` as a regexp), `page`/`occurrence` (optional: target a single match) | Edited PDF bytes; header `X-Replacements: N` |
| `/api/convert` | POST | multipart form: `file` (PDF, max 50 MB), `format` (optional: `docx` default, or `pptx`/`xlsx`/`html`/`txt`/`svg`/`png`/`jpeg`/`tiff`/`webp`), `lang` (optional: `latin` default, or `turkish` — only affects decoding simple-font bytes, see `encoding.go`; ignored for the image formats, which don't decode text at all) | Converted file bytes (content type/filename match `format`; a multi-page image conversion instead returns a `.zip` of one image per page — see below); header `X-Pages: N` |

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

Regex mode uses Go's `regexp` syntax (RE2) for `find`, and `regexp.Expand` syntax for `replace` (`$1`, `${name}`, ...):

```bash
curl -o edited.pdf \
  -F file=@report.pdf \
  -F 'find=Invoice (\d+)-(\d+)' \
  -F 'replace=INV-$2/$1' \
  -F 'regex=true' \
  http://localhost:8080/api/edit
```

Note: with `curl -F`, escape/avoid literal parentheses in the field value or use `--form-string` instead of `-F` — curl's own multipart syntax treats unescaped `(`/`,` specially in `-F` values. This only affects the `curl` CLI, not the browser UI (which uses `FormData` and sends field values literally).

Converting to Word (the default format):

```bash
curl -o converted.docx \
  -F file=@report.pdf \
  http://localhost:8080/api/convert
```

Converting to another format — pass `format`:

```bash
curl -o converted.pptx -F file=@report.pdf -F 'format=pptx' http://localhost:8080/api/convert
curl -o converted.xlsx -F file=@report.pdf -F 'format=xlsx' http://localhost:8080/api/convert
curl -o converted.html -F file=@report.pdf -F 'format=html' http://localhost:8080/api/convert
curl -o converted.txt  -F file=@report.pdf -F 'format=txt'  http://localhost:8080/api/convert
curl -o converted.svg  -F file=@report.pdf -F 'format=svg'  http://localhost:8080/api/convert
```

Converting to an image format (requires `pdftoppm`, and `cwebp` for `webp` — see Prerequisites). A single-page PDF downloads as one image; a multi-page PDF downloads as a `.zip` of `page-1.<ext>`, `page-2.<ext>`, ... instead, since there's no single-image container to merge pages into:

```bash
curl -o converted.png  -F file=@report.pdf -F 'format=png'  http://localhost:8080/api/convert
curl -o converted.jpg  -F file=@report.pdf -F 'format=jpeg' http://localhost:8080/api/convert
curl -o converted.tif  -F file=@report.pdf -F 'format=tiff' http://localhost:8080/api/convert
curl -o converted.webp -F file=@report.pdf -F 'format=webp' http://localhost:8080/api/convert
```

Errors return plain text with status 400 (bad input — including an unknown `lang`, an unknown `format`, an invalid regex, or a character unsupported by the selected language), 422 (PDF could not be processed), or 503 (an image format was requested but `pdftoppm`/`cwebp` isn't installed on this server — see Prerequisites).

## How it works

1. **Normalize** — `qpdf` rewrites the upload as a classic PDF: object streams disabled, content streams uncompressed, so text operators (`(Hello) Tj`) become plain bytes.
2. **Replace** — the `find`/`replace` strings are transcoded from UTF-8 into the selected language's single-byte encoding (see `encoding.go`), then matched and replaced inside every non-image stream, with `/Length` updated.
3. **Rebuild** — the file is re-serialized with a fresh `xref` table and trailer, then recompressed with `qpdf`.

### Converting to other formats

`convert.go` and its per-format siblings (`convert_docx.go`, `convert_pptx.go`, `convert_xlsx.go`, `convert_html.go`, `convert_txt.go`, `convert_svg.go`) are a separate module from the find/replace engine above — they don't edit the PDF at all, they read it. PDF has no concept of a paragraph: a content stream only positions individual strings on the page via `Tf`/`Td`/`Tm`/`TL`/`T*` operators. The pipeline (`convert.go`):

1. Normalizes the upload with `qpdf`, same as editing.
2. Walks the page tree and, for each page, decodes its actual text (reusing the same simple-font/CID-`/ToUnicode` font resolution as the editor), while additionally tracking the text line matrix's Y position and the current leading — enough to know where each line sits vertically without doing full matrix math (real-world PDFs essentially never rotate or skew text).
3. Groups consecutive lines into paragraphs with the same heuristic tools like `pdftotext -layout` use: a same-line gap (an inline font-size change, e.g. a bold word mid-sentence) concatenates directly, an ordinary line-wrap gap (roughly one line height) joins with a space, and anything bigger (or a move back up the page) starts a new paragraph. Also resolves each page's `/MediaBox` (with inheritance), used only by the SVG output.
4. Hands the resulting per-page paragraph lists to the requested format's builder, one file per `format`:
   - **docx** — minimal OOXML WordprocessingML, one `<w:p>` per paragraph, an explicit page break between pages.
   - **pptx** — minimal OOXML PresentationML: one slide per page, all paragraphs in a single autofit text box (PowerPoint/LibreOffice/Keynote reflow and shrink the text themselves). Requires the usual slide-master/layout/theme boilerplate PowerPoint's schema needs even for a blank design.
   - **xlsx** — minimal OOXML SpreadsheetML: one worksheet per page ("Page 1", "Page 2", ...), one row per paragraph in column A, using inline strings (no shared-strings table needed).
   - **html** — paragraphs as `<p>` tags with a dashed, print-`page-break`-marked `<hr>` between pages.
   - **txt** — paragraphs separated by blank lines, pages separated by a form-feed (`\f`), the conventional plain-text page-break character.
   - **svg** — the one format with no native text reflow, so paragraphs are manually word-wrapped (`wrapText` in `convert.go`) to the page's actual width; all pages are stacked into a single tall canvas so a multi-page PDF still downloads as one file.

  All OOXML formats (docx/xlsx/pptx) are zips of small XML parts built with the standard library's `archive/zip` — no third-party document library.

**png/jpeg/tiff/webp are a completely different pipeline** (`convert_raster.go`), because they're the one place this app produces actual pixels instead of extracted text — and text extraction has no way to draw a page, only to read what's in it. Instead of the paragraph pipeline above, these formats:

1. Shell out to `pdftoppm` (from poppler, an external CLI dependency exactly like `qpdf` - see Prerequisites) to rasterize the page directly at 150 DPI, straight from the original upload — no `qpdf` normalize step needed here, since poppler's own parser already handles compressed/object-stream PDFs that this project's hand-rolled parser can't.
2. For `webp` specifically (which `pdftoppm` can't write directly): render to PNG first, then re-encode with `cwebp` (the reference libwebp encoder).
3. Return the result directly if the PDF had one page; zip all pages together (`page-1.<ext>`, `page-2.<ext>`, ...) if it had more than one, since (unlike text, which can just be concatenated) merging several page images into one file would need an image-decoding/compositing library this project doesn't have.

If `pdftoppm` (or, for `webp`, `cwebp`) isn't installed, requesting one of these four formats fails with a clear `503` error rather than crashing the server — every other feature keeps working normally.

## Limitations (honest ones)

- **CID/subset fonts are matched via `/ToUnicode`, not their embedded CMap**: text rendered through a CID/Type0 font is decoded using that font's `/ToUnicode` map, which is present in the vast majority of real-world PDFs (Word, Google Docs, LibreOffice, browser print-to-PDF) - but if a document omits it, that text won't match.
- **Font `/Differences` isn't parsed for matching**: the `lang` option assumes WinAnsiEncoding (`latin`) or Windows-1254 (`turkish`) for *decoding* a simple font's bytes. Replacement text that the matched font can't safely render (wrong glyph, or a byte with no `/Widths` entry in that specific subset font) is automatically routed through a bundled fallback font instead of guessing.
- **Layout is not reflowed**: replacing "Hello" with a much longer string keeps the original glyph positions, so long replacements can overlap neighboring text.
- **Text split across operators**: a phrase kerned into pieces like `(AC) -12 (ME)` won't match a search for `ACME`.
- Scanned/image-only PDFs contain no text at all — you'd need OCR first.
- **Regex mode and mouse-selection targeting don't combine**: drag-to-select occurrence targeting counts matches by plain substring search, which doesn't correspond to how a regex pattern matches, so the UI disables drag-to-select while "Use regex" is checked (regex mode always replaces every match in the document).
- **Undo is in-memory and per-tab**: it steps back through the last 5 versions held in the browser tab's memory, not a server-side history — closing/reloading the tab loses it, same as the original upload if not downloaded.
- **Change highlighting is a review aid, not a ground truth**: it's a browser-side overlay (nothing is written into the PDF itself) that marks any rendered text span containing a session edit's replacement text. There's no per-edit position tracking, so it can occasionally highlight an unrelated pre-existing occurrence of the same text, and it's skipped entirely for regex-mode edits (the replacement text differs per match, so there's no single fixed string to look for).
- **PDF conversion to a document format (docx/pptx/xlsx/html/txt/svg) is best-effort text extraction, not layout reconstruction**: paragraphs are inferred from vertical line spacing (see "Converting to other formats" above), so multi-column layouts, tables, and precise indentation are not reproduced — everything comes out as a linear sequence of paragraphs (or, for xlsx, rows), per page. Embedded images aren't carried over at all in these formats. It shares the same CID/`/ToUnicode` and font-encoding limitations as find/replace, described above.
- **Raster image export (PNG/JPEG/TIFF/WEBP) requires `pdftoppm`/`cwebp` to be installed** (see Prerequisites) — unlike every other feature in this app, it isn't optional-dependency-free, since actually rendering a page's pixels needs a real PDF renderer, not text extraction. It also doesn't apply the `lang` setting at all (there's no text decoding involved), and a multi-page PDF comes back as a `.zip` of one image per page rather than a single file (see "Converting to other formats" above).

## Project structure

```
pdf-editor/
├── go.mod                  # module pdfer (no dependencies)
├── main.go                 # HTTP server + PDF parse/rebuild/qpdf pipeline
├── middleware.go            # panic recovery, logging, security headers, rate/concurrency limiting
├── pdftext.go               # font-aware content-stream tokenizer, decoder, and find/replace engine
├── convert.go               # shared PDF -> paragraphs pipeline + format dispatch
├── convert_docx.go           # .docx (Word) writer
├── convert_pptx.go           # .pptx (PowerPoint) writer
├── convert_xlsx.go           # .xlsx (Excel) writer
├── convert_html.go           # .html writer
├── convert_txt.go            # .txt writer
├── convert_svg.go            # .svg writer
├── convert_raster.go         # .png/.jpeg/.tiff/.webp writer (shells out to pdftoppm/cwebp - see Prerequisites)
├── encoding.go              # rune <-> byte tables per language (latin, turkish, ...)
├── fallbackfont.go           # bundled fallback-font PDF object builder
├── fallbackfont_data.go      # generated widths/metrics for the fallback font
├── pdfer_test.go             # regression tests (programmatically-built fixture PDFs)
├── convert_test.go           # regression tests for the PDF converter (all output formats)
├── Dockerfile
├── docker-compose.yml         # pdfer + Caddy (reverse proxy, automatic HTTPS) for VPS deploys
├── Caddyfile
├── .env.example
├── assets/
│   └── pdfer-fallback.ttf   # subsetted DejaVu Sans, embedded for fallback rendering
└── static/
    ├── index.html           # browser UI markup/styles
    ├── app.js                # browser UI logic: PDF.js preview, drag-to-select, zoom, language picker
    └── vendor/                # self-hosted PDF.js (required for a strict CSP - no third-party CDN)
```
