// Command gh-issue-graph visualizes GitHub issues as a graph, with pull
// requests hanging off the issues they implement.
//
// It is the issue-side counterpart to orangain/gh-pr-graph (MIT), which this
// project follows in structure and spirit.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"

	"github.com/vanilla-bar/gh-issue-graph/internal/demo"
	"github.com/vanilla-bar/gh-issue-graph/internal/github"
	"github.com/vanilla-bar/gh-issue-graph/internal/server"
)

var version = "dev"

// watchForOrphan closes the returned channel once this process has been
// reparented, which on Unix means the process that launched it is gone.
func watchForOrphan(ctx context.Context) <-chan struct{} {
	orphaned := make(chan struct{})
	parent := os.Getppid()
	go func() {
		defer close(orphaned)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if os.Getppid() != parent {
					return
				}
			}
		}
	}()
	return orphaned
}

func main() {
	var (
		port        int
		noOpen      bool
		hostname    string
		repo        string
		refsPattern string
	)
	flag.IntVar(&port, "port", server.DefaultPort, "local server port (0 selects a free port)")
	flag.BoolVar(&noOpen, "no-open", false, "do not open the browser")
	flag.StringVar(&hostname, "hostname", "", "GitHub hostname (defaults to gh configuration)")
	flag.StringVar(&repo, "repo", "", "limit the graph to OWNER/REPO (defaults to every repository you are involved in)")
	flag.StringVar(&refsPattern, "refs-pattern", "", "regexp matching non-closing issue references in a PR body; submatch 1 holds the numbers")
	flag.Parse()

	portExplicit := false
	flag.Visit(func(current *flag.Flag) {
		if current.Name == "port" {
			portExplicit = true
		}
	})
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "gh issue-graph: unexpected arguments:", flag.Args())
		os.Exit(2)
	}

	var loader server.Loader
	if os.Getenv("GH_ISSUE_GRAPH_DEMO") != "" {
		loader = demo.New()
	} else {
		client := github.New(hostname)
		if refsPattern != "" {
			pattern, err := regexp.Compile(refsPattern)
			if err != nil {
				fmt.Fprintln(os.Stderr, "gh issue-graph: invalid -refs-pattern:", err)
				os.Exit(2)
			}
			client.RefsPattern = pattern
		}
		if repo != "" {
			// A single repository never needs the cross-repository fan-out.
			client.MaxRepos = 1
		}
		loader = client
	}

	srv := server.New(loader, version)
	var (
		addr string
		err  error
	)
	if portExplicit {
		addr, err = srv.Start(port)
	} else {
		addr, err = srv.StartPreferred(port)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gh issue-graph:", err)
		os.Exit(1)
	}

	if repo != "" {
		addr += "/?repo=" + repo
	}
	fmt.Printf("gh issue-graph %s\n", version)
	fmt.Println("https://github.com/vanilla-bar/gh-issue-graph")
	fmt.Println(addr)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Ctrl-C is supposed to reach the whole foreground process group, but not
	// every terminal does that: some send the signal to `gh` alone, which dies
	// and leaves this process orphaned, still holding the port. Watching for the
	// parent to disappear makes the extension exit with whatever launched it.
	orphaned := watchForOrphan(ctx)

	if !noOpen {
		if err := server.OpenBrowser(addr); err != nil {
			fmt.Fprintln(os.Stderr, "gh issue-graph: could not open the browser:", err)
		}
	}

	select {
	case <-ctx.Done():
	case <-orphaned:
		fmt.Fprintln(os.Stderr, "gh issue-graph: parent process exited")
	}

	// Hand the signal back to the runtime, so a second Ctrl-C kills the process
	// outright however wedged anything below gets.
	stop()

	// Close rather than Shutdown: there is nothing worth draining. A refresh in
	// flight is several seconds of `gh api graphql` calls feeding a browser tab
	// that is about to be abandoned, and Ctrl-C means give me my terminal back
	// now. Cutting the connections cancels those request contexts, which kills
	// the `gh` subprocesses with them.
	fmt.Fprintln(os.Stderr, "\ngh issue-graph: stopped")
	_ = srv.Close()
}
