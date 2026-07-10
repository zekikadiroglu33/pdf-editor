// Character-encoding support for non-ASCII find/replace text.
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
// Since the uploaded PDF is never decoded to Unicode (see main.go), the
// only way to byte-match the user's UTF-8 find/replace text against the
// stream is to transcode it into the same single-byte encoding first. The
// "language" selected in the UI picks which table to transcode with.
package main

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

// buildTable returns a rune -> byte map for a single-byte, Latin-1-based
// Windows code page, with the given code points overridden.
func buildTable(overrides map[byte]rune) map[rune]byte {
	table := make(map[rune]byte, 256)
	for b := 0; b < 0x80; b++ {
		table[rune(b)] = byte(b)
	}
	for b := 0x80; b < 0xA0; b++ {
		r := rune(b)
		if special, ok := cp1252SpecialUpper[byte(b)]; ok {
			r = special
		}
		table[r] = byte(b)
	}
	for b := 0xA0; b <= 0xFF; b++ {
		table[rune(b)] = byte(b)
	}
	for b, r := range overrides {
		table[r] = b
	}
	return table
}

// encodingTables maps a UI language code to its rune -> byte table.
var encodingTables = map[string]map[rune]byte{
	"latin":   buildTable(nil),
	"turkish": buildTable(turkishOverrides),
}

// encodeAndEscape transcodes s into the given single-byte table and escapes
// it for use inside a PDF literal string ( ... ). ok is false if s contains
// a character the table cannot represent.
func encodeAndEscape(s string, table map[rune]byte) (out []byte, ok bool) {
	var buf []byte
	for _, r := range s {
		b, found := table[r]
		if !found {
			return nil, false
		}
		if b == '\\' || b == '(' || b == ')' {
			buf = append(buf, '\\')
		}
		buf = append(buf, b)
	}
	return buf, true
}
