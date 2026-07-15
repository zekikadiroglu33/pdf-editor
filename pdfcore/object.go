// Package pdfcore is the shared PDF parsing/object-model foundation used
// by every feature package in pdfer (editor, conversion, pdfTools): the
// minimal classic-PDF object model (parse/rebuild), the qpdf CLI
// wrapper, page-tree/resource resolution, the font-aware content-stream
// tokenizer, and the bundled fallback font + character-encoding tables.
// None of it is specific to any one feature - it's the plumbing every
// feature is built on top of.
package pdfcore

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strconv"
)

// PdfObject is one indirect object ("N G obj ... endobj") in a classic
// (non-object-stream, non-cross-reference-stream) PDF.
type PdfObject struct {
	Num, Gen  int
	Body      []byte // dict (or any non-stream body), without "obj"/"endobj"
	Stream    []byte // nil if the object has no stream
	HasStream bool
}

var (
	objStartRe = regexp.MustCompile(`(?m)^(\d+)[ \t]+(\d+)[ \t]+obj\b`)
	// LengthRe matches a stream dict's /Length entry - exported since
	// editor.EditPDF rewrites it in place after changing a stream's size.
	LengthRe  = regexp.MustCompile(`/Length[ \t\r\n]+(\d+)`)
	trailerRe = regexp.MustCompile(`(?s)trailer[ \t\r\n]*(<<.*?>>)`)
	sizeRe    = regexp.MustCompile(`/Size[ \t\r\n]+\d+`)
	prevRe    = regexp.MustCompile(`/Prev[ \t\r\n]+\d+`)
)

// DictEnd returns the index just past the ">>" matching the "<<" at start.
func DictEnd(data []byte, start int) int {
	depth := 0
	for i := start; i < len(data)-1; i++ {
		if data[i] == '<' && data[i+1] == '<' {
			depth++
			i++
		} else if data[i] == '>' && data[i+1] == '>' {
			depth--
			i++
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

func SkipWS(data []byte, i int) int {
	for i < len(data) {
		switch data[i] {
		case ' ', '\t', '\r', '\n', '\f', 0:
			i++
		default:
			return i
		}
	}
	return i
}

// ParseObjects walks a normalized (classic, direct-/Length) PDF.
func ParseObjects(pdf []byte) ([]PdfObject, error) {
	var objects []PdfObject
	locs := objStartRe.FindAllSubmatchIndex(pdf, -1)
	for _, m := range locs {
		num, _ := strconv.Atoi(string(pdf[m[2]:m[3]]))
		gen, _ := strconv.Atoi(string(pdf[m[4]:m[5]]))
		pos := SkipWS(pdf, m[1])

		var obj PdfObject
		obj.Num, obj.Gen = num, gen

		if pos+1 < len(pdf) && pdf[pos] == '<' && pdf[pos+1] == '<' {
			end := DictEnd(pdf, pos)
			if end < 0 {
				return nil, fmt.Errorf("object %d: unterminated dictionary", num)
			}
			obj.Body = pdf[pos:end]
			after := SkipWS(pdf, end)
			if bytes.HasPrefix(pdf[after:], []byte("stream")) {
				lm := LengthRe.FindSubmatch(obj.Body)
				if lm == nil {
					return nil, fmt.Errorf("object %d: stream without direct /Length", num)
				}
				length, _ := strconv.Atoi(string(lm[1]))
				dataStart := after + len("stream")
				if dataStart < len(pdf) && pdf[dataStart] == '\r' {
					dataStart++
				}
				if dataStart < len(pdf) && pdf[dataStart] == '\n' {
					dataStart++
				}
				if dataStart+length > len(pdf) {
					return nil, fmt.Errorf("object %d: /Length exceeds file size", num)
				}
				obj.Stream = pdf[dataStart : dataStart+length]
				obj.HasStream = true
			}
		} else {
			// Non-dictionary object (number, array, name...): safe to
			// scan for endobj because there is no binary payload.
			end := bytes.Index(pdf[pos:], []byte("endobj"))
			if end < 0 {
				return nil, fmt.Errorf("object %d: missing endobj", num)
			}
			obj.Body = bytes.TrimSpace(pdf[pos : pos+end])
		}
		objects = append(objects, obj)
	}
	if len(objects) == 0 {
		return nil, fmt.Errorf("no PDF objects found")
	}
	return objects, nil
}

// ---------------------------------------------------------------------------
// Serialization: rebuild the PDF with a fresh xref table
// ---------------------------------------------------------------------------

func Rebuild(objects []PdfObject, original []byte) ([]byte, error) {
	tm := trailerRe.FindSubmatch(original)
	if tm == nil {
		return nil, fmt.Errorf("no classic trailer found (normalization failed?)")
	}
	trailer := tm[1]

	sort.Slice(objects, func(i, j int) bool { return objects[i].Num < objects[j].Num })
	maxNum := objects[len(objects)-1].Num

	var out bytes.Buffer
	out.WriteString("%PDF-1.7\n%\xE2\xE3\xCF\xD3\n")

	offsets := make(map[int]int, len(objects))
	for _, o := range objects {
		offsets[o.Num] = out.Len()
		fmt.Fprintf(&out, "%d %d obj\n", o.Num, o.Gen)
		out.Write(o.Body)
		if o.HasStream {
			out.WriteString("\nstream\n")
			out.Write(o.Stream)
			out.WriteString("\nendstream")
		}
		out.WriteString("\nendobj\n")
	}

	xrefPos := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n", maxNum+1)
	out.WriteString("0000000000 65535 f \n")
	for n := 1; n <= maxNum; n++ {
		if off, ok := offsets[n]; ok {
			fmt.Fprintf(&out, "%010d %05d n \n", off, 0)
		} else {
			out.WriteString("0000000000 65535 f \n")
		}
	}

	trailer = sizeRe.ReplaceAll(trailer, []byte(fmt.Sprintf("/Size %d", maxNum+1)))
	trailer = prevRe.ReplaceAll(trailer, nil)
	out.WriteString("trailer\n")
	out.Write(trailer)
	fmt.Fprintf(&out, "\nstartxref\n%d\n%%%%EOF\n", xrefPos)
	return out.Bytes(), nil
}
