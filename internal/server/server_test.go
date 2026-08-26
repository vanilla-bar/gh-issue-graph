package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/vanilla-bar/gh-issue-graph/internal/github"
	"github.com/vanilla-bar/gh-issue-graph/internal/graph"
)

type fakeLoader struct {
	input   graph.Input
	err     error
	options graph.SearchOptions
	phases  []string
}

func (f *fakeLoader) Load(ctx context.Context, options graph.SearchOptions, progress github.Progress) (graph.Input, error) {
	f.options = options
	for _, phase := range f.phases {
		progress(phase, 1, 2, 3)
	}
	return f.input, f.err
}

// A loader that can also hand over a body. fakeLoader deliberately cannot, so
// the two together cover both sides of the optional interface.
type detailingLoader struct {
	fakeLoader
	body    string
	created time.Time
	err     error
	askedID string
}

func (d *detailingLoader) Detail(ctx context.Context, id string) (graph.Detail, error) {
	d.askedID = id
	if d.err != nil {
		return graph.Detail{}, d.err
	}
	return graph.Detail{ID: id, BodyHTML: d.body, CreatedAt: d.created}, nil
}

func TestDetailEndpointReturnsTheBody(t *testing.T) {
	loader := &detailingLoader{
		body:    "<p>why this matters</p>",
		created: time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC),
	}
	server := httptest.NewServer((&Server{Loader: loader}).handler())
	defer server.Close()

	response, err := http.Get(server.URL + "/api/v1/detail?id=I_100")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	var detail graph.Detail
	if err := json.NewDecoder(response.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	if detail.BodyHTML != loader.body {
		t.Fatalf("bodyHtml = %q, want %q", detail.BodyHTML, loader.body)
	}
	// The drawer dates the body's card with this; dropping it would leave the
	// first card the only one with no time on it.
	if !detail.CreatedAt.Equal(loader.created) {
		t.Fatalf("createdAt = %v, want %v", detail.CreatedAt, loader.created)
	}
	if loader.askedID != "I_100" {
		t.Fatalf("the loader was asked for %q, want I_100", loader.askedID)
	}
}

// A body arrives as HTML and goes into the page as HTML. The CSP is what keeps
// that safe, so it has to be on this response too — not only on the document.
func TestDetailEndpointCarriesTheContentSecurityPolicy(t *testing.T) {
	server := httptest.NewServer((&Server{Loader: &detailingLoader{body: "<p>hi</p>"}}).handler())
	defer server.Close()

	response, err := http.Get(server.URL + "/api/v1/detail?id=I_100")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	policy := response.Header.Get("Content-Security-Policy")
	for _, want := range []string{"script-src 'self'", "default-src 'self'"} {
		if !strings.Contains(policy, want) {
			t.Fatalf("policy %q is missing %q", policy, want)
		}
	}
}

// The demo loader can do this; a loader that cannot should say so rather than
// leaving the page waiting.
func TestDetailEndpointSaysWhenTheLoaderCannot(t *testing.T) {
	server := httptest.NewServer((&Server{Loader: &fakeLoader{}}).handler())
	defer server.Close()

	response, err := http.Get(server.URL + "/api/v1/detail?id=I_100")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", response.StatusCode)
	}
}

func TestDetailEndpointNeedsAnID(t *testing.T) {
	server := httptest.NewServer((&Server{Loader: &detailingLoader{}}).handler())
	defer server.Close()

	response, err := http.Get(server.URL + "/api/v1/detail")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.StatusCode)
	}
}

func TestDetailEndpointReportsAFailedFetch(t *testing.T) {
	server := httptest.NewServer((&Server{Loader: &detailingLoader{err: errors.New("gh exploded")}}).handler())
	defer server.Close()

	response, err := http.Get(server.URL + "/api/v1/detail?id=I_100")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", response.StatusCode)
	}
}

func TestParseOptionsDefaultsToEveryScope(t *testing.T) {
	options := ParseOptions(url.Values{})
	if !options.Assigned || !options.Authored || !options.Mentioned {
		t.Fatalf("options = %+v, want every scope on by default", options)
	}
	if options.IncludeClosed || options.IncludeXref {
		t.Fatalf("options = %+v, want closed issues and cross references off by default", options)
	}
}

func TestParseOptionsHonoursExplicitScopes(t *testing.T) {
	options := ParseOptions(url.Values{"assigned": {"1"}, "authored": {"0"}, "mentioned": {"0"}})
	if !options.Assigned || options.Authored || options.Mentioned {
		t.Fatalf("options = %+v, want only the assigned scope", options)
	}
}

