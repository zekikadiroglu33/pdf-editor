package pdfcore

import "time"

// MaxUpload caps the size of any single PDF (or image) upload accepted by
// any endpoint in this app.
const MaxUpload = 50 << 20 // 50 MB

// MaxFieldLen caps the find/replace text fields. MaxUpload already bounds
// the whole request body, but this keeps a single request from spending
// its entire budget on megabytes of find/replace text instead of a PDF.
const MaxFieldLen = 10000

// RequestTimeout bounds a whole request pipeline (qpdf subprocess calls
// plus this app's own in-process parsing/rebuilding), so one request
// can't tie up a concurrency slot indefinitely even if something
// downstream misbehaves. Shared by every feature package (editor,
// conversion, pdfTools), not just editing - the name predates the other
// features but the budget applies equally to all of them.
const RequestTimeout = 55 * time.Second
