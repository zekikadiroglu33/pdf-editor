package pdfcore

import "testing"

// --- Turkish/Latin encoding table sanity --------------------------------

func TestEncodingTableIsBijective(t *testing.T) {
	// Regression: buildTable used to be able to produce a rune -> byte
	// table where inverting it for decoding was ambiguous/nondeterministic
	// (two runes mapping to the same byte), causing a subset of
	// characters to decode incorrectly at random depending on Go's map
	// iteration order.
	for lang, enc := range EncodingTables {
		dec := DecodingTables[lang]
		for r, b := range enc {
			if got := dec[b]; got != r {
				t.Errorf("%s: byte %#x round-trips to %q, want %q", lang, b, got, r)
			}
		}
	}
}

func TestTurkishSpecialCharsRoundTrip(t *testing.T) {
	table := EncodingTables["turkish"]
	for _, r := range []rune{'ş', 'Ş', 'ğ', 'Ğ', 'ı', 'İ'} {
		b, ok := table[r]
		if !ok {
			t.Errorf("turkish table missing %q", r)
			continue
		}
		if DecodingTables["turkish"][b] != r {
			t.Errorf("turkish table: %q -> byte %#x does not decode back to %q", r, b, r)
		}
	}
}
