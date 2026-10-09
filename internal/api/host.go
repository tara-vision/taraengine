package api

import (
	"net"
	"net/http"
	"strings"

	"github.com/tara-vision/taraengine/internal/config"
)

const hintHost = "use the address the engine listens on, or set an api_key"

// hostGuard refuses a request whose Host header names anything but this daemon's own addresses, while no api_key
// is set. It stops DNS rebinding. The page points a host name of its own at 127.0.0.1, and the browser then takes
// the daemon for the page's own origin: it sends no preflight, sends no Origin header on a GET, and hands every
// answer over, so the origin rule of browser.go has nothing to judge. The request still carries the page's host
// name, and holding the Host to the daemon's own names closes the gap. The pages of other origins that use the
// daemon's real address are stopped by the origin rule and the JSON rule, and the proxy strips the CORS headers of
// the router, so that no answer from the router grants a page more than those rules do. With a key set the check
// is skipped, because the key is what keeps a rebound page out.
func hostGuard(cfg config.Config, next http.Handler) http.Handler {
	if cfg.APIKey != "" {
		return next
	}
	host, _, err := net.SplitHostPort(cfg.Listen)
	if err == nil && (host == "" || isUnspecified(host)) {
		// A daemon that listens on every interface is reached by whatever names its machine has. The operator
		// opened it to the network on purpose, and there is no list of names to hold a request to.
		return next
	}
	allowed := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
	if err == nil {
		allowed[hostName(host)] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[hostName(r.Host)] {
			writeError(w, http.StatusForbidden, "the Host header is not one of the engine's own addresses", hintHost)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isUnspecified(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

// hostName is the name in a Host header or a listen address: lower case, without the port, the brackets of an
// IPv6 address or the dot that ends a fully qualified name.
func hostName(hostport string) string {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	return strings.TrimSuffix(strings.ToLower(host), ".")
}
