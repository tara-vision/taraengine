package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"testing"

	"github.com/tara-vision/taraengine/pkg/client"
)

const notRunning = "taraengine is not running at http://127.0.0.1:8272: start it with `taraengine serve` (or `taraengine doctor` for details)"

// timeoutError is a net.Error whose Timeout reports true.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestExplainSaysTheEngineIsNotRunningWhenNothingAnswered(t *testing.T) {
	f := &globalFlags{host: "http://127.0.0.1:8272"}
	_, dialErr := net.Dial("tcp", "127.0.0.1:1")
	if dialErr == nil {
		t.Fatal("something listens on port 1")
	}
	for name, err := range map[string]error{
		"a refused dial":               dialErr,
		"a dial inside a URL error":    &url.Error{Op: "Post", URL: "http://127.0.0.1:8272/api/load", Err: dialErr},
		"a dial that timed out":        &net.OpError{Op: "dial", Net: "tcp", Err: timeoutError{}},
		"a dial inside a wrapped call": fmt.Errorf("client: %w", dialErr),
	} {
		if got := f.explain(err); got == nil || got.Error() != notRunning {
			t.Errorf("%s: got %v", name, got)
		}
	}
}

func TestExplainLeavesEveryOtherErrorAlone(t *testing.T) {
	f := &globalFlags{host: "http://127.0.0.1:8272"}
	if got := f.explain(nil); got != nil {
		t.Errorf("nil stays nil: %v", got)
	}
	for _, err := range []error{
		errors.New("boom"),
		&client.Error{Status: http.StatusNotFound, Message: "unknown model", Hint: "taraengine list shows the catalog"},
		fmt.Errorf("client: decode /api/models: %w", errors.New("invalid character")),
		// A connection that opened and was cut afterwards is no engine that is not running.
		&net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET},
		fmt.Errorf("reading the pull stream: %w", io.ErrUnexpectedEOF),
		&url.Error{Op: "Post", URL: "http://127.0.0.1:8272/api/load", Err: io.EOF},
		client.ErrIncompleteStream,
		context.Canceled,
		context.DeadlineExceeded,
	} {
		if got := f.explain(err); got != err {
			t.Errorf("%v became %v", err, got)
		}
	}
}

func TestTheClientTakesTheKeyFromTheFlagFirstThenTheEnvironment(t *testing.T) {
	client := func(f *globalFlags) *client.Client {
		resolved, err := f.resolved()
		if err != nil {
			t.Fatal(err)
		}
		return resolved.client()
	}
	t.Setenv("TARAENGINE_API_KEY", "from-env")
	if c := client(&globalFlags{host: "http://h:1", apiKey: "from-flag"}); c.APIKey != "from-flag" || c.BaseURL != "http://h:1" {
		t.Errorf("%+v", c)
	}
	if c := client(&globalFlags{host: "http://h:1"}); c.APIKey != "from-env" {
		t.Errorf("%+v", c)
	}
	t.Setenv("TARAENGINE_API_KEY", "")
	withConfigHome(t) // no config file: the defaults have no key
	if c := client(&globalFlags{host: "http://h:1"}); c.APIKey != "" {
		t.Errorf("%+v", c)
	}
}

func TestExplainGivesARefusedKeyTheCLIsOwnHintAndLeavesTheOriginalAlone(t *testing.T) {
	f := &globalFlags{host: "http://127.0.0.1:8272"}
	original := &client.Error{
		Status: http.StatusUnauthorized, Message: "missing or wrong API key", Hint: "send Authorization: Bearer <api_key>",
	}
	for _, part := range []string{"--api-key", "TARAENGINE_API_KEY", "leave out --host", "api_key in the config file"} {
		if !strings.Contains(keyHint, part) {
			t.Errorf("the hint lacks %q: the key of the config file goes only with the address of the file: %s", part, keyHint)
		}
	}
	const want = "missing or wrong API key (" + keyHint + ")"
	for name, err := range map[string]error{"the error itself": original, "the error inside another": fmt.Errorf("pull: %w", original)} {
		got := f.explain(err)
		var refused *client.Error
		if got == nil || got.Error() != want || !errors.As(got, &refused) || refused.Status != http.StatusUnauthorized {
			t.Errorf("%s: got %v, want %q", name, got, want)
		}
	}
	if original.Hint != "send Authorization: Bearer <api_key>" {
		t.Errorf("the error the client returned must not be changed: %+v", original)
	}
	if got := f.explain(&client.Error{Status: http.StatusForbidden, Message: "the Host header is not one of the engine's own addresses",
		Hint: "use the address the engine listens on, or set an api_key"}); got.Error() !=
		"the Host header is not one of the engine's own addresses (use the address the engine listens on, or set an api_key)" {
		t.Errorf("only a 401 is about the key: %v", got)
	}
}
