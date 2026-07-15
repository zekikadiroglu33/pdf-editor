// Regression tests for the PDF -> various-format converter (convert*.go).
package conversion

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"pdfer/pdfcore"
	"pdfer/pdftestutil"
)

// zipPart unzips a converted OOXML file (docx/xlsx/pptx) and returns the
// named part's contents as a string, for substring assertions.
func zipPart(t *testing.T, data []byte, name string) string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("not a valid zip: %v", err)
	}
	for _, f := range zr.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				t.Fatalf("open %s: %v", name, err)
			}
			defer rc.Close()
			b, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			return string(b)
		}
	}
	t.Fatalf("zip has no %s part", name)
	return ""
}

func docxDocumentXML(t *testing.T, docx []byte) string {
	return zipPart(t, docx, "word/document.xml")
}

func TestConvertLineWrapMergesIntoOneParagraph(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdf := pdftestutil.SimplePagePDF(
		"BT /F1 12 Tf 72 700 Td (First line of paragraph one.) Tj\n" +
			"0 -14 Td (Second line continuing paragraph one.) Tj\n" +
			"0 -28 Td (This starts paragraph two after a blank line.) Tj ET\n")

	docx, pages, _, filename, err := ConvertPDF(context.Background(), pdf, "latin", "docx")
	if err != nil {
		t.Fatalf("ConvertPDF: %v", err)
	}
	if pages != 1 {
		t.Fatalf("want 1 page, got %d", pages)
	}
	if filename != "converted.docx" {
		t.Errorf("want filename converted.docx, got %s", filename)
	}
	xml := docxDocumentXML(t, docx)
	if !strings.Contains(xml, "First line of paragraph one. Second line continuing paragraph one.") {
		t.Errorf("expected the two wrapped lines merged into one paragraph, got: %s", xml)
	}
	if !strings.Contains(xml, "This starts paragraph two after a blank line.") {
		t.Errorf("expected the second paragraph's text, got: %s", xml)
	}
	// Two distinct <w:p> paragraphs with text (order matters: the merged
	// line-wrap paragraph must not also contain the second paragraph's text).
	if strings.Count(xml, "<w:p>") != 2 {
		t.Errorf("expected exactly 2 non-empty paragraphs, got: %s", xml)
	}
}

func TestConvertMultiPageInsertsPageBreak(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	objs := []pdftestutil.TestObj{
		{Dict: "<< /Type /Catalog /Pages 2 0 R >>"},
		{Dict: "<< /Type /Pages /Kids [3 0 R 6 0 R] /Count 2 >>"},
		{Dict: "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>"},
		{Dict: "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"},
		{Stream: []byte("BT /F1 12 Tf 72 700 Td (Page one text.) Tj ET\n"), Dict: "<<>>"},
		{Dict: "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 7 0 R >>"},
		{Stream: []byte("BT /F1 12 Tf 72 700 Td (Page two text.) Tj ET\n"), Dict: "<<>>"},
	}
	pdf := pdftestutil.BuildTestPDF(objs, 1)

	docx, pages, _, _, err := ConvertPDF(context.Background(), pdf, "latin", "docx")
	if err != nil {
		t.Fatalf("ConvertPDF: %v", err)
	}
	if pages != 2 {
		t.Fatalf("want 2 pages, got %d", pages)
	}
	xml := docxDocumentXML(t, docx)
	if !strings.Contains(xml, "Page one text.") || !strings.Contains(xml, "Page two text.") {
		t.Errorf("expected both pages' text, got: %s", xml)
	}
	if !strings.Contains(xml, `<w:br w:type="page"/>`) {
		t.Errorf("expected an explicit page break between pages, got: %s", xml)
	}
	if strings.Index(xml, "Page one text.") > strings.Index(xml, `<w:br w:type="page"/>`) ||
		strings.Index(xml, `<w:br w:type="page"/>`) > strings.Index(xml, "Page two text.") {
		t.Errorf("expected page break to sit between page one and page two text, got: %s", xml)
	}
}

