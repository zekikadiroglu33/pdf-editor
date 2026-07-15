// pdfimage.go - the two image-codec-backed tools: embedding an uploaded
// image onto a page (AddImageToPDF), and downsampling oversized images
// already embedded in a PDF (CompressImages). Unlike every other tool in
// this project, these need an actual image codec (Go's stdlib
// image/jpeg, image/png, image/draw), not just PDF object-model editing
// or a qpdf CLI flag - drawing/resizing pixels is unavoidable here.
package pdftools

import (
	"bytes"
	"compress/zlib"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"regexp"
	"strconv"

	"pdfer/pdfcore"
)

// ---------------------------------------------------------------------------
// Add image to PDF
// ---------------------------------------------------------------------------

func sniffImageFormat(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("\xFF\xD8\xFF")):
		return "jpeg"
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "png"
	default:
		return ""
	}
}

// buildImageXObject decodes an uploaded JPEG or PNG and returns the PDF
// object(s) needed to embed it, numbered starting at firstNum, plus the
// main Image XObject's number and its pixel width/height (for the
// page's placement matrix).
//
// JPEG passes the original bytes straight through as /DCTDecode - no
// re-encoding, so no quality loss and no size overhead. PNG has no such
// direct passthrough PDF filter that matches Go's decoder output, so it
// goes through image/png -> raw RGB bytes -> /FlateDecode instead (no
// PNG per-scanline predictor logic to get wrong, at the cost of a larger
// embedded size than a hand-optimized predictor passthrough would give -
// running the "Compress PDF" tool afterward, which re-encodes images qpdf
// recognizes, shrinks this back down). A PNG's alpha channel becomes a
// separate grayscale /SMask object.
func buildImageXObject(firstNum int, data []byte) (objs []pdfcore.PdfObject, imgObjNum, width, height int, err error) {
	switch sniffImageFormat(data) {
	case "jpeg":
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return nil, 0, 0, 0, &pdfcore.BadInputError{Msg: "could not read JPEG image: " + err.Error()}
		}
		colorSpace := "DeviceRGB"
		switch cfg.ColorModel {
		case color.GrayModel:
			colorSpace = "DeviceGray"
		case color.CMYKModel:
			return nil, 0, 0, 0, &pdfcore.BadInputError{Msg: "CMYK JPEG images aren't supported - please convert to RGB first"}
		}
		obj := pdfcore.PdfObject{
			Num: firstNum,
			Body: []byte(fmt.Sprintf(
				"<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /%s /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>",
				cfg.Width, cfg.Height, colorSpace, len(data))),
			Stream:    data,
			HasStream: true,
		}
		return []pdfcore.PdfObject{obj}, firstNum, cfg.Width, cfg.Height, nil

	case "png":
		src, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, 0, 0, 0, &pdfcore.BadInputError{Msg: "could not read PNG image: " + err.Error()}
		}
		b := src.Bounds()
		w, h := b.Dx(), b.Dy()
		nrgba := image.NewNRGBA(image.Rect(0, 0, w, h))
		draw.Draw(nrgba, nrgba.Bounds(), src, b.Min, draw.Src)

		rgb := make([]byte, w*h*3)
		alpha := make([]byte, w*h)
		hasAlpha := false
		for i := 0; i < w*h; i++ {
			r, g, bl, a := nrgba.Pix[i*4], nrgba.Pix[i*4+1], nrgba.Pix[i*4+2], nrgba.Pix[i*4+3]
			rgb[i*3], rgb[i*3+1], rgb[i*3+2] = r, g, bl
			alpha[i] = a
			if a != 255 {
				hasAlpha = true
			}
		}

		imgNum := firstNum
		var objs []pdfcore.PdfObject
		smaskRef := ""
		if hasAlpha {
			smaskNum := firstNum
			imgNum = firstNum + 1 // image object comes after its SMask so the two numbers don't collide
			var aBuf bytes.Buffer
			zw := zlib.NewWriter(&aBuf)
			zw.Write(alpha)
			zw.Close()
			objs = append(objs, pdfcore.PdfObject{
				Num: smaskNum,
				Body: []byte(fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceGray /BitsPerComponent 8 /Filter /FlateDecode /Length %d >>",
					w, h, aBuf.Len())),
				Stream:    aBuf.Bytes(),
				HasStream: true,
			})
			smaskRef = fmt.Sprintf(" /SMask %d 0 R", smaskNum)
		}

		var rgbBuf bytes.Buffer
		zw := zlib.NewWriter(&rgbBuf)
		zw.Write(rgb)
		zw.Close()
		objs = append(objs, pdfcore.PdfObject{
			Num: imgNum,
			Body: []byte(fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /FlateDecode /Length %d%s >>",
				w, h, rgbBuf.Len(), smaskRef)),
			Stream:    rgbBuf.Bytes(),
			HasStream: true,
		})
		return objs, imgNum, w, h, nil

	default:
		return nil, 0, 0, 0, &pdfcore.BadInputError{Msg: "unsupported image format - only JPEG and PNG are supported"}
	}
}

