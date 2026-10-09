package config

import (
	"fmt"
	"net/url"
	"strings"
)

// ParseOrigin checks that s is a web origin as a browser writes it in an Origin header: a scheme, a host and an
// optional port, with no user, path, query or fragment. It returns the scheme and the name of the host, in lower
// case and without the port, the brackets of an IPv6 address or a trailing dot. The error reads as the end of a
// sentence that begins with the key the origin was found under.
func ParseOrigin(s string) (scheme, host string, err error) {
	u, perr := url.Parse(s)
	if perr != nil || u.Scheme == "" || u.Hostname() == "" || u.Opaque != "" || u.User != nil || u.Path != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", "", fmt.Errorf("%q is not an origin: write scheme://host or scheme://host:port, "+
			"such as https://app.example", s)
	}
	return u.Scheme, strings.TrimSuffix(strings.ToLower(u.Hostname()), "."), nil
}
