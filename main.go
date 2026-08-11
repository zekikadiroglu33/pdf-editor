// pdfer - a tiny PDF editing/conversion server written in Go (stdlib +
// the qpdf CLI only). See the editor, conversion, and pdfTools packages
// for the actual PDF pipelines; this file only wires up the HTTP server:
// static file serving, route registration, rate/concurrency limiting
// (middleware.go), and graceful shutdown.
package main

import (
	"bytes"
	"context"
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"pdfer/converter"
	"pdfer/editor"
	pdftools "pdfer/pdfTools"
)

//go:embed static
var staticFS embed.FS

// pageRoutes maps each clean, extensionless URL this site serves to the
// source file under static/ that renders it - a page's URL is a routing
// decision, not a filename constraint. Every other embedded asset
// (css/js/images/...) is served as-is via http.FileServer.
var pageRoutes = map[string]string{
	"/":             "hub.html",
	"/edit-pdf":     "edit-pdf.html",
	"/merge-pdf":    "merge-pdf.html",
	"/split-pdf":    "split-pdf.html",
	"/compress-pdf": "compress-pdf.html",
	"/rotate-pdf":   "rotate-pdf.html",
	"/pdf-to-word":  "pdf-to-word.html",
	"/convert.html": "convert.html",
	"/tools.html":   "tools.html",
}

// staticAssetTypes forces an explicit Content-Type for the handful of
// assets crawlers actually validate by header/content, rather than
// leaving it to http.FileServer's mime.TypeByExtension guess - which
// depends on the OS's /etc/mime.types (absent in the slim Docker runtime
// image, so this already fell back correctly to Go's builtin ".xml"
// default) or, failing that, sniffing the file's first 512 bytes. Both
// paths happen to land on a non-HTML type today, but a search engine
// rejecting a sitemap because it "appears to be an HTML page" is exactly
// the failure mode not worth leaving to an implicit fallback chain.
var staticAssetTypes = map[string]string{
	"/sitemap.xml": "application/xml; charset=utf-8",
	"/robots.txt":  "text/plain; charset=utf-8",
}

// staticHandler serves the embedded static/ tree, resolving each of
// pageRoutes and staticAssetTypes to its file once at startup - not per
// request, since the embedded file set never changes at runtime. Google
// Analytics is entirely a static-asset concern (see analytics.js): every
// page loads it unconditionally via a plain <script> tag, so nothing
// server-side needs to know about it.
// defaultLangMarker is the data-default-lang="latin" attribute the editor
// and converter pages (edit-pdf.html, convert.html, pdf-to-word.html) carry
// on <body> - app.js/convert.js read it to decide the language dropdown's
// initial value. Pages without the marker (rotate/merge/split/... have no
// language dropdown at all) are unaffected by the replace-once below.
var defaultLangMarker = []byte(`data-default-lang="latin"`)

func staticHandler(fsys fs.FS) http.Handler {
	rendered := make(map[string][]byte, len(pageRoutes))
	renderedTurkish := make(map[string][]byte, len(pageRoutes))
	for urlPath, file := range pageRoutes {
		data, err := fs.ReadFile(fsys, file)
		if err != nil {
			log.Fatalf("static/%s: %v", file, err)
		}
		rendered[urlPath] = data
		renderedTurkish[urlPath] = bytes.Replace(data, defaultLangMarker, []byte(`data-default-lang="turkish"`), 1)
	}

	assets := make(map[string][]byte, len(staticAssetTypes))
	for urlPath := range staticAssetTypes {
		data, err := fs.ReadFile(fsys, strings.TrimPrefix(urlPath, "/"))
		if err != nil {
			log.Fatalf("static%s: %v", urlPath, err)
		}
		assets[urlPath] = data
	}

	fileServer := http.FileServer(http.FS(fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if data, ok := rendered[r.URL.Path]; ok {
			if prefersTurkish(r) {
				data = renderedTurkish[r.URL.Path]
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Add("Vary", "Accept-Language")
			w.Write(data)
			return
		}
		if contentType, ok := staticAssetTypes[r.URL.Path]; ok {
			w.Header().Set("Content-Type", contentType)
			w.Write(assets[r.URL.Path])
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

func main() {
	if _, err := exec.LookPath("qpdf"); err != nil {
		log.Fatal("qpdf not found in PATH - install it first (apt install qpdf / brew install qpdf)")
	}
	// pdftoppm/cwebp are only needed for the raster image output formats
	// (png/jpeg/tiff/webp) - unlike qpdf, their absence doesn't break the
	// rest of the app, so it's a warning, not a fatal error; requests for
	// those formats fail individually with a clear message (see
	// conversion/convert_raster.go) if the tool turns out to be missing.
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
	editRateLimiter := newRateLimiter(0.5, 15) // ~1 edit/2s sustained, burst 15, per IP
	editHandler := editLimiter.middleware(rateLimitMiddleware(editRateLimiter, http.HandlerFunc(editor.HandleEdit)))

	convertLimiter := newConcurrencyLimiter(8)
	convertRateLimiter := newRateLimiter(0.5, 15)
	convertHandler := convertLimiter.middleware(rateLimitMiddleware(convertRateLimiter, http.HandlerFunc(converter.HandleConvert)))

	// The five qpdf-backed page/document tools (rotate, merge, split,
	// delete-pages, compress - see pdfTools/pdftools.go) share one limiter
	// pair: each is a single qpdf subprocess call, a similar cost profile
	// to each other and lighter than /api/edit's two-pass pipeline, so
	// they don't need a limiter per route the way edit/convert do - just
	// their own budget, isolated from those two busier routes.
	toolsLimiter := newConcurrencyLimiter(8)
	toolsRateLimiter := newRateLimiter(0.5, 15)
	toolsMiddleware := func(h http.HandlerFunc) http.Handler {
		return toolsLimiter.middleware(rateLimitMiddleware(toolsRateLimiter, h))
	}

	// Watermark/crop (direct PDF object-model edits) and add-image/
	// compress-images (image codec work - the heaviest CPU cost in this
	// app alongside qpdf itself) get their own limiter pair, isolated
	// from the qpdf-native tools above, since a burst of image
	// compression requests could otherwise starve simple rotate/merge
	// calls or vice versa.
	editToolsLimiter := newConcurrencyLimiter(8)
	editToolsRateLimiter := newRateLimiter(0.5, 15)
	editToolsMiddleware := func(h http.HandlerFunc) http.Handler {
		return editToolsLimiter.middleware(rateLimitMiddleware(editToolsRateLimiter, h))
	}

	mux := http.NewServeMux()
	mux.Handle("/", staticHandler(static))
	mux.Handle("/api/edit", editHandler)
	mux.Handle("/api/convert", convertHandler)
	mux.Handle("/api/rotate", toolsMiddleware(pdftools.HandleRotate))
	mux.Handle("/api/merge", toolsMiddleware(pdftools.HandleMerge))
	mux.Handle("/api/split", toolsMiddleware(pdftools.HandleSplit))
	mux.Handle("/api/delete-pages", toolsMiddleware(pdftools.HandleDeletePages))
	mux.Handle("/api/compress", toolsMiddleware(pdftools.HandleCompress))
	mux.Handle("/api/watermark", editToolsMiddleware(pdftools.HandleWatermark))
	mux.Handle("/api/crop", editToolsMiddleware(pdftools.HandleCrop))
	mux.Handle("/api/add-image", editToolsMiddleware(pdftools.HandleAddImage))
	mux.Handle("/api/compress-images", editToolsMiddleware(pdftools.HandleCompressImages))
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