var imagePlacements = map[string]bool{"center": true, "top-left": true, "top-right": true, "bottom-left": true, "bottom-right": true}

// AddImageToPDF embeds an uploaded JPEG/PNG onto the given pages (see
// parsePageSpec; "" defaults to just page 1, unlike watermark's "every
// page" default, since stamping one photo/signature onto one page is the
// common case here), sized to widthPct of that page's width (height
// follows the image's own aspect ratio) and placed at one of five
// position presets.
func AddImageToPDF(ctx context.Context, pdf []byte, imageData []byte, pagesSpec string, widthPct float64, position string) ([]byte, error) {
	if widthPct <= 0 || widthPct > 100 {
		return nil, &pdfcore.BadInputError{Msg: "image width must be between 1 and 100 percent of the page width"}
	}
	if position == "" {
		position = "center"
	}
	if !imagePlacements[position] {
		return nil, &pdfcore.BadInputError{Msg: fmt.Sprintf("unknown position %q", position)}
	}
	if pagesSpec == "" {
		pagesSpec = "1"
	}

	norm, err := pdfcore.Normalize(ctx, pdf)
	if err != nil {
		return nil, fmt.Errorf("normalize: %w", err)
	}
	objects, err := pdfcore.ParseObjects(norm)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	objByNum := make(map[int]int, len(objects))
	maxNum := 0
	for idx, o := range objects {
		objByNum[o.Num] = idx
		if o.Num > maxNum {
			maxNum = o.Num
		}
	}

	pageOrder := pdfcore.ResolvePageOrder(objects, objByNum)
	if len(pageOrder) == 0 {
		for idx, o := range objects {
			if pdfcore.IsPageObject(o.Body) {
				pageOrder = append(pageOrder, idx)
			}
		}
	}
	if len(pageOrder) == 0 {
		return nil, fmt.Errorf("no pages found")
	}

	targets, err := parsePageSpec(pagesSpec, len(pageOrder))
	if err != nil {
		return nil, err
	}

	imgObjs, imgObjNum, imgW, imgH, err := buildImageXObject(maxNum+1, imageData)
	if err != nil {
		return nil, err
	}
	objects = append(objects, imgObjs...)
	nextNum := maxNum + len(imgObjs) + 1
	// Named after the image's own object number, not a fixed name: a
	// fixed name would collide if AddImageToPDF is ever called more than
	// once against the same growing PDF (e.g. adding two images, one
	// after another) - ensureIndirectResource's "skip if this name is
	// already present" check (needed so a Resources dict shared across
	// pages doesn't get the same entry inserted twice) would then treat
	// the second image as already registered and leave the first image's
	// mapping in place, silently drawing the wrong picture.
	imageResourceName := fmt.Sprintf("PDFerImg%d", imgObjNum)

	for _, pn := range targets {
		pageIdx := pageOrder[pn]

		resOwner, rStart, rEnd, ok := pdfcore.ResolvePageResources(objects, objByNum, pageIdx)
		if !ok {
			return nil, fmt.Errorf("page %d has no /Resources to attach the image to", pn+1)
		}
		ensureIndirectResource(objects, objByNum, resOwner, rStart, rEnd, pdfcore.XObjectKeyRe, "XObject", imageResourceName, imgObjNum)

		x0, y0, x1, y1 := resolvePageBoxCorners(objects, objByNum, pageIdx)
		pageW, pageH := x1-x0, y1-y0
		drawW := pageW * widthPct / 100
		drawH := drawW * float64(imgH) / float64(imgW)

		var x, y float64
		switch position {
		case "top-left":
			x, y = x0, y1-drawH
		case "top-right":
			x, y = x1-drawW, y1-drawH
		case "bottom-left":
			x, y = x0, y0
		case "bottom-right":
			x, y = x1-drawW, y0
		default: // center
			x, y = x0+(pageW-drawW)/2, y0+(pageH-drawH)/2
		}

		content := fmt.Sprintf("q\n%s 0 0 %s %s %s cm\n/%s Do\nQ\n",
			strconv.FormatFloat(drawW, 'f', 3, 64), strconv.FormatFloat(drawH, 'f', 3, 64),
			strconv.FormatFloat(x, 'f', 3, 64), strconv.FormatFloat(y, 'f', 3, 64),
			imageResourceName)
		streamNum := nextNum
		nextNum++
		objects = append(objects, pdfcore.PdfObject{
			Num:       streamNum,
			Body:      []byte(fmt.Sprintf("<< /Length %d >>", len(content))),
			Stream:    []byte(content),
			HasStream: true,
		})
		if err := appendPageContentStream(&objects[pageIdx], streamNum); err != nil {
			return nil, fmt.Errorf("page %d: %w", pn+1, err)
		}
	}

	rebuilt, err := pdfcore.Rebuild(objects, norm)
	if err != nil {
		return nil, fmt.Errorf("rebuild: %w", err)
	}
	return pdfcore.Recompress(ctx, rebuilt), nil
}

