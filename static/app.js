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
const undoBtn = document.getElementById('undo');
const findInput = document.getElementById('find');
const regexCheckbox = document.getElementById('regex');
const confirmRow = document.getElementById('confirmRow');
const confirmBtn = document.getElementById('confirmBtn');
const pendingCount = document.getElementById('pendingCount');
const emptyPreview = document.getElementById('emptyPreview');
const themeToggle = document.getElementById('themeToggle');
const tabs = document.querySelectorAll('.tab');
const MIN_SCALE = 0.5, MAX_SCALE = 5, ZOOM_STEP = 0.25;
const MAX_HISTORY = 5; // capped so undo can't grow unbounded memory on large PDFs
let pdfFile = null;
let pdfDoc = null;
let scale = 1.5;
let selectedTarget = null; // { page, occurrence } for the last mouse selection, or null
// historyStack holds, for each edit applied this session, the state from
// just *before* that edit: the File it started from and a copy of
// sessionEdits as it stood at that point - so undo can restore both
// together and highlighting stays consistent with what's on screen.
let historyStack = [];
// sessionEdits tracks every completed edit's replacement text (and which
// page it landed on, or 0 for "anywhere") purely so the preview can
// highlight it for review - it's never sent to the server.
let sessionEdits = [];

// Theme: defaults to the OS preference (handled entirely in CSS via
// prefers-color-scheme), with an explicit override stored in localStorage
// once the user clicks the toggle - the override always wins over the OS
// setting on every future load, in either direction.
const THEME_KEY = 'pdfer-theme';
function applyTheme(theme) {
  document.documentElement.setAttribute('data-theme', theme);
  themeToggle.textContent = theme === 'dark' ? '☀️' : '🌙';
  themeToggle.setAttribute('aria-label', theme === 'dark' ? 'Switch to light theme' : 'Switch to dark theme');
}
(function initTheme() {
  const stored = localStorage.getItem(THEME_KEY);
  if (stored === 'light' || stored === 'dark') { applyTheme(stored); return; }
  applyTheme(window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');
})();
themeToggle.addEventListener('click', () => {
  const next = document.documentElement.getAttribute('data-theme') === 'dark' ? 'light' : 'dark';
  localStorage.setItem(THEME_KEY, next);
  applyTheme(next);
});

// Edit and Convert are two independent tools sharing one page; only one
// panel is shown at a time so the layout stays focused instead of stacking
// both tools' cards on top of each other.
tabs.forEach(tab => {
  tab.addEventListener('click', () => {
    tabs.forEach(t => {
      t.classList.toggle('active', t === tab);
      t.setAttribute('aria-selected', t === tab ? 'true' : 'false');
    });
    document.querySelectorAll('.panel').forEach(p => {
      p.hidden = p.id !== tab.dataset.panel;
    });
  });
});

function updateUndoButton() {
  undoBtn.disabled = historyStack.length === 0;
}

// The Download/preview panel (#result) always reflects a snapshot the user
// explicitly confirmed - any edit or undo after that hides it again, so a
// stale file can never be handed out silently instead of the current one.
function updateConfirmUI() {
  result.style.display = 'none';
  if (sessionEdits.length > 0) {
    confirmRow.classList.add('show');
    pendingCount.textContent = `${sessionEdits.length} change(s) made this session — review the highlights above, then:`;
  } else {
    confirmRow.classList.remove('show');
  }
}

confirmBtn.addEventListener('click', () => {
  const url = URL.createObjectURL(pdfFile);
  document.getElementById('preview').src = url;
  document.getElementById('dl').href = url;
  result.style.display = 'block';
});

undoBtn.addEventListener('click', () => {
  if (!historyStack.length) return;
  const prevEntry = historyStack.pop();
  updateUndoButton();
  sessionEdits = prevEntry.edits;
  // setFile() itself clears #status/#result as part of loading the file, so
  // the "Reverted" message must be set after it returns, not before.
  setFile(prevEntry.file, true);
  setStatus('Reverted to the previous version.', 'ok');
});

const setStatus = (msg, cls = '') => { status.textContent = msg; status.className = cls; };

// Marks every rendered text-layer span on this page whose content contains
// one of this session's tracked replacement texts, so all the edits made so
// far - not just the last one - stay visible for review as the user keeps
// editing, zooms, or undoes. This is a review aid layered on the PDF.js text
// layer, not something written into the PDF itself: it can occasionally
// highlight a pre-existing, unrelated occurrence of the same text (there's
// no per-edit position tracking to disambiguate), but it's a good-enough
// approximation for "did my edits land where I expected".
function applyHighlights(pageNum, textLayerDiv) {
  if (sessionEdits.length === 0) return;
  const edits = sessionEdits.filter(e => e.page === 0 || e.page === pageNum);
  if (edits.length === 0) return;
  for (const span of textLayerDiv.querySelectorAll('span')) {
    const text = span.textContent;
    if (text && edits.some(e => text.includes(e.text))) {
      span.classList.add('pdfer-highlight');
    }
  }
}

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

      applyHighlights(n, textLayerDiv);
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

// Counts how many times target already appears in pageText before offset,
// giving the 0-indexed occurrence a match starting at (or after) offset
// would be - shared by drag-to-select (locateSelectionOccurrence, from a
// live browser Selection) and click-to-edit (startInlineEdit, from a
// clicked span's position) since both need the same "which occurrence is
// this" answer, just starting from a different kind of position.
function occurrenceAtOffset(pageText, target, offset) {
  let occurrence = 0, idx = 0;
  for (;;) {
    const m = pageText.indexOf(target, idx);
    if (m === -1 || m >= offset) break;
    occurrence++;
    idx = m + target.length;
  }
  return occurrence;
}

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

  return { page: pageNum, occurrence: occurrenceAtOffset(pageText, rawSel, offset) };
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
// Set whenever this mouseup handler acts on a real drag-selection, so the
// click handler right after it (see below) can tell "the user dragged to
// select text" apart from "the user just clicked once" - which it
// otherwise couldn't, since focusing findInput below collapses the
// browser's text selection before the click handler ever runs.
let justDragSelected = false;

document.addEventListener('mouseup', () => {
  // Occurrence targeting counts matches by plain substring search (see
  // locateSelectionOccurrence below), which doesn't correspond to how a
  // regex pattern matches - so drag-to-select is a literal-find-only
  // convenience and is skipped while regex mode is on.
  if (regexCheckbox.checked) return;
  const winSel = window.getSelection();
  if (!winSel || winSel.rangeCount === 0) return;
  const anchorNode = winSel.anchorNode;
  const anchorEl = anchorNode && (anchorNode.nodeType === 1 ? anchorNode : anchorNode.parentNode);
  if (!anchorEl || !anchorEl.closest || !anchorEl.closest('#viewer')) return;

  const rawSel = winSel.toString();
  const sel = rawSel.replace(/\s+/g, ' ').trim();
  if (sel) {
    justDragSelected = true;
    // Compute the occurrence target *before* moving focus - focusing
    // findInput collapses the document's text selection in Chrome, which
    // would otherwise wipe out window.getSelection() right before we
    // read it.
    selectedTarget = locateSelectionOccurrence(rawSel);
    findInput.value = sel;
    findInput.focus();
  }
});

// --- Click-to-edit: click text directly on the rendered page to retype it
// in place, Acrobat-style. This is a workflow shortcut over the same
// single-occurrence /api/edit call the drag-to-select flow above already
// makes - there's no live reflow (see README limitations: replacement text
// still sits at the original glyph position), just a nicer, more direct
// way to trigger it than selecting text and retyping it in the sidebar.
let activeInlineEdit = null; // { span, onKeydown, onBlur } for the span currently being edited, or null

function endInlineEdit(span) {
  if (!activeInlineEdit || activeInlineEdit.span !== span) return;
  span.removeEventListener('keydown', activeInlineEdit.onKeydown);
  span.removeEventListener('blur', activeInlineEdit.onBlur);
  activeInlineEdit = null;
  span.contentEditable = 'false';
  span.classList.remove('editing');
}

async function commitInlineEdit(span, pageNum, originalText, occurrence, cancelled) {
  endInlineEdit(span);
  if (cancelled) {
    span.textContent = originalText;
    return;
  }
  const newText = span.textContent.replace(/\s+/g, ' ').trim();
  if (!newText || newText === originalText) {
    span.textContent = originalText; // no-op or whitespace-only edit
    return;
  }

  setStatus('Editing…');
  go.disabled = true;
  try {
    const form = new FormData();
    form.append('file', pdfFile);
    form.append('find', originalText);
    form.append('replace', newText);
    form.append('lang', document.getElementById('lang').value);
    form.append('page', String(pageNum));
    form.append('occurrence', String(occurrence));

    const res = await fetch('/api/edit', { method: 'POST', body: form });
    if (!res.ok) throw new Error(await res.text());

    const count = res.headers.get('X-Replacements') || '0';
    const blob = await res.blob();

    if (count === '0') {
      span.textContent = originalText;
      setStatus('Done, but no matches were found. (Try clicking the text again.)', 'err');
      return;
    }

    historyStack.push({ file: pdfFile, edits: sessionEdits });
    if (historyStack.length > MAX_HISTORY) historyStack.shift();
    updateUndoButton();
    sessionEdits = [...sessionEdits, { page: pageNum, text: newText }];

    const editedFile = new File([blob], 'edited.pdf', { type: 'application/pdf' });
    setFile(editedFile, true);
    setStatus('Done — inline edit applied. Click more text to keep editing.', 'ok');
  } catch (err) {
    span.textContent = originalText;
    setStatus('Error: ' + err.message, 'err');
  } finally {
    go.disabled = false;
  }
}

function startInlineEdit(span, pageNum, pageText) {
  if (activeInlineEdit) {
    // Switching targets mid-edit: commit/discard whatever was being typed
    // into the previous span first, so at most one is ever editable.
    const prev = activeInlineEdit;
    commitInlineEdit(prev.span, prev.pageNum, prev.originalText, prev.occurrence, false);
  }

  const originalText = span.textContent;
  if (!originalText || !originalText.trim()) return;

  const textLayerDiv = span.closest('.textLayer');
  const range = document.createRange();
  range.setStart(textLayerDiv, 0);
  range.setEndBefore(span);
  const occurrence = occurrenceAtOffset(pageText, originalText, range.toString().length);

  let cancelled = false;
  const onKeydown = e => {
    if (e.key === 'Enter') {
      e.preventDefault();
      span.blur();
    } else if (e.key === 'Escape') {
      e.preventDefault();
      cancelled = true;
      span.blur();
    }
  };
  const onBlur = () => commitInlineEdit(span, pageNum, originalText, occurrence, cancelled);

  span.contentEditable = 'true';
  span.classList.add('editing');
  span.addEventListener('keydown', onKeydown);
  span.addEventListener('blur', onBlur);
  activeInlineEdit = { span, pageNum, originalText, occurrence, onKeydown, onBlur };

  span.focus();
  // Select the run's existing text so typing immediately replaces it,
  // matching how a normal text input behaves when you click into it.
  const selRange = document.createRange();
  selRange.selectNodeContents(span);
  const winSel = window.getSelection();
  winSel.removeAllRanges();
  winSel.addRange(selRange);
}

viewer.addEventListener('click', e => {
  if (justDragSelected) { justDragSelected = false; return; }
  if (regexCheckbox.checked || !pdfFile || go.disabled) return;

  const span = e.target.closest('.textLayer span');
  if (!span || span.isContentEditable) return;

  const pageWrap = span.closest('.page-wrap');
  const pageNum = Array.prototype.indexOf.call(viewer.children, pageWrap) + 1;
  if (pageNum < 1) return;

  startInlineEdit(span, pageNum, pageWrap.querySelector('.textLayer').textContent);
});

// Manually editing "Find text" invalidates the mouse-selection target,
// since the typed text may no longer match what was selected - falls
// back to the (still available) replace-everywhere behavior.
findInput.addEventListener('input', () => { selectedTarget = null; });

// Switching into regex mode invalidates any earlier mouse-selection target
// for the same reason (see the mouseup listener above).
regexCheckbox.addEventListener('change', () => { selectedTarget = null; });

const setFile = (f, keepZoom = false) => {
  if (!f || f.type !== 'application/pdf') { setStatus('Please choose a PDF file.', 'err'); return; }
  pdfFile = f;
  selectedTarget = null;
  // keepZoom is only ever true for a programmatic transition within the
  // same editing session (a chained edit's result, or an undo) - a fresh
  // upload (keepZoom=false) starts a new session, so past versions no
  // longer apply.
  if (!keepZoom) {
    historyStack = [];
    sessionEdits = [];
    updateUndoButton();
  }
  updateConfirmUI(); // also hides the (now stale) #result panel
  emptyPreview.style.display = 'none';

  // Built via DOM methods (not innerHTML) so an unusual filename can't be
  // interpreted as markup.
  drop.innerHTML = '';
  const icon = document.createElement('span');
  icon.className = 'drop-icon';
  icon.textContent = '📄';
  const text = document.createElement('span');
  text.className = 'drop-text';
  text.textContent = `${f.name} (${(f.size / 1024).toFixed(1)} KB)`;
  drop.append(icon, text);
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

  const replaceValue = document.getElementById('replace').value;
  const form = new FormData();
  form.append('file', pdfFile);
  form.append('find', find);
  form.append('replace', replaceValue);
  form.append('lang', document.getElementById('lang').value);
  if (regexCheckbox.checked) form.append('regex', 'true');
  const targeted = !!selectedTarget;
  if (selectedTarget) {
    form.append('page', String(selectedTarget.page));
    form.append('occurrence', String(selectedTarget.occurrence));
  }

  go.disabled = true;
  setStatus('Editing…');

  try {
    const res = await fetch('/api/edit', { method: 'POST', body: form });
    if (!res.ok) throw new Error(await res.text());

    const count = res.headers.get('X-Replacements') || '0';
    const blob = await res.blob();

    if (count !== '0') {
      // Continue the same session: the next edit builds on this result
      // instead of re-editing the original upload from scratch, so the
      // user can make as many edits as they want, one after another,
      // without the page or the download panel updating on every single
      // edit - only the interactive preview above (with highlights) does,
      // and #result/#dl only reflect a state the user explicitly confirmed
      // via the "Confirm changes" button.
      // Push the pre-edit state onto the undo history first, capped so a
      // long chained-editing session can't grow this unbounded.
      historyStack.push({ file: pdfFile, edits: sessionEdits });
      if (historyStack.length > MAX_HISTORY) historyStack.shift();
      updateUndoButton();

      // Track this edit's replacement text for the review highlight, one
      // entry per completed edit. Regex mode is skipped: the actual
      // replacement text differs per match (capture groups), so there's no
      // single fixed string to search the rendered preview for.
      if (!regexCheckbox.checked && replaceValue) {
        sessionEdits = [...sessionEdits, { page: targeted ? selectedTarget.page : 0, text: replaceValue }];
      }
      // setFile() below calls updateConfirmUI() itself, using the
      // sessionEdits value just set above.

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

// --- PDF -> Word converter -------------------------------------------------
// A separate module from the Edit/Preview flow above: its own file (never
// shared with pdfFile/historyStack), its own drop zone, and its own status
// line, since converting a document has nothing to do with the find/replace
// editing session.
const convDrop = document.getElementById('convDrop');
const convFileIn = document.getElementById('convFile');
const convGo = document.getElementById('convGo');
const convStatus = document.getElementById('convStatus');
const convDl = document.getElementById('convDl');
const convLang = document.getElementById('convLang');
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

// The format picker is a row of toggle buttons (not a <select>) so each
// format's icon is visible at a glance; only one can be active at a time.
formatOpts.forEach(opt => {
  opt.addEventListener('click', () => {
    formatOpts.forEach(o => o.classList.toggle('active', o === opt));
    convFormat = opt.dataset.format;
    convGo.textContent = `Convert to ${opt.dataset.ext}`;
    rasterHint.style.display = rasterFormats.has(convFormat) ? 'block' : 'none';
    convResult.style.display = 'none';
    convEmptyPreview.style.display = 'flex';
    setConvStatus('');
  });
});

const setConvFile = f => {
  if (!f || f.type !== 'application/pdf') { setConvStatus('Please choose a PDF file.', 'err'); return; }
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
  setConvStatus('Converting…');

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
      ? `${pages} page(s) converted (one image per page, zipped)`
      : `${pages} page(s) converted`;
    convEmptyPreview.style.display = 'none';
    convResult.style.display = 'block';
    setConvStatus(`Done — converted ${pages} page(s) to ${filename}.`, 'ok');
  } catch (err) {
    setConvStatus('Error: ' + err.message, 'err');
  } finally {
    convGo.disabled = false;
  }
});
