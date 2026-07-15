// Regression tests for the text-watermark tool (pdfwatermark.go).
package pdftools

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"pdfer/pdfcore"
	"pdfer/pdftestutil"
)

// pdftotextPages runs pdftotext -layout on pdf and returns the extracted
// text per page (poppler's pdftotext, already a Prerequisite for the
// raster converter - see conversion/convert_raster.go).
func pdftotextPages(t *testing.T, pdf []byte) []string {
	t.Helper()
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("pdftotext not installed - skipping (install poppler-utils / poppler to run this test)")
	}
	path, err := writeTempPDF(pdf, "pdfer-wmtest")
	if err != nil {
		t.Fatalf("writeTempPDF: %v", err)
	}
	out, err := exec.Command("pdftotext", "-layout", path, "-").Output()
	if err != nil {
		t.Fatalf("pdftotext: %v", err)
	}
	return strings.Split(string(out), "\f")
}

// hasAllRunes reports whether every rune in needle appears somewhere in
// haystack. Used instead of strings.Contains for the diagonal watermark
// text specifically: pdftotext's layout mode breaks rotated text into
// separate short lines in an order that doesn't reassemble into the
// original word via simple concatenation, so this checks "were all these
// glyphs actually drawn" without depending on poppler's line-ordering
// for non-horizontal text.
func hasAllRunes(haystack, needle string) bool {
	for _, r := range needle {
		if !strings.ContainsRune(haystack, r) {
			return false
		}
	}
	return true
}

func TestWatermarkAppliesToAllPagesByDefault(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdf := pdftestutil.TwoPagePDF()

	out, err := WatermarkPDF(context.Background(), pdf, "CONFIDENTIAL", 0.3, 48, "")
	if err != nil {
		t.Fatalf("WatermarkPDF: %v", err)
	}
	pages := pdftotextPages(t, out)
	if len(pages) < 2 {
		t.Fatalf("want at least 2 pages of text, got %d", len(pages))
	}
	for i := 0; i < 2; i++ {
		if !hasAllRunes(pages[i], "CONFIDENTIAL") {
			t.Errorf("page %d: expected watermark text, got: %q", i+1, pages[i])
		}
	}
}

func TestWatermarkAppliesToSinglePageOnly(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdf := pdftestutil.TwoPagePDF()

	out, err := WatermarkPDF(context.Background(), pdf, "DRAFT", 0.3, 48, "2")
	if err != nil {
		t.Fatalf("WatermarkPDF: %v", err)
	}
	pages := pdftotextPages(t, out)
	if len(pages) < 2 {
		t.Fatalf("want at least 2 pages of text, got %d", len(pages))
	}
	if hasAllRunes(pages[0], "DRAFT") {
		t.Errorf("expected page 1 to be unwatermarked, got: %q", pages[0])
	}
	if !hasAllRunes(pages[1], "DRAFT") {
		t.Errorf("expected page 2 to contain the watermark, got: %q", pages[1])
	}
	// Original page text must still be present on both pages.
	if !strings.Contains(pages[0], "Page one text.") || !strings.Contains(pages[1], "Page two text.") {
		t.Errorf("expected original page text preserved, got: %q / %q", pages[0], pages[1])
	}
}

func TestWatermarkRejectsBadInput(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdf := pdftestutil.SimplePagePDF("BT /F1 12 Tf 72 700 Td (Hello) Tj ET\n")

	cases := []struct {
		name            string
		text            string
		opacity, fontPt float64
		pages           string
	}{
		{"empty text", "", 0.3, 48, ""},
		{"zero opacity", "X", 0, 48, ""},
		{"opacity too high", "X", 1.5, 48, ""},
		{"zero font size", "X", 0.3, 0, ""},
		{"font size too big", "X", 0.3, 5000, ""},
		{"page out of range", "X", 0.3, 48, "99"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := WatermarkPDF(context.Background(), pdf, c.text, c.opacity, c.fontPt, c.pages)
			var bad *pdfcore.BadInputError
			if !errors.As(err, &bad) {
				t.Fatalf("expected a BadInputError, got %T: %v", err, err)
			}
		})
	}
}

func TestWatermarkOutputIsValidPDF(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdf := pdftestutil.SimplePagePDF("BT /F1 12 Tf 72 700 Td (Hello) Tj ET\n")

	out, err := WatermarkPDF(context.Background(), pdf, "SAMPLE", 0.4, 60, "")
	if err != nil {
		t.Fatalf("WatermarkPDF: %v", err)
	}
	path, err := writeTempPDF(out, "pdfer-wmcheck")
	if err != nil {
		t.Fatalf("writeTempPDF: %v", err)
	}
	if err := exec.Command("qpdf", "--check", path).Run(); err != nil {
		t.Fatalf("qpdf --check failed on watermarked output: %v", err)
	}
}
