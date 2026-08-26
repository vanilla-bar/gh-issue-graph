// Package graph holds the domain model shared by the GitHub client, the graph
// builder and the HTTP server.
//
// The overall shape of this project follows orangain/gh-pr-graph (MIT), which
// visualizes pull requests. This tool inverts the hierarchy: issues sit near the
// root and pull requests hang off them as leaves.
package graph

import "time"

// User is an actor rendered on a node.
type User struct {
	Login     string `json:"login"`
	AvatarURL string `json:"avatarUrl,omitempty"`
}

// Review states as GitHub reports them on the latest review by each person.
const (
	ReviewApproved         = "APPROVED"
	ReviewChangesRequested = "CHANGES_REQUESTED"
)

// Reviewer is one voice on a pull request: somebody who has answered, somebody
// being waited on, or both at once when a re-review has been asked for.
type Reviewer struct {
	Login     string `json:"login"`
	AvatarURL string `json:"avatarUrl,omitempty"`
	// State is APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED, or empty when
	// they have not answered yet.
	State string `json:"state,omitempty"`
	// Requested means GitHub is waiting on them right now.
	Requested bool `json:"requested,omitempty"`
	// IsTeam marks a request sent to a team rather than a person. A team has no
	// avatar, so the card draws its initial instead.
	IsTeam bool `json:"isTeam,omitempty"`
}

// Detail is what a card cannot hold: the body of an issue or a pull request,
// fetched only when somebody asks to read it.
//
// BodyHTML is GitHub's own rendering, which is why this module needs no Markdown
// library. It is dropped into the page as HTML, so the server's CSP is what
// stands between it and anything unwanted: script-src 'self' means nothing in it
// can execute.
type Detail struct {
	ID       string `json:"id"`
	BodyHTML string `json:"bodyHtml"`
	// CreatedAt is when it was opened. The board carries updatedAt, which is a
	// different question: the drawer's first card is what somebody wrote, and
	// it is dated by when they wrote it.
	CreatedAt time.Time `json:"createdAt"`
	// Comments is the tail of the conversation, oldest first. Reviews that
	// carry no words of their own are left out: an approval with nothing said
	// is already on the card as part of the count.
	Comments []Comment `json:"comments,omitempty"`
	// CommentTotal counts everything there is, before the tail was taken. A
	// list cut short without saying so reads as the whole conversation.
	CommentTotal int `json:"commentTotal,omitempty"`
}

// Comment is one thing somebody said, either in the conversation or as the
// body of a review.
type Comment struct {
	Author    User      `json:"author"`
	BodyHTML  string    `json:"bodyHtml"`
	CreatedAt time.Time `json:"createdAt"`
	URL       string    `json:"url,omitempty"`
	// ReviewState is APPROVED / CHANGES_REQUESTED / COMMENTED for a review, and
	// empty for an ordinary comment.
	ReviewState string `json:"reviewState,omitempty"`
}

// Label is a GitHub issue label.
type Label struct {
	Name  string `json:"name"`
	Color string `json:"color,omitempty"`
}

// IssueType is the GitHub issue type (Bug / Feature / Task ...).
type IssueType struct {
	Name  string `json:"name"`
	Color string `json:"color,omitempty"`
}

// SearchOptions describes which issues to collect and how much to link.
type SearchOptions struct {
	// Repo restricts the graph to a single OWNER/NAME repository. When empty
	// the graph spans every repository the viewer is involved in.
	Repo string
	// Query is a raw GitHub search query appended to the built-in scopes.
	Query string
	// ReviewRequested collects open pull requests waiting on the viewer's
	// review, and reverse-looks-up the issues they reference so each one hangs
	// under real work rather than floating on its own.
	ReviewRequested bool

	// Assigned, Authored and Mentioned select the built-in search scopes.
	Assigned  bool
	Authored  bool
	Mentioned bool
	// IncludeClosed keeps closed issues found by the search itself. Issues
	// pulled in as complements are kept regardless, because a closed child is
	// what makes a completed parent readable.
	IncludeClosed bool
	// IncludeXref turns on the noisy cross-reference edges.
	IncludeXref bool
}

// Link kinds recorded on a pull request, ordered by how much we trust them.
const (
	// LinkCloses comes from closingIssuesReferences: merging the PR closes the
	// issue. Drawn as a solid line.
	LinkCloses = "closes"
	// LinkRefs comes from a `refs #N` line in the PR body: deliberately related
	// without closing. Drawn as a dashed line.
	LinkRefs = "refs"
	// LinkMentions comes from an issue number in the title or body without any
	// keyword — "feat(parse): #603 ..." — which is how a lot of pull requests
	// actually name their issue.
	LinkMentions = "mentions"
	// LinkXref comes from a timeline cross-reference. Noisy, off by default.
	LinkXref = "xref"
)

