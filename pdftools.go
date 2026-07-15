// pdftools.go - PDF page/document operations backed entirely by qpdf's own
// CLI capabilities: rotate, merge, split, delete pages, and compress.
// Unlike find/replace or the converters, none of these need this
// project's own hand-rolled PDF object parser at all - qpdf already does
// page selection, concatenation, and stream/image recompression more
// robustly than reimplementing it here would, so each function below is
// just one qpdf invocation with the right flags/page-range syntax.
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// writeTempPDF writes pdf to a fresh temp file, returning its path. The
// caller is responsible for os.Remove-ing it.
func writeTempPDF(pdf []byte, prefix string) (string, error) {
	f, err := os.CreateTemp("", prefix+"-*.pdf")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(pdf); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// newTempPDFPath reserves an output path for qpdf to write to, without
// leaving an empty file behind for qpdf to complain about.
func newTempPDFPath(prefix string) (string, error) {
	f, err := os.CreateTemp("", prefix+"-*.pdf")
	if err != nil {
		return "", err
	}
	path := f.Name()
	f.Close()
	return path, nil
}

// runQpdfRaw runs qpdf with exactly the given argv (the caller supplies
// input/output paths and flags in whatever order that operation needs -
// unlike runQpdf in main.go, which always appends "in out" itself), under
// the same qpdfTimeout budget every other qpdf call in this app uses.
// qpdf's own stderr is surfaced close to verbatim, since these operations'
// most likely failure mode is the caller's own page-range/argument syntax.
func runQpdfRaw(ctx context.Context, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, qpdfTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "qpdf", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("qpdf: timed out after %s", qpdfTimeout)
		}
		// exit code 3 == "succeeded with warnings" - acceptable, same as
		// runQpdf in main.go.
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 3 {
			return fmt.Errorf("qpdf: %s", strings.TrimSpace(stderr.String()))
		}
	}
	return nil
}

// pdfPageCount returns pdf's page count via qpdf --show-npages - used by
// SplitPDF to default to "one file per page" when the caller doesn't
// supply explicit ranges.
func pdfPageCount(ctx context.Context, pdf []byte) (int, error) {
	in, err := writeTempPDF(pdf, "pdfer-count-in")
	if err != nil {
		return 0, err
	}
	defer os.Remove(in)

	cctx, cancel := context.WithTimeout(ctx, qpdfTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "qpdf", "--show-npages", in)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("qpdf --show-npages: %s", strings.TrimSpace(stderr.String()))
	}
	n, err := strconv.Atoi(strings.TrimSpace(out.String()))
	if err != nil {
		return 0, fmt.Errorf("qpdf --show-npages: unexpected output %q", out.String())
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Rotate
// ---------------------------------------------------------------------------

// RotatePDF rotates pages (a qpdf page-range string; "" means every page)
// by angle degrees relative to their current rotation - angle's sign
// controls direction (e.g. +90 clockwise, -90 counterclockwise) per
// qpdf's own --rotate convention, and must be a multiple of 90.
func RotatePDF(ctx context.Context, pdf []byte, pages string, angle int) ([]byte, error) {
	if angle == 0 || angle%90 != 0 {
		return nil, &badInputError{"rotation angle must be a non-zero multiple of 90"}
	}
	if pages == "" {
		pages = "1-z"
	}

	in, err := writeTempPDF(pdf, "pdfer-rotate-in")
	if err != nil {
		return nil, err
	}
	defer os.Remove(in)
	outPath, err := newTempPDFPath("pdfer-rotate-out")
	if err != nil {
		return nil, err
	}
	defer os.Remove(outPath)

	rotateArg := fmt.Sprintf("--rotate=%+d:%s", angle, pages)
	if err := runQpdfRaw(ctx, rotateArg, in, outPath); err != nil {
		return nil, &badInputError{err.Error()}
	}
	return os.ReadFile(outPath)
}

// ---------------------------------------------------------------------------
// Merge
// ---------------------------------------------------------------------------

// MergePDFs concatenates the given PDFs' pages, in order, into one file.
func MergePDFs(ctx context.Context, pdfs [][]byte) ([]byte, error) {
	if len(pdfs) < 2 {
		return nil, &badInputError{"merge needs at least 2 PDF files"}
	}

	var paths []string
	defer func() {
		for _, p := range paths {
			os.Remove(p)
		}
	}()
	for i, p := range pdfs {
		path, err := writeTempPDF(p, fmt.Sprintf("pdfer-merge-in%d", i))
		if err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}

	outPath, err := newTempPDFPath("pdfer-merge-out")
	if err != nil {
		return nil, err
	}
	defer os.Remove(outPath)

	args := append([]string{"--empty", "--pages"}, paths...)
	args = append(args, "--", outPath)
	if err := runQpdfRaw(ctx, args...); err != nil {
		return nil, &badInputError{err.Error()}
	}
	return os.ReadFile(outPath)
}

// ---------------------------------------------------------------------------
// Split
// ---------------------------------------------------------------------------

// SplitPDF splits pdf into one output file per entry in ranges (each a
// qpdf page-range string, e.g. "1-3"). If ranges is empty, it defaults to
// one file per individual page - the common "split every page out" case.
// The caller (handleSplit) packages a single result directly or zips
// several together, the same pattern convert_raster.go already uses for
// multi-page image output.
func SplitPDF(ctx context.Context, pdf []byte, ranges []string) ([][]byte, error) {
	in, err := writeTempPDF(pdf, "pdfer-split-in")
	if err != nil {
		return nil, err
	}
	defer os.Remove(in)

	if len(ranges) == 0 {
		n, err := pdfPageCount(ctx, pdf)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, fmt.Errorf("no pages found")
		}
		ranges = make([]string, n)
		for i := range ranges {
			ranges[i] = strconv.Itoa(i + 1)
		}
	}

	results := make([][]byte, len(ranges))
	for i, r := range ranges {
		outPath, err := newTempPDFPath("pdfer-split-out")
		if err != nil {
			return nil, err
		}
		err = runQpdfRaw(ctx, in, "--pages", ".", r, "--", outPath)
		if err == nil {
			results[i], err = os.ReadFile(outPath)
		}
		os.Remove(outPath)
		if err != nil {
			return nil, &badInputError{fmt.Sprintf("range %q: %s", r, err.Error())}
		}
	}
	return results, nil
}