func TestConvertBadLangReturnsBadInput(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdf := pdftestutil.SimplePagePDF("BT /F1 12 Tf 72 700 Td (Hello) Tj ET\n")
	_, _, _, _, err := ConvertPDF(context.Background(), pdf, "klingon", "docx")
	if err == nil {
		t.Fatal("expected an error for an unsupported language")
	}
	var bad *pdfcore.BadInputError
	if !errors.As(err, &bad) {
		t.Fatalf("expected a BadInputError, got %T: %v", err, err)
	}
}

func TestConvertBadFormatReturnsBadInput(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdf := pdftestutil.SimplePagePDF("BT /F1 12 Tf 72 700 Td (Hello) Tj ET\n")
	_, _, _, _, err := ConvertPDF(context.Background(), pdf, "latin", "pdf")
	if err == nil {
		t.Fatal("expected an error for an unsupported format")
	}
	var bad *pdfcore.BadInputError
	if !errors.As(err, &bad) {
		t.Fatalf("expected a BadInputError, got %T: %v", err, err)
	}
}

func TestConvertToTxtUsesFormFeedPageBreak(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	objs := []pdftestutil.TestObj{
		{Dict: "<< /Type /Catalog /Pages 2 0 R >>"},
		{Dict: "<< /Type /Pages /Kids [3 0 R 6 0 R] /Count 2 >>"},
		{Dict: "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>"},
		{Dict: "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"},
		{Stream: []byte("BT /F1 12 Tf 72 700 Td (Page one text.) Tj ET\n"), Dict: "<<>>"},
		{Dict: "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 7 0 R >>"},
		{Stream: []byte("BT /F1 12 Tf 72 700 Td (Page two text.) Tj ET\n"), Dict: "<<>>"},
	}
	pdf := pdftestutil.BuildTestPDF(objs, 1)

	data, pages, contentType, filename, err := ConvertPDF(context.Background(), pdf, "latin", "txt")
	if err != nil {
		t.Fatalf("ConvertPDF: %v", err)
	}
	if pages != 2 {
		t.Fatalf("want 2 pages, got %d", pages)
	}
	if filename != "converted.txt" || !strings.HasPrefix(contentType, "text/plain") {
		t.Errorf("unexpected filename/contentType: %s / %s", filename, contentType)
	}
	text := string(data)
	if !strings.Contains(text, "Page one text.") || !strings.Contains(text, "Page two text.") {
		t.Errorf("expected both pages' text, got: %q", text)
	}
	if !strings.Contains(text, "\f") {
		t.Errorf("expected a form-feed page break, got: %q", text)
	}
}

func TestConvertToHTMLEscapesAndBreaksPages(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdf := pdftestutil.SimplePagePDF(`BT /F1 12 Tf 72 700 Td (A <tag> & an ampersand) Tj ET` + "\n")

	data, _, contentType, filename, err := ConvertPDF(context.Background(), pdf, "latin", "html")
	if err != nil {
		t.Fatalf("ConvertPDF: %v", err)
	}
	if filename != "converted.html" || !strings.HasPrefix(contentType, "text/html") {
		t.Errorf("unexpected filename/contentType: %s / %s", filename, contentType)
	}
	html := string(data)
	if strings.Contains(html, "<tag>") {
		t.Errorf("expected PDF text to be HTML-escaped, got raw markup: %s", html)
	}
	if !strings.Contains(html, "&lt;tag&gt;") || !strings.Contains(html, "&amp;") {
		t.Errorf("expected escaped text in output, got: %s", html)
	}
}

