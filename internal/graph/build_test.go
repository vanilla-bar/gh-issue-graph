package graph

import (
	"encoding/json"
	"testing"
	"time"
)

func repo(id, name, defaultBranch string) *Repository {
	return &Repository{ID: id, NameWithOwner: name, URL: "https://example.com/" + name, DefaultBranch: defaultBranch}
}

func issue(id string, number int, state string) *Issue {
	return &Issue{ID: id, Number: number, State: state, RepositoryID: "R", Repository: "hoge/fuga", Source: "search"}
}

func pull(id string, number int, state, base, head string) *PullRequest {
	return &PullRequest{ID: id, Number: number, State: state, BaseRefName: base, HeadRefName: head,
		RepositoryID: "R", HeadRepositoryID: "R", Repository: "hoge/fuga"}
}

func edgeKinds(result Result) map[string]int {
	counts := map[string]int{}
	for _, edge := range result.Edges {
		counts[edge.Kind]++
	}
	return counts
}

func rankOf(t *testing.T, result Result, id string) int {
	t.Helper()
	for _, node := range result.Nodes {
		if node.ID == id {
			return node.Rank
		}
	}
	t.Fatalf("node %q not in result", id)
	return -1
}

func TestParentHierarchyDrivesRank(t *testing.T) {
	parent := issue("I_1", 1, "OPEN")
	child := issue("I_2", 2, "OPEN")
	child.ParentID = parent.ID
	grandchild := issue("I_3", 3, "OPEN")
	grandchild.ParentID = child.ID

	result := Build(Input{
		Issues:       []*Issue{parent, child, grandchild},
		Repositories: map[string]*Repository{"R": repo("R", "hoge/fuga", "dev")},
	})

	if got := rankOf(t, result, IssueNodeID("I_1")); got != 0 {
		t.Fatalf("root issue rank = %d, want 0", got)
	}
	if got := rankOf(t, result, IssueNodeID("I_2")); got != 1 {
		t.Fatalf("child rank = %d, want 1", got)
	}
	if got := rankOf(t, result, IssueNodeID("I_3")); got != 2 {
		t.Fatalf("grandchild rank = %d, want 2", got)
	}
	// No repository edge: the lane frame says which repository these belong to.
	if counts := edgeKinds(result); counts[EdgeParent] != 2 || len(counts) != 1 {
		t.Fatalf("edge kinds = %v, want two parent edges and nothing else", counts)
	}
}

func TestPullRequestSitsRightOfItsIssue(t *testing.T) {
	parent := issue("I_1", 1, "OPEN")
	child := issue("I_2", 2, "CLOSED")
	child.ParentID = parent.ID

	pr := pull("PR_1", 10, "MERGED", "dev", "feature/2-thing")
	pr.Links = []IssueLink{{IssueID: child.ID, Repository: child.Repository, Number: child.Number, Kind: LinkCloses}}

	result := Build(Input{
		Issues:       []*Issue{parent, child},
		PullRequests: []*PullRequest{pr},
		Repositories: map[string]*Repository{"R": repo("R", "hoge/fuga", "dev")},
	})

	if got := rankOf(t, result, PRNodeID("PR_1")); got != 2 {
		t.Fatalf("pull request rank = %d, want 2 (one right of its rank 1 issue)", got)
	}
	if counts := edgeKinds(result); counts[EdgePRCloses] != 1 {
		t.Fatalf("edge kinds = %v, want one pr-closes", counts)
	}
}

// A release pull request whose head is the default branch must not adopt every
// pull request opened against that branch.
func TestReleasePullRequestDoesNotAdoptTheWholeRepository(t *testing.T) {
	release := pull("PR_release", 100, "OPEN", "stage", "dev")
	first := pull("PR_1", 1, "OPEN", "dev", "feature/one")
	second := pull("PR_2", 2, "OPEN", "dev", "feature/two")
	stacked := pull("PR_3", 3, "OPEN", "feature/one", "feature/one-part-two")

	result := Build(Input{
		PullRequests: []*PullRequest{release, first, second, stacked},
		Repositories: map[string]*Repository{"R": repo("R", "hoge/fuga", "dev")},
	})

	counts := edgeKinds(result)
	if counts[EdgePRStack] != 1 {
		t.Fatalf("pr-stack edges = %d, want exactly 1 (only feature/one -> feature/one-part-two)", counts[EdgePRStack])
	}
	for _, edge := range result.Edges {
		if edge.Kind == EdgePRStack && edge.Source != PRNodeID("PR_1") {
			t.Fatalf("unexpected stack parent %q", edge.Source)
		}
	}
}

