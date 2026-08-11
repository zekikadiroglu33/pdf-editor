// HTTP-level production hardening: panic recovery, request logging,
// security headers, per-IP rate limiting, and process-wide concurrency
// limiting. Kept separate from main.go's PDF pipeline since these are
// generic HTTP concerns, not PDF ones - stdlib only, no dependencies.
package main

import (
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Panic recovery
// ---------------------------------------------------------------------------

// recoverMiddleware turns a panic in any handler into a clean 500 response.
// net/http already recovers panics per-connection so a bug here can't take
// down the whole process, but without this the client just sees a reset
// connection with no body, and the crash goes to stderr with no request
// context attached.
func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				log.Printf("panic handling %s %s: %v", r.Method, r.URL.Path, err)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Request logging
// ---------------------------------------------------------------------------

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s %d %s %s", r.Method, r.URL.Path, rec.status, time.Since(start), clientIP(r))
	})
}

// ---------------------------------------------------------------------------
// Language detection
// ---------------------------------------------------------------------------

// prefersTurkish reports whether the request's highest-priority
// Accept-Language tag is Turkish ("tr", "tr-TR", ...). Used by staticHandler
// to pre-select "Turkish" in the editor/converter language dropdown for
// likely-Turkish visitors, without ever looking at (or geolocating) the
// client's IP - consistent with this app's no-third-party-tracking stance.
// This is a proxy for the visitor's language preference, not their
// location: a browser set to Turkish gets it regardless of where the
// request originates, and vice versa.
func prefersTurkish(r *http.Request) bool {
	header := r.Header.Get("Accept-Language")
	if header == "" {
		return false
	}
	bestTag := ""
	bestQ := -1.0
	for _, part := range strings.Split(header, ",") {
		tag, qStr, hasQ := strings.Cut(strings.TrimSpace(part), ";")
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		q := 1.0
		if hasQ {
			if v, ok := strings.CutPrefix(strings.TrimSpace(qStr), "q="); ok {
				if parsed, err := strconv.ParseFloat(v, 64); err == nil {
					q = parsed
				}
			}
		}
		if q > bestQ {
			bestQ, bestTag = q, tag
		}
	}
	primary, _, _ := strings.Cut(bestTag, "-")
	return strings.EqualFold(primary, "tr")
}

// ---------------------------------------------------------------------------
// Security headers
// ---------------------------------------------------------------------------

// securityHeadersMiddleware sends a strict CSP plus standard hardening
// headers. script-src is 'self' plus Google Analytics' and Google
// AdSense's own script hosts (all first-party JS, including pdf.js, is
// still self-hosted under /static - see README; analytics.js is the one
// file that dynamically loads a script from the GA host - see
// static/analytics.js - and the AdSense loader, pagead2.googlesyndication.com,
// is embedded directly as a plain <script src=...> tag in every page's
// <head>, exactly as Google's own AdSense site-verification snippet
// requires, rather than injected via a same-origin wrapper file).
// style-src is 'self' only - every page's CSS lives in style.css, and
// visibility toggles in app.js/convert.js/tools.js set individual
// element.style.display properties, which CSP's style-src never governs
// (only <style> elements, style="" attributes, and .style.cssText are).
// connect-src/img-src additions are GA's own beacon/collect endpoints -
// 'unsafe-inline' is never needed anywhere in this CSP. gtag.js hits
// googletagmanager.com's own /a and /td image-pixel paths, and
// analytics.google.com/www.google.com collect endpoints, as part of its
// normal collection even with allow_google_signals: false in
// analytics.js (that flag controls what Google does with the data once
// collected, not which network paths the script uses) - all allowed.
// stats.g.doubleclick.net (Google's ad-serving/remarketing domain,
// reachable via gtag.js's default-on Conversion Linker feature) is
// deliberately NOT allowlisted for Analytics: that's cross-device ad
// tracking bolted onto GA itself, not the AdSense integration below, and
// keeping it out of gtag's own reporting matches this app's no-ads-
// tracking-via-Analytics posture even at the cost of one harmless CSP
// console warning from that one gtag feature.
// AdSense (pagead2.googlesyndication.com, allowlisted above and in
// img-src/connect-src below) is a deliberate, separate exception: this
// app does run AdSense display ads, so the ad creatives themselves need
// frame-src entries for the two hosts Google actually serves ad iframes
// from (googleads.g.doubleclick.net, tpc.googlesyndication.com) - without
// those the loader script would load but every ad slot would render
// blank, silently defeating the whole integration.
func securityHeadersMiddleware(next http.Handler) http.Handler {
	const csp = "default-src 'self'; " +
		"script-src 'self' https://www.googletagmanager.com https://pagead2.googlesyndication.com; " +
		"style-src 'self'; " +
		"img-src 'self' data: blob: https://www.google-analytics.com https://www.googletagmanager.com https://pagead2.googlesyndication.com; " +
		"connect-src 'self' https://www.googletagmanager.com https://www.google-analytics.com https://*.google-analytics.com https://analytics.google.com https://www.google.com https://pagead2.googlesyndication.com https://googleads.g.doubleclick.net; " +
		"worker-src 'self' blob:; " +
		"frame-src 'self' blob: https://googleads.g.doubleclick.net https://tpc.googlesyndication.com; " +
		"object-src 'none'; " +
		"base-uri 'none'; " +
		"form-action 'self'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", csp)
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Per-IP rate limiting (token bucket)
// ---------------------------------------------------------------------------

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

