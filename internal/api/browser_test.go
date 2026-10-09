package api

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/tara-vision/taraengine/internal/backend"
	"github.com/tara-vision/taraengine/pkg/api"
)

// proxyProbe is a backend whose /v1 proxy counts the requests that reach it, for the tests of what keeps a web
// page away from the router.
type proxyProbe struct {
	backend.Backend
	mu   sync.Mutex
	seen []string
}

func (p *proxyProbe) Proxy() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.seen = append(p.seen, r.Method+" "+r.URL.Path)
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"proxied":true}`))
	})
}

func (p *proxyProbe) requests() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen...)
}

// browserHarness is the API over a backend whose proxy is watched.
func browserHarness(t *testing.T, apiKey string, opts ...func(*Deps)) (*harness, *proxyProbe) {
	t.Helper()
	probe := &proxyProbe{}
	watch := func(d *Deps) {
		probe.Backend = d.Backend
		d.Backend = probe
	}
	return newHarness(t, apiKey, append([]func(*Deps){watch}, opts...)...), probe
}

func withHeader(name, value string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set(name, value) }
}

func withOrigin(origin string) func(*http.Request) { return withHeader("Origin", origin) }

// call sends one request and fails the test when it cannot be sent.
func (h *harness) call(t *testing.T, method, path, contentType, key, body string, edits ...func(*http.Request)) (int, http.Header, []byte) {
	t.Helper()
	status, header, data, err := h.request(method, path, contentType, key, strings.NewReader(body), edits...)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return status, header, data
}

func assertNoCORSHeaders(t *testing.T, what string, header http.Header) {
	t.Helper()
	for name := range header {
		if strings.HasPrefix(name, "Access-Control-") {
			t.Errorf("%s: an answer to a page that is not allowed carries %s: %v", what, name, header[name])
		}
	}
}

func TestAPageOfAnotherOriginIsRefusedOnEveryRoute(t *testing.T) {
	const evil = "https://evil.example"
	for _, key := range []string{"", "secret"} {
		h, probe := browserHarness(t, key)
		routes := []struct{ method, path, contentType, body string }{
			{http.MethodGet, "/api/health", "", ""},
			{http.MethodGet, "/api/models", "", ""},
			{http.MethodGet, "/v1/models", "", ""},
			{http.MethodPost, "/api/load", "application/json", `{"model":"gemma4:12b"}`},
			{http.MethodPost, "/api/pull", "text/plain", `{"model":"gemma4:12b"}`},
			{http.MethodPost, "/v1/chat/completions", "text/plain", `{"model":"gemma4:12b"}`},
			{http.MethodPost, "/v1/chat/completions", "application/json", `{"model":"gemma4:12b"}`},
			{http.MethodDelete, "/api/models/gemma4:12b", "", ""},
		}
		for _, rt := range routes {
			status, header, data := h.call(t, rt.method, rt.path, rt.contentType, key, rt.body, withOrigin(evil))
			got := decode[api.Error](t, data).Error
			if status != http.StatusForbidden || got.Message == "" || got.Hint == "" || strings.Contains(string(data), "evil") {
				t.Errorf("key %q, %s %s from %s: %d %s", key, rt.method, rt.path, evil, status, data)
			}
			assertNoCORSHeaders(t, rt.method+" "+rt.path, header)
		}
		if got := probe.requests(); len(got) != 0 || len(h.fake.Loads) != 0 || h.fake.Refreshes != 0 {
			t.Errorf("key %q: a refused page reached the router or a handler: %v %+v %d", key, got, h.fake.Loads, h.fake.Refreshes)
		}
	}
}

func TestAPageThatSendsAnotherOriginsSimplePostNeverReachesTheRouter(t *testing.T) {
	h, probe := browserHarness(t, "")
	status, _, data := h.call(t, http.MethodPost, "/v1/chat/completions", "text/plain", "",
		`{"model":"gemma4:12b","messages":[]}`, withOrigin("https://evil.example"))
	if status != http.StatusForbidden || len(probe.requests()) != 0 {
		t.Fatalf("the simple POST of a page: %d %s, router saw %v", status, data, probe.requests())
	}
}

func TestPreflightsAreAnsweredHereAndNeverForwarded(t *testing.T) {
	for _, key := range []string{"", "secret"} {
		h, probe := browserHarness(t, key)
		preflight := func(origin string, edits ...func(*http.Request)) (int, http.Header, []byte) {
			all := append([]func(*http.Request){
				withOrigin(origin), withHeader("Access-Control-Request-Method", "POST"),
				withHeader("Access-Control-Request-Headers", "content-type, authorization"),
			}, edits...)
			return h.call(t, http.MethodOptions, "/v1/chat/completions", "", "", "", all...)
		}
		status, header, data := preflight("https://evil.example")
		if status != http.StatusForbidden || decode[api.Error](t, data).Error.Message == "" {
			t.Errorf("key %q: a preflight from a page that is not allowed: %d %s", key, status, data)
		}
		assertNoCORSHeaders(t, "the preflight from evil.example", header)

		status, header, data = preflight("http://localhost:5173")
		want := map[string]string{
			"Access-Control-Allow-Origin":  "http://localhost:5173",
			"Access-Control-Allow-Methods": "GET, POST, DELETE, OPTIONS",
			"Access-Control-Allow-Headers": "content-type, authorization",
			"Access-Control-Max-Age":       "600",
		}
		if status != http.StatusNoContent || len(data) != 0 {
			t.Errorf("key %q: a preflight from localhost: %d %q", key, status, data)
		}
		for name, value := range want {
			if got := header.Get(name); got != value {
				t.Errorf("key %q: %s is %q, want %q", key, name, got, value)
			}
		}
		if got := strings.Join(header.Values("Vary"), ","); !strings.Contains(got, "Origin") {
			t.Errorf("key %q: the answer depends on the Origin, so Vary must say so: %q", key, got)
		}
		if got := header.Get("Access-Control-Allow-Private-Network"); got != "" {
			t.Errorf("key %q: nothing asked for private network access: %q", key, got)
		}
		status, header, _ = preflight("http://localhost:5173", withHeader("Access-Control-Request-Private-Network", "true"))
		if status != http.StatusNoContent || header.Get("Access-Control-Allow-Private-Network") != "true" {
			t.Errorf("key %q: a page that asks for private network access gets it when its origin is allowed: %d %v", key, status, header)
		}
		if got := probe.requests(); len(got) != 0 {
			t.Errorf("key %q: a preflight reached the router: %v", key, got)
		}
	}
}

func TestAPreflightIsAnsweredOnTheApiRoutesToo(t *testing.T) {
	h, _ := browserHarness(t, "secret")
	for _, path := range []string{"/api/load", "/api/models/gemma4:12b", "/api/health"} {
		status, header, _ := h.call(t, http.MethodOptions, path, "", "", "", withOrigin("http://127.0.0.1:3000"),
			withHeader("Access-Control-Request-Method", "DELETE"))
		if status != http.StatusNoContent || header.Get("Access-Control-Allow-Origin") != "http://127.0.0.1:3000" {
			t.Errorf("%s: %d %v", path, status, header)
		}
	}
	if len(h.fake.Loads) != 0 {
		t.Fatalf("a preflight must not reach a handler: %+v", h.fake.Loads)
	}
}

func TestAnOptionsRequestWithoutAnOriginIsAnsweredToo(t *testing.T) {
	h, probe := browserHarness(t, "secret")
	status, header, data := h.call(t, http.MethodOptions, "/v1/models", "", "", "")
	if status != http.StatusNoContent || len(data) != 0 || header.Get("Allow") != "GET, POST, DELETE, OPTIONS" {
		t.Fatalf("OPTIONS without an Origin: %d %v %q", status, header, data)
	}
	assertNoCORSHeaders(t, "OPTIONS without an Origin", header)
	if got := probe.requests(); len(got) != 0 {
		t.Fatalf("no OPTIONS request is forwarded: %v", got)
	}
}

func TestTheActualRequestOfAnAllowedPageIsServedAndAnswersItsOrigin(t *testing.T) {
	h, probe := browserHarness(t, "")
	status, header, data := h.call(t, http.MethodPost, "/v1/chat/completions", "application/json", "",
		`{"model":"gemma4:12b"}`, withOrigin("http://localhost:5173"))
	if status != 200 || string(data) != `{"proxied":true}` || len(probe.requests()) != 1 {
		t.Fatalf("an allowed page reaches the router: %d %s %v", status, data, probe.requests())
	}
	if got := header.Values("Access-Control-Allow-Origin"); len(got) != 1 || got[0] != "http://localhost:5173" {
		t.Errorf("Access-Control-Allow-Origin %v", got)
	}
	if got := strings.Join(header.Values("Vary"), ","); !strings.Contains(got, "Origin") {
		t.Errorf("Vary %q", got)
	}
	status, header, _ = h.call(t, http.MethodGet, "/api/version", "", "", "", withOrigin("http://[::1]:8080"))
	if status != 200 || header.Get("Access-Control-Allow-Origin") != "http://[::1]:8080" {
		t.Errorf("a route of /api: %d %v", status, header)
	}
	if header.Get("Access-Control-Allow-Methods") != "" {
		t.Errorf("only a preflight lists methods: %v", header)
	}
}

func TestAnAllowedPageReadsTheRefusalOfAKeyedDaemon(t *testing.T) {
	h, probe := browserHarness(t, "secret")
	status, header, data := h.call(t, http.MethodPost, "/v1/chat/completions", "application/json", "", `{}`,
		withOrigin("http://localhost:5173"))
	if status != http.StatusUnauthorized || header.Get("Access-Control-Allow-Origin") != "http://localhost:5173" {
		t.Fatalf("a page must be able to read a 401, or it cannot tell its user to set a key: %d %v %s", status, header, data)
	}
	status, header, _ = h.call(t, http.MethodPost, "/v1/chat/completions", "application/json", "secret", `{}`,
		withOrigin("http://localhost:5173"))
	if status != 200 || header.Get("Access-Control-Allow-Origin") != "http://localhost:5173" || len(probe.requests()) != 1 {
		t.Fatalf("with the key: %d %v %v", status, header, probe.requests())
	}
}

func TestTheDefaultAllowedOriginsAreLocalPagesOnAnyPort(t *testing.T) {
	h, _ := browserHarness(t, "")
	allowed := []string{
		"http://localhost", "http://localhost:5173", "https://localhost:8443", "http://127.0.0.1", "http://127.0.0.1:3000",
		"https://127.0.0.1:3000", "http://[::1]", "http://[::1]:3000", "HTTP://LOCALHOST:80", "http://localhost.:3000",
	}
	refused := []string{
		"", "null", "https://evil.example", "http://evil.example:80", "http://localhost.evil.example",
		"http://127.0.0.1.evil.example:3000", "http://evil.example/localhost", "http://localhost@evil.example",
		"http://localhost:3000@evil.example", "http://evil.example#@localhost", "http://127.0.0.2", "http://0.0.0.0:3000",
		"http://[::2]:3000", "http://192.0.2.10:3000", "ftp://localhost", "file://localhost", "localhost:3000",
		"//localhost:3000", "http://localhost:3000/", "http://localhost:3000/app", "http://localhost:3000?x=1",
		"http://localhost:abc", "http://localhost%00.evil.example", "app://localhost", "chrome-extension://localhost",
	}
	for _, origin := range allowed {
		status, header, data := h.call(t, http.MethodGet, "/api/version", "", "", "", withOrigin(origin))
		if status != 200 || header.Get("Access-Control-Allow-Origin") != origin {
			t.Errorf("origin %q: %d %v %s", origin, status, header, data)
		}
	}
	for _, origin := range refused {
		status, header, _ := h.call(t, http.MethodGet, "/api/version", "", "", "", withOrigin(origin))
		if status != http.StatusForbidden {
			t.Errorf("origin %q: %d", origin, status)
		}
		assertNoCORSHeaders(t, "origin "+origin, header)
	}
}

func TestAllowedOriginsAddToTheLocalOnes(t *testing.T) {
	h, _ := browserHarness(t, "", func(d *Deps) {
		d.Config.AllowedOrigins = []string{"https://app.example", "app://obsidian.md", "HTTP://Admin.Example:8080"}
	})
	for _, origin := range []string{
		"https://app.example", "https://APP.example", "app://obsidian.md", "http://admin.example:8080", "http://localhost:3000",
	} {
		status, header, _ := h.call(t, http.MethodGet, "/api/version", "", "", "", withOrigin(origin))
		if status != 200 || header.Get("Access-Control-Allow-Origin") != origin {
			t.Errorf("origin %q: %d %v", origin, status, header)
		}
	}
	for _, origin := range []string{
		"https://app.example:8443", "http://app.example", "https://app.example.evil.example", "https://sub.app.example",
		"app://obsidian.md.evil.example", "null", "https://evil.example",
	} {
		if status, _, _ := h.call(t, http.MethodGet, "/api/version", "", "", "", withOrigin(origin)); status != http.StatusForbidden {
			t.Errorf("origin %q: %d", origin, status)
		}
	}
	status, header, _ := h.call(t, http.MethodOptions, "/v1/chat/completions", "", "", "", withOrigin("https://app.example"))
	if status != http.StatusNoContent || header.Get("Access-Control-Allow-Origin") != "https://app.example" {
		t.Errorf("the preflight of a listed origin: %d %v", status, header)
	}
}

func TestAnOriginHeaderThatIsEmptyOrRepeatedIsRefused(t *testing.T) {
	h, probe := browserHarness(t, "")
	status, _, _ := h.call(t, http.MethodGet, "/api/version", "", "", "", func(r *http.Request) { r.Header["Origin"] = []string{""} })
	if status != http.StatusForbidden {
		t.Errorf("an empty Origin header: %d", status)
	}
	status, _, _ = h.call(t, http.MethodPost, "/v1/chat/completions", "application/json", "", `{}`, func(r *http.Request) {
		r.Header["Origin"] = []string{"http://localhost:5173", "https://evil.example"}
	})
	if status != http.StatusForbidden || len(probe.requests()) != 0 {
		t.Errorf("two Origin headers, one of them foreign: %d %v", status, probe.requests())
	}
	status, _, _ = h.call(t, http.MethodGet, "/api/version", "", "", "", func(r *http.Request) {
		r.Header["Origin"] = []string{"http://localhost:5173", "http://localhost:5173"}
	})
	if status != http.StatusForbidden {
		t.Errorf("an Origin header that is not one value is refused even when every value is allowed: %d", status)
	}
}

func TestARefusedOriginIsLoggedAtDebugLevelOnly(t *testing.T) {
	h, _ := browserHarness(t, "")
	h.call(t, http.MethodGet, "/api/version", "", "", "", withOrigin("https://evil.example"))
	logs := h.logs.String()
	if !strings.Contains(logs, "level=DEBUG") || !strings.Contains(logs, "https://evil.example") ||
		strings.Contains(logs, "level=INFO") || strings.Contains(logs, "level=WARN") || strings.Contains(logs, "level=ERROR") {
		t.Fatalf("a page that is turned away is no event for the operator unless they ask for debug: %s", logs)
	}
}

func TestEveryPostMustBeJSONAndOnlyPosts(t *testing.T) {
	for _, key := range []string{"", "secret"} {
		h, probe := browserHarness(t, key)
		for _, contentType := range []string{"", "text/plain", "text/plain;charset=UTF-8", "application/x-www-form-urlencoded", "multipart/form-data", "application/jsonx"} {
			for _, path := range []string{"/v1/chat/completions", "/v1/completions", "/v1/messages", "/api/load"} {
				status, _, data := h.call(t, http.MethodPost, path, contentType, key, `{"model":"gemma4:12b"}`)
				got := decode[api.Error](t, data).Error
				if status != http.StatusBadRequest || got.Message != "the request body must be application/json" ||
					!strings.Contains(got.Hint, "Content-Type: application/json") {
					t.Errorf("key %q, POST %s as %q: %d %s", key, path, contentType, status, data)
				}
			}
		}
		if got := probe.requests(); len(got) != 0 {
			t.Errorf("key %q: a body that is not JSON reached the router: %v", key, got)
		}
		for _, contentType := range []string{"application/json", "application/json; charset=utf-8", "Application/JSON"} {
			if status, _, data := h.call(t, http.MethodPost, "/v1/chat/completions", contentType, key, `{}`); status != 200 {
				t.Errorf("key %q, POST as %q: %d %s", key, contentType, status, data)
			}
		}
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodDelete} {
			status, _, _ := h.call(t, method, "/api/models/absent", "text/plain", key, "")
			if status == http.StatusBadRequest {
				t.Errorf("key %q: %s needs no JSON body: %d", key, method, status)
			}
		}
		if status, _, _ := h.call(t, http.MethodGet, "/v1/models", "text/plain", key, ""); status != 200 {
			t.Errorf("key %q: GET /v1/models with a content type: %d", key, status)
		}
	}
}

func TestAKeyedDaemonChecksTheKeyBeforeTheContentType(t *testing.T) {
	h, _ := browserHarness(t, "secret")
	if status, _, _ := h.call(t, http.MethodPost, "/v1/chat/completions", "text/plain", "", `{}`); status != http.StatusUnauthorized {
		t.Errorf("without the key: %d", status)
	}
	if status, _, _ := h.call(t, http.MethodPost, "/v1/chat/completions", "text/plain", "secret", `{}`); status != http.StatusBadRequest {
		t.Errorf("with the key: %d", status)
	}
}

func TestTheHostGuardAnswersBeforeTheOriginPolicy(t *testing.T) {
	h, probe := browserHarness(t, "")
	status, header, _ := h.call(t, http.MethodGet, "/v1/models", "", "", "", withOrigin("http://localhost:5173"), withHost("evil.example"))
	if status != http.StatusForbidden || len(probe.requests()) != 0 {
		t.Fatalf("a rebound name is refused whatever the Origin says: %d %v", status, probe.requests())
	}
	assertNoCORSHeaders(t, "a refused host", header)
}

func TestAPreflightEchoesTheRequestedHeadersOfAnAllowedOrigin(t *testing.T) {
	const requested = "authorization, content-type, x-stainless-os"
	for _, key := range []string{"", "secret"} {
		h, probe := browserHarness(t, key)
		preflight := func(origin string, edits ...func(*http.Request)) (int, http.Header) {
			status, header, _ := h.call(t, http.MethodOptions, "/v1/chat/completions", "", "", "",
				append([]func(*http.Request){withOrigin(origin), withHeader("Access-Control-Request-Method", "POST")}, edits...)...)
			return status, header
		}
		status, header := preflight("http://localhost:5173", withHeader("Access-Control-Request-Headers", requested))
		if status != http.StatusNoContent || header.Get("Access-Control-Allow-Headers") != requested {
			t.Errorf("key %q: the headers the page names are the headers it may send: %d %v", key, status, header)
		}
		if got := strings.Join(header.Values("Vary"), ","); !strings.Contains(got, "Origin") ||
			!strings.Contains(got, "Access-Control-Request-Headers") {
			t.Errorf("key %q: the answer depends on both, so Vary must say so: %q", key, got)
		}
		status, header = preflight("http://localhost:5173", func(r *http.Request) {
			r.Header["Access-Control-Request-Headers"] = []string{"authorization", "x-stainless-lang"}
		})
		if status != http.StatusNoContent || header.Get("Access-Control-Allow-Headers") != "authorization, x-stainless-lang" {
			t.Errorf("key %q: a request that names its headers on two lines: %d %v", key, status, header)
		}
		for name, edit := range map[string]func(*http.Request){
			"names none":                 func(*http.Request) {},
			"names an empty list":        withHeader("Access-Control-Request-Headers", ""),
			"names only separators":      withHeader("Access-Control-Request-Headers", " , ,"),
			"names nothing on two lines": func(r *http.Request) { r.Header["Access-Control-Request-Headers"] = []string{"", ""} },
		} {
			status, header = preflight("http://localhost:5173", edit)
			if status != http.StatusNoContent || header.Get("Access-Control-Allow-Headers") != "Authorization, Content-Type" {
				t.Errorf("key %q: a request that %s gets the headers the API itself uses: %d %v", key, name, status, header)
			}
		}
		status, header = preflight("https://evil.example", withHeader("Access-Control-Request-Headers", requested))
		if status != http.StatusForbidden {
			t.Errorf("key %q: a page that is not allowed: %d", key, status)
		}
		assertNoCORSHeaders(t, "the preflight of a page that is not allowed", header)
		if got := probe.requests(); len(got) != 0 {
			t.Errorf("key %q: a preflight reached the router: %v", key, got)
		}
	}
}

// varyOrigin counts the Origin fields in the Vary headers of an answer.
func varyOrigin(header http.Header) int {
	n := 0
	for _, value := range header.Values("Vary") {
		for _, field := range strings.Split(value, ",") {
			if strings.TrimSpace(field) == "Origin" {
				n++
			}
		}
	}
	return n
}

func TestEveryAnswerThatDependsOnTheOriginSaysSoInVary(t *testing.T) {
	cases := []struct {
		what, method, path, contentType, body string
		edits                                 []func(*http.Request)
	}{
		{"a page that is allowed", http.MethodGet, "/api/version", "", "", []func(*http.Request){withOrigin("http://localhost:5173")}},
		{"a page that is not", http.MethodGet, "/api/version", "", "", []func(*http.Request){withOrigin("https://evil.example")}},
		{"a request without an origin", http.MethodGet, "/api/version", "", "", nil},
		{"a proxied request without an origin", http.MethodPost, "/v1/chat/completions", "application/json", `{}`, nil},
		{"a body that is not JSON", http.MethodPost, "/v1/chat/completions", "text/plain", `{}`, nil},
		{"a refused origin on a POST", http.MethodPost, "/v1/chat/completions", "application/json", `{}`, []func(*http.Request){withOrigin("null")}},
		{"an OPTIONS request without an origin", http.MethodOptions, "/v1/models", "", "", nil},
		{"a preflight of a page that is allowed", http.MethodOptions, "/v1/models", "", "", []func(*http.Request){withOrigin("http://127.0.0.1:3000")}},
		{"a preflight of a page that is not", http.MethodOptions, "/v1/models", "", "", []func(*http.Request){withOrigin("https://evil.example")}},
	}
	for _, key := range []string{"", "secret"} {
		h, _ := browserHarness(t, key)
		for _, tc := range cases {
			status, header, _ := h.call(t, tc.method, tc.path, tc.contentType, key, tc.body, tc.edits...)
			if got := varyOrigin(header); got != 1 {
				t.Errorf("key %q, %s: %d, Origin is in Vary %d times: %v", key, tc.what, status, got, header.Values("Vary"))
			}
		}
	}
	h, _ := browserHarness(t, "secret")
	status, header, _ := h.call(t, http.MethodGet, "/api/models", "", "wrong", "")
	if varyOrigin(header) != 1 || status != http.StatusUnauthorized {
		t.Errorf("a refused key: %d %v", status, header.Values("Vary"))
	}
}

func TestAPreflightEchoesOnlyTheHeaderNamesThatAreTokens(t *testing.T) {
	const tokenMarks = "azAZ09!#$%&'*+-.^_`|~" // the ends of the ranges of letters and digits, and every mark RFC 9110 lets a token hold
	h, probe := browserHarness(t, "")
	allowed := func(requested ...string) string {
		status, header, _ := h.call(t, http.MethodOptions, "/v1/chat/completions", "", "", "", withOrigin("http://localhost:5173"),
			func(r *http.Request) { r.Header["Access-Control-Request-Headers"] = requested })
		if status != http.StatusNoContent {
			t.Errorf("%q: %d", requested, status)
		}
		return header.Get("Access-Control-Allow-Headers")
	}
	for _, tc := range []struct {
		what      string
		requested []string
		want      string
	}{
		{"junk between names", []string{`x-a, <script>, "q", c d, (e)`}, "x-a"},
		{"nothing but junk", []string{`<script>, "q", c d, (e)`}, "Authorization, Content-Type"},
		{"every mark of a token", []string{tokenMarks}, tokenMarks},
		{"white space around the names", []string{" x-a ,\tx-b\t"}, "x-a, x-b"},
		{"names and junk on several lines", []string{"x-a, <b>", "x-c"}, "x-a, x-c"},
		{"a name beyond ASCII", []string{"x-a, x-\u00e9, \u00a0x-b"}, "x-a"},
		{"names without spaces after the commas", []string{"authorization,content-type"}, "authorization, content-type"},
	} {
		if got := allowed(tc.requested...); got != tc.want {
			t.Errorf("%s: %q is allowed as %q, want %q", tc.what, tc.requested, got, tc.want)
		}
	}
	for _, mark := range "\"(/:;<=>?@[\\]{}" { // the ASCII marks that a token may not hold, but for the comma that separates
		if got := allowed("x-a, b" + string(mark) + "d"); got != "x-a" {
			t.Errorf("a name with %q in it is allowed as %q, want only x-a", mark, got)
		}
	}
	if got := probe.requests(); len(got) != 0 {
		t.Errorf("a preflight reached the router: %v", got)
	}
}