func TestAttentionWhenChildrenAreDoneButParentIsOpen(t *testing.T) {
	parent := issue("I_1", 1, "OPEN")
	parent.SubTotal, parent.SubCompleted = 3, 3

	result := Build(Input{
		Issues:       []*Issue{parent},
		Repositories: map[string]*Repository{"R": repo("R", "hoge/fuga", "dev")},
	})
	_ = result

	if len(parent.Attention) != 1 || parent.Attention[0] != AttentionChildrenDone {
		t.Fatalf("attention = %v, want [%s]", parent.Attention, AttentionChildrenDone)
	}
	if parent.Actionable {
		t.Fatal("an issue that only needs wrapping up should not be flagged ready")
	}
}

func TestAttentionWhenAMergedPullRequestLeftTheIssueOpen(t *testing.T) {
	target := issue("I_1", 1, "OPEN")
	pr := pull("PR_1", 5, "MERGED", "dev", "fix/1")
	pr.Links = []IssueLink{{IssueID: target.ID, Repository: target.Repository, Number: 1, Kind: LinkRefs}}

	Build(Input{
		Issues:       []*Issue{target},
		PullRequests: []*PullRequest{pr},
		Repositories: map[string]*Repository{"R": repo("R", "hoge/fuga", "dev")},
	})

	if len(target.Attention) != 1 || target.Attention[0] != AttentionMergedPROpen {
		t.Fatalf("attention = %v, want [%s]", target.Attention, AttentionMergedPROpen)
	}
}

func TestActionableNeedsSomethingBlockedToContrastWith(t *testing.T) {
	free := issue("I_1", 1, "OPEN")
	free.Assignees = []User{{Login: "octocat"}}

	Build(Input{
		Issues:       []*Issue{free},
		Repositories: map[string]*Repository{"R": repo("R", "hoge/fuga", "dev")},
		Viewer:       "octocat",
	})
	if free.Actionable {
		t.Fatal("without any blocked issue the ready badge says nothing and should be dropped")
	}

	blocker := issue("I_2", 2, "OPEN")
	blocked := issue("I_3", 3, "OPEN")
	blocked.BlockedByIDs = []string{blocker.ID}
	blocked.Assignees = []User{{Login: "octocat"}}
	free2 := issue("I_4", 4, "OPEN")
	free2.Assignees = []User{{Login: "octocat"}}

	Build(Input{
		Issues:       []*Issue{blocker, blocked, free2},
		Repositories: map[string]*Repository{"R": repo("R", "hoge/fuga", "dev")},
		Viewer:       "octocat",
	})
	if blocked.Actionable {
		t.Fatal("an issue with an open blocker is not ready")
	}
	if !free2.Actionable {
		t.Fatal("an unblocked issue should be ready once the graph has blocked work in it")
	}
}

func TestStrongestLinkWins(t *testing.T) {
	target := issue("I_1", 1, "OPEN")
	pr := pull("PR_1", 5, "OPEN", "dev", "fix/1")
	pr.Links = []IssueLink{
		{IssueID: target.ID, Repository: target.Repository, Number: 1, Kind: LinkXref},
		{IssueID: target.ID, Repository: target.Repository, Number: 1, Kind: LinkCloses},
	}

	result := Build(Input{
		Issues:       []*Issue{target},
		PullRequests: []*PullRequest{pr},
		Repositories: map[string]*Repository{"R": repo("R", "hoge/fuga", "dev")},
	})

	counts := edgeKinds(result)
	if counts[EdgePRCloses] != 1 || counts[EdgePRXref] != 0 {
		t.Fatalf("edge kinds = %v, want the closes edge only", counts)
	}
}

