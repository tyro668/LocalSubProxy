// Package web is the shared mock-SaaS chat page served by the Go, Node and
// Python LSP2 gateways at / and /chat. It is a static page: it holds no
// secrets; the user signs in with a tenant API key that stays in the tab's
// sessionStorage and is sent only to this gateway's /v2/tenant and /v1 APIs.
package web

import (
	"embed"
	"net/http"
)

//go:embed chat.html chat.js qrcode.js
var files embed.FS

// Files maps request path to embedded file and content type.
var Files = map[string][2]string{
	"/":               {"chat.html", "text/html; charset=utf-8"},
	"/chat":           {"chat.html", "text/html; charset=utf-8"},
	"/chat/":          {"chat.html", "text/html; charset=utf-8"},
	"/chat/chat.js":   {"chat.js", "text/javascript; charset=utf-8"},
	"/chat/qrcode.js": {"qrcode.js", "text/javascript; charset=utf-8"},
}

// CSP is the Content-Security-Policy sent with every UI file (the Node and
// Python gateways send the same value).
const CSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// Handler serves the UI files; other paths get 404.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := Files[r.URL.Path]
		if !ok || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
			http.NotFound(w, r)
			return
		}
		b, err := files.ReadFile(f[0])
		if err != nil {
			http.Error(w, "missing", 500)
			return
		}
		h := w.Header()
		h.Set("Content-Type", f[1])
		h.Set("Content-Security-Policy", CSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache")
		_, _ = w.Write(b)
	})
}

// Wrap routes the UI paths to Handler and everything else to api.
func Wrap(api http.Handler) http.Handler {
	ui := Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := Files[r.URL.Path]; ok {
			ui.ServeHTTP(w, r)
			return
		}
		api.ServeHTTP(w, r)
	})
}
