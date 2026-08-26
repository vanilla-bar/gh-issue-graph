# gh-issue-graph

[日本語](README.md) | **English**

A GitHub CLI extension that draws your issues as a graph. Parent/child issue
relations form the trunk; the pull requests that implement them hang off the
leaves.

![gh-issue-graph](docs/images/screenshot.png)

<details>
<summary>Dark mode</summary>

![gh-issue-graph in dark mode](docs/images/screenshot-dark.png)

</details>

Each repository is drawn as a frame around its graph.

## Alongside gh-pr-graph

The idea comes from [`gh-pr-graph`][pr-graph]. The difference is which axis
each one draws.

`gh-pr-graph` draws the branch axis: pull requests and the branches they are
stacked on. If you work in stacked branches, that is the tool you want.
`gh-issue-graph` does not model branch topology; it draws a stacked pull
request only when it hangs off an issue already on the canvas.

`gh-issue-graph` draws the issue axis. I mostly track my work by issue rather
than by pull request, and I wanted to see the issues a pull request belongs
to, not just the pull request itself. The shape is one parent issue with
several sub-issues, each implemented by its own pull request.

So this tool puts the issue hierarchy on the X axis and hangs pull requests
off it.

If you also organise your work around issues, I hope you will give it a try.

## Install

```console
gh extension install vanilla-bar/gh-issue-graph
```

## Use

```console
gh issue-graph                                  # every repository you are involved in
gh issue-graph -repo owner/name                 # one repository
gh issue-graph -port 9000 -no-open              # pick a port, stay in the terminal
```

It starts a local server on `127.0.0.1:8788` and opens your browser. Every
request is made by shelling out to `gh api graphql`, so your token never leaves
`gh`.

| Flag | Default | What it does |
|---|---|---|
| `-repo` | _(empty)_ | Limit to `OWNER/NAME`. Otherwise the graph spans every repository you touch. |
| `-port` | `8788` | Local port. `0` picks a free one; if `8788` is taken it falls back automatically. |
| `-no-open` | `false` | Do not launch a browser. |
| `-hostname` | _(gh config)_ | GitHub Enterprise hostname. |
| `-refs-pattern` | see below | Regexp for non-closing references in a pull request body. |

The browser controls mirror these, plus `to review` (pull requests waiting on
you — on by default), `closed` (keep closed issues the search itself found) and
`xrefs` (draw loose cross references — noisy, off by default).

### Scopes

`assigned`, `authored` and `mentioned` each run their own search and the results
are merged. A repository filter narrows them rather than replacing them:
`-repo hoge/fuga` with only `assigned` ticked means issues in that repository
assigned to you. Untick all three, with a repository named, to see the whole
repository.

`to review` searches pull requests, not issues
(`is:pr is:open review-requested:@me`). Those are other people's pull requests,
so no issue search would return them. It is always open-only, whatever `closed`
says.

Each of those pull requests gets a reverse lookup: the issue it closes, or names
in a `refs #N` line, is fetched and becomes its parent, so a review request
shows up under the work it belongs to instead of floating in the first column.
Those issues are marked `reviewing #356`. A bare `#N` in a pull request title is
not followed.

Issues you did not match can still appear: a parent, a child, a blocker or the
subject of a review request gets pulled in so the tree is not cut in half. Those
are drawn with a dashed border and say `for context`.

### Repository lanes

Each repository is a lane. Lanes are ordered by most recent activity by
default; the `sort` control switches to name or open-issue count. Every lane
starts folded. Which repositories you opened is remembered in `localStorage`,
so you land back where you were after a reload.

## What the picture means

### Nodes

