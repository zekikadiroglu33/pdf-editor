// pdfwatermark.go - adds a diagonal, semi-transparent text watermark to
// PDF pages by directly editing the PDF object model (parse -> mutate ->
// rebuild -> recompress) via pdfcore, the same technique the editor
// package's find/replace engine uses - unlike the qpdf-native tools in
// pdftools.go, there's no CLI flag for "draw text on a page", so this
// works at the content-stream level instead.
package pdftools

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"

	"pdfer/pdfcore"
)

// extGStateKeyRe locates a /ExtGState key the same way pdfcore.FontKeyRe
// locates /Font - reused by ensureIndirectResource below.
var extGStateKeyRe = regexp.MustCompile(`/ExtGState\b`)

// insertRawBeforeEnd splices " "+raw right before the ">>" that closes
// the dict spanning [start,end) in obj.Body. Generic version of
// pdfcore.InsertFontEntry (which only ever splices a fixed
// "/Name N 0 R" shape) - this one takes arbitrary dict-entry text, since
// watermarking sometimes needs to add a whole new sub-dictionary
// ("/Font << /Name N 0 R >>"), not just an entry inside one that already
// exists.
func insertRawBeforeEnd(obj *pdfcore.PdfObject, end int, raw string) {
	pos := end - 2 // start of the dict's closing ">>"
	newBody := make([]byte, 0, len(obj.Body)+1+len(raw))
	newBody = append(newBody, obj.Body[:pos]...)
	newBody = append(newBody, ' ')
	newBody = append(newBody, raw...)
	newBody = append(newBody, obj.Body[pos:]...)
	obj.Body = newBody
}

// ensureIndirectResource makes sure the resources dict at
// objects[resOwner].Body[rStart:rEnd) has a dictKeyName sub-dictionary
// (e.g. "Font", "ExtGState") containing a "/resourceName resObjNum 0 R"
// entry - creating the sub-dictionary if the resources dict doesn't have
// one yet, and doing nothing if the entry is already there (so a
// Resources dict shared by several pages, which is common, only gets the
// entry added once no matter how many target pages reuse it).
func ensureIndirectResource(objects []pdfcore.PdfObject, objByNum map[int]int, resOwner, rStart, rEnd int, dictKeyRe *regexp.Regexp, dictKeyName, resourceName string, resObjNum int) {
	entryMarker := []byte("/" + resourceName + " ")
	if fo, fs, fe, ok := pdfcore.FindDictValue(objects, objByNum, resOwner, rStart, rEnd, dictKeyRe); ok {
		if bytes.Contains(objects[fo].Body[fs:fe], entryMarker) {
			return
		}
		insertRawBeforeEnd(&objects[fo], fe, fmt.Sprintf("/%s %d 0 R", resourceName, resObjNum))
		return
	}
	insertRawBeforeEnd(&objects[resOwner], rEnd, fmt.Sprintf("/%s << /%s %d 0 R >>", dictKeyName, resourceName, resObjNum))
}

// appendPageContentStream appends a reference to a new content-stream
// object (newStreamNum) to a page's own /Contents entry, converting a
// single indirect reference into a two-element array if needed. /Contents
// is never inherited from a parent /Pages node (unlike /Resources), so
// this only ever needs to touch the page object's own body.
func appendPageContentStream(page *pdfcore.PdfObject, newStreamNum int) error {
	if loc := pdfcore.ContentsArrRe.FindSubmatchIndex(page.Body); loc != nil {
		insertion := []byte(fmt.Sprintf(" %d 0 R", newStreamNum))
		var buf []byte
		buf = append(buf, page.Body[:loc[3]]...)
		buf = append(buf, insertion...)
		buf = append(buf, page.Body[loc[3]:]...)
		page.Body = buf
		return nil
	}
	if loc := pdfcore.ContentsRefRe.FindSubmatchIndex(page.Body); loc != nil {
		refPart := bytes.TrimSpace(bytes.TrimPrefix(page.Body[loc[0]:loc[1]], []byte("/Contents")))
		replacement := []byte(fmt.Sprintf("/Contents [%s %d 0 R]", refPart, newStreamNum))
		var buf []byte
		buf = append(buf, page.Body[:loc[0]]...)
		buf = append(buf, replacement...)
		buf = append(buf, page.Body[loc[1]:]...)
		page.Body = buf
		return nil
	}
	return fmt.Errorf("page has no /Contents")
}

const watermarkAngleDegrees = 45.0 // fixed diagonal, bottom-left to top-right - not user-configurable yet

