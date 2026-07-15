// convert_docx.go - minimal .docx (OOXML WordprocessingML) writer.
package conversion

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
)

const docxContentTypes = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
  <Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
  <Default Extension="xml" ContentType="application/xml"/>
  <Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>
  <Override PartName="/docProps/core.xml" ContentType="application/vnd.openxmlformats-package.core-properties+xml"/>
</Types>`

const docxRootRels = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>
  <Relationship Id="rId2" Type="http://schemas.openxmlformats.org/package/2006/relationships/metadata/core-properties" Target="docProps/core.xml"/>
</Relationships>`

// buildDocxParagraphXML renders one paragraph as a single run. A blank
// (zero-length) paragraph is still emitted so an empty PDF page still
// produces something in the output, rather than silently vanishing.
func buildDocxParagraphXML(text string) string {
	if text == "" {
		return `<w:p/>`
	}
	return fmt.Sprintf(`<w:p><w:r><w:t xml:space="preserve">%s</w:t></w:r></w:p>`, xmlEscaper.Replace(text))
}

const docxPageBreakXML = `<w:p><w:r><w:br w:type="page"/></w:r></w:p>`

// buildDocx assembles a minimal, valid .docx from one paragraph list per
// PDF page, inserting an explicit page break between pages.
func buildDocx(pages [][]string) ([]byte, error) {
	var body strings.Builder
	for pi, paras := range pages {
		if pi > 0 {
			body.WriteString(docxPageBreakXML)
		}
		if len(paras) == 0 {
			body.WriteString(`<w:p/>`)
			continue
		}
		for _, p := range paras {
			body.WriteString(buildDocxParagraphXML(p))
		}
	}
	document := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		`<w:body>` + body.String() + `<w:sectPr/></w:body></w:document>`

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	files := []struct{ name, content string }{
		{"[Content_Types].xml", docxContentTypes},
		{"_rels/.rels", docxRootRels},
		{"docProps/core.xml", oouxmlCoreProps},
		{"word/document.xml", document},
	}
	for _, f := range files {
		w, err := zw.Create(f.name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write([]byte(f.content)); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// buildDocxFromPages adapts buildDocx to the convertFormat.build signature.
func buildDocxFromPages(pages []pdfPage) ([]byte, error) {
	paras := make([][]string, len(pages))
	for i, p := range pages {
		paras[i] = p.paragraphs
	}
	return buildDocx(paras)
}
