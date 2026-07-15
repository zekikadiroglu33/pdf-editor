// convert.go - best-effort PDF -> other-format conversion, shared pipeline.
//
// PDF has no paragraph model: a content stream only positions individual
// glyphs/strings on a page via Tf (font/size), Td/TD/Tm (move to a new
// text line), TL (line spacing) and T*/'/" (advance a line then show
// text). This reuses the same font-resolution machinery as pdftext.go
// (simple-font byte tables and CID /ToUnicode maps) to decode each
// page's actual Unicode text, but additionally tracks that positioning
// state to recover each line's vertical position, then groups
// consecutive lines into paragraphs the same way pdftotext/PyMuPDF-style
// tools do: a normal line-wrap gap continues the current paragraph, a
// larger gap (or a change of font size) starts a new one. Columns,
// tables and images are not reconstructed - see README limitations.
//
// The extraction above (this file) is shared by every output format; each
// format's own file (convert_docx.go, convert_txt.go, convert_html.go,
// convert_svg.go, convert_xlsx.go, convert_pptx.go) only turns the
// resulting per-page paragraph lists into its own file bytes.
package main

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
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
// invokes - see walkContent in pdftext.go) into an ordered slice of lines.
func extractPageLines(objects []pdfObject, units []formContentUnit) []extractedLine {
	var lines []extractedLine
	for _, u := range units {
		lines = append(lines, extractLinesFromTokens(tokenizeContentStream(objects[u.streamIdx].stream), u.scope.fontMap)...)
	}
	return lines
}

