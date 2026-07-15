// Minimal PDF content-stream lexer, shared by editor (find/replace) and
// conversion (paragraph extraction).
package pdfcore

// CsToken is one lexical token from a content stream.
type CsToken struct {
	Kind       byte // 'S' string (literal or hex, decoded into B), 'N' name, 'n' number, 'O' operator, '[' / ']' array delimiters, 'D' opaque inline dict
	Start, End int  // byte range in the stream, including delimiters
	B          []byte
	S          string
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

// TokenizeContentStream performs a minimal PDF content-stream lexical
// scan: literal/hex strings are decoded to raw bytes, names/numbers/
// operators are captured as-is, and inline dicts (marked-content
// property lists) are skipped opaquely.
func TokenizeContentStream(data []byte) []CsToken {
	var toks []CsToken
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
			toks = append(toks, CsToken{Kind: 'S', Start: start, End: i, B: buf})
		case c == '<':
			if i+1 < n && data[i+1] == '<' {
				start := i
				e := DictEnd(data, i)
				if e < 0 {
					e = n
				}
				toks = append(toks, CsToken{Kind: 'D', Start: start, End: e})
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
				toks = append(toks, CsToken{Kind: 'S', Start: start, End: i, B: b})
			}
		case c == '[':
			toks = append(toks, CsToken{Kind: '[', Start: i, End: i + 1})
			i++
		case c == ']':
			toks = append(toks, CsToken{Kind: ']', Start: i, End: i + 1})
			i++
		case c == '/':
			start := i
			i++
			for i < n && !isCSWhitespace(data[i]) && !isCSDelim(data[i]) {
				i++
			}
			toks = append(toks, CsToken{Kind: 'N', Start: start, End: i, S: string(data[start+1 : i])})
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
			toks = append(toks, CsToken{Kind: kind, Start: start, End: i, S: text})
		}
	}
	return toks
}
