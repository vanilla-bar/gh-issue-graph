package github

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/vanilla-bar/gh-issue-graph/internal/graph"
)

// The `refs #N` convention is what separates "merging closes this" from
// "related, deliberately left open". Getting this parser wrong silently drops
// roughly a third of the issue-to-pull-request links.
func TestParseRefs(t *testing.T) {
	client := New("")
	cases := []struct {
		name string
		body string
		want []int
	}{
		{"leading line", "refs #327\n\n## why\n...", []int{327}},
		{"capitalised", "Refs #12\n", []int{12}},
		{"singular", "ref #8\n", []int{8}},
		{"colon", "refs: #99\n", []int{99}},
		{"several", "refs #12, #34 #56\n", []int{12, 34, 56}},
		{"inside a list", "- refs #7\n", []int{7}},
		{"repeated", "refs #5\nrefs #5\n", []int{5}},
		{"not a reference", "this refactors #5\n", nil},
		{"closes is not refs", "Closes #5\n", nil},
		{"mention alone", "see #5 for background\n", nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := client.parseRefs(testCase.body)
			if len(got) != len(testCase.want) {
				t.Fatalf("parseRefs(%q) = %v, want %v", testCase.body, got, testCase.want)
			}
			for i := range got {
				if got[i] != testCase.want[i] {
					t.Fatalf("parseRefs(%q) = %v, want %v", testCase.body, got, testCase.want)
				}
			}
		})
	}
}

func TestParseRefsHonoursACustomPattern(t *testing.T) {
	client := New("")
	client.RefsPattern = regexp.MustCompile(`(?im)^related[ \t]+((?:#\d+[ \t,]*)+)`)
	if got := client.parseRefs("related #42\n"); len(got) != 1 || got[0] != 42 {
		t.Fatalf("parseRefs with a custom pattern = %v, want [42]", got)
	}
	if got := client.parseRefs("refs #42\n"); got != nil {
		t.Fatalf("the custom pattern should replace the default, got %v", got)
	}
}

// queriesOf flattens the specs to their query strings for assertions.
func queriesOf(specs []searchSpec) []string {
	out := make([]string, 0, len(specs))
	for _, spec := range specs {
		out = append(out, spec.query)
	}
	return out
}

func TestBuildSearchSpecsRunsOneQueryPerScope(t *testing.T) {
	specs := queriesOf(buildSearchSpecs(graph.SearchOptions{Assigned: true, Authored: true, Mentioned: true}))
	if len(specs) != 3 {
		t.Fatalf("specs = %v, want one query per scope", specs)
	}
	wants := []string{"assignee:@me", "author:@me", "mentions:@me"}
	for i, want := range wants {
		if !strings.Contains(specs[i], want) {
			t.Fatalf("spec %d = %q, want it to contain %q", i, specs[i], want)
		}
	}
}

func TestBuildSearchSpecsIncludesClosedOnRequest(t *testing.T) {
	specs := queriesOf(buildSearchSpecs(graph.SearchOptions{Assigned: true, IncludeClosed: true}))
	if len(specs) != 1 || strings.Contains(specs[0], "is:open") {
		t.Fatalf("spec = %v, want the open filter dropped", specs)
	}
}

func TestBuildSearchSpecsAppendsARawQuery(t *testing.T) {
	specs := queriesOf(buildSearchSpecs(graph.SearchOptions{Assigned: true, Query: "label:bug"}))
	if len(specs) != 2 || !strings.HasSuffix(specs[1], "label:bug") {
		t.Fatalf("specs = %v, want the raw query appended", specs)
	}
}

func TestIssueFieldsRequestEveryRelation(t *testing.T) {
	// These five relations are the whole graph; losing one silently flattens it.
	for _, field := range []string{"parent{", "subIssues(", "subIssuesSummary{", "blockedBy(", "duplicateOf{", "closedByPullRequestsReferences("} {
		if !strings.Contains(issueFields, field) {
			t.Fatalf("issueFields is missing %q", field)
		}
	}
}