func TestBodyParsedRefsResolveToIssueIDs(t *testing.T) {
	target := issue("I_1", 42, "OPEN")
	pr := pull("PR_1", 5, "MERGED", "dev", "feature/42-thing")
	// Only repository and number are known until Build resolves them.
	pr.Links = []IssueLink{
		{Repository: "hoge/fuga", Number: 42, Kind: LinkRefs},
		{Repository: "hoge/fuga", Number: 999, Kind: LinkRefs}, // not in the graph
	}

	result := Build(Input{
		Issues:       []*Issue{target},
		PullRequests: []*PullRequest{pr},
		Repositories: map[string]*Repository{"R": repo("R", "hoge/fuga", "dev")},
	})

	if len(pr.Links) != 1 || pr.Links[0].IssueID != target.ID {
		t.Fatalf("links = %+v, want only the resolved #42 link", pr.Links)
	}
	if counts := edgeKinds(result); counts[EdgePRRefs] != 1 {
		t.Fatalf("edge kinds = %v, want one pr-refs", counts)
	}
}

func TestUnlinkedPullRequestHangsOffTheRepository(t *testing.T) {
	pr := pull("PR_1", 5, "OPEN", "dev", "chore/cleanup")

	result := Build(Input{
		PullRequests: []*PullRequest{pr},
		Repositories: map[string]*Repository{"R": repo("R", "hoge/fuga", "dev")},
	})

	if got := rankOf(t, result, PRNodeID("PR_1")); got != 0 {
		t.Fatalf("unlinked pull request rank = %d, want 0 (in the issues' column)", got)
	}
	if counts := edgeKinds(result); len(counts) != 0 {
		t.Fatalf("edge kinds = %v, want none: it connects to nothing", counts)
	}
}

func TestBlockedAndDuplicateDoNotChangeRank(t *testing.T) {
	blocker := issue("I_1", 1, "OPEN")
	blocked := issue("I_2", 2, "OPEN")
	blocked.BlockedByIDs = []string{blocker.ID}
	original := issue("I_3", 3, "OPEN")
	dup := issue("I_4", 4, "OPEN")
	dup.DuplicateOfID = original.ID

	result := Build(Input{
		Issues:       []*Issue{blocker, blocked, original, dup},
		Repositories: map[string]*Repository{"R": repo("R", "hoge/fuga", "dev")},
	})

	for _, id := range []string{"I_1", "I_2", "I_3", "I_4"} {
		if got := rankOf(t, result, IssueNodeID(id)); got != 0 {
			t.Fatalf("%s rank = %d, want 0: blocking and duplicate must not move nodes", id, got)
		}
	}
	counts := edgeKinds(result)
	if counts[EdgeBlocked] != 1 || counts[EdgeDuplicate] != 1 {
		t.Fatalf("edge kinds = %v, want one blocked and one duplicate", counts)
	}
}

func TestResultEncodesEmptyCollectionsAsArrays(t *testing.T) {
	encoded, err := json.Marshal(Build(Input{Repositories: map[string]*Repository{}}))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"nodes", "edges", "warnings"} {
		if string(decoded[key]) != "[]" {
			t.Fatalf("%s = %s, want [] so the frontend can iterate without a null check", key, decoded[key])
		}
	}
}

func TestRepositoryKey(t *testing.T) {
	owner, name, ok := RepositoryKey("hoge/fuga")
	if !ok || owner != "hoge" || name != "fuga" {
		t.Fatalf("RepositoryKey(hoge/fuga) = %q, %q, %v", owner, name, ok)
	}
	if _, _, ok := RepositoryKey("fuga"); ok {
		t.Fatal("RepositoryKey should reject a name without an owner")
	}
}

func TestRepositoriesAreSummarizedForAFoldedLane(t *testing.T) {
	now := time.Now().UTC()
	older := issue("I_1", 1, "OPEN")
	older.UpdatedAt = now.Add(-48 * time.Hour)
	newer := issue("I_2", 2, "CLOSED")
	newer.UpdatedAt = now.Add(-1 * time.Hour)
	pr := pull("PR_1", 9, "OPEN", "dev", "chore/thing")
	pr.UpdatedAt = now.Add(-30 * time.Minute)

	fuga := repo("R", "hoge/fuga", "dev")
	Build(Input{
		Issues:       []*Issue{older, newer},
		PullRequests: []*PullRequest{pr},
		Repositories: map[string]*Repository{"R": fuga},
	})

	if fuga.IssueCount != 2 || fuga.OpenIssueCount != 1 || fuga.PullRequestCount != 1 {
		t.Fatalf("summary = %d issues / %d open / %d PRs, want 2/1/1",
			fuga.IssueCount, fuga.OpenIssueCount, fuga.PullRequestCount)
	}
	if !fuga.UpdatedAt.Equal(pr.UpdatedAt) {
		t.Fatalf("updatedAt = %v, want the most recent item %v", fuga.UpdatedAt, pr.UpdatedAt)
	}
}

