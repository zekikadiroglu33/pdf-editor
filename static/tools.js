// PDF Tools: rotate, merge, split, delete pages, compress - all backed by
// qpdf's own CLI capabilities (see pdftools.go), not this app's own PDF
// parser. A separate module from the Edit/Preview flow in app.js above:
// its own file state, never shared with pdfFile/historyStack, since these
// are one-shot document operations rather than a find/replace session.
const toolOpts = document.querySelectorAll('#toolPicker .format-opt');
const toolDrop = document.getElementById('toolDrop');
const toolFileIn = document.getElementById('toolFile');
const mergeDrop = document.getElementById('mergeDrop');
const mergeFilesIn = document.getElementById('mergeFiles');
const mergeFileList = document.getElementById('mergeFileList');
const rotateOpts = document.getElementById('rotateOpts');
const splitOpts = document.getElementById('splitOpts');
const deleteOpts = document.getElementById('deleteOpts');
const compressOpts = document.getElementById('compressOpts');
const rotateAngle = document.getElementById('rotateAngle');
const rotatePages = document.getElementById('rotatePages');
const splitRanges = document.getElementById('splitRanges');
const deletePages = document.getElementById('deletePages');
const compressQuality = document.getElementById('compressQuality');
const toolGo = document.getElementById('toolGo');
const toolStatus = document.getElementById('toolStatus');
const toolEmptyPreview = document.getElementById('toolEmptyPreview');
const toolResult = document.getElementById('toolResult');
const toolResultIcon = document.getElementById('toolResultIcon');
const toolResultName = document.getElementById('toolResultName');
const toolMeta = document.getElementById('toolMeta');
const toolDl = document.getElementById('toolDl');

const TOOL_LABELS = {
  rotate: 'Rotate PDF',
  merge: 'Merge PDFs',
  split: 'Split PDF',
  delete: 'Delete pages',
  compress: 'Compress PDF',
};

let currentTool = 'rotate';
let toolFile = null;
let mergeFiles = [];

const setToolStatus = (msg, cls = '') => { toolStatus.textContent = msg; toolStatus.className = cls; };

function hideToolResult() {
  toolResult.style.display = 'none';
  toolEmptyPreview.style.display = 'flex';
}

function updateToolGoState() {
  toolGo.disabled = currentTool === 'merge' ? mergeFiles.length < 2 : !toolFile;
}

function selectTool(tool) {
  currentTool = tool;
  toolOpts.forEach(o => o.classList.toggle('active', o.dataset.tool === tool));
  toolGo.textContent = TOOL_LABELS[tool];

  const isMerge = tool === 'merge';
  toolDrop.hidden = isMerge;
  mergeDrop.hidden = !isMerge;
  rotateOpts.hidden = tool !== 'rotate';
  splitOpts.hidden = tool !== 'split';
  deleteOpts.hidden = tool !== 'delete';
  compressOpts.hidden = tool !== 'compress';

  hideToolResult();
  setToolStatus('');
  updateToolGoState();
}
toolOpts.forEach(opt => opt.addEventListener('click', () => selectTool(opt.dataset.tool)));
selectTool('rotate');

// Single-file drop, shared by rotate/split/delete/compress (only one of
// which is visible at a time - see selectTool above).
const setToolFile = f => {
  if (!f || f.type !== 'application/pdf') { setToolStatus('Please choose a PDF file.', 'err'); return; }
  toolFile = f;
  hideToolResult();
  setToolStatus('');

  toolDrop.innerHTML = '';
  const icon = document.createElement('span');
  icon.className = 'drop-icon';
  icon.textContent = '📄';
  const text = document.createElement('span');
  text.className = 'drop-text';
  text.textContent = `${f.name} (${(f.size / 1024).toFixed(1)} KB)`;
  toolDrop.append(icon, text);
  toolDrop.classList.add('hasfile');

  updateToolGoState();
};
toolDrop.addEventListener('click', () => toolFileIn.click());
toolFileIn.addEventListener('change', () => setToolFile(toolFileIn.files[0]));
toolDrop.addEventListener('dragover', e => { e.preventDefault(); toolDrop.classList.add('over'); });
toolDrop.addEventListener('dragleave', () => toolDrop.classList.remove('over'));
toolDrop.addEventListener('drop', e => {
  e.preventDefault(); toolDrop.classList.remove('over');
  setToolFile(e.dataTransfer.files[0]);
});

