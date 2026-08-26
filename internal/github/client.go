// Package github collects issues and pull requests through the `gh` CLI.
//
// Like orangain/gh-pr-graph (MIT), this package shells out to `gh api graphql`
// instead of speaking HTTP itself. That keeps the module free of third-party
// dependencies and, more importantly, means the OAuth token stays inside `gh`
// and never passes through this process or the local web server.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vanilla-bar/gh-issue-graph/internal/graph"
)

// Client loads a graph from GitHub.
type Client struct {
	Hostname string
	// MaxIssues caps how many issues end up in the graph.
	MaxIssues int
	// MaxDepth caps how many rounds of hierarchy expansion run.
	MaxDepth int
	// MaxRepos caps how many repositories are scanned for pull requests.
	MaxRepos int
	// RefsPattern matches "related but not closing" references in a pull
	// request body. Submatch 1 must hold the "#12, #34" part.
	RefsPattern *regexp.Regexp
}

// DefaultRefsPattern matches a leading `refs #123` / `ref #123, #124` line.
// This convention is what makes the difference between "merging closes the
// issue" and "related, deliberately left open" visible in the graph.
var DefaultRefsPattern = regexp.MustCompile(`(?im)^[ \t>*-]*refs?[:：]?[ \t]+((?:#\d+[ \t,、]*)+)`)

var refNumberPattern = regexp.MustCompile(`#(\d+)`)

// mentionPattern finds a bare issue number anywhere in a title or the opening
// of a body — "feat(parse): #603 OOXML ..." is how a lot of pull requests name
// their issue, with no keyword at all.
var mentionPattern = regexp.MustCompile(`#(\d+)`)

// New returns a client with the default limits.
func New(hostname string) *Client {
	return &Client{
		Hostname:    hostname,
		MaxIssues:   500,
		MaxDepth:    20,
		MaxRepos:    20,
		RefsPattern: DefaultRefsPattern,
	}
}

const (
	maxConcurrentRequests = 6
	// nodes(ids:) accepts at most 100 IDs per call.
	maxIDsPerQuery = 100
	// Each repository in a batch can return a page of pull requests with
	// several nested connections. Keep headroom below GitHub's node limit.
	maxReposPerQuery = 5
	pullRequestPage  = 60
)

// Progress reports loading progress to the caller.
type Progress func(phase string, current, total, collected int)

// Phases, in the order they run.
const (
	PhaseSearch   = "Searching issues"
	PhaseExpand   = "Expanding issue hierarchy"
	PhasePulls    = "Linking pull requests"
	PhaseCrossRef = "Reading cross references"
)

// Load collects everything needed to build the graph.
func (c *Client) Load(ctx context.Context, options graph.SearchOptions, progress Progress) (graph.Input, error) {
	if progress == nil {
		progress = func(string, int, int, int) {}
	}
	in := graph.Input{Repositories: map[string]*graph.Repository{}}

	// Pull requests waiting on your review are your work, but they are somebody
	// else's pull request: no issue search returns them. It is an independent
	// query, so it runs beside the issue searches rather than after them:
	// in sequence its latency lands on top of the load, while alongside it
	// hides inside the slowest issue search.
	// Its own cancellable context: when an issue search fails there is nothing
	// left to build, and the caller should not be held for however long the
	// review query still has to run.
	reviewCtx, cancelReview := context.WithCancel(ctx)
	defer cancelReview()
	var (
		reviewPRs      []rawPullRequest
		reviewViewer   string
		reviewWarnings []string
		reviewErr      error
		reviewDone     sync.WaitGroup
	)
	reviewDone.Add(1)
	go func() {
		defer reviewDone.Done()
		reviewPRs, reviewViewer, reviewWarnings, reviewErr = c.searchReviewRequested(reviewCtx, options)
	}()

	viewer, issues, searchReasons, warnings, err := c.search(ctx, options, progress)
	if err != nil {
		cancelReview()
	}
	// Always waited on before returning: the goroutine writes the variables
	// above, so leaving while it runs would be a data race.
	reviewDone.Wait()
	if err != nil {
		return in, err
	}
	if reviewErr != nil {
		return in, reviewErr
	}
	in.Viewer = viewer
	in.Warnings = append(in.Warnings, warnings...)
	in.Warnings = append(in.Warnings, reviewWarnings...)
	if in.Viewer == "" {
		in.Viewer = reviewViewer
		viewer = reviewViewer
	}

	byID := map[string]*graph.Issue{}
	order := []*graph.Issue{}
	closesLinks := map[string][]string{} // pull request node ID -> issue node IDs
	repos := map[string]*graph.Repository{}

	// Why each complement was pulled in, keyed by issue node ID.
	complementReasons := map[string]string{}

	absorb := func(raw []rawIssue, source string) {
		for i := range raw {
			issue, closes := raw[i].convert(source)
			if source == "search" {
				issue.Reasons = searchReasons[issue.ID]
			} else if reason := complementReasons[issue.ID]; reason != "" {
				issue.Reasons = []string{reason}
			}
			if _, ok := byID[issue.ID]; ok {
				continue
			}
			if len(byID) >= c.MaxIssues {
				return
			}
			byID[issue.ID] = issue
			order = append(order, issue)
			if repos[issue.RepositoryID] == nil {
				repos[issue.RepositoryID] = &graph.Repository{
					ID:            issue.RepositoryID,
					NameWithOwner: issue.Repository,
					URL:           issue.RepositoryURL,
					DefaultBranch: issue.DefaultBranch,
				}
			}
			for _, prID := range closes {
				closesLinks[prID] = append(closesLinks[prID], issue.ID)
			}
		}
	}
	absorb(issues, "search")

	// The reverse lookup. Whatever a review request points at becomes a
	// complement, and the expansion below then walks up to its parents, so the
	// pull request ends up under a real tree rather than alone in column zero.
	//
	// Anything named by node ID is *not* fetched here: it is handed to the
	// expansion loop below, which is about to run the same nodes(ids:) query
	// anyway. A `refs #N` line needs a different query shape, so it is the one
	// case that still costs a round trip — and only when there is one.
	var reverseIDs []string
	if len(reviewPRs) > 0 {
		for _, raw := range reviewPRs {
			reason := graph.ReasonReviewing(raw.Number)
			for _, ref := range raw.ClosingIssuesReferences.Nodes {
				if ref.ID != "" && complementReasons[ref.ID] == "" {
					complementReasons[ref.ID] = reason
				}
			}
		}
		ids, refs := c.referencedIssues(reviewPRs)
		reverseIDs = ids
		var referenced []rawIssue
		if len(refs) > 0 {
			fetched, err := c.fetchIssuesByNumber(ctx, refs)
			if err != nil {
				return in, err
			}
			// A `refs #N` line carries no node ID, so the reason has to be
			// matched back by repository and number.
			byKey := map[string]string{}
			for _, raw := range reviewPRs {
				for _, number := range c.parseRefs(raw.Body) {
					key := raw.Repository.NameWithOwner + "#" + strconv.Itoa(number)
					if byKey[key] == "" {
						byKey[key] = graph.ReasonReviewing(raw.Number)
					}
				}
			}
			for _, issue := range fetched {
				key := issue.Repository.NameWithOwner + "#" + strconv.Itoa(issue.Number)
				if reason := byKey[key]; reason != "" && complementReasons[issue.ID] == "" {
					complementReasons[issue.ID] = reason
				}
			}
			referenced = append(referenced, fetched...)
		}
		absorb(referenced, "complement")
	}

	// Expand the hierarchy. Without this a cross-repository search shows a row
	// of disconnected issues: the parent of an open issue is usually not itself
	// a search hit, and the children of a finished parent are all closed.
	pending, pendingReasons := c.pendingIDs(order, byID)
	for _, id := range reverseIDs {
		if _, known := byID[id]; known {
			continue
		}
		pending = appendUnique(pending, id)
		if pendingReasons[id] == "" {
			pendingReasons[id] = complementReasons[id]
		}
	}
	for depth := 0; depth < c.MaxDepth && len(pending) > 0; depth++ {
		if len(byID) >= c.MaxIssues {
			in.Warnings = append(in.Warnings, "Issue limit reached; narrow the search to see the complete graph.")
			break
		}
		progress(PhaseExpand, depth+1, depth+2, len(byID))
		fetched, err := c.fetchIssuesByID(ctx, pending)
		if err != nil {
			return in, err
		}
		before := len(byID)
		for id, reason := range pendingReasons {
			// First reason wins, matching the refs path above. Otherwise an
			// issue reached both by a review request and as somebody's parent
			// would drop "reviewing #682" for "parent of #12".
			if complementReasons[id] == "" {
				complementReasons[id] = reason
			}
		}
		absorb(fetched, "complement")
		if len(byID) == before {
			break
		}
		pending, pendingReasons = c.pendingIDs(order, byID)
	}
	progress(PhaseExpand, 1, 1, len(byID))

	// Drop closed issues that the search itself returned, unless asked to keep
	// them. Closed issues pulled in as complements stay: a parent with six
	// closed children is exactly the picture worth seeing.
	if !options.IncludeClosed {
		kept := order[:0]
		for _, issue := range order {
			if issue.State == "CLOSED" && issue.Source == "search" && !c.hasKeptRelative(issue, byID) {
				delete(byID, issue.ID)
				continue
			}
			kept = append(kept, issue)
		}
		order = kept
	}

	prs, prWarnings, err := c.loadPullRequests(ctx, options, viewer, order, byID, closesLinks, reviewPRs, progress)
	if err != nil {
		return in, err
	}
	in.Warnings = append(in.Warnings, prWarnings...)

	if options.IncludeXref {
		if err := c.addCrossReferences(ctx, order, prs, progress); err != nil {
			return in, err
		}
	}

	for _, issue := range order {
		if repos[issue.RepositoryID] != nil && repos[issue.RepositoryID].DefaultBranch == "" {
			repos[issue.RepositoryID].DefaultBranch = issue.DefaultBranch
		}
	}
	in.Issues = order
	in.PullRequests = prs
	in.Repositories = repos
	return in, nil
}

