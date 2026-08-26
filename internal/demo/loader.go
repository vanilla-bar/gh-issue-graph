// Package demo serves a fixed graph so the UI can be developed, screenshotted
// and regression tested without touching GitHub.
//
// It is deliberately not exposed as a CLI flag: GH_ISSUE_GRAPH_DEMO=1 turns it
// on. The dataset covers every visual state the frontend has to handle.
package demo

import (
	"context"
	"time"

	"github.com/vanilla-bar/gh-issue-graph/internal/github"
	"github.com/vanilla-bar/gh-issue-graph/internal/graph"
)

// Loader returns the canned dataset.
type Loader struct {
	// started pins the clock the relative timestamps are derived from, so the
	// same data really is the same bytes on every request.
	started time.Time
}

// New returns a demo loader.
func New() *Loader { return &Loader{started: time.Now().UTC()} }

const viewer = "octocat"

// One picture for everybody in the fixture. It is the identicon GitHub serves
// for its own mascot, so the demo needs no bundled image and no network call
// that could reveal who is running it.
const avatar = "https://avatars.githubusercontent.com/u/583231?v=4"

func user(login string) graph.User {
	return graph.User{Login: login, AvatarURL: avatar}
}

// Detail implements server.Detailer. The bodies are canned like everything else
// here, and cover what the drawer has to lay out: headings, a list, a table,
// code and a quote.
func (l *Loader) Detail(ctx context.Context, id string) (graph.Detail, error) {
	body, ok := demoBodies[id]
	if !ok {
		body = "<p><em>No description provided.</em></p>"
	}
	said := demoComments(l.started, id)
	return graph.Detail{ID: id, BodyHTML: body, Comments: said, CommentTotal: demoTotals(id, len(said))}, nil
}

// demoComments covers what the panel has to lay out: an ordinary comment, a
// review that asked for changes, and a reply to it.
func demoComments(now time.Time, id string) []graph.Comment {
	ago := func(h int) time.Time { return now.Add(-time.Duration(h) * time.Hour) }
	switch id {
	case "PR_210":
		return []graph.Comment{
			{Author: user("mona"), CreatedAt: ago(20), ReviewState: graph.ReviewChangesRequested,
				BodyHTML: `<p>The parser reads the whole file into memory. For the sizes we expect that is
fine, but it is worth a note in the code so nobody is surprised later.</p>`},
			{Author: user(viewer), CreatedAt: ago(19),
				BodyHTML: `<p>Added the note, and a guard at 8 MB that falls back to streaming.</p>`},
			{Author: user("mona"), CreatedAt: ago(18), ReviewState: graph.ReviewApproved,
				BodyHTML: `<p>Reads well now. Thanks.</p>`},
		}
	case "PR_220":
		return []graph.Comment{
			{Author: user("mona"), CreatedAt: ago(6), ReviewState: graph.ReviewChangesRequested,
				BodyHTML: `<p>The checklist still tells people to run the old script. That step should go.</p>`},
		}
	case "I_100":
		return []graph.Comment{
			{Author: user("hubot"), CreatedAt: ago(30),
				BodyHTML: `<p>Does this change the export format? Anything reading the old shape will need a version bump.</p>`},
			{Author: user(viewer), CreatedAt: ago(29),
				BodyHTML: `<p>It does. The exporter writes <code>schema: 2</code> and the importer reads both.</p>`},
		}
	}
	return nil
}

// A fixed extra so the "and N more" line has something to say in the demo.
func demoTotals(id string, kept int) int {
	if id == "PR_210" {
		return kept + 4
	}
	return kept
}