// WatermarkPDF stamps text diagonally across the given pages (see
// parsePageSpec; "" means every page) of pdf, semi-transparent
// (opacity in (0,1]) at fontSize points, using the same bundled
// Latin+Turkish fallback font the editor uses for replacement text it
// can't render through the original font (see pdfcore.BuildFallbackFontObjects)
// - a watermark string has no "original font" to match at all, so this
// is the one font every watermark request can always rely on.
func WatermarkPDF(ctx context.Context, pdf []byte, text string, opacity, fontSize float64, pagesSpec string) ([]byte, error) {
	if text == "" {
		return nil, &pdfcore.BadInputError{Msg: "watermark text is required"}
	}
	if opacity <= 0 || opacity > 1 {
		return nil, &pdfcore.BadInputError{Msg: "opacity must be greater than 0 and at most 1"}
	}
	if fontSize <= 0 || fontSize > 500 {
		return nil, &pdfcore.BadInputError{Msg: "font size must be between 1 and 500"}
	}
	textBytes, ok := pdfcore.EncodeRaw(text, pdfcore.EncodingTables["turkish"])
	if !ok {
		return nil, &pdfcore.BadInputError{Msg: "watermark text contains a character not supported by the bundled fallback font"}
	}
	escapedText, _ := pdfcore.EncodeAndEscape(text, pdfcore.EncodingTables["turkish"])

	norm, err := pdfcore.Normalize(ctx, pdf)
	if err != nil {
		return nil, fmt.Errorf("normalize: %w", err)
	}
	objects, err := pdfcore.ParseObjects(norm)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	objByNum := make(map[int]int, len(objects))
	maxNum := 0
	for idx, o := range objects {
		objByNum[o.Num] = idx
		if o.Num > maxNum {
			maxNum = o.Num
		}
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

	targets, err := parsePageSpec(pagesSpec, len(pageOrder))
	if err != nil {
		return nil, err
	}

	// Text width in glyph-space (1000 units/em), converted to points, so
	// the watermark can be centered on the page regardless of string
	// length.
	widthUnits := 0
	for _, b := range textBytes {
		widthUnits += pdfcore.FallbackFontWidths[b]
	}
	textWidthPts := float64(widthUnits) / 1000 * fontSize

	fontObjs, fontObjNum := pdfcore.BuildFallbackFontObjects(maxNum + 1)
	objects = append(objects, fontObjs...)
	extGStateNum := maxNum + len(fontObjs) + 1
	objects = append(objects, pdfcore.PdfObject{
		Num: extGStateNum,
		Body: []byte(fmt.Sprintf("<< /Type /ExtGState /ca %s /CA %s >>",
			strconv.FormatFloat(opacity, 'f', -1, 64), strconv.FormatFloat(opacity, 'f', -1, 64))),
	})
	nextNum := extGStateNum + 1
	// Named after this call's own ExtGState object number, not a fixed
	// name: a fixed name would collide if a PDF that this tool already
	// watermarked once gets watermarked again (e.g. re-uploading its own
	// output with a different opacity) - ensureIndirectResource's "skip
	// if this name is already present" check (needed so a Resources dict
	// shared across pages doesn't get the entry inserted twice) would
	// then leave the first pass's ExtGState mapped, silently applying
	// its stale opacity to the second pass's text instead of the new one.
	extGStateResourceName := fmt.Sprintf("PDFerWM%d", extGStateNum)

	theta := watermarkAngleDegrees * math.Pi / 180
	cos, sin := math.Cos(theta), math.Sin(theta)
	halfWidth := textWidthPts / 2

	for _, pn := range targets {
		pageIdx := pageOrder[pn]

		resOwner, rStart, rEnd, ok := pdfcore.ResolvePageResources(objects, objByNum, pageIdx)
		if !ok {
			return nil, fmt.Errorf("page %d has no /Resources to attach the watermark font to", pn+1)
		}
		ensureIndirectResource(objects, objByNum, resOwner, rStart, rEnd, pdfcore.FontKeyRe, "Font", pdfcore.FallbackFontResourceName, fontObjNum)

		resOwner, rStart, rEnd, _ = pdfcore.ResolvePageResources(objects, objByNum, pageIdx) // re-fetch: the call above may have grown resOwner's body
		ensureIndirectResource(objects, objByNum, resOwner, rStart, rEnd, extGStateKeyRe, "ExtGState", extGStateResourceName, extGStateNum)

		w, h := pdfcore.ResolvePageMediaBox(objects, objByNum, pageIdx)
		centerX, centerY := w/2, h/2
		originX := centerX - halfWidth*cos
		originY := centerY - halfWidth*sin

		content := fmt.Sprintf(
			"q\n/%s gs\nBT\n/%s %s Tf\n0.5 0.5 0.5 rg\n%s %s %s %s %s %s Tm\n(%s) Tj\nET\nQ\n",
			extGStateResourceName, pdfcore.FallbackFontResourceName,
			strconv.FormatFloat(fontSize, 'f', -1, 64),
			strconv.FormatFloat(cos, 'f', 6, 64), strconv.FormatFloat(sin, 'f', 6, 64),
			strconv.FormatFloat(-sin, 'f', 6, 64), strconv.FormatFloat(cos, 'f', 6, 64),
			strconv.FormatFloat(originX, 'f', 3, 64), strconv.FormatFloat(originY, 'f', 3, 64),
			escapedText,
		)
		streamNum := nextNum
		nextNum++
		objects = append(objects, pdfcore.PdfObject{
			Num:       streamNum,
			Body:      []byte(fmt.Sprintf("<< /Length %d >>", len(content))),
			Stream:    []byte(content),
			HasStream: true,
		})
		if err := appendPageContentStream(&objects[pageIdx], streamNum); err != nil {
			return nil, fmt.Errorf("page %d: %w", pn+1, err)
		}
	}

	rebuilt, err := pdfcore.Rebuild(objects, norm)
	if err != nil {
		return nil, fmt.Errorf("rebuild: %w", err)
	}
	return pdfcore.Recompress(ctx, rebuilt), nil
}
