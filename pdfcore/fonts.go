// Font resolution: mapping a page's /Font and /XObject resource names to
// decode tables (simple-font byte table, or CID font's /ToUnicode map)
// and to Form XObject content - shared by editor (find/replace needs to
// decode every font a document uses) and conversion (paragraph
// extraction needs the same thing). See tounicode.go for the /ToUnicode
// CMap parser and tokenizer.go for the content-stream tokenizer this
// also depends on.
package pdfcore

import (
	"regexp"
	"strconv"
)

var (
	fontEntryRe    = regexp.MustCompile(`/([A-Za-z0-9_+\-.#]+)\s+(\d+)\s+\d+\s+R`)
	subtypeType0Re = regexp.MustCompile(`/Subtype\s*/Type0\b`)
	toUnicodeRefRe = regexp.MustCompile(`/ToUnicode\s+(\d+)\s+\d+\s+R`)
	firstCharRe    = regexp.MustCompile(`/FirstChar\s+(\d+)`)
	widthsInlineRe = regexp.MustCompile(`(?s)/Widths\s*\[(.*?)\]`)
	widthsRefRe    = regexp.MustCompile(`/Widths\s+(\d+)\s+\d+\s+R`)
	numTokenRe     = regexp.MustCompile(`-?\d+(?:\.\d+)?`)
)

// ResolvedFont is the decode table for one font resource: either a
// simple single-byte table (shared with the language encoding tables) or
// a CID font's own ToUnicode map.
type ResolvedFont struct {
	IsCID        bool
	CidToRune    map[uint32]rune
	SimpleDecode map[byte]rune
	// Widths maps byte code -> advance width (1000 units/em) for a
	// simple font, parsed from its own /FirstChar + /Widths - nil if
	// that couldn't be determined (e.g. no /Widths found). Real-world
	// subset fonts (very common from tools like iText) often only embed
	// a non-zero /Widths entry for the exact bytes the original document
	// used; every other byte in FirstChar..LastChar is explicitly 0.
	// Reusing such a byte for replacement text would draw the glyph (or
	// nothing) with zero advance, stacking every following character on
	// top of it - see the width-safety check in editor's replace logic.
	Widths map[byte]int
}

// parseSimpleFontWidths reads a simple (non-CID) font's /FirstChar and
// /Widths (inline array or indirect reference to one) into a byte ->
// width map. Returns nil if either piece couldn't be found.
func parseSimpleFontWidths(objects []PdfObject, objByNum map[int]int, body []byte) map[byte]int {
	fm := firstCharRe.FindSubmatch(body)
	if fm == nil {
		return nil
	}
	firstChar, _ := strconv.Atoi(string(fm[1]))

	var widthsBody []byte
	if m := widthsInlineRe.FindSubmatch(body); m != nil {
		widthsBody = m[1]
	} else if m := widthsRefRe.FindSubmatch(body); m != nil {
		num, _ := strconv.Atoi(string(m[1]))
		if idx, ok := objByNum[num]; ok {
			widthsBody = objects[idx].Body
		}
	}
	if widthsBody == nil {
		return nil
	}

	toks := numTokenRe.FindAll(widthsBody, -1)
	widths := make(map[byte]int, len(toks))
	for i, tok := range toks {
		code := firstChar + i
		if code < 0 || code > 255 {
			continue
		}
		v, _ := strconv.ParseFloat(string(tok), 64)
		widths[byte(code)] = int(v)
	}
	return widths
}

func resolveFont(objects []PdfObject, objByNum map[int]int, fontObjIdx, fontObjNum int, cache map[int]*ResolvedFont, lang string) *ResolvedFont {
	if f, ok := cache[fontObjNum]; ok {
		return f
	}
	f := &ResolvedFont{}
	body := objects[fontObjIdx].Body
	if subtypeType0Re.Match(body) {
		f.IsCID = true
		f.CidToRune = map[uint32]rune{}
		if m := toUnicodeRefRe.FindSubmatch(body); m != nil {
			tuNum, _ := strconv.Atoi(string(m[1]))
			if tuIdx, ok := objByNum[tuNum]; ok {
				f.CidToRune = parseToUnicodeCMap(objects[tuIdx].Stream)
			}
		}
	} else {
		f.SimpleDecode = DecodingTables[lang]
		f.Widths = parseSimpleFontWidths(objects, objByNum, body)
	}
	cache[fontObjNum] = f
	return f
}

func buildFontMap(fontDictBody []byte, objects []PdfObject, objByNum map[int]int, cache map[int]*ResolvedFont, lang string) map[string]*ResolvedFont {
	m := map[string]*ResolvedFont{}
	for _, pair := range fontEntryRe.FindAllSubmatch(fontDictBody, -1) {
		name := string(pair[1])
		num, _ := strconv.Atoi(string(pair[2]))
		idx, ok := objByNum[num]
		if !ok {
			continue
		}
		m[name] = resolveFont(objects, objByNum, idx, num, cache, lang)
	}
	return m
}

