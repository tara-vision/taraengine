//go:build integration

package integration

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// versionHeader is the header with which the proxy stamps every answer it passes on.
const versionHeader = "X-Taraengine-Version"

// chatBody is a request for a few tokens from the model, greedy, so that the same model gives the same words.
func chatBody(t *testing.T, model string, stream bool) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model":       model,
		"messages":    []map[string]string{{"role": "user", "content": "Once upon a time"}},
		"max_tokens":  8,
		"temperature": 0,
		"stream":      stream,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// chatAnswer is the part of a completion that the test looks at.
type chatAnswer struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Timings struct {
		PredictedN int `json:"predicted_n"`
	} `json:"timings"`
}

// complete asks the model for a completion through /v1 under its catalog name and returns what it said.
func (s *scenario) complete(t *testing.T) string {
	t.Helper()
	status, header, data := s.do(t, http.MethodPost, "/v1/chat/completions", chatBody(t, s.entry.Name, false), chatTimeout)
	var chat chatAnswer
	if status != http.StatusOK || json.Unmarshal(data, &chat) != nil || len(chat.Choices) == 0 {
		t.Fatalf("chat: %d %s", status, data)
	}
	if chat.Choices[0].Message.Content == "" || chat.Timings.PredictedN == 0 || chat.Usage.CompletionTokens == 0 {
		t.Fatalf("chat gave no words: %s", data)
	}
	if got, want := header.Get(versionHeader), s.version(t).Version; got == "" || got != want {
		t.Fatalf("the proxy stamped %q on the answer, want the daemon's version %q", got, want)
	}
	return chat.Choices[0].Message.Content
}

// checkChat asks for a completion through /v1 and asserts that the model answers with words.
func checkChat(t *testing.T, s *scenario) {
	s.complete(t)
}

// checkChatStream asks for the same completion as a stream and asserts that the events arrive through the proxy as
// server-sent events, carry words, and end with [DONE].
func checkChatStream(t *testing.T, s *scenario) {
	resp := s.open(t, http.MethodPost, "/v1/chat/completions", chatBody(t, s.entry.Name, true), chatTimeout)
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); resp.StatusCode != http.StatusOK || !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("stream: %d %q", resp.StatusCode, ct)
	}
	var words strings.Builder
	finished := false
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		switch {
		case !ok:
			continue
		case finished:
			t.Fatalf("an event after [DONE]: %q", data)
		case data == "[DONE]":
			finished = true
		default:
			words.WriteString(deltaOf(t, data))
		}
	}
	if err := sc.Err(); err != nil || !finished || words.Len() == 0 {
		t.Fatalf("stream: finished=%v words=%q err=%v", finished, words.String(), err)
	}
}

// deltaOf is the text that one event of a stream adds. The first event and the last carry none.
func deltaOf(t *testing.T, event string) string {
	t.Helper()
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content *string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(event), &chunk); err != nil {
		t.Fatalf("a stream event that is not JSON: %q (%v)", event, err)
	}
	if len(chunk.Choices) == 0 || chunk.Choices[0].Delta.Content == nil {
		return ""
	}
	return *chunk.Choices[0].Delta.Content
}

// checkContextWindow loads the model at another window and back, and asserts that the instance really runs at each
// one: the router re-reads the preset, restarts the model, and the API and the router's own list agree. The other
// window is the larger one, because a load may not ask for less than the window the model runs at already.
func checkContextWindow(t *testing.T, s *scenario) {
	for _, window := range []int{largerWindow, contextWindow} {
		m, err := s.c.Load(within(t, loadTimeout), s.entry.Name, window, false)
		if err != nil || m.Status != "loaded" || m.Context != window {
			t.Fatalf("load at %d tokens: %+v %v", window, m, err)
		}
		assertListedAt(t, s, window)
		loaded, err := s.c.Loaded(within(t, callTimeout))
		if err != nil || len(loaded) != 1 || loaded[0].Context != window || loaded[0].Memory == nil {
			t.Fatalf("loaded at %d tokens: %+v %v", window, loaded, err)
		}
	}
}
