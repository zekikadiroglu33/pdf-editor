pdfjsLib.GlobalWorkerOptions.workerSrc = 'vendor/pdf.worker.min.js';

const drop = document.getElementById('drop');
const fileIn = document.getElementById('file');
const go = document.getElementById('go');
const status = document.getElementById('status');
const result = document.getElementById('result');
const viewer = document.getElementById('viewer');
const viewerHint = document.getElementById('viewerHint');
const toolbar = document.getElementById('toolbar');
const zoomIn = document.getElementById('zoomIn');
const zoomOut = document.getElementById('zoomOut');
const zoomLevel = document.getElementById('zoomLevel');
const findInput = document.getElementById('find');
const MIN_SCALE = 0.5, MAX_SCALE = 5, ZOOM_STEP = 0.25;
let pdfFile = null;
let pdfDoc = null;
let scale = 1.5;
let selectedTarget = null; // { page, occurrence } for the last mouse selection, or null

const setStatus = (msg, cls = '') => { status.textContent = msg; status.className = cls; };

async function renderPages() {
  viewer.innerHTML = '';
  zoomLevel.textContent = Math.round(scale * 100) + '%';
  try {
    for (let n = 1; n <= pdfDoc.numPages; n++) {
      const page = await pdfDoc.getPage(n);
      const viewport = page.getViewport({ scale });

      const wrap = document.createElement('div');
      wrap.className = 'page-wrap';
      wrap.style.width = viewport.width + 'px';
      wrap.style.height = viewport.height + 'px';

      const canvas = document.createElement('canvas');
      canvas.width = viewport.width;
      canvas.height = viewport.height;
      wrap.appendChild(canvas);
      await page.render({ canvasContext: canvas.getContext('2d'), viewport }).promise;

      const textLayerDiv = document.createElement('div');
      textLayerDiv.className = 'textLayer';
      wrap.appendChild(textLayerDiv);

      const textContent = await page.getTextContent();
      await pdfjsLib.renderTextLayer({
        textContentSource: textContent,
        container: textLayerDiv,
        viewport,
      }).promise;

      viewer.appendChild(wrap);
    }

    viewer.classList.add('show');
    viewerHint.style.display = 'block';
    toolbar.classList.add('show');
  } catch (err) {
    setStatus('Could not render preview: ' + err.message, 'err');
  }
}

async function renderPreview(file, keepZoom = false) {
  viewer.innerHTML = '';
  viewer.classList.remove('show');
  viewerHint.style.display = 'none';
  toolbar.classList.remove('show');
  try {
    const buf = await file.arrayBuffer();
    pdfDoc = await pdfjsLib.getDocument({ data: buf }).promise;
    if (!keepZoom) scale = 1.5;
    await renderPages();
  } catch (err) {
    setStatus('Could not render preview: ' + err.message, 'err');
  }
}

function setZoom(next) {
  scale = Math.min(MAX_SCALE, Math.max(MIN_SCALE, next));
  renderPages();
}
zoomIn.addEventListener('click', () => setZoom(scale + ZOOM_STEP));
zoomOut.addEventListener('click', () => setZoom(scale - ZOOM_STEP));

// Figures out which page and which occurrence-within-that-page (0-indexed,
// reading order) a mouse selection corresponds to, so the edit can target
// exactly the word the user clicked instead of every occurrence of it in
// the document.
function locateSelectionOccurrence(rawSel) {
  if (!rawSel) return null;
  const winSel = window.getSelection();
  if (!winSel.rangeCount) return null;
  const range = winSel.getRangeAt(0);
  const startEl = range.startContainer.nodeType === 1
    ? range.startContainer : range.startContainer.parentNode;
  const pageWrap = startEl && startEl.closest('.page-wrap');
  if (!pageWrap) return null;
  const pageNum = Array.prototype.indexOf.call(viewer.children, pageWrap) + 1;
  if (pageNum < 1) return null;

  const textLayerDiv = pageWrap.querySelector('.textLayer');
  const pageText = textLayerDiv.textContent;

  // Character offset of the selection's start within pageText. Built via
  // Range.toString() (rather than manually walking text nodes and
  // comparing against range.startContainer) because the selection's
  // boundary container isn't always a text node - it can land on the
  // parent element with a child-index offset instead, depending on
  // exactly where the drag/double-click started - and a strict text-node
  // comparison silently fails to match in that case.
  let offset;
  try {
    const measure = document.createRange();
    measure.setStart(textLayerDiv, 0);
    measure.setEnd(range.startContainer, range.startOffset);
    offset = measure.toString().length;
  } catch (e) {
    return null;
  }

  let occurrence = 0, idx = 0;
  for (;;) {
    const m = pageText.indexOf(rawSel, idx);
    if (m === -1 || m >= offset) break;
    occurrence++;
    idx = m + rawSel.length;
  }
  return { page: pageNum, occurrence };
}