func TestParseOptionsReadsRepoAndQuery(t *testing.T) {
	options := ParseOptions(url.Values{"repo": {" hoge/fuga "}, "q": {" label:bug "}, "closed": {"1"}, "xref": {"1"}})
	if options.Repo != "hoge/fuga" || options.Query != "label:bug" {
		t.Fatalf("options = %+v, want trimmed repo and query", options)
	}
	if !options.IncludeClosed || !options.IncludeXref {
		t.Fatalf("options = %+v, want both toggles on", options)
	}
}

func TestPercentForRisesMonotonicallyThroughThePhases(t *testing.T) {
	previous := -1
	for _, phase := range []string{github.PhaseSearch, github.PhaseExpand, github.PhasePulls, github.PhaseCrossRef} {
		start := percentFor(phase, 0, 1)
		end := percentFor(phase, 1, 1)
		if start < previous {
			t.Fatalf("phase %q starts at %d%%, below the previous phase's end %d%%", phase, start, previous)
		}
		if end < start {
			t.Fatalf("phase %q ends at %d%%, below its own start %d%%", phase, end, start)
		}
		previous = end
	}
	if previous != 100 {
		t.Fatalf("the last phase ends at %d%%, want 100", previous)
	}
}

func TestPercentForClampsOverflow(t *testing.T) {
	if got := percentFor(github.PhaseSearch, 5, 2); got != 25 {
		t.Fatalf("percentFor with current beyond total = %d, want the phase ceiling 25", got)
	}
	if got := percentFor(github.PhaseSearch, 1, 0); got != 0 {
		t.Fatalf("percentFor with a zero total = %d, want 0", got)
	}
}

func TestGraphEndpointStreamsProgressThenResult(t *testing.T) {
	loader := &fakeLoader{
		phases: []string{github.PhaseSearch, github.PhasePulls},
		input: graph.Input{
			Issues:       []*graph.Issue{{ID: "I_1", Number: 1, State: "OPEN", RepositoryID: "R", Repository: "hoge/fuga"}},
			Repositories: map[string]*graph.Repository{"R": {ID: "R", NameWithOwner: "hoge/fuga"}},
		},
	}
	server := New(loader, "test")

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/graph?repo=hoge/fuga", nil)
	server.handleGraph(recorder, request)

	if got := recorder.Header().Get("Content-Type"); got != "application/x-ndjson" {
		t.Fatalf("content type = %q, want application/x-ndjson", got)
	}
	if loader.options.Repo != "hoge/fuga" {
		t.Fatalf("loader saw options %+v, want the repo passed through", loader.options)
	}

	lines := strings.Split(strings.TrimSpace(recorder.Body.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want two progress lines and a result:\n%s", len(lines), recorder.Body.String())
	}
	for _, line := range lines[:2] {
		var message map[string]any
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			t.Fatal(err)
		}
		if message["type"] != "progress" {
			t.Fatalf("line = %s, want a progress message", line)
		}
	}
	var final struct {
		Type   string       `json:"type"`
		Result graph.Result `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &final); err != nil {
		t.Fatal(err)
	}
	if final.Type != "result" || len(final.Result.Nodes) != 2 {
		t.Fatalf("final line = %s, want a result with the repository and issue nodes", lines[2])
	}
}

func TestGraphEndpointReportsLoaderFailures(t *testing.T) {
	server := New(&fakeLoader{err: errors.New("gh exploded")}, "test")
	recorder := httptest.NewRecorder()
	server.handleGraph(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/graph", nil))

	var message map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(recorder.Body.String())), &message); err != nil {
		t.Fatal(err)
	}
	if message["type"] != "error" || !strings.Contains(message["error"], "gh exploded") {
		t.Fatalf("body = %s, want the loader error surfaced", recorder.Body.String())
	}
}

func TestStaticAssetsAreEmbeddedAndLocked_down(t *testing.T) {
	server := New(&fakeLoader{}, "test")
	handler := server.handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "issue-graph") {
		t.Fatalf("serving / gave %d: %s", recorder.Code, recorder.Body.String())
	}

	policy := recorder.Header().Get("Content-Security-Policy")
	for _, directive := range []string{"script-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(policy, directive) {
			t.Fatalf("policy = %q, want it to contain %q", policy, directive)
		}
	}
}

func TestMetaReportsTheVersion(t *testing.T) {
	server := New(&fakeLoader{}, "1.2.3")
	recorder := httptest.NewRecorder()
	server.handleMeta(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/meta", nil))

	var meta map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if meta["version"] != "1.2.3" {
		t.Fatalf("meta = %v", meta)
	}
}

func TestStartListensOnLoopbackOnly(t *testing.T) {
	server := New(&fakeLoader{}, "test")
	addr, err := server.Start(0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Shutdown(context.Background()) }()

	if !strings.HasPrefix(addr, "http://127.0.0.1:") {
		t.Fatalf("addr = %q, want a loopback address", addr)
	}
	if err := WaitReady(context.Background(), addr); err != nil {
		t.Fatal(err)
	}
}