func TestConvertToSVGPositionsText(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdf := pdftestutil.SimplePagePDF("BT /F1 12 Tf 72 700 Td (Hello from SVG) Tj ET\n")

	data, pages, contentType, filename, err := ConvertPDF(context.Background(), pdf, "latin", "svg")
	if err != nil {
		t.Fatalf("ConvertPDF: %v", err)
	}
	if pages != 1 {
		t.Fatalf("want 1 page, got %d", pages)
	}
	if filename != "converted.svg" || contentType != "image/svg+xml" {
		t.Errorf("unexpected filename/contentType: %s / %s", filename, contentType)
	}
	svg := string(data)
	if !strings.Contains(svg, "<svg") || !strings.Contains(svg, "Hello from SVG") {
		t.Errorf("expected an <svg> root with the page's text, got: %s", svg)
	}
}

func TestConvertToXlsxOneSheetPerPage(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	objs := []pdftestutil.TestObj{
		{Dict: "<< /Type /Catalog /Pages 2 0 R >>"},
		{Dict: "<< /Type /Pages /Kids [3 0 R 6 0 R] /Count 2 >>"},
		{Dict: "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>"},
		{Dict: "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"},
		{Stream: []byte("BT /F1 12 Tf 72 700 Td (Row one.) Tj ET\n"), Dict: "<<>>"},
		{Dict: "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 7 0 R >>"},
		{Stream: []byte("BT /F1 12 Tf 72 700 Td (Row two.) Tj ET\n"), Dict: "<<>>"},
	}
	pdf := pdftestutil.BuildTestPDF(objs, 1)

	data, pages, _, filename, err := ConvertPDF(context.Background(), pdf, "latin", "xlsx")
	if err != nil {
		t.Fatalf("ConvertPDF: %v", err)
	}
	if pages != 2 {
		t.Fatalf("want 2 pages, got %d", pages)
	}
	if filename != "converted.xlsx" {
		t.Errorf("want filename converted.xlsx, got %s", filename)
	}
	sheet1 := zipPart(t, data, "xl/worksheets/sheet1.xml")
	sheet2 := zipPart(t, data, "xl/worksheets/sheet2.xml")
	if !strings.Contains(sheet1, "Row one.") {
		t.Errorf("expected page one's text in sheet1, got: %s", sheet1)
	}
	if !strings.Contains(sheet2, "Row two.") {
		t.Errorf("expected page two's text in sheet2, got: %s", sheet2)
	}
}

func TestConvertToPptxOneSlidePerPage(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	objs := []pdftestutil.TestObj{
		{Dict: "<< /Type /Catalog /Pages 2 0 R >>"},
		{Dict: "<< /Type /Pages /Kids [3 0 R 6 0 R] /Count 2 >>"},
		{Dict: "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>"},
		{Dict: "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"},
		{Stream: []byte("BT /F1 12 Tf 72 700 Td (Slide one text.) Tj ET\n"), Dict: "<<>>"},
		{Dict: "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 7 0 R >>"},
		{Stream: []byte("BT /F1 12 Tf 72 700 Td (Slide two text.) Tj ET\n"), Dict: "<<>>"},
	}
	pdf := pdftestutil.BuildTestPDF(objs, 1)

	data, pages, _, filename, err := ConvertPDF(context.Background(), pdf, "latin", "pptx")
	if err != nil {
		t.Fatalf("ConvertPDF: %v", err)
	}
	if pages != 2 {
		t.Fatalf("want 2 pages, got %d", pages)
	}
	if filename != "converted.pptx" {
		t.Errorf("want filename converted.pptx, got %s", filename)
	}
	slide1 := zipPart(t, data, "ppt/slides/slide1.xml")
	slide2 := zipPart(t, data, "ppt/slides/slide2.xml")
	if !strings.Contains(slide1, "Slide one text.") {
		t.Errorf("expected page one's text in slide1, got: %s", slide1)
	}
	if !strings.Contains(slide2, "Slide two text.") {
		t.Errorf("expected page two's text in slide2, got: %s", slide2)
	}
}

