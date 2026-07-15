package conversion

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"

	"pdfer/pdfcore"
)

// HandleConvert is the /api/convert HTTP handler.
func HandleConvert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, pdfcore.MaxUpload)
	if err := r.ParseMultipartForm(pdfcore.MaxUpload); err != nil {
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

	ctx, cancel := context.WithTimeout(r.Context(), pdfcore.RequestTimeout)
	defer cancel()
	data, pages, contentType, filename, err := ConvertPDF(ctx, pdf, lang, format)
	if err != nil {
		var bad *pdfcore.BadInputError
		if errors.As(err, &bad) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var unavail *pdfcore.UnavailableError
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
