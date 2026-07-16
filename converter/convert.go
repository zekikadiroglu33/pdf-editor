// Package conversion is the best-effort PDF -> other-format conversion
// pipeline.
//
// PDF has no paragraph model: a content stream only positions individual
// glyphs/strings on a page via Tf (font/size), Td/TD/Tm (move to a new
// text line), TL (line spacing) and T*/'/" (advance a line then show
// text). This reuses pdfcore's font-resolution machinery (simple-font
// byte tables and CID /ToUnicode maps) to decode each page's actual
// Unicode text, but additionally tracks that positioning state to
// recover each line's vertical position, then groups consecutive lines
// into paragraphs the same way pdftotext/PyMuPDF-style tools do: a
// normal line-wrap gap continues the current paragraph, a larger gap (or
// a change of font size) starts a new one. Columns, tables and images
// are not reconstructed - see README limitations.
//
// The extraction above (this file) is shared by every output format;
// each format's own file (convert_docx.go, convert_txt.go,
// convert_html.go, convert_svg.go, convert_xlsx.go, convert_pptx.go)
// only turns the resulting per-page paragraph lists into its own file
// bytes.
package converter

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"pdfer/pdfcore"
)

// ---------------------------------------------------------------------------
// Content-stream -> positioned lines
// ---------------------------------------------------------------------------

// extractedLine is one text-showing operation (a Tj/'/" call, or one TJ
// array) decoded to Unicode, along with the font size and text-line-matrix
// Y translation active when it was shown.
type extractedLine struct {
	y, fontSize float64
	text        string
}

// extractPageLines decodes every text-bearing content-stream unit
// belonging to one page (its own /Contents, plus any Form XObjects it
// invokes - see pdfcore.WalkContent) into an ordered slice of lines.
func extractPageLines(objects []pdfcore.PdfObject, units []pdfcore.FormContentUnit) []extractedLine {
	var lines []extractedLine
	for _, u := range units {
		lines = append(lines, extractLinesFromTokens(pdfcore.TokenizeContentStream(objects[u.StreamIdx].Stream), u.Scope.FontMap)...)
	}
	return lines
}

// extractLinesFromTokens walks one content stream's tokens, tracking just
// enough PDF text-positioning state - the text-line matrix's Y translation
// and the current leading (TL) - to know each shown line's vertical
// position, without needing full text/CTM matrix math: real-world PDFs
// essentially never rotate or skew text, so the Y translation component
// alone is a reliable proxy for "how far down the page".
func extractLinesFromTokens(toks []pdfcore.CsToken, fontMap map[string]*pdfcore.ResolvedFont) []extractedLine {
	var lines []extractedLine
	var curResolved *pdfcore.ResolvedFont
	var curSize float64
	var lastName string
	var nums []float64
	var y, leading float64

	// Tc/Tw/Tz/Tf/TL/Tr/Ts are text *state*, part of the graphics state
	// that q/Q save and restore - but BT resets the text line matrix (our
	// y) to identity regardless of q/Q, so y itself must not be pushed
	// here (see the "BT" case below).
	type gstate struct {
		size, leading float64
		resolved      *pdfcore.ResolvedFont
	}
	var gsStack []gstate

	decodeStr := func(b []byte) string {
		if curResolved == nil {
			return ""
		}
		var rs []rune
		if curResolved.IsCID {
			for k := 0; k+1 < len(b); k += 2 {
				code := uint32(b[k])<<8 | uint32(b[k+1])
				if r, ok := curResolved.CidToRune[code]; ok {
					rs = append(rs, r)
				}
			}
		} else {
			for _, bb := range b {
				if r, ok := curResolved.SimpleDecode[bb]; ok {
					rs = append(rs, r)
				}
			}
		}
		return string(rs)
	}

	flush := func(text string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		lines = append(lines, extractedLine{y: y, fontSize: curSize, text: text})
	}

	i := 0
	for i < len(toks) {
		t := toks[i]
		switch t.Kind {
		case 'N':
			lastName = t.S
			nums = nums[:0]
		case 'n':
			if v, err := strconv.ParseFloat(t.S, 64); err == nil {
				nums = append(nums, v)
			}
		case '[':
			depth := 1
			j := i + 1
			var parts []string
			for j < len(toks) && depth > 0 {
				switch toks[j].Kind {
				case '[':
					depth++
				case ']':
					depth--
					if depth == 0 {
						goto arrDone
					}
				case 'S':
					parts = append(parts, decodeStr(toks[j].B))
				}
				j++
			}
		arrDone:
			if j+1 < len(toks) && toks[j+1].Kind == 'O' && toks[j+1].S == "TJ" {
				flush(strings.Join(parts, ""))
				i = j + 1
			} else {
				i = j
			}
			nums = nums[:0]
		case 'O':
			switch t.S {
			case "BT":
				// BT always resets the text line matrix to identity,
				// regardless of whatever q/Q or prior text objects did.
				y = 0
			case "q":
				gsStack = append(gsStack, gstate{curSize, leading, curResolved})
			case "Q":
				if len(gsStack) > 0 {
					top := gsStack[len(gsStack)-1]
					gsStack = gsStack[:len(gsStack)-1]
					curSize, leading, curResolved = top.size, top.leading, top.resolved
				}
			case "Tf":
				curResolved = fontMap[lastName]
				if len(nums) > 0 {
					curSize = nums[len(nums)-1]
				}
			case "Tm":
				if len(nums) == 6 {
					y = nums[5]
				}
			case "Td":
				if len(nums) == 2 {
					y += nums[1]
				}
			case "TD":
				if len(nums) == 2 {
					y += nums[1]
					leading = -nums[1]
				}
			case "TL":
				if len(nums) == 1 {
					leading = nums[0]
				}
			case "T*":
				y -= leading
			case "Tj":
				if i > 0 && toks[i-1].Kind == 'S' {
					flush(decodeStr(toks[i-1].B))
				}
			case "'":
				y -= leading
				if i > 0 && toks[i-1].Kind == 'S' {
					flush(decodeStr(toks[i-1].B))
				}
			case "\"":
				y -= leading
				if i > 0 && toks[i-1].Kind == 'S' {
					flush(decodeStr(toks[i-1].B))
				}
			}
			nums = nums[:0]
			lastName = ""
		}
		i++
	}
	return lines
}

