// pdfcrop.go - crops PDF pages by setting their /CropBox, the PDF-native
// way to change a page's visible area without touching its content
// stream at all (viewers simply clip to it) - by far the simplest of the
// content-stream-adjacent tools, since there's no drawing involved, just
// one dictionary entry per target page.
package pdftools

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"pdfer/pdfcore"
)

var cropBoxKeyRe = regexp.MustCompile(`/CropBox\s*\[[^\]]*\]`)

// resolvePageBoxCorners finds the actual [x0 y0 x1 y1] /MediaBox corners
// that apply to a page (walking /Parent for an inherited one, the same
// pattern as pdfcore.ResolvePageMediaBox) - unlike that function, which
// only needs width/height for its callers, cropping (and add-image's
// placement math) needs the true origin too, since a MediaBox doesn't
// have to start at [0,0].
func resolvePageBoxCorners(objects []pdfcore.PdfObject, objByNum map[int]int, pageIdx int) (x0, y0, x1, y1 float64) {
	curIdx := pageIdx
	for hops := 0; hops < 16; hops++ {
		if m := pdfcore.MediaBoxKeyRe.FindSubmatch(objects[curIdx].Body); m != nil {
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
					lx, ly, ux, uy := nums[0], nums[1], nums[2], nums[3]
					if ux < lx {
						lx, ux = ux, lx
					}
					if uy < ly {
						ly, uy = uy, ly
					}
					if ux-lx > 0 && uy-ly > 0 {
						return lx, ly, ux, uy
					}
				}
			}
		}
		pOwner, _, _, pOk := pdfcore.FindDictValue(objects, objByNum, curIdx, 0, len(objects[curIdx].Body), pdfcore.ParentKeyRe)
		if !pOk {
			break
		}
		curIdx = pOwner
	}
	return 0, 0, pdfcore.DefaultPageWidth, pdfcore.DefaultPageHeight
}

// setPageCropBox replaces (or adds) /CropBox directly on the page's own
// dictionary - not wherever /MediaBox happened to be inherited from -
// since an explicit /CropBox on the page itself always takes precedence
// over any inherited value, per the PDF spec.
func setPageCropBox(page *pdfcore.PdfObject, x0, y0, x1, y1 float64) {
	entry := fmt.Sprintf("/CropBox [%s %s %s %s]",
		strconv.FormatFloat(x0, 'f', 2, 64), strconv.FormatFloat(y0, 'f', 2, 64),
		strconv.FormatFloat(x1, 'f', 2, 64), strconv.FormatFloat(y1, 'f', 2, 64))
	if loc := cropBoxKeyRe.FindIndex(page.Body); loc != nil {
		var buf []byte
		buf = append(buf, page.Body[:loc[0]]...)
		buf = append(buf, entry...)
		buf = append(buf, page.Body[loc[1]:]...)
		page.Body = buf
		return
	}
	insertRawBeforeEnd(page, pdfcore.DictEnd(page.Body, 0), entry)
}

// CropPDF sets each target page's (see parsePageSpec) /CropBox to a
// sub-rectangle of its current MediaBox, trimmed by the given margins as
// percentages of that page's own width/height - e.g. marginTopPct=10
// trims the top 10% of the page. Each margin must leave most of the page
// visible (0-49%, and opposite margins summing to under 100%).
func CropPDF(ctx context.Context, pdf []byte, marginLeftPct, marginRightPct, marginTopPct, marginBottomPct float64, pagesSpec string) ([]byte, error) {
	for _, m := range []float64{marginLeftPct, marginRightPct, marginTopPct, marginBottomPct} {
		if m < 0 || m >= 50 {
			return nil, &pdfcore.BadInputError{Msg: "crop margins must each be between 0 and 49 percent"}
		}
	}
	if marginLeftPct+marginRightPct >= 100 || marginTopPct+marginBottomPct >= 100 {
		return nil, &pdfcore.BadInputError{Msg: "combined left+right (and top+bottom) crop margins must total under 100 percent"}
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

	targets, err := parsePageSpec(pagesSpec, len(pageOrder))
	if err != nil {
		return nil, err
	}

	for _, pn := range targets {
		pageIdx := pageOrder[pn]
		x0, y0, x1, y1 := resolvePageBoxCorners(objects, objByNum, pageIdx)
		w, h := x1-x0, y1-y0
		setPageCropBox(&objects[pageIdx],
			x0+w*marginLeftPct/100, y0+h*marginBottomPct/100,
			x1-w*marginRightPct/100, y1-h*marginTopPct/100)
	}

	rebuilt, err := pdfcore.Rebuild(objects, norm)
	if err != nil {
		return nil, fmt.Errorf("rebuild: %w", err)
	}
	return pdfcore.Recompress(ctx, rebuilt), nil
}
