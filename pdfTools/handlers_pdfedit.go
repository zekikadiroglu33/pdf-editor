// handlers_pdfedit.go - HTTP handlers for the object-model-editing tools:
// watermark, crop, add-image, compress-images. Shares zipNamedFiles,
// readPDFFormFile, writeToolError, writePDFResponse with
// handlers_pdftools.go (same package).
package pdftools

import (
	"context"
	"io"
	"log"
	"net/http"
	"strconv"

	"pdfer/pdfcore"
)

func HandleWatermark(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, pdfcore.MaxUpload)
	if err := r.ParseMultipartForm(pdfcore.MaxUpload); err != nil {
		http.Error(w, "upload too large or malformed: "+err.Error(), http.StatusBadRequest)
		return
	}
	pdf, err := readPDFFormFile(r, "file")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	text := r.FormValue("text")
	opacity := 0.3
	if raw := r.FormValue("opacity"); raw != "" {
		if opacity, err = strconv.ParseFloat(raw, 64); err != nil {
			http.Error(w, `"opacity" must be a number between 0 and 1`, http.StatusBadRequest)
			return
		}
	}
	fontSize := 48.0
	if raw := r.FormValue("fontSize"); raw != "" {
		if fontSize, err = strconv.ParseFloat(raw, 64); err != nil {
			http.Error(w, `"fontSize" must be a number`, http.StatusBadRequest)
			return
		}
	}
	pages := r.FormValue("pages")

	ctx, cancel := context.WithTimeout(r.Context(), pdfcore.RequestTimeout)
	defer cancel()
	watermarked, err := WatermarkPDF(ctx, pdf, text, opacity, fontSize, pages)
	if err != nil {
		writeToolError(w, "watermark", len(pdf), err)
		return
	}
	log.Printf("watermarked: %d -> %d bytes", len(pdf), len(watermarked))
	writePDFResponse(w, "watermarked.pdf", watermarked)
}

func HandleCrop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, pdfcore.MaxUpload)
	if err := r.ParseMultipartForm(pdfcore.MaxUpload); err != nil {
		http.Error(w, "upload too large or malformed: "+err.Error(), http.StatusBadRequest)
		return
	}
	pdf, err := readPDFFormFile(r, "file")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	margin := func(field string) (float64, bool) {
		raw := r.FormValue(field)
		if raw == "" {
			return 0, true
		}
		v, err := strconv.ParseFloat(raw, 64)
		return v, err == nil
	}
	left, ok1 := margin("left")
	right, ok2 := margin("right")
	top, ok3 := margin("top")
	bottom, ok4 := margin("bottom")
	if !ok1 || !ok2 || !ok3 || !ok4 {
		http.Error(w, `crop margins must be numbers (percent of page size)`, http.StatusBadRequest)
		return
	}
	pages := r.FormValue("pages")

	ctx, cancel := context.WithTimeout(r.Context(), pdfcore.RequestTimeout)
	defer cancel()
	cropped, err := CropPDF(ctx, pdf, left, right, top, bottom, pages)
	if err != nil {
		writeToolError(w, "crop", len(pdf), err)
		return
	}
	log.Printf("cropped: %d -> %d bytes", len(pdf), len(cropped))
	writePDFResponse(w, "cropped.pdf", cropped)
}

func HandleAddImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, pdfcore.MaxUpload)
	if err := r.ParseMultipartForm(pdfcore.MaxUpload); err != nil {
		http.Error(w, "upload too large or malformed: "+err.Error(), http.StatusBadRequest)
		return
	}
	pdf, err := readPDFFormFile(r, "file")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	imgFile, _, err := r.FormFile("image")
	if err != nil {
		http.Error(w, `missing "image" file field`, http.StatusBadRequest)
		return
	}
	defer imgFile.Close()
	imageData, err := io.ReadAll(imgFile)
	if err != nil {
		http.Error(w, "could not read uploaded image", http.StatusBadRequest)
		return
	}
	widthPct := 40.0
	if raw := r.FormValue("width"); raw != "" {
		if widthPct, err = strconv.ParseFloat(raw, 64); err != nil {
			http.Error(w, `"width" must be a number (percent of page width)`, http.StatusBadRequest)
			return
		}
	}
	position := r.FormValue("position")
	pages := r.FormValue("pages")

	ctx, cancel := context.WithTimeout(r.Context(), pdfcore.RequestTimeout)
	defer cancel()
	result, err := AddImageToPDF(ctx, pdf, imageData, pages, widthPct, position)
	if err != nil {
		writeToolError(w, "add-image", len(pdf), err)
		return
	}
	log.Printf("added image: %d + %d -> %d bytes", len(pdf), len(imageData), len(result))
	writePDFResponse(w, "with-image.pdf", result)
}

func HandleCompressImages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, pdfcore.MaxUpload)
	if err := r.ParseMultipartForm(pdfcore.MaxUpload); err != nil {
		http.Error(w, "upload too large or malformed: "+err.Error(), http.StatusBadRequest)
		return
	}
	pdf, err := readPDFFormFile(r, "file")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	maxDimension := 1600
	if raw := r.FormValue("maxDimension"); raw != "" {
		maxDimension, err = strconv.Atoi(raw)
		if err != nil {
			http.Error(w, `"maxDimension" must be an integer`, http.StatusBadRequest)
			return
		}
	}
	quality := 75
	if raw := r.FormValue("quality"); raw != "" {
		quality, err = strconv.Atoi(raw)
		if err != nil {
			http.Error(w, `"quality" must be an integer between 1 and 100`, http.StatusBadRequest)
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), pdfcore.RequestTimeout)
	defer cancel()
	result, changed, err := CompressImages(ctx, pdf, maxDimension, quality)
	if err != nil {
		writeToolError(w, "compress-images", len(pdf), err)
		return
	}
	log.Printf("compressed images (%d changed): %d -> %d bytes", changed, len(pdf), len(result))
	w.Header().Set("X-Images-Changed", strconv.Itoa(changed))
	w.Header().Set("X-Original-Size", strconv.Itoa(len(pdf)))
	w.Header().Set("X-Compressed-Size", strconv.Itoa(len(result)))
	writePDFResponse(w, "images-compressed.pdf", result)
}
