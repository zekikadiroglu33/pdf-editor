// convert_xlsx.go - minimal .xlsx (OOXML SpreadsheetML) writer.
//
// One worksheet per PDF page ("Page 1", "Page 2", ...), one row per
// paragraph, single column - there's no table/column structure to recover
// from a PDF's text, so this is the same "linear sequence of paragraphs"
// model as the other formats, just laid out as rows instead of prose.
// Cells use inline strings (t="inlineStr") so no shared-strings table is
// needed.
package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
)

func buildXlsxFromPages(pages []pdfPage) ([]byte, error) {
	var contentTypeOverrides strings.Builder
	var workbookSheets strings.Builder
	var workbookRels strings.Builder

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	for i, p := range pages {
		n := i + 1
		fmt.Fprintf(&contentTypeOverrides,
			`<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`,
			n)
		fmt.Fprintf(&workbookSheets, `<sheet name="Page %d" sheetId="%d" r:id="rId%d"/>`, n, n, n)
		fmt.Fprintf(&workbookRels,
			`<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`,
			n, n)

		var rows strings.Builder
		for ri, para := range p.paragraphs {
			row := ri + 1
			fmt.Fprintf(&rows, `<row r="%d"><c r="A%d" t="inlineStr"><is><t xml:space="preserve">%s</t></is></c></row>`,
				row, row, xmlEscaper.Replace(para))
		}
		sheet := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
			`<sheetData>` + rows.String() + `</sheetData></worksheet>`

		w, err := zw.Create(fmt.Sprintf("xl/worksheets/sheet%d.xml", n))
		if err != nil {
			return nil, err
		}
		if _, err := w.Write([]byte(sheet)); err != nil {
			return nil, err
		}
	}

	contentTypes := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
  <Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
  <Default Extension="xml" ContentType="application/xml"/>
  <Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>
  ` + contentTypeOverrides.String() + `
  <Override PartName="/docProps/core.xml" ContentType="application/vnd.openxmlformats-package.core-properties+xml"/>
</Types>`

	rootRels := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>
  <Relationship Id="rId2" Type="http://schemas.openxmlformats.org/package/2006/relationships/metadata/core-properties" Target="docProps/core.xml"/>
</Relationships>`

	workbook := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
  <sheets>` + workbookSheets.String() + `</sheets>
</workbook>`

	workbookRelsXML := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` + workbookRels.String() + `</Relationships>`

	files := []struct{ name, content string }{
		{"[Content_Types].xml", contentTypes},
		{"_rels/.rels", rootRels},
		{"docProps/core.xml", oouxmlCoreProps},
		{"xl/workbook.xml", workbook},
		{"xl/_rels/workbook.xml.rels", workbookRelsXML},
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
