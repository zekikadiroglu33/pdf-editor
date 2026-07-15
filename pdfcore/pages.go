// Page tree / Resources / MediaBox resolution - shared by every feature
// that needs to know a PDF's pages, their resources, or their geometry
// (editor, conversion, pdfTools all depend on this file).
package pdfcore

import (
	"math"
	"regexp"
	"strconv"
	"strings"
)

var (
	pageTypeRe     = regexp.MustCompile(`/Type\s*/Page\b`)
	resourcesKeyRe = regexp.MustCompile(`/Resources\b`)
	// FontKeyRe locates a /Font key within a Resources dict - exported
	// since pdfTools' watermark tool needs it directly to add its own
	// font resource entry.
	FontKeyRe     = regexp.MustCompile(`/Font\b`)
	indirectRefRe = regexp.MustCompile(`^(\d+)\s+\d+\s+R`)
	// ParentKeyRe locates a page/Pages node's /Parent key - exported
	// since pdfTools' crop tool walks it directly to resolve an inherited
	// /MediaBox.
	ParentKeyRe = regexp.MustCompile(`/Parent\b`)
	// ContentsArrRe/ContentsRefRe locate a page's /Contents entry (array
	// or single indirect reference form) - exported since pdfTools'
	// watermark/add-image tools append a new content stream to it directly.
	ContentsArrRe = regexp.MustCompile(`/Contents\s*\[(.*?)\]`)
	ContentsRefRe = regexp.MustCompile(`/Contents\s+(\d+)\s+\d+\s+R`)
	refInArrRe    = regexp.MustCompile(`(\d+)\s+\d+\s+R`)
	typeCatalogRe = regexp.MustCompile(`/Type\s*/Catalog\b`)
	pagesKeyRe    = regexp.MustCompile(`/Pages\b`)
	kidsArrRe     = regexp.MustCompile(`(?s)/Kids\s*\[(.*?)\]`)
	// XObjectKeyRe locates a /XObject key within a Resources dict -
	// exported since pdfTools' add-image tool needs it directly to add
	// its own XObject resource entry.
	XObjectKeyRe  = regexp.MustCompile(`/XObject\b`)
	subtypeFormRe = regexp.MustCompile(`/Subtype\s*/Form\b`)
	// MediaBoxKeyRe locates a /MediaBox entry - exported since pdfTools'
	// crop tool parses it directly (to get the true box origin, not just
	// width/height - see ResolvePageMediaBox below for the width/height-only
	// version).
	MediaBoxKeyRe = regexp.MustCompile(`/MediaBox\s*\[([^\]]*)\]`)
)

// DefaultPageWidth/DefaultPageHeight (US Letter, points) are the fallback
// page size when a /MediaBox can't be found or doesn't parse cleanly.
const DefaultPageWidth, DefaultPageHeight = 612.0, 792.0

func IsPageObject(body []byte) bool { return pageTypeRe.Match(body) }

// pageTreeKids returns the object numbers listed in a /Pages node's /Kids
// array, in document order.
func pageTreeKids(body []byte) []int {
	m := kidsArrRe.FindSubmatch(body)
	if m == nil {
		return nil
	}
	var nums []int
	for _, ref := range refInArrRe.FindAllSubmatch(m[1], -1) {
		n, _ := strconv.Atoi(string(ref[1]))
		nums = append(nums, n)
	}
	return nums
}

// ResolvePageOrder walks the document's page tree (/Root -> /Pages ->
// /Kids, recursively) to produce the logical, front-to-back page order -
// the same order a PDF viewer (and pdf.js in the browser UI) numbers
// pages in. This can differ from the physical order objects appear in
// the file, which is why occurrence targeting (see editor.EditPDF) can't
// just use object order to figure out which page the browser called
// "page N".
func ResolvePageOrder(objects []PdfObject, objByNum map[int]int) []int {
	rootIdx := -1
	for i, o := range objects {
		if typeCatalogRe.Match(o.Body) {
			rootIdx = i
			break
		}
	}
	if rootIdx < 0 {
		return nil
	}
	pagesIdx, _, _, ok := FindDictValue(objects, objByNum, rootIdx, 0, len(objects[rootIdx].Body), pagesKeyRe)
	if !ok {
		return nil
	}

	var order []int
	seen := map[int]bool{}
	var walk func(idx, depth int)
	walk = func(idx, depth int) {
		if depth > 64 || idx < 0 || idx >= len(objects) || seen[idx] {
			return
		}
		seen[idx] = true
		body := objects[idx].Body
		if IsPageObject(body) {
			order = append(order, idx)
			return
		}
		for _, kidNum := range pageTreeKids(body) {
			if kidIdx, ok := objByNum[kidNum]; ok {
				walk(kidIdx, depth+1)
			}
		}
	}
	walk(pagesIdx, 0)
	return order
}