// IssueLink is one issue a pull request points at.
type IssueLink struct {
	// IssueID is the GraphQL node ID when known. Body-parsed refs only carry a
	// repository and a number until they are resolved.
	IssueID    string `json:"issueId,omitempty"`
	Repository string `json:"repository"`
	Number     int    `json:"number"`
	Kind       string `json:"kind"`
}

// PullRequest is a leaf node hanging off an issue.
type PullRequest struct {
	ID               string    `json:"id"`
	Number           int       `json:"number"`
	Title            string    `json:"title"`
	URL              string    `json:"url"`
	State            string    `json:"state"` // OPEN / MERGED / CLOSED
	IsDraft          bool      `json:"isDraft,omitempty"`
	UpdatedAt        time.Time `json:"updatedAt"`
	Author           User      `json:"author"`
	IsBot            bool      `json:"isBot,omitempty"`
	RepositoryID     string    `json:"repositoryId"`
	Repository       string    `json:"repository"`
	BaseRefName      string    `json:"baseRefName"`
	HeadRefName      string    `json:"headRefName"`
	HeadRepositoryID string    `json:"headRepositoryId,omitempty"`
	ReviewDecision   string    `json:"reviewDecision,omitempty"`
	// ReviewRequested means GitHub is waiting on *you* to review it. Kept as a
	// flag rather than a reason string because graph.describeLinks overwrites
	// Reasons with the link descriptions once a pull request has any links.
	ReviewRequested bool `json:"reviewRequested,omitempty"`
	// Reviewers is everybody who has answered or is being waited on, in the
	// order they should be drawn: those still owed a review first.
	Reviewers []Reviewer `json:"reviewers,omitempty"`
	// ReviewApproved and ReviewTotal are the "1 of 2" on the card. Total counts
	// people, not reviews: somebody who approved twice is still one voice.
	//
	// Approved is sent even when it is zero. Dropping it left the card reading
	// "undefined/2" for the case that matters most: nobody has approved yet.
	ReviewApproved int `json:"reviewApproved"`
	ReviewTotal    int `json:"reviewTotal,omitempty"`
	// ReReviewRequested means somebody who already answered has been asked
	// again — usually because the author pushed after a review.
	ReReviewRequested bool `json:"reReviewRequested,omitempty"`
	// ViewerPendingReview means you have a review started and not submitted.
	// Nobody else can see it, so nothing moves until you press the button.
	ViewerPendingReview bool        `json:"viewerPendingReview,omitempty"`
	CIState             string      `json:"ciState,omitempty"`
	Links               []IssueLink `json:"links,omitempty"`
	Relation            string      `json:"relation"`
	// Reasons explains why this pull request is on the canvas.
	Reasons []string `json:"reasons,omitempty"`
}

// Reasons a node is on the canvas at all. Shown on the card so it is always
// possible to answer "why am I looking at this?".
const (
	ReasonAssigned   = "assigned to you"
	ReasonAuthored   = "you opened it"
	ReasonMentioned  = "mentions you"
	ReasonQuery      = "matched your query"
	ReasonRepository = "in this repository"
	ReasonYours      = "your pull request"
	// ReasonReviewRequested is only ever the whole story for a pull request
	// that references no issue at all; the rest get their reason from the link.
	ReasonReviewRequested = "review requested from you"
)

// ReasonReviewing explains an issue that was pulled in only because a pull
// request awaiting your review points at it.
func ReasonReviewing(prNumber int) string {
	return "reviewing #" + itoa(prNumber)
}

// Attention reasons surfaced as a warning badge on an issue node.
const (
	// AttentionChildrenDone means every sub-issue is closed but the parent is
	// still open: the wrap-up work is outstanding.
	AttentionChildrenDone = "children-done"
	// AttentionMergedPROpen means a merged pull request is linked but the issue
	// never closed.
	AttentionMergedPROpen = "merged-pr-open"
)

