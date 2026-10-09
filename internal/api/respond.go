// Package api serves the taraengine HTTP API: /api for control, /v1 proxied to the backend.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/tara-vision/taraengine/internal/backend"
	"github.com/tara-vision/taraengine/pkg/api"
)

// maxBody caps a JSON request body; every body of this API is a few hundred bytes.
const maxBody = 1 << 20

// The hints that go with the errors of the backend.
const (
	hintList    = "taraengine list shows the models the engine knows"
	hintForce   = "load it with force to try again"
	hintDoctor  = "taraengine doctor shows the last lines of the server log"
	hintTimeout = "raise load_timeout in the config for a slow disk"
	hintRetry   = "wait a moment and retry; check taraengine doctor if it persists"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message, hint string) {
	writeJSON(w, status, api.Error{Error: api.ErrorBody{Message: message, Hint: hint}})
}

// catalogError is a failure to read the catalog. It answers 500, where a backend that is down answers 503.
type catalogError struct{ err error }

func (e catalogError) Error() string { return e.err.Error() }

// backendError maps the backend's sentinel errors onto status codes. Any other error may carry whatever the
// backend's code put in it, such as the address the child listens on, so the client is told a fixed text and the
// error goes to the log. A context error of a request that has ended is the client going away, which is nobody's
// fault: it is noted at debug level and nothing is written to a connection that is closed. The same error while
// the request lives comes from the backend, such as a deadline of the router's client, and is answered as any other.
func (s *Server) backendError(w http.ResponseWriter, r *http.Request, err error) {
	var broken catalogError
	switch {
	case r.Context().Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)):
		s.deps.Logger.Debug("the request ended before the backend answered", "error", err)
	case errors.As(err, &broken):
		s.deps.Logger.Error("catalog unreadable", "error", err)
		writeError(w, http.StatusInternalServerError, err.Error(), "")
	case errors.Is(err, backend.ErrUnknownModel):
		writeError(w, http.StatusNotFound, err.Error(), hintList)
	case errors.Is(err, backend.ErrFaulty):
		writeError(w, http.StatusConflict, err.Error(), hintForce)
	case errors.Is(err, backend.ErrLoadFailed):
		writeError(w, http.StatusBadGateway, err.Error(), hintDoctor)
	case errors.Is(err, backend.ErrLoadTimeout):
		writeError(w, http.StatusGatewayTimeout, err.Error(), hintTimeout)
	case errors.Is(err, backend.ErrNotRunning):
		writeError(w, http.StatusServiceUnavailable, err.Error(), hintRetry)
	default:
		s.deps.Logger.Error("backend request failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "the engine backend is unavailable", hintRetry)
	}
}

// readJSON decodes the request body into v, up to maxBody bytes. That the body is JSON at all is settled by
// jsonOnly, in front of every handler.
func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(v)
}

// decodeBody reads the JSON body into v, or answers 400 itself and reports false.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := readJSON(w, r, v); err != nil {
		message, hint := bodyProblem(err)
		writeError(w, http.StatusBadRequest, message, hint)
		return false
	}
	return true
}

// bodyProblem says what is wrong with a body that could not be read: its size, or its JSON.
func bodyProblem(err error) (message, hint string) {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return fmt.Sprintf("the request body is larger than %d bytes", tooBig.Limit), ""
	}
	return "the request body must be a JSON object", `for example {"model": "gemma4:12b"}`
}
