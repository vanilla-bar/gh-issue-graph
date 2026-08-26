package graph

import (
	"sort"
	"strings"
	"time"
)

// Input is everything collected from GitHub before the graph is laid out.
type Input struct {
	Issues       []*Issue
	PullRequests []*PullRequest
	Repositories map[string]*Repository // keyed by repository node ID
	Viewer       string
	Warnings     []string
}

// Build turns the collected issues and pull requests into nodes and edges.
//
// The trunk is the sub-issue hierarchy: a GitHub issue has at most one parent,
// so the parent relation always forms a forest and can drive the X axis. Pull
// requests are placed one rank to the right of the issue they implement, and
// pull requests stacked on other pull requests continue rightwards from there.
// Blocking and duplicate relations are drawn over that tree without moving
// anything, because they form a DAG that would otherwise fight the layout.
func Build(in Input) Result {
	issues := indexIssues(in.Issues)
	prs := indexPRs(in.PullRequests)
	resolveLinks(prs, issues, in.Issues)

	describeLinks(in.PullRequests)

	linkedPRs := prsByIssue(prs)
	stackParents := prStackParents(in.PullRequests, in.Repositories)

	issueRanks := rankIssues(in.Issues, issues)
	prRanks := rankPRs(in.PullRequests, prs, issueRanks, stackParents)

	for _, issue := range in.Issues {
		issue.Relation = RelationFor(issue, in.Viewer)
	}
	annotate(in.Issues, issues, linkedPRs)
	for _, pr := range in.PullRequests {
		pr.Relation = RelationForPR(pr, in.Viewer)
	}

	summarizeRepositories(in)

	nodes := buildNodes(in, issueRanks, prRanks)
	edges := buildEdges(in, issues, prs, stackParents)

	// "Ready to pick up" only says something next to something that is not
	// ready. In a project that never records dependencies every open issue
	// would light up, so the badge is dropped unless a block exists somewhere.
	if !hasKind(edges, EdgeBlocked) {
		for _, issue := range in.Issues {
			issue.Actionable = false
		}
	}

	return Result{
		Nodes:     nodes,
		Edges:     edges,
		Warnings:  append([]string{}, in.Warnings...),
		Viewer:    in.Viewer,
		UpdatedAt: time.Now().UTC(),
	}
}

func hasKind(edges []Edge, kind string) bool {
	for _, edge := range edges {
		if edge.Kind == kind {
			return true
		}
	}
	return false
}

func indexIssues(list []*Issue) map[string]*Issue {
	byID := make(map[string]*Issue, len(list))
	for _, issue := range list {
		byID[issue.ID] = issue
	}
	return byID
}

func indexPRs(list []*PullRequest) map[string]*PullRequest {
	byID := make(map[string]*PullRequest, len(list))
	for _, pr := range list {
		byID[pr.ID] = pr
	}
	return byID
}

// linkStrength ranks how much a link is trusted: a closing reference beats a
// deliberate `refs`, which beats a passing mention.
var linkStrength = map[string]int{LinkCloses: 3, LinkRefs: 2, LinkXref: 1}

// resolveLinks fills in the issue node ID for links that were parsed out of a
// pull request body, where only "OWNER/NAME#123" was known, and keeps one link
// per issue: the strongest one.
func resolveLinks(prs map[string]*PullRequest, issues map[string]*Issue, list []*Issue) {
	byNumber := make(map[string]*Issue, len(list))
	for _, issue := range list {
		byNumber[issueKey(issue.Repository, issue.Number)] = issue
	}
	for _, pr := range prs {
		best := map[string]IssueLink{}
		order := []string{}
		for _, link := range pr.Links {
			if link.IssueID == "" {
				match := byNumber[issueKey(link.Repository, link.Number)]
				if match == nil {
					// The referenced issue is not part of this graph.
					continue
				}
				link.IssueID = match.ID
			}
			target, ok := issues[link.IssueID]
			if !ok {
				continue
			}
			link.Repository = target.Repository
			link.Number = target.Number
			if current, seen := best[link.IssueID]; seen {
				if linkStrength[current.Kind] >= linkStrength[link.Kind] {
					continue
				}
			} else {
				order = append(order, link.IssueID)
			}
			best[link.IssueID] = link
		}
		kept := pr.Links[:0]
		for _, id := range order {
			kept = append(kept, best[id])
		}
		pr.Links = kept
	}
}

