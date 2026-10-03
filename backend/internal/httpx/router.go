package httpx

import (
	"net/http"
	"strings"
)

// CORS returns middleware implementing a permissive-but-explicit CORS policy:
// only origins on the allow list are echoed, and the preflight cache is capped so
// config edits take effect within five minutes.
func CORS(allowed []string) func(http.Handler) http.Handler {
	allowSet := make(map[string]bool, len(allowed))
	allowAll := false
	for _, o := range allowed {
		o = strings.TrimRight(strings.TrimSpace(o), "/")
		if o == "*" {
			allowAll = true
			continue
		}
		allowSet[o] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := strings.TrimRight(r.Header.Get("Origin"), "/")

			// Vary regardless of match so caches key on Origin correctly.
			w.Header().Add("Vary", "Origin")

			if origin != "" {
				switch {
				case allowAll:
					w.Header().Set("Access-Control-Allow-Origin", "*")
				case allowSet[origin]:
					w.Header().Set("Access-Control-Allow-Origin", origin)
				}
			}

			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Last-Event-ID")
			w.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Type, X-Total-Count")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Max-Age", "300")

			// Preflight never reaches a handler.
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// SecurityHeaders applies conservative defaults. The API serves JSON only, so a
// strict CSP is both safe and simple.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		next.ServeHTTP(w, r)
	})
}

// Chain applies middleware in the order given: the first argument is outermost.
func Chain(h http.Handler, mw ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// NewRouter returns a mux with JSON-shaped fallbacks so the SPA can parse
// 404/405 responses the same way it parses every other error.
//
// Go 1.22+ ServeMux handles method-prefixed patterns ("POST /api/tickets") and
// path parameters ("/api/tickets/{ticket_id}") natively, read via r.PathValue.
func NewRouter() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			OK(w, map[string]any{
				"service": "itsm-api",
				"status":  "ok",
				"docs":    "/api/v1",
			})
			return
		}
		Error(w, http.StatusNotFound, "no route matches this path", "route_not_found")
	})
	return mux
}