// The fixture's bodies, as GitHub would render them: plain HTML with no style
// attributes, which is exactly what the CSP allows through.
var demoBodies = map[string]string{
	"I_100": `<h2>Why</h2>
<p>Steps currently belong to the account rather than to a recipe, so two recipes
cannot have a step with the same name. Moving them under the recipe fixes that
and lets a recipe be copied whole.</p>
<h2>Shape</h2>
<table><thead><tr><th>Before</th><th>After</th></tr></thead>
<tbody><tr><td><code>account/steps</code></td><td><code>recipe/steps</code></td></tr>
<tr><td>flat</td><td>nested</td></tr></tbody></table>
<h2>Order</h2>
<ol><li>Write the decision down (#101)</li>
<li>Move the editor over (#102)</li>
<li>Warn before a delete takes notes with it (#103)</li></ol>
<blockquote><p>Migration runs on first launch and is not reversible.</p></blockquote>`,
	"I_120": `<p>Import a recipe from a Markdown file so it can be kept in a repository.</p>
<pre><code>gh recipe import ./dinner.md
</code></pre>
<ul><li>Headings become steps</li><li>A list under a heading becomes its ingredients</li>
<li>Anything else is kept as a note</li></ul>`,
	"PR_210": `<p>Closes #120.</p>
<p>Parses the file into the intermediate representation from PR #211, then writes
it through the normal recipe path so nothing new touches the database.</p>`,
	"PR_230": `<p>Closes #130.</p>
<p>The 1x set was picked whichever screen asked, so a high-DPI display scaled it
up and blurred it. This reads <code>devicePixelRatio</code> and asks for the 2x
set above 1.5.</p>`,
}

