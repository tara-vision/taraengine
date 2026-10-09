package llamaserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/tara-vision/taraengine/internal/llamatest"
)

func proxyHarness(t *testing.T, chatDelay time.Duration) (*llamatest.Server, *httptest.Server) {
	t.Helper()
	dataDir := t.TempDir()
	fake := llamatest.New(t, llamatest.Options{APIKey: "child-key", PresetPath: PresetPath(dataDir), ChatDelay: chatDelay})
	b, err := New(Options{DataDir: dataDir, Attach: fake.URL(), APIKey: "child-key", Build: "bTEST", Version: "0.1.0-test", SleepIdleSeconds: 900,
		State: testStore(dataDir),
		Models: func(context.Context, map[string]int) ([]PresetModel, error) {
			return []PresetModel{{Name: "a", ModelPath: "/m/a.gguf"}}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(b.Proxy())
	t.Cleanup(front.Close)
	return fake, front
}

func streamRequest(t *testing.T, url string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", strings.NewReader(`{"model":"a","stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer the-clients-own-key")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestProxyStreamsChunksAsTheyArrive(t *testing.T) {
	_, front := proxyHarness(t, 150*time.Millisecond)
	start := time.Now()
	resp := streamRequest(t, front.URL)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s (the proxy must replace the client's key with the child's)", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Taraengine-Version"); got != "0.1.0-test" {
		t.Fatalf("version header %q", got)
	}
	sc := bufio.NewScanner(resp.Body)
	var first time.Duration
	var lines int
	for sc.Scan() {
		if !strings.HasPrefix(sc.Text(), "data: ") {
			continue
		}
		if lines == 0 {
			first = time.Since(start)
		}
		lines++
	}
	total := time.Since(start)
	if lines < 4 || first > total/2 {
		t.Fatalf("first chunk after %v of %v with %d lines: the proxy buffered the stream", first, total, lines)
	}
}

func TestProxyEndsWhenRouterDies(t *testing.T) {
	fake, front := proxyHarness(t, 100*time.Millisecond)
	resp := streamRequest(t, front.URL)
	defer func() { _ = resp.Body.Close() }()
	buf := make([]byte, 64)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("first read: %v", err)
	}
	fake.Close()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the response hung after the router died")
	}
}

