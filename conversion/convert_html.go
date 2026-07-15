// convert_html.go - minimal, dependency-free HTML output.
package conversion

import "strings"

const htmlHead = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<title>Converted document</title>
<style>
  body { font-family: Georgia, "Times New Roman", serif; max-width: 720px; margin: 40px auto; padding: 0 16px; line-height: 1.5; color: #1a1a1a; }
  p { margin: 0 0 1em; }
  .page-break { border: none; border-top: 1px dashed #999; margin: 2.5em 0; page-break-after: always; }
</style>
</head>
<body>
`

// buildHTMLFromPages renders each page's paragraphs as <p> tags, with a
// dashed rule (also marked page-break-after for print/PDF-from-browser)
// between pages.
func buildHTMLFromPages(pages []pdfPage) ([]byte, error) {
	var b strings.Builder
	b.WriteString(htmlHead)
	for i, p := range pages {
		if i > 0 {
			b.WriteString("<hr class=\"page-break\">\n")
		}
		for _, para := range p.paragraphs {
			b.WriteString("<p>")
			b.WriteString(xmlEscaper.Replace(para))
			b.WriteString("</p>\n")
		}
	}
	b.WriteString("</body>\n</html>\n")
	return []byte(b.String()), nil
}
