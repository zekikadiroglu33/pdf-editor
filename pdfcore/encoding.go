// Character-encoding support for non-ASCII find/replace/watermark text.
//
// PDF content streams store text as raw bytes in whatever single-byte
// encoding the font's /Encoding uses - almost always WinAnsiEncoding, which
// is byte-for-byte identical to Windows-1252 (and, for 0xA0-0xFF, to
// Latin-1). Turkish text additionally needs six code points that
// WinAnsiEncoding doesn't have at all (ş ş Ş ğ Ğ ı İ), because Windows
// reserves those exact byte slots in cp1252 for four Icelandic letters
// instead. Windows-1254 ("Turkish") is cp1252 with just those six slots
// swapped, so that's the table this file builds for the "turkish" language.
//
// Since the uploaded PDF is never decoded to Unicode ahead of time, the
// only way to byte-match UTF-8 text against the stream is to transcode
// it into the same single-byte encoding first. The "language" selected
// in the UI picks which table to transcode with.
package pdfcore

// cp1252SpecialUpper holds the Windows-1252 code points in 0x80-0x9F that
// are not identical to Latin-1 (curly quotes, dashes, etc.).
var cp1252SpecialUpper = map[byte]rune{
	0x80: '€', 0x82: '‚', 0x83: 'ƒ', 0x84: '„',
	0x85: '…', 0x86: '†', 0x87: '‡', 0x88: 'ˆ',
	0x89: '‰', 0x8A: 'Š', 0x8B: '‹', 0x8C: 'Œ',
	0x8E: 'Ž', 0x91: '‘', 0x92: '’', 0x93: '“',
	0x94: '”', 0x95: '•', 0x96: '–', 0x97: '—',
	0x98: '˜', 0x99: '™', 0x9A: 'š', 0x9B: '›',
	0x9C: 'œ', 0x9E: 'ž', 0x9F: 'Ÿ',
}

// turkishOverrides holds the six code points where Windows-1254 replaces a
// Windows-1252 Icelandic letter with a Turkish one.
var turkishOverrides = map[byte]rune{
	0xD0: 'Ğ', 0xDD: 'İ', 0xDE: 'Ş',
	0xF0: 'ğ', 0xFD: 'ı', 0xFE: 'ş',
}

// turkishSpecialRunes are the six Turkish letters with no representation in
// standard WinAnsiEncoding at all. A PDF's original font almost always
// declares plain /WinAnsiEncoding (this app doesn't parse /Differences), so
// reusing it to encode one of these runes would silently render the wrong
// (Icelandic) glyph. Text needing any of these must go through the bundled
// fallback font instead (see editor's reroute decision).
var turkishSpecialRunes = map[rune]bool{}

func init() {
	for _, r := range turkishOverrides {
		turkishSpecialRunes[r] = true
	}
}

func NeedsTurkishSpecial(runes []rune) bool {
	for _, r := range runes {
		if turkishSpecialRunes[r] {
			return true
		}
	}
	return false
}

// buildTable returns a rune -> byte map for a single-byte, Latin-1-based
// Windows code page, with the given code points overridden.
func buildTable(overrides map[byte]rune) map[rune]byte {
	// Resolve the byte -> rune assignment first, so an override cleanly
	// replaces the base rune at that byte instead of leaving both the old
	// and new rune mapped to it (which would make the reverse, byte->rune
	// decoding table built in init() ambiguous/nondeterministic).
	var byteRune [256]rune
	for b := 0; b < 0x80; b++ {
		byteRune[b] = rune(b)
	}
	for b := 0x80; b < 0xA0; b++ {
		r := rune(b)
		if special, ok := cp1252SpecialUpper[byte(b)]; ok {
			r = special
		}
		byteRune[b] = r
	}
	for b := 0xA0; b <= 0xFF; b++ {
		byteRune[b] = rune(b)
	}
	for b, r := range overrides {
		byteRune[b] = r
	}

	table := make(map[rune]byte, 256)
	for b, r := range byteRune {
		table[r] = byte(b)
	}
	return table
}

// EncodingTables maps a UI language code to its rune -> byte table.
var EncodingTables = map[string]map[rune]byte{
	"latin":   buildTable(nil),
	"turkish": buildTable(turkishOverrides),
}

// DecodingTables are the byte -> rune inverses of EncodingTables, used to
// decode existing simple-font PDF content-stream bytes back into Unicode
// for matching.
var DecodingTables = map[string]map[byte]rune{}

func init() {
	for lang, enc := range EncodingTables {
		dec := make(map[byte]rune, len(enc))
		for r, b := range enc {
			dec[b] = r
		}
		DecodingTables[lang] = dec
	}
}

// EncodeRaw transcodes s into the given single-byte table, unescaped. ok is
// false if s contains a character the table cannot represent.
func EncodeRaw(s string, table map[rune]byte) (out []byte, ok bool) {
	for _, r := range s {
		b, found := table[r]
		if !found {
			return nil, false
		}
		out = append(out, b)
	}
	return out, true
}

// EncodeAndEscape transcodes s into the given single-byte table and escapes
// it for use inside a PDF literal string ( ... ). ok is false if s contains
// a character the table cannot represent.
func EncodeAndEscape(s string, table map[rune]byte) (out []byte, ok bool) {
	raw, ok := EncodeRaw(s, table)
	if !ok {
		return nil, false
	}
	var buf []byte
	for _, b := range raw {
		if b == '\\' || b == '(' || b == ')' {
			buf = append(buf, '\\')
		}
		buf = append(buf, b)
	}
	return buf, true
}
