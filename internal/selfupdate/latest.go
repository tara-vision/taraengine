package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"
)

// maxAnswer caps what the lookup reads of an answer.
const maxAnswer = 1 << 20

// Finder looks the latest stable release up. Each source is asked only when the one before it gave no answer: the
// release page's redirect, which no rate limit stops, then the GitHub API, then engine.tara.vision's version route.
type Finder struct {
	HTTP     *http.Client
	Releases string
	API      string
	Site     string
}

// DefaultFinder asks Tara Vision's own places.
func DefaultFinder() *Finder {
	return &Finder{
		HTTP:     &http.Client{Timeout: 30 * time.Second},
		Releases: "https://github.com/tara-vision/taraengine/releases/latest",
		API:      "https://api.github.com/repos/tara-vision/taraengine/releases/latest",
		Site:     "https://engine.tara.vision/api/version",
	}
}

// Latest is the newest stable release. A pre-release is never the latest: none of the sources names one.
func (f *Finder) Latest(ctx context.Context) (Version, error) {
	var errs []error
	for _, source := range []struct {
		name string
		find func(context.Context) (string, error)
	}{{"the release page", f.fromRedirect}, {"the GitHub API", f.fromAPI}, {"engine.tara.vision", f.fromSite}} {
		tag, err := source.find(ctx)
		if err == nil {
			var v Version
			if v, err = ParseVersion(tag); err == nil {
				return v, nil
			}
		}
		errs = append(errs, fmt.Errorf("%s: %w", source.name, err))
	}
	return Version{}, fmt.Errorf("selfupdate: no release could be found: %w", errors.Join(errs...))
}

// fromRedirect follows the release page to the tag it lands on.
func (f *Finder) fromRedirect(ctx context.Context) (string, error) {
	resp, err := f.get(ctx, f.Releases)
	if err != nil {
		return "", err
	}
	_ = resp.Body.Close()
	if dir, tag := path.Split(resp.Request.URL.Path); strings.HasSuffix(dir, "/releases/tag/") {
		return tag, nil
	}
	return "", fmt.Errorf("it led to %s, which names no release", resp.Request.URL)
}

func (f *Finder) fromAPI(ctx context.Context) (string, error) {
	var answer struct {
		TagName string `json:"tag_name"`
	}
	return answer.TagName, f.getJSON(ctx, f.API, &answer)
}

func (f *Finder) fromSite(ctx context.Context) (string, error) {
	var answer struct {
		Version string `json:"version"`
	}
	return answer.Version, f.getJSON(ctx, f.Site, &answer)
}

func (f *Finder) getJSON(ctx context.Context, url string, v any) error {
	resp, err := f.get(ctx, url)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return json.NewDecoder(io.LimitReader(resp.Body, maxAnswer)).Decode(v)
}

// get asks for url and refuses an answer that is not a success.
func (f *Finder) get(ctx context.Context, url string) (*http.Response, error) {
	return getOK(ctx, f.HTTP, url)
}

func getOK(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "taraengine-update")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%s answered %s", url, resp.Status)
	}
	return resp, nil
}