// Issue is the trunk of the graph.
type Issue struct {
	ID            string     `json:"id"`
	Number        int        `json:"number"`
	Title         string     `json:"title"`
	URL           string     `json:"url"`
	State         string     `json:"state"` // OPEN / CLOSED
	StateReason   string     `json:"stateReason,omitempty"`
	UpdatedAt     time.Time  `json:"updatedAt"`
	Type          *IssueType `json:"issueType,omitempty"`
	RepositoryID  string     `json:"repositoryId"`
	Repository    string     `json:"repository"`
	RepositoryURL string     `json:"repositoryUrl"`
	DefaultBranch string     `json:"defaultBranch,omitempty"`
	Author        User       `json:"author"`
	Assignees     []User     `json:"assignees,omitempty"`
	Labels        []Label    `json:"labels,omitempty"`
	Milestone     string     `json:"milestone,omitempty"`

	ParentID      string   `json:"parentId,omitempty"`
	SubIssueIDs   []string `json:"subIssueIds,omitempty"`
	SubTotal      int      `json:"subTotal"`
	SubCompleted  int      `json:"subCompleted"`
	BlockedByIDs  []string `json:"blockedByIds,omitempty"`
	BlockingIDs   []string `json:"blockingIds,omitempty"`
	DuplicateOfID string   `json:"duplicateOfId,omitempty"`

	// Relation is the viewer's relationship to the issue: assigned, mine,
	// or other. Source is "search" or "complement".
	Relation string `json:"relation"`
	Source   string `json:"source"`
	// Actionable is true when the issue is open, nothing blocks it and it has
	// no unfinished children: it can be picked up right now.
	Actionable bool     `json:"actionable,omitempty"`
	Attention  []string `json:"attention,omitempty"`
	// Reasons explains why this issue is on the canvas: which search scope
	// matched it, or which relation dragged it in.
	Reasons []string `json:"reasons,omitempty"`
}

// Repository is the root node of a lane. It stands for the default branch.
type Repository struct {
	ID            string `json:"id"`
	NameWithOwner string `json:"nameWithOwner"`
	URL           string `json:"url"`
	DefaultBranch string `json:"defaultBranch,omitempty"`

	// UpdatedAt is the most recent update across everything in the lane, so
	// lanes can be ordered by what moved last rather than alphabetically.
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
	// Counts let a folded lane still say how much it is hiding.
	IssueCount       int `json:"issueCount"`
	OpenIssueCount   int `json:"openIssueCount"`
	PullRequestCount int `json:"pullRequestCount"`
}

// Node kinds.
const (
	KindRepository  = "repository"
	KindIssue       = "issue"
	KindPullRequest = "pullRequest"
)

// Node is one box on the canvas.
type Node struct {
	ID    string       `json:"id"`
	Kind  string       `json:"kind"`
	Rank  int          `json:"rank"`
	Repo  *Repository  `json:"repository,omitempty"`
	Issue *Issue       `json:"issue,omitempty"`
	PR    *PullRequest `json:"pullRequest,omitempty"`
}

// Edge kinds. A single bool like gh-pr-graph's Dashed cannot express these,
// so the kind travels to the frontend and the stylesheet decides.
//
// There is deliberately no repository edge: every node in a lane belongs to
// that repository, so drawing a line from the repository to each of them says
// nothing and buries the lines that do. The lane's frame carries that instead.
const (
	EdgeParent    = "parent"    // sub-issue hierarchy, the trunk
	EdgePRCloses  = "pr-closes" // issue -> PR, closingIssuesReferences
	EdgePRRefs    = "pr-refs"   // issue -> PR, `refs #N` in the body
	EdgePRXref    = "pr-xref"   // issue -> PR, timeline cross-reference
	EdgePRStack   = "pr-stack"  // PR -> PR, base/head chain
	EdgeBlocked   = "blocked"   // blocker -> blocked, drawn over the tree
	EdgeDuplicate = "duplicate" // duplicate -> original
)

// Edge connects two nodes.
type Edge struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Target string `json:"target"`
	Kind   string `json:"kind"`
	Label  string `json:"label,omitempty"`
}

// Result is the payload the frontend renders.
type Result struct {
	Nodes     []Node    `json:"nodes"`
	Edges     []Edge    `json:"edges"`
	Warnings  []string  `json:"warnings"`
	Viewer    string    `json:"viewer,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Relations, most specific first.
const (
	RelationAssigned = "assigned"
	RelationMine     = "mine"
	RelationOther    = "other"
)

// RelationFor reports how the viewer relates to an issue. Being assigned wins
// over having authored it, because the assignee is the one who has to act.
func RelationFor(issue *Issue, viewer string) string {
	if viewer == "" {
		return RelationOther
	}
	for _, assignee := range issue.Assignees {
		if assignee.Login == viewer {
			return RelationAssigned
		}
	}
	if issue.Author.Login == viewer {
		return RelationMine
	}
	return RelationOther
}

// RelationForPR mirrors RelationFor for pull request nodes.
func RelationForPR(pr *PullRequest, viewer string) string {
	if viewer != "" && pr.Author.Login == viewer {
		return RelationMine
	}
	return RelationOther
}

// NodeID builders keep IDs stable and collision free across kinds.
func RepoNodeID(repositoryID string) string { return "repo:" + repositoryID }
func IssueNodeID(issueID string) string     { return "issue:" + issueID }
func PRNodeID(pullRequestID string) string  { return "pr:" + pullRequestID }
func edgeID(kind, source, target string) string {
	return kind + ":" + source + ">" + target
}
