// convert_txt.go - plain-text (.txt) output.
package conversion

import "strings"

// buildTxtFromPages joins each page's paragraphs with a blank line, and
// separates pages with a form-feed (U+000C) - the conventional plain-text
// page-break character most editors and printers already understand.
func buildTxtFromPages(pages []pdfPage) ([]byte, error) {
	var b strings.Builder
	for i, p := range pages {
		if i > 0 {
			b.WriteString("\f\n")
		}
		b.WriteString(strings.Join(p.paragraphs, "\n\n"))
		if len(p.paragraphs) > 0 {
			b.WriteString("\n")
		}
	}
	return []byte(b.String()), nil
}
