package llamaserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httputil"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/tara-vision/taraengine/internal/backend"
)

const (
	// maxRequestBody is the largest /v1 request body the proxy buffers in order to rewrite its model
	// field. It leaves room for a long prompt and a few images as base64; a body over it is refused
	// with 413 instead of being held in memory without bound.
	maxRequestBody = 32 << 20
	// maxListAnswer is the largest /v1/models answer the proxy filters; a longer one is refused.
	maxListAnswer = 1 << 20
	// maxNameShown bounds how much of an unknown model name an answer repeats.
	maxNameShown  = 80
	versionHeader = "X-Taraengine-Version"
	// allowedMethods are the methods of the proxied /v1 routes; the proxy refuses every other one.
	allowedMethods = "GET, HEAD, POST"
	// The hints of the API for an unknown and for a faulty model.
	hintList  = "taraengine list shows the models the engine knows"
	hintForce = "load it with force to try again"
)

// errModelList is a model list from the router that the proxy cannot filter.
var errModelList = errors.New("llamaserver: the router's model list cannot be filtered")

// Proxy forwards /v1 traffic to the router with the child's key, flushing every chunk so streams arrive
// as they are produced. It holds the traffic to the catalog: a POST must name a pulled model that is not
// marked faulty in its model field, where the catalog name becomes the router's section id on the way in,
// and a /v1/models answer lists the pulled models under their catalog names and nothing else. The
// router's CORS headers are dropped, because the router approves every origin; a preflight, or any
// method but GET, HEAD and POST, is refused here and never forwarded. Every other answer passes
// unchanged. Only /v1/ paths are served. Without a running router it answers 503.
func (b *Backend) Proxy() http.Handler {
	rp := &httputil.ReverseProxy{
		Rewrite:        b.rewriteOutgoing,
		FlushInterval:  -1,
		ModifyResponse: b.modifyResponse,
		ErrorHandler:   b.proxyFailed,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !servesPath(r.URL.Path) {
			b.fail(w, http.StatusNotFound, "not found", "") // the child's key must never open the router's own endpoints
			return
		}
		if !servesMethod(r.Method) {
			w.Header().Set("Allow", allowedMethods)
			b.fail(w, http.StatusMethodNotAllowed, "the method "+r.Method+" is not served under /v1", "")
			return
		}
		if sup := b.supervisor(); sup != nil && !sup.Running() {
			b.unavailable(w)
			return
		}
		out, ok := b.bufferRequest(w, r)
		if !ok {
			return
		}
		rp.ServeHTTP(w, out)
	})
}

// servesMethod reports whether the method is one the /v1 routes use. OPTIONS is not: the daemon answers
// the preflights it approves before the proxy, and a preflight that reached the router would be approved
// for any origin.
func servesMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodPost
}

