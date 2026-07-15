// Package editor is the find/replace engine: EditPDF and everything only
// it needs (the decoded-text matcher/splicer below). The font-aware
// decoding these are built on (tokenizer, font/page resolution, the
// bundled fallback font, encoding tables) lives in pdfcore, shared with
// the conversion and pdfTools packages - but decoding *for the purpose
// of finding and replacing text* is specific to this package alone.
package editor

import (
	"context"
	"fmt"
	"regexp"
	"unicode/utf8"

	"pdfer/pdfcore"
)

// ---------------------------------------------------------------------------
// Decode content-stream text (spanning font switches) to Unicode
// ---------------------------------------------------------------------------

// gapRune marks a byte position that couldn't be decoded (unknown font,
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
	// replaceInDecodedStream can consult its /Widths (see pdfcore.ResolvedFont)
	// when deciding whether reusing this font for replacement text is
	// width-safe.
	resolved *pdfcore.ResolvedFont
}

type decodedStream struct {
	runes  []rune
	spanOf []int // len(runes); index into spans, or -1 for a gap marker
	spans  []textSpan
}

func decodeContentStream(toks []pdfcore.CsToken, fontMap map[string]*pdfcore.ResolvedFont) *decodedStream {
	res := &decodedStream{}
	var curFont, curSize string
	var curResolved *pdfcore.ResolvedFont
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
		resolved   *pdfcore.ResolvedFont
	}
	var gsStack []fontState

	appendSpan := func(strTok pdfcore.CsToken) {
		spanIdx := len(res.spans)
		var runes []rune
		switch {
		case curResolved == nil:
			runes = []rune{gapRune}
		case curResolved.IsCID:
			b := strTok.B
			for k := 0; k+1 < len(b); k += 2 {
				code := uint32(b[k])<<8 | uint32(b[k+1])
				if r, ok := curResolved.CidToRune[code]; ok {
					runes = append(runes, r)
				} else {
					runes = append(runes, gapRune)
				}
			}
			if len(b)%2 == 1 {
				runes = append(runes, gapRune)
			}
		default:
			for _, bb := range strTok.B {
				if r, ok := curResolved.SimpleDecode[bb]; ok {
					runes = append(runes, r)
				} else {
					runes = append(runes, gapRune)
				}
			}
		}
		res.spans = append(res.spans, textSpan{
			start: strTok.Start, end: strTok.End,
			fontName: curFont, fontSize: curSize,
			isCID:    curResolved != nil && curResolved.IsCID,
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
		switch t.Kind {
		case 'N':
			lastName = t.S
		case 'n':
			lastNumber = t.S
		case '[':
			depth := 1
			j := i + 1
			var arrSpanIdxs []int
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
					appendSpan(toks[j])
					arrSpanIdxs = append(arrSpanIdxs, len(res.spans)-1)
				}
				j++
			}
		arrDone:
			arrEnd := toks[j].End
			if j+1 < len(toks) && toks[j+1].Kind == 'O' && toks[j+1].S == "TJ" {
				arrEnd = toks[j+1].End
			}
			for _, si := range arrSpanIdxs {
				res.spans[si].inArray = true
				res.spans[si].arrayStart = t.Start
				res.spans[si].arrayEnd = arrEnd
			}
			i = j
		case 'O':
			switch t.S {
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
				if i > 0 && toks[i-1].Kind == 'S' {
					appendSpan(toks[i-1])
					res.spans[len(res.spans)-1].opEnd = t.End
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

// textMatcher finds occurrences of the user's "find" text in a stream's
// decoded rune sequence and produces the replacement runes for each match.
// In literal mode every match gets the same replRunes; in regex mode the
// replacement is expanded per match so capture-group references
// ($1, ${name}, ...) work the way Go's regexp.Expand documents.
//
// Go's regexp package compiles to RE2, which runs in time linear in the
// input length by construction - there's no catastrophic-backtracking
// pattern a caller could submit to hang a worker, unlike PCRE/Perl-style
// engines.
type textMatcher struct {
	findRunes []rune // literal mode only
	replRunes []rune // literal mode only
	re        *regexp.Regexp
	replTmpl  string // regex mode: Go regexp.Expand-style template
}

func newTextMatcher(find, repl string, useRegex bool) (*textMatcher, error) {
	if !useRegex {
		return &textMatcher{findRunes: []rune(find), replRunes: []rune(repl)}, nil
	}
	re, err := regexp.Compile(find)
	if err != nil {
		return nil, fmt.Errorf("invalid regex: %w", err)
	}
	return &textMatcher{re: re, replTmpl: repl}, nil
}

type textMatch struct {
	i, j int // rune range [i, j) in the stream's decoded runes
	repl []rune
}

// findMatches returns every non-overlapping match in runes, in order.
func (m *textMatcher) findMatches(runes []rune) []textMatch {
	if m.re == nil {
		fl := len(m.findRunes)
		if fl == 0 {
			return nil
		}
		var out []textMatch
		for i := 0; i+fl <= len(runes); {
			if runesEqual(runes[i:i+fl], m.findRunes) {
				out = append(out, textMatch{i, i + fl, m.replRunes})
				i += fl
			} else {
				i++
			}
		}
		return out
	}

	s := string(runes)
	locs := m.re.FindAllStringSubmatchIndex(s, -1)
	if locs == nil {
		return nil
	}
	out := make([]textMatch, 0, len(locs))
	for _, loc := range locs {
		i := utf8.RuneCountInString(s[:loc[0]])
		j := i + utf8.RuneCountInString(s[loc[0]:loc[1]])
		expanded := m.re.ExpandString(nil, m.replTmpl, s, loc)
		out = append(out, textMatch{i, j, []rune(string(expanded))})
	}
	return out
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
func replaceInDecodedStream(streamBytes []byte, dr *decodedStream, m *textMatcher, simpleTable, fallbackTable map[rune]byte, occFilter *occurrenceFilter) (newStream []byte, count int, usesFallback bool) {
	matches := m.findMatches(dr.runes)
	if len(matches) == 0 {
		return streamBytes, 0, false
	}
	n := len(dr.runes)

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
		fullRunes = append(fullRunes, mt.repl...)
		fullRunes = append(fullRunes, dr.runes[mt.j:suffixEnd]...)

		// Reroute to the bundled fallback font whenever the matched span
		// came from a CID font (no fixed byte meaning outside that font),
		// or whenever the text we're about to emit needs a Turkish-specific
		// character: this app doesn't parse the original font's
		// /Differences, so it can't assume a plain-WinAnsiEncoding font
		// supports those six code points - reusing them would silently
		// render the wrong (Icelandic) glyph, as WinAnsiEncoding defines them.
		reroute := pdfcore.NeedsTurkishSpecial(fullRunes)
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
			if rf := dr.spans[minSpan].resolved; rf != nil && rf.Widths != nil {
				if raw, ok := pdfcore.EncodeRaw(string(fullRunes), simpleTable); ok {
					for _, b := range raw {
						if w, known := rf.Widths[b]; !known || w == 0 {
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
			targetName, targetSize, table = pdfcore.FallbackFontResourceName, dr.spans[minSpan].fontSize, fallbackTable
		} else {
			targetName, targetSize, table = dr.spans[minSpan].fontName, dr.spans[minSpan].fontSize, simpleTable
		}
		if targetName == "" || targetSize == "" {
			continue // no known font/size to render through; skip rather than guess
		}

		encoded, ok := pdfcore.EncodeAndEscape(string(fullRunes), table)
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

// ---------------------------------------------------------------------------
// EditPDF
// ---------------------------------------------------------------------------

// EditPDF is the full pipeline: normalize -> replace -> rebuild -> recompress.
//
// Replace is font-aware (see pdfcore): it walks each page's actual fonts
// (including CID/Identity-H fonts via their /ToUnicode CMap) to
// reconstruct the real Unicode text before matching, so find/replace works
// regardless of which font rendered which character. Replacement text that
// the original font can't render is routed through a bundled fallback font.
//
// targetPage/targetOccurrence optionally restrict the edit to a single
// occurrence: targetPage <= 0 means "every page" (legacy replace-all
// behavior); targetPage > 0 with targetOccurrence < 0 means "every match on
// that page"; targetPage > 0 with targetOccurrence >= 0 means "only the
// Nth match (0-indexed, in reading order) on that page" - this is what the
// UI uses when the user selects a specific word with the mouse, so that
// selecting one occurrence doesn't rewrite every other occurrence of the
// same word throughout the document.
//
// useRegex treats find as a Go regexp (RE2, so it's immune to catastrophic
// backtracking regardless of what pattern is submitted) matched against the
// document's actual decoded Unicode text; repl is then expanded per match
// using regexp.Expand syntax ($1, ${name}, ...) instead of used verbatim.
func EditPDF(ctx context.Context, pdf []byte, find, repl, lang string, targetPage, targetOccurrence int, useRegex bool) ([]byte, int, error) {
	simpleTable, ok := pdfcore.EncodingTables[lang]
	if !ok {
		return nil, 0, &pdfcore.BadInputError{Msg: fmt.Sprintf("unsupported language %q", lang)}
	}
	fallbackTable := pdfcore.EncodingTables["turkish"] // superset covering latin + Turkish; used for the bundled fallback font
	matcher, err := newTextMatcher(find, repl, useRegex)
	if err != nil {
		return nil, 0, &pdfcore.BadInputError{Msg: err.Error()}
	}

	norm, err := pdfcore.Normalize(ctx, pdf)
	if err != nil {
		return nil, 0, fmt.Errorf("normalize: %w", err)
	}
	objects, err := pdfcore.ParseObjects(norm)
	if err != nil {
		return nil, 0, fmt.Errorf("parse: %w", err)
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
		// Page tree couldn't be resolved (unusual/malformed PDF) - fall
		// back to physical object order. Page-number targeting becomes
		// best-effort in that case, but whole-document replace-all is
		// unaffected since it doesn't care about page order.
		for idx, o := range objects {
			if pdfcore.IsPageObject(o.Body) {
				pageOrder = append(pageOrder, idx)
			}
		}
	}

	fontCache := map[int]*pdfcore.ResolvedFont{}
	type fbTarget struct{ ownerIdx, start, end int }
	var fbTargets []fbTarget
	seenOwner := map[int]bool{}
	needFallback := false
	totalCount := 0

	for pn, pageIdx := range pageOrder {
		pageNum := pn + 1
		if targetPage > 0 && pageNum != targetPage {
			continue
		}
		var occFilter *occurrenceFilter
		if targetPage > 0 && targetOccurrence >= 0 {
			occFilter = &occurrenceFilter{target: targetOccurrence}
		}

		var rootScope pdfcore.ResScope
		if resOwner, rStart, rEnd, rOk := pdfcore.ResolvePageResources(objects, objByNum, pageIdx); rOk {
			rootScope = pdfcore.ResolveScope(objects, objByNum, fontCache, lang, resOwner, rStart, rEnd)
		}

		// Expand the page's own /Contents into every text-bearing stream
		// reachable from it, including any Form XObjects it invokes via
		// "Do" (and Forms those invoke, recursively) - some PDF generators
		// (e.g. iText) put all of a page's real content inside a Form
		// XObject rather than directly in /Contents.
		var units []pdfcore.FormContentUnit
		visiting := map[int]bool{}
		for _, cIdx := range pdfcore.ResolvePageContents(objects, objByNum, pageIdx) {
			pdfcore.WalkContent(objects, objByNum, fontCache, lang, cIdx, rootScope, 0, visiting, &units)
		}

		for _, u := range units {
			dr := decodeContentStream(pdfcore.TokenizeContentStream(objects[u.StreamIdx].Stream), u.Scope.FontMap)
			newStream, n, usesFallback := replaceInDecodedStream(objects[u.StreamIdx].Stream, dr, matcher, simpleTable, fallbackTable, occFilter)
			if n == 0 {
				continue
			}
			objects[u.StreamIdx].Stream = newStream
			objects[u.StreamIdx].Body = pdfcore.LengthRe.ReplaceAll(objects[u.StreamIdx].Body,
				[]byte(fmt.Sprintf("/Length %d", len(newStream))))
			totalCount += n
			if usesFallback {
				needFallback = true
				if u.Scope.FontOk && !seenOwner[u.Scope.FontOwnerIdx] {
					seenOwner[u.Scope.FontOwnerIdx] = true
					fbTargets = append(fbTargets, fbTarget{u.Scope.FontOwnerIdx, u.Scope.FStart, u.Scope.FEnd})
				}
			}
		}
	}

	if needFallback {
		newObjs, fontObjNum := pdfcore.BuildFallbackFontObjects(maxNum + 1)
		objects = append(objects, newObjs...)
		for _, t := range fbTargets {
			pdfcore.InsertFontEntry(&objects[t.ownerIdx], t.start, t.end, pdfcore.FallbackFontResourceName, fontObjNum)
		}
	}

	rebuilt, err := pdfcore.Rebuild(objects, norm)
	if err != nil {
		return nil, 0, fmt.Errorf("rebuild: %w", err)
	}
	return pdfcore.Recompress(ctx, rebuilt), totalCount, nil
}
