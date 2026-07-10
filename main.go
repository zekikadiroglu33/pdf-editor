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
	"errors"
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

// EditPDF is the full pipeline: normalize -> replace -> rebuild -> recompress.
//
// Replace is font-aware (see pdftext.go): it walks each page's actual
// fonts (including CID/Identity-H fonts via their /ToUnicode CMap) to
// reconstruct the real Unicode text before matching, so find/replace works
// regardless of which font rendered which character. Replacement text that
// the original font can't render is routed through a bundled fallback font.
func EditPDF(pdf []byte, find, repl, lang string) ([]byte, int, error) {
	simpleTable, ok := encodingTables[lang]
	if !ok {
		return nil, 0, &badInputError{fmt.Sprintf("unsupported language %q", lang)}
	}
	fallbackTable := encodingTables["turkish"] // superset covering latin + Turkish; used for the bundled fallback font
	findRunes := []rune(find)
	replRunes := []rune(repl)

	norm, err := normalize(pdf)
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

	fontCache := map[int]*resolvedFont{}
	type fbTarget struct{ ownerIdx, start, end int }
	var fbTargets []fbTarget
	seenOwner := map[int]bool{}
	needFallback := false
	totalCount := 0

	for pageIdx := range objects {
		if !isPageObject(objects[pageIdx].body) {
			continue
		}
		var fontMap map[string]*resolvedFont
		ownerIdx, fStart, fEnd, ok := resolvePageFontDict(objects, objByNum, pageIdx)
		if ok {
			fontMap = buildFontMap(objects[ownerIdx].body[fStart:fEnd], objects, objByNum, fontCache, lang)
		}
		for _, cIdx := range resolvePageContents(objects, objByNum, pageIdx) {
			dr := decodeContentStream(tokenizeContentStream(objects[cIdx].stream), fontMap)
			newStream, n, usesFallback := replaceInDecodedStream(objects[cIdx].stream, dr, findRunes, replRunes, simpleTable, fallbackTable)
			if n == 0 {
				continue
			}
			objects[cIdx].stream = newStream
			objects[cIdx].body = lengthRe.ReplaceAll(objects[cIdx].body,
				[]byte(fmt.Sprintf("/Length %d", len(newStream))))
			totalCount += n
			if usesFallback {
				needFallback = true
				if ok && !seenOwner[ownerIdx] {
					seenOwner[ownerIdx] = true
					fbTargets = append(fbTargets, fbTarget{ownerIdx, fStart, fEnd})
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
	return recompress(rebuilt), totalCount, nil
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

	edited, count, err := EditPDF(pdf, find, repl, lang)
	if err != nil {
		var bad *badInputError
		if errors.As(err, &bad) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
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