// extractLinesFromTokens walks one content stream's tokens, tracking just
// enough PDF text-positioning state - the text-line matrix's Y translation
// and the current leading (TL) - to know each shown line's vertical
// position, without needing full text/CTM matrix math: real-world PDFs
// essentially never rotate or skew text, so the Y translation component
// alone is a reliable proxy for "how far down the page".
func extractLinesFromTokens(toks []csToken, fontMap map[string]*resolvedFont) []extractedLine {
	var lines []extractedLine
	var curResolved *resolvedFont
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
		resolved      *resolvedFont
	}
	var gsStack []gstate

	decodeStr := func(b []byte) string {
		if curResolved == nil {
			return ""
		}
		var rs []rune
		if curResolved.isCID {
			for k := 0; k+1 < len(b); k += 2 {
				code := uint32(b[k])<<8 | uint32(b[k+1])
				if r, ok := curResolved.cidToRune[code]; ok {
					rs = append(rs, r)
				}
			}
		} else {
			for _, bb := range b {
				if r, ok := curResolved.simpleDecode[bb]; ok {
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
		switch t.kind {
		case 'N':
			lastName = t.s
			nums = nums[:0]
		case 'n':
			if v, err := strconv.ParseFloat(t.s, 64); err == nil {
				nums = append(nums, v)
			}
		case '[':
			depth := 1
			j := i + 1
			var parts []string
			for j < len(toks) && depth > 0 {
				switch toks[j].kind {
				case '[':
					depth++
				case ']':
					depth--
					if depth == 0 {
						goto arrDone
					}
				case 'S':
					parts = append(parts, decodeStr(toks[j].b))
				}
				j++
			}
		arrDone:
			if j+1 < len(toks) && toks[j+1].kind == 'O' && toks[j+1].s == "TJ" {
				flush(strings.Join(parts, ""))
				i = j + 1
			} else {
				i = j
			}
			nums = nums[:0]
		case 'O':
			switch t.s {
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
				if i > 0 && toks[i-1].kind == 'S' {
					flush(decodeStr(toks[i-1].b))
				}
			case "'":
				y -= leading
				if i > 0 && toks[i-1].kind == 'S' {
					flush(decodeStr(toks[i-1].b))
				}
			case "\"":
				y -= leading
				if i > 0 && toks[i-1].kind == 'S' {
					flush(decodeStr(toks[i-1].b))
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
// Page size (/MediaBox, with inheritance) - only used by formats that need
// an actual page canvas (currently svg); text-flow formats ignore it.
// ---------------------------------------------------------------------------

var mediaBoxKeyRe = regexp.MustCompile(`/MediaBox\s*\[([^\]]*)\]`)

const defaultPageWidth, defaultPageHeight = 612.0, 792.0 // US Letter, points

// resolvePageMediaBox finds the /MediaBox that applies to a page (walking
// /Parent for an inherited one, same pattern as resolvePageResources),
// falling back to US Letter if none is found or it doesn't parse cleanly.
func resolvePageMediaBox(objects []pdfObject, objByNum map[int]int, pageIdx int) (width, height float64) {
	curIdx := pageIdx
	for hops := 0; hops < 16; hops++ {
		if m := mediaBoxKeyRe.FindSubmatch(objects[curIdx].body); m != nil {
			fields := strings.Fields(string(m[1]))
			if len(fields) == 4 {
				var nums [4]float64
				ok := true
				for i, f := range fields {
					v, err := strconv.ParseFloat(f, 64)
					if err != nil {
						ok = false
						break
					}
					nums[i] = v
				}
				if ok {
					w, h := math.Abs(nums[2]-nums[0]), math.Abs(nums[3]-nums[1])
					if w > 0 && h > 0 {
						return w, h
					}
				}
			}
		}
		pOwner, _, _, pOk := findDictValue(objects, objByNum, curIdx, 0, len(objects[curIdx].body), parentKeyRe)
		if !pOk {
			break
		}
		curIdx = pOwner
	}
	return defaultPageWidth, defaultPageHeight
}

// wrapText greedily wraps s into lines of at most maxChars characters,
// breaking only at word boundaries. Used by output formats with no native
// text reflow of their own (svg; pptx boxes do reflow themselves, but a
// pre-wrap keeps very long paragraphs from visually overrunning the box).
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
// groupParagraphs) and its page size in points (see resolvePageMediaBox).
type pdfPage struct {
	paragraphs    []string
	width, height float64
}

// extractPDFPages is the shared first half of every conversion: normalize
// with qpdf (so content streams are plain, uncompressed bytes, same as
// EditPDF), walk the page tree, and decode each page's text into
// paragraphs. Every output format builds its file from this same slice.
func extractPDFPages(ctx context.Context, pdf []byte, lang string) ([]pdfPage, error) {
	if _, ok := decodingTables[lang]; !ok {
		return nil, &badInputError{fmt.Sprintf("unsupported language %q", lang)}
	}

	norm, err := normalize(ctx, pdf)
	if err != nil {
		return nil, fmt.Errorf("normalize: %w", err)
	}
	objects, err := parseObjects(norm)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}

	objByNum := make(map[int]int, len(objects))
	for idx, o := range objects {
		objByNum[o.num] = idx
	}

	pageOrder := resolvePageOrder(objects, objByNum)
	if len(pageOrder) == 0 {
		for idx, o := range objects {
			if isPageObject(o.body) {
				pageOrder = append(pageOrder, idx)
			}
		}
	}
	if len(pageOrder) == 0 {
		return nil, fmt.Errorf("no pages found")
	}

	fontCache := map[int]*resolvedFont{}
	pages := make([]pdfPage, len(pageOrder))

	for pn, pageIdx := range pageOrder {
		var rootScope resScope
		if resOwner, rStart, rEnd, rOk := resolvePageResources(objects, objByNum, pageIdx); rOk {
			rootScope = resolveScope(objects, objByNum, fontCache, lang, resOwner, rStart, rEnd)
		}

		var units []formContentUnit
		visiting := map[int]bool{}
		for _, cIdx := range resolvePageContents(objects, objByNum, pageIdx) {
			walkContent(objects, objByNum, fontCache, lang, cIdx, rootScope, 0, visiting, &units)
		}

		lines := extractPageLines(objects, units)
		w, h := resolvePageMediaBox(objects, objByNum, pageIdx)
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
		return nil, 0, "", "", &badInputError{fmt.Sprintf("unsupported format %q", format)}
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
