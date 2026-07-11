// Regression tests for the PDF text-editing engine. Fixtures are built
// programmatically (rather than checked in as binary .pdf files) to keep
// the repo dependency-free and diffs readable. Each test targets a real bug
// found and fixed during development, so a future change that reintroduces
// one of them fails loudly here instead of only showing up against a real
// user document.
package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

func requireQpdf(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("qpdf"); err != nil {
		t.Skip("qpdf not installed - skipping (it's a hard runtime dependency of the app, install it to run this test)")
	}
}

// testObj is one object in a hand-built fixture PDF. Dict must be a
// complete "<< ... >>" dictionary; for a stream object it must NOT include
// /Length - buildTestPDF injects that automatically from len(Stream).
type testObj struct {
	Dict   string
	Stream []byte
}

// buildTestPDF assembles a minimal classic (non-encrypted, non-linearized)
// PDF from a list of objects, numbered 1..len(objs) in order, with a
// trailer pointing /Root at rootObjNum.
func buildTestPDF(objs []testObj, rootObjNum int) []byte {
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

// simplePagePDF builds a one-page PDF using plain Helvetica (a standard
// base-14 font with no embedded /Widths, so it's always width-safe) with
// the given content stream, plus any extra objects (fonts, XObjects, etc.)
// the caller wants appended after the standard 5 objects.
func simplePagePDF(content string, extra ...testObj) []byte {
	objs := []testObj{
		{Dict: "<< /Type /Catalog /Pages 2 0 R >>"},
		{Dict: "<< /Type /Pages /Kids [3 0 R] /Count 1 >>"},
		{Dict: "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>"},
		{Dict: "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"},
		{Stream: []byte(content), Dict: "<<>>"},
	}
	objs = append(objs, extra...)
	return buildTestPDF(objs, 1)
}

func mustEditPDF(t *testing.T, pdf []byte, find, repl, lang string, page, occurrence int) (out []byte, count int) {
	t.Helper()
	requireQpdf(t)
	out, count, err := EditPDF(context.Background(), pdf, find, repl, lang, page, occurrence)
	if err != nil {
		t.Fatalf("EditPDF(%q -> %q): %v", find, repl, err)
	}
	return out, count
}

func extractText(t *testing.T, pdf []byte) string {
	t.Helper()
	objects, err := parseObjects(mustNormalize(t, pdf))
	if err != nil {
		t.Fatalf("parseObjects: %v", err)
	}
	objByNum := make(map[int]int, len(objects))
	for idx, o := range objects {
		objByNum[o.num] = idx
	}
	var out strings.Builder
	for pageIdx := range objects {
		if !isPageObject(objects[pageIdx].body) {
			continue
		}
		var rootScope resScope
		if resOwner, rStart, rEnd, ok := resolvePageResources(objects, objByNum, pageIdx); ok {
			rootScope = resolveScope(objects, objByNum, map[int]*resolvedFont{}, "turkish", resOwner, rStart, rEnd)
		}
		var units []formContentUnit
		visiting := map[int]bool{}
		for _, cIdx := range resolvePageContents(objects, objByNum, pageIdx) {
			walkContent(objects, objByNum, map[int]*resolvedFont{}, "turkish", cIdx, rootScope, 0, visiting, &units)
		}
		for _, u := range units {
			dr := decodeContentStream(tokenizeContentStream(objects[u.streamIdx].stream), u.scope.fontMap)
			out.WriteString(string(dr.runes))
		}
	}
	return out.String()
}

func mustNormalize(t *testing.T, pdf []byte) []byte {
	t.Helper()
	norm, err := normalize(context.Background(), pdf)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return norm
}

// --- Basic replace-all and occurrence targeting -----------------------

func TestReplaceAll(t *testing.T) {
	pdf := simplePagePDF("BT /F1 24 Tf 50 700 Td (ACME first line) Tj ET\n" +
		"BT /F1 24 Tf 50 650 Td (ACME second line) Tj ET\n" +
		"BT /F1 24 Tf 50 600 Td (ACME third line) Tj ET\n")

	out, count := mustEditPDF(t, pdf, "ACME", "GLOBEX", "latin", 0, -1)
	if count != 3 {
		t.Fatalf("want 3 replacements, got %d", count)
	}
	text := extractText(t, out)
	if strings.Contains(text, "ACME") {
		t.Errorf("expected no ACME left, got %q", text)
	}
	if strings.Count(text, "GLOBEX") != 3 {
		t.Errorf("expected 3 GLOBEX, got %q", text)
	}
}

func TestOccurrenceTargeting(t *testing.T) {
	pdf := simplePagePDF("BT /F1 24 Tf 50 700 Td (ACME first line) Tj ET\n" +
		"BT /F1 24 Tf 50 650 Td (ACME second line) Tj ET\n" +
		"BT /F1 24 Tf 50 600 Td (ACME third line) Tj ET\n")

	// Target only the 2nd occurrence (0-indexed: 1) on page 1.
	out, count := mustEditPDF(t, pdf, "ACME", "GLOBEX", "latin", 1, 1)
	if count != 1 {
		t.Fatalf("want 1 replacement, got %d", count)
	}
	text := extractText(t, out)
	if !strings.Contains(text, "ACME first") || !strings.Contains(text, "ACME third") {
		t.Errorf("expected first/third ACME untouched, got %q", text)
	}
	if !strings.Contains(text, "GLOBEX second") {
		t.Errorf("expected second occurrence replaced, got %q", text)
	}
}

// --- Structure preservation: kerning numbers and array siblings must ---
// --- survive a same-font replacement untouched --------------------------

func TestKerningPreservedInSameFontReplace(t *testing.T) {
	// A TJ array with kerning numbers around a colon, mimicking a
	// "Label: value" field - the pattern that used to lose its kerning
	// numbers (and, before that, corrupt the array's syntax entirely).
	pdf := simplePagePDF("BT /F1 24 Tf 50 700 Td [(Hello) -30 (World:) 20 (Test)] TJ ET\n")

	out, count := mustEditPDF(t, pdf, "World", "Earth", "latin", 0, -1)
	if count != 1 {
		t.Fatalf("want 1 replacement, got %d", count)
	}
	objects, err := parseObjects(mustNormalize(t, out))
	if err != nil {
		t.Fatal(err)
	}
	var streamStr string
	for _, o := range objects {
		if o.hasStream && strings.Contains(string(o.stream), "TJ") {
			streamStr = string(o.stream)
		}
	}
	if !strings.Contains(streamStr, "-30") || !strings.Contains(streamStr, "20") {
		t.Errorf("kerning numbers were not preserved, stream: %q", streamStr)
	}
	if !strings.Contains(streamStr, "Earth:") {
		t.Errorf("expected the colon to stay attached to the replaced word, stream: %q", streamStr)
	}
	text := extractText(t, out)
	if text != "HelloEarth:Test" {
		t.Errorf("want %q, got %q", "HelloEarth:Test", text)
	}
}

func TestNoDuplicateOperatorOnSimpleReplace(t *testing.T) {
	// Regression: a same-font replacement of a plain "(x) Tj" call used to
	// leave the original operator behind, producing a bare, operand-less
	// "Tj" right after the new one - invalid content-stream syntax.
	pdf := simplePagePDF("BT /F1 24 Tf 50 700 Td (ACME) Tj ET\n")
	out, count := mustEditPDF(t, pdf, "ACME", "GLOBEX", "latin", 0, -1)
	if count != 1 {
		t.Fatalf("want 1 replacement, got %d", count)
	}
	objects, err := parseObjects(mustNormalize(t, out))
	if err != nil {
		t.Fatal(err)
	}
	var streamStr string
	for _, o := range objects {
		if o.hasStream && strings.Contains(string(o.stream), "GLOBEX") {
			streamStr = string(o.stream)
		}
	}
	if strings.Count(streamStr, "Tj") != 1 {
		t.Errorf("expected exactly one Tj operator, stream: %q", streamStr)
	}
}

// --- Zero-width subset font: must reroute instead of overlapping -------

func TestZeroWidthFontReroutesToFallback(t *testing.T) {
	// A font declaring /Widths only for 'A' (byte 65); every other byte in
	// range has width 0, mimicking a subset font that never embedded
	// widths for characters the original document didn't use. Replacing
	// with text that needs one of those zero-width bytes must reroute to
	// the fallback font instead of silently overlapping glyphs.
	sparseFont := testObj{Dict: "<< /Type /Font /Subtype /TrueType /BaseFont /Sparse " +
		"/FirstChar 65 /LastChar 67 /Widths [ 700 0 0 ] /FontDescriptor 6 0 R >>"}
	descriptor := testObj{Dict: "<< /Type /FontDescriptor /FontName /Sparse /Flags 32 " +
		"/FontBBox [0 0 1000 1000] /ItalicAngle 0 /Ascent 700 /Descent -200 /CapHeight 700 /StemV 80 >>"}

	pdf := simplePagePDF("BT /F2 10 Tf 50 700 Td (A) Tj ET\n",
		sparseFont, descriptor)
	// Point the page at font F2 = obj 6 (sparseFont) instead of F1.
	pdf = []byte(strings.Replace(string(pdf), "/F1 4 0 R", "/F1 4 0 R /F2 6 0 R", 1))

	out, count := mustEditPDF(t, pdf, "A", "AB", "latin", 0, -1)
	if count != 1 {
		t.Fatalf("want 1 replacement, got %d", count)
	}
	objects, err := parseObjects(mustNormalize(t, out))
	if err != nil {
		t.Fatal(err)
	}
	var streamStr string
	for _, o := range objects {
		if o.hasStream && strings.Contains(string(o.stream), "AB") {
			streamStr = string(o.stream)
		}
	}
	if !strings.Contains(streamStr, fallbackFontResourceName) {
		t.Errorf("expected reroute to the fallback font (byte 66/'B' has width 0 in the sparse font), stream: %q", streamStr)
	}
}

// --- q/Q graphics-state font stack --------------------------------------

func TestFontStateRestoredAcrossQQ(t *testing.T) {
	// Regression: a font switch made via Tf *inside* a q/Q pair must not
	// leak into content after the matching Q - Q restores whatever font
	// was active before the q. This used to cause a field with no Tf of
	// its own (relying on the font from before some earlier q/Q block) to
	// be decoded through the wrong, leftover font.
	//
	// F2 here is a fake CID font (no real embedded font data - our decoder
	// never touches FontFile2, only /ToUnicode) so that misinterpreting
	// "(AB)" through it (as one 2-byte CID code) is easy to detect: it
	// wouldn't decode to "AB" at all.
	cidFont := testObj{Dict: "<< /Type /Font /Subtype /Type0 /BaseFont /Fake " +
		"/Encoding /Identity-H /ToUnicode 7 0 R >>"}
	toUnicode := testObj{Stream: []byte(
		"/CIDInit /ProcSet findresource begin\n" +
			"1 begincodespacerange <0000> <FFFF> endcodespacerange\n" +
			"1 beginbfchar <0001> <005A> endbfchar\n" + // CID 1 -> 'Z'
			"endcmap"), Dict: "<<>>"}

	content := "BT /F1 10 Tf 1 0 0 1 0 700 Td (X) Tj ET " +
		"q BT /F2 10 Tf 1 0 0 1 0 650 Td <0001> Tj ET Q " +
		"BT 1 0 0 1 0 600 Td (AB) Tj ET"

	pdf := simplePagePDF(content, cidFont, toUnicode)
	pdf = []byte(strings.Replace(string(pdf), "/F1 4 0 R", "/F1 4 0 R /F2 6 0 R", 1))

	text := extractText(t, pdf)
	if !strings.Contains(text, "AB") {
		t.Errorf("expected font state to be restored to F1 after Q so \"AB\" decodes as plain text, got %q", text)
	}
	if !strings.Contains(text, "XZ") && !strings.Contains(text, "X") {
		t.Errorf("expected the CID char to decode via ToUnicode to 'Z', got %q", text)
	}

	out, count := mustEditPDF(t, pdf, "AB", "CD", "latin", 0, -1)
	if count != 1 {
		t.Fatalf("want 1 replacement (proves \"AB\" was matchable at all), got %d", count)
	}
	if !strings.Contains(extractText(t, out), "CD") {
		t.Error("replacement did not apply")
	}
}

// --- Form XObject content resolution ------------------------------------

func TestFormXObjectContentIsSearched(t *testing.T) {
	// Regression: some generators (iText and others) put all of a page's
	// real content inside a Form XObject invoked via "Do", leaving the
	// page's own /Contents almost empty and with no /Font resources at
	// all. pdfer must walk into the Form to find any text.
	form := testObj{Dict: "<< /Type /XObject /Subtype /Form /FormType 1 " +
		"/BBox [0 0 612 792] /Matrix [1 0 0 1 0 0] " +
		"/Resources << /Font << /F1 4 0 R >> >> >>",
		Stream: []byte("BT /F1 24 Tf 50 700 Td (ACME inside form) Tj ET\n")}

	objs := []testObj{
		{Dict: "<< /Type /Catalog /Pages 2 0 R >>"},
		{Dict: "<< /Type /Pages /Kids [3 0 R] /Count 1 >>"},
		{Dict: "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] " +
			"/Resources << /XObject << /Xf1 6 0 R >> >> /Contents 5 0 R >>"},
		{Dict: "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"},
		{Stream: []byte("q 1 0 0 1 0 0 cm /Xf1 Do Q\n"), Dict: "<<>>"},
		form,
	}
	pdf := buildTestPDF(objs, 1)

	out, count := mustEditPDF(t, pdf, "ACME", "GLOBEX", "latin", 0, -1)
	if count != 1 {
		t.Fatalf("want 1 replacement found inside the Form XObject, got %d", count)
	}
	text := extractText(t, out)
	if !strings.Contains(text, "GLOBEX inside form") {
		t.Errorf("want replaced text inside the form, got %q", text)
	}
}

// --- Turkish/Latin encoding table sanity --------------------------------

func TestEncodingTableIsBijective(t *testing.T) {
	// Regression: buildTable used to be able to produce a rune -> byte
	// table where inverting it for decoding was ambiguous/nondeterministic
	// (two runes mapping to the same byte), causing a subset of
	// characters to decode incorrectly at random depending on Go's map
	// iteration order.
	for lang, enc := range encodingTables {
		dec := decodingTables[lang]
		for r, b := range enc {
			if got := dec[b]; got != r {
				t.Errorf("%s: byte %#x round-trips to %q, want %q", lang, b, got, r)
			}
		}
	}
}

func TestTurkishSpecialCharsRoundTrip(t *testing.T) {
	table := encodingTables["turkish"]
	for _, r := range []rune{'ş', 'Ş', 'ğ', 'Ğ', 'ı', 'İ'} {
		b, ok := table[r]
		if !ok {
			t.Errorf("turkish table missing %q", r)
			continue
		}
		if decodingTables["turkish"][b] != r {
			t.Errorf("turkish table: %q -> byte %#x does not decode back to %q", r, b, r)
		}
	}
}
