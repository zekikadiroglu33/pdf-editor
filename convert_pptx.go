// convert_pptx.go - minimal .pptx (OOXML PresentationML) writer.
//
// One slide per PDF page: a single text box holding that page's paragraphs
// as separate runs, with autofit-to-shrink so PowerPoint/LibreOffice/Keynote
// reflow and scale the text themselves rather than needing pre-wrapped
// lines (unlike convert_svg.go, which has no such reader-side reflow to
// rely on). A presentation still needs one slide master, one slide layout
// and a theme even when every slide is this simple - PowerPoint's schema
// requires the reference chain to resolve, even to boilerplate.
package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
)

const pptxSlideW, pptxSlideH = 12192000, 6858000 // EMU, 16:9 widescreen (13.333in x 7.5in)

const pptxTheme = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<a:theme xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" name="pdfer">
  <a:themeElements>
    <a:clrScheme name="pdfer">
      <a:dk1><a:sysClr val="windowText" lastClr="000000"/></a:dk1>
      <a:lt1><a:sysClr val="window" lastClr="FFFFFF"/></a:lt1>
      <a:dk2><a:srgbClr val="1F1F1F"/></a:dk2>
      <a:lt2><a:srgbClr val="EEEEEE"/></a:lt2>
      <a:accent1><a:srgbClr val="3B6FE0"/></a:accent1>
      <a:accent2><a:srgbClr val="4F8CFF"/></a:accent2>
      <a:accent3><a:srgbClr val="6790FF"/></a:accent3>
      <a:accent4><a:srgbClr val="8BA9FF"/></a:accent4>
      <a:accent5><a:srgbClr val="AFC7FF"/></a:accent5>
      <a:accent6><a:srgbClr val="D3E1FF"/></a:accent6>
      <a:hlink><a:srgbClr val="3B6FE0"/></a:hlink>
      <a:folHlink><a:srgbClr val="8B5CF6"/></a:folHlink>
    </a:clrScheme>
    <a:fontScheme name="pdfer">
      <a:majorFont><a:latin typeface="Calibri"/><a:ea typeface=""/><a:cs typeface=""/></a:majorFont>
      <a:minorFont><a:latin typeface="Calibri"/><a:ea typeface=""/><a:cs typeface=""/></a:minorFont>
    </a:fontScheme>
    <a:fmtScheme name="pdfer">
      <a:fillStyleLst>
        <a:solidFill><a:schemeClr val="phClr"/></a:solidFill>
        <a:solidFill><a:schemeClr val="phClr"/></a:solidFill>
        <a:solidFill><a:schemeClr val="phClr"/></a:solidFill>
      </a:fillStyleLst>
      <a:lnStyleLst>
        <a:ln w="6350"><a:solidFill><a:schemeClr val="phClr"/></a:solidFill></a:ln>
        <a:ln w="12700"><a:solidFill><a:schemeClr val="phClr"/></a:solidFill></a:ln>
        <a:ln w="19050"><a:solidFill><a:schemeClr val="phClr"/></a:solidFill></a:ln>
      </a:lnStyleLst>
      <a:effectStyleLst>
        <a:effectStyle><a:effectLst/></a:effectStyle>
        <a:effectStyle><a:effectLst/></a:effectStyle>
        <a:effectStyle><a:effectLst/></a:effectStyle>
      </a:effectStyleLst>
      <a:bgFillStyleLst>
        <a:solidFill><a:schemeClr val="phClr"/></a:solidFill>
        <a:solidFill><a:schemeClr val="phClr"/></a:solidFill>
        <a:solidFill><a:schemeClr val="phClr"/></a:solidFill>
      </a:bgFillStyleLst>
    </a:fmtScheme>
  </a:themeElements>
</a:theme>`

const pptxSlideMaster = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:sldMaster xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main">
  <p:cSld>
    <p:spTree>
      <p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr>
      <p:grpSpPr/>
    </p:spTree>
  </p:cSld>
  <p:clrMap bg1="lt1" tx1="dk1" bg2="lt2" tx2="dk2" accent1="accent1" accent2="accent2" accent3="accent3" accent4="accent4" accent5="accent5" accent6="accent6" hlink="hlink" folHlink="folHlink"/>
  <p:sldLayoutIdLst>
    <p:sldLayoutId id="2147483649" r:id="rId1"/>
  </p:sldLayoutIdLst>
</p:sldMaster>`

const pptxSlideMasterRels = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideLayout" Target="../slideLayouts/slideLayout1.xml"/>
  <Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/theme" Target="../theme/theme1.xml"/>
</Relationships>`

const pptxSlideLayout = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:sldLayout xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" type="blank" preserve="1">
  <p:cSld name="Blank">
    <p:spTree>
      <p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr>
      <p:grpSpPr/>
    </p:spTree>
  </p:cSld>
  <p:clrMapOvr><a:masterClrMapping/></p:clrMapOvr>
</p:sldLayout>`

