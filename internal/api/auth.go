package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

const bearerScheme = "Bearer "

// bearer requires Authorization: Bearer <key> on every route but GET and HEAD /api/health when a key is set.
func bearer(key string, next http.Handler) http.Handler {
	if key == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.URL.Path == "/api/health" {
			next.ServeHTTP(w, r)
			return
		}
		if !keyMatches(bearerToken(r), key) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "missing or wrong API key", "send Authorization: Bearer <api_key>")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerToken is the credential of an Authorization: Bearer header, and empty for any other header. The scheme
// name is matched without regard to case, as RFC 7235 asks.
func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if len(header) < len(bearerScheme) || !strings.EqualFold(header[:len(bearerScheme)], bearerScheme) {
		return ""
	}
	return header[len(bearerScheme):]
}

// keyMatches compares digests of the two keys, so neither the key nor its length shows in the time it takes.
func keyMatches(got, want string) bool {
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}
