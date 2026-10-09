package llamaserver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// rawRequest sends the bytes as they are, so the client library cannot clean the path or add a header,
// and returns what the server answered. It gives up after three seconds.
func rawRequest(t *testing.T, frontURL, raw string) (*http.Response, string) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(frontURL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer to %q: %v", strings.SplitN(raw, "\r\n", 2)[0], err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp, string(data)
}

func TestProxyAnswers413FromTheLengthBeforeReadingTheBody(t *testing.T) {
	up := newRecordingUpstream(t, respondOK)
	b, front := proxyOver(t, up, PresetModel{Name: "plain", ModelPath: "/m/p.gguf"})
	b.maxBody = 64
	// The headers promise 100 bytes and the body never comes: only an answer sent before any read gets through.
	resp, body := rawRequest(t, front.URL, "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\n"+
		"Content-Type: application/json\r\nContent-Length: 100\r\n\r\n")
	if resp.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(body, "64") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if !resp.Close {
		t.Fatalf("the unread body means the connection must not be reused: %v", resp.Header)
	}
	if n := len(up.requests()); n != 0 {
		t.Fatalf("upstream saw %d requests", n)
	}
}

func TestProxyCapsAChunkedBodyWhileReadingIt(t *testing.T) {
	up := newRecordingUpstream(t, respondOK)
	b, front := proxyOver(t, up, PresetModel{Name: "plain", ModelPath: "/m/p.gguf"})
	b.maxBody = 64
	body := `{"model":"plain","messages":[{"role":"user","content":"` + strings.Repeat("x", 100) + `"}]}`
	req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions", io.NopCloser(strings.NewReader(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = -1 // no length to go by, so the cap has to be enforced while reading
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(string(data), "64") {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	if n := len(up.requests()); n != 0 {
		t.Fatalf("upstream saw %d requests", n)
	}
}

func TestProxyRefusesPathsWithDotSegments(t *testing.T) {
	up := newRecordingUpstream(t, respondOK)
	_, front := proxyOver(t, up, PresetModel{Name: "plain", ModelPath: "/m/p.gguf"})
	targets := []string{
		"/v1/../models/load",
		"/v1/./models",
		"/v1/models/..",
		"/v1/%2e%2e/models/load",
		"/v1/..%2fmodels/load",
	}
	for _, target := range targets {
		resp, body := rawRequest(t, front.URL, "GET "+target+" HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
		if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "not found") {
			t.Errorf("%s: %d %s", target, resp.StatusCode, body)
		}
	}
	if n := len(up.requests()); n != 0 {
		t.Fatalf("the child's key must not open the router's own endpoints, upstream saw %d requests", n)
	}
	if resp, _ := rawRequest(t, front.URL, "GET /v1/models/x.y HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"); resp.StatusCode != http.StatusOK {
		t.Fatalf("a dot inside a name is no dot segment: %d", resp.StatusCode)
	}
}

func TestProxyKeepsHTMLCharactersOfARewrittenBody(t *testing.T) {
	up := newRecordingUpstream(t, respondOK)
	_, front := proxyOver(t, up, PresetModel{Name: "gemma4:12b", ModelPath: "/m/g.gguf"})
	resp := postJSON(t, front.URL+"/v1/chat/completions", `{"model":"gemma4:12b","messages":[{"content":"a <b> & c"}]}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	seen := up.requests()[0]
	if !bytes.Contains(seen.Body, []byte("a <b> & c")) {
		t.Fatalf("the text the client sent must keep its bytes, not be escaped: %s", seen.Body)
	}
	if bytes.HasSuffix(seen.Body, []byte("\n")) || seen.Header.Get("Content-Length") != strconv.Itoa(len(seen.Body)) {
		t.Fatalf("no stray newline, and a length that matches: %q %s", seen.Body, seen.Header.Get("Content-Length"))
	}
}

func TestProxyKeepsHTMLCharactersOfARenamedModelList(t *testing.T) {
	list := `{"data":[{"id":"gemma4-12b","note":"a <b> & c"}],"object":"list"}`
	up := listUpstream(t, list)
	_, front := proxyOver(t, up, PresetModel{Name: "gemma4:12b", ModelPath: "/m/g.gguf"})
	resp, err := http.Get(front.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	var got struct{ Data []struct{ ID, Note string } }
	if err := json.Unmarshal(data, &got); err != nil || got.Data[0].ID != "gemma4:12b" || got.Data[0].Note != "a <b> & c" {
		t.Fatalf("%v %s", err, data)
	}
	if !bytes.Contains(data, []byte("a <b> & c")) {
		t.Fatalf("the router's text keeps its bytes: %s", data)
	}
}
