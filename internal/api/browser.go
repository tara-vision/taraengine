package api

import (
	"mime"
	"net/http"
	"strings"

	"github.com/tara-vision/taraengine/internal/config"
)

// A web page can reach this daemon at 127.0.0.1 from a browser on the same machine. Three rules keep it out, and
// each answers what the others cannot: hostGuard stops DNS rebinding, originPolicy stops a page of another origin
// that uses the daemon's real address, and jsonOnly stops the simple POST that needs no preflight. The proxy to the
// router strips the router's own CORS headers, so that the headers set here are the only ones a page ever sees.

// What the daemon allows a page to do, in the words of the CORS headers.
const (
	allowedMethods  = "GET, POST, DELETE, OPTIONS"
	allowedHeaders  = "Authorization, Content-Type" // what a preflight allows when the request names no headers
	preflightMaxAge = "600"                         // seconds a browser may keep the answer to a preflight
	hintOrigin      = "add the origin to allowed_origins in the config file"
)

// The words of the refusal of a body that is not JSON.
const (
	messageNotJSON = "the request body must be application/json"
	hintNotJSON    = "send Content-Type: application/json"
)

// localNames are the host names of the pages that may use the API without being listed.
var localNames = map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}

// originPolicy decides which web pages may use the API. A request that carries an Origin header comes from a page,
// and it is refused with 403 unless its origin is allowed: an http or https page served from localhost, 127.0.0.1
// or [::1] on any port, or one of the allowed_origins of the configuration, compared as written but for the case.
// A request without an Origin header does not come from a page and is not asked about it. The rule holds with or
// without an api_key, because a browser will send an Authorization header for a page that is allowed to ask.
//
// A preflight (any OPTIONS request) is answered here and never passed on: 204 with the CORS headers for an allowed
// origin, 403 for the others. The answers to an allowed origin carry its Access-Control-Allow-Origin, also when
// they refuse the request, so that the page can read why. The answers to the others carry no CORS header at all.
// Every answer says Vary: Origin, since what it is depends on that header, or on its absence.
func (s *Server) originPolicy(next http.Handler) http.Handler {
	listed := make(map[string]bool, len(s.deps.Config.AllowedOrigins))
	for _, origin := range s.deps.Config.AllowedOrigins {
		listed[strings.ToLower(origin)] = true
	}
	allowed := func(origin string) bool { return listed[strings.ToLower(origin)] || isLocalOrigin(origin) }
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Origin")
		origins := r.Header.Values("Origin")
		switch {
		case len(origins) == 0 && r.Method == http.MethodOptions:
			w.Header().Set("Allow", allowedMethods)
			w.WriteHeader(http.StatusNoContent)
		case len(origins) == 0:
			next.ServeHTTP(w, r)
		case len(origins) > 1 || !allowed(origins[0]):
			s.deps.Logger.Debug("refused a page of an origin that is not allowed",
				"origin", strings.Join(origins, ", "), "method", r.Method, "path", r.URL.Path)
			writeError(w, http.StatusForbidden, "the Origin of this request is not allowed", hintOrigin)
		default:
			w.Header().Set("Access-Control-Allow-Origin", origins[0])
			if r.Method == http.MethodOptions {
				answerPreflight(w, r)
				return
			}
			next.ServeHTTP(w, r)
		}
	})
}

// isLocalOrigin reports whether origin is an http or https origin on a local host name, at any port.
func isLocalOrigin(origin string) bool {
	scheme, host, err := config.ParseOrigin(origin)
	return err == nil && (scheme == "http" || scheme == "https") && localNames[host]
}

// answerPreflight tells an allowed page what it may send. The origin is the decision, so the headers the page names
// in Access-Control-Request-Headers are allowed, which lets an SDK send headers of its own, and a request that names
// none gets the headers the API itself uses. A page that asks to reach a private network address is told yes for
// the same reason.
func answerPreflight(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	headers := allowedHeaders
	if names := requestedHeaders(r); len(names) > 0 {
		headers = strings.Join(names, ", ")
	}
	h.Set("Access-Control-Allow-Methods", allowedMethods)
	h.Set("Access-Control-Allow-Headers", headers)
	h.Add("Vary", "Access-Control-Request-Headers")
	h.Set("Access-Control-Max-Age", preflightMaxAge)
	if r.Header.Get("Access-Control-Request-Private-Network") == "true" {
		h.Set("Access-Control-Allow-Private-Network", "true")
	}
	w.WriteHeader(http.StatusNoContent)
}

// requestedHeaders are the header names that a preflight lists in Access-Control-Request-Headers, in the order given.
// Only the elements of the list that are tokens (RFC 9110, section 5.6.2) are names; the rest is dropped, so that the
// answer repeats nothing that a page made up.
func requestedHeaders(r *http.Request) []string {
	var names []string
	for _, line := range r.Header.Values("Access-Control-Request-Headers") {
		for _, element := range strings.Split(line, ",") {
			if name := strings.Trim(element, " \t"); isToken(name) {
				names = append(names, name)
			}
		}
	}
	return names
}

// isToken reports whether s is a token: one or more of the letters, digits and marks !#$%&'*+-.^_`|~.
func isToken(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		letterOrDigit := 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
		if !letterOrDigit && !strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)) {
			return false
		}
	}
	return s != ""
}

// jsonOnly refuses a POST whose Content-Type is not application/json, parameters such as a charset allowed, before
// a handler or the proxy sees it. A browser sends a form or a text/plain body to any address without asking first,
// and the router would parse such a body as JSON all the same. The requests with no body to speak of, GET, HEAD,
// DELETE and OPTIONS, are not affected.
func jsonOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && !hasJSONBody(r) {
			writeError(w, http.StatusBadRequest, messageNotJSON, hintNotJSON)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func hasJSONBody(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mediaType == "application/json"
}
