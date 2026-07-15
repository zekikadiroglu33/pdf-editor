// The fallback font lets replacement/watermark text render correctly
// even when there's no original font to match at all (a watermark), or
// when the matched text came from a subsetted embedded font (typically
// a CID/Identity-H font) that may lack the glyphs needed for the
// replacement. See fallbackfont_data.go (generated) for its metrics.
package pdfcore

import (
	"bytes"
	_ "embed"
	"fmt"
)

//go:embed assets/pdfer-fallback.ttf
var fallbackFontTTF []byte

// FallbackFontResourceName is the /Font resource key added to a page's
// Resources dictionary the first time that page needs the fallback font.
const FallbackFontResourceName = "PDFerFB1"

// BuildFallbackFontObjects returns the three new PDF objects needed to
// embed the fallback font (FontFile2 stream, FontDescriptor, Font dict),
// numbered starting at firstNum. It also returns the Font dict's object
// number, for wiring into a page's Resources /Font entry.
func BuildFallbackFontObjects(firstNum int) (objs []PdfObject, fontObjNum int) {
	fontFileNum := firstNum
	descNum := firstNum + 1
	fontNum := firstNum + 2

	fontFileObj := PdfObject{
		Num:       fontFileNum,
		Body:      []byte(fmt.Sprintf("<< /Length %d /Length1 %d >>", len(fallbackFontTTF), len(fallbackFontTTF))),
		Stream:    fallbackFontTTF,
		HasStream: true,
	}

	descObj := PdfObject{
		Num: descNum,
		Body: []byte(fmt.Sprintf(
			"<< /Type /FontDescriptor /FontName /PDFerFallback /Flags 32 "+
				"/FontBBox [%d %d %d %d] /ItalicAngle %d /Ascent %d /Descent %d "+
				"/CapHeight %d /StemV %d /FontFile2 %d 0 R >>",
			fallbackFontBBox[0], fallbackFontBBox[1], fallbackFontBBox[2], fallbackFontBBox[3],
			fallbackFontItalicAngle, fallbackFontAscent, fallbackFontDescent,
			fallbackFontCapHeight, fallbackFontStemV, fontFileNum)),
	}

	var widths bytes.Buffer
	widths.WriteByte('[')
	for i, w := range FallbackFontWidths {
		if i > 0 {
			widths.WriteByte(' ')
		}
		fmt.Fprintf(&widths, "%d", w)
	}
	widths.WriteByte(']')

	fontObj := PdfObject{
		Num: fontNum,
		Body: []byte(fmt.Sprintf(
			"<< /Type /Font /Subtype /TrueType /BaseFont /PDFerFallback "+
				"/FirstChar 0 /LastChar 255 /Widths %s /FontDescriptor %d 0 R "+
				"/Encoding << /BaseEncoding /WinAnsiEncoding /Differences "+
				"[208 /Gbreve 221 /Idotaccent 222 /Scedilla 240 /gbreve 253 /dotlessi 254 /scedilla] >> >>",
			widths.String(), descNum)),
	}

	return []PdfObject{fontFileObj, descObj, fontObj}, fontNum
}