// ---------------------------------------------------------------------------
// Compress (downsample) embedded images
// ---------------------------------------------------------------------------

var (
	imageXObjectRe = regexp.MustCompile(`/Subtype\s*/Image\b`)
	imgWidthRe     = regexp.MustCompile(`/Width\s+(\d+)`)
	imgHeightRe    = regexp.MustCompile(`/Height\s+(\d+)`)
)

// decodeRawRGBOrGray decodes a /FlateDecode image XObject's stream this
// project's own AddImageToPDF (or another simple generator) produced:
// plain zlib-compressed rows of 8-bit DeviceRGB or DeviceGray samples,
// no PNG-style predictor. Anything else (a /DecodeParms predictor, an
// unexpected data length for the declared dimensions/colorspace) returns
// an error so the caller skips that image rather than risking a
// misinterpreted decode.
func decodeRawRGBOrGray(stream, body []byte, width, height int) (image.Image, error) {
	if bytes.Contains(body, []byte("/DecodeParms")) {
		return nil, fmt.Errorf("predictor/decode-parms not supported")
	}
	zr, err := zlib.NewReader(bytes.NewReader(stream))
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		return nil, err
	}

	gray := bytes.Contains(body, []byte("/DeviceGray"))
	components := 3
	if gray {
		components = 1
	}
	if len(raw) != width*height*components {
		return nil, fmt.Errorf("unexpected raw image data length")
	}

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for i := 0; i < width*height; i++ {
		var r, g, b byte
		if gray {
			r, g, b = raw[i], raw[i], raw[i]
		} else {
			r, g, b = raw[i*3], raw[i*3+1], raw[i*3+2]
		}
		img.Set(i%width, i/width, color.RGBA{R: r, G: g, B: b, A: 255})
	}
	return img, nil
}