// FindDictValue locates the value of `/key`-matching keyRe within the
// window objects[ownerIdx].Body[searchFrom:searchTo]. If the value is an
// inline dict "<< ... >>", it returns the same owner with the dict's
// *absolute* byte range within that owner's full body. If the value is
// an indirect reference, it returns the referenced object as the new
// owner, spanning that object's entire body.
func FindDictValue(objects []PdfObject, objByNum map[int]int, ownerIdx, searchFrom, searchTo int, keyRe *regexp.Regexp) (newOwnerIdx, start, end int, ok bool) {
	body := objects[ownerIdx].Body
	loc := keyRe.FindIndex(body[searchFrom:searchTo])
	if loc == nil {
		return 0, 0, 0, false
	}
	i := SkipWS(body, searchFrom+loc[1])
	if i+1 < len(body) && body[i] == '<' && body[i+1] == '<' {
		e := DictEnd(body, i)
		if e < 0 {
			return 0, 0, 0, false
		}
		return ownerIdx, i, e, true
	}
	limit := i + 40
	if limit > len(body) {
		limit = len(body)
	}
	m := indirectRefRe.FindSubmatch(body[i:limit])
	if m == nil {
		return 0, 0, 0, false
	}
	num, _ := strconv.Atoi(string(m[1]))
	idx, ok2 := objByNum[num]
	if !ok2 {
		return 0, 0, 0, false
	}
	return idx, 0, len(objects[idx].Body), true
}

// ResolvePageResources finds the /Resources dict that applies to a page
// (walking /Parent for inherited /Resources), returning its owning
// object index and its byte range within that object's body.
func ResolvePageResources(objects []PdfObject, objByNum map[int]int, pageIdx int) (ownerIdx, start, end int, ok bool) {
	curIdx := pageIdx
	for hops := 0; hops < 16; hops++ {
		if resOwner, rStart, rEnd, rOk := FindDictValue(objects, objByNum, curIdx, 0, len(objects[curIdx].Body), resourcesKeyRe); rOk {
			return resOwner, rStart, rEnd, true
		}
		pOwner, _, _, pOk := FindDictValue(objects, objByNum, curIdx, 0, len(objects[curIdx].Body), ParentKeyRe)
		if !pOk {
			return 0, 0, 0, false
		}
		curIdx = pOwner
	}
	return 0, 0, 0, false
}

func ResolvePageContents(objects []PdfObject, objByNum map[int]int, pageIdx int) []int {
	body := objects[pageIdx].Body
	if m := ContentsArrRe.FindSubmatch(body); m != nil {
		var idxs []int
		for _, r := range refInArrRe.FindAllSubmatch(m[1], -1) {
			num, _ := strconv.Atoi(string(r[1]))
			if idx, ok := objByNum[num]; ok {
				idxs = append(idxs, idx)
			}
		}
		return idxs
	}
	if m := ContentsRefRe.FindSubmatch(body); m != nil {
		num, _ := strconv.Atoi(string(m[1]))
		if idx, ok := objByNum[num]; ok {
			return []int{idx}
		}
	}
	return nil
}

// ResolvePageMediaBox finds the /MediaBox that applies to a page
// (walking /Parent for an inherited one), falling back to US Letter if
// none is found or it doesn't parse cleanly. Only returns width/height;
// pdfTools' crop tool needs the true box origin too (a MediaBox doesn't
// have to start at [0,0]) and resolves that itself directly via
// MediaBoxKeyRe/ParentKeyRe/FindDictValue.
func ResolvePageMediaBox(objects []PdfObject, objByNum map[int]int, pageIdx int) (width, height float64) {
	curIdx := pageIdx
	for hops := 0; hops < 16; hops++ {
		if m := MediaBoxKeyRe.FindSubmatch(objects[curIdx].Body); m != nil {
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
		pOwner, _, _, pOk := FindDictValue(objects, objByNum, curIdx, 0, len(objects[curIdx].Body), ParentKeyRe)
		if !pOk {
			break
		}
		curIdx = pOwner
	}
	return DefaultPageWidth, DefaultPageHeight
}