// ---------------------------------------------------------------------------
// Delete pages
// ---------------------------------------------------------------------------

// DeletePages removes pagesToDelete (a qpdf page-range string of pages to
// remove, e.g. "2,4-5") from pdf, keeping the rest in their original
// order. Built as "keep everything except that range" via qpdf's own
// x-prefix exclusion syntax (see qpdf --help=page-ranges) rather than
// this app computing the complement itself.
func DeletePages(ctx context.Context, pdf []byte, pagesToDelete string) ([]byte, error) {
	if strings.TrimSpace(pagesToDelete) == "" {
		return nil, &badInputError{"specify at least one page to delete"}
	}

	in, err := writeTempPDF(pdf, "pdfer-delete-in")
	if err != nil {
		return nil, err
	}
	defer os.Remove(in)
	outPath, err := newTempPDFPath("pdfer-delete-out")
	if err != nil {
		return nil, err
	}
	defer os.Remove(outPath)

	keepRange := fmt.Sprintf("1-z,x%s", pagesToDelete)
	if err := runQpdfRaw(ctx, in, "--pages", ".", keepRange, "--", outPath); err != nil {
		return nil, &badInputError{err.Error()}
	}
	return os.ReadFile(outPath)
}

// ---------------------------------------------------------------------------
// Compress
// ---------------------------------------------------------------------------

// CompressPDF recompresses pdf's content streams and (lossily)
// re-encodes its images to reduce file size, entirely via qpdf's own
// optimization flags. jpegQuality is 1-100, or 0 to leave qpdf's default
// quality choice alone.
func CompressPDF(ctx context.Context, pdf []byte, jpegQuality int) ([]byte, error) {
	if jpegQuality < 0 || jpegQuality > 100 {
		return nil, &badInputError{"jpeg quality must be between 1 and 100"}
	}

	in, err := writeTempPDF(pdf, "pdfer-compress-in")
	if err != nil {
		return nil, err
	}
	defer os.Remove(in)
	outPath, err := newTempPDFPath("pdfer-compress-out")
	if err != nil {
		return nil, err
	}
	defer os.Remove(outPath)

	args := []string{
		"--compress-streams=y",
		"--recompress-flate",
		"--compression-level=9",
		"--object-streams=generate",
		"--optimize-images",
	}
	if jpegQuality > 0 {
		args = append(args, fmt.Sprintf("--jpeg-quality=%d", jpegQuality))
	}
	args = append(args, in, outPath)
	if err := runQpdfRaw(ctx, args...); err != nil {
		return nil, &badInputError{err.Error()}
	}
	return os.ReadFile(outPath)
}
