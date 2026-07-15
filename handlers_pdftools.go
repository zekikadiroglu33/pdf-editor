// handlers_pdftools.go - HTTP handlers for the qpdf-backed page/document
// tools in pdftools.go: rotate, merge, split, delete pages, compress.
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// zipNamedFiles bundles multiple files into one zip archive, preserving
// the given order - used by handleSplit when a request produces more
// than one output file (the same "zip if multi, direct file if single"
// pattern convert_raster.go already uses for multi-page image export).
func zipNamedFiles(names []string, contents [][]byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i, name := range names {
		w, err := zw.Create(name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(contents[i]); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// readPDFFormFile reads and sanity-checks one uploaded PDF from a
// multipart form field, mirroring the same "missing file"/"not a valid
// PDF" checks handleEdit and handleConvert already do.
func readPDFFormFile(r *http.Request, field string) ([]byte, error) {
	file, _, err := r.FormFile(field)
	if err != nil {
		return nil, fmt.Errorf("missing %q file field", field)
	}
	defer file.Close()
	pdf, err := io.ReadAll(file)
	if err != nil || !bytes.HasPrefix(pdf, []byte("%PDF")) {
		return nil, fmt.Errorf("%q is not a valid PDF file", field)
	}
	return pdf, nil
}

// writeToolError maps a pdftools.go error to the right HTTP status: a
// badInputError (bad page range, too few files, bad angle/quality, ...)
// is the caller's fault (400); anything else is treated as qpdf/the PDF
// itself failing unexpectedly (422), same convention as handleEdit and
// handleConvert.
func writeToolError(w http.ResponseWriter, op string, uploadSize int, err error) {
	var bad *badInputError
	if errors.As(err, &bad) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	log.Printf("%s failed (%d byte upload): %v", op, uploadSize, err)
	http.Error(w, err.Error(), http.StatusUnprocessableEntity)
}

func writePDFResponse(w http.ResponseWriter, filename string, data []byte) {
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.Write(data)
}

func handleRotate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	if err := r.ParseMultipartForm(maxUpload); err != nil {
		http.Error(w, "upload too large or malformed: "+err.Error(), http.StatusBadRequest)
		return
	}
	pdf, err := readPDFFormFile(r, "file")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	angle, err := strconv.Atoi(r.FormValue("angle"))
	if err != nil {
		http.Error(w, `"angle" must be an integer multiple of 90 (e.g. 90, -90, 180)`, http.StatusBadRequest)
		return
	}
	pages := r.FormValue("pages")

	ctx, cancel := context.WithTimeout(r.Context(), editTimeout)
	defer cancel()
	rotated, err := RotatePDF(ctx, pdf, pages, angle)
	if err != nil {
		writeToolError(w, "rotate", len(pdf), err)
		return
	}
	log.Printf("rotated: %d -> %d bytes", len(pdf), len(rotated))
	writePDFResponse(w, "rotated.pdf", rotated)
}

func handleMerge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	if err := r.ParseMultipartForm(maxUpload); err != nil {
		http.Error(w, "upload too large or malformed: "+err.Error(), http.StatusBadRequest)
		return
	}
	files := r.MultipartForm.File["files"]
	if len(files) < 2 {
		http.Error(w, `provide at least 2 files in the "files" field`, http.StatusBadRequest)
		return
	}

	var totalSize int
	pdfs := make([][]byte, len(files))
	for i, fh := range files {
		f, err := fh.Open()
		if err != nil {
			http.Error(w, "could not read an uploaded file", http.StatusBadRequest)
			return
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil || !bytes.HasPrefix(data, []byte("%PDF")) {
			http.Error(w, fmt.Sprintf("%q is not a valid PDF file", fh.Filename), http.StatusBadRequest)
			return
		}
		pdfs[i] = data
		totalSize += len(data)
	}

	ctx, cancel := context.WithTimeout(r.Context(), editTimeout)
	defer cancel()
	merged, err := MergePDFs(ctx, pdfs)
	if err != nil {
		writeToolError(w, "merge", totalSize, err)
		return
	}
	log.Printf("merged %d file(s): %d -> %d bytes", len(pdfs), totalSize, len(merged))
	writePDFResponse(w, "merged.pdf", merged)
}

func handleSplit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	if err := r.ParseMultipartForm(maxUpload); err != nil {
		http.Error(w, "upload too large or malformed: "+err.Error(), http.StatusBadRequest)
		return
	}
	pdf, err := readPDFFormFile(r, "file")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var ranges []string
	if raw := strings.TrimSpace(r.FormValue("ranges")); raw != "" {
		for _, part := range strings.Split(raw, ";") {
			if part = strings.TrimSpace(part); part != "" {
				ranges = append(ranges, part)
			}
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), editTimeout)
	defer cancel()
	parts, err := SplitPDF(ctx, pdf, ranges)
	if err != nil {
		writeToolError(w, "split", len(pdf), err)
		return
	}

	if len(parts) == 1 {
		log.Printf("split into 1 file: %d -> %d bytes", len(pdf), len(parts[0]))
		writePDFResponse(w, "split.pdf", parts[0])
		return
	}

	names := make([]string, len(parts))
	for i := range parts {
		names[i] = fmt.Sprintf("part-%d.pdf", i+1)
	}
	zipped, err := zipNamedFiles(names, parts)
	if err != nil {
		writeToolError(w, "split", len(pdf), err)
		return
	}
	log.Printf("split into %d files: %d -> %d bytes", len(parts), len(pdf), len(zipped))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("X-Parts", strconv.Itoa(len(parts)))
	w.Header().Set("Content-Disposition", `attachment; filename="split.zip"`)
	w.Write(zipped)
}

func handleDeletePages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	if err := r.ParseMultipartForm(maxUpload); err != nil {
		http.Error(w, "upload too large or malformed: "+err.Error(), http.StatusBadRequest)
		return
	}
	pdf, err := readPDFFormFile(r, "file")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	pages := r.FormValue("pages")

	ctx, cancel := context.WithTimeout(r.Context(), editTimeout)
	defer cancel()
	result, err := DeletePages(ctx, pdf, pages)
	if err != nil {
		writeToolError(w, "delete-pages", len(pdf), err)
		return
	}
	log.Printf("deleted pages %q: %d -> %d bytes", pages, len(pdf), len(result))
	writePDFResponse(w, "deleted.pdf", result)
}

func handleCompress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	if err := r.ParseMultipartForm(maxUpload); err != nil {
		http.Error(w, "upload too large or malformed: "+err.Error(), http.StatusBadRequest)
		return
	}
	pdf, err := readPDFFormFile(r, "file")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	quality := 0
	if raw := r.FormValue("quality"); raw != "" {
		quality, err = strconv.Atoi(raw)
		if err != nil {
			http.Error(w, `"quality" must be an integer between 1 and 100`, http.StatusBadRequest)
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), editTimeout)
	defer cancel()
	compressed, err := CompressPDF(ctx, pdf, quality)
	if err != nil {
		writeToolError(w, "compress", len(pdf), err)
		return
	}
	log.Printf("compressed: %d -> %d bytes", len(pdf), len(compressed))
	w.Header().Set("X-Original-Size", strconv.Itoa(len(pdf)))
	w.Header().Set("X-Compressed-Size", strconv.Itoa(len(compressed)))
	writePDFResponse(w, "compressed.pdf", compressed)
}