// hasKeptRelative reports whether a closed search hit is worth keeping because
// it anchors part of the hierarchy.
func (c *Client) hasKeptRelative(issue *graph.Issue, byID map[string]*graph.Issue) bool {
	if issue.ParentID != "" {
		return true
	}
	return len(issue.SubIssueIDs) > 0
}

// pendingIDs lists related issue IDs that have not been fetched yet, along with
// why each one is wanted, so a complement can say what dragged it onto the
// canvas rather than appearing unexplained.
func (c *Client) pendingIDs(order []*graph.Issue, byID map[string]*graph.Issue) ([]string, map[string]string) {
	seen := map[string]bool{}
	var pending []string
	reasons := map[string]string{}
	want := func(id, reason string) {
		if id == "" || byID[id] != nil || seen[id] {
			return
		}
		seen[id] = true
		pending = append(pending, id)
		reasons[id] = reason
	}
	for _, issue := range order {
		label := "#" + strconv.Itoa(issue.Number)
		want(issue.ParentID, "parent of "+label)
		for _, id := range issue.SubIssueIDs {
			want(id, "sub-issue of "+label)
		}
		for _, id := range issue.BlockedByIDs {
			want(id, "blocks "+label)
		}
		for _, id := range issue.BlockingIDs {
			want(id, "blocked by "+label)
		}
		want(issue.DuplicateOfID, "duplicated by "+label)
	}
	sort.Strings(pending)
	return pending, reasons
}

// search runs the built-in scopes in parallel and de-duplicates by node ID.
// Separate queries rather than one OR'd query, because GitHub's search syntax
// treats OR inconsistently across qualifiers.
func (c *Client) search(ctx context.Context, options graph.SearchOptions, progress Progress) (string, []rawIssue, map[string][]string, []string, error) {
	specs := buildSearchSpecs(options)
	if len(specs) == 0 {
		return "", nil, nil, nil, fmt.Errorf("no search scope selected")
	}

	type outcome struct {
		viewer  string
		issues  []rawIssue
		warning string
		err     error
	}
	results := make([]outcome, len(specs))
	var wg sync.WaitGroup
	var done int
	var mu sync.Mutex
	sem := make(chan struct{}, maxConcurrentRequests)

	for i, spec := range specs {
		wg.Add(1)
		go func(i int, spec searchSpec) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			var payload struct {
				Data struct {
					Viewer struct {
						Login string `json:"login"`
					} `json:"viewer"`
					Search struct {
						IssueCount int        `json:"issueCount"`
						Nodes      []rawIssue `json:"nodes"`
					} `json:"search"`
				} `json:"data"`
			}
			err := c.graphql(ctx, searchQuery, map[string]string{"q": spec.query}, &payload)
			out := outcome{err: err}
			if err == nil {
				out.viewer = payload.Data.Viewer.Login
				out.issues = payload.Data.Search.Nodes
				if payload.Data.Search.IssueCount > len(payload.Data.Search.Nodes) {
					out.warning = fmt.Sprintf("Search %q has more than %d results; showing the first %d.",
						spec.query, len(payload.Data.Search.Nodes), len(payload.Data.Search.Nodes))
				}
			}
			results[i] = out

			mu.Lock()
			done++
			progress(PhaseSearch, done, len(specs), 0)
			mu.Unlock()
		}(i, spec)
	}
	wg.Wait()

	var viewer string
	var issues []rawIssue
	var warnings []string
	reasons := map[string][]string{}
	seen := map[string]bool{}
	for i, out := range results {
		if out.err != nil {
			return "", nil, nil, nil, out.err
		}
		if viewer == "" {
			viewer = out.viewer
		}
		if out.warning != "" {
			warnings = append(warnings, out.warning)
		}
		for _, issue := range out.issues {
			if issue.ID == "" {
				continue
			}
			// An issue can match several scopes; record each one.
			reasons[issue.ID] = appendUnique(reasons[issue.ID], specs[i].reason)
			if seen[issue.ID] {
				continue
			}
			seen[issue.ID] = true
			issues = append(issues, issue)
		}
	}
	return viewer, issues, reasons, warnings, nil
}

