package selfupdate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// sources is a release page, a GitHub API and a site, each answering with what the test set, counting what it is
// asked.
type sources struct {
	tag, apiBody, siteBody string // empty answers 404
	apiStatus              int
	apiCalls, siteCalls    atomic.Int32
}

func (s *sources) finder(t *testing.T) *Finder {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /releases/latest", func(w http.ResponseWriter, r *http.Request) {
		switch s.tag {
		case "":
			http.NotFound(w, r)
			return
		case "no redirect":
			_, _ = w.Write([]byte("<html>a page that names no release</html>"))
			return
		}
		http.Redirect(w, r, "/releases/tag/"+s.tag, http.StatusFound)
	})
	mux.HandleFunc("GET /releases/tag/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) })
	mux.HandleFunc("GET /api", func(w http.ResponseWriter, _ *http.Request) {
		s.apiCalls.Add(1)
		if s.apiStatus != 0 {
			w.WriteHeader(s.apiStatus)
		}
		_, _ = w.Write([]byte(s.apiBody))
	})
	mux.HandleFunc("GET /site", func(w http.ResponseWriter, _ *http.Request) {
		s.siteCalls.Add(1)
		_, _ = w.Write([]byte(s.siteBody))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Finder{HTTP: srv.Client(), Releases: srv.URL + "/releases/latest", API: srv.URL + "/api", Site: srv.URL + "/site"}
}

func TestLatestFollowsTheReleasePageFirst(t *testing.T) {
	s := &sources{tag: "v0.3.1"}
	v, err := s.finder(t).Latest(context.Background())
	if err != nil || v.String() != "0.3.1" || s.apiCalls.Load() != 0 {
		t.Fatalf("%v %v, api calls %d", v, err, s.apiCalls.Load())
	}
}

func TestLatestFallsBackToTheAPIThenTheSite(t *testing.T) {
	s := &sources{apiBody: `{"tag_name":"v0.3.2"}`}
	if v, err := s.finder(t).Latest(context.Background()); err != nil || v.String() != "0.3.2" {
		t.Fatalf("the API: %v %v", v, err)
	}
	s = &sources{apiStatus: http.StatusForbidden, apiBody: `{"message":"rate limit"}`, siteBody: `{"version":"0.3.3","name":"taraengine"}`}
	if v, err := s.finder(t).Latest(context.Background()); err != nil || v.String() != "0.3.3" {
		t.Fatalf("the site: %v %v", v, err)
	}
	s = &sources{tag: "no redirect", siteBody: `{"version":"0.3.4"}`}
	if v, err := s.finder(t).Latest(context.Background()); err != nil || v.String() != "0.3.4" {
		t.Fatalf("a release page that leads nowhere: %v %v", v, err)
	}
	s = &sources{tag: "nightly", apiBody: `{"tag_name":""}`, siteBody: `not json`}
	_, err := s.finder(t).Latest(context.Background())
	for _, want := range []string{"release page", "GitHub API", "engine.tara.vision"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("an error that names every source, missing %q: %v", want, err)
		}
	}
}

func TestLatestReportsSourcesThatCannotBeReached(t *testing.T) {
	f := &Finder{HTTP: http.DefaultClient, Releases: "http://127.0.0.1:1/r", API: "http://127.0.0.1:1/a", Site: "http://127.0.0.1:1/s"}
	if _, err := f.Latest(context.Background()); err == nil {
		t.Fatal("nothing answers")
	}
	bad := &Finder{HTTP: http.DefaultClient, Releases: "::bad", API: "::bad", Site: "::bad"}
	if _, err := bad.Latest(context.Background()); err == nil {
		t.Fatal("addresses that are no URLs")
	}
}

func TestDefaultFinderAsksTaraVisionsOwnPlaces(t *testing.T) {
	f := DefaultFinder()
	if f.Releases != "https://github.com/tara-vision/taraengine/releases/latest" ||
		f.API != "https://api.github.com/repos/tara-vision/taraengine/releases/latest" ||
		f.Site != "https://engine.tara.vision/api/version" || f.HTTP == nil || f.HTTP.Timeout == 0 {
		t.Fatalf("%+v", f)
	}
}
