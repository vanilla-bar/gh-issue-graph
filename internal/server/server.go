// Package server serves the graph UI on a loopback-only HTTP listener.
//
// The structure follows orangain/gh-pr-graph (MIT): embedded static assets, an
// NDJSON progress stream, and a mutex that serializes refreshes so polling and
// manual reloads cannot multiply the API cost.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/vanilla-bar/gh-issue-graph/internal/github"
	"github.com/vanilla-bar/gh-issue-graph/internal/graph"
)

// DefaultPort is one above gh-pr-graph's 8787 so both tools can run together.
const DefaultPort = 8788

//go:embed web/*
var assets embed.FS

// Loader collects the graph input. The demo loader implements it too.
type Loader interface {
	Load(ctx context.Context, options graph.SearchOptions, progress github.Progress) (graph.Input, error)
}

// Detailer is a loader that can also fetch one node's body. It is separate from
// Loader, and optional: a loader that cannot do it simply says so, and the
// drawer stays shut rather than the whole board failing to build.
type Detailer interface {
	Detail(ctx context.Context, id string) (graph.Detail, error)
}

// Server owns the listener and the loader.
type Server struct {
	Loader  Loader
	Version string

	mu       sync.Mutex
	listener net.Listener
	http     *http.Server
}

// New returns a server backed by loader.
func New(loader Loader, version string) *Server {
	return &Server{Loader: loader, Version: version}
}

// Start listens on the given port. Port 0 selects a free port.
func (s *Server) Start(port int) (string, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return "", err
	}
	s.listener = listener
	s.http = &http.Server{Handler: s.handler()}
	go func() {
		if err := s.http.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server: %v", err)
		}
	}()
	return fmt.Sprintf("http://%s", listener.Addr().String()), nil
}

// StartPreferred tries the preferred port and falls back to a free one.
func (s *Server) StartPreferred(port int) (string, error) {
	addr, err := s.Start(port)
	if err == nil {
		return addr, nil
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		return "", err
	}
	return s.Start(0)
}

// Shutdown stops the server, letting live requests finish.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.http == nil {
		return nil
	}
	return s.http.Shutdown(ctx)
}

// Close drops the listener and every live connection at once. Cutting a
// connection cancels its request context, which in turn kills the `gh`
// subprocess that request was waiting on.
func (s *Server) Close() error {
	if s.http == nil {
		return nil
	}
	return s.http.Close()
}

func (s *Server) handler() http.Handler {
	web, err := fs.Sub(assets, "web")
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/graph", s.handleGraph)
	mux.HandleFunc("/api/v1/meta", s.handleMeta)
	mux.HandleFunc("/api/v1/detail", s.handleDetail)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.Handle("/", http.FileServer(http.FS(web)))
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// img-src allows https: so GitHub avatars render; everything else is
		// same-origin only and there are no external scripts or styles at all.
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' https: data:; style-src 'self'; script-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// handleDetail answers with one node's body, fetched on demand. The board is
// built without any bodies at all — a hundred of them would be a hundred times
// the payload for text nobody has asked to read yet.
func (s *Server) handleDetail(w http.ResponseWriter, r *http.Request) {
	detailer, ok := s.Loader.(Detailer)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "this loader cannot fetch bodies"})
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id is required"})
		return
	}
	// Not behind s.mu: this is a single node, and holding the refresh lock for
	// it would let opening a card stall behind a full reload of the board.
	detail, err := detailer.Detail(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": s.Version})
}

// ParseOptions reads the search options out of a query string.
func ParseOptions(query url.Values) graph.SearchOptions {
	options := graph.SearchOptions{
		Repo:            strings.TrimSpace(query.Get("repo")),
		Query:           strings.TrimSpace(query.Get("q")),
		Assigned:        true,
		Authored:        true,
		Mentioned:       true,
		ReviewRequested: true,
		IncludeClosed:   query.Get("closed") == "1",
		IncludeXref:     query.Get("xref") == "1",
	}
	if query.Has("assigned") || query.Has("authored") || query.Has("mentioned") {
		options.Assigned = query.Get("assigned") == "1"
		options.Authored = query.Get("authored") == "1"
		options.Mentioned = query.Get("mentioned") == "1"
	}
	// Its own condition: the scope is newer than the other three, so a bookmarked
	// URL from before it existed should still get it rather than silently losing
	// the pull requests waiting on the reader.
	if query.Has("review") {
		options.ReviewRequested = query.Get("review") == "1"
	}
	return options
}

type progressLine struct {
	Type      string `json:"type"`
	Phase     string `json:"phase,omitempty"`
	Current   int    `json:"current,omitempty"`
	Total     int    `json:"total,omitempty"`
	Percent   int    `json:"percent"`
	Collected int    `json:"collected,omitempty"`
}

func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	// Serialize refreshes so polling and manual refresh cannot multiply API cost.
	s.mu.Lock()
	defer s.mu.Unlock()

	options := ParseOptions(r.URL.Query())

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	flusher, _ := w.(http.Flusher)

	emit := func(value any) {
		if err := encoder.Encode(value); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}

	progress := func(phase string, current, total, collected int) {
		emit(progressLine{Type: "progress", Phase: phase, Current: current, Total: total,
			Percent: percentFor(phase, current, total), Collected: collected})
	}

	in, err := s.Loader.Load(r.Context(), options, progress)
	if err != nil {
		emit(map[string]string{"type": "error", "error": err.Error()})
		return
	}
	result := graph.Build(in)
	emit(map[string]any{"type": "result", "result": result})
}

// percentFor maps a phase's own ratio onto its slice of the overall bar.
func percentFor(phase string, current, total int) int {
	ratio := 0.0
	if total > 0 {
		ratio = float64(current) / float64(total)
		if ratio > 1 {
			ratio = 1
		}
	}
	switch phase {
	case github.PhaseSearch:
		return int(ratio * 25)
	case github.PhaseExpand:
		return 25 + int(ratio*35)
	case github.PhasePulls:
		return 60 + int(ratio*30)
	case github.PhaseCrossRef:
		return 90 + int(ratio*10)
	default:
		return int(ratio * 100)
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// OpenBrowser opens rawURL in the platform's default browser.
func OpenBrowser(rawURL string) error {
	if _, err := url.ParseRequestURI(rawURL); err != nil {
		return err
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", rawURL)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	default:
		cmd = exec.Command("xdg-open", rawURL)
	}
	return cmd.Start()
}

// WaitReady polls the health endpoint until the server answers.
func WaitReady(ctx context.Context, addr string) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, addr+"/healthz", nil)
		if err != nil {
			return err
		}
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_ = response.Body.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return errors.New("server did not become ready")
}
