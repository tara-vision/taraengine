// Package client is a typed client of the taraengine API, used by the CLI and by taracode.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tara-vision/taraengine/pkg/api"
)

// ErrIncompleteStream is matched by errors.Is in exactly three cases: the answer is not an event stream, the stream
// closes before the overall done event, or reading the answer body fails before that event. A failed read keeps its
// own error in the chain too, so a truncated body also matches io.ErrUnexpectedEOF. The pull may still have finished
// on the daemon.
var ErrIncompleteStream = errors.New("client: the stream ended before the pull finished")

// Limits on what the client reads from the daemon, and the wire values it matches.
const (
	eventBufferSize   = 64 << 10 // the first buffer of a pull stream
	maxEventSize      = 1 << 20  // no single event may be longer than this
	maxErrorBody      = 1 << 20  // an error answer is read up to this many bytes
	maxForeignMessage = 300      // the most of a refusal's body that an Error repeats when it is not the engine's
	eventPrefix       = "data: "
	mediaEventStream  = "text/event-stream"
	stageDone         = "done"
	stageError        = "error"
)

// Error is an answer that refused a call, or the failure that ended a pull. NotJSON says that the refusal's body was
// not the engine's error JSON, as a proxy's page or a gateway's own answer is not; Message is then its first
// maxForeignMessage bytes, on one line.
type Error struct {
	Status  int
	Message string
	Hint    string
	NotJSON bool
}

// Error is the message, with the hint in parentheses when there is one.
func (e *Error) Error() string {
	if e.Hint != "" {
		return fmt.Sprintf("%s (%s)", e.Message, e.Hint)
	}
	return e.Message
}

// IsConnectionError reports whether a dial to the address failed or timed out, so that nothing answered there. A call
// that the caller canceled is not one, and neither is a reset, a read error, or a deadline that fires after the dial
// completed.
func IsConnectionError(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial" && !errors.Is(err, context.Canceled)
}

// Client talks to one daemon.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// New is a client without a timeout on the connection; loads and pulls take as long as they take.
func New(baseURL, apiKey string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey, HTTP: &http.Client{Timeout: 0}}
}

// Health is GET /api/health.
func (c *Client) Health(ctx context.Context) (api.Health, error) {
	var out api.Health
	err := c.call(ctx, http.MethodGet, "/api/health", nil, &out)
	return out, err
}

// Version is GET /api/version.
func (c *Client) Version(ctx context.Context) (api.Version, error) {
	var out api.Version
	err := c.call(ctx, http.MethodGet, "/api/version", nil, &out)
	return out, err
}

// Models is GET /api/models.
func (c *Client) Models(ctx context.Context) ([]api.Model, error) {
	var out []api.Model
	err := c.call(ctx, http.MethodGet, "/api/models", nil, &out)
	return out, err
}

// Model is GET /api/models/{name}.
func (c *Client) Model(ctx context.Context, name string) (api.ModelDetail, error) {
	var out api.ModelDetail
	err := c.call(ctx, http.MethodGet, modelPath(name), nil, &out)
	return out, err
}

// Loaded is GET /api/loaded.
func (c *Client) Loaded(ctx context.Context) ([]api.Loaded, error) {
	var out []api.Loaded
	err := c.call(ctx, http.MethodGet, "/api/loaded", nil, &out)
	return out, err
}

// Load is POST /api/load; a window of 0 keeps the preset's context.
func (c *Client) Load(ctx context.Context, name string, window int, force bool) (api.Model, error) {
	var out api.Model
	req := api.LoadRequest{Model: name, Context: window, Force: force}
	err := c.call(ctx, http.MethodPost, "/api/load", req, &out)
	return out, err
}

// Unload is POST /api/unload.
func (c *Client) Unload(ctx context.Context, name string) error {
	return c.call(ctx, http.MethodPost, "/api/unload", api.UnloadRequest{Model: name}, nil)
}

// Delete is DELETE /api/models/{name}.
func (c *Client) Delete(ctx context.Context, name string) error {
	return c.call(ctx, http.MethodDelete, modelPath(name), nil, nil)
}

// Doctor is GET /api/doctor.
func (c *Client) Doctor(ctx context.Context) (api.Doctor, error) {
	var out api.Doctor
	err := c.call(ctx, http.MethodGet, "/api/doctor", nil, &out)
	return out, err
}