// Listen on the whole document, not just #viewer: a drag that starts
// inside the viewer very often *ends* outside it - past the last visible
// line, into the container's own padding, or onto whatever sits right
// below it in the page - and "mouseup" only fires on/bubbles through the
// element actually under the cursor at release time, not through
// #viewer's ancestry unless the release point happens to be a descendant
// of it. That made word selection silently do nothing whenever the mouse
// drifted a few pixels past the text on release - "sometimes I can't
// select a word". Checking where the resulting *selection* lives (rather
// than where the mouseup landed) makes this work regardless of exactly
// where the button came up.
document.addEventListener('mouseup', () => {
  const winSel = window.getSelection();
  if (!winSel || winSel.rangeCount === 0) return;
  const anchorNode = winSel.anchorNode;
  const anchorEl = anchorNode && (anchorNode.nodeType === 1 ? anchorNode : anchorNode.parentNode);
  if (!anchorEl || !anchorEl.closest || !anchorEl.closest('#viewer')) return;

  const rawSel = winSel.toString();
  const sel = rawSel.replace(/\s+/g, ' ').trim();
  if (sel) {
    // Compute the occurrence target *before* moving focus - focusing
    // findInput collapses the document's text selection in Chrome, which
    // would otherwise wipe out window.getSelection() right before we
    // read it.
    selectedTarget = locateSelectionOccurrence(rawSel);
    findInput.value = sel;
    findInput.focus();
  }
});

// Manually editing "Find text" invalidates the mouse-selection target,
// since the typed text may no longer match what was selected - falls
// back to the (still available) replace-everywhere behavior.
findInput.addEventListener('input', () => { selectedTarget = null; });

const setFile = (f, keepZoom = false) => {
  if (!f || f.type !== 'application/pdf') { setStatus('Please choose a PDF file.', 'err'); return; }
  pdfFile = f;
  selectedTarget = null;
  drop.textContent = `📄 ${f.name} (${(f.size / 1024).toFixed(1)} KB)`;
  drop.classList.add('hasfile');
  go.disabled = false;
  setStatus('');
  renderPreview(f, keepZoom);
};

drop.addEventListener('click', () => fileIn.click());
fileIn.addEventListener('change', () => setFile(fileIn.files[0]));
drop.addEventListener('dragover', e => { e.preventDefault(); drop.classList.add('over'); });
drop.addEventListener('dragleave', () => drop.classList.remove('over'));
drop.addEventListener('drop', e => {
  e.preventDefault(); drop.classList.remove('over');
  setFile(e.dataTransfer.files[0]);
});

go.addEventListener('click', async () => {
  const find = document.getElementById('find').value;
  if (!find) { setStatus('Enter the text to find.', 'err'); return; }

  const form = new FormData();
  form.append('file', pdfFile);
  form.append('find', find);
  form.append('replace', document.getElementById('replace').value);
  form.append('lang', document.getElementById('lang').value);
  const targeted = !!selectedTarget;
  if (selectedTarget) {
    form.append('page', String(selectedTarget.page));
    form.append('occurrence', String(selectedTarget.occurrence));
  }

  go.disabled = true;
  setStatus('Editing…');
  result.style.display = 'none';

  try {
    const res = await fetch('/api/edit', { method: 'POST', body: form });
    if (!res.ok) throw new Error(await res.text());

    const count = res.headers.get('X-Replacements') || '0';
    const blob = await res.blob();
    const url = URL.createObjectURL(blob);
    document.getElementById('preview').src = url;
    document.getElementById('dl').href = url;
    result.style.display = 'block';

    if (count !== '0') {
      // Continue the same session: the next edit builds on this result
      // instead of re-editing the original upload from scratch, so the
      // user can make as many edits as they want, one after another.
      const editedFile = new File([blob], 'edited.pdf', { type: 'application/pdf' });
      document.getElementById('find').value = '';
      document.getElementById('replace').value = '';
      setFile(editedFile, true);
    }

    if (count === '0') {
      const why = targeted
        ? 'The selected occurrence could not be matched — try selecting the text again.'
        : 'Some fonts encode text non-literally — see README limitations.';
      setStatus(`Done, but no matches were found. (${why})`, 'err');
    } else if (targeted) {
      setStatus('Done — replaced your selected occurrence. Select more text to keep editing.', 'ok');
    } else {
      setStatus(`Done — ${count} replacement(s) made. Select more text to keep editing.`, 'ok');
    }
  } catch (err) {
    setStatus('Error: ' + err.message, 'err');
  } finally {
    go.disabled = false;
  }
});
