# Changelog

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