// Load implements server.Loader.
func (l *Loader) Load(ctx context.Context, options graph.SearchOptions, progress github.Progress) (graph.Input, error) {
	if progress != nil {
		progress(github.PhaseSearch, 1, 1, 0)
		progress(github.PhaseExpand, 1, 1, 6)
		progress(github.PhasePulls, 1, 1, 9)
	}
	// Pinned at start-up rather than recomputed per request. Two loads a second
	// apart used to differ in the microseconds of every repository's updatedAt,
	// which made the payload unstable and hid the frontend's skip-unchanged
	// path from the test harness — the one place it could be exercised.
	now := l.started
	ago := func(h int) time.Time { return now.Add(-time.Duration(h) * time.Hour) }

	fuga := &graph.Repository{ID: "R_fuga", NameWithOwner: "hoge/fuga", URL: "https://github.com/hoge/fuga", DefaultBranch: "dev"}
	piyo := &graph.Repository{ID: "R_piyo", NameWithOwner: "hoge/piyo", URL: "https://github.com/hoge/piyo", DefaultBranch: "main"}

	issue := func(id string, number int, title, state string, repo *graph.Repository) *graph.Issue {
		return &graph.Issue{
			ID: id, Number: number, Title: title, State: state,
			URL:           repo.URL + "/issues/" + itoa(number),
			RepositoryID:  repo.ID,
			Repository:    repo.NameWithOwner,
			RepositoryURL: repo.URL,
			DefaultBranch: repo.DefaultBranch,
			UpdatedAt:     ago(number % 48),
			Author:        user(viewer),
			Source:        "search",
		}
	}

	// A parent whose children are all done but which never closed: the wrap-up
	// badge is the whole point of this tool.
	parent := issue("I_100", 100, "Rework the recipe data model so steps belong to a recipe", "OPEN", fuga)
	parent.Assignees = []graph.User{user(viewer)}
	parent.Labels = []graph.Label{{Name: "feature", Color: "a2eeef"}}
	parent.Type = &graph.IssueType{Name: "Feature", Color: "BLUE"}
	parent.SubTotal, parent.SubCompleted = 3, 3
	parent.SubIssueIDs = []string{"I_101", "I_102", "I_103"}

	child1 := issue("I_101", 101, "Record the data model decision in an ADR", "CLOSED", fuga)
	child2 := issue("I_102", 102, "Add a step by tapping the timeline", "CLOSED", fuga)
	child3 := issue("I_103", 103, "Warn about notes lost when a recipe is deleted", "CLOSED", fuga)
	for _, child := range []*graph.Issue{child1, child2, child3} {
		child.ParentID = parent.ID
		child.Source = "complement"
		child.Assignees = []graph.User{user(viewer)}
	}

	// Blocked work: the blocker itself is somebody else's issue, pulled in as a
	// complement so the dependency has something to point at.
	blocked := issue("I_110", 110, "Show cooking timers on the recipe card", "OPEN", fuga)
	blocked.Assignees = []graph.User{user(viewer)}
	blocked.BlockedByIDs = []string{"I_111"}
	blocked.Labels = []graph.Label{{Name: "ux", Color: "d4c5f9"}}

	blocker := issue("I_111", 111, "Decide the timer colour scale", "OPEN", fuga)
	blocker.Author = user("hubot")
	blocker.Assignees = []graph.User{user("hubot")}
	blocker.Source = "complement"
	blocker.BlockingIDs = []string{"I_110"}

	// Ready to pick up: open, unblocked, no unfinished children.
	ready := issue("I_120", 120, "Import recipes from Markdown files", "OPEN", fuga)
	ready.Assignees = []graph.User{user(viewer)}
	ready.Labels = []graph.Label{{Name: "enhancement", Color: "a2eeef"}}
	ready.Type = &graph.IssueType{Name: "Task", Color: "GREEN"}

	// A merged pull request that deliberately left the issue open.
	lingering := issue("I_300", 300, "Reminder never clears after a snooze", "OPEN", piyo)
	lingering.Labels = []graph.Label{{Name: "bug", Color: "d73a4a"}}
	lingering.Type = &graph.IssueType{Name: "Bug", Color: "RED"}
	lingering.Milestone = "2026 Q3"

	duplicate := issue("I_301", 301, "Reminders keep firing after being dismissed", "OPEN", piyo)
	duplicate.Author = user("hubot")
	duplicate.DuplicateOfID = lingering.ID
	duplicate.Source = "complement"

	// Every card carries its reason, the same as live data does.
	parent.Reasons = []string{graph.ReasonAssigned, graph.ReasonAuthored}
	for _, child := range []*graph.Issue{child1, child2, child3} {
		child.Reasons = []string{"sub-issue of #100"}
	}
	blocked.Reasons = []string{graph.ReasonAssigned}
	blocker.Reasons = []string{"blocks #110"}
	ready.Reasons = []string{graph.ReasonAssigned}
	lingering.Reasons = []string{graph.ReasonAuthored}
	duplicate.Reasons = []string{"duplicated by #300"}

	// Somebody else's issue, in nobody's search scope, pulled in only because a
	// pull request waiting on your review points at it. This is what the reverse
	// lookup produces: without it PR #230 below would float in column zero with
	// nothing to say what it is for.
	reviewed := issue("I_130", 130, "Thumbnails blur on high-DPI screens", "OPEN", fuga)
	reviewed.Source = "complement"
	reviewed.Author = user("mona")
	reviewed.Labels = []graph.Label{{Name: "bug", Color: "d73a4a"}}
	reviewed.Reasons = []string{graph.ReasonReviewing(230)}

	issues := []*graph.Issue{parent, child1, child2, child3, blocked, blocker, ready, lingering, duplicate, reviewed}

	pr := func(id string, number int, title, state string, repo *graph.Repository, base, head string) *graph.PullRequest {
		return &graph.PullRequest{
			ID: id, Number: number, Title: title, State: state,
			URL:          repo.URL + "/pull/" + itoa(number),
			RepositoryID: repo.ID, Repository: repo.NameWithOwner,
			BaseRefName: base, HeadRefName: head,
			HeadRepositoryID: repo.ID,
			UpdatedAt:        ago(number % 24),
			Author:           user(viewer),
		}
	}
	link := func(issue *graph.Issue, kind string) graph.IssueLink {
		return graph.IssueLink{IssueID: issue.ID, Repository: issue.Repository, Number: issue.Number, Kind: kind}
	}

	pr200 := pr("PR_200", 200, "docs: record the data model ADR", "MERGED", fuga, "dev", "docs/101-datamodel-adr")
	pr200.Links = []graph.IssueLink{link(child1, graph.LinkCloses)}
	pr200.CIState = "SUCCESS"

	pr201 := pr("PR_201", 201, "feat: add a step by tapping the timeline", "MERGED", fuga, "dev", "feature/102-tap-to-add-step")
	pr201.Links = []graph.IssueLink{link(child2, graph.LinkRefs)}
	pr201.CIState = "SUCCESS"

	pr202 := pr("PR_202", 202, "feat: cascade recipe deletion with a warning", "MERGED", fuga, "dev", "feature/103-recipe-cascade-delete")
	pr202.Links = []graph.IssueLink{link(child3, graph.LinkRefs)}
	pr202.CIState = "SUCCESS"

	// One issue, two pull requests, and the second is stacked on the first.
	pr210 := pr("PR_210", 210, "feat: Markdown recipe importer", "OPEN", fuga, "dev", "feat/issue-120-md-import")
	pr210.Links = []graph.IssueLink{link(ready, graph.LinkCloses)}
	pr210.CIState = "SUCCESS"
	pr210.ReviewDecision = "APPROVED"
	// Approved by one of two: the second reviewer has not answered yet.
	pr210.Reviewers = []graph.Reviewer{
		{Login: "hubot", AvatarURL: avatar, Requested: true},
		{Login: "mona", AvatarURL: avatar, State: graph.ReviewApproved},
	}
	pr210.ReviewApproved, pr210.ReviewTotal = 1, 2

	pr211 := pr("PR_211", 211, "feat: Markdown import to intermediate representation", "OPEN", fuga, "feat/issue-120-md-import", "feat/issue-120-md-ir")
	pr211.Links = []graph.IssueLink{link(ready, graph.LinkRefs)}
	pr211.CIState = "PENDING"
	pr211.IsDraft = true
	// A team was asked, and a team has no avatar to draw.
	pr211.Reviewers = []graph.Reviewer{{Login: "recipe-app", Requested: true, IsTeam: true}}
	pr211.ReviewTotal = 1

	// Unlinked but open: work that never got an issue.
	pr220 := pr("PR_220", 220, "chore: refresh the review checklist", "OPEN", fuga, "dev", "chore/review-checklist")
	pr220.CIState = "FAILURE"
	// Changes were requested and the author pushed since, so the same reviewer
	// is being waited on again.
	pr220.ReviewDecision = "CHANGES_REQUESTED"
	pr220.Reviewers = []graph.Reviewer{
		{Login: "mona", AvatarURL: avatar, State: graph.ReviewChangesRequested, Requested: true},
	}
	pr220.ReviewTotal = 1
	pr220.ReReviewRequested = true

	// Waiting on your review. Authored by somebody else, so no issue search of
	// yours would ever return it.
	pr230 := pr("PR_230", 230, "fix: pick the 2x thumbnail set on high-DPI screens", "OPEN", fuga, "dev", "fix/130-hidpi-thumbnails")
	pr230.Author = user("mona")
	pr230.Links = []graph.IssueLink{link(reviewed, graph.LinkCloses)}
	pr230.CIState = "SUCCESS"
	pr230.ReviewDecision = "REVIEW_REQUIRED"
	pr230.ReviewRequested = true
	// You have a review open and unsubmitted: nobody else can see it, which is
	// why the card says so.
	pr230.Reviewers = []graph.Reviewer{
		{Login: viewer, AvatarURL: avatar, Requested: true},
		{Login: "hubot", AvatarURL: avatar, State: "COMMENTED"},
	}
	pr230.ReviewTotal = 2
	pr230.ViewerPendingReview = true

	pr400 := pr("PR_400", 400, "fix: clear the reminder after a snooze", "MERGED", piyo, "main", "fix/300-snooze")
	pr400.Links = []graph.IssueLink{link(lingering, graph.LinkRefs)}
	pr400.CIState = "SUCCESS"

	// Linked pull requests get their reason from graph.Build; only the unlinked
	// one has to say for itself why it is on the canvas.
	pr220.Reasons = []string{graph.ReasonYours}

	prs := []*graph.PullRequest{pr200, pr201, pr202, pr210, pr211, pr220, pr230, pr400}
	if !options.ReviewRequested {
		prs = prs[:0]
		for _, candidate := range []*graph.PullRequest{pr200, pr201, pr202, pr210, pr211, pr220, pr400} {
			prs = append(prs, candidate)
		}
		issues = issues[:len(issues)-1]
	}

	return graph.Input{
		Issues:       issues,
		PullRequests: prs,
		Repositories: map[string]*graph.Repository{fuga.ID: fuga, piyo.ID: piyo},
		Viewer:       viewer,
		Warnings:     []string{"Demo mode: showing canned data, GitHub was not contacted."},
	}, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
