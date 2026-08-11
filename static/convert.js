// PDF -> other-format converter. Its own page (convert.html), own file
// state - never shared with the editor's pdfFile/historyStack in app.js,
// since converting a document has nothing to do with a find/replace
// editing session.
const convDrop = document.getElementById('convDrop');
const convFileIn = document.getElementById('convFile');
const convGo = document.getElementById('convGo');
const convStatus = document.getElementById('convStatus');
const convDl = document.getElementById('convDl');
const convLang = document.getElementById('convLang');
// The server pre-selects this based on the request's Accept-Language header
// (see prefersTurkish in middleware.go) - a lightweight, IP-free proxy for
// "this visitor probably wants Turkish", not a locale/UI translation.
if (document.body.dataset.defaultLang === 'turkish') convLang.value = 'turkish';
const convEmptyPreview = document.getElementById('convEmptyPreview');
const convResult = document.getElementById('convResult');
const convMeta = document.getElementById('convMeta');
const convResultIcon = document.getElementById('convResultIcon');
const convResultName = document.getElementById('convResultName');
const formatOpts = document.querySelectorAll('.format-opt');
const rasterHint = document.getElementById('rasterHint');
const rasterFormats = new Set(['png', 'jpeg', 'tiff', 'webp']);
let convFile = null;
let convFormat = document.querySelector('.format-opt.active').dataset.format;

const setConvStatus = (msg, cls = '') => { convStatus.textContent = msg; convStatus.className = cls; };
const errorMsg = err => pdferT('common.errorPrefix', 'Error: ') + err.message;

// The format picker is a row of toggle buttons (not a <select>) so each
// format's icon is visible at a glance; only one can be active at a time.
formatOpts.forEach(opt => {
  opt.addEventListener('click', () => {
    formatOpts.forEach(o => o.classList.toggle('active', o === opt));
    convFormat = opt.dataset.format;
    convGo.textContent = pdferT('convert.convertToExt', 'Convert to {ext}', { ext: opt.dataset.ext });
    rasterHint.style.display = rasterFormats.has(convFormat) ? 'block' : 'none';
    convResult.style.display = 'none';
    convEmptyPreview.style.display = 'flex';
    setConvStatus('');
  });
});
// Match the format button's translated text on first paint too, not just
// after the user clicks a different format (the click handler above is the
// only other place this text is set).
convGo.textContent = pdferT('convert.convertToExt', 'Convert to {ext}', { ext: document.querySelector('.format-opt.active').dataset.ext });

const setConvFile = f => {
  if (!f || f.type !== 'application/pdf') { setConvStatus(pdferT('common.pleaseChoosePdf', 'Please choose a PDF file.'), 'err'); return; }
  convFile = f;
  convResult.style.display = 'none';
  convEmptyPreview.style.display = 'flex';
  setConvStatus('');

  // Built via DOM methods (not innerHTML) so an unusual filename can't be
  // interpreted as markup.
  convDrop.innerHTML = '';
  const icon = document.createElement('span');
  icon.className = 'drop-icon';
  icon.textContent = '📄';
  const text = document.createElement('span');
  text.className = 'drop-text';
  text.textContent = `${f.name} (${(f.size / 1024).toFixed(1)} KB)`;
  convDrop.append(icon, text);
  convDrop.classList.add('hasfile');

  convGo.disabled = false;
};

convDrop.addEventListener('click', () => convFileIn.click());
convFileIn.addEventListener('change', () => setConvFile(convFileIn.files[0]));
convDrop.addEventListener('dragover', e => { e.preventDefault(); convDrop.classList.add('over'); });
convDrop.addEventListener('dragleave', () => convDrop.classList.remove('over'));
convDrop.addEventListener('drop', e => {
  e.preventDefault(); convDrop.classList.remove('over');
  setConvFile(e.dataTransfer.files[0]);
});

convGo.addEventListener('click', async () => {
  if (!convFile) return;

  const activeOpt = document.querySelector('.format-opt.active');

  const form = new FormData();
  form.append('file', convFile);
  form.append('lang', convLang.value);
  form.append('format', convFormat);

  convGo.disabled = true;
  convResult.style.display = 'none';
  setConvStatus(pdferT('convert.converting', 'Converting…'));

  try {
    const res = await fetch('/api/convert', { method: 'POST', body: form });
    if (!res.ok) throw new Error(await res.text());

    const pages = res.headers.get('X-Pages') || '0';
    // The actual filename is read from the response, not assumed from the
    // picked format: a multi-page image conversion comes back as
    // converted_<format>.zip instead of converted.<ext> (see
    // convert_raster.go), and this way the UI never has to know that rule.
    const disposition = res.headers.get('Content-Disposition') || '';
    const filenameMatch = disposition.match(/filename="?([^";]+)"?/);
    const filename = filenameMatch ? filenameMatch[1] : `converted${activeOpt.dataset.ext}`;
    const isZip = filename.endsWith('.zip');

    const blob = await res.blob();
    convDl.href = URL.createObjectURL(blob);
    convDl.download = filename;
    convDl.textContent = `⬇ Download ${filename}`;
    convResultIcon.textContent = isZip ? '📦' : activeOpt.dataset.icon;
    convResultName.textContent = filename;
    convMeta.textContent = isZip
      ? pdferT('convert.pagesConvertedZip', '{pages} page(s) converted (one image per page, zipped)', { pages })
      : pdferT('convert.pagesConverted', '{pages} page(s) converted', { pages });
    convEmptyPreview.style.display = 'none';
    convResult.style.display = 'block';
    setConvStatus(pdferT('convert.doneConverted', 'Done — converted {pages} page(s) to {filename}.', { pages, filename }), 'ok');
  } catch (err) {
    setConvStatus(errorMsg(err), 'err');
  } finally {
    convGo.disabled = false;
  }
});
