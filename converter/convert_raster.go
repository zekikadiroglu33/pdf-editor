// convert_raster.go - PDF -> raster image (png/jpeg/tiff/webp) conversion.
//
// Every other format in this package is built from *extracted text*
// (extractPDFPages in convert.go): none of them can produce an image,
// because text extraction never sees the page's actual drawn pixels.
// Rasterizing a PDF page for real requires an actual rendering engine, so
// this file shells out to poppler's `pdftoppm` (same external-CLI
// pattern as qpdf elsewhere in this project) to render each page
// directly to PNG/JPEG/TIFF; WEBP has no native writer in poppler, so it
// is produced by rendering to PNG first and then converting with `cwebp`
// (the reference libwebp encoder). Neither tool is required for any other
// conversion format or for editing, so their absence only disables these
// four formats (see the pdftoppm/cwebp checks in main's startup and
// pdfcore.UnavailableError below), not the whole server.
package converter

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"pdfer/pdfcore"
)

// rasterTimeout bounds each pdftoppm/cwebp subprocess call, same
// reasoning as pdfcore.QpdfTimeout: without it, a pathological upload
// could hang a worker (and its concurrency slot) indefinitely.
const rasterTimeout = 40 * time.Second

const rasterDPI = "150"

// rasterExt is the file extension pdftoppm/cwebp actually write for each
// supported raster format (jpeg/tiff's conventional extensions differ
// from the format name used in the API/UI).
var rasterExt = map[string]string{
	"png":  "png",
	"jpeg": "jpg",
	"tiff": "tif",
	"webp": "webp",
}

var rasterContentType = map[string]string{
	"png":  "image/png",
	"jpeg": "image/jpeg",
	"tiff": "image/tiff",
	"webp": "image/webp",
}

// runRasterTool runs an external image-conversion command with a bounded
// timeout, translating "not installed" into a pdfcore.UnavailableError
// (a deployment problem) rather than a generic failure.
func runRasterTool(ctx context.Context, name string, args ...string) error {
	if _, err := exec.LookPath(name); err != nil {
		return &pdfcore.UnavailableError{Msg: fmt.Sprintf("%s is not installed on this server - required for this output format", name)}
	}
	ctx, cancel := context.WithTimeout(ctx, rasterTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("%s: timed out after %s", name, rasterTimeout)
		}
		return fmt.Errorf("%s: %v: %s", name, err, stderr.String())
	}
	return nil
}

// pngToWebP converts one PNG image to WEBP via cwebp (poppler has no
// native WEBP writer, so PNG is always the intermediate format).
func pngToWebP(ctx context.Context, png []byte) ([]byte, error) {
	in, err := os.CreateTemp("", "pdfer-webp-in-*.png")
	if err != nil {
		return nil, err
	}
	defer os.Remove(in.Name())
	if _, err := in.Write(png); err != nil {
		return nil, err
	}
	in.Close()

	out, err := os.CreateTemp("", "pdfer-webp-out-*.webp")
	if err != nil {
		return nil, err
	}
	out.Close()
	defer os.Remove(out.Name())

	if err := runRasterTool(ctx, "cwebp", "-quiet", in.Name(), "-o", out.Name()); err != nil {
		return nil, err
	}
	return os.ReadFile(out.Name())
}

// RasterizePDF renders every page of pdf to the requested image format
// via pdftoppm (rendering to PNG first, then to WEBP via cwebp, for the
// one format pdftoppm can't write directly), returning one image per
// page in page order. It rasterizes the caller's original upload
// directly - unlike the text-extraction formats, this never needs
// qpdf's normalize step, since poppler (unlike this project's own
// hand-rolled object parser) already handles compressed/object-stream
// PDFs natively. Exported since pdfTools' image tests call it directly
// to verify AddImageToPDF's output actually renders correctly.
func RasterizePDF(ctx context.Context, pdf []byte, format string) ([][]byte, error) {
	in, err := os.CreateTemp("", "pdfer-raster-in-*.pdf")
	if err != nil {
		return nil, err
	}
	defer os.Remove(in.Name())
	if _, err := in.Write(pdf); err != nil {
		return nil, err
	}
	in.Close()

	outDir, err := os.MkdirTemp("", "pdfer-raster-out-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(outDir)
	prefix := filepath.Join(outDir, "page")

	// pdftoppm can write png/jpeg/tiff directly; webp is rendered as png
	// first and converted per-page below.
	renderFormat := format
	if format == "webp" {
		renderFormat = "png"
	}
	renderFlag := "-" + renderFormat
	if renderFormat == "tiff" {
		renderFlag = "-tiff"
	}

	if err := runRasterTool(ctx, "pdftoppm", renderFlag, "-r", rasterDPI, in.Name(), prefix); err != nil {
		return nil, err
	}

	matches, err := filepath.Glob(prefix + "*." + rasterExt[renderFormat])
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("pdftoppm produced no pages")
	}
	// pdftoppm zero-pads the page-number suffix to a fixed width based on
	// the page count of this run, so a lexical sort is already numeric
	// order.
	sort.Strings(matches)

	images := make([][]byte, len(matches))
	for i, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			return nil, err
		}
		if format == "webp" {
			b, err = pngToWebP(ctx, b)
			if err != nil {
				return nil, err
			}
		}
		images[i] = b
	}
	return images, nil
}

// zipRasterPages bundles more than one page's image into a single
// downloadable zip (page-1.<ext>, page-2.<ext>, ...) - every other
// format in this app returns exactly one file per conversion, and a
// multi-page raster conversion has no single-image format to merge into
// (unlike convert_svg.go's text, pixels can't just be stacked without
// decoding/re-encoding every page, which would need an image codec
// dependency this project deliberately avoids).
func zipRasterPages(images [][]byte, format string) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	ext := rasterExt[format]
	for i, img := range images {
		w, err := zw.Create(fmt.Sprintf("page-%d.%s", i+1, ext))
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(img); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// convertPDFToRaster is the raster-format entry point dispatched from
// ConvertPDF: rasterize every page, then return either the lone image
// directly (single-page PDF) or a zip of all pages (multi-page), along
// with the content type/filename appropriate to whichever it produced.
func convertPDFToRaster(ctx context.Context, pdf []byte, format string) (data []byte, pageCount int, contentType, filename string, err error) {
	images, err := RasterizePDF(ctx, pdf, format)
	if err != nil {
		return nil, 0, "", "", err
	}
	if len(images) == 1 {
		return images[0], 1, rasterContentType[format], "converted." + rasterExt[format], nil
	}
	zipped, err := zipRasterPages(images, format)
	if err != nil {
		return nil, 0, "", "", fmt.Errorf("zip raster pages: %w", err)
	}
	return zipped, len(images), "application/zip", fmt.Sprintf("converted_%s.zip", format), nil
}