func TestSummaryIsRecomputedRatherThanAccumulated(t *testing.T) {
	fuga := repo("R", "hoge/fuga", "dev")
	fuga.IssueCount, fuga.OpenIssueCount, fuga.PullRequestCount = 99, 99, 99

	only := issue("I_1", 1, "OPEN")
	in := Input{Issues: []*Issue{only}, Repositories: map[string]*Repository{"R": fuga}}
	Build(in)
	Build(in) // a refresh reuses the same structs

	if fuga.IssueCount != 1 || fuga.OpenIssueCount != 1 || fuga.PullRequestCount != 0 {
		t.Fatalf("summary = %d/%d/%d, want it reset and recounted each build",
			fuga.IssueCount, fuga.OpenIssueCount, fuga.PullRequestCount)
	}
}

func TestRepositoryNodesComeBackMostRecentlyUpdatedFirst(t *testing.T) {
	now := time.Now().UTC()
	stale := repo("R_stale", "hoge/aaa-stale", "main")
	fresh := repo("R_fresh", "hoge/zzz-fresh", "main")

	staleIssue := &Issue{ID: "I_1", Number: 1, State: "OPEN", RepositoryID: "R_stale", Repository: "hoge/aaa-stale", UpdatedAt: now.Add(-72 * time.Hour)}
	freshIssue := &Issue{ID: "I_2", Number: 2, State: "OPEN", RepositoryID: "R_fresh", Repository: "hoge/zzz-fresh", UpdatedAt: now}

	result := Build(Input{
		Issues:       []*Issue{staleIssue, freshIssue},
		Repositories: map[string]*Repository{"R_stale": stale, "R_fresh": fresh},
	})

	var order []string
	for _, node := range result.Nodes {
		if node.Kind == KindRepository {
			order = append(order, node.Repo.NameWithOwner)
		}
	}
	// Alphabetically "aaa-stale" would win; recency has to override that.
	if len(order) != 2 || order[0] != "hoge/zzz-fresh" {
		t.Fatalf("repository order = %v, want the recently updated one first", order)
	}
}

// An issue reached by both a `refs` line and a bare mention should read as one
// reason, not two, once the weaker link has been dropped.
func TestReasonsDescribeOnlyTheSurvivingLink(t *testing.T) {
	target := issue("I_1", 534, "OPEN")
	pr := pull("PR_1", 619, "MERGED", "dev", "feat/534")
	pr.Links = []IssueLink{
		{IssueID: target.ID, Repository: target.Repository, Number: 534, Kind: LinkMentions},
		{IssueID: target.ID, Repository: target.Repository, Number: 534, Kind: LinkRefs},
	}

	Build(Input{
		Issues:       []*Issue{target},
		PullRequests: []*PullRequest{pr},
		Repositories: map[string]*Repository{"R": repo("R", "hoge/fuga", "dev")},
	})

	if len(pr.Reasons) != 1 || pr.Reasons[0] != "refs #534" {
		t.Fatalf("reasons = %v, want [refs #534]", pr.Reasons)
	}
}

// A pull request that implements nothing keeps whatever the collector said.
func TestReasonsAreLeftAloneForUnlinkedPullRequests(t *testing.T) {
	pr := pull("PR_1", 9, "OPEN", "dev", "chore/thing")
	pr.Reasons = []string{ReasonYours}

	Build(Input{
		PullRequests: []*PullRequest{pr},
		Repositories: map[string]*Repository{"R": repo("R", "hoge/fuga", "dev")},
	})

	if len(pr.Reasons) != 1 || pr.Reasons[0] != ReasonYours {
		t.Fatalf("reasons = %v, want [%s]", pr.Reasons, ReasonYours)
	}
}
