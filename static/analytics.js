// Loads Google Analytics (gtag.js) only if a ga-id meta tag with a
// non-empty value is present - see main.go's GA_MEASUREMENT_ID env var
// and the GA_SNIPPET placeholder each page's <head> is rendered from.
// Kept as an external file (not an inline <script>) so the site's CSP
// can stay script-src 'self' plus GA's own domains, instead of needing
// 'unsafe-inline'.
(function () {
  const meta = document.querySelector('meta[name="ga-id"]');
  const gaID = meta && meta.content;
  if (!gaID) return;

  const s = document.createElement('script');
  s.async = true;
  s.src = 'https://www.googletagmanager.com/gtag/js?id=' + encodeURIComponent(gaID);
  document.head.appendChild(s);

  window.dataLayer = window.dataLayer || [];
  window.gtag = function () { dataLayer.push(arguments); };
  gtag('js', new Date());
  gtag('config', gaID);
})();