func TestProxyAnswers503WithoutARouter(t *testing.T) {
	dataDir := t.TempDir()
	b, err := New(Options{DataDir: dataDir, Attach: "http://127.0.0.1:1", APIKey: "x", Build: "bTEST", Version: "0.1.0-test", SleepIdleSeconds: 900,
		State: testStore(dataDir), Logger: quietLogger(),
		Models: func(context.Context, map[string]int) ([]PresetModel, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	b.Proxy().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "not running") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" || rec.Header().Get("X-Taraengine-Version") != "0.1.0-test" {
		t.Fatalf("a 503 must carry a retry hint and the version: %v", rec.Header())
	}
	var body struct {
		Error struct{ Message, Hint string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Hint == "" {
		t.Fatalf("the 503 body must be JSON with a hint: %v %s", err, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "127.0.0.1") {
		t.Fatalf("the child's address must stay out of the answer: %s", rec.Body.String())
	}
}

// seenRequest is what the recording upstream received.
type seenRequest struct {
	Method           string
	URI              string
	Host             string
	Header           http.Header
	TransferEncoding []string
	Body             []byte
}

// recordingUpstream stands in for the router. It answers the router's own endpoints quietly, so the
// backend can start against it, and records everything else before handing it to reply.
type recordingUpstream struct {
	*httptest.Server
	mu   sync.Mutex
	seen []seenRequest
}

func newRecordingUpstream(t *testing.T, reply func(w http.ResponseWriter, r *http.Request)) *recordingUpstream {
	t.Helper()
	u := &recordingUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		case "/models":
			_, _ = w.Write([]byte(`{"data":[],"object":"list"}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.seen = append(u.seen, seenRequest{
			Method: r.Method, URI: r.RequestURI, Host: r.Host, Header: r.Header.Clone(), TransferEncoding: r.TransferEncoding, Body: body,
		})
		u.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		reply(w, r)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *recordingUpstream) requests() []seenRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]seenRequest(nil), u.seen...)
}

// proxyOver starts a backend attached to up that serves models, and a front server that runs its proxy.
func proxyOver(t *testing.T, up *recordingUpstream, models ...PresetModel) (*Backend, *httptest.Server) {
	t.Helper()
	dataDir := t.TempDir()
	b, err := New(Options{DataDir: dataDir, Attach: up.URL, APIKey: "child-key", Build: "bTEST", Version: "0.1.0-test", SleepIdleSeconds: 900,
		State: testStore(dataDir), Logger: quietLogger(),
		Models: func(context.Context, map[string]int) ([]PresetModel, error) { return models, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(b.Proxy())
	t.Cleanup(front.Close)
	return b, front
}

func respondOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func postJSON(t *testing.T, url, body string, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestProxyReplacesTheClientsCredentialsAndRewritesTheModel(t *testing.T) {
	up := newRecordingUpstream(t, respondOK)
	_, front := proxyOver(t, up, PresetModel{Name: "gemma4:12b", ModelPath: "/m/g.gguf"})
	body := "{\n  \"model\": \"gemma4:12b\",\n  \"stream\": true,\n  \"temperature\": 0.2,\n" +
		"  \"messages\": [ { \"role\": \"user\", \"content\": \"<hi> & bye\" } ]\n}"
	header := http.Header{
		"Authorization": {"Bearer the-clients-own-key"},
		"X-Api-Key":     {"the-clients-own-key"},
		"Expect":        {"100-continue"},
		"X-Custom":      {"kept"},
	}
	resp := postJSON(t, front.URL+"/v1/chat/completions?x=1&y=two", body, header)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Taraengine-Version") != "0.1.0-test" {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
	got := up.requests()
	if len(got) != 1 {
		t.Fatalf("upstream saw %d requests", len(got))
	}
	seen := got[0]
	if seen.Method != http.MethodPost || seen.URI != "/v1/chat/completions?x=1&y=two" {
		t.Fatalf("method and target: %s %s", seen.Method, seen.URI)
	}
	if seen.Header.Get("Authorization") != "Bearer child-key" || seen.Header.Get("X-Api-Key") != "" || seen.Header.Get("Expect") != "" {
		t.Fatalf("credentials and hop hints: %v", seen.Header)
	}
	if seen.Header.Get("X-Custom") != "kept" || seen.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("other headers must pass: %v", seen.Header)
	}
	if seen.Host != strings.TrimPrefix(up.URL, "http://") {
		t.Fatalf("host header %q, want the upstream's", seen.Host)
	}
	if seen.Header.Get("Content-Length") != strconv.Itoa(len(seen.Body)) {
		t.Fatalf("content length %q for %d bytes", seen.Header.Get("Content-Length"), len(seen.Body))
	}
	var sent, want map[string]any
	if err := json.Unmarshal(seen.Body, &sent); err != nil {
		t.Fatalf("rewritten body is not JSON: %v %s", err, seen.Body)
	}
	_ = json.Unmarshal([]byte(strings.Replace(body, "gemma4:12b", "gemma4-12b", 1)), &want)
	if !jsonEqual(sent, want) {
		t.Fatalf("body after the rewrite:\n got %s\nwant %s", seen.Body, mustMarshal(want))
	}
}

func jsonEqual(a, b any) bool { return bytes.Equal(mustMarshal(a), mustMarshal(b)) }

func mustMarshal(v any) []byte {
	out, _ := json.Marshal(v)
	return out
}

func TestProxyLeavesBodiesItCannotRewriteAlone(t *testing.T) {
	up := newRecordingUpstream(t, respondOK)
	_, front := proxyOver(t, up, PresetModel{Name: "gemma4:12b", ModelPath: "/m/g.gguf"}, PresetModel{Name: "plain", ModelPath: "/m/p.gguf"})
	bodies := map[string]string{
		"a name without a colon":     `{"model":"plain","messages":[]}`,
		"the section id itself":      `{"model":"gemma4-12b","messages":[]}`,
		"spacing and key order kept": "{ \"messages\" : [ ] ,\n \"model\" : \"plain\" }",
	}
	want := []string{}
	for name, body := range bodies {
		resp := postJSON(t, front.URL+"/v1/chat/completions", body, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", name, resp.StatusCode)
		}
		want = append(want, body)
	}
	seen := up.requests()
	if len(seen) != len(bodies) {
		t.Fatalf("upstream saw %d requests, want %d", len(seen), len(bodies))
	}
	for _, body := range want {
		found := false
		for _, s := range seen {
			found = found || string(s.Body) == body
		}
		if !found {
			t.Errorf("body %q did not arrive byte for byte", body)
		}
	}
}

func TestProxyPassesGetRequestsThrough(t *testing.T) {
	up := newRecordingUpstream(t, respondOK)
	_, front := proxyOver(t, up, PresetModel{Name: "plain", ModelPath: "/m/p.gguf"})
	resp, err := http.Get(front.URL + "/v1/other?x=1")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	seen := up.requests()
	if len(seen) != 1 || seen[0].URI != "/v1/other?x=1" || seen[0].Header.Get("Authorization") != "Bearer child-key" {
		t.Fatalf("seen %+v", seen)
	}
}

func TestProxyRefusesBodiesOverTheCap(t *testing.T) {
	up := newRecordingUpstream(t, respondOK)
	b, front := proxyOver(t, up, PresetModel{Name: "plain", ModelPath: "/m/p.gguf"})
	b.maxBody = 64
	big := `{"model":"plain","messages":[{"role":"user","content":"` + strings.Repeat("x", 100) + `"}]}`
	resp := postJSON(t, front.URL+"/v1/chat/completions", big, nil)
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(string(data), "64") {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	if resp.Header.Get("X-Taraengine-Version") != "0.1.0-test" || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers %v", resp.Header)
	}
	if n := len(up.requests()); n != 0 {
		t.Fatalf("a refused body must not reach the router, upstream saw %d", n)
	}
	ok := postJSON(t, front.URL+"/v1/chat/completions", `{"model":"plain"}`, nil)
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("a body under the cap: %d", ok.StatusCode)
	}
}

func TestProxyAnswers400WhenTheRequestBodyCannotBeRead(t *testing.T) {
	up := newRecordingUpstream(t, respondOK)
	b, _ := proxyOver(t, up, PresetModel{Name: "plain", ModelPath: "/m/p.gguf"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", iotest.ErrReader(errors.New("client went away")))
	b.Proxy().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "request body") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if n := len(up.requests()); n != 0 {
		t.Fatalf("upstream saw %d requests", n)
	}
}

func modelList(ids ...string) string {
	items := make([]string, 0, len(ids))
	for _, id := range ids {
		items = append(items, `{"id":"`+id+`","object":"model","owned_by":"llamacpp","status":{"value":"unloaded"}}`)
	}
	return `{"data":[` + strings.Join(items, ",") + `],"object":"list"}`
}

func listUpstream(t *testing.T, body string) *recordingUpstream {
	t.Helper()
	return newRecordingUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
}

func TestProxyMapsModelIDsBackToCatalogNames(t *testing.T) {
	// Pretty-printed, as a router may send it: the rewritten list is shorter, so a stale length shows.
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, []byte(modelList("gemma4-12b", "plain", "stranger")), "", "    "); err != nil {
		t.Fatal(err)
	}
	up := listUpstream(t, pretty.String())
	_, front := proxyOver(t, up, PresetModel{Name: "gemma4:12b", ModelPath: "/m/g.gguf"}, PresetModel{Name: "plain", ModelPath: "/m/p.gguf"})
	resp, err := http.Get(front.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	var list struct {
		Data []struct {
			ID     string
			Object string
			Status struct{ Value string }
		}
	}
	if err := json.Unmarshal(data, &list); err != nil || len(list.Data) != 2 {
		t.Fatalf("%v %s", err, data)
	}
	if ids := []string{list.Data[0].ID, list.Data[1].ID}; ids[0] != "gemma4:12b" || ids[1] != "plain" {
		t.Fatalf("ids %v: catalog names for ours, and the stranger left out", ids)
	}
	if list.Data[0].Object != "model" || list.Data[0].Status.Value != "unloaded" {
		t.Fatalf("other fields must survive: %+v", list.Data[0])
	}
	if resp.Header.Get("X-Taraengine-Version") != "0.1.0-test" || resp.Header.Get("Content-Length") != strconv.Itoa(len(data)) {
		t.Fatalf("headers after the rewrite: %v for %d bytes", resp.Header, len(data))
	}
}

func TestProxyPassesAListWithNothingToChangeByteForByte(t *testing.T) {
	body := "{\"object\":\"list\",  \"data\":[ {\"id\":\"plain\",\"note\":\"<kept>\"} ]}"
	up := listUpstream(t, body)
	_, front := proxyOver(t, up, PresetModel{Name: "plain", ModelPath: "/m/p.gguf"})
	resp, err := http.Get(front.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if data, _ := io.ReadAll(resp.Body); string(data) != body {
		t.Fatalf("a list that holds only our models under their own names passes as it came: %s", data)
	}
}

func TestProxyPassesAFailedModelListAsItCame(t *testing.T) {
	up := newRecordingUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, modelList("gemma4-12b", "stranger"), http.StatusInternalServerError)
	})
	_, front := proxyOver(t, up, PresetModel{Name: "gemma4:12b", ModelPath: "/m/g.gguf"})
	resp, err := http.Get(front.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(string(data), `"gemma4-12b"`) {
		t.Fatalf("an error answer is no list and passes as it came: %d %s", resp.StatusCode, data)
	}
}

func TestProxyServesColonNamedModelsThroughTheirSectionIDs(t *testing.T) {
	dataDir := t.TempDir()
	fake := llamatest.New(t, llamatest.Options{APIKey: "child-key", PresetPath: PresetPath(dataDir)})
	b, err := New(Options{DataDir: dataDir, Attach: fake.URL(), APIKey: "child-key", Version: "0.1.0-test", State: testStore(dataDir), Logger: quietLogger(), SleepIdleSeconds: 900,
		Models: func(context.Context, map[string]int) ([]PresetModel, error) {
			return []PresetModel{{Name: "gemma4:12b", ModelPath: "/m/g.gguf"}, {Name: "plain", ModelPath: "/m/p.gguf"}}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(b.Proxy())
	t.Cleanup(front.Close)

	resp := postJSON(t, front.URL+"/v1/chat/completions", `{"model":"gemma4:12b","messages":[]}`, nil)
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(data), "Hello from gemma4-12b") {
		t.Fatalf("a colon name must reach the router as its section id: %d %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), `"model":"gemma4-12b"`) {
		t.Fatalf("chat answers pass through unchanged, so they carry the section id: %s", data)
	}
	streamed := postJSON(t, front.URL+"/v1/chat/completions", `{"model":"gemma4:12b","stream":true,"messages":[]}`, nil)
	text, _ := io.ReadAll(streamed.Body)
	if streamed.StatusCode != http.StatusOK || !strings.HasSuffix(strings.TrimSpace(string(text)), "data: [DONE]") {
		t.Fatalf("streamed colon name: %d %s", streamed.StatusCode, text)
	}
	missing := postJSON(t, front.URL+"/v1/chat/completions", `{"model":"gemma4:21b","messages":[]}`, nil)
	data, _ = io.ReadAll(missing.Body)
	if missing.StatusCode != http.StatusNotFound || !strings.Contains(string(data), "the nearest are gemma4:12b, plain") {
		t.Fatalf("an unknown name is answered with the nearest catalog names: %d %s", missing.StatusCode, data)
	}
	if n := countRequests(fake, "POST /v1/chat/completions"); n != 2 {
		t.Fatalf("only the two known names reach the router, it saw %d chats", n)
	}
	listing, err := http.Get(front.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listing.Body.Close() }()
	data, _ = io.ReadAll(listing.Body)
	var list struct{ Data []struct{ ID string } }
	if err := json.Unmarshal(data, &list); err != nil || len(list.Data) != 2 || list.Data[0].ID != "gemma4:12b" || list.Data[1].ID != "plain" {
		t.Fatalf("the listing must carry catalog names: %v %s", err, data)
	}
}

func TestProxyAnswers503WhenTheModelListIsCutShort(t *testing.T) {
	up := newRecordingUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte(`{"data":[`))
	})
	_, front := proxyOver(t, up, PresetModel{Name: "gemma4:12b", ModelPath: "/m/g.gguf"})
	resp, err := http.Get(front.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a list that cannot be read in full must not be passed on half rewritten: %d", resp.StatusCode)
	}
}

func TestProxySendsABufferedUploadWithItsLength(t *testing.T) {
	up := newRecordingUpstream(t, respondOK)
	_, front := proxyOver(t, up, PresetModel{Name: "gemma4:12b", ModelPath: "/m/g.gguf"})
	body := `{"model":"gemma4:12b","messages":[]}`
	req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions", io.NopCloser(strings.NewReader(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = -1 // an upload of unknown length goes out chunked
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	seen := up.requests()
	if len(seen) != 1 || len(seen[0].TransferEncoding) != 0 || seen[0].Header.Get("Content-Length") != strconv.Itoa(len(seen[0].Body)) {
		t.Fatalf("a buffered body must travel with its length, not chunked: %+v", seen)
	}
	if !strings.Contains(string(seen[0].Body), "gemma4-12b") {
		t.Fatalf("body %s", seen[0].Body)
	}
}

func TestProxyRenamesAChunkedModelList(t *testing.T) {
	list := modelList("gemma4-12b", "plain")
	up := newRecordingUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		half := len(list) / 2
		_, _ = w.Write([]byte(list[:half]))
		w.(http.Flusher).Flush() // no length is known once the first part is out, so the answer is chunked
		_, _ = w.Write([]byte(list[half:]))
	})
	_, front := proxyOver(t, up, PresetModel{Name: "gemma4:12b", ModelPath: "/m/g.gguf"}, PresetModel{Name: "plain", ModelPath: "/m/p.gguf"})
	resp, err := http.Get(front.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	var got struct{ Data []struct{ ID string } }
	if err := json.Unmarshal(data, &got); err != nil || len(got.Data) != 2 || got.Data[0].ID != "gemma4:12b" || got.Data[1].ID != "plain" {
		t.Fatalf("%v %s", err, data)
	}
	if resp.Header.Get("Content-Length") != strconv.Itoa(len(data)) {
		t.Fatalf("the rewritten list has a known length: %v for %d bytes", resp.Header, len(data))
	}
}

func TestProxyServesOnlyTheV1Paths(t *testing.T) {
	up := newRecordingUpstream(t, respondOK)
	_, front := proxyOver(t, up, PresetModel{Name: "plain", ModelPath: "/m/p.gguf"})
	for _, target := range []string{"/models/load", "/props?model=plain", "/slots", "/v1", "/health/v1/x", "/"} {
		resp, err := http.Post(front.URL+target, "application/json", strings.NewReader(`{"model":"plain"}`))
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(data), "not found") {
			t.Errorf("%s: %d %s", target, resp.StatusCode, data)
		}
	}
	if n := len(up.requests()); n != 0 {
		t.Fatalf("the child's key must not open the router's own endpoints, upstream saw %d requests", n)
	}
}

func TestProxyLeavesTheAnswerToAHeadRequestAlone(t *testing.T) {
	up := newRecordingUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "123") // what a GET would carry; a HEAD answer has no body
	})
	_, front := proxyOver(t, up, PresetModel{Name: "gemma4:12b", ModelPath: "/m/g.gguf"})
	resp, err := http.Head(front.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Length") != "123" {
		t.Fatalf("a HEAD answer keeps the length of the GET it describes: %d %v", resp.StatusCode, resp.Header)
	}
}
