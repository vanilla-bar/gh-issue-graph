# Changelog

## v0.2.1

Reading a conversation in the drawer.

- **Each comment in its own frame.** Indentation was enough while the bodies
  were a paragraph long; a comment carrying its own headings, quotes and lists
  ran into the next one's header with no line to stop it.
- **The body is framed too**, with the author and when they opened it. Left
  bare it read as a loose paragraph above a list of framed replies.
- **A heading inside a comment is somebody's heading.** The `CONVERSATION`
  label's style was reaching into the comment bodies and dressing their `h3`
  as part of the panel.

## v0.2.0

Folding, reading and reviewing. The board could show you where the work was;
this release is about what you do once you have found it.

- **Fold a sub-issue tree.** The `4/6` progress chip becomes a button when
  children are drawn below it: click it to take that whole subtree away, pull
  requests included.
- **Tuck a card into a line.** An issue you have filed but are not working on
  keeps its place without keeping its space — 113px down to 30px, with its
  sub-issues and pull requests going under the line with it. The line says what
  it carried, as `3 sub · 3 PRs`.
- **The board remembers what you shut.** Lanes were already remembered; the
  folds inside them and the tucked cards are now too. `unfold all` opens the
  folds and leaves tucked cards tucked, because those are different decisions.
- **Review state on a pull request card.** How many of the people asked have
  approved, as `1/2`, with a face for each reviewer: a green ring approved, a
  red one asked for changes, a dim one has not answered. A re-review and your
  own unsubmitted draft each get a mark.
- **Read the body without leaving.** Click a card and a panel slides in from the
  right with the body in it, the last twenty comments under that, and the
  reviewers, labels and relations beside it. `⧉` copies the GitHub link. GitHub
  renders the Markdown, so this still ships no Markdown library.

## v0.1.0

First release.

Draws your GitHub issues as a graph, with the pull requests that implement them
hanging off the leaves. The sub-issue hierarchy is the trunk, because a GitHub
issue has at most one parent and that relation always forms a forest.

- **Issues, not branches.** The X axis is the sub-issue hierarchy. Pull requests
  sit one column to the right of the issue they implement, and pull requests
  stacked on other pull requests continue rightwards from there.
- **Three layers of issue-to-PR linking**, drawn from most to least certain:
  `closingIssuesReferences` (solid), a `refs #N` line in the body (dashed), and
  a bare `#N` in the title (dotted — a guess, and the only kind that can be
  wrong).
- **Repositories are frames, not nodes.** No line is drawn from a repository to
  its issues: it would be true of every node and would bury the lines that say
  something. Lanes fold away and are ordered by what moved last.
- **Every card says why it is on the canvas** — which search scope matched it,
  or which relation pulled it in.
- **`to review`** collects pull requests waiting on your review and reverse-looks
  up the issue each one closes, so a review request arrives attached to the work
  it belongs to.
- **Wrap-up warnings**: an issue whose sub-issues are all done but which never
  closed, or one with a merged pull request still sitting open.
- Cross-repository, or scoped to one with `-repo`.
- No third-party dependencies. `go.mod` has no `require` block and the frontend
  is plain HTML, CSS and JavaScript — no bundler, no framework, no CDN.
- Your token never leaves `gh`: every request shells out to `gh api graphql`.