func TestPullRequestFieldsRequestTheBody(t *testing.T) {
	// Without the body there is no way to see a `refs #N` link.
	if !strings.Contains(prFields, "body") {
		t.Fatal("prFields must request body so refs can be parsed")
	}
	if !strings.Contains(prFields, "closingIssuesReferences(") {
		t.Fatal("prFields must request closingIssuesReferences")
	}
}

func TestCrossReferenceFieldsAskWhetherTheReferenceCloses(t *testing.T) {
	if !strings.Contains(xrefFields, "willCloseTarget") {
		t.Fatal("xrefFields must request willCloseTarget to tell a closing reference from a mention")
	}
}

func TestReviewRequestedSpecIsOffUnlessAsked(t *testing.T) {
	if _, ok := reviewRequestedSpec(graph.SearchOptions{}); ok {
		t.Fatal("the review scope ran without being asked for")
	}
}

func TestReviewRequestedSpecSearchesOpenPullRequests(t *testing.T) {
	spec, ok := reviewRequestedSpec(graph.SearchOptions{ReviewRequested: true})
	if !ok {
		t.Fatal("no spec produced")
	}
	for _, want := range []string{"is:pr", "is:open", "review-requested:@me"} {
		if !strings.Contains(spec.query, want) {
			t.Fatalf("query %q is missing %q", spec.query, want)
		}
	}
	if spec.reason != graph.ReasonReviewRequested {
		t.Fatalf("reason = %q", spec.reason)
	}
}

// A closed pull request is waiting on nobody, so this scope stays open-only
// even when the reader asked to see closed issues.
func TestReviewRequestedSpecStaysOpenEvenWithClosedIssues(t *testing.T) {
	spec, _ := reviewRequestedSpec(graph.SearchOptions{ReviewRequested: true, IncludeClosed: true})
	if !strings.Contains(spec.query, "is:open") {
		t.Fatalf("query %q dropped is:open", spec.query)
	}
}

func TestReviewRequestedSpecIsNarrowedByRepository(t *testing.T) {
	spec, _ := reviewRequestedSpec(graph.SearchOptions{ReviewRequested: true, Repo: "hoge/fuga"})
	if !strings.HasPrefix(spec.query, "repo:hoge/fuga ") {
		t.Fatalf("query %q is not scoped to the repository", spec.query)
	}
}

func TestPullRequestSearchAsksForPullRequestFields(t *testing.T) {
	if !strings.Contains(prSearchQuery, "on PullRequest") {
		t.Fatal("the pull request search does not select pull requests")
	}
	if !strings.Contains(prSearchQuery, "closingIssuesReferences") {
		t.Fatal("without closingIssuesReferences there is nothing to reverse-look-up")
	}
}

// The reverse lookup takes the two layers it can trust. A bare number in the
// title is a guess, and fetching on it would invent a parent.
func TestReferencedIssuesTakesIDsAndRefsButNotTitleGuesses(t *testing.T) {
	client := New("")
	raw := rawPullRequest{Body: "refs #42\n"}
	raw.Title = "fix(map): #99 something else"
	raw.Repository.NameWithOwner = "hoge/fuga"
	raw.ClosingIssuesReferences.Nodes = []rawRef{{ID: "I_1"}}

	ids, refs := client.referencedIssues([]rawPullRequest{raw, raw})
	if len(ids) != 1 || ids[0] != "I_1" {
		t.Fatalf("ids = %v, want one I_1", ids)
	}
	if len(refs) != 1 || refs[0] != (issueRef{repository: "hoge/fuga", number: 42}) {
		t.Fatalf("refs = %v, want one hoge/fuga#42", refs)
	}
	for _, ref := range refs {
		if ref.number == 99 {
			t.Fatal("a bare number in the title was turned into a fetch")
		}
	}
}

// The check rollup is the most expensive field GitHub was being asked for, and
// the scan used to pull it for every open and merged pull request in every
// repository before throwing most of them away.
func TestPullRequestFieldsLeaveTheCheckRollupToASecondPass(t *testing.T) {
	if strings.Contains(prFields, "statusCheckRollup") {
		t.Fatal("the bulk pull request scan still asks for the check rollup")
	}
	if !strings.Contains(ciFields, "statusCheckRollup") {
		t.Fatal("nothing asks for the check rollup at all")
	}
}

