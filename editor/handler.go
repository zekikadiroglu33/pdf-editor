package editor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"

	"pdfer/pdfcore"
)

// HandleEdit is the /api/edit HTTP handler.
func HandleEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, pdfcore.MaxUpload)
	if err := r.ParseMultipartForm(pdfcore.MaxUpload); err != nil {
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
	if len(find) > pdfcore.MaxFieldLen || len(repl) > pdfcore.MaxFieldLen {
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

	ctx, cancel := context.WithTimeout(r.Context(), pdfcore.RequestTimeout)
	defer cancel()
	edited, count, err := EditPDF(ctx, pdf, find, repl, lang, page, occurrence, useRegex)
	if err != nil {
		var bad *pdfcore.BadInputError
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