// ---------------------------------------------------------------------------
// Lines -> paragraphs
// ---------------------------------------------------------------------------

// groupParagraphs joins consecutive lines into paragraphs: a normal
// single-line-wrap gap (roughly one line height, moving down the page)
// continues the current paragraph; a same-line gap (an inline font-size
// change midway through the same line, e.g. bold/italic run) concatenates
// with no separator; anything else (a bigger vertical jump, moving back
// up, or an unrelated font-size change) starts a new paragraph.
func groupParagraphs(lines []extractedLine) []string {
	var paras []string
	var cur strings.Builder
	for i, ln := range lines {
		if i == 0 {
			cur.WriteString(ln.text)
			continue
		}
		prev := lines[i-1]
		gap := prev.y - ln.y // positive = moved down the page, as expected
		refSize := prev.fontSize
		if ln.fontSize > refSize {
			refSize = ln.fontSize
		}
		if refSize <= 0 {
			refSize = 1
		}
		switch {
		case gap > -0.3*refSize && gap < 0.3*refSize:
			// Same line (e.g. a styled run splitting one Tj call in two).
			cur.WriteString(ln.text)
		case gap >= 0.3*refSize && gap <= 1.45*refSize:
			// Ordinary line wrap within the same paragraph.
			cur.WriteString(" ")
			cur.WriteString(ln.text)
		default:
			if s := strings.TrimSpace(cur.String()); s != "" {
				paras = append(paras, s)
			}
			cur.Reset()
			cur.WriteString(ln.text)
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		paras = append(paras, s)
	}
	return paras
}

// ---------------------------------------------------------------------------
// Shared XML escaping (reused by the OOXML and markup output formats)
// ---------------------------------------------------------------------------

var xmlEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
)

// oouxmlCoreProps is the docProps/core.xml part shared by every OOXML
// output format (docx/xlsx/pptx all use the same package-level metadata).
const oouxmlCoreProps = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/">
  <dc:creator>pdfer</dc:creator>
  <dc:title>Converted document</dc:title>
