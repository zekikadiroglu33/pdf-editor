// Loads Google Analytics (gtag.js). Entirely a static-asset concern -
// every page includes this file unconditionally (like theme.js), and
// the measurement ID is a constant right here, not server-injected -
// see main.go's staticHandler, which no longer knows anything about GA.
// Kept as an external file (not an inline <script>) so the site's CSP
// can stay script-src 'self' plus GA's own domains, instead of needing
// 'unsafe-inline'.
(function () {
  const gaID = 'G-KFDD0LK8JH';

  // Skip on localhost so local dev/test runs never send hits to GA.
  if (location.hostname === 'localhost' || location.hostname === '127.0.0.1') return;

  const s = document.createElement('script');
  s.async = true;
  s.src = 'https://www.googletagmanager.com/gtag/js?id=' + encodeURIComponent(gaID);
  document.head.appendChild(s);

  window.dataLayer = window.dataLayer || [];
  window.gtag = function () { dataLayer.push(arguments); };
  gtag('js', new Date());
  // Explicitly off (not left to the GA4 property's own Admin > Data
  // Settings toggle) - keeps Google Signals (cross-device reporting,
  // remarketing audiences, ads personalization) disabled regardless of
  // that account-level setting, matching pdfer's no-tracking posture.
  gtag('config', gaID, { allow_google_signals: false });
})();
