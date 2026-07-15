// pagerange.go - a small page-range parser shared by the tools that need
// an actual list of page indices to iterate in Go (watermark, crop,
// add-image), unlike rotate/split/delete-pages in pdftools.go, which
// just forward a range string straight through to qpdf's own page-range
// syntax.
package pdftools

import (
	"fmt"
	"strconv"
	"strings"

	"pdfer/pdfcore"
)

// parsePageSpec parses a 1-indexed, comma-separated page spec (e.g.
// "1,3,5-7") into a sorted, deduplicated list of 0-indexed page
// positions in [0,total). An empty spec (or "all", case-insensitive)
// means every page.
func parsePageSpec(spec string, total int) ([]int, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" || strings.EqualFold(spec, "all") {
		all := make([]int, total)
		for i := range all {
			all[i] = i
		}
		return all, nil
	}

	seen := make(map[int]bool)
	var pages []int
	addPage := func(n int) error {
		if n < 1 || n > total {
			return &pdfcore.BadInputError{Msg: fmt.Sprintf("page %d is out of range (this PDF has %d page(s))", n, total)}
		}
		if !seen[n-1] {
			seen[n-1] = true
			pages = append(pages, n-1)
		}
		return nil
	}

	for _, group := range strings.Split(spec, ",") {
		group = strings.TrimSpace(group)
		if group == "" {
			continue
		}
		if dash := strings.IndexByte(group, '-'); dash > 0 {
			a, errA := strconv.Atoi(strings.TrimSpace(group[:dash]))
			b, errB := strconv.Atoi(strings.TrimSpace(group[dash+1:]))
			if errA != nil || errB != nil {
				return nil, &pdfcore.BadInputError{Msg: fmt.Sprintf("invalid page range %q", group)}
			}
			if a > b {
				a, b = b, a
			}
			for n := a; n <= b; n++ {
				if err := addPage(n); err != nil {
					return nil, err
				}
			}
			continue
		}
		n, err := strconv.Atoi(group)
		if err != nil {
			return nil, &pdfcore.BadInputError{Msg: fmt.Sprintf("invalid page number %q", group)}
		}
		if err := addPage(n); err != nil {
			return nil, err
		}
	}
	if len(pages) == 0 {
		return nil, &pdfcore.BadInputError{Msg: "no pages selected"}
	}

	// addPage already appended in first-seen order, not necessarily
	// ascending (e.g. "5,1" or "5-3") - callers (drawing/cropping page by
	// page) don't care about order, but a stable ascending order makes
	// results/tests predictable.
	for i := 1; i < len(pages); i++ {
		for j := i; j > 0 && pages[j-1] > pages[j]; j-- {
			pages[j-1], pages[j] = pages[j], pages[j-1]
		}
	}
	return pages, nil
}
