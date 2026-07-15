package pdfcore

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// QpdfTimeout bounds each qpdf subprocess call. Without this, a malicious
// or pathologically malformed upload could hang a worker (and the
// concurrency slot it holds) indefinitely.
const QpdfTimeout = 25 * time.Second

func runQpdf(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, QpdfTimeout)
	defer cancel()

	in, err := os.CreateTemp("", "pdfer-in-*.pdf")
	if err != nil {
		return nil, err
	}
	defer os.Remove(in.Name())
	out, err := os.CreateTemp("", "pdfer-out-*.pdf")
	if err != nil {
		return nil, err
	}
	defer os.Remove(out.Name())
	out.Close()

	if _, err := in.Write(input); err != nil {
		return nil, err
	}
	in.Close()

	full := append(args, in.Name(), out.Name())
	cmd := exec.CommandContext(ctx, "qpdf", full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("qpdf: timed out after %s", QpdfTimeout)
		}
		// exit code 3 == "succeeded with warnings" - acceptable
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 3 {
			return nil, fmt.Errorf("qpdf: %v: %s", err, stderr.String())
		}
	}
	return os.ReadFile(out.Name())
}

// Normalize turns any PDF into a classic PDF with uncompressed,
// text-searchable content streams and no object streams.
func Normalize(ctx context.Context, pdf []byte) ([]byte, error) {
	return runQpdf(ctx, pdf, "--object-streams=disable", "--stream-data=uncompress")
}

// Recompress shrinks the rebuilt file back down.
func Recompress(ctx context.Context, pdf []byte) []byte {
	out, err := runQpdf(ctx, pdf, "--object-streams=generate")
	if err != nil {
		return pdf // uncompressed output is still a valid PDF
	}
	return out
}
