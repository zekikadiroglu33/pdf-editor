// Regression tests for the crop tool (pdfcrop.go).
package pdftools

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"testing"

	"pdfer/pdfcore"
	"pdfer/pdftestutil"
)

func TestCropSetsCropBoxFromMargins(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdf := pdftestutil.SimplePagePDF("BT /F1 12 Tf 72 700 Td (Hello) Tj ET\n") // MediaBox [0 0 612 792]

	out, err := CropPDF(context.Background(), pdf, 10, 10, 20, 20, "")
	if err != nil {
		t.Fatalf("CropPDF: %v", err)
	}
	norm, err := pdfcore.Normalize(context.Background(), out)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	// left/right 10% of 612 = 61.2 -> [61.2, ..., 550.8]; top/bottom 20% of
	// 792 = 158.4 -> [..., 158.4, ..., 633.6]. Checked without the exact
	// bracket spacing, since re-normalizing through qpdf (as this test
	// does via pdfcore.Normalize()) reformats array spacing to its own
	// pretty-printing convention ("[ a b c d ]" vs our "[a b c d]").
	if !bytes.Contains(norm, []byte("/CropBox")) || !bytes.Contains(norm, []byte("61.20 158.40 550.80 633.60")) {
		t.Errorf("expected a /CropBox of 61.20 158.40 550.80 633.60 in output, got:\n%s", norm)
	}
}

func TestCropAppliesToSelectedPageOnly(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdf := pdftestutil.TwoPagePDF()

	out, err := CropPDF(context.Background(), pdf, 5, 5, 5, 5, "1")
	if err != nil {
		t.Fatalf("CropPDF: %v", err)
	}
	norm, err := pdfcore.Normalize(context.Background(), out)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if strconv.Itoa(bytes.Count(norm, []byte("/CropBox"))) != "1" {
		t.Errorf("expected exactly 1 /CropBox entry (page 1 only), got %d in:\n%s",
			bytes.Count(norm, []byte("/CropBox")), norm)
	}
}

func TestCropReplacesExistingCropBox(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	objs := []pdftestutil.TestObj{
		{Dict: "<< /Type /Catalog /Pages 2 0 R >>"},
		{Dict: "<< /Type /Pages /Kids [3 0 R] /Count 1 >>"},
		{Dict: "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /CropBox [0 0 300 300] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>"},
		{Dict: "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"},
		{Stream: []byte("BT /F1 12 Tf 72 700 Td (Hello) Tj ET\n"), Dict: "<<>>"},
	}
	pdf := pdftestutil.BuildTestPDF(objs, 1)

	out, err := CropPDF(context.Background(), pdf, 0, 0, 0, 0, "")
	if err != nil {
		t.Fatalf("CropPDF: %v", err)
	}
	norm, err := pdfcore.Normalize(context.Background(), out)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if bytes.Contains(norm, []byte("[0 0 300 300]")) {
		t.Errorf("expected the pre-existing /CropBox to be replaced, still found old value in:\n%s", norm)
	}
	if bytes.Count(norm, []byte("/CropBox")) != 1 {
		t.Errorf("expected exactly one /CropBox entry (replaced, not duplicated), got %d", bytes.Count(norm, []byte("/CropBox")))
	}
}

func TestCropRejectsBadMargins(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdf := pdftestutil.SimplePagePDF("BT /F1 12 Tf 72 700 Td (Hello) Tj ET\n")

	cases := []struct {
		name                     string
		left, right, top, bottom float64
	}{
		{"negative", -1, 0, 0, 0},
		{"too large single", 50, 0, 0, 0},
		{"left+right over 100", 60, 60, 0, 0},
		{"top+bottom over 100", 0, 0, 60, 60},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := CropPDF(context.Background(), pdf, c.left, c.right, c.top, c.bottom, "")
			var bad *pdfcore.BadInputError
			if !errors.As(err, &bad) {
				t.Fatalf("expected a BadInputError, got %T: %v", err, err)
			}
		})
	}
}

func TestCropOutputIsValidPDF(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdf := pdftestutil.SimplePagePDF("BT /F1 12 Tf 72 700 Td (Hello) Tj ET\n")

	out, err := CropPDF(context.Background(), pdf, 10, 10, 10, 10, "")
	if err != nil {
		t.Fatalf("CropPDF: %v", err)
	}
	path, err := writeTempPDF(out, "pdfer-cropcheck")
	if err != nil {
		t.Fatalf("writeTempPDF: %v", err)
	}
	if err := runQpdfRaw(context.Background(), "--check", path); err != nil {
		t.Fatalf("qpdf --check failed on cropped output: %v", err)
	}
}
