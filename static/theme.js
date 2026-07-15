// Shared light/dark theme toggle - used by index.html (editor),
// convert.html (converter), and tools.html (PDF Tools), each of which has
// its own #themeToggle button.
//
// Defaults to the OS preference (handled entirely in CSS via
// prefers-color-scheme), with an explicit override stored in localStorage
// once the user clicks the toggle - the override always wins over the OS
// setting on every future load, in either direction, and is shared across
// both pages since it's the same localStorage key.
const themeToggle = document.getElementById('themeToggle');
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