func TestChunkSplitsWithoutLosingAnything(t *testing.T) {
	for _, tc := range []struct {
		length, size int
		want         [][2]int
	}{
		{0, 100, nil},
		{1, 100, [][2]int{{0, 1}}},
		{100, 100, [][2]int{{0, 100}}},
		{101, 100, [][2]int{{0, 100}, {100, 101}}},
		{250, 100, [][2]int{{0, 100}, {100, 200}, {200, 250}}},
	} {
		got := chunk(tc.length, tc.size)
		if len(got) != len(tc.want) {
			t.Fatalf("chunk(%d,%d) = %v, want %v", tc.length, tc.size, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("chunk(%d,%d) = %v, want %v", tc.length, tc.size, got, tc.want)
			}
		}
	}
}

func TestInParallelRunsEveryJobWhenNothingFails(t *testing.T) {
	var mu sync.Mutex
	seen := map[int]bool{}
	err := inParallel(context.Background(), 25, func(i int) error {
		mu.Lock()
		seen[i] = true
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(seen) != 25 {
		t.Fatalf("ran %d of 25 jobs", len(seen))
	}
}

func TestInParallelReturnsTheFirstError(t *testing.T) {
	err := inParallel(context.Background(), 25, func(i int) error {
		if i == 7 {
			return errTest
		}
		return nil
	})
	if err != errTest {
		t.Fatalf("err = %v, want the job's error", err)
	}
}

// Once a batch has failed, the rest would each spawn a gh process only to fail
// too; likewise for a cancelled context.
func TestInParallelSkipsJobsUnderACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var ran int32
	err := inParallel(ctx, 25, func(int) error {
		atomic.AddInt32(&ran, 1)
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := atomic.LoadInt32(&ran); got != 0 {
		t.Fatalf("ran %d jobs under a cancelled context", got)
	}
}

// A single batch must not pay for a goroutine and a channel, and must surface
// its error unchanged.
func TestInParallelIsATailCallForOneJob(t *testing.T) {
	if err := inParallel(context.Background(), 1, func(int) error { return errTest }); err != errTest {
		t.Fatalf("err = %v", err)
	}
	if err := inParallel(context.Background(), 0, func(int) error { return errTest }); err != nil {
		t.Fatalf("err = %v, want nil for no jobs", err)
	}
}

func TestQuoteJoin(t *testing.T) {
	if got := quoteJoin([]string{"I_1", "I_2"}); got != `"I_1","I_2"` {
		t.Fatalf("quoteJoin = %s", got)
	}
	if got := quoteJoin(nil); got != "" {
		t.Fatalf("quoteJoin(nil) = %q, want empty", got)
	}
}

func TestPendingIDsSkipsWhatIsAlreadyLoaded(t *testing.T) {
	client := New("")
	loaded := &graph.Issue{ID: "I_1", ParentID: "I_parent", SubIssueIDs: []string{"I_child", "I_1"}, BlockedByIDs: []string{"I_blocker"}}
	byID := map[string]*graph.Issue{"I_1": loaded, "I_child": {ID: "I_child"}}

	pending, reasons := client.pendingIDs([]*graph.Issue{loaded}, byID)
	want := map[string]bool{"I_parent": true, "I_blocker": true}
	if len(pending) != len(want) {
		t.Fatalf("pending = %v, want %v", pending, want)
	}
	for _, id := range pending {
		if !want[id] {
			t.Fatalf("pending = %v, did not expect %q", pending, id)
		}
		if reasons[id] == "" {
			t.Fatalf("no reason recorded for %q; a complement with no explanation looks like a bug", id)
		}
	}
}

func TestConvertMarksBotAuthors(t *testing.T) {
	raw := rawPullRequest{ID: "PR_1", Author: &rawActor{Login: "dependabot[bot]"}}
	if !raw.convert().IsBot {
		t.Fatal("a [bot] suffix should mark the author as a bot")
	}
	raw = rawPullRequest{ID: "PR_2", Author: &rawActor{Typename: "Bot", Login: "renovate"}}
	if !raw.convert().IsBot {
		t.Fatal("a Bot typename should mark the author as a bot")
	}
}

func TestConvertIssueCarriesRelations(t *testing.T) {
	raw := rawIssue{ID: "I_1", Number: 7, State: "OPEN"}
	raw.Parent = &rawRef{ID: "I_parent"}
	raw.SubIssues.Nodes = []rawRef{{ID: "I_a"}, {ID: "I_b"}}
	raw.SubIssuesSummary.Total, raw.SubIssuesSummary.Completed = 2, 1
	raw.BlockedBy.Nodes = []rawRef{{ID: "I_blocker"}}
	raw.DuplicateOf = &rawRef{ID: "I_original"}
	raw.ClosedByPullRequestsReferences.Nodes = []rawRef{{ID: "PR_1"}}

	issue, closes := raw.convert("search")
	if issue.ParentID != "I_parent" || len(issue.SubIssueIDs) != 2 || issue.SubTotal != 2 || issue.SubCompleted != 1 {
		t.Fatalf("issue = %+v", issue)
	}
	if len(issue.BlockedByIDs) != 1 || issue.DuplicateOfID != "I_original" {
		t.Fatalf("issue = %+v", issue)
	}
	if len(closes) != 1 || closes[0] != "PR_1" {
		t.Fatalf("closes = %v", closes)
	}
}

// A repository filter has to narrow the scopes, not replace them. It used to
// replace them, so ticking "assigned" with a repository named still returned
// every open issue in that repository.
func TestRepositoryFilterNarrowsEachScope(t *testing.T) {
	specs := queriesOf(buildSearchSpecs(graph.SearchOptions{Repo: "hoge/fuga", Assigned: true}))
	if len(specs) != 1 {
		t.Fatalf("specs = %v, want one", specs)
	}
	for _, want := range []string{"repo:hoge/fuga", "is:issue", "is:open", "assignee:@me"} {
		if !strings.Contains(specs[0], want) {
			t.Fatalf("spec = %q, want it to contain %q", specs[0], want)
		}
	}
	if strings.Contains(specs[0], "author:@me") || strings.Contains(specs[0], "mentions:@me") {
		t.Fatalf("spec = %q, want the unticked scopes left out", specs[0])
	}
}

func TestRepositoryFilterAppliesToEveryScope(t *testing.T) {
	specs := queriesOf(buildSearchSpecs(graph.SearchOptions{Repo: "hoge/fuga", Assigned: true, Authored: true, Mentioned: true}))
	if len(specs) != 3 {
		t.Fatalf("specs = %v, want one per scope", specs)
	}
	for _, spec := range specs {
		if !strings.HasPrefix(spec, "repo:hoge/fuga ") {
			t.Fatalf("spec = %q, want it scoped to the repository", spec)
		}
	}
}

// With a repository named and nothing ticked, the repository is the scope.
func TestRepositoryWithoutScopesFallsBackToTheWholeRepository(t *testing.T) {
	specs := queriesOf(buildSearchSpecs(graph.SearchOptions{Repo: "hoge/fuga"}))
	if len(specs) != 1 || strings.Contains(specs[0], "@me") {
		t.Fatalf("specs = %v, want one unscoped repository query", specs)
	}
}

// Without a repository there is nothing to fall back to: searching all of
// GitHub is not a graph.
func TestNoRepositoryAndNoScopesProducesNoQuery(t *testing.T) {
	if specs := buildSearchSpecs(graph.SearchOptions{}); len(specs) != 0 {
		t.Fatalf("specs = %v, want none", specs)
	}
}

func TestRawQueryIsScopedToo(t *testing.T) {
	specs := queriesOf(buildSearchSpecs(graph.SearchOptions{Repo: "hoge/fuga", Query: "label:bug"}))
	if len(specs) != 1 || !strings.HasPrefix(specs[0], "repo:hoge/fuga ") || !strings.HasSuffix(specs[0], "label:bug") {
		t.Fatalf("specs = %v, want the raw query narrowed by the repository", specs)
	}
}

var errTest = errors.New("test")