| | Meaning |
|---|---|
| Solid blue left edge | Assigned to you |
| Pale blue left edge | You opened it |
| No left edge | Somebody else's |
| Dashed border, `for context` | Pulled in only to complete a relation; nobody asked for it |
| ⊙ | Open |
| ▶ green, `ready` | **Ready to pick up**: open, unblocked, no unfinished children. Only shown when something else in the graph *is* blocked, otherwise the badge says nothing. |
| ✓ purple, faded card | Closed |
| ⚠ `wrap up` | **Needs wrapping up**: every sub-issue is done but the issue is still open, or a pull request was merged and it never closed. |
| ⊘ `1 blocker` | Waiting on another issue. |
| `4/6` and a meter | Sub-issue progress. |
| Coloured dot on a label | The label's own GitHub colour. The name stays at reading contrast whatever that colour is. |
| PR card, `merged` / `open` / `draft` / `closed` | The state, as a word and as the colour of the card's left edge. |
| `👁 review requested` | GitHub is waiting on **you** to review it. Blue, like everything else on a card that is about you rather than about the work. |
| Bottom line of every card | **Why it is on the canvas**: `assigned to you`, `you opened it`, `sub-issue of #321`, `closes #534`, `refs #325`, `blocks #110`, `your pull request`. |
| Lane header | Click it to open the repository, or to fold it away again. `↗` opens it on GitHub. |
| `⌄ 2 PRs` | An issue's pull requests, **shown by default**. Click to fold them away; nothing is re-fetched either way. |

### Edges

| Line | Meaning |
|---|---|
| Thick solid | Sub-issue hierarchy — the trunk |
| Solid to a PR | `Closes #123`: merging shuts the issue |
| **Dashed** to a PR | `refs #123`: deliberately related, the issue stays open |
| **Faint dotted** to a PR | `#123` in the title with no keyword — our guess, and the only kind that can be wrong |
| Solid PR → PR | A stacked pull request (its base is the other one's head) |
| Red dashed arrow | Blocked by |
| Faint dotted between issues | Duplicate |

The three pull-request lines run from most to least certain; only the faint
dotted one is a guess. Every line except the trunk is labelled on the line
itself — `closes`, `refs`, `mentions?`, `stacked`, `blocked by`, `duplicate` —
so you never have to match a dash pattern against the key. Hovering a card dims
every line not attached to it.

## How issues and pull requests get linked

Links are collected in three layers, plus a fourth that is off by default:

| Layer | Source | Drawn as |
|---|---|---|
| 1 | `closingIssuesReferences` | Solid |
| 2 | `refs #N` in the body | Dashed |
| 3 | A bare `#N` in the title — `feat(parse): #603 ...` | Dashed |
| 4 | Timeline cross references | Faint dotted, `xrefs` only |

Layer 3 only trusts a number whose issue is already on the canvas, so a stray
`#5` in a title cannot create a link. Each card shows which layer its link came
from.

A pull request that implements nothing on the canvas is shown only when it is
your own open work.

Layer 2 is a convention, not an API. The default pattern matches
`refs #123`, `Refs: #12, #34`, `ref #8` at the start of a line:

```
(?im)^[ \t>*-]*refs?[:：]?[ \t]+((?:#\d+[ \t,、]*)+)
```

Use `-refs-pattern` if your team writes something else. Submatch 1 must hold the
`#12, #34` part.

## Cost

One search per scope, then the hierarchy is expanded in bulk through
`nodes(ids:)` (one rate-limit point per call however many IDs it carries). The
check rollup (`● CI passing`) is fetched only for the pull requests that made
it onto the canvas. A repository costs a few points and takes a few seconds.

## Development

```console
make demo     # canned data, GitHub is never contacted
make check    # go test, go vet, node --check, node --test
make ui-test  # drives the real frontend in headless Chrome
make build    # ./gh-issue-graph
gh extension install .
```

`make ui-test` loads the frontend in Chrome through the DevTools Protocol and
exercises it with real pointer events. It skips itself when no Chrome is
installed; point `CHROME` at a binary to override. `GH_ISSUE_GRAPH_DEMO= make
ui-test` runs the same checks against your live data instead of the fixture.

`GH_ISSUE_GRAPH_DEMO=1` switches to the fixture used for UI work.

No third-party dependencies: `go.mod` has no `require` block, and the frontend is
plain HTML, CSS and JavaScript with no bundler, no framework and no CDN.

Bug reports and feature requests are welcome as issues or pull requests.

## Credits

The idea for this tool came from [gh-pr-graph][pr-graph] by
[orangain][orangain]. Thank you.

## License

MIT

[pr-graph]: https://github.com/orangain/gh-pr-graph
[orangain]: https://github.com/orangain