// rateLimiter is a simple per-key token bucket. No external dependency,
// consistent with the project's zero-Go-dependency design.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens added per second
	burst   float64 // bucket capacity
}

func newRateLimiter(rate, burst float64) *rateLimiter {
	rl := &rateLimiter{buckets: map[string]*bucket{}, rate: rate, burst: burst}
	go rl.cleanupLoop()
	return rl
}

func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	b, ok := rl.buckets[key]
	if !ok {
		b = &bucket{tokens: rl.burst, lastSeen: now}
		rl.buckets[key] = b
	}
	b.tokens += now.Sub(b.lastSeen).Seconds() * rl.rate
	if b.tokens > rl.burst {
		b.tokens = rl.burst
	}
	b.lastSeen = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// cleanupLoop evicts buckets for IPs that haven't been seen in a while, so
// the map doesn't grow without bound under sustained traffic from many
// distinct IPs.
func (rl *rateLimiter) cleanupLoop() {
	for range time.Tick(5 * time.Minute) {
		cutoff := time.Now().Add(-10 * time.Minute)
		rl.mu.Lock()
		for k, b := range rl.buckets {
			if b.lastSeen.Before(cutoff) {
				delete(rl.buckets, k)
			}
		}
		rl.mu.Unlock()
	}
}

func rateLimitMiddleware(rl *rateLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow(clientIP(r)) {
			w.Header().Set("Retry-After", "5")
			http.Error(w, "too many requests, slow down", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Process-wide concurrency limiting
// ---------------------------------------------------------------------------

// concurrencyLimiter caps how many requests run a wrapped handler at once.
// Each /api/edit call spawns two qpdf subprocesses and holds a decoded PDF
// in memory, so unbounded concurrency under a burst of uploads is a real
// resource-exhaustion risk with no auth in front of it. Requests over the
// cap fail fast with 503 rather than queuing indefinitely.
type concurrencyLimiter struct {
	sem chan struct{}
}

func newConcurrencyLimiter(n int) *concurrencyLimiter {
	return &concurrencyLimiter{sem: make(chan struct{}, n)}
}

func (c *concurrencyLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case c.sem <- struct{}{}:
			defer func() { <-c.sem }()
			next.ServeHTTP(w, r)
		default:
			w.Header().Set("Retry-After", "5")
			http.Error(w, "server busy, try again shortly", http.StatusServiceUnavailable)
		}
	})
}