// inParallel runs jobs at most maxConcurrentRequests at a time and returns the
// first error. The batched fetches below were straight loops: fine at one
// batch, a stack of round trips waiting on each other as soon as a graph passes
// 100 issues or 5 repositories.
//
// The limit is per call, not a pool shared with c.search and the pull request
// scan — each of those keeps its own semaphore of the same size, and the two
// searches in Load overlap, so peak concurrency across a load is a little above
// the constant rather than bounded by it.
//
// Once one job has failed the rest are skipped rather than each spawning a gh
// process that is only going to fail too.
func inParallel(ctx context.Context, count int, job func(i int) error) error {
	if count <= 0 {
		return nil
	}
	if count == 1 {
		return job(0)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	failed := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return firstErr != nil
	}
	sem := make(chan struct{}, maxConcurrentRequests)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if failed() || ctx.Err() != nil {
				return
			}
			if err := job(i); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	return firstErr
}

func appendUnique(list []string, value string) []string {
	for _, existing := range list {
		if existing == value {
			return list
		}
	}
	return append(list, value)
}

// buildSearchSpecs turns the options into one search per scope. Separate
// queries rather than one OR'd query, because GitHub's search syntax treats OR
// inconsistently across qualifiers; the results are merged by node ID.
//
// A repository filter narrows every scope rather than replacing them: ticking
// "assigned" has to mean assigned whether or not a repository is named.
func buildSearchSpecs(options graph.SearchOptions) []searchSpec {
	qualifiers := "is:issue"
	if !options.IncludeClosed {
		qualifiers += " is:open"
	}
	if repo := strings.TrimSpace(options.Repo); repo != "" {
		qualifiers = "repo:" + repo + " " + qualifiers
	}

	var specs []searchSpec
	scoped := func(scope, reason string) {
		specs = append(specs, searchSpec{query: qualifiers + " " + scope + " sort:updated-desc", reason: reason})
	}
	if options.Assigned {
		scoped("assignee:@me", graph.ReasonAssigned)
	}
	if options.Authored {
		scoped("author:@me", graph.ReasonAuthored)
	}
	if options.Mentioned {
		scoped("mentions:@me", graph.ReasonMentioned)
	}
	if query := strings.TrimSpace(options.Query); query != "" {
		specs = append(specs, searchSpec{query: qualifiers + " " + query, reason: graph.ReasonQuery})
	}

	// With a repository named and no scope ticked, the repository itself is the
	// scope. Without one there is nothing to fall back to.
	if len(specs) == 0 && strings.TrimSpace(options.Repo) != "" {
		specs = append(specs, searchSpec{query: qualifiers + " sort:updated-desc", reason: graph.ReasonRepository})
	}
	return specs
}

// searchSpec is one search and the reason a hit from it lands on the canvas.
type searchSpec struct {
	query  string
	reason string
}

// reviewRequestedSpec is the one scope that searches pull requests rather than
// issues: what is waiting on your review is your work, but it is somebody
// else's pull request, so no issue search will ever return it.
//
// `is:open` is fixed rather than following IncludeClosed: a merged or closed
// pull request is not waiting on anybody.
func reviewRequestedSpec(options graph.SearchOptions) (searchSpec, bool) {
	if !options.ReviewRequested {
		return searchSpec{}, false
	}
	query := "is:pr is:open review-requested:@me sort:updated-desc"
	if repo := strings.TrimSpace(options.Repo); repo != "" {
		query = "repo:" + repo + " " + query
	}
	return searchSpec{query: query, reason: graph.ReasonReviewRequested}, true
}

// searchReviewRequested returns the open pull requests waiting on the viewer's
// review. Empty, with no error, when the scope is off.
func (c *Client) searchReviewRequested(ctx context.Context, options graph.SearchOptions) ([]rawPullRequest, string, []string, error) {
	spec, ok := reviewRequestedSpec(options)
	if !ok {
		return nil, "", nil, nil
	}
	var payload struct {
		Data struct {
			Viewer struct {
				Login string `json:"login"`
			} `json:"viewer"`
			Search struct {
				IssueCount int              `json:"issueCount"`
				Nodes      []rawPullRequest `json:"nodes"`
			} `json:"search"`
		} `json:"data"`
	}
	if err := c.graphql(ctx, prSearchQuery, map[string]string{"q": spec.query}, &payload); err != nil {
		return nil, "", nil, err
	}
	nodes := payload.Data.Search.Nodes
	var warnings []string
	if payload.Data.Search.IssueCount > len(nodes) {
		warnings = append(warnings, fmt.Sprintf("Search %q has more than %d results; showing the first %d.",
			spec.query, len(nodes), len(nodes)))
	}
	return nodes, payload.Data.Viewer.Login, warnings, nil
}

// issueRef is an issue named by repository and number, before it has been
// resolved to a node ID.
type issueRef struct {
	repository string
	number     int
}

// referencedIssues reverse-looks-up what a pull request points at, so a review
// request can be hung under the work it belongs to instead of floating.
//
// Two of the three link layers are used. `closingIssuesReferences` hands over
// node IDs, and a `refs #N` line names a repository and a number. The third
// layer — a bare `#N` in the title — is deliberately left out: the rest of this
// file only trusts it when the issue is already on the canvas, and fetching on
// that guess would invent a parent for a pull request that never had one.
func (c *Client) referencedIssues(prs []rawPullRequest) (ids []string, refs []issueRef) {
	seenID := map[string]bool{}
	seenRef := map[issueRef]bool{}
	for _, raw := range prs {
		for _, ref := range raw.ClosingIssuesReferences.Nodes {
			if ref.ID == "" || seenID[ref.ID] {
				continue
			}
			seenID[ref.ID] = true
			ids = append(ids, ref.ID)
		}
		for _, number := range c.parseRefs(raw.Body) {
			key := issueRef{repository: raw.Repository.NameWithOwner, number: number}
			if key.repository == "" || seenRef[key] {
				continue
			}
			seenRef[key] = true
			refs = append(refs, key)
		}
	}
	return ids, refs
}