// servesPath reports whether the proxy answers for the path: it lies under /v1/ and has no dot segment,
// which could lead out of /v1/ once the router resolves it. The path is the decoded one, so an encoded
// dot is caught as well.
func servesPath(urlPath string) bool {
	if !strings.HasPrefix(urlPath, "/v1/") {
		return false
	}
	for _, segment := range strings.Split(urlPath, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// rewriteOutgoing aims the request at the router and swaps the client's credentials for the child's.
// Expect is dropped because the body is already in hand; asking the router for a 100 Continue would
// only add a wait. A model list is asked for without the client's Accept-Encoding, so the transport
// receives it in a form it decodes itself and the list can always be filtered.
func (b *Backend) rewriteOutgoing(pr *httputil.ProxyRequest) {
	pr.SetURL(b.target)
	pr.Out.Header.Set("Authorization", "Bearer "+b.opts.APIKey)
	pr.Out.Header.Del("X-Api-Key")
	pr.Out.Header.Del("Expect")
	if pr.In.Method == http.MethodGet && pr.In.URL.Path == "/v1/models" {
		pr.Out.Header.Del("Accept-Encoding")
	}
}

// bufferRequest reads a POST body, up to the cap, holds it to the catalog, and returns a copy of the
// request that carries the body with the model's router id. A body that cannot be read, is over the cap
// or is refused by the catalog is answered here, and ok is false.
func (b *Backend) bufferRequest(w http.ResponseWriter, r *http.Request) (out *http.Request, ok bool) {
	if r.Method != http.MethodPost {
		return r, true
	}
	if r.ContentLength > b.maxBody {
		w.Header().Set("Connection", "close") // the body stays unread, so the connection cannot be reused
		b.tooLarge(w, b.maxBody)
		return nil, false
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, b.maxBody))
	if err != nil {
		b.refuseBody(w, err)
		return nil, false
	}
	if data, ok = b.admit(w, data); !ok {
		return nil, false
	}
	out = r.Clone(r.Context())
	out.Body = io.NopCloser(bytes.NewReader(data))
	out.ContentLength = int64(len(data))
	out.TransferEncoding = nil // the length is known now, so the body no longer goes out chunked
	out.Header.Set("Content-Length", strconv.Itoa(len(data)))
	return out, true
}

// admit holds a request body to the catalog. It must be a JSON object whose model field names a pulled
// model, by its catalog name or by its router id, that the state does not mark faulty. A body that does
// not is answered here, with 400, 404 or 409. The body that goes on carries the router id, and comes
// back byte for byte when the name already is the id.
func (b *Backend) admit(w http.ResponseWriter, body []byte) ([]byte, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		b.fail(w, http.StatusBadRequest, "the request body must be a JSON object with a model field", "")
		return nil, false
	}
	var asked string
	if json.Unmarshal(fields["model"], &asked) != nil || asked == "" {
		b.fail(w, http.StatusBadRequest, "the request body must name a model in its model field", "")
		return nil, false
	}
	r := b.currentRoster()
	name, known := r.resolve(asked)
	if !known {
		b.unknownModel(w, asked, r)
		return nil, false
	}
	if _, faulty, _ := b.lookup(name); faulty {
		b.fail(w, http.StatusConflict, fmt.Errorf("%w: %s", backend.ErrFaulty, name).Error(), hintForce)
		return nil, false
	}
	if id := SectionID(name); id != asked {
		fields["model"] = marshalPlain(id)
		return marshalPlain(fields), true
	}
	return body, true
}

// resolve is the catalog name a request means: the name itself, or the model whose router id it is.
func (r roster) resolve(asked string) (string, bool) {
	if _, ok := r.byName[asked]; ok {
		return asked, true
	}
	name, ok := r.nameOf[asked]
	return name, ok
}

// unknownModel answers a name the catalog does not serve with the nearest names it does.
func (b *Backend) unknownModel(w http.ResponseWriter, asked string, r roster) {
	shown := asked
	if len(shown) > maxNameShown {
		shown = shown[:maxNameShown] + "..."
	}
	message := fmt.Sprintf("model %q is not a pulled model", shown)
	if nearest := nearestNames(asked, slices.Sorted(maps.Keys(r.byName)), 3); len(nearest) > 0 {
		message += "; the nearest are " + strings.Join(nearest, ", ")
	} else {
		message += ", and no model is pulled yet"
	}
	b.fail(w, http.StatusNotFound, message, hintList)
}

// refuseBody answers a body that could not be read in full: too long for the cap, which a body without a
// declared length only shows while it is read, or cut off.
func (b *Backend) refuseBody(w http.ResponseWriter, err error) {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		b.tooLarge(w, tooBig.Limit)
		return
	}
	b.fail(w, http.StatusBadRequest, "could not read the request body", "")
}

func (b *Backend) tooLarge(w http.ResponseWriter, limit int64) {
	message := "the request body is larger than " + strconv.FormatInt(limit, 10) + " bytes"
	b.fail(w, http.StatusRequestEntityTooLarge, message, "")
}

// modifyResponse drops the router's CORS headers, stamps the version on every answer and holds a
// /v1/models answer to the catalog.
func (b *Backend) modifyResponse(resp *http.Response) error {
	dropCORS(resp.Header)
	b.stamp(resp.Header)
	if b.isModelList(resp) {
		return b.filterListedModels(resp)
	}
	return nil
}

// dropCORS removes every Access-Control-* header. The router approves every origin; which pages may read
// an answer is the daemon's decision alone.
func dropCORS(h http.Header) {
	for key := range h {
		if strings.HasPrefix(key, "Access-Control-") {
			h.Del(key)
		}
	}
}

