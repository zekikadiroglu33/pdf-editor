// Font-aware text search/replace inside PDF content streams.
//
// The naive approach (main.go's original replaceInStreams) matched raw
// bytes across whole streams, which only works when the PDF's font uses a
// simple, known single-byte encoding. Many real-world PDFs (Word, Google
// Docs, LibreOffice, browser print-to-PDF) render some text through
// embedded CID/Type0 (Identity-H) subset fonts, where content-stream bytes
// are glyph indices with no fixed meaning outside that specific font - the
// only way to know what a byte sequence "says" is to read that font's own
// /ToUnicode CMap.
//
// This file builds, per page, a map from font resource name to a decode
// table (simple-font byte table, or CID font's ToUnicode map), tokenizes
// each content stream, and reconstructs the Unicode text actually shown -
// spanning font switches - so find/replace works regardless of which font
// rendered which character. Replacement text that would need a glyph the
// original (possibly heavily subsetted) font doesn't have is instead routed
// through a bundled fallback font with full Latin+Turkish coverage (see
// fallbackfont.go), which is only added to a page's Resources when actually
// used.
package main

import (
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
)

// ---------------------------------------------------------------------------
// Page / Resources / Font resolution
// ---------------------------------------------------------------------------

var (
	pageTypeRe     = regexp.MustCompile(`/Type\s*/Page\b`)
	resourcesKeyRe = regexp.MustCompile(`/Resources\b`)
	fontKeyRe      = regexp.MustCompile(`/Font\b`)
	parentKeyRe    = regexp.MustCompile(`/Parent\b`)
	indirectRefRe  = regexp.MustCompile(`^(\d+)\s+\d+\s+R`)
	fontEntryRe    = regexp.MustCompile(`/([A-Za-z0-9_+\-.#]+)\s+(\d+)\s+\d+\s+R`)
	subtypeType0Re = regexp.MustCompile(`/Subtype\s*/Type0\b`)
	toUnicodeRefRe = regexp.MustCompile(`/ToUnicode\s+(\d+)\s+\d+\s+R`)
	contentsArrRe  = regexp.MustCompile(`/Contents\s*\[(.*?)\]`)
	contentsRefRe  = regexp.MustCompile(`/Contents\s+(\d+)\s+\d+\s+R`)
	refInArrRe     = regexp.MustCompile(`(\d+)\s+\d+\s+R`)
	typeCatalogRe  = regexp.MustCompile(`/Type\s*/Catalog\b`)
	pagesKeyRe     = regexp.MustCompile(`/Pages\b`)
	kidsArrRe      = regexp.MustCompile(`(?s)/Kids\s*\[(.*?)\]`)
	xobjectKeyRe   = regexp.MustCompile(`/XObject\b`)
	subtypeFormRe  = regexp.MustCompile(`/Subtype\s*/Form\b`)
)

func isPageObject(body []byte) bool { return pageTypeRe.Match(body) }

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

