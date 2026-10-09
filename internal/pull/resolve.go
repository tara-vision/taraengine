// Package pull resolves, downloads, verifies and stores model artifacts from Hugging Face.
package pull

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Artifact is one file of a Hugging Face repository as the paths-info API describes it.
type Artifact struct {
	Repo   string
	File   string
	Size   int64
	SHA256 string
}

// Resolver asks the Hugging Face paths-info API for a file's size and LFS sha256.
type Resolver struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

// NewResolver talks to huggingface.co, with an optional token for gated repositories.
func NewResolver(token string) *Resolver {
	return &Resolver{BaseURL: "https://huggingface.co", Token: token, Client: newHTTPClient()}
}

func newHTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

// httpClient returns the configured client, or a default one for a Resolver built as a literal.
func (r *Resolver) httpClient() *http.Client {
	if r.Client != nil {
		return r.Client
	}
	return newHTTPClient()
}

// pathsInfoBody is the JSON the paths-info endpoint reads: the paths to describe.
type pathsInfoBody struct {
	Paths []string `json:"paths"`
}

// pathEntry is one entry of a paths-info answer, in the shape the tree listing uses.
type pathEntry struct {
	Type string `json:"type"`
	Path string `json:"path"`
	Size int64  `json:"size"`
	LFS  *struct {
		OID string `json:"oid"`
	} `json:"lfs"`
}

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidRepo accepts "owner/name" and nothing else.
func ValidRepo(repo string) error {
	if !repoPattern.MatchString(repo) || strings.Contains(repo, "..") {
		return fmt.Errorf("pull: repository %q must be owner/name", repo)
	}
	return nil
}

// Resolve returns the artifact for file in repo. It asks the paths-info endpoint about that one path, because
// the tree listing is paged and a file past its first page would look missing. It fails when the repository or
// the file is missing, and when the file is not stored in LFS: then the API carries no sha256 and nothing could
// be verified.
func (r *Resolver) Resolve(ctx context.Context, repo, file string) (Artifact, error) {
	if err := ValidRepo(repo); err != nil {
		return Artifact{}, err
	}
	// Both errors share one exit: encoding a single path cannot fail, and building the request fails only for a
	// malformed base URL.
	payload, encodeErr := json.Marshal(pathsInfoBody{Paths: []string{file}})
	u := fmt.Sprintf("%s/api/models/%s/paths-info/main", r.BaseURL, repo)
	req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err := errors.Join(encodeErr, reqErr); err != nil {
		return Artifact{}, fmt.Errorf("pull: resolve %s: %w", repo, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if r.Token != "" {
		req.Header.Set("Authorization", "Bearer "+r.Token)
	}
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return Artifact{}, fmt.Errorf("pull: resolve %s: %w", repo, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Artifact{}, fmt.Errorf("pull: resolve %s: Hugging Face answered %d", repo, resp.StatusCode)
	}
	var entries []pathEntry
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&entries); err != nil {
		return Artifact{}, fmt.Errorf("pull: resolve %s: decode response: %w", repo, err)
	}
	for _, e := range entries {
		if e.Type != "file" || e.Path != file {
			continue
		}
		if e.LFS == nil || e.LFS.OID == "" {
			return Artifact{}, fmt.Errorf("pull: %s/%s is not an LFS file, so there is no checksum to verify", repo, file)
		}
		return Artifact{Repo: repo, File: file, Size: e.Size, SHA256: e.LFS.OID}, nil
	}
	return Artifact{}, fmt.Errorf("pull: %s has no file %s", repo, file)
}
