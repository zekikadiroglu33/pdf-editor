// convert_svg.go - best-effort SVG output.
//
// SVG has no native text reflow (every element is absolutely positioned),
// unlike docx/html/pptx where a paragraph or text box wraps on its own -
// so this is the one format that needs a manual word-wrap (wrapText below)
// to keep long paragraphs from running off the page. All pages are
// stacked into one tall canvas (rather than one file per page, or a zip
// of several) so a multi-page PDF still downloads as one file.
package converter

import (
	"fmt"
	"strings"

	"pdfer/pdfcore"
)

const (
	svgMargin     = 40.0
	svgLineHeight = 16.0
	svgParaGap    = 10.0
	svgFontSize   = 12.0
	svgPageGap    = 24.0
)

// wrapText greedily wraps s into lines of at most maxChars characters,
// breaking only at word boundaries. Used by output formats with no
// native text reflow of their own (svg; pptx boxes do reflow themselves,
// but a pre-wrap keeps very long paragraphs from visually overrunning
// the box).
func wrapText(s string, maxChars int) []string {
	if maxChars <= 0 {
		return []string{s}
	}
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var lines []string
	var cur strings.Builder
	for _, w := range words {
		if cur.Len() > 0 && cur.Len()+1+len(w) > maxChars {
			lines = append(lines, cur.String())
			cur.Reset()
		}
		if cur.Len() > 0 {
			cur.WriteByte(' ')
		}
		cur.WriteString(w)
	}
	if cur.Len() > 0 {
		lines = append(lines, cur.String())
	}
	return lines
}

func buildSVGFromPages(pages []pdfPage) ([]byte, error) {
	maxWidth := 0.0
	for _, p := range pages {
		if p.width > maxWidth {
			maxWidth = p.width
		}
	}
	if maxWidth == 0 {
		maxWidth = pdfcore.DefaultPageWidth
	}
	maxChars := int((maxWidth - 2*svgMargin) / (svgFontSize * 0.5))
	if maxChars < 10 {
		maxChars = 10
	}

	var body strings.Builder
	y := 0.0
	for pi, p := range pages {
		h := p.height
		if h <= 0 {
			h = pdfcore.DefaultPageHeight
		}
		if pi > 0 {
			y += svgPageGap
		}
		fmt.Fprintf(&body, `<rect x="0" y="%.2f" width="%.2f" height="%.2f" fill="#ffffff" stroke="#cccccc"/>`+"\n", y, maxWidth, h)

		ty := y + svgMargin
		for _, para := range p.paragraphs {
			for _, line := range wrapText(para, maxChars) {
				ty += svgLineHeight
				fmt.Fprintf(&body, `<text x="%.2f" y="%.2f" font-family="Helvetica, Arial, sans-serif" font-size="%.0f" fill="#111111">%s</text>`+"\n",
					svgMargin, ty, svgFontSize, xmlEscaper.Replace(line))
			}
			ty += svgParaGap
		}
		y += h
	}

	svg := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="no"?>
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %.2f %.2f" width="%.2f" height="%.2f">
%s</svg>
`, maxWidth, y, maxWidth, y, body.String())
	return []byte(svg), nil
}