// resolvePageOrder walks the document's page tree (/Root -> /Pages ->
// /Kids, recursively) to produce the logical, front-to-back page order -
// the same order a PDF viewer (and pdf.js in the browser UI) numbers pages
// in. This can differ from the physical order objects appear in the file,
// which is why occurrence targeting (see EditPDF) can't just use object
// order to figure out which page the browser called "page N".
func resolvePageOrder(objects []pdfObject, objByNum map[int]int) []int {
	rootIdx := -1
	for i, o := range objects {
		if typeCatalogRe.Match(o.body) {
			rootIdx = i
			break
		}
	}
	if rootIdx < 0 {
		return nil
	}
	pagesIdx, _, _, ok := findDictValue(objects, objByNum, rootIdx, 0, len(objects[rootIdx].body), pagesKeyRe)
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
		body := objects[idx].body
		if isPageObject(body) {
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

// findDictValue locates the value of `/key`-matching keyRe within the
// window objects[ownerIdx].body[searchFrom:searchTo]. If the value is an
// inline dict "<< ... >>", it returns the same owner with the dict's
// *absolute* byte range within that owner's full body. If the value is an
// indirect reference, it returns the referenced object as the new owner,
// spanning that object's entire body.
func findDictValue(objects []pdfObject, objByNum map[int]int, ownerIdx, searchFrom, searchTo int, keyRe *regexp.Regexp) (newOwnerIdx, start, end int, ok bool) {
	body := objects[ownerIdx].body
	loc := keyRe.FindIndex(body[searchFrom:searchTo])
	if loc == nil {
		return 0, 0, 0, false
	}
	i := skipWS(body, searchFrom+loc[1])
	if i+1 < len(body) && body[i] == '<' && body[i+1] == '<' {
		e := dictEnd(body, i)
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
	return idx, 0, len(objects[idx].body), true
}

// resolvePageResources finds the /Resources dict that applies to a page
// (walking /Parent for inherited /Resources), returning its owning object
// index and its byte range within that object's body.
func resolvePageResources(objects []pdfObject, objByNum map[int]int, pageIdx int) (ownerIdx, start, end int, ok bool) {
	curIdx := pageIdx
	for hops := 0; hops < 16; hops++ {
		if resOwner, rStart, rEnd, rOk := findDictValue(objects, objByNum, curIdx, 0, len(objects[curIdx].body), resourcesKeyRe); rOk {
			return resOwner, rStart, rEnd, true
		}
		pOwner, _, _, pOk := findDictValue(objects, objByNum, curIdx, 0, len(objects[curIdx].body), parentKeyRe)
		if !pOk {
			return 0, 0, 0, false
		}
		curIdx = pOwner
	}
	return 0, 0, 0, false
}

func resolvePageContents(objects []pdfObject, objByNum map[int]int, pageIdx int) []int {
	body := objects[pageIdx].body
	if m := contentsArrRe.FindSubmatch(body); m != nil {
		var idxs []int
		for _, r := range refInArrRe.FindAllSubmatch(m[1], -1) {
			num, _ := strconv.Atoi(string(r[1]))
			if idx, ok := objByNum[num]; ok {
				idxs = append(idxs, idx)
			}
		}
		return idxs
	}
	if m := contentsRefRe.FindSubmatch(body); m != nil {
		num, _ := strconv.Atoi(string(m[1]))
		if idx, ok := objByNum[num]; ok {
			return []int{idx}
		}
	}
	return nil
}

// resolvedFont is the decode table for one font resource: either a simple
// single-byte table (shared with encoding.go's language tables) or a CID
// font's own ToUnicode map.
type resolvedFont struct {
	isCID        bool
	cidToRune    map[uint32]rune
	simpleDecode map[byte]rune
	// widths maps byte code -> advance width (1000 units/em) for a simple
	// font, parsed from its own /FirstChar + /Widths - nil if that
	// couldn't be determined (e.g. no /Widths found). Real-world subset
	// fonts (very common from tools like iText) often only embed a
	// non-zero /Widths entry for the exact bytes the original document
	// used; every other byte in FirstChar..LastChar is explicitly 0. Reusing
	// such a byte for replacement text would draw the glyph (or nothing)
	// with zero advance, stacking every following character on top of it -
	// see the width-safety check in replaceInDecodedStream.
	widths map[byte]int
}

var (
	firstCharRe    = regexp.MustCompile(`/FirstChar\s+(\d+)`)
	widthsInlineRe = regexp.MustCompile(`(?s)/Widths\s*\[(.*?)\]`)
	widthsRefRe    = regexp.MustCompile(`/Widths\s+(\d+)\s+\d+\s+R`)
	numTokenRe     = regexp.MustCompile(`-?\d+(?:\.\d+)?`)
)

// parseSimpleFontWidths reads a simple (non-CID) font's /FirstChar and
// /Widths (inline array or indirect reference to one) into a byte -> width
// map. Returns nil if either piece couldn't be found.
func parseSimpleFontWidths(objects []pdfObject, objByNum map[int]int, body []byte) map[byte]int {
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
			widthsBody = objects[idx].body
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

func resolveFont(objects []pdfObject, objByNum map[int]int, fontObjIdx, fontObjNum int, cache map[int]*resolvedFont, lang string) *resolvedFont {
	if f, ok := cache[fontObjNum]; ok {
		return f
	}
	f := &resolvedFont{}
	body := objects[fontObjIdx].body
	if subtypeType0Re.Match(body) {
		f.isCID = true
		f.cidToRune = map[uint32]rune{}
		if m := toUnicodeRefRe.FindSubmatch(body); m != nil {
			tuNum, _ := strconv.Atoi(string(m[1]))
			if tuIdx, ok := objByNum[tuNum]; ok {
				f.cidToRune = parseToUnicodeCMap(objects[tuIdx].stream)
			}
		}
	} else {
		f.simpleDecode = decodingTables[lang]
		f.widths = parseSimpleFontWidths(objects, objByNum, body)
	}
	cache[fontObjNum] = f
	return f
}

func buildFontMap(fontDictBody []byte, objects []pdfObject, objByNum map[int]int, cache map[int]*resolvedFont, lang string) map[string]*resolvedFont {
	m := map[string]*resolvedFont{}
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

// insertFontEntry splices a new "/Name N 0 R" entry into an existing font
// dict, given the dict's byte range [start,end) within obj.body (obj.body
// itself if the dict was indirect).
func insertFontEntry(obj *pdfObject, start, end int, resourceName string, fontObjNum int) {
	insertion := []byte(fmt.Sprintf(" /%s %d 0 R", resourceName, fontObjNum))
	pos := end - 2 // start of the dict's closing ">>"
	newBody := make([]byte, 0, len(obj.body)+len(insertion))
	newBody = append(newBody, obj.body[:pos]...)
	newBody = append(newBody, insertion...)
	newBody = append(newBody, obj.body[pos:]...)
	obj.body = newBody
}

// xobjectMap maps an /XObject dict's resource names to the object index of
// what they reference (same "/Name N 0 R" shape as a /Font dict).
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

// resScope is the /Font + /XObject resolution that applies within one
// content stream: either a page's own /Resources, or a Form XObject's own
// /Resources if it declares one (a Form without its own /Resources
// inherits whatever was active where it's invoked).
type resScope struct {
	fontMap                    map[string]*resolvedFont
	fontOwnerIdx, fStart, fEnd int
	fontOk                     bool
	xobjMap                    map[string]int
}

func resolveScope(objects []pdfObject, objByNum map[int]int, fontCache map[int]*resolvedFont, lang string, resOwner, rStart, rEnd int) resScope {
	var s resScope
	if fo, fs, fe, ok := findDictValue(objects, objByNum, resOwner, rStart, rEnd, fontKeyRe); ok {
		s.fontOwnerIdx, s.fStart, s.fEnd, s.fontOk = fo, fs, fe, true
		s.fontMap = buildFontMap(objects[fo].body[fs:fe], objects, objByNum, fontCache, lang)
	}
	if xo, xs, xe, ok := findDictValue(objects, objByNum, resOwner, rStart, rEnd, xobjectKeyRe); ok {
		s.xobjMap = xobjectMap(objects[xo].body[xs:xe], objByNum)
	}
	return s
}

// collectDoNames scans a content stream's tokens for "/Name Do" operator
// pairs (in the order they appear), returning the invoked XObject resource
// names.
func collectDoNames(toks []csToken) []string {
	var names []string
	for i, t := range toks {
		if t.kind == 'O' && t.s == "Do" && i > 0 && toks[i-1].kind == 'N' {
			names = append(names, toks[i-1].s)
		}
	}
	return names
}

// formContentUnit is one stream to search/replace text in - a page's own
// /Contents, or a Form XObject reached from it via a "/Name Do" call -
// paired with the font/XObject resolution that applies within it.
type formContentUnit struct {
	streamIdx int
	scope     resScope
}

// walkContent recursively expands a content stream into every text-bearing
// unit reachable from it: the stream itself, plus - for every "/Name Do"
// invoking a Form XObject in its /Resources/XObject - that Form's own
// content, recursively. This is what lets pdfer find and edit text in PDFs
// (commonly produced by tools like iText) that put all of a page's real
// content inside a Form XObject instead of directly in the page's own
// /Contents, which otherwise has no text and no /Font resources at all.
func walkContent(objects []pdfObject, objByNum map[int]int, fontCache map[int]*resolvedFont, lang string, streamIdx int, scope resScope, depth int, visiting map[int]bool, out *[]formContentUnit) {
	if depth > 8 || visiting[streamIdx] || !objects[streamIdx].hasStream {
		return
	}
	visiting[streamIdx] = true
	defer delete(visiting, streamIdx)

	*out = append(*out, formContentUnit{streamIdx: streamIdx, scope: scope})

	if len(scope.xobjMap) == 0 {
		return
	}
	for _, name := range collectDoNames(tokenizeContentStream(objects[streamIdx].stream)) {
		formIdx, ok := scope.xobjMap[name]
		if !ok || !objects[formIdx].hasStream || !subtypeFormRe.Match(objects[formIdx].body) {
			continue // not resolvable, or an Image/other XObject subtype - not text-bearing
		}
		childScope := scope
		if fOwner, fStart, fEnd, fOk := findDictValue(objects, objByNum, formIdx, 0, len(objects[formIdx].body), resourcesKeyRe); fOk {
			childScope = resolveScope(objects, objByNum, fontCache, lang, fOwner, fStart, fEnd)
		}
		walkContent(objects, objByNum, fontCache, lang, formIdx, childScope, depth+1, visiting, out)
	}
}

// ---------------------------------------------------------------------------
// ToUnicode CMap parsing
// ---------------------------------------------------------------------------

var (
	bfcharBlockRe  = regexp.MustCompile(`(?s)beginbfchar(.*?)endbfchar`)
	bfrangeBlockRe = regexp.MustCompile(`(?s)beginbfrange(.*?)endbfrange`)
	bfcharPairRe   = regexp.MustCompile(`<([0-9A-Fa-f]+)>\s*<([0-9A-Fa-f]+)>`)
	bfrangeEntryRe = regexp.MustCompile(`(?s)<([0-9A-Fa-f]+)>\s*<([0-9A-Fa-f]+)>\s*(?:<([0-9A-Fa-f]+)>|\[(.*?)\])`)
	hexTokenRe     = regexp.MustCompile(`<([0-9A-Fa-f]+)>`)
)

// decodeUTF16BEHex decodes a hex string as UTF-16BE, honoring surrogate pairs.
func decodeUTF16BEHex(hexStr string) []rune {
	b, err := hex.DecodeString(hexStr)
	if err != nil || len(b) == 0 {
		return nil
	}
	var runes []rune
	for i := 0; i+1 < len(b); i += 2 {
		u := uint16(b[i])<<8 | uint16(b[i+1])
		if u >= 0xD800 && u <= 0xDBFF && i+3 < len(b) {
			u2 := uint16(b[i+2])<<8 | uint16(b[i+3])
			if u2 >= 0xDC00 && u2 <= 0xDFFF {
				r := (rune(u-0xD800)<<10 | rune(u2-0xDC00)) + 0x10000
				runes = append(runes, r)
				i += 2
				continue
			}
		}
		runes = append(runes, rune(u))
	}
	return runes
}

// parseToUnicodeCMap parses the bfchar/bfrange blocks of a /ToUnicode CMap
// stream into a CID -> rune map (first rune only, for multi-rune mappings).
func parseToUnicodeCMap(stream []byte) map[uint32]rune {
	m := map[uint32]rune{}
	for _, blk := range bfcharBlockRe.FindAllSubmatch(stream, -1) {
		for _, pair := range bfcharPairRe.FindAllSubmatch(blk[1], -1) {
			src, _ := strconv.ParseUint(string(pair[1]), 16, 32)
			if runes := decodeUTF16BEHex(string(pair[2])); len(runes) > 0 {
				m[uint32(src)] = runes[0]
			}
		}
	}
	for _, blk := range bfrangeBlockRe.FindAllSubmatch(stream, -1) {
		for _, e := range bfrangeEntryRe.FindAllSubmatch(blk[1], -1) {
			lo, _ := strconv.ParseUint(string(e[1]), 16, 32)
			hi, _ := strconv.ParseUint(string(e[2]), 16, 32)
			switch {
			case e[3] != nil: // <lo> <hi> <dst>
				dstRunes := decodeUTF16BEHex(string(e[3]))
				if len(dstRunes) == 0 {
					continue
				}
				base := dstRunes[0]
				for c := lo; c <= hi; c++ {
					m[uint32(c)] = base + rune(c-lo)
				}
			case e[4] != nil: // <lo> <hi> [<d0> <d1> ...]
				dsts := hexTokenRe.FindAllSubmatch(e[4], -1)
				for i := uint64(0); lo+i <= hi && int(i) < len(dsts); i++ {
					if runes := decodeUTF16BEHex(string(dsts[i][1])); len(runes) > 0 {
						m[uint32(lo+i)] = runes[0]
					}
				}
			}
		}
	}
	return m
}

// ---------------------------------------------------------------------------
// Content-stream tokenizer
// ---------------------------------------------------------------------------

type csToken struct {
	kind       byte // 'S' string (literal or hex, decoded into b), 'N' name, 'n' number, 'O' operator, '[' / ']' array delimiters, 'D' opaque inline dict
	start, end int  // byte range in the stream, including delimiters
	b          []byte
	s          string
}

func isCSWhitespace(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '\n', '\f', 0:
		return true
	}
	return false
}

func isCSDelim(c byte) bool {
	switch c {
	case '(', ')', '<', '>', '[', ']', '{', '}', '/', '%':
		return true
	}
	return false
}

func hexNibble(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return 0
}

func isNumberToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			continue
		}
		if (c == '+' || c == '-') && i == 0 {
			continue
		}
		if c == '.' {
			continue
		}
		return false
	}
	return true
}

// tokenizeContentStream performs a minimal PDF content-stream lexical scan:
// literal/hex strings are decoded to raw bytes, names/numbers/operators are
// captured as-is, and inline dicts (marked-content property lists) are
// skipped opaquely.
func tokenizeContentStream(data []byte) []csToken {
	var toks []csToken
	i, n := 0, len(data)
	for i < n {
		c := data[i]
		switch {
		case isCSWhitespace(c):
			i++
		case c == '%':
			for i < n && data[i] != '\n' && data[i] != '\r' {
				i++
			}
		case c == '(':
			start := i
			i++
			depth := 1
			var buf []byte
			for i < n && depth > 0 {
				ch := data[i]
				if ch == '\\' && i+1 < n {
					i++
					e := data[i]
					switch {
					case e == 'n':
						buf = append(buf, '\n')
						i++
					case e == 'r':
						buf = append(buf, '\r')
						i++
					case e == 't':
						buf = append(buf, '\t')
						i++
					case e == 'b':
						buf = append(buf, '\b')
						i++
					case e == 'f':
						buf = append(buf, '\f')
						i++
					case e == '(' || e == ')' || e == '\\':
						buf = append(buf, e)
						i++
					case e == '\r':
						i++
						if i < n && data[i] == '\n' {
							i++
						}
					case e == '\n':
						i++
					case e >= '0' && e <= '7':
						val, cnt := 0, 0
						for cnt < 3 && i < n && data[i] >= '0' && data[i] <= '7' {
							val = val*8 + int(data[i]-'0')
							i++
							cnt++
						}
						buf = append(buf, byte(val))
					default:
						buf = append(buf, e)
						i++
					}
					continue
				}
				if ch == '(' {
					depth++
					buf = append(buf, ch)
					i++
					continue
				}
				if ch == ')' {
					depth--
					i++
					if depth == 0 {
						break
					}
					buf = append(buf, ch)
					continue
				}
				buf = append(buf, ch)
				i++
			}
			toks = append(toks, csToken{kind: 'S', start: start, end: i, b: buf})
		case c == '<':
			if i+1 < n && data[i+1] == '<' {
				start := i
				e := dictEnd(data, i)
				if e < 0 {
					e = n
				}
				toks = append(toks, csToken{kind: 'D', start: start, end: e})
				i = e
			} else {
				start := i
				i++
				var hexDigits []byte
				for i < n && data[i] != '>' {
					if !isCSWhitespace(data[i]) {
						hexDigits = append(hexDigits, data[i])
					}
					i++
				}
				if i < n {
					i++
				}
				if len(hexDigits)%2 == 1 {
					hexDigits = append(hexDigits, '0')
				}
				b := make([]byte, len(hexDigits)/2)
				for k := range b {
					b[k] = byte(hexNibble(hexDigits[2*k])<<4 | hexNibble(hexDigits[2*k+1]))
				}
				toks = append(toks, csToken{kind: 'S', start: start, end: i, b: b})
			}
		case c == '[':
			toks = append(toks, csToken{kind: '[', start: i, end: i + 1})
			i++
		case c == ']':
			toks = append(toks, csToken{kind: ']', start: i, end: i + 1})
			i++
		case c == '/':
			start := i
			i++
			for i < n && !isCSWhitespace(data[i]) && !isCSDelim(data[i]) {
				i++
			}
			toks = append(toks, csToken{kind: 'N', start: start, end: i, s: string(data[start+1 : i])})
		case c == '{' || c == '}':
			i++
		default:
			start := i
			for i < n && !isCSWhitespace(data[i]) && !isCSDelim(data[i]) {
				i++
			}
			if i == start {
				i++
				continue
			}
			text := string(data[start:i])
			kind := byte('O')
			if isNumberToken(text) {
				kind = 'n'
			}
			toks = append(toks, csToken{kind: kind, start: start, end: i, s: text})
		}
	}
	return toks
}

// ---------------------------------------------------------------------------
// Decode content-stream text (spanning font switches) to Unicode
// ---------------------------------------------------------------------------

// gapRune marks a byte position pdfer could not decode (unknown font,
// unmapped CID, or unrepresentable byte). It can never appear in a real
// find string, so it naturally blocks matches from spanning it.
const gapRune = rune(0xFFFD)

type textSpan struct {
	start, end         int // byte range of this string token in the stream
	fontName, fontSize string
	isCID              bool
	// inArray marks a span that is a string element of a TJ array. Since
	// operators like Tf/Tj cannot appear inside "[ ... ]", a replacement
	// that needs a different font than this span's must rewrite the whole
	// array construct (arrayStart..arrayEnd, covering "[ ... ] TJ") rather
	// than splicing just the string token - see replaceInDecodedStream.
	inArray              bool
	arrayStart, arrayEnd int
	// opEnd is the end of the Tj/'/" operator immediately following this
	// span's string token (0 if the span is an array element, which has no
	// operator of its own). A same-font replacement that needs to remove
	// this span entirely (see replaceInDecodedStream) must consume through
	// opEnd, not just end, or the original operator is left behind with no
	// operand - producing invalid, structure-breaking content-stream syntax.
	opEnd int
	// resolved is the font active for this span, kept so
	// replaceInDecodedStream can consult its /Widths (see resolvedFont)
	// when deciding whether reusing this font for replacement text is
	// width-safe.
	resolved *resolvedFont
}

type decodedStream struct {
	runes  []rune
	spanOf []int // len(runes); index into spans, or -1 for a gap marker
	spans  []textSpan
}

func decodeContentStream(toks []csToken, fontMap map[string]*resolvedFont) *decodedStream {
	res := &decodedStream{}
	var curFont, curSize string
	var curResolved *resolvedFont
	var lastName, lastNumber string

	// The current font is part of the graphics state, which "q" saves and
	// "Q" restores - a font set via Tf inside a q/Q pair (e.g. one PDF
	// generators commonly wrap around each individual field/line) must not
	// leak into whatever comes after the matching Q. Without this stack, a
	// field that switches to a CID font and is immediately followed by a
	// field with no Tf of its own would incorrectly keep decoding through
	// the leftover CID font instead of being restored to what was active
	// before the "q".
	type fontState struct {
		font, size string
		resolved   *resolvedFont
	}
	var gsStack []fontState

	appendSpan := func(strTok csToken) {
		spanIdx := len(res.spans)
		var runes []rune
		switch {
		case curResolved == nil:
			runes = []rune{gapRune}
		case curResolved.isCID:
			b := strTok.b
			for k := 0; k+1 < len(b); k += 2 {
				code := uint32(b[k])<<8 | uint32(b[k+1])
				if r, ok := curResolved.cidToRune[code]; ok {
					runes = append(runes, r)
				} else {
					runes = append(runes, gapRune)
				}
			}
			if len(b)%2 == 1 {
				runes = append(runes, gapRune)
			}
		default:
			for _, bb := range strTok.b {
				if r, ok := curResolved.simpleDecode[bb]; ok {
					runes = append(runes, r)
				} else {
					runes = append(runes, gapRune)
				}
			}
		}
		res.spans = append(res.spans, textSpan{
			start: strTok.start, end: strTok.end,
			fontName: curFont, fontSize: curSize,
			isCID:    curResolved != nil && curResolved.isCID,
			resolved: curResolved,
		})
		for _, r := range runes {
			res.runes = append(res.runes, r)
			res.spanOf = append(res.spanOf, spanIdx)
		}
	}

	i := 0
	for i < len(toks) {
		t := toks[i]
		switch t.kind {
		case 'N':
			lastName = t.s
		case 'n':
			lastNumber = t.s
		case '[':
			depth := 1
			j := i + 1
			var arrSpanIdxs []int
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
					appendSpan(toks[j])
					arrSpanIdxs = append(arrSpanIdxs, len(res.spans)-1)
				}
				j++
			}
		arrDone:
			arrEnd := toks[j].end
			if j+1 < len(toks) && toks[j+1].kind == 'O' && toks[j+1].s == "TJ" {
				arrEnd = toks[j+1].end
			}
			for _, si := range arrSpanIdxs {
				res.spans[si].inArray = true
				res.spans[si].arrayStart = t.start
				res.spans[si].arrayEnd = arrEnd
			}
			i = j
		case 'O':
			switch t.s {
			case "q":
				gsStack = append(gsStack, fontState{curFont, curSize, curResolved})
			case "Q":
				if len(gsStack) > 0 {
					top := gsStack[len(gsStack)-1]
					gsStack = gsStack[:len(gsStack)-1]
					curFont, curSize, curResolved = top.font, top.size, top.resolved
				}
			case "Tf":
				curFont, curSize = lastName, lastNumber
				curResolved = fontMap[curFont]
			case "Tj", "'", "\"":
				if i > 0 && toks[i-1].kind == 'S' {
					appendSpan(toks[i-1])
					res.spans[len(res.spans)-1].opEnd = t.end
				}
			}
			lastName, lastNumber = "", ""
		}
		i++
	}
	return res
}