// describeLinks turns the surviving links into the "why is this here?" line.
// Run after resolveLinks so an issue reached by both `refs` and a bare mention
// reads "refs #534" rather than "refs #534 · mentions #534".
func describeLinks(prs []*PullRequest) {
	for _, pr := range prs {
		if len(pr.Links) == 0 {
			continue
		}
		pr.Reasons = pr.Reasons[:0]
		for _, link := range pr.Links {
			pr.Reasons = append(pr.Reasons, link.Kind+" #"+itoa(link.Number))
		}
	}
}

func issueKey(repository string, number int) string {
	return repository + "#" + itoa(number)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// prsByIssue groups pull requests by the issue they implement.
func prsByIssue(prs map[string]*PullRequest) map[string][]*PullRequest {
	byIssue := map[string][]*PullRequest{}
	for _, pr := range prs {
		for _, link := range pr.Links {
			byIssue[link.IssueID] = append(byIssue[link.IssueID], pr)
		}
	}
	return byIssue
}

// prStackParents maps a pull request to the pull request it is stacked on.
// A PR is stacked when its base branch is another PR's head branch in the same
// repository, which is exactly the relation gh-pr-graph uses for its trunk.
//
// The default branch is excluded on both sides. A release pull request whose
// head is `dev` would otherwise adopt every pull request opened against `dev`,
// turning a repository with one release PR into a single very deep stack.
func prStackParents(list []*PullRequest, repos map[string]*Repository) map[string]*PullRequest {
	isDefault := func(repositoryID, ref string) bool {
		repo := repos[repositoryID]
		return repo != nil && repo.DefaultBranch != "" && repo.DefaultBranch == ref
	}

	byHead := map[string]*PullRequest{}
	for _, pr := range list {
		head := pr.HeadRepositoryID
		if head == "" {
			head = pr.RepositoryID
		}
		if isDefault(head, pr.HeadRefName) {
			continue
		}
		byHead[refKey(head, pr.HeadRefName)] = pr
	}
	parents := map[string]*PullRequest{}
	for _, pr := range list {
		if isDefault(pr.RepositoryID, pr.BaseRefName) {
			continue
		}
		parent := byHead[refKey(pr.RepositoryID, pr.BaseRefName)]
		if parent == nil || parent.ID == pr.ID {
			continue
		}
		parents[pr.ID] = parent
	}
	return parents
}

func refKey(repositoryID, ref string) string {
	return repositoryID + "\x00" + ref
}

// rankIssues puts issues without a parent in column 0 and relaxes children
// downstream, the same longest-path pass gh-pr-graph runs over its branch chain.
func rankIssues(list []*Issue, byID map[string]*Issue) map[string]int {
	ranks := make(map[string]int, len(list))
	for _, issue := range list {
		ranks[issue.ID] = 0
	}
	limit := len(list) + 1
	for pass := 0; pass < len(list); pass++ {
		changed := false
		for _, issue := range list {
			parent := byID[issue.ParentID]
			if parent == nil || parent.ID == issue.ID {
				continue
			}
			if next := ranks[parent.ID] + 1; next > ranks[issue.ID] && next <= limit {
				ranks[issue.ID] = next
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	return ranks
}

// rankPRs places a pull request to the right of every issue it implements and
// to the right of the pull request it is stacked on.
func rankPRs(list []*PullRequest, byID map[string]*PullRequest, issueRanks map[string]int, stack map[string]*PullRequest) map[string]int {
	ranks := make(map[string]int, len(list))
	for _, pr := range list {
		best := 0 // unlinked pull requests sit in the issues' column
		for _, link := range pr.Links {
			if next := issueRanks[link.IssueID] + 1; next > best {
				best = next
			}
		}
		ranks[pr.ID] = best
	}
	limit := len(list) + len(issueRanks) + 2
	for pass := 0; pass < len(list); pass++ {
		changed := false
		for _, pr := range list {
			parent := stack[pr.ID]
			if parent == nil {
				continue
			}
			if next := ranks[parent.ID] + 1; next > ranks[pr.ID] && next <= limit {
				ranks[pr.ID] = next
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	return ranks
}

// annotate computes the actionable flag and the attention badges.
func annotate(list []*Issue, byID map[string]*Issue, linked map[string][]*PullRequest) {
	for _, issue := range list {
		issue.Attention = nil
		issue.Actionable = false
		if issue.State != "OPEN" {
			continue
		}

		unfinishedChildren := issue.SubTotal > 0 && issue.SubCompleted < issue.SubTotal
		if issue.SubTotal > 0 && issue.SubCompleted == issue.SubTotal {
			// Every child is done but the parent never closed.
			issue.Attention = append(issue.Attention, AttentionChildrenDone)
		}
		for _, pr := range linked[issue.ID] {
			if pr.State == "MERGED" {
				issue.Attention = append(issue.Attention, AttentionMergedPROpen)
				break
			}
		}

		blocked := false
		for _, id := range issue.BlockedByIDs {
			blocker := byID[id]
			// An unknown blocker is treated as blocking: better to under-promise
			// than to call something actionable that is not.
			if blocker == nil || blocker.State == "OPEN" {
				blocked = true
				break
			}
		}

		// "Ready to pick up" means the next move is to do the work. An issue
		// that needs wrapping up, that duplicates another, or that belongs to
		// somebody else needs a different move, so it does not get the badge.
		issue.Actionable = !blocked &&
			!unfinishedChildren &&
			len(issue.Attention) == 0 &&
			issue.DuplicateOfID == "" &&
			issue.Relation != RelationOther
	}
}

// summarizeRepositories rolls the lane contents up onto the repository node:
// when a lane is folded away, this is all that is left to read.
func summarizeRepositories(in Input) {
	for _, repo := range in.Repositories {
		repo.IssueCount, repo.OpenIssueCount, repo.PullRequestCount = 0, 0, 0
		repo.UpdatedAt = time.Time{}
	}
	touch := func(repositoryID string, at time.Time) *Repository {
		repo := in.Repositories[repositoryID]
		if repo == nil {
			return nil
		}
		if at.After(repo.UpdatedAt) {
			repo.UpdatedAt = at
		}
		return repo
	}
	for _, issue := range in.Issues {
		repo := touch(issue.RepositoryID, issue.UpdatedAt)
		if repo == nil {
			continue
		}
		repo.IssueCount++
		if issue.State == "OPEN" {
			repo.OpenIssueCount++
		}
	}
	for _, pr := range in.PullRequests {
		if repo := touch(pr.RepositoryID, pr.UpdatedAt); repo != nil {
			repo.PullRequestCount++
		}
	}
}

func buildNodes(in Input, issueRanks, prRanks map[string]int) []Node {
	nodes := make([]Node, 0, len(in.Issues)+len(in.PullRequests)+len(in.Repositories))

	repos := make([]*Repository, 0, len(in.Repositories))
	for _, repo := range in.Repositories {
		repos = append(repos, repo)
	}
	// Most recently touched first; the name only breaks ties so the order is
	// stable when nothing has a timestamp.
	sort.Slice(repos, func(i, j int) bool {
		a, b := repos[i], repos[j]
		if !a.UpdatedAt.Equal(b.UpdatedAt) {
			return a.UpdatedAt.After(b.UpdatedAt)
		}
		return a.NameWithOwner < b.NameWithOwner
	})
	for _, repo := range repos {
		nodes = append(nodes, Node{ID: RepoNodeID(repo.ID), Kind: KindRepository, Rank: 0, Repo: repo})
	}

	issues := append([]*Issue(nil), in.Issues...)
	sort.Slice(issues, func(i, j int) bool {
		a, b := issues[i], issues[j]
		if ra, rb := issueRanks[a.ID], issueRanks[b.ID]; ra != rb {
			return ra < rb
		}
		if a.Repository != b.Repository {
			return a.Repository < b.Repository
		}
		return a.Number < b.Number
	})
	for _, issue := range issues {
		nodes = append(nodes, Node{ID: IssueNodeID(issue.ID), Kind: KindIssue, Rank: issueRanks[issue.ID], Issue: issue})
	}

	prs := append([]*PullRequest(nil), in.PullRequests...)
	sort.Slice(prs, func(i, j int) bool {
		a, b := prs[i], prs[j]
		if ra, rb := prRanks[a.ID], prRanks[b.ID]; ra != rb {
			return ra < rb
		}
		if a.Repository != b.Repository {
			return a.Repository < b.Repository
		}
		return a.Number < b.Number
	})
	for _, pr := range prs {
		nodes = append(nodes, Node{ID: PRNodeID(pr.ID), Kind: KindPullRequest, Rank: prRanks[pr.ID], PR: pr})
	}
	return nodes
}

func buildEdges(in Input, issues map[string]*Issue, prs map[string]*PullRequest, stack map[string]*PullRequest) []Edge {
	edges := make([]Edge, 0, len(in.Issues)+len(in.PullRequests))
	seen := map[string]bool{}
	add := func(kind, source, target, label string) {
		if source == target {
			return
		}
		id := edgeID(kind, source, target)
		if seen[id] {
			return
		}
		seen[id] = true
		edges = append(edges, Edge{ID: id, Source: source, Target: target, Kind: kind, Label: label})
	}

	for _, issue := range in.Issues {
		if parent := issues[issue.ParentID]; parent != nil {
			add(EdgeParent, IssueNodeID(parent.ID), IssueNodeID(issue.ID), "")
		}
	}

	// resolveLinks already kept one link per issue, the strongest one.
	for _, pr := range in.PullRequests {
		node := PRNodeID(pr.ID)
		if parent := stack[pr.ID]; parent != nil {
			add(EdgePRStack, PRNodeID(parent.ID), node, "")
		}
		for _, link := range pr.Links {
			add(prEdgeKind(link.Kind), IssueNodeID(link.IssueID), node, prEdgeLabel(link.Kind))
		}
	}

	for _, issue := range in.Issues {
		node := IssueNodeID(issue.ID)
		for _, id := range issue.BlockedByIDs {
			if blocker := issues[id]; blocker != nil {
				add(EdgeBlocked, IssueNodeID(blocker.ID), node, "blocks")
			}
		}
		if original := issues[issue.DuplicateOfID]; original != nil {
			add(EdgeDuplicate, node, IssueNodeID(original.ID), "duplicate of")
		}
	}
	return edges
}

func prEdgeKind(link string) string {
	switch link {
	case LinkCloses:
		return EdgePRCloses
	case LinkRefs:
		return EdgePRRefs
	default:
		return EdgePRXref
	}
}

func prEdgeLabel(link string) string {
	switch link {
	case LinkCloses:
		return "closes"
	case LinkRefs:
		return "refs"
	default:
		return ""
	}
}

// RepositoryKey splits OWNER/NAME. It returns false when the input is not a
// two-part path.
func RepositoryKey(nameWithOwner string) (owner, name string, ok bool) {
	owner, name, ok = strings.Cut(nameWithOwner, "/")
	if !ok || owner == "" || name == "" {
		return "", "", false
	}
	return owner, name, true
}
