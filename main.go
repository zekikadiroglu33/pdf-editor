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
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"syscall"
	"time"
)

//go:embed static
var staticFS embed.FS

const maxUpload = 50 << 20 // 50 MB

// maxFieldLen caps the find/replace text fields. maxUpload already bounds
// the whole request body, but this keeps a single request from spending
// its entire budget on megabytes of find/replace text instead of a PDF.
const maxFieldLen = 10000

// qpdfTimeout bounds each qpdf subprocess call. Without this, a malicious
// or pathologically malformed upload could hang a worker (and the
// concurrency slot it holds, see middleware.go) indefinitely.
const qpdfTimeout = 25 * time.Second

// editTimeout bounds the whole /api/edit pipeline (two qpdf calls plus our
// own in-process parsing/rebuilding), so one request can't tie up a
// concurrency slot indefinitely even if something downstream misbehaves.
const editTimeout = 55 * time.Second

// ---------------------------------------------------------------------------
// qpdf helpers
// ---------------------------------------------------------------------------

func runQpdf(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, qpdfTimeout)
	defer cancel()

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
	cmd := exec.CommandContext(ctx, "qpdf", full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("qpdf: timed out after %s", qpdfTimeout)
		}
		// exit code 3 == "succeeded with warnings" - acceptable
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 3 {
			return nil, fmt.Errorf("qpdf: %v: %s", err, stderr.String())
		}
	}
	return os.ReadFile(out.Name())
}

// normalize turns any PDF into a classic PDF with uncompressed,
// text-searchable content streams and no object streams.
func normalize(ctx context.Context, pdf []byte) ([]byte, error) {
	return runQpdf(ctx, pdf, "--object-streams=disable", "--stream-data=uncompress")
}

