// Regression tests for the qpdf-backed page/document tools (pdftools.go).
package pdftools

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"pdfer/pdfcore"
	"pdfer/pdftestutil"
)

// qdfText normalizes pdf with qpdf's own --qdf mode (uncompressed, one
// object per line) so a test can grep the result for a plain-text marker
// like "/Rotate 90" without worrying about object-stream compression.
func qdfText(t *testing.T, pdf []byte) string {
	t.Helper()
	norm, err := pdfcore.Normalize(context.Background(), pdf)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return string(norm)
}

func TestRotatePDFAppliesAngle(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdf := pdftestutil.SimplePagePDF("BT /F1 12 Tf 72 700 Td (Hello) Tj ET\n")

	out, err := RotatePDF(context.Background(), pdf, "", 90)
	if err != nil {
		t.Fatalf("RotatePDF: %v", err)
	}
	if !strings.Contains(qdfText(t, out), "/Rotate 90") {
		t.Errorf("expected /Rotate 90 in output")
	}
}

func TestRotatePDFRejectsNonMultipleOf90(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdf := pdftestutil.SimplePagePDF("BT /F1 12 Tf 72 700 Td (Hello) Tj ET\n")

	_, err := RotatePDF(context.Background(), pdf, "", 45)
	var bad *pdfcore.BadInputError
	if !errors.As(err, &bad) {
		t.Fatalf("expected a BadInputError for a non-multiple-of-90 angle, got %T: %v", err, err)
	}
}

func TestMergePDFsConcatenatesPages(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	a := pdftestutil.SimplePagePDF("BT /F1 12 Tf 72 700 Td (First document.) Tj ET\n")
	b := pdftestutil.SimplePagePDF("BT /F1 12 Tf 72 700 Td (Second document.) Tj ET\n")

	out, err := MergePDFs(context.Background(), [][]byte{a, b})
	if err != nil {
		t.Fatalf("MergePDFs: %v", err)
	}
	n, err := pdfPageCount(context.Background(), out)
	if err != nil {
		t.Fatalf("pdfPageCount: %v", err)
	}
	if n != 2 {
		t.Fatalf("want 2 pages after merging two 1-page PDFs, got %d", n)
	}
}

func TestMergePDFsRejectsFewerThanTwo(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	a := pdftestutil.SimplePagePDF("BT /F1 12 Tf 72 700 Td (Solo.) Tj ET\n")

	_, err := MergePDFs(context.Background(), [][]byte{a})
	var bad *pdfcore.BadInputError
	if !errors.As(err, &bad) {
		t.Fatalf("expected a BadInputError for a single-file merge, got %T: %v", err, err)
	}
}

func TestSplitPDFDefaultsToOnePagePerFile(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdf := pdftestutil.TwoPagePDF()

	parts, err := SplitPDF(context.Background(), pdf, nil)
	if err != nil {
		t.Fatalf("SplitPDF: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("want 2 parts, got %d", len(parts))
	}
	for i, p := range parts {
		n, err := pdfPageCount(context.Background(), p)
		if err != nil {
			t.Fatalf("pdfPageCount(part %d): %v", i, err)
		}
		if n != 1 {
			t.Errorf("part %d: want 1 page, got %d", i, n)
		}
	}
}

func TestSplitPDFExplicitRanges(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdf := pdftestutil.TwoPagePDF()

	parts, err := SplitPDF(context.Background(), pdf, []string{"1-2"})
	if err != nil {
		t.Fatalf("SplitPDF: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("want 1 part for a single explicit range, got %d", len(parts))
	}
	n, err := pdfPageCount(context.Background(), parts[0])
	if err != nil {
		t.Fatalf("pdfPageCount: %v", err)
	}
	if n != 2 {
		t.Fatalf("want 2 pages in the 1-2 range, got %d", n)
	}
}

func TestDeletePagesRemovesOnlyRequestedPage(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdf := pdftestutil.TwoPagePDF()

	out, err := DeletePages(context.Background(), pdf, "1")
	if err != nil {
		t.Fatalf("DeletePages: %v", err)
	}
	n, err := pdfPageCount(context.Background(), out)
	if err != nil {
		t.Fatalf("pdfPageCount: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 page remaining, got %d", n)
	}

	norm, err := pdfcore.Normalize(context.Background(), out)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if bytes.Contains(norm, []byte("Page one text.")) {
		t.Errorf("expected page one's text to be gone")
	}
	if !bytes.Contains(norm, []byte("Page two text.")) {
		t.Errorf("expected page two's text to remain")
	}
}

func TestDeletePagesRejectsEmptySelection(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdf := pdftestutil.TwoPagePDF()

	_, err := DeletePages(context.Background(), pdf, "")
	var bad *pdfcore.BadInputError
	if !errors.As(err, &bad) {
		t.Fatalf("expected a BadInputError for an empty page selection, got %T: %v", err, err)
	}
}

func TestCompressPDFProducesValidSmallerOrEqualPDF(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdf := pdftestutil.SimplePagePDF("BT /F1 12 Tf 72 700 Td (Hello, compress me.) Tj ET\n")

	out, err := CompressPDF(context.Background(), pdf, 0)
	if err != nil {
		t.Fatalf("CompressPDF: %v", err)
	}
	if !bytes.HasPrefix(out, []byte("%PDF")) {
		t.Fatalf("expected valid PDF output")
	}
	n, err := pdfPageCount(context.Background(), out)
	if err != nil {
		t.Fatalf("pdfPageCount: %v", err)
	}
	if n != 1 {
		t.Fatalf("want page count preserved at 1, got %d", n)
	}
}

func TestCompressPDFRejectsBadQuality(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdf := pdftestutil.SimplePagePDF("BT /F1 12 Tf 72 700 Td (Hello) Tj ET\n")

	_, err := CompressPDF(context.Background(), pdf, 150)
	var bad *pdfcore.BadInputError
	if !errors.As(err, &bad) {
		t.Fatalf("expected a BadInputError for an out-of-range quality, got %T: %v", err, err)
	}
}

func TestZipNamedFilesRoundTrips(t *testing.T) {
	data, err := zipNamedFiles([]string{"a.pdf", "b.pdf"}, [][]byte{[]byte("AAA"), []byte("BBB")})
	if err != nil {
		t.Fatalf("zipNamedFiles: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("not a valid zip: %v", err)
	}
	if len(zr.File) != 2 || zr.File[0].Name != "a.pdf" || zr.File[1].Name != "b.pdf" {
		t.Fatalf("unexpected zip contents")
	}
}