const pptxSlideLayoutRels = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideMaster" Target="../slideMasters/slideMaster1.xml"/>
</Relationships>`

// buildPptxSlideXML renders one slide: a single text box covering most of
// the slide, one <a:p> per paragraph (an empty page still gets one blank
// paragraph so the shape isn't left with no content at all).
func buildPptxSlideXML(paragraphs []string) string {
	var paras strings.Builder
	if len(paragraphs) == 0 {
		paras.WriteString(`<a:p><a:endParaRPr lang="en-US" sz="1800"/></a:p>`)
	}
	for _, p := range paragraphs {
		fmt.Fprintf(&paras, `<a:p><a:r><a:rPr lang="en-US" sz="1800" dirty="0"/><a:t>%s</a:t></a:r></a:p>`, xmlEscaper.Replace(p))
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:sld xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main">
  <p:cSld>
    <p:spTree>
      <p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr>
      <p:grpSpPr/>
      <p:sp>
        <p:nvSpPr>
          <p:cNvPr id="2" name="Content"/>
          <p:cNvSpPr txBox="1"/>
          <p:nvPr/>
        </p:nvSpPr>
        <p:spPr>
          <a:xfrm><a:off x="457200" y="457200"/><a:ext cx="11277600" cy="5943600"/></a:xfrm>
          <a:prstGeom prst="rect"><a:avLst/></a:prstGeom>
        </p:spPr>
        <p:txBody>
          <a:bodyPr wrap="square"><a:normAutofit/></a:bodyPr>
          <a:lstStyle/>
          %s
        </p:txBody>
      </p:sp>
    </p:spTree>
  </p:cSld>
</p:sld>`, paras.String())
}

const pptxSlideRels = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideLayout" Target="../slideLayouts/slideLayout1.xml"/>
</Relationships>`

func buildPptxFromPages(pages []pdfPage) ([]byte, error) {
	var contentTypeOverrides strings.Builder
	var presSlideIdLst strings.Builder
	var presRels strings.Builder

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	write := func(name, content string) error {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = w.Write([]byte(content))
		return err
	}

	for i, p := range pages {
		n := i + 1
		fmt.Fprintf(&contentTypeOverrides,
			`<Override PartName="/ppt/slides/slide%d.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slide+xml"/>`, n)
		fmt.Fprintf(&presSlideIdLst, `<p:sldId id="%d" r:id="rId%d"/>`, 255+n, n+1)
		fmt.Fprintf(&presRels,
			`<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide" Target="slides/slide%d.xml"/>`,
			n+1, n)

		if err := write(fmt.Sprintf("ppt/slides/slide%d.xml", n), buildPptxSlideXML(p.paragraphs)); err != nil {
			return nil, err
		}
		if err := write(fmt.Sprintf("ppt/slides/_rels/slide%d.xml.rels", n), pptxSlideRels); err != nil {
			return nil, err
		}
	}

	contentTypes := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
  <Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
  <Default Extension="xml" ContentType="application/xml"/>
  <Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/>
  <Override PartName="/ppt/slideMasters/slideMaster1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slideMaster+xml"/>
  <Override PartName="/ppt/slideLayouts/slideLayout1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slideLayout+xml"/>
  <Override PartName="/ppt/theme/theme1.xml" ContentType="application/vnd.openxmlformats-officedocument.theme+xml"/>
  ` + contentTypeOverrides.String() + `
  <Override PartName="/docProps/core.xml" ContentType="application/vnd.openxmlformats-package.core-properties+xml"/>
</Types>`

	rootRels := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="ppt/presentation.xml"/>
  <Relationship Id="rId2" Type="http://schemas.openxmlformats.org/package/2006/relationships/metadata/core-properties" Target="docProps/core.xml"/>
</Relationships>`

	presentation := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:presentation xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main">
  <p:sldMasterIdLst><p:sldMasterId id="2147483648" r:id="rId1"/></p:sldMasterIdLst>
  <p:sldIdLst>%s</p:sldIdLst>
  <p:sldSz cx="%d" cy="%d"/>
  <p:notesSz cx="6858000" cy="9144000"/>
</p:presentation>`, presSlideIdLst.String(), pptxSlideW, pptxSlideH)

	presentationRels := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideMaster" Target="slideMasters/slideMaster1.xml"/>
  ` + presRels.String() + `
</Relationships>`

	files := []struct{ name, content string }{
		{"[Content_Types].xml", contentTypes},
		{"_rels/.rels", rootRels},
		{"docProps/core.xml", oouxmlCoreProps},
		{"ppt/presentation.xml", presentation},
		{"ppt/_rels/presentation.xml.rels", presentationRels},
		{"ppt/slideMasters/slideMaster1.xml", pptxSlideMaster},
		{"ppt/slideMasters/_rels/slideMaster1.xml.rels", pptxSlideMasterRels},
		{"ppt/slideLayouts/slideLayout1.xml", pptxSlideLayout},
		{"ppt/slideLayouts/_rels/slideLayout1.xml.rels", pptxSlideLayoutRels},
		{"ppt/theme/theme1.xml", pptxTheme},
	}
	for _, f := range files {
		if err := write(f.name, f.content); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