func TestConvertToPNGSinglePage(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdftestutil.RequirePdftoppm(t)
	pdf := pdftestutil.SimplePagePDF("BT /F1 12 Tf 72 700 Td (Hello raster) Tj ET\n")

	data, pages, contentType, filename, err := ConvertPDF(context.Background(), pdf, "latin", "png")
	if err != nil {
		t.Fatalf("ConvertPDF: %v", err)
	}
	if pages != 1 {
		t.Fatalf("want 1 page, got %d", pages)
	}
	if filename != "converted.png" || contentType != "image/png" {
		t.Errorf("unexpected filename/contentType: %s / %s", filename, contentType)
	}
	if !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
		t.Errorf("expected a PNG signature, got %d bytes starting %x", len(data), data[:minInt(8, len(data))])
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestConvertToJPEGAndTIFF(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdftestutil.RequirePdftoppm(t)
	pdf := pdftestutil.SimplePagePDF("BT /F1 12 Tf 72 700 Td (Hello raster) Tj ET\n")

	cases := []struct{ format, filename, contentType string }{
		{"jpeg", "converted.jpg", "image/jpeg"},
		{"tiff", "converted.tif", "image/tiff"},
	}
	for _, c := range cases {
		data, pages, contentType, filename, err := ConvertPDF(context.Background(), pdf, "latin", c.format)
		if err != nil {
			t.Fatalf("ConvertPDF(%s): %v", c.format, err)
		}
		if pages != 1 {
			t.Fatalf("%s: want 1 page, got %d", c.format, pages)
		}
		if filename != c.filename || contentType != c.contentType {
			t.Errorf("%s: unexpected filename/contentType: %s / %s", c.format, filename, contentType)
		}
		if len(data) == 0 {
			t.Errorf("%s: expected non-empty image data", c.format)
		}
	}
}

func TestConvertToWebP(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdftestutil.RequirePdftoppm(t)
	pdftestutil.RequireCwebp(t)
	pdf := pdftestutil.SimplePagePDF("BT /F1 12 Tf 72 700 Td (Hello raster) Tj ET\n")

	data, pages, contentType, filename, err := ConvertPDF(context.Background(), pdf, "latin", "webp")
	if err != nil {
		t.Fatalf("ConvertPDF: %v", err)
	}
	if pages != 1 {
		t.Fatalf("want 1 page, got %d", pages)
	}
	if filename != "converted.webp" || contentType != "image/webp" {
		t.Errorf("unexpected filename/contentType: %s / %s", filename, contentType)
	}
	if !bytes.Contains(data[:minInt(16, len(data))], []byte("WEBP")) {
		t.Errorf("expected a RIFF/WEBP signature, got %d bytes starting %x", len(data), data[:minInt(16, len(data))])
	}
}

func TestConvertToPNGMultiPageZips(t *testing.T) {
	pdftestutil.RequireQpdf(t)
	pdftestutil.RequirePdftoppm(t)
	objs := []pdftestutil.TestObj{
		{Dict: "<< /Type /Catalog /Pages 2 0 R >>"},
		{Dict: "<< /Type /Pages /Kids [3 0 R 6 0 R] /Count 2 >>"},
		{Dict: "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>"},
		{Dict: "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"},
		{Stream: []byte("BT /F1 12 Tf 72 700 Td (Page one.) Tj ET\n"), Dict: "<<>>"},
		{Dict: "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 7 0 R >>"},
		{Stream: []byte("BT /F1 12 Tf 72 700 Td (Page two.) Tj ET\n"), Dict: "<<>>"},
	}
	pdf := pdftestutil.BuildTestPDF(objs, 1)

	data, pages, contentType, filename, err := ConvertPDF(context.Background(), pdf, "latin", "png")
	if err != nil {
		t.Fatalf("ConvertPDF: %v", err)
	}
	if pages != 2 {
		t.Fatalf("want 2 pages, got %d", pages)
	}
	if filename != "converted_png.zip" || contentType != "application/zip" {
		t.Errorf("unexpected filename/contentType: %s / %s", filename, contentType)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("not a valid zip: %v", err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if len(names) != 2 || names[0] != "page-1.png" || names[1] != "page-2.png" {
		t.Errorf("expected [page-1.png page-2.png], got %v", names)
	}
}