// recompress shrinks the rebuilt file back down.
func recompress(ctx context.Context, pdf []byte) []byte {
	out, err := runQpdf(ctx, pdf, "--object-streams=generate")
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

// badInputError marks errors caused by the caller's input (language,
// find/replace text) rather than by PDF parsing/rebuilding.
type badInputError struct{ msg string }

func (e *badInputError) Error() string { return e.msg }

// unavailableError marks a request that failed only because a required
// external tool isn't installed on this server (e.g. pdftoppm/cwebp for
// raster image conversion) - a deployment/environment problem, not
// something wrong with the caller's request or the uploaded PDF.
type unavailableError struct{ msg string }

func (e *unavailableError) Error() string { return e.msg }

// EditPDF is the full pipeline: normalize -> replace -> rebuild -> recompress.
//
// Replace is font-aware (see pdftext.go): it walks each page's actual
// fonts (including CID/Identity-H fonts via their /ToUnicode CMap) to
// reconstruct the real Unicode text before matching, so find/replace works
// regardless of which font rendered which character. Replacement text that
// the original font can't render is routed through a bundled fallback font.
//
// targetPage/targetOccurrence optionally restrict the edit to a single
// occurrence: targetPage <= 0 means "every page" (legacy replace-all
// behavior); targetPage > 0 with targetOccurrence < 0 means "every match on
// that page"; targetPage > 0 with targetOccurrence >= 0 means "only the
// Nth match (0-indexed, in reading order) on that page" - this is what the
// UI uses when the user selects a specific word with the mouse, so that
// selecting one occurrence doesn't rewrite every other occurrence of the
// same word throughout the document.
//
// useRegex treats find as a Go regexp (RE2, so it's immune to catastrophic
// backtracking regardless of what pattern is submitted) matched against the
// document's actual decoded Unicode text; repl is then expanded per match
// using regexp.Expand syntax ($1, ${name}, ...) instead of used verbatim.
func EditPDF(ctx context.Context, pdf []byte, find, repl, lang string, targetPage, targetOccurrence int, useRegex bool) ([]byte, int, error) {
	simpleTable, ok := encodingTables[lang]
	if !ok {
		return nil, 0, &badInputError{fmt.Sprintf("unsupported language %q", lang)}
	}
	fallbackTable := encodingTables["turkish"] // superset covering latin + Turkish; used for the bundled fallback font
	matcher, err := newTextMatcher(find, repl, useRegex)
	if err != nil {
		return nil, 0, &badInputError{err.Error()}
	}

	norm, err := normalize(ctx, pdf)
	if err != nil {
		return nil, 0, fmt.Errorf("normalize: %w", err)
	}
	objects, err := parseObjects(norm)
	if err != nil {
		return nil, 0, fmt.Errorf("parse: %w", err)
	}

	objByNum := make(map[int]int, len(objects))
	maxNum := 0
	for idx, o := range objects {
		objByNum[o.num] = idx
		if o.num > maxNum {
			maxNum = o.num
		}
	}

	pageOrder := resolvePageOrder(objects, objByNum)
	if len(pageOrder) == 0 {
		// Page tree couldn't be resolved (unusual/malformed PDF) - fall
		// back to physical object order. Page-number targeting becomes
		// best-effort in that case, but whole-document replace-all is
		// unaffected since it doesn't care about page order.
		for idx, o := range objects {
			if isPageObject(o.body) {
				pageOrder = append(pageOrder, idx)
			}
		}
	}

	fontCache := map[int]*resolvedFont{}
	type fbTarget struct{ ownerIdx, start, end int }
	var fbTargets []fbTarget
	seenOwner := map[int]bool{}
	needFallback := false
	totalCount := 0

	for pn, pageIdx := range pageOrder {
		pageNum := pn + 1
		if targetPage > 0 && pageNum != targetPage {
			continue
		}
		var occFilter *occurrenceFilter
		if targetPage > 0 && targetOccurrence >= 0 {
			occFilter = &occurrenceFilter{target: targetOccurrence}
		}

		var rootScope resScope
		if resOwner, rStart, rEnd, rOk := resolvePageResources(objects, objByNum, pageIdx); rOk {
			rootScope = resolveScope(objects, objByNum, fontCache, lang, resOwner, rStart, rEnd)
		}

		// Expand the page's own /Contents into every text-bearing stream
		// reachable from it, including any Form XObjects it invokes via
		// "Do" (and Forms those invoke, recursively) - some PDF generators
		// (e.g. iText) put all of a page's real content inside a Form
		// XObject rather than directly in /Contents.
		var units []formContentUnit
		visiting := map[int]bool{}
		for _, cIdx := range resolvePageContents(objects, objByNum, pageIdx) {
			walkContent(objects, objByNum, fontCache, lang, cIdx, rootScope, 0, visiting, &units)
		}

		for _, u := range units {
			dr := decodeContentStream(tokenizeContentStream(objects[u.streamIdx].stream), u.scope.fontMap)
			newStream, n, usesFallback := replaceInDecodedStream(objects[u.streamIdx].stream, dr, matcher, simpleTable, fallbackTable, occFilter)
			if n == 0 {
				continue
			}
			objects[u.streamIdx].stream = newStream
			objects[u.streamIdx].body = lengthRe.ReplaceAll(objects[u.streamIdx].body,
				[]byte(fmt.Sprintf("/Length %d", len(newStream))))
			totalCount += n
			if usesFallback {
				needFallback = true
				if u.scope.fontOk && !seenOwner[u.scope.fontOwnerIdx] {
					seenOwner[u.scope.fontOwnerIdx] = true
					fbTargets = append(fbTargets, fbTarget{u.scope.fontOwnerIdx, u.scope.fStart, u.scope.fEnd})
				}
			}
		}
	}

	if needFallback {
		newObjs, fontObjNum := buildFallbackFontObjects(maxNum + 1)
		objects = append(objects, newObjs...)
		for _, t := range fbTargets {
			insertFontEntry(&objects[t.ownerIdx], t.start, t.end, fallbackFontResourceName, fontObjNum)
		}
	}

	rebuilt, err := rebuild(objects, norm)
	if err != nil {
		return nil, 0, fmt.Errorf("rebuild: %w", err)
	}
	return recompress(ctx, rebuilt), totalCount, nil
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
	lang := r.FormValue("lang")
	if lang == "" {
		lang = "latin"
	}
	// page/occurrence are optional: when the UI's selection tracking
	// supplies them, only that one occurrence (Nth match, 0-indexed, on
	// that 1-indexed page) is edited instead of every match in the
	// document - see EditPDF.
	page := 0
	if v := r.FormValue("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			page = n
		}
	}
	occurrence := -1
	if v := r.FormValue("occurrence"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			occurrence = n
		}
	}
	useRegex := r.FormValue("regex") == "true"
	if len(find) > maxFieldLen || len(repl) > maxFieldLen {
		http.Error(w, "find/replace text too long", http.StatusBadRequest)
		return
	}
	file, _, err := r.FormFile("file")
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

	ctx, cancel := context.WithTimeout(r.Context(), editTimeout)
	defer cancel()
	edited, count, err := EditPDF(ctx, pdf, find, repl, lang, page, occurrence, useRegex)
	if err != nil {
		var bad *badInputError
		if errors.As(err, &bad) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		log.Printf("edit failed (%d byte upload): %v", len(pdf), err)
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	log.Printf("edited: %d replacement(s), %d -> %d bytes", count, len(pdf), len(edited))

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("X-Replacements", strconv.Itoa(count))
	w.Header().Set("Content-Disposition", `attachment; filename="edited.pdf"`)
	w.Write(edited)
}