// boxDownsample shrinks img so its longer side is at most maxDim,
// averaging each destination pixel over its corresponding block of
// source pixels (a box filter) - meaningfully better quality than
// nearest-neighbor for the one direction this is ever used in
// (shrinking), while staying simple to get right.
func boxDownsample(img image.Image, maxDim int) *image.RGBA {
	b := img.Bounds()
	srcW, srcH := b.Dx(), b.Dy()
	longer := srcW
	if srcH > longer {
		longer = srcH
	}
	scale := float64(maxDim) / float64(longer)
	dstW := int(float64(srcW)*scale + 0.5)
	dstH := int(float64(srcH)*scale + 0.5)
	if dstW < 1 {
		dstW = 1
	}
	if dstH < 1 {
		dstH = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	for dy := 0; dy < dstH; dy++ {
		sy0 := b.Min.Y + dy*srcH/dstH
		sy1 := b.Min.Y + (dy+1)*srcH/dstH
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for dx := 0; dx < dstW; dx++ {
			sx0 := b.Min.X + dx*srcW/dstW
			sx1 := b.Min.X + (dx+1)*srcW/dstW
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var rs, gs, bs, count uint64
			for sy := sy0; sy < sy1 && sy < b.Max.Y; sy++ {
				for sx := sx0; sx < sx1 && sx < b.Max.X; sx++ {
					r, g, bl, _ := img.At(sx, sy).RGBA()
					rs += uint64(r >> 8)
					gs += uint64(g >> 8)
					bs += uint64(bl >> 8)
					count++
				}
			}
			if count == 0 {
				count = 1
			}
			dst.Set(dx, dy, color.RGBA{R: uint8(rs / count), G: uint8(gs / count), B: uint8(bs / count), A: 255})
		}
	}
	return dst
}

// CompressImages downsamples every image XObject in pdf whose width or
// height exceeds maxDimension pixels to fit within it, re-encoding as
// JPEG at the given quality - a resolution-aware complement to the
// qpdf-backed "Compress PDF" tool (pdftools.go's CompressPDF), which can
// re-encode/re-quality images but can't reduce their pixel dimensions.
// Images already within maxDimension, images this project doesn't
// confidently understand (an unrecognized filter, a /DecodeParms
// predictor), and images with transparency (/SMask - resized without
// also resizing its mask would desync them) are left untouched.
// changedCount reports how many images were actually resized.
func CompressImages(ctx context.Context, pdf []byte, maxDimension, quality int) (data []byte, changedCount int, err error) {
	if maxDimension < 16 || maxDimension > 10000 {
		return nil, 0, &pdfcore.BadInputError{Msg: "max dimension must be between 16 and 10000 pixels"}
	}
	if quality < 1 || quality > 100 {
		return nil, 0, &pdfcore.BadInputError{Msg: "quality must be between 1 and 100"}
	}

	norm, err := pdfcore.Normalize(ctx, pdf)
	if err != nil {
		return nil, 0, fmt.Errorf("normalize: %w", err)
	}
	objects, err := pdfcore.ParseObjects(norm)
	if err != nil {
		return nil, 0, fmt.Errorf("parse: %w", err)
	}

	changed := 0
	for i := range objects {
		o := &objects[i]
		if !o.HasStream || !imageXObjectRe.Match(o.Body) || bytes.Contains(o.Body, []byte("/SMask")) {
			continue
		}
		wm, hm := imgWidthRe.FindSubmatch(o.Body), imgHeightRe.FindSubmatch(o.Body)
		if wm == nil || hm == nil {
			continue
		}
		width, _ := strconv.Atoi(string(wm[1]))
		height, _ := strconv.Atoi(string(hm[1]))
		if width <= 0 || height <= 0 || (width <= maxDimension && height <= maxDimension) {
			continue
		}

		var img image.Image
		switch {
		case bytes.Contains(o.Body, []byte("/DCTDecode")):
			img, err = jpeg.Decode(bytes.NewReader(o.Stream))
		case bytes.Contains(o.Body, []byte("/FlateDecode")):
			img, err = decodeRawRGBOrGray(o.Stream, o.Body, width, height)
		default:
			continue
		}
		if err != nil {
			continue // couldn't decode this one - leave it as-is rather than failing the whole request
		}

		resized := boxDownsample(img, maxDimension)
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, resized, &jpeg.Options{Quality: quality}); err != nil {
			continue
		}

		o.Body = []byte(fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>",
			resized.Bounds().Dx(), resized.Bounds().Dy(), buf.Len()))
		o.Stream = buf.Bytes()
		changed++
	}

	rebuilt, err := pdfcore.Rebuild(objects, norm)
	if err != nil {
		return nil, 0, fmt.Errorf("rebuild: %w", err)
	}
	return pdfcore.Recompress(ctx, rebuilt), changed, nil
}