// InsertFontEntry splices a new "/Name N 0 R" entry into an existing
// font dict, given the dict's byte range [start,end) within obj.Body
// (obj.Body itself if the dict was indirect). Exported since editor's
// EditPDF calls this directly to register the bundled fallback font the
// first time a page needs it.
func InsertFontEntry(obj *PdfObject, start, end int, resourceName string, fontObjNum int) {
	insertion := []byte(" /" + resourceName + " " + strconv.Itoa(fontObjNum) + " 0 R")
	pos := end - 2 // start of the dict's closing ">>"
	newBody := make([]byte, 0, len(obj.Body)+len(insertion))
	newBody = append(newBody, obj.Body[:pos]...)
	newBody = append(newBody, insertion...)
	newBody = append(newBody, obj.Body[pos:]...)
	obj.Body = newBody
}

// xobjectMap maps an /XObject dict's resource names to the object index
// of what they reference (same "/Name N 0 R" shape as a /Font dict).
func xobjectMap(xobjDictBody []byte, objByNum map[int]int) map[string]int {
	m := map[string]int{}
	for _, pair := range fontEntryRe.FindAllSubmatch(xobjDictBody, -1) {
		name := string(pair[1])
		num, _ := strconv.Atoi(string(pair[2]))
		if idx, ok := objByNum[num]; ok {
			m[name] = idx
		}
	}
	return m
}

// ResScope is the /Font + /XObject resolution that applies within one
// content stream: either a page's own /Resources, or a Form XObject's
// own /Resources if it declares one (a Form without its own /Resources
// inherits whatever was active where it's invoked).
type ResScope struct {
	FontMap                    map[string]*ResolvedFont
	FontOwnerIdx, FStart, FEnd int
	FontOk                     bool
	XobjMap                    map[string]int
}

func ResolveScope(objects []PdfObject, objByNum map[int]int, fontCache map[int]*ResolvedFont, lang string, resOwner, rStart, rEnd int) ResScope {
	var s ResScope
	if fo, fs, fe, ok := FindDictValue(objects, objByNum, resOwner, rStart, rEnd, FontKeyRe); ok {
		s.FontOwnerIdx, s.FStart, s.FEnd, s.FontOk = fo, fs, fe, true
		s.FontMap = buildFontMap(objects[fo].Body[fs:fe], objects, objByNum, fontCache, lang)
	}
	if xo, xs, xe, ok := FindDictValue(objects, objByNum, resOwner, rStart, rEnd, XObjectKeyRe); ok {
		s.XobjMap = xobjectMap(objects[xo].Body[xs:xe], objByNum)
	}
	return s
}

// collectDoNames scans a content stream's tokens for "/Name Do" operator
// pairs (in the order they appear), returning the invoked XObject
// resource names.
func collectDoNames(toks []CsToken) []string {
	var names []string
	for i, t := range toks {
		if t.Kind == 'O' && t.S == "Do" && i > 0 && toks[i-1].Kind == 'N' {
			names = append(names, toks[i-1].S)
		}
	}
	return names
}

// FormContentUnit is one stream to search/replace (or extract) text
// from - a page's own /Contents, or a Form XObject reached from it via
// a "/Name Do" call - paired with the font/XObject resolution that
// applies within it.
type FormContentUnit struct {
	StreamIdx int
	Scope     ResScope
}

// WalkContent recursively expands a content stream into every
// text-bearing unit reachable from it: the stream itself, plus - for
// every "/Name Do" invoking a Form XObject in its /Resources/XObject -
// that Form's own content, recursively. This is what lets pdfer find
// text in PDFs (commonly produced by tools like iText) that put all of
// a page's real content inside a Form XObject instead of directly in
// the page's own /Contents, which otherwise has no text and no /Font
// resources at all.
func WalkContent(objects []PdfObject, objByNum map[int]int, fontCache map[int]*ResolvedFont, lang string, streamIdx int, scope ResScope, depth int, visiting map[int]bool, out *[]FormContentUnit) {
	if depth > 8 || visiting[streamIdx] || !objects[streamIdx].HasStream {
		return
	}
	visiting[streamIdx] = true
	defer delete(visiting, streamIdx)

	*out = append(*out, FormContentUnit{StreamIdx: streamIdx, Scope: scope})

	if len(scope.XobjMap) == 0 {
		return
	}
	for _, name := range collectDoNames(TokenizeContentStream(objects[streamIdx].Stream)) {
		formIdx, ok := scope.XobjMap[name]
		if !ok || !objects[formIdx].HasStream || !subtypeFormRe.Match(objects[formIdx].Body) {
			continue // not resolvable, or an Image/other XObject subtype - not text-bearing
		}
		childScope := scope
		if fOwner, fStart, fEnd, fOk := FindDictValue(objects, objByNum, formIdx, 0, len(objects[formIdx].Body), resourcesKeyRe); fOk {
			childScope = ResolveScope(objects, objByNum, fontCache, lang, fOwner, fStart, fEnd)
		}
		WalkContent(objects, objByNum, fontCache, lang, formIdx, childScope, depth+1, visiting, out)
	}
}