// Pull is POST /api/pull as a stream. onEvent, which may be nil, sees every event. An error event ends the pull with
// an *Error, and the overall done event, which carries no file name, ends it without an error. ErrIncompleteStream is
// returned in exactly the cases its doc lists. A refused call returns an *Error, and a line longer than the bound
// returns bufio.ErrTooLong, wrapped, which does not match ErrIncompleteStream. The stream is read as the daemon
// writes it, with one "data: " line per event; see readEvents.
func (c *Client) Pull(ctx context.Context, ref string, onEvent func(api.PullEvent)) error {
	resp, err := c.request(ctx, http.MethodPost, "/api/pull", api.PullRequest{Model: ref})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return decodeError(resp)
	}
	if !isEventStream(resp) {
		return fmt.Errorf("%w: the answer is %q, not an event stream",
			ErrIncompleteStream, resp.Header.Get("Content-Type"))
	}
	return readEvents(resp.Body, onEvent)
}

// isEventStream reports whether the answer is an event stream, whatever parameters its Content-Type carries.
func isEventStream(resp *http.Response) bool {
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return err == nil && mediaType == mediaEventStream
}

// readEvents hands each event of a pull stream to onEvent. It returns nil at the overall done event and an *Error at
// an error event. A stream that closes cleanly before either returns ErrIncompleteStream alone, and a read that fails
// before either returns ErrIncompleteStream with the read error wrapped as well. A line beyond the bound returns
// bufio.ErrTooLong, wrapped and without the sentinel.
//
// The reader understands the stream as the daemon writes it, one event on one line that begins with "data: ". Every
// other line is skipped, so it does not interpret an event that is spread over several data lines, a data line
// without the space after the colon, or the event, id and retry fields of the event-stream format.
func readEvents(body io.Reader, onEvent func(api.PullEvent)) error {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, eventBufferSize), maxEventSize)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), eventPrefix)
		if !ok {
			continue
		}
		var e api.PullEvent
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			return fmt.Errorf("client: pull event: %w", err)
		}
		if onEvent != nil {
			onEvent(e)
		}
		switch {
		case e.Stage == stageError:
			return &Error{Status: http.StatusBadGateway, Message: messageOr(e.Error, http.StatusBadGateway)}
		case e.Stage == stageDone && e.File == "":
			return nil
		}
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return fmt.Errorf("reading the pull stream: %w", err)
		}
		return fmt.Errorf("%w: reading the pull stream: %w", ErrIncompleteStream, err)
	}
	return ErrIncompleteStream
}

// request sends one call. A body is encoded as JSON and then carries its content type.
func (c *Client) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("client: encode request: %w", err)
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, payload)
	if err != nil {
		return nil, fmt.Errorf("client: %w", err)
	}
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.HTTP.Do(req)
}

// call makes one call and decodes a successful answer into out, unless out is nil.
func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	resp, err := c.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return decodeError(resp)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("client: decode %s: %w", path, err)
	}
	return nil
}

// decodeError turns a refused answer into an *Error. A body is the engine's when it decodes as its error JSON with a
// message that is not blank, and the message and the hint come from there. Any other body is marked NotJSON and
// shortened to one line, and a body with no text leaves the HTTP status text.
func decodeError(resp *http.Response) error {
	// A body that cannot be read in full still leaves the status, so the read error is not reported.
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var body api.Error
	if json.Unmarshal(data, &body) == nil && strings.TrimSpace(body.Error.Message) != "" {
		return &Error{Status: resp.StatusCode, Message: strings.TrimSpace(body.Error.Message), Hint: body.Error.Hint}
	}
	message := messageOr(oneLine(data, maxForeignMessage), resp.StatusCode)
	return &Error{Status: resp.StatusCode, Message: message, NotJSON: true}
}

// oneLine is text as one line of at most limit bytes: every run of carriage returns, line feeds and tabs becomes one
// space, every other control character goes, the result is trimmed, and a cut never splits a character.
func oneLine(data []byte, limit int) string {
	var b strings.Builder
	gap := false
	for _, r := range string(data) {
		switch {
		case r == '\r' || r == '\n' || r == '\t':
			gap = true
			continue
		case unicode.IsControl(r):
			continue
		}
		if gap && b.Len() > 0 {
			b.WriteByte(' ')
		}
		gap = false
		b.WriteRune(r)
	}
	text := strings.TrimSpace(b.String())
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return strings.TrimSpace(text[:cut])
}

// messageOr is the message without its surrounding white space, or the HTTP status text when it is blank.
func messageOr(message string, status int) string {
	if trimmed := strings.TrimSpace(message); trimmed != "" {
		return trimmed
	}
	return statusMessage(status)
}

// statusMessage is the HTTP status text of status, or the bare code for a status that has no text.
func statusMessage(status int) string {
	if text := http.StatusText(status); text != "" {
		return text
	}
	return "HTTP status " + strconv.Itoa(status)
}

// modelPath is the route of one model, with the name escaped as a single path segment.
func modelPath(name string) string {
	return "/api/models/" + url.PathEscape(name)
}
