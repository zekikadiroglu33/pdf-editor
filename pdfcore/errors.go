package pdfcore

// BadInputError marks errors caused by the caller's input (language,
// find/replace text, page ranges, ...) rather than by PDF parsing/rebuilding.
type BadInputError struct{ Msg string }

func (e *BadInputError) Error() string { return e.Msg }

// UnavailableError marks a request that failed only because a required
// external tool isn't installed on this server (e.g. pdftoppm/cwebp for
// raster image conversion) - a deployment/environment problem, not
// something wrong with the caller's request or the uploaded PDF.
type UnavailableError struct{ Msg string }

func (e *UnavailableError) Error() string { return e.Msg }