func handleConvert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	if err := r.ParseMultipartForm(maxUpload); err != nil {
		http.Error(w, "upload too large or malformed: "+err.Error(), http.StatusBadRequest)
		return
	}
	lang := r.FormValue("lang")
	if lang == "" {
		lang = "latin"
	}
	format := r.FormValue("format")
	if format == "" {
		format = "docx"
	}
	file, _, err := r.FormFile("file")
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

	ctx, cancel := context.WithTimeout(r.Context(), editTimeout)
	defer cancel()
	data, pages, contentType, filename, err := ConvertPDF(ctx, pdf, lang, format)
	if err != nil {
		var bad *badInputError
		if errors.As(err, &bad) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var unavail *unavailableError
		if errors.As(err, &unavail) {
			log.Printf("convert unavailable: %v", err)
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		log.Printf("convert failed (%d byte upload): %v", len(pdf), err)
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	log.Printf("converted to %s: %d page(s), %d -> %d bytes", format, pages, len(pdf), len(data))

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Pages", strconv.Itoa(pages))
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.Write(data)
}

func main() {
	if _, err := exec.LookPath("qpdf"); err != nil {
		log.Fatal("qpdf not found in PATH - install it first (apt install qpdf / brew install qpdf)")
	}
	// pdftoppm/cwebp are only needed for the raster image output formats
	// (png/jpeg/tiff/webp) - unlike qpdf, their absence doesn't break the
	// rest of the app, so it's a warning, not a fatal error; requests for
	// those formats fail individually with a clear message (see
	// convert_raster.go) if the tool turns out to be missing.
	if _, err := exec.LookPath("pdftoppm"); err != nil {
		log.Print("warning: pdftoppm not found in PATH - PNG/JPEG/TIFF/WEBP conversion will be unavailable (install poppler-utils / poppler)")
	}
	if _, err := exec.LookPath("cwebp"); err != nil {
		log.Print("warning: cwebp not found in PATH - WEBP conversion will be unavailable (install webp)")
	}

	static, _ := fs.Sub(staticFS, "static")

	// /api/edit and /api/convert are the expensive, abuse-prone routes:
	// rate-limited per IP and concurrency-limited process-wide, since each
	// call spawns at least one qpdf subprocess. The static file server and
	// /api/health stay unrestricted. Each route gets its own limiter so a
	// burst of conversions can't starve edit requests or vice versa.
	editLimiter := newConcurrencyLimiter(8)
	rateLimiter := newRateLimiter(0.5, 15) // ~1 edit/2s sustained, burst 15, per IP
	editHandler := editLimiter.middleware(rateLimitMiddleware(rateLimiter, http.HandlerFunc(handleEdit)))

	convertLimiter := newConcurrencyLimiter(8)
	convertRateLimiter := newRateLimiter(0.5, 15)
	convertHandler := convertLimiter.middleware(rateLimitMiddleware(convertRateLimiter, http.HandlerFunc(handleConvert)))

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(static)))
	mux.Handle("/api/edit", editHandler)
	mux.Handle("/api/convert", convertHandler)
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})

	handler := securityHeadersMiddleware(loggingMiddleware(recoverMiddleware(mux)))

	addr := ":8080"
	if p := os.Getenv("PORT"); p != "" {
		addr = ":" + p
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      90 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		log.Printf("pdfer listening on http://localhost%s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	stop()
	log.Print("shutting down: waiting for in-flight requests to finish...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown did not complete cleanly: %v", err)
	}
}
