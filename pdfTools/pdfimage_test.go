// Regression tests for the image tools (pdfimage.go): add image to PDF,
// and compress (downsample) embedded images.
package pdftools

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"regexp"
	"testing"

	"pdfer/conversion"
	"pdfer/pdfcore"
	"pdfer/pdftestutil"
)

// solidJPEG returns a w x h all-red JPEG, encoded via Go's stdlib.
func solidJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 220, G: 30, B: 30, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	return buf.Bytes()
}

// solidPNG returns a w x h all-blue, half-transparent PNG.
func solidPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.NRGBA{R: 20, G: 30, B: 220, A: 128})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

func TestAddImageToPDFEmbedsJPEG(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdftestutil.RequirePdftoppm(t)
	pdf := pdftestutil.SimplePagePDF("BT /F1 12 Tf 72 700 Td (Hello) Tj ET\n")
	img := solidJPEG(t, 200, 100)

	out, err := AddImageToPDF(context.Background(), pdf, img, "1", 40, "center")
	if err != nil {
		t.Fatalf("AddImageToPDF: %v", err)
	}
	norm, err := pdfcore.Normalize(context.Background(), out)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !bytes.Contains(norm, []byte("/DCTDecode")) {
		t.Errorf("expected the JPEG to be embedded via /DCTDecode passthrough, got:\n%s", norm)
	}
	// Resource name is derived from the image's own object number (see
	// pdfimage.go), not a fixed name, so match the pattern rather than a
	// literal "/PDFerImg1".
	if !regexp.MustCompile(`/PDFerImg\d+ Do`).Match(norm) {
		t.Errorf("expected a Do operator invoking the image XObject, got:\n%s", norm)
	}

	// Rasterize and sample a center pixel to confirm the image actually
	// renders (not just that the right operators are present).
	images, err := conversion.RasterizePDF(context.Background(), out, "png")
	if err != nil {
		t.Fatalf("RasterizePDF: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("want 1 rendered page, got %d", len(images))
	}
	page, err := png.Decode(bytes.NewReader(images[0]))
	if err != nil {
		t.Fatalf("decode rendered page: %v", err)
	}
	b := page.Bounds()
	r, g, bl, _ := page.At(b.Min.X+b.Dx()/2, b.Min.Y+b.Dy()/2).RGBA()
	// Expect something close to the red (220,30,30) we embedded, not white.
	if r>>8 < 150 || g>>8 > 100 || bl>>8 > 100 {
		t.Errorf("expected a reddish pixel at page center, got RGB(%d,%d,%d)", r>>8, g>>8, bl>>8)
	}
}

func TestAddImageToPDFEmbedsPNGWithAlpha(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdf := pdftestutil.SimplePagePDF("BT /F1 12 Tf 72 700 Td (Hello) Tj ET\n")
	img := solidPNG(t, 64, 64)

	out, err := AddImageToPDF(context.Background(), pdf, img, "", 30, "top-left")
	if err != nil {
		t.Fatalf("AddImageToPDF: %v", err)
	}
	// Checked directly on AddImageToPDF's own output, not a re-normalized
	// copy: qpdf's --stream-data=uncompress (part of normalize) decompresses
	// *all* streams it can reverse, including image streams, and drops
	// their now-inapplicable /Filter key - so re-normalizing for inspection
	// would strip the very evidence this test is checking for.
	if !bytes.Contains(out, []byte("/SMask")) {
		t.Errorf("expected a /SMask for the PNG's alpha channel")
	}
	if !bytes.Contains(out, []byte("/FlateDecode")) {
		t.Errorf("expected the PNG to be embedded as raw /FlateDecode")
	}
}

func TestAddImageToPDFRejectsBadInput(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdf := pdftestutil.SimplePagePDF("BT /F1 12 Tf 72 700 Td (Hello) Tj ET\n")
	img := solidJPEG(t, 50, 50)

	cases := []struct {
		name     string
		data     []byte
		widthPct float64
		pos      string
	}{
		{"not an image", []byte("not an image"), 40, "center"},
		{"zero width", img, 0, "center"},
		{"width over 100", img, 101, "center"},
		{"unknown position", img, 40, "diagonal"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := AddImageToPDF(context.Background(), pdf, c.data, "1", c.widthPct, c.pos)
			var bad *pdfcore.BadInputError
			if !errors.As(err, &bad) {
				t.Fatalf("expected a BadInputError, got %T: %v", err, err)
			}
		})
	}
}

func TestCompressImagesDownsamplesOversizedJPEG(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdf := pdftestutil.SimplePagePDF("BT /F1 12 Tf 72 700 Td (Hello) Tj ET\n")
	bigImg := solidJPEG(t, 2000, 1000)

	withImage, err := AddImageToPDF(context.Background(), pdf, bigImg, "1", 90, "center")
	if err != nil {
		t.Fatalf("AddImageToPDF: %v", err)
	}

	out, changed, err := CompressImages(context.Background(), withImage, 500, 70)
	if err != nil {
		t.Fatalf("CompressImages: %v", err)
	}
	if changed != 1 {
		t.Fatalf("want 1 image changed, got %d", changed)
	}

	norm, err := pdfcore.Normalize(context.Background(), out)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !bytes.Contains(norm, []byte("/Width 500")) {
		t.Errorf("expected the oversized image downsampled to width 500, got:\n%s", norm)
	}
	if len(out) >= len(withImage) {
		t.Errorf("expected the compressed PDF to be smaller: %d -> %d bytes", len(withImage), len(out))
	}
}

func TestCompressImagesSkipsImagesAlreadyWithinBudget(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdf := pdftestutil.SimplePagePDF("BT /F1 12 Tf 72 700 Td (Hello) Tj ET\n")
	smallImg := solidJPEG(t, 100, 100)

	withImage, err := AddImageToPDF(context.Background(), pdf, smallImg, "1", 40, "center")
	if err != nil {
		t.Fatalf("AddImageToPDF: %v", err)
	}

	_, changed, err := CompressImages(context.Background(), withImage, 500, 70)
	if err != nil {
		t.Fatalf("CompressImages: %v", err)
	}
	if changed != 0 {
		t.Fatalf("want 0 images changed (already within budget), got %d", changed)
	}
}

func TestCompressImagesSkipsTransparentImages(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdf := pdftestutil.SimplePagePDF("BT /F1 12 Tf 72 700 Td (Hello) Tj ET\n")
	bigPNG := solidPNG(t, 1500, 1500)

	withImage, err := AddImageToPDF(context.Background(), pdf, bigPNG, "1", 90, "center")
	if err != nil {
		t.Fatalf("AddImageToPDF: %v", err)
	}

	_, changed, err := CompressImages(context.Background(), withImage, 500, 70)
	if err != nil {
		t.Fatalf("CompressImages: %v", err)
	}
	if changed != 0 {
		t.Fatalf("want 0 images changed (transparent image should be skipped), got %d", changed)
	}
}

func TestCompressImagesRejectsBadInput(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdf := pdftestutil.SimplePagePDF("BT /F1 12 Tf 72 700 Td (Hello) Tj ET\n")

	cases := []struct {
		name         string
		maxDim, qual int
	}{
		{"max dim too small", 1, 70},
		{"max dim too big", 20000, 70},
		{"quality zero", 500, 0},
		{"quality over 100", 500, 101},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := CompressImages(context.Background(), pdf, c.maxDim, c.qual)
			var bad *pdfcore.BadInputError
			if !errors.As(err, &bad) {
				t.Fatalf("expected a BadInputError, got %T: %v", err, err)
			}
		})
	}
}