</cp:coreProperties>`

// ---------------------------------------------------------------------------
// Pipeline: PDF bytes -> per-page paragraphs (+ page size)
// ---------------------------------------------------------------------------

// pdfPage is one PDF page's extracted content: its paragraphs (see
// groupParagraphs) and its page size in points (see
// pdfcore.ResolvePageMediaBox).
type pdfPage struct {
	paragraphs    []string
	width, height float64
}

// extractPDFPages is the shared first half of every conversion: normalize
// with qpdf (so content streams are plain, uncompressed bytes, same as
// editor.EditPDF), walk the page tree, and decode each page's text into
// paragraphs. Every output format builds its file from this same slice.
func extractPDFPages(ctx context.Context, pdf []byte, lang string) ([]pdfPage, error) {
	if _, ok := pdfcore.DecodingTables[lang]; !ok {
		return nil, &pdfcore.BadInputError{Msg: fmt.Sprintf("unsupported language %q", lang)}
	}

	norm, err := pdfcore.Normalize(ctx, pdf)
	if err != nil {
		return nil, fmt.Errorf("normalize: %w", err)
	}
	objects, err := pdfcore.ParseObjects(norm)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}

	objByNum := make(map[int]int, len(objects))
	for idx, o := range objects {
		objByNum[o.Num] = idx
	}

	pageOrder := pdfcore.ResolvePageOrder(objects, objByNum)
	if len(pageOrder) == 0 {
		for idx, o := range objects {
			if pdfcore.IsPageObject(o.Body) {
				pageOrder = append(pageOrder, idx)
			}
		}
	}
	if len(pageOrder) == 0 {
		return nil, fmt.Errorf("no pages found")
	}

	fontCache := map[int]*pdfcore.ResolvedFont{}
	pages := make([]pdfPage, len(pageOrder))

	for pn, pageIdx := range pageOrder {
		var rootScope pdfcore.ResScope
		if resOwner, rStart, rEnd, rOk := pdfcore.ResolvePageResources(objects, objByNum, pageIdx); rOk {
			rootScope = pdfcore.ResolveScope(objects, objByNum, fontCache, lang, resOwner, rStart, rEnd)
		}

		var units []pdfcore.FormContentUnit
		visiting := map[int]bool{}
		for _, cIdx := range pdfcore.ResolvePageContents(objects, objByNum, pageIdx) {
			pdfcore.WalkContent(objects, objByNum, fontCache, lang, cIdx, rootScope, 0, visiting, &units)
		}

		lines := extractPageLines(objects, units)
		w, h := pdfcore.ResolvePageMediaBox(objects, objByNum, pageIdx)
		pages[pn] = pdfPage{paragraphs: groupParagraphs(lines), width: w, height: h}
	}

	return pages, nil
}

// ---------------------------------------------------------------------------
// Format dispatch
// ---------------------------------------------------------------------------

// convertFormat describes one downloadable output format: its file
// extension, HTTP content type, and the function that turns extracted
// pages into that format's file bytes.
type convertFormat struct {
	ext, contentType string
	build            func(pages []pdfPage) ([]byte, error)
}

var convertFormats = map[string]convertFormat{
	"docx": {"docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", buildDocxFromPages},
	"txt":  {"txt", "text/plain; charset=utf-8", buildTxtFromPages},
	"html": {"html", "text/html; charset=utf-8", buildHTMLFromPages},
	"svg":  {"svg", "image/svg+xml", buildSVGFromPages},
	"xlsx": {"xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", buildXlsxFromPages},
	"pptx": {"pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation", buildPptxFromPages},
}

// rasterFormats are handled by a completely different pipeline
// (convertPDFToRaster, in convert_raster.go): they rasterize the
// original PDF bytes directly via an external renderer instead of
// extracting text, and their content type/filename depend on whether the
// PDF had one page (a single image) or several (a zip of images) - so
// they're dispatched before, instead of through, convertFormats/extractPDFPages.
var rasterFormats = map[string]bool{"png": true, "jpeg": true, "tiff": true, "webp": true}

// ConvertPDF is the full PDF -> <format> pipeline. Text-based formats
// extract pages (shared) and hand them to the requested format's
// builder; raster formats rasterize the PDF directly (see rasterFormats
// above). pageCount is the number of PDF pages processed.
func ConvertPDF(ctx context.Context, pdf []byte, lang, format string) (data []byte, pageCount int, contentType, filename string, err error) {
	if rasterFormats[format] {
		return convertPDFToRaster(ctx, pdf, format)
	}

	spec, ok := convertFormats[format]
	if !ok {
		return nil, 0, "", "", &pdfcore.BadInputError{Msg: fmt.Sprintf("unsupported format %q", format)}
	}

	pages, err := extractPDFPages(ctx, pdf, lang)
	if err != nil {
		return nil, 0, "", "", err
	}

	data, err = spec.build(pages)
	if err != nil {
		return nil, 0, "", "", fmt.Errorf("build %s: %w", format, err)
	}
	return data, len(pages), spec.contentType, "converted." + spec.ext, nil
}