// fetchIssuesByNumber resolves repository/number pairs, which is all a
// `refs #N` line gives us. Batched with aliases the same way the per-repository
// pull request scan is, so a page of references costs one request.
func (c *Client) fetchIssuesByNumber(ctx context.Context, refs []issueRef) ([]rawIssue, error) {
	byRepo := map[string][]int{}
	var order []string
	for _, ref := range refs {
		if _, seen := byRepo[ref.repository]; !seen {
			order = append(order, ref.repository)
		}
		byRepo[ref.repository] = append(byRepo[ref.repository], ref.number)
	}

	batches := chunk(len(order), maxReposPerQuery)
	results := make([][]rawIssue, len(batches))
	err := inParallel(ctx, len(batches), func(b int) error {
		start, end := batches[b][0], batches[b][1]
		var query strings.Builder
		query.WriteString("query{")
		fields := 0
		for r, repository := range order[start:end] {
			owner, name, ok := graph.RepositoryKey(repository)
			if !ok {
				continue
			}
			fmt.Fprintf(&query, "r%d:repository(owner:%s,name:%s){", r, strconv.Quote(owner), strconv.Quote(name))
			for i, number := range byRepo[repository] {
				fmt.Fprintf(&query, "i%d:issue(number:%d){%s}", i, number, issueFields)
				fields++
			}
			query.WriteByte('}')
		}
		query.WriteByte('}')
		if fields == 0 {
			return nil
		}

		var payload struct {
			Data map[string]map[string]*rawIssue `json:"data"`
		}
		// Nulls are the expected answer here, and GitHub reports them as
		// errors alongside the data; see graphqlAllowingMissing.
		if err := c.graphqlAllowingMissing(ctx, query.String(), &payload); err != nil {
			return err
		}
		for _, repository := range payload.Data {
			for _, issue := range repository {
				// A number that names a pull request, or an issue you cannot
				// see, comes back as null.
				if issue == nil || issue.ID == "" {
					continue
				}
				results[b] = append(results[b], *issue)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var out []rawIssue
	for _, batch := range results {
		out = append(out, batch...)
	}
	return out, nil
}

// fetchIssuesByID resolves related issues through nodes(ids:), which costs a
// single point per call regardless of how many IDs it carries.
func (c *Client) fetchIssuesByID(ctx context.Context, ids []string) ([]rawIssue, error) {
	batches := chunk(len(ids), maxIDsPerQuery)
	results := make([][]rawIssue, len(batches))
	err := inParallel(ctx, len(batches), func(i int) error {
		span := batches[i]
		query := "query{nodes(ids:[" + quoteJoin(ids[span[0]:span[1]]) + "]){...on Issue{" + issueFields + "}}}"

		var payload struct {
			Data struct {
				Nodes []rawIssue `json:"nodes"`
			} `json:"data"`
		}
		if err := c.graphql(ctx, query, nil, &payload); err != nil {
			return err
		}
		for _, issue := range payload.Data.Nodes {
			if issue.ID != "" {
				results[i] = append(results[i], issue)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var out []rawIssue
	for _, batch := range results {
		out = append(out, batch...)
	}
	return out, nil
}

// chunk splits a length into [start, end) spans of at most size. Batches are
// collected into a slice so the parallel fetches keep a stable order.
func chunk(length, size int) [][2]int {
	var spans [][2]int
	for start := 0; start < length; start += size {
		spans = append(spans, [2]int{start, min(start+size, length)})
	}
	return spans
}

// fetchPullRequestsByID resolves pull requests the issue side pointed at.
func (c *Client) fetchPullRequestsByID(ctx context.Context, ids []string) ([]rawPullRequest, error) {
	batches := chunk(len(ids), maxIDsPerQuery)
	results := make([][]rawPullRequest, len(batches))
	err := inParallel(ctx, len(batches), func(i int) error {
		span := batches[i]
		query := "query{nodes(ids:[" + quoteJoin(ids[span[0]:span[1]]) + "]){...on PullRequest{" + prFields + "}}}"

		var payload struct {
			Data struct {
				Nodes []rawPullRequest `json:"nodes"`
			} `json:"data"`
		}
		if err := c.graphql(ctx, query, nil, &payload); err != nil {
			return err
		}
		results[i] = payload.Data.Nodes
		return nil
	})
	if err != nil {
		return nil, err
	}
	var out []rawPullRequest
	for _, batch := range results {
		out = append(out, batch...)
	}
	return out, nil
}

// quoteJoin renders GraphQL node IDs as a quoted, comma separated list. The gh
// CLI can only pass string variables, so ID lists are inlined into the query.
func quoteJoin(values []string) string {
	var b strings.Builder
	for i, value := range values {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Quote(value))
	}
	return b.String()
}

// loadPullRequests fetches pull requests for every repository in the graph and
// links them to issues.
//
// closingIssuesReferences alone misses a large share of the links, because a
// pull request that deliberately leaves its issue open writes `refs #123`
// instead of `Closes #123`. That is why the body is fetched and parsed here
// rather than relying on the issue side only.
// seeded carries the pull requests already found by the review-requested
// search. They join the per-repository scan's results before the link layers
// run, so they get the same treatment as anything else and are not lost when
// their repository holds no issue of yours, or when they have fallen off the
// back of the recency page.
func (c *Client) loadPullRequests(ctx context.Context, options graph.SearchOptions, viewer string, issues []*graph.Issue, byID map[string]*graph.Issue, closesLinks map[string][]string, seeded []rawPullRequest, progress Progress) ([]*graph.PullRequest, []string, error) {
	type repoRef struct {
		id    string
		owner string
		name  string
	}
	seenRepo := map[string]bool{}
	var targets []repoRef
	for _, issue := range issues {
		if seenRepo[issue.Repository] {
			continue
		}
		owner, name, ok := graph.RepositoryKey(issue.Repository)
		if !ok {
			continue
		}
		seenRepo[issue.Repository] = true
		targets = append(targets, repoRef{id: issue.RepositoryID, owner: owner, name: name})
	}

	reviewRequested := map[string]bool{}
	for _, raw := range seeded {
		if raw.ID != "" {
			reviewRequested[raw.ID] = true
		}
	}

	var warnings []string
	if len(targets) > c.MaxRepos {
		warnings = append(warnings, fmt.Sprintf("Scanning pull requests in the first %d of %d repositories.", c.MaxRepos, len(targets)))
		targets = targets[:c.MaxRepos]
	}
	if len(targets) == 0 && len(seeded) == 0 {
		return nil, warnings, nil
	}

	issueByNumber := map[string]*graph.Issue{}
	for _, issue := range issues {
		issueByNumber[issue.Repository+"#"+strconv.Itoa(issue.Number)] = issue
	}

	var mu sync.Mutex
	var collected []rawPullRequest
	var firstErr error
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrentRequests)
	batches := (len(targets) + maxReposPerQuery - 1) / maxReposPerQuery
	done := 0

	for start := 0; start < len(targets); start += maxReposPerQuery {
		end := min(start+maxReposPerQuery, len(targets))
		batch := targets[start:end]
		wg.Add(1)
		go func(batch []repoRef) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			var query strings.Builder
			query.WriteString("query{")
			for i, target := range batch {
				fmt.Fprintf(&query, "r%d:repository(owner:%s,name:%s){id nameWithOwner url defaultBranchRef{name} pullRequests(first:%d,states:[OPEN,MERGED],orderBy:{field:UPDATED_AT,direction:DESC}){nodes{%s}}}",
					i, strconv.Quote(target.owner), strconv.Quote(target.name), pullRequestPage, prFields)
			}
			query.WriteByte('}')

			var payload struct {
				Data map[string]struct {
					PullRequests struct {
						Nodes []rawPullRequest `json:"nodes"`
					} `json:"pullRequests"`
				} `json:"data"`
			}
			err := c.graphql(ctx, query.String(), nil, &payload)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			for _, repo := range payload.Data {
				collected = append(collected, repo.PullRequests.Nodes...)
			}
			done++
			progress(PhasePulls, done, batches, len(issues))
		}(batch)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, warnings, firstErr
	}

	collected = append(collected, seeded...)

	var prs []*graph.PullRequest
	seenPR := map[string]bool{}
	for i := range collected {
		raw := collected[i]
		if raw.ID == "" || seenPR[raw.ID] {
			continue
		}
		pr := raw.convert()

		// Layer 1: the API's own closing references.
		for _, ref := range raw.ClosingIssuesReferences.Nodes {
			if byID[ref.ID] != nil {
				pr.Links = append(pr.Links, graph.IssueLink{IssueID: ref.ID, Repository: ref.Repository.NameWithOwner, Number: ref.Number, Kind: graph.LinkCloses})
			}
		}
		// Layer 2: `refs #N` in the body, meaning related without closing.
		for _, number := range c.parseRefs(raw.Body) {
			key := pr.Repository + "#" + strconv.Itoa(number)
			target := issueByNumber[key]
			if target == nil {
				continue
			}
			pr.Links = append(pr.Links, graph.IssueLink{IssueID: target.ID, Repository: target.Repository, Number: number, Kind: graph.LinkRefs})
		}
		// Layer 3: a bare number in the title. Weaker than the two above and
		// only trusted when the issue is already on the canvas, which keeps a
		// stray "#5" in a title from inventing a link.
		for _, number := range mentionedNumbers(raw.Title) {
			key := pr.Repository + "#" + strconv.Itoa(number)
			target := issueByNumber[key]
			if target == nil {
				continue
			}
			pr.Links = append(pr.Links, graph.IssueLink{IssueID: target.ID, Repository: target.Repository, Number: number, Kind: graph.LinkMentions})
		}

		pr.ReviewRequested = reviewRequested[raw.ID]
		seenPR[raw.ID] = true
		prs = append(prs, pr)
	}

	// The per-repository page above is ordered by recency, so a pull request
	// that closed an old issue can fall outside it. The issue side already told
	// us those node IDs, so fetch whatever is still missing by ID.
	var missing []string
	for prID := range closesLinks {
		if !seenPR[prID] {
			missing = append(missing, prID)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		extra, err := c.fetchPullRequestsByID(ctx, missing)
		if err != nil {
			return nil, warnings, err
		}
		for i := range extra {
			raw := extra[i]
			if raw.ID == "" || seenPR[raw.ID] {
				continue
			}
			pr := raw.convert()
			for _, issueID := range closesLinks[raw.ID] {
				target := byID[issueID]
				if target == nil {
					continue
				}
				pr.Links = append(pr.Links, graph.IssueLink{IssueID: target.ID, Repository: target.Repository, Number: target.Number, Kind: graph.LinkCloses})
			}
			if len(pr.Links) == 0 {
				continue
			}
			seenPR[raw.ID] = true
			prs = append(prs, pr)
		}
	}

	// A pull request that implements nothing in the graph earns its place only
	// if it is your own open work. Showing every open pull request in the
	// repository buries your issues under other people's branches.
	kept := prs[:0]
	for _, pr := range prs {
		if len(pr.Links) > 0 {
			// The reason line is filled in by graph.Build, once the links have
			// been narrowed to the strongest one per issue.
			kept = append(kept, pr)
			continue
		}
		if pr.State == "OPEN" && pr.Author.Login == viewer {
			pr.Reasons = []string{graph.ReasonYours}
			kept = append(kept, pr)
			continue
		}
		// Waiting on your review and referencing nothing we could resolve: it
		// is still your work, so it stays, in the issues' own column.
		if pr.ReviewRequested {
			pr.Reasons = []string{graph.ReasonReviewRequested}
			kept = append(kept, pr)
		}
	}

	if err := c.fillReviewAndCI(ctx, viewer, kept); err != nil {
		return nil, warnings, err
	}
	return kept, warnings, nil
}

// The drawer's query. Written out whole rather than assembled from pieces, so
// a test can read exactly what GitHub is asked for — including what it is not
// asked for. Line comments would multiply the node count by ten for a level of
// detail the panel does not show.
const detailQuery = `query($id:ID!){node(id:$id){
  __typename
  ...on Issue{
    bodyHTML
    comments(last:20){totalCount nodes{author{login avatarUrl} bodyHTML createdAt url}}
  }
  ...on PullRequest{
    bodyHTML
    comments(last:20){totalCount nodes{author{login avatarUrl} bodyHTML createdAt url}}
    reviews(last:20){nodes{author{login avatarUrl} state bodyHTML createdAt url}}
  }
}}`

// commentTail is how far back the drawer reads, and matches the `last:` above.
// The recent exchange is what says whether something is stuck; the opening of a
// long thread is usually the body, which is fetched in full anyway.
const commentTail = 20

// Detail fetches one node's rendered body and the tail of its conversation.
// GitHub does the Markdown, which is why this module needs no renderer of its
// own and the page needs no library.
//
// One node, one call, and only when somebody opens a card. Asking for bodies
// during the scan would carry the text of every pull request in every
// repository across the wire for the sake of the one that gets read.
func (c *Client) Detail(ctx context.Context, id string) (graph.Detail, error) {
	type rawComment struct {
		Author *struct {
			Login     string `json:"login"`
			AvatarURL string `json:"avatarUrl"`
		} `json:"author"`
		BodyHTML  string    `json:"bodyHTML"`
		CreatedAt time.Time `json:"createdAt"`
		URL       string    `json:"url"`
		State     string    `json:"state"`
	}
	var payload struct {
		Data struct {
			Node *struct {
				Typename string `json:"__typename"`
				BodyHTML string `json:"bodyHTML"`
				Comments struct {
					TotalCount int          `json:"totalCount"`
					Nodes      []rawComment `json:"nodes"`
				} `json:"comments"`
				Reviews struct {
					Nodes []rawComment `json:"nodes"`
				} `json:"reviews"`
			} `json:"node"`
		} `json:"data"`
	}
	if err := c.graphql(ctx, detailQuery, map[string]string{"id": id}, &payload); err != nil {
		return graph.Detail{}, err
	}
	node := payload.Data.Node
	if node == nil {
		return graph.Detail{}, fmt.Errorf("no issue or pull request with id %q", id)
	}

	said := make([]graph.Comment, 0, len(node.Comments.Nodes)+len(node.Reviews.Nodes))
	total := node.Comments.TotalCount
	keep := func(raw rawComment, state string) {
		// A review with no words is an approval and nothing else. It is already
		// counted on the card; repeating it here would bury the ones that speak.
		if strings.TrimSpace(raw.BodyHTML) == "" {
			return
		}
		if state != "" {
			total++
		}
		who := graph.User{}
		if raw.Author != nil {
			who = graph.User{Login: raw.Author.Login, AvatarURL: raw.Author.AvatarURL}
		}
		said = append(said, graph.Comment{
			Author: who, BodyHTML: raw.BodyHTML, CreatedAt: raw.CreatedAt,
			URL: raw.URL, ReviewState: state,
		})
	}
	for _, raw := range node.Comments.Nodes {
		keep(raw, "")
	}
	for _, raw := range node.Reviews.Nodes {
		keep(raw, raw.State)
	}

	return graph.Detail{
		ID: id, BodyHTML: node.BodyHTML,
		Comments:     tailOfConversation(said),
		CommentTotal: total,
	}, nil
}

// tailOfConversation sorts by when things were said and keeps the last page of
// them. The two connections arrive separately and interleave in time, so they
// have to be merged before anything can be cut.
func tailOfConversation(said []graph.Comment) []graph.Comment {
	sort.SliceStable(said, func(i, j int) bool { return said[i].CreatedAt.Before(said[j].CreatedAt) })
	if len(said) > commentTail {
		said = said[len(said)-commentTail:]
	}
	if len(said) == 0 {
		return nil
	}
	return said
}

// reviewVoice is one row of either review connection, flattened so the two can
// be merged without caring which one it came from.
type reviewVoice struct {
	login  string
	avatar string
	state  string
	team   bool
}

// applyReviewState merges what people have said with who is still being waited
// on, and fills in the counts the card shows.
//
// The two connections overlap on purpose. Somebody who approved and was then
// asked again appears in both, and that pair is what a re-review *is*: GitHub
// has no field for it. Counting people rather than reviews keeps a reviewer who
// approved twice from making the total say 2 of 3.
func applyReviewState(pr *graph.PullRequest, answered, requested []reviewVoice) {
	order := []string{}
	byLogin := map[string]*graph.Reviewer{}
	add := func(login string) *graph.Reviewer {
		if existing := byLogin[login]; existing != nil {
			return existing
		}
		byLogin[login] = &graph.Reviewer{Login: login}
		order = append(order, login)
		return byLogin[login]
	}

	for _, voice := range answered {
		who := add(voice.login)
		who.State = voice.state
		if voice.avatar != "" {
			who.AvatarURL = voice.avatar
		}
	}
	for _, voice := range requested {
		who := add(voice.login)
		who.Requested = true
		who.IsTeam = voice.team
		if voice.avatar != "" {
			who.AvatarURL = voice.avatar
		}
		if who.State != "" {
			pr.ReReviewRequested = true
		}
	}

	if len(order) == 0 {
		pr.Reviewers, pr.ReviewApproved, pr.ReviewTotal = nil, 0, 0
		return
	}

	// Those still owed a review lead: the card is read to answer "what is this
	// waiting on?", and a row of green ticks ahead of them buries the answer.
	sort.SliceStable(order, func(i, j int) bool {
		left, right := byLogin[order[i]], byLogin[order[j]]
		if left.Requested != right.Requested {
			return left.Requested
		}
		return false
	})

	reviewers := make([]graph.Reviewer, 0, len(order))
	approved := 0
	for _, login := range order {
		who := byLogin[login]
		if who.State == graph.ReviewApproved {
			approved++
		}
		reviewers = append(reviewers, *who)
	}
	pr.Reviewers = reviewers
	pr.ReviewApproved = approved
	pr.ReviewTotal = len(reviewers)
}

// fillReviewAndCI fetches the check rollup and the review state for the pull
// requests that made it onto the canvas. Splitting it out of the scan trades
// one extra round trip for a query that no longer asks GitHub to aggregate
// checks, or list reviewers, for every open and merged pull request in every
// repository.
func (c *Client) fillReviewAndCI(ctx context.Context, viewer string, prs []*graph.PullRequest) error {
	byID := make(map[string]*graph.PullRequest, len(prs))
	ids := make([]string, 0, len(prs))
	for _, pr := range prs {
		if pr.ID == "" || byID[pr.ID] != nil {
			continue
		}
		byID[pr.ID] = pr
		ids = append(ids, pr.ID)
	}
	batches := chunk(len(ids), maxIDsPerQuery)
	return inParallel(ctx, len(batches), func(i int) error {
		span := batches[i]
		query := "query{nodes(ids:[" + quoteJoin(ids[span[0]:span[1]]) + "]){...on PullRequest{" + ciFields + "}}}"

		var payload struct {
			Data struct {
				Nodes []struct {
					ID      string `json:"id"`
					Commits struct {
						Nodes []struct {
							Commit struct {
								StatusCheckRollup *struct {
									State string `json:"state"`
								} `json:"statusCheckRollup"`
							} `json:"commit"`
						} `json:"nodes"`
					} `json:"commits"`
					ReviewRequests struct {
						Nodes []struct {
							RequestedReviewer *struct {
								Typename  string `json:"__typename"`
								Login     string `json:"login"`
								AvatarURL string `json:"avatarUrl"`
								Slug      string `json:"slug"`
							} `json:"requestedReviewer"`
						} `json:"nodes"`
					} `json:"reviewRequests"`
					LatestReviews struct {
						Nodes []struct {
							State  string `json:"state"`
							Author *struct {
								Login     string `json:"login"`
								AvatarURL string `json:"avatarUrl"`
							} `json:"author"`
						} `json:"nodes"`
					} `json:"latestReviews"`
					PendingReviews struct {
						Nodes []struct {
							Author *struct {
								Login string `json:"login"`
							} `json:"author"`
						} `json:"nodes"`
					} `json:"pendingReviews"`
				} `json:"nodes"`
			} `json:"data"`
		}
		if err := c.graphql(ctx, query, nil, &payload); err != nil {
			return err
		}
		for _, node := range payload.Data.Nodes {
			pr := byID[node.ID]
			if pr == nil {
				continue
			}
			if len(node.Commits.Nodes) > 0 {
				if rollup := node.Commits.Nodes[0].Commit.StatusCheckRollup; rollup != nil {
					pr.CIState = rollup.State
				}
			}
			answered := make([]reviewVoice, 0, len(node.LatestReviews.Nodes))
			for _, review := range node.LatestReviews.Nodes {
				if review.Author == nil || review.Author.Login == "" {
					continue
				}
				answered = append(answered, reviewVoice{
					login: review.Author.Login, avatar: review.Author.AvatarURL, state: review.State,
				})
			}
			requested := make([]reviewVoice, 0, len(node.ReviewRequests.Nodes))
			for _, request := range node.ReviewRequests.Nodes {
				who := request.RequestedReviewer
				if who == nil {
					continue
				}
				if who.Typename == "Team" {
					if who.Slug != "" {
						requested = append(requested, reviewVoice{login: who.Slug, team: true})
					}
					continue
				}
				if who.Login != "" {
					requested = append(requested, reviewVoice{login: who.Login, avatar: who.AvatarURL})
				}
			}
			applyReviewState(pr, answered, requested)
			for _, review := range node.PendingReviews.Nodes {
				if review.Author != nil && review.Author.Login == viewer {
					pr.ViewerPendingReview = true
					break
				}
			}
		}
		return nil
	})
}

// mentionedNumbers pulls bare issue numbers out of a title.
func mentionedNumbers(title string) []int {
	var numbers []int
	seen := map[int]bool{}
	for _, match := range mentionPattern.FindAllStringSubmatch(title, -1) {
		number, err := strconv.Atoi(match[1])
		if err != nil || seen[number] {
			continue
		}
		seen[number] = true
		numbers = append(numbers, number)
	}
	return numbers
}

// parseRefs extracts issue numbers from a `refs #12, #34` line.
func (c *Client) parseRefs(body string) []int {
	pattern := c.RefsPattern
	if pattern == nil {
		pattern = DefaultRefsPattern
	}
	var numbers []int
	seen := map[int]bool{}
	for _, match := range pattern.FindAllStringSubmatch(body, -1) {
		if len(match) < 2 {
			continue
		}
		for _, found := range refNumberPattern.FindAllStringSubmatch(match[1], -1) {
			number, err := strconv.Atoi(found[1])
			if err != nil || seen[number] {
				continue
			}
			seen[number] = true
			numbers = append(numbers, number)
		}
	}
	return numbers
}

// addCrossReferences adds the noisy third layer: anything the timeline says
// mentions the issue. willCloseTarget separates a real closing reference from a
// passing mention.
func (c *Client) addCrossReferences(ctx context.Context, issues []*graph.Issue, prs []*graph.PullRequest, progress Progress) error {
	prByKey := map[string]*graph.PullRequest{}
	for _, pr := range prs {
		prByKey[pr.Repository+"#"+strconv.Itoa(pr.Number)] = pr
	}
	linked := map[string]bool{}
	for _, pr := range prs {
		for _, link := range pr.Links {
			linked[link.IssueID+"\x00"+pr.ID] = true
		}
	}

	ids := make([]string, 0, len(issues))
	for _, issue := range issues {
		ids = append(ids, issue.ID)
	}
	for start := 0; start < len(ids); start += maxIDsPerQuery {
		end := min(start+maxIDsPerQuery, len(ids))
		progress(PhaseCrossRef, end, len(ids), len(issues))

		query := "query{nodes(ids:[" + quoteJoin(ids[start:end]) + "]){...on Issue{id " + xrefFields + "}}}"

		var payload struct {
			Data struct {
				Nodes []struct {
					ID            string `json:"id"`
					TimelineItems struct {
						Nodes []struct {
							Typename        string `json:"__typename"`
							WillCloseTarget bool   `json:"willCloseTarget"`
							Source          struct {
								Typename   string `json:"__typename"`
								Number     int    `json:"number"`
								Repository struct {
									NameWithOwner string `json:"nameWithOwner"`
								} `json:"repository"`
							} `json:"source"`
						} `json:"nodes"`
					} `json:"timelineItems"`
				} `json:"nodes"`
			} `json:"data"`
		}
		if err := c.graphql(ctx, query, nil, &payload); err != nil {
			return err
		}
		for _, node := range payload.Data.Nodes {
			for _, item := range node.TimelineItems.Nodes {
				if item.Source.Typename != "PullRequest" {
					continue
				}
				pr := prByKey[item.Source.Repository.NameWithOwner+"#"+strconv.Itoa(item.Source.Number)]
				if pr == nil || linked[node.ID+"\x00"+pr.ID] {
					continue
				}
				linked[node.ID+"\x00"+pr.ID] = true
				kind := graph.LinkXref
				if item.WillCloseTarget {
					kind = graph.LinkCloses
				}
				pr.Links = append(pr.Links, graph.IssueLink{IssueID: node.ID, Repository: item.Source.Repository.NameWithOwner, Number: item.Source.Number, Kind: kind})
			}
		}
	}
	return nil
}

// graphql runs one GraphQL operation through the gh CLI.
func (c *Client) graphql(ctx context.Context, query string, variables map[string]string, target any) error {
	out, err := c.run(ctx, query, variables)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(out, target); err != nil {
		return fmt.Errorf("decode GitHub response: %w", err)
	}
	return nil
}

// graphqlAllowingMissing runs a query whose answer is expected to contain holes.
//
// `gh api graphql` exits non-zero the moment the response carries an `errors`
// array, even when `data` came back populated beside it — and GraphQL fills
// both in for a partial result. `repository{issue(number:)}` does exactly that:
// GitHub answers `{"data":{"r0":{"i0":null}},"errors":[{"type":"NOT_FOUND"…}]}`
// and gh exits 1. Going through the plain path therefore failed the whole load
// whenever a `refs #N` line named a pull request — issues and pull requests
// share one number space, so that is ordinary in a stacked-branch repository —
// or simply a number that no longer exists.
//
// Only NOT_FOUND is forgiven, and only when data actually came back. A response
// with no data, or carrying any other kind of error, is still a failure.
func (c *Client) graphqlAllowingMissing(ctx context.Context, query string, target any) error {
	out, runErr := c.run(ctx, query, nil)
	if len(out) == 0 {
		return runErr
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		if runErr != nil {
			return runErr
		}
		return fmt.Errorf("decode GitHub response: %w", err)
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		if runErr != nil {
			return runErr
		}
		return fmt.Errorf("GitHub API returned no data")
	}
	for _, failure := range envelope.Errors {
		if failure.Type != "NOT_FOUND" {
			if runErr != nil {
				return runErr
			}
			return fmt.Errorf("GitHub API: %s", failure.Message)
		}
	}
	if err := json.Unmarshal(out, target); err != nil {
		return fmt.Errorf("decode GitHub response: %w", err)
	}
	return nil
}

// run shells out to gh and hands back stdout even when the command failed, so
// a caller that knows how to read a partial GraphQL answer can still do so.
func (c *Client) run(ctx context.Context, query string, variables map[string]string) ([]byte, error) {
	args := []string{"api", "graphql"}
	if c.Hostname != "" {
		args = append(args, "--hostname", c.Hostname)
	}
	args = append(args, "-f", "query="+query)

	keys := make([]string, 0, len(variables))
	for key := range variables {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "-F", key+"="+variables[key])
	}

	cmd := exec.CommandContext(ctx, "gh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("GitHub API: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

type rawRef struct {
	ID         string `json:"id"`
	Number     int    `json:"number"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
}

type rawActor struct {
	Typename  string `json:"__typename"`
	Login     string `json:"login"`
	AvatarURL string `json:"avatarUrl"`
}

type rawIssue struct {
	ID          string    `json:"id"`
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	URL         string    `json:"url"`
	State       string    `json:"state"`
	StateReason string    `json:"stateReason"`
	UpdatedAt   time.Time `json:"updatedAt"`
	IssueType   *struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	} `json:"issueType"`
	Repository struct {
		ID               string `json:"id"`
		NameWithOwner    string `json:"nameWithOwner"`
		URL              string `json:"url"`
		DefaultBranchRef *struct {
			Name string `json:"name"`
		} `json:"defaultBranchRef"`
	} `json:"repository"`
	Author    *rawActor `json:"author"`
	Assignees struct {
		Nodes []struct {
			Login     string `json:"login"`
			AvatarURL string `json:"avatarUrl"`
		} `json:"nodes"`
	} `json:"assignees"`
	Labels struct {
		Nodes []struct {
			Name  string `json:"name"`
			Color string `json:"color"`
		} `json:"nodes"`
	} `json:"labels"`
	Milestone *struct {
		Title string `json:"title"`
	} `json:"milestone"`
	Parent    *rawRef `json:"parent"`
	SubIssues struct {
		TotalCount int      `json:"totalCount"`
		Nodes      []rawRef `json:"nodes"`
	} `json:"subIssues"`
	SubIssuesSummary struct {
		Total     int `json:"total"`
		Completed int `json:"completed"`
	} `json:"subIssuesSummary"`
	BlockedBy struct {
		TotalCount int      `json:"totalCount"`
		Nodes      []rawRef `json:"nodes"`
	} `json:"blockedBy"`
	Blocking struct {
		TotalCount int      `json:"totalCount"`
		Nodes      []rawRef `json:"nodes"`
	} `json:"blocking"`
	DuplicateOf                    *rawRef `json:"duplicateOf"`
	ClosedByPullRequestsReferences struct {
		Nodes []rawRef `json:"nodes"`
	} `json:"closedByPullRequestsReferences"`
}

func (r rawIssue) convert(source string) (*graph.Issue, []string) {
	issue := &graph.Issue{
		ID:            r.ID,
		Number:        r.Number,
		Title:         r.Title,
		URL:           r.URL,
		State:         r.State,
		StateReason:   r.StateReason,
		UpdatedAt:     r.UpdatedAt,
		RepositoryID:  r.Repository.ID,
		Repository:    r.Repository.NameWithOwner,
		RepositoryURL: r.Repository.URL,
		SubTotal:      r.SubIssuesSummary.Total,
		SubCompleted:  r.SubIssuesSummary.Completed,
		Source:        source,
	}
	if r.Repository.DefaultBranchRef != nil {
		issue.DefaultBranch = r.Repository.DefaultBranchRef.Name
	}
	if r.IssueType != nil {
		issue.Type = &graph.IssueType{Name: r.IssueType.Name, Color: r.IssueType.Color}
	}
	if r.Author != nil {
		issue.Author = graph.User{Login: r.Author.Login, AvatarURL: r.Author.AvatarURL}
	}
	for _, assignee := range r.Assignees.Nodes {
		issue.Assignees = append(issue.Assignees, graph.User{Login: assignee.Login, AvatarURL: assignee.AvatarURL})
	}
	for _, label := range r.Labels.Nodes {
		issue.Labels = append(issue.Labels, graph.Label{Name: label.Name, Color: label.Color})
	}
	if r.Milestone != nil {
		issue.Milestone = r.Milestone.Title
	}
	if r.Parent != nil {
		issue.ParentID = r.Parent.ID
	}
	for _, sub := range r.SubIssues.Nodes {
		issue.SubIssueIDs = append(issue.SubIssueIDs, sub.ID)
	}
	for _, blocker := range r.BlockedBy.Nodes {
		issue.BlockedByIDs = append(issue.BlockedByIDs, blocker.ID)
	}
	for _, blocked := range r.Blocking.Nodes {
		issue.BlockingIDs = append(issue.BlockingIDs, blocked.ID)
	}
	if r.DuplicateOf != nil {
		issue.DuplicateOfID = r.DuplicateOf.ID
	}
	closes := make([]string, 0, len(r.ClosedByPullRequestsReferences.Nodes))
	for _, pr := range r.ClosedByPullRequestsReferences.Nodes {
		closes = append(closes, pr.ID)
	}
	return issue, closes
}

type rawPullRequest struct {
	ID          string    `json:"id"`
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	URL         string    `json:"url"`
	State       string    `json:"state"`
	IsDraft     bool      `json:"isDraft"`
	UpdatedAt   time.Time `json:"updatedAt"`
	Body        string    `json:"body"`
	BaseRefName string    `json:"baseRefName"`
	HeadRefName string    `json:"headRefName"`
	Author      *rawActor `json:"author"`
	Repository  struct {
		ID            string `json:"id"`
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	HeadRepository *struct {
		ID string `json:"id"`
	} `json:"headRepository"`
	ReviewDecision          string `json:"reviewDecision"`
	ClosingIssuesReferences struct {
		Nodes []rawRef `json:"nodes"`
	} `json:"closingIssuesReferences"`
}

func (r rawPullRequest) convert() *graph.PullRequest {
	pr := &graph.PullRequest{
		ID:             r.ID,
		Number:         r.Number,
		Title:          r.Title,
		URL:            r.URL,
		State:          r.State,
		IsDraft:        r.IsDraft,
		UpdatedAt:      r.UpdatedAt,
		RepositoryID:   r.Repository.ID,
		Repository:     r.Repository.NameWithOwner,
		BaseRefName:    r.BaseRefName,
		HeadRefName:    r.HeadRefName,
		ReviewDecision: r.ReviewDecision,
	}
	if r.Author != nil {
		pr.Author = graph.User{Login: r.Author.Login, AvatarURL: r.Author.AvatarURL}
		pr.IsBot = r.Author.Typename == "Bot" || strings.HasSuffix(strings.ToLower(r.Author.Login), "[bot]")
	}
	if r.HeadRepository != nil {
		pr.HeadRepositoryID = r.HeadRepository.ID
	}
	// CIState is filled in later by fillReviewAndCI: the check rollup is too
	// expensive to ask for on every pull request the scan looks at.
	return pr
}

const issueFields = `
  id number title url state stateReason updatedAt
  issueType{name color}
  repository{id nameWithOwner url defaultBranchRef{name}}
  author{__typename login avatarUrl}
  assignees(first:10){nodes{login avatarUrl}}
  labels(first:10){nodes{name color}}
  milestone{title}
  parent{id number repository{nameWithOwner}}
  subIssues(first:50){totalCount nodes{id number repository{nameWithOwner}}}
  subIssuesSummary{total completed}
  blockedBy(first:20){totalCount nodes{id number repository{nameWithOwner}}}
  blocking(first:20){totalCount nodes{id number repository{nameWithOwner}}}
  duplicateOf{id number repository{nameWithOwner}}
  closedByPullRequestsReferences(first:20,includeClosedPrs:true){nodes{id number repository{nameWithOwner}}}
`

const prFields = `
  id number title url state isDraft updatedAt body baseRefName headRefName
  author{__typename login avatarUrl}
  repository{id nameWithOwner}
  headRepository{id}
  reviewDecision
  closingIssuesReferences(first:10){nodes{id number repository{nameWithOwner}}}
`

// What is asked for once the canvas is settled, for the handful of pull
// requests that made it. statusCheckRollup aggregates every check run on the
// head commit and is the most expensive thing GitHub is asked for here: the
// per-repository scan pulled it for 60 pull requests each, then threw most of
// them away. The review connections ride along in the same request rather than
// costing a round trip of their own.
//
// pendingReviews is the viewer's own unsubmitted review. It is invisible to
// everybody else, which is exactly why it is worth a mark on the card.
const ciFields = `
  id
  commits(last:1){nodes{commit{statusCheckRollup{state}}}}
  reviewRequests(first:10){nodes{requestedReviewer{__typename
    ...on User{login avatarUrl}
    ...on Bot{login avatarUrl}
    ...on Team{slug}}}}
  latestReviews(first:20){nodes{state author{login avatarUrl}}}
  pendingReviews:reviews(first:10,states:[PENDING]){nodes{author{login}}}
`

const xrefFields = `
  timelineItems(last:50,itemTypes:[CROSS_REFERENCED_EVENT]){nodes{
    __typename
    ... on CrossReferencedEvent{willCloseTarget source{__typename ... on PullRequest{number repository{nameWithOwner}}}}
  }}
`

const searchQuery = `query($q:String!){
  viewer{login}
  search(query:$q,type:ISSUE,first:100){
    issueCount
    nodes{... on Issue{` + issueFields + `}}
  }
}`

// GitHub's search index is one index: pull requests come back from type:ISSUE
// too, so this is the same call shape with the other inline fragment.
const prSearchQuery = `query($q:String!){
  viewer{login}
  search(query:$q,type:ISSUE,first:100){
    issueCount
    nodes{... on PullRequest{` + prFields + `}}
  }
}`
