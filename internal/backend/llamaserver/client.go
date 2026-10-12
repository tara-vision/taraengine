package llamaserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tara-vision/taraengine/internal/backend"
)

// RouterModel is one entry of the router's GET /models.
type RouterModel struct {
	ID       string
	Status   string // unloaded, loading, loaded, sleeping, downloading
	ExitCode int
	Failed   bool
	Preset   string
}

// RouterError is an error body the router returned.
type RouterError struct {
	Code    int
	Message string
}

func (e *RouterError) Error() string { return fmt.Sprintf("router answered %d: %s", e.Code, e.Message) }

// Client talks to one llama-server in router mode.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// NewClient is a client with a 30-second timeout per call; chat traffic does not go through it.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

type modelsResponse struct {
	Data []struct {
		ID     string `json:"id"`
		Status struct {
			Value    string `json:"value"`
			ExitCode int    `json:"exit_code"`
			Failed   bool   `json:"failed"`
			Preset   string `json:"preset"`
		} `json:"status"`
	} `json:"data"`
}

type propsResponse struct {
	DefaultGenerationSettings struct {
		NCtx int `json:"n_ctx"`
	} `json:"default_generation_settings"`
	ModelPath        string `json:"model_path"`
	ChatTemplate     string `json:"chat_template"`
	ChatTemplateCaps struct {
		SupportsTools bool `json:"supports_tools"`
	} `json:"chat_template_caps"`
	Modalities struct {
		Vision bool `json:"vision"`
	} `json:"modalities"`
}

// Health is GET /health; the router answers ok even with nothing loaded.
func (c *Client) Health(ctx context.Context) error {
	return c.call(ctx, http.MethodGet, "/health", nil, nil)
}

// Models is GET /models, with ?reload=1 to make the router re-read its preset file.
func (c *Client) Models(ctx context.Context, reload bool) ([]RouterModel, error) {
	path := "/models"
	if reload {
		path += "?reload=1"
	}
	var resp modelsResponse
	if err := c.call(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	out := make([]RouterModel, 0, len(resp.Data))
	for _, m := range resp.Data {
		out = append(out, RouterModel{
			ID:       m.ID,
			Status:   m.Status.Value,
			ExitCode: m.Status.ExitCode,
			Failed:   m.Status.Failed,
			Preset:   m.Status.Preset,
		})
	}
	return out, nil
}

// Load is POST /models/load; the router answers at once and loads in the background.
func (c *Client) Load(ctx context.Context, id string) error {
	err := c.call(ctx, http.MethodPost, "/models/load", map[string]string{"model": id}, nil)
	var rerr *RouterError
	if errorsAs(err, &rerr) && rerr.Code == http.StatusNotFound {
		return fmt.Errorf("%w: %s", backend.ErrUnknownModel, id)
	}
	return err
}

// Unload is POST /models/unload. A model that is not running is already unloaded.
func (c *Client) Unload(ctx context.Context, id string) error {
	err := c.call(ctx, http.MethodPost, "/models/unload", map[string]string{"model": id}, nil)
	var rerr *RouterError
	if errorsAs(err, &rerr) && rerr.Code == http.StatusBadRequest && strings.Contains(rerr.Message, "not running") {
		return nil
	}
	return err
}

// Props is GET /props?model=<id>&autoload=false for a running model. With autoload off the router answers 400 for a
// model it does not run instead of loading it, so reading the props never starts a model.
func (c *Client) Props(ctx context.Context, id string) (backend.Props, error) {
	var resp propsResponse
	if err := c.call(ctx, http.MethodGet, "/props?model="+url.QueryEscape(id)+"&autoload=false", nil, &resp); err != nil {
		return backend.Props{}, err
	}
	tpl := resp.ChatTemplate
	return backend.Props{
		ContextLength: resp.DefaultGenerationSettings.NCtx,
		ModelPath:     resp.ModelPath,
		SupportsTools: resp.ChatTemplateCaps.SupportsTools,
		Vision:        resp.Modalities.Vision,
		Thinking:      strings.Contains(tpl, "enable_thinking") || strings.Contains(tpl, "<think>"),
	}, nil
}

func (c *Client) call(ctx context.Context, method, path string, body any, out any) error {
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("llamaserver: %w", err)
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, payload)
	if err != nil {
		return fmt.Errorf("llamaserver: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() == nil && dialFailed(err) {
			return fmt.Errorf("%w: llama-server takes no connection for %s %s", backend.ErrNotRunning, method, path)
		}
		return fmt.Errorf("llamaserver: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("llamaserver: %s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 300 {
		return routerError(resp.StatusCode, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("llamaserver: %s %s: decode: %w", method, path, err)
		}
	}
	return nil
}

func routerError(status int, data []byte) error {
	var body struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &body)
	code := body.Error.Code
	if code == 0 {
		code = status
	}
	msg := body.Error.Message
	if msg == "" {
		msg = http.StatusText(status)
	}
	return &RouterError{Code: code, Message: msg}
}

func errorsAs(err error, target **RouterError) bool { return errors.As(err, target) }

// dialFailed reports a connection that could not be made: nothing listens where the router should. The
// error it becomes leaves the address out, because the API shows it to the client.
func dialFailed(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}