// Multi-file drop, merge only.
const setMergeFiles = fileList => {
  const files = Array.from(fileList).filter(f => f.type === 'application/pdf');
  if (files.length === 0) { setToolStatus('Please choose PDF files.', 'err'); return; }
  mergeFiles = files;
  hideToolResult();
  setToolStatus('');

  mergeDrop.innerHTML = '';
  const icon = document.createElement('span');
  icon.className = 'drop-icon';
  icon.textContent = '📄';
  const text = document.createElement('span');
  text.className = 'drop-text';
  text.textContent = `${files.length} file(s) selected`;
  mergeDrop.append(icon, text);
  mergeDrop.classList.add('hasfile');
  mergeFileList.textContent = files.length < 2
    ? 'Select at least 2 files to merge.'
    : files.map(f => f.name).join(', ');

  updateToolGoState();
};
mergeDrop.addEventListener('click', () => mergeFilesIn.click());
mergeFilesIn.addEventListener('change', () => setMergeFiles(mergeFilesIn.files));
mergeDrop.addEventListener('dragover', e => { e.preventDefault(); mergeDrop.classList.add('over'); });
mergeDrop.addEventListener('dragleave', () => mergeDrop.classList.remove('over'));
mergeDrop.addEventListener('drop', e => {
  e.preventDefault(); mergeDrop.classList.remove('over');
  setMergeFiles(e.dataTransfer.files);
});

toolGo.addEventListener('click', async () => {
  const form = new FormData();
  let url;

  if (currentTool === 'merge') {
    mergeFiles.forEach(f => form.append('files', f));
    url = '/api/merge';
  } else {
    form.append('file', toolFile);
    if (currentTool === 'rotate') {
      form.append('angle', rotateAngle.value);
      if (rotatePages.value.trim()) form.append('pages', rotatePages.value.trim());
      url = '/api/rotate';
    } else if (currentTool === 'split') {
      if (splitRanges.value.trim()) form.append('ranges', splitRanges.value.trim());
      url = '/api/split';
    } else if (currentTool === 'delete') {
      if (!deletePages.value.trim()) { setToolStatus('Enter at least one page to delete.', 'err'); return; }
      form.append('pages', deletePages.value.trim());
      url = '/api/delete-pages';
    } else if (currentTool === 'compress') {
      if (compressQuality.value.trim()) form.append('quality', compressQuality.value.trim());
      url = '/api/compress';
    }
  }

  toolGo.disabled = true;
  hideToolResult();
  setToolStatus('Processing…');

  try {
    const res = await fetch(url, { method: 'POST', body: form });
    if (!res.ok) throw new Error(await res.text());

    const disposition = res.headers.get('Content-Disposition') || '';
    const filenameMatch = disposition.match(/filename="?([^";]+)"?/);
    const filename = filenameMatch ? filenameMatch[1] : 'result.pdf';
    const isZip = filename.endsWith('.zip');
    const parts = res.headers.get('X-Parts');
    const origSize = Number(res.headers.get('X-Original-Size'));
    const compSize = Number(res.headers.get('X-Compressed-Size'));

    const blob = await res.blob();
    toolDl.href = URL.createObjectURL(blob);
    toolDl.download = filename;
    toolDl.textContent = `⬇ Download ${filename}`;
    toolResultIcon.textContent = isZip ? '📦' : '📄';
    toolResultName.textContent = filename;

    if (parts) {
      toolMeta.textContent = `${parts} file(s), zipped`;
    } else if (origSize && compSize) {
      const pct = Math.round((1 - compSize / origSize) * 100);
      toolMeta.textContent = `${(origSize / 1024).toFixed(1)} KB → ${(compSize / 1024).toFixed(1)} KB`
        + (pct > 0 ? ` (${pct}% smaller)` : '');
    } else {
      toolMeta.textContent = 'Ready to download';
    }

    toolEmptyPreview.style.display = 'none';
    toolResult.style.display = 'block';
    setToolStatus(`Done — ${filename} is ready.`, 'ok');
  } catch (err) {
    setToolStatus('Error: ' + err.message, 'err');
  } finally {
    updateToolGoState();
  }
});