// isModelList reports whether resp is the 200 answer to a GET of /v1/models. An answer to HEAD has no
// body to filter.
func (b *Backend) isModelList(resp *http.Response) bool {
	return resp.Request.Method == http.MethodGet && resp.StatusCode == http.StatusOK &&
		resp.Request.URL.Path == path.Join(b.target.Path, "/v1/models")
}

// filterListedModels buffers a model list, up to the cap, and replaces the answer with the pulled models
// under their catalog names. A list that is encoded, over the cap or not a list is refused, since it
// cannot be held to the catalog.
func (b *Backend) filterListedModels(resp *http.Response) error {
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return fmt.Errorf("%w: it is encoded as %s", errModelList, encoding)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxListAnswer+1))
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if len(data) > maxListAnswer {
		return fmt.Errorf("%w: it is longer than %d bytes", errModelList, maxListAnswer)
	}
	data, ok := filterModelList(data, b.currentRoster())
	if !ok {
		return fmt.Errorf("%w: it is not a list of models", errModelList)
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
	resp.ContentLength = int64(len(data))
	resp.Header.Set("Content-Length", strconv.Itoa(len(data)))
	resp.Header.Del("Content-Encoding")
	return nil
}

// proxyFailed answers a request the router could not take, or an answer the proxy could not pass on. The
// detail goes to the log: the child's address is not the client's business.
func (b *Backend) proxyFailed(w http.ResponseWriter, r *http.Request, err error) {
	b.opts.Logger.Debug("proxy error", "path", r.URL.Path, "error", err)
	if errors.Is(err, errModelList) {
		b.fail(w, http.StatusBadGateway, "the engine backend sent a model list that cannot be read",
			"check taraengine doctor")
		return
	}
	b.unavailable(w)
}

func (b *Backend) unavailable(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "2")
	b.fail(w, http.StatusServiceUnavailable, "the engine backend is not running",
		"wait a moment and retry; check taraengine doctor if it persists")
}

// fail answers with an OpenAI-style error body: the message, the type and the status code, and a hint
// when there is one.
func (b *Backend) fail(w http.ResponseWriter, status int, message, hint string) {
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    int    `json:"code"`
			Hint    string `json:"hint,omitempty"`
		} `json:"error"`
	}
	body.Error.Message, body.Error.Code, body.Error.Hint = message, status, hint
	switch {
	case status == http.StatusServiceUnavailable:
		body.Error.Type = "unavailable_error"
	case status >= http.StatusInternalServerError:
		body.Error.Type = "server_error"
	default:
		body.Error.Type = "invalid_request_error"
	}
	data, _ := json.Marshal(body) // strings and a number always marshal
	b.stamp(w.Header())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func (b *Backend) stamp(h http.Header) {
	if b.opts.Version != "" {
		h.Set(versionHeader, b.opts.Version)
	}
}

// marshalPlain is json.Marshal without HTML escaping, so text the client or the router sent keeps its bytes
// when only a name is changed. What it is given always encodes: a string, or values that were decoded.
func marshalPlain(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")) // Encode ends the value with a newline
}

// filterModelList keeps the models of a model list that the roster knows, under their catalog names, and
// leaves out every other item: the router may list models it found elsewhere, and an item without a
// text id is no model of ours. A list in which nothing changes comes back byte for byte. It reports
// false for an answer that is not a model list.
func filterModelList(body []byte, r roster) ([]byte, bool) {
	var doc map[string]json.RawMessage
	var items []map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil || json.Unmarshal(doc["data"], &items) != nil {
		return nil, false
	}
	kept := make([]map[string]json.RawMessage, 0, len(items))
	changed := false
	for _, item := range items {
		var id string
		_ = json.Unmarshal(item["id"], &id) // an id that is no text stays empty, and no model has that id
		name, ours := r.nameOf[id]
		switch {
		case !ours:
			changed = true
			continue
		case name != id:
			item["id"] = marshalPlain(name)
			changed = true
		}
		kept = append(kept, item)
	}
	if !changed {
		return body, true
	}
	doc["data"] = marshalPlain(kept)
	return marshalPlain(doc), true
}
