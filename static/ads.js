// Requests an AdSense fill for every .adsbygoogle <ins> element already in
// the page's HTML (see the ad-slot markup near the end of each page's
// <body>). Kept as an external file (not an inline <script>) for the same
// CSP reason as analytics.js: this app's script-src has no 'unsafe-inline',
// so the (adsbygoogle = window.adsbygoogle || []).push({}) call AdSense's
// own snippet normally inlines has to live here instead. The loader script
// itself (pagead2.googlesyndication.com/pagead/js/adsbygoogle.js) stays a
// plain <script src=...> tag in each page's <head>, exactly as Google's
// site-verification snippet requires - see middleware.go's CSP comment.
(function () {
  document.querySelectorAll('ins.adsbygoogle').forEach(function () {
    (window.adsbygoogle = window.adsbygoogle || []).push({});
  });
})();
