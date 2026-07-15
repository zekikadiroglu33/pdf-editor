// /ToUnicode CMap parsing, used by fonts.go's resolveFont to decode CID
// (Identity-H/Type0) fonts' glyph codes back into Unicode.
package pdfcore

import (
	"encoding/hex"
	"regexp"
	"strconv"
)

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

// parseToUnicodeCMap parses the bfchar/bfrange blocks of a /ToUnicode
// CMap stream into a CID -> rune map (first rune only, for multi-rune
// mappings).
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
