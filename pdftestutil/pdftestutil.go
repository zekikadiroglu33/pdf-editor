// Package pdftestutil holds fixture-building helpers shared by every
// feature package's tests (editor, conversion, pdfTools): hand-built
// classic-PDF fixtures (rather than checked-in binary .pdf files, to
// keep the repo dependency-free and diffs readable) and skip-if-missing
// guards for the external CLI tools some tests need. It's a normal
// (non-_test.go) package specifically so multiple packages' test files
// can import it - Go test files can only see another package's
// unexported internals when they live in that same package's directory,
// but a plain exported helper package like this one is importable from
// anywhere, the same way a project might share a "testutil" library
// across many packages' tests.
package pdftestutil

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// RequireQpdf skips the test if qpdf isn't installed - it's a hard
// runtime dependency of the whole app, so every test that needs it can
// assume it's present in CI/dev but still degrade gracefully elsewhere.
func RequireQpdf(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("qpdf"); err != nil {
		t.Skip("qpdf not installed - skipping (it's a hard runtime dependency of the app, install it to run this test)")
	}
}

func RequirePdftoppm(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("pdftoppm"); err != nil {
		t.Skip("pdftoppm not installed - skipping (install poppler-utils / poppler to run this test)")
	}
}

func RequireCwebp(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("cwebp"); err != nil {
		t.Skip("cwebp not installed - skipping (install webp to run this test)")
	}
}

// TestObj is one object in a hand-built fixture PDF. Dict must be a
// complete "<< ... >>" dictionary; for a stream object it must NOT
// include /Length - BuildTestPDF injects that automatically from
// len(Stream).
type TestObj struct {
	Dict   string
	Stream []byte
}

// BuildTestPDF assembles a minimal classic (non-encrypted,
// non-linearized) PDF from a list of objects, numbered 1..len(objs) in
// order, with a trailer pointing /Root at rootObjNum.
func BuildTestPDF(objs []TestObj, rootObjNum int) []byte {
	var buf strings.Builder
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs)+1)
	for i, o := range objs {
		num := i + 1
		offsets[num] = buf.Len()
		dict := o.Dict
		if o.Stream != nil {
			dict = strings.TrimSuffix(strings.TrimSpace(dict), ">>")
			dict = dict + fmt.Sprintf(" /Length %d >>", len(o.Stream))
		}
		fmt.Fprintf(&buf, "%d 0 obj\n%s", num, dict)
		if o.Stream != nil {
			buf.WriteString("\nstream\n")
			buf.Write(o.Stream)
			buf.WriteString("\nendstream")
		}
		buf.WriteString("\nendobj\n")
	}
	xrefPos := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(objs)+1)
	buf.WriteString("0000000000 65535 f \n")
	for n := 1; n <= len(objs); n++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[n])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root %d 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, rootObjNum, xrefPos)
	return []byte(buf.String())
}

// SimplePagePDF builds a one-page PDF using plain Helvetica (a standard
// base-14 font with no embedded /Widths, so it's always width-safe) with
// the given content stream, plus any extra objects (fonts, XObjects,
// etc.) the caller wants appended after the standard 5 objects.
func SimplePagePDF(content string, extra ...TestObj) []byte {
	objs := []TestObj{
		{Dict: "<< /Type /Catalog /Pages 2 0 R >>"},
		{Dict: "<< /Type /Pages /Kids [3 0 R] /Count 1 >>"},
		{Dict: "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>"},
		{Dict: "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"},
		{Stream: []byte(content), Dict: "<<>>"},
	}
	objs = append(objs, extra...)
	return BuildTestPDF(objs, 1)
}

// TwoPagePDF builds a plain two-page PDF ("Page one text." / "Page two
// text."), used across the pdfTools package's tests (rotate/merge/split/
// delete-pages/compress/watermark/crop) wherever a multi-page fixture is
// needed.
func TwoPagePDF() []byte {
	objs := []TestObj{
		{Dict: "<< /Type /Catalog /Pages 2 0 R >>"},
		{Dict: "<< /Type /Pages /Kids [3 0 R 6 0 R] /Count 2 >>"},
		{Dict: "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>"},
		{Dict: "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"},
		{Stream: []byte("BT /F1 12 Tf 72 700 Td (Page one text.) Tj ET\n"), Dict: "<<>>"},
		{Dict: "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 7 0 R >>"},
		{Stream: []byte("BT /F1 12 Tf 72 700 Td (Page two text.) Tj ET\n"), Dict: "<<>>"},
	}
	return BuildTestPDF(objs, 1)
}