// ---------------------------------------------------------------------------
// Find/replace over decoded text, splicing the original byte stream
// ---------------------------------------------------------------------------

// occurrenceFilter restricts replacement to a single occurrence within a
// page, counted across all of that page's content streams in order.
// target < 0 means "no filter" (replace every match, the legacy
// find-and-replace-all behavior).
type occurrenceFilter struct {
	next   int
	target int
}

func (f *occurrenceFilter) keep() bool {
	if f == nil || f.target < 0 {
		return true
	}
	k := f.next == f.target
	f.next++
	return k
}

func runesEqual(a, b []rune) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// spanFullEnd is the byte offset through which a span must be consumed to
// remove it cleanly: through its own Tj/'/" operator if it has one (opEnd),
// or just its string token's end for a TJ array element, which has no
// operator of its own (the array's single trailing TJ covers every element).
func spanFullEnd(sp textSpan) int {
	if sp.opEnd > sp.end {
		return sp.opEnd
	}
	return sp.end
}

// literalBytes wraps already-escaped bytes in a PDF literal string.
func literalBytes(escaped []byte) []byte {
	b := make([]byte, 0, len(escaped)+2)
	b = append(b, '(')
	b = append(b, escaped...)
	b = append(b, ')')
	return b
}

// replaceInDecodedStream finds every non-overlapping occurrence of
// findRunes in dr.runes and rewrites the corresponding byte range(s) in
// streamBytes. A match that touches any CID-font span is rerouted entirely
// through the bundled fallback font (fallbackTable); matches confined to a
// single simple font are re-encoded through simpleTable, preserving that
// font. usesFallback reports whether any match needed rerouting.
//
// occFilter, if non-nil with a target >= 0, restricts the edit to the
// single Nth match seen (across this and any earlier calls sharing the
// same filter, e.g. multiple content streams on one page) - every other
// match is left untouched. This is what lets a user's mouse selection in
// the UI target exactly the word they clicked instead of every occurrence
// of that word in the document.
func replaceInDecodedStream(streamBytes []byte, dr *decodedStream, findRunes, replRunes []rune, simpleTable, fallbackTable map[rune]byte, occFilter *occurrenceFilter) (newStream []byte, count int, usesFallback bool) {
	fl := len(findRunes)
	n := len(dr.runes)
	if fl == 0 {
		return streamBytes, 0, false
	}

	type match struct{ i, j int }
	var matches []match
	for i := 0; i+fl <= n; {
		if runesEqual(dr.runes[i:i+fl], findRunes) {
			matches = append(matches, match{i, i + fl})
			i += fl
		} else {
			i++
		}
	}
	if len(matches) == 0 {
		return streamBytes, 0, false
	}

	type edit struct {
		start, end int
		newBytes   []byte
	}
	var edits []edit

	for _, mt := range matches {
		minSpan, maxSpan := dr.spanOf[mt.i], dr.spanOf[mt.j-1]
		if minSpan < 0 || maxSpan < 0 {
			continue
		}
		if !occFilter.keep() {
			continue
		}

		prefixStart := mt.i
		for prefixStart > 0 && dr.spanOf[prefixStart-1] == minSpan {
			prefixStart--
		}
		suffixEnd := mt.j
		for suffixEnd < n && dr.spanOf[suffixEnd] == maxSpan {
			suffixEnd++
		}

		var fullRunes []rune
		fullRunes = append(fullRunes, dr.runes[prefixStart:mt.i]...)
		fullRunes = append(fullRunes, replRunes...)
		fullRunes = append(fullRunes, dr.runes[mt.j:suffixEnd]...)

		// Reroute to the bundled fallback font whenever the matched span
		// came from a CID font (no fixed byte meaning outside that font),
		// or whenever the text we're about to emit needs a Turkish-specific
		// character: pdfer doesn't parse the original font's /Differences,
		// so it can't assume a plain-WinAnsiEncoding font supports those six
		// code points - reusing them would silently render the wrong
		// (Icelandic) glyph, as WinAnsiEncoding defines them.
		reroute := needsTurkishSpecial(fullRunes)
		for s := minSpan; s <= maxSpan; s++ {
			if dr.spans[s].isCID {
				reroute = true
				break
			}
		}

		// Even a font that can represent every character here may not be
		// safe to reuse: real-world subset fonts (very common from tools
		// like iText) frequently only embed a non-zero /Widths entry for
		// the exact bytes the original document actually used - every
		// other byte in FirstChar..LastChar has width 0. Rendering
		// replacement text through one of those bytes would draw the
		// glyph (or a gap) with zero advance, stacking every following
		// character on top of it. If we know the span's font's widths and
		// any byte the replacement needs isn't safely covered, reroute to
		// the fallback font instead, which has full-width coverage.
		if !reroute {
			if rf := dr.spans[minSpan].resolved; rf != nil && rf.widths != nil {
				if raw, ok := encodeRaw(string(fullRunes), simpleTable); ok {
					for _, b := range raw {
						if w, known := rf.widths[b]; !known || w == 0 {
							reroute = true
							break
						}
					}
				}
			}
		}

		var targetName, targetSize string
		var table map[rune]byte
		if reroute {
			targetName, targetSize, table = fallbackFontResourceName, dr.spans[minSpan].fontSize, fallbackTable
		} else {
			targetName, targetSize, table = dr.spans[minSpan].fontName, dr.spans[minSpan].fontSize, simpleTable
		}
		if targetName == "" || targetSize == "" {
			continue // no known font/size to render through; skip rather than guess
		}

		encoded, ok := encodeAndEscape(string(fullRunes), table)
		if !ok {
			continue // character not representable even via the fallback font
		}

		if !reroute {
			// Same font throughout: swap only the literal string bytes of
			// the matched span(s) in place. Every operator, kerning number,
			// positioning command (Td/TD/Tm), and array bracket is left
			// completely untouched, which is what keeps the rest of the
			// document's structure - table layout, tightly kerned
			// punctuation like ":" or ";", multi-column flow - exactly as
			// it was in the original file.
			edits = append(edits, edit{
				start:    dr.spans[minSpan].start,
				end:      dr.spans[minSpan].end,
				newBytes: literalBytes(encoded),
			})
			for s := minSpan + 1; s <= maxSpan; s++ {
				// Any other span consumed by this match had its text
				// folded into the replacement above; blank it out with a
				// single space (never delete outright - that could merge
				// two adjacent numeric tokens, e.g. a TJ kerning number,
				// into one wrong number) rather than restructuring
				// anything around it.
				edits = append(edits, edit{start: dr.spans[s].start, end: spanFullEnd(dr.spans[s]), newBytes: []byte(" ")})
			}
			count++
			continue
		}

		// reroute: the replacement needs a font the matched span(s) don't
		// use (a CID font's byte codes are meaningless outside that font,
		// or the text needs a Turkish-specific glyph the original simple
		// font isn't assumed to support). Tf can't appear inside
		// "[ ... ]", so touching a TJ array element still requires
		// rewriting that whole array construct; untouched sibling
		// elements are passed through as their own Tj calls (losing their
		// exact kerning against the replaced text - a far smaller,
		// localized approximation than disturbing anything outside the
		// array).
		var replacementBytes []byte
		replacementBytes = append(replacementBytes, []byte(fmt.Sprintf("/%s %s Tf ", targetName, targetSize))...)
		replacementBytes = append(replacementBytes, literalBytes(encoded)...)
		replacementBytes = append(replacementBytes, []byte(" Tj")...)
		origName, origSize := dr.spans[maxSpan].fontName, dr.spans[maxSpan].fontSize
		if origName != "" {
			replacementBytes = append(replacementBytes, []byte(fmt.Sprintf(" /%s %s Tf", origName, origSize))...)
		}

		if dr.spans[minSpan].inArray || dr.spans[maxSpan].inArray {
			editStart, editEnd := dr.spans[minSpan].start, spanFullEnd(dr.spans[maxSpan])
			if dr.spans[minSpan].inArray {
				editStart = dr.spans[minSpan].arrayStart
			}
			if dr.spans[maxSpan].inArray {
				editEnd = dr.spans[maxSpan].arrayEnd
			}
			var nb []byte
			for s, sp := range dr.spans {
				if sp.start < editStart || sp.end > editEnd {
					continue
				}
				switch {
				case s < minSpan || s > maxSpan:
					nb = append(nb, []byte(fmt.Sprintf("/%s %s Tf ", sp.fontName, sp.fontSize))...)
					nb = append(nb, streamBytes[sp.start:sp.end]...)
					nb = append(nb, []byte(" Tj ")...)
				case s == minSpan:
					nb = append(nb, replacementBytes...)
					nb = append(nb, ' ')
				}
			}
			edits = append(edits, edit{start: editStart, end: editEnd, newBytes: nb})
		} else {
			// Multiple separate Tj/'/" calls (not an array) consumed by
			// this match: replace the first one's own text+operator with
			// the font-switching replacement, and blank out any other
			// consumed span's own text+operator individually - anything
			// between them (e.g. a Td moving to the next line) is left
			// completely alone.
			edits = append(edits, edit{
				start:    dr.spans[minSpan].start,
				end:      spanFullEnd(dr.spans[minSpan]),
				newBytes: replacementBytes,
			})
			for s := minSpan + 1; s <= maxSpan; s++ {
				edits = append(edits, edit{start: dr.spans[s].start, end: spanFullEnd(dr.spans[s]), newBytes: []byte(" ")})
			}
		}

		usesFallback = true
		count++
	}

	out := append([]byte(nil), streamBytes...)
	for k := len(edits) - 1; k >= 0; k-- {
		e := edits[k]
		merged := append([]byte(nil), out[:e.start]...)
		merged = append(merged, e.newBytes...)
		merged = append(merged, out[e.end:]...)
		out = merged
	}
	return out, count, usesFallback
}
