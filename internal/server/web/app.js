'use strict'

// The ui-check harness asks the page for window.__errors after every
// interaction. Nothing was filling it in, so the check could only ever pass.
// Collect the real thing: script errors and rejected promises, but not failed
// resource loads, which fire the same event and would turn one missing avatar
// into a red build.
window.__errors = []
window.addEventListener('error', (event) => {
  if (event instanceof ErrorEvent) window.__errors.push(String(event.message))
})
window.addEventListener('unhandledrejection', (event) => {
  window.__errors.push(String(event.reason))
})

// Layout constants. Columns are uniform so ranks line up across a lane even
// though repository, issue and pull request nodes have different widths. The
// column gap is wide enough for an edge to turn a corner, run vertically and
// carry a word saying what it is, which is what the gap is really for.
const COLUMN_WIDTH = 290
const COLUMN_GAP = 78
const ROW_GAP = 16
const LANE_GAP = 18

// Breathing room between the repository frame and the graph inside it.
//
// It has to be part of the coordinates rather than padding on .lane-body:
// measured in Chrome, a static child of the body does land inside the padding,
// but an absolutely positioned one with `left: 0` — which every card is — lands
// on the frame's inner edge instead. Cards were therefore flush against the
// border, and a back edge bowing left of the first column fell off the canvas.
// Putting the inset in the numbers also keeps it visible to the edge router,
// which reads the same positions.
const LANE_PAD = 30

// Edges that define the tree. Blocking and duplicate relations are drawn on top
// of the result but never move a node, otherwise the DAG fights the layout.
const STRUCTURAL = new Set(['parent', 'pr-closes', 'pr-refs', 'pr-xref', 'pr-stack'])

const viewport = document.getElementById('viewport')
const canvas = document.getElementById('canvas')
const lanesEl = document.getElementById('lanes')
const edgesEl = document.getElementById('edges')
const progressEl = document.getElementById('progress')
const progressBar = document.getElementById('progress-bar')
const progressText = document.getElementById('progress-text')
const warningsEl = document.getElementById('warnings')
const emptyEl = document.getElementById('empty')
const form = document.getElementById('controls')
const brand = document.getElementById('brand')
const sortField = document.getElementById('sort')
const foldAllButton = document.getElementById('fold-all')
const unfoldAllButton = document.getElementById('unfold-all')

const fields = {
  repo: document.getElementById('repo'),
  q: document.getElementById('q'),
  assigned: document.getElementById('assigned'),
  authored: document.getElementById('authored'),
  mentioned: document.getElementById('mentioned'),
  closed: document.getElementById('closed'),
  xref: document.getElementById('xref'),
  review: document.getElementById('review'),
  auto: document.getElementById('auto'),
}

// Issues whose pull requests are hidden. The set is of what is *closed*, not
// of what is open, because the default is open: once you have chosen to look
// inside a repository, you want the work that is happening in it, and a pull
// request is where the work is. Nothing is refetched either way — the whole
// graph is already in memory — so hiding them buys only space.
const collapsed = new Set()

// Issues whose sub-issues are hidden, along with everything hanging off them.
// Same shape and lifetime as `collapsed` above: open by default, and dropped on
// reload rather than stored, so a fold never outlives the board it was made on.
const foldedSubs = new Set()

// Glyphs. Inline SVG rather than Unicode: the CSP rules out an icon font, and
// characters like U+26A0 render as a different weight — sometimes as emoji —
// on every platform. Every glyph is 16x16 and paints in currentColor, so the
// colour comes from CSS and dark mode needs no second copy.
//
// Shape carries the meaning first and colour second, so the three issue states
// stay apart for a reader who cannot separate green from grey: a dot is open,
// a play triangle is ready to start, a tick is done.
const ICON = {
  open: '<svg viewBox="0 0 16 16"><circle cx="8" cy="8" r="6.25" fill="none" stroke="currentColor" stroke-width="2"/><circle cx="8" cy="8" r="2.2" fill="currentColor"/></svg>',
  ready: '<svg viewBox="0 0 16 16"><circle cx="8" cy="8" r="6.25" fill="none" stroke="currentColor" stroke-width="2"/><path d="M6.6 5.1 11.1 8l-4.5 2.9Z" fill="currentColor"/></svg>',
  done: '<svg viewBox="0 0 16 16"><circle cx="8" cy="8" r="6.25" fill="none" stroke="currentColor" stroke-width="2"/><path d="M5.1 8.3 7.1 10.3 10.9 6" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></svg>',
  pr: '<svg viewBox="0 0 16 16"><g fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><circle cx="4.4" cy="3.6" r="1.9"/><circle cx="4.4" cy="12.4" r="1.9"/><circle cx="11.6" cy="12.4" r="1.9"/><path d="M4.4 5.5v5"/><path d="M11.6 10.5V6.4a2 2 0 0 0-2-2H7.3"/><path d="M8.9 2.9 7.2 4.4l1.7 1.5"/></g></svg>',
  merged: '<svg viewBox="0 0 16 16"><g fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><circle cx="4.4" cy="3.6" r="1.9" fill="currentColor"/><circle cx="4.4" cy="12.4" r="1.9" fill="currentColor"/><circle cx="11.6" cy="8" r="1.9" fill="currentColor"/><path d="M4.4 5.5v5"/><path d="M9.7 8H7.4a3 3 0 0 1-3-3"/></g></svg>',
  warn: '<svg viewBox="0 0 16 16"><path d="M8 2.2 14.8 13.8H1.2Z" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linejoin="round"/><path d="M8 6.6v3.2" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/><circle cx="8" cy="11.7" r=".95" fill="currentColor"/></svg>',
  blocked: '<svg viewBox="0 0 16 16"><circle cx="8" cy="8" r="6.1" fill="none" stroke="currentColor" stroke-width="1.8"/><path d="M3.9 12.1 12.1 3.9" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"/></svg>',
  eye: '<svg viewBox="0 0 16 16"><g fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M1.4 8S3.9 3.6 8 3.6 14.6 8 14.6 8 12.1 12.4 8 12.4 1.4 8 1.4 8Z"/><circle cx="8" cy="8" r="2.1"/></g></svg>',
  chevron: '<svg viewBox="0 0 16 16"><path d="M6 3.4 10.6 8 6 12.6" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></svg>',
  external: '<svg viewBox="0 0 16 16"><path d="M6.2 3.2H3.4a1 1 0 0 0-1 1v8.4a1 1 0 0 0 1 1h8.4a1 1 0 0 0 1-1V9.8M9.2 2.6h4.2v4.2M13.4 2.6 7.6 8.4" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg>',
}

// Repositories the reader has opened, keyed by owner/name so the choice
// survives a reload and a change of search scope.
//
// The set is of what is *open*, not of what is folded, because the default is
// folded: with several repositories on the board the first screen is then an
// index you can read in one glance, and you go into the one you actually meant.
// Storing it the other way round could not express "closed by default" at all —
// a repository has to appear in the data before its name can go in the set, so
// everything would be open on the first sight of it.
const OPEN_KEY = 'gh-issue-graph:unfolded'
// Deliberately still the old key: renaming it would silently reset a choice
// that has nothing to do with folding.
const SORT_KEY = 'gh-issue-graph:folded:sort'
const unfolded = new Set(readStoredOpen())

function isFolded(nameWithOwner) {
  return !unfolded.has(nameWithOwner)
}

function readStoredOpen() {
  try {
    // The set used to hold what was folded. Its meaning is inverted now, so the
    // old key cannot be migrated — but leaving it behind is just litter.
    localStorage.removeItem('gh-issue-graph:folded')
    const stored = JSON.parse(localStorage.getItem(OPEN_KEY) || '[]')
    return Array.isArray(stored) ? stored : []
  } catch {
    return []
  }
}

function storeFolds() {
  try {
    localStorage.setItem(OPEN_KEY, JSON.stringify([...unfolded]))
  } catch {
    // Private windows and blocked site data are fine; folding just will not stick.
  }
}

function readStoredSort() {
  try {
    return localStorage.getItem(SORT_KEY) || 'recent'
  } catch {
    return 'recent'
  }
}

function storeSort(value) {
  try {
    localStorage.setItem(SORT_KEY, value)
  } catch {
    // ignored, see storeFolds
  }
}

let sourceData = null
// What the last render was built from. A background refresh usually returns a
// board identical to the one on screen, and rebuilding it throws away every
// card, every edge and the hover you were in the middle of for nothing.
let renderedSignature = null
let positions = new Map()
let lastUpdated = 0
let failures = 0
let retryAt = 0
let timer = null

// A card calls this a dozen times, and a board of 500 issues renders thousands
// of them; building a throwaway <div> each time was the single most-called
// allocation in a render.
const ESCAPES = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }

function escapeHTML(value) {
  if (value == null) return ''
  return String(value).replace(/[&<>"']/g, (character) => ESCAPES[character])
}

// ---------------------------------------------------------------- data loading

function readQueryString() {
  const params = new URLSearchParams(location.search)
  if (params.has('repo')) fields.repo.value = params.get('repo')
  if (params.has('q')) fields.q.value = params.get('q')
  for (const name of ['assigned', 'authored', 'mentioned', 'closed', 'xref', 'review']) {
    if (params.has(name)) fields[name].checked = params.get(name) === '1'
  }
}

function currentParams() {
  const params = new URLSearchParams()
  if (fields.repo.value.trim()) params.set('repo', fields.repo.value.trim())
  if (fields.q.value.trim()) params.set('q', fields.q.value.trim())
  for (const name of ['assigned', 'authored', 'mentioned']) {
    params.set(name, fields[name].checked ? '1' : '0')
  }
  if (fields.closed.checked) params.set('closed', '1')
  if (fields.xref.checked) params.set('xref', '1')
  // Always sent, unlike closed/xref: this one defaults on, so its absence has
  // to mean "on" for old bookmarks and "off" has to be said out loud.
  params.set('review', fields.review.checked ? '1' : '0')
  return params
}

function showProgress(percent, text, failed) {
  progressEl.hidden = false
  progressEl.classList.toggle('is-error', Boolean(failed))
  progressBar.style.width = `${Math.max(0, Math.min(100, percent))}%`
  progressText.textContent = text
}

function hideProgress() {
  progressEl.hidden = true
  progressBar.style.width = '0'
}

async function load() {
  const params = currentParams()
  history.replaceState(null, '', `?${params}`)
  showProgress(2, 'Starting…')

  try {
    const response = await fetch(`/api/v1/graph?${params}`)
    if (!response.ok) throw new Error(`HTTP ${response.status}`)

    const reader = response.body.getReader()
    const decoder = new TextDecoder()
    let buffer = ''
    let result = null

    for (;;) {
      const { value, done } = await reader.read()
      if (done) break
      buffer += decoder.decode(value, { stream: true })
      let newline
      while ((newline = buffer.indexOf('\n')) >= 0) {
        const line = buffer.slice(0, newline).trim()
        buffer = buffer.slice(newline + 1)
        if (!line) continue
        const message = JSON.parse(line)
        if (message.type === 'progress') {
          const collected = message.collected ? ` — ${message.collected} issues` : ''
          showProgress(message.percent, `${message.phase}${collected}`)
        } else if (message.type === 'error') {
          throw new Error(message.error)
        } else if (message.type === 'result') {
          result = message.result
        }
      }
    }
    if (!result) throw new Error('no graph returned')

    failures = 0
    lastUpdated = Date.now()
    sourceData = result
    // updatedAt is the time of the fetch, so it differs on every poll and has
    // to be left out of the comparison.
    const signature = JSON.stringify({ nodes: result.nodes, edges: result.edges, warnings: result.warnings })
    if (signature !== renderedSignature) {
      render(result)
      // Set after, so a render that throws is retried rather than remembered
      // as done.
      renderedSignature = signature
    } else {
      // The board is unchanged, but "4h ago" is not: it is computed at render
      // time, so skipping the render would freeze every lane header at whatever
      // it said the first time. Retouch just those, which costs nothing.
      refreshRelativeTimes(result)
    }
    hideProgress()
  } catch (error) {
    failures += 1
    retryAt = Date.now() + refreshTiming.retryDelay(failures)
    showProgress(100, `Could not load: ${error.message} — retrying shortly`, true)
  }
  schedule()
}

function schedule() {
  if (timer) clearTimeout(timer)
  timer = null
  if (!fields.auto.checked) return
  if (document.hidden || !navigator.onLine) return
  const delay = refreshTiming.nextDelay(Date.now(), lastUpdated, failures, retryAt)
  timer = setTimeout(load, delay)
}

// -------------------------------------------------------------- visible subset

// visibleNodes shows an issue's pull requests unless they have been closed up.
// Pull requests stacked on a visible pull request come along, and pull requests
// that belong to no issue stay in view because they are work nobody tracked.
function visibleNodes(data) {
  const byID = new Map(data.nodes.map((node) => [node.id, node]))
  // Keyed by the repository's own id, not the node id: issue and pull request
  // nodes carry `repositoryId`, which has no `repo:` prefix.
  const foldedRepoIDs = new Set(
    data.nodes
      .filter((node) => node.kind === 'repository' && isFolded(node.repository.nameWithOwner))
      .map((node) => node.repository.id),
  )
  const issueOfPR = new Map()
  const stackParent = new Map()
  const subIssues = new Map()
  for (const edge of data.edges) {
    if (edge.kind === 'pr-closes' || edge.kind === 'pr-refs' || edge.kind === 'pr-xref') {
      if (!issueOfPR.has(edge.target)) issueOfPR.set(edge.target, [])
      issueOfPR.get(edge.target).push(edge.source)
    } else if (edge.kind === 'pr-stack') {
      stackParent.set(edge.target, edge.source)
    } else if (edge.kind === 'parent') {
      if (!subIssues.has(edge.source)) subIssues.set(edge.source, [])
      subIssues.get(edge.source).push(edge.target)
    }
  }

  // Folding a parent takes its whole subtree, not just the row beneath it:
  // leaving grandchildren behind would strand them in a column with nothing
  // pointing at them.
  const hiddenIssues = new Set()
  const walk = [...foldedSubs]
  while (walk.length) {
    for (const child of subIssues.get(walk.pop()) || []) {
      if (hiddenIssues.has(child)) continue
      hiddenIssues.add(child)
      walk.push(child)
    }
  }

  const visible = new Set()
  for (const node of data.nodes) {
    if (node.kind === 'repository') {
      visible.add(node.id)
      continue
    }
    // A folded repository keeps only its own node; its summary line says how
    // much is behind it.
    const repositoryID = node.kind === 'issue' ? node.issue.repositoryId : node.pullRequest.repositoryId
    if (foldedRepoIDs.has(repositoryID)) continue
    if (node.kind !== 'pullRequest') {
      if (!hiddenIssues.has(node.id)) visible.add(node.id)
      continue
    }
    const owners = issueOfPR.get(node.id)
    if (!owners) {
      // Unlinked: shown beside the issues, at the same rank.
      if (!stackParent.has(node.id)) visible.add(node.id)
      continue
    }
    // Shown unless every issue it belongs to has been closed up or folded away
    // with its parent. A pull request that implements two issues stays for as
    // long as one of them is still on the canvas and open.
    if (owners.some((id) => !collapsed.has(id) && !hiddenIssues.has(id))) visible.add(node.id)
  }
  // Stacked pull requests follow whatever they are stacked on.
  for (let pass = 0; pass < data.nodes.length; pass += 1) {
    let changed = false
    for (const [child, parent] of stackParent) {
      if (!visible.has(child) && visible.has(parent)) {
        visible.add(child)
        changed = true
      }
    }
    if (!changed) break
  }
  return { visible, byID, issueOfPR, subIssues }
}

// ------------------------------------------------------------------- rendering

// Set while handling a click so the node just interacted with is the one held
// still across the re-render.
let anchorHint = null

function render(data) {
  const { visible, byID, issueOfPR, subIssues } = visibleNodes(data)
  const anchor = captureViewport(anchorHint)
  anchorHint = null

  const nodes = data.nodes.filter((node) => visible.has(node.id))
  const edges = data.edges.filter((edge) => visible.has(edge.source) && visible.has(edge.target))

  renderWarnings(data.warnings)
  emptyEl.hidden = data.nodes.some((node) => node.kind === 'issue')
  lanesEl.innerHTML = ''
  positions = new Map()

  const counts = { prs: countPRs(data, issueOfPR), subs: countSubIssues(data, subIssues) }
  const lanes = sortLanes(groupIntoLanes(nodes, edges))

  for (const lane of lanes) {
    const section = document.createElement('section')
    section.className = 'lane'
    if (lane.repo && isFolded(lane.repo.nameWithOwner)) section.classList.add('is-folded')
    section.appendChild(laneHeader(lane))

    const body = document.createElement('div')
    body.className = 'lane-body'
    section.appendChild(body)
    lanesEl.appendChild(section)
    lane.bodyEl = body

    const laneRepo = lane.repo ? lane.repo.nameWithOwner : ''
    for (const node of lane.nodes) {
      body.appendChild(createNode(node, counts, laneRepo))
    }
    const size = layoutLane(lane, body)
    body.style.height = `${size.height}px`
    body.style.width = `${size.width}px`
  }

  // Positions were measured inside each lane. Convert them to canvas
  // coordinates by measuring where each lane actually landed — the frames,
  // margins and headers are CSS, and guessing at their heights drifts.
  const canvasBox = canvas.getBoundingClientRect()
  for (const header of lanesEl.querySelectorAll('.lane-head[data-id]')) {
    const box = header.getBoundingClientRect()
    positions.set(header.dataset.id, {
      left: box.left - canvasBox.left + canvas.scrollLeft,
      top: box.top - canvasBox.top + canvas.scrollTop,
      width: box.width,
      height: box.height,
    })
  }
  for (const lane of lanes) {
    if (!lane.bodyEl) continue
    const bodyBox = lane.bodyEl.getBoundingClientRect()
    const offsetX = bodyBox.left - canvasBox.left + canvas.scrollLeft
    const offsetY = bodyBox.top - canvasBox.top + canvas.scrollTop
    for (const node of lane.nodes) {
      const position = positions.get(node.id)
      if (!position) continue
      position.left += offsetX
      position.top += offsetY
    }
  }

  restoreViewport(anchor)
  drawEdges(edges)
}

// laneHeader is the repository: a frame around the graph rather than a node
// inside it, since a line from the repository to each of its issues is true of
// every node and therefore tells you nothing.
function laneHeader(lane) {
  const header = document.createElement('header')
  header.className = 'lane-head'
  if (!lane.repo) {
    header.innerHTML = '<div class="lane-head-inner"><span class="title">Elsewhere</span></div>'
    return header
  }
  const repo = lane.repo
  const folded = isFolded(repo.nameWithOwner)
  header.dataset.fold = repo.nameWithOwner
  header.dataset.id = RepoNodeID(repo.id)
  header.setAttribute('role', 'button')
  header.setAttribute('aria-expanded', String(!folded))
  header.title = folded ? 'click to open this repository' : 'click to fold it away'

  const open = repo.openIssueCount || 0
  const meta = []
  if (repo.defaultBranch) meta.push(`<span class="branch">${escapeHTML(repo.defaultBranch)}</span>`)
  // "4 open of 7" rather than "4 open / 7 issues": the slash read as a second,
  // unrelated count instead of the total the first one came out of.
  meta.push(`<span>${open} open${repo.issueCount > open ? ` of ${repo.issueCount}` : ''}</span>`)
  if (repo.pullRequestCount) meta.push(`<span>${repo.pullRequestCount} PR${repo.pullRequestCount === 1 ? '' : 's'}</span>`)
  if (repo.updatedAt) meta.push(`<span class="age" title="last updated ${escapeHTML(repo.updatedAt)}">${escapeHTML(relativeTime(repo.updatedAt))}</span>`)

  // The inner wrapper is what sticks to the left edge, so the repository name
  // stays readable however far right the graph is panned.
  header.innerHTML = `<div class="lane-head-inner">
      <span class="fold" aria-hidden="true">${ICON.chevron}</span>
      <span class="title">${escapeHTML(repo.nameWithOwner)}</span>
      <span class="lane-meta">${meta.join('')}</span>
      <a class="open" href="${escapeHTML(repo.url)}" target="_blank" rel="noreferrer" title="Open ${escapeHTML(repo.nameWithOwner)} on GitHub" aria-label="Open on GitHub">${ICON.external}</a>
    </div>`
  return header
}

// The only text on the board that goes stale on its own. Kept out of render so
// an unchanged refresh does not have to rebuild 90 cards to move one clock.
function refreshRelativeTimes(data) {
  for (const node of data.nodes) {
    if (node.kind !== 'repository' || !node.repository.updatedAt) continue
    const header = lanesEl.querySelector(`.lane-head[data-id="${CSS.escape(RepoNodeID(node.repository.id))}"]`)
    const age = header && header.querySelector('.age')
    if (age) age.textContent = relativeTime(node.repository.updatedAt)
  }
}

function RepoNodeID(repositoryID) {
  return `repo:${repositoryID}`
}

function renderWarnings(warnings) {
  if (!warnings || warnings.length === 0) {
    warningsEl.hidden = true
    warningsEl.innerHTML = ''
    return
  }
  warningsEl.hidden = false
  warningsEl.innerHTML = warnings.map((text) => `<p>${escapeHTML(text)}</p>`).join('')
}

// countPRs counts how many pull requests each issue has, so a folded issue can
// still say how much is hidden behind it.
function countPRs(data, issueOfPR) {
  const counts = new Map()
  for (const [prID, owners] of issueOfPR) {
    for (const issueID of owners) {
      counts.set(issueID, (counts.get(issueID) || 0) + 1)
    }
    void prID
  }
  return counts
}

// countSubIssues counts the children each issue has *on the canvas*, which is
// not the same number as `subIssuesSummary` reports: a sub-issue in a folded
// repository, or one the search never reached, is counted by GitHub and not
// drawn here. The toggle only claims to fold away what can be seen.
function countSubIssues(data, subIssues) {
  const counts = new Map()
  const onCanvas = new Set(data.nodes.map((node) => node.id))
  for (const [parentID, children] of subIssues) {
    const drawn = children.filter((id) => onCanvas.has(id)).length
    if (drawn) counts.set(parentID, drawn)
  }
  return counts
}

// groupIntoLanes puts every node in its own repository's lane and picks a
// single layout parent per node.
function groupIntoLanes(nodes, edges) {
  const lanes = new Map()
  for (const node of nodes) {
    if (node.kind !== 'repository') continue
    lanes.set(node.repository.id, { repo: node.repository, nodes: [], children: new Map(), parent: new Map() })
  }
  const elsewhere = { repo: null, nodes: [], children: new Map(), parent: new Map() }

  for (const node of nodes) {
    if (node.kind === 'repository') continue
    const repositoryID = node.kind === 'issue' ? node.issue.repositoryId : node.pullRequest.repositoryId
    const lane = lanes.get(repositoryID) || elsewhere
    lane.nodes.push(node)
  }

  const parentOf = new Map()
  for (const edge of edges) {
    if (!STRUCTURAL.has(edge.kind)) continue
    if (parentOf.has(edge.target)) continue
    parentOf.set(edge.target, edge.source)
  }

  // Every repository keeps its lane even when folded: the frame and its summary
  // line are the whole point of folding, and dropping the empty lane would take
  // the control to unfold it with them.
  const all = [...lanes.values()]
  if (elsewhere.nodes.length) all.push(elsewhere)

  for (const lane of all) {
    const ids = new Set(lane.nodes.map((node) => node.id))
    for (const node of lane.nodes) {
      const parent = parentOf.get(node.id)
      // A parent in another repository cannot be a layout parent here; the node
      // simply starts a tree of its own in its own lane.
      if (parent && ids.has(parent)) {
        lane.parent.set(node.id, parent)
        if (!lane.children.has(parent)) lane.children.set(parent, [])
        lane.children.get(parent).push(node.id)
      }
    }
    lane.byID = new Map(lane.nodes.map((node) => [node.id, node]))
  }
  return all
}

// sortLanes orders the repositories. Recently updated first by default: what
// moved last is what you are most likely looking for.
function sortLanes(lanes) {
  const mode = sortField.value
  return lanes.slice().sort((a, b) => {
    const left = a.repo
    const right = b.repo
    // The bucket of nodes with no repository always sinks to the bottom.
    if (!left) return 1
    if (!right) return -1
    if (mode === 'name') return left.nameWithOwner.localeCompare(right.nameWithOwner)
    if (mode === 'open') {
      const diff = (right.openIssueCount || 0) - (left.openIssueCount || 0)
      if (diff !== 0) return diff
      return left.nameWithOwner.localeCompare(right.nameWithOwner)
    }
    const diff = Date.parse(right.updatedAt || 0) - Date.parse(left.updatedAt || 0)
    if (diff) return diff
    return left.nameWithOwner.localeCompare(right.nameWithOwner)
  })
}

// layoutLane is a bottom-up tidy tree: measure each subtree with the real DOM
// height, then place parents at the centre of their children.
function layoutLane(lane, laneEl) {
  // One pass over the lane's own children instead of a querySelector per node.
  // The old code ran two — one to measure, one to place — and each was a scan
  // of the whole subtree, so laying out a lane was quadratic in its size.
  const elements = new Map()
  for (const element of laneEl.children) {
    if (element.dataset && element.dataset.id) elements.set(element.dataset.id, element)
  }

  // Every measurement is taken before a single style is written. Reading
  // offsetWidth after setting style.left forces the browser to lay the page out
  // again, once per card; batched this way the whole lane costs one layout.
  const boxes = new Map()
  for (const [id, element] of elements) {
    boxes.set(id, { width: element.offsetWidth, height: element.offsetHeight })
  }
  const boxOf = (id) => boxes.get(id) || { width: COLUMN_WIDTH, height: 40 }

  const heights = new Map()
  const visiting = new Set()

  const measure = (id) => {
    if (heights.has(id)) return heights.get(id)
    if (visiting.has(id)) return 0
    visiting.add(id)
    const own = boxOf(id).height
    const children = lane.children.get(id) || []
    let total = 0
    for (const child of children) total += measure(child) + ROW_GAP
    if (total > 0) total -= ROW_GAP
    const height = Math.max(own, total)
    visiting.delete(id)
    heights.set(id, height)
    return height
  }

  const roots = lane.nodes.filter((node) => !lane.parent.has(node.id)).map((node) => node.id)
  for (const id of roots) measure(id)
  if (roots.length === 0) return { height: 0, width: 0 }

  // The widest point of *this* lane. It used to be taken from `positions`,
  // which is the whole board's map: every lane after the first inherited the
  // width of the widest one before it, so most lanes were sized for content
  // they did not hold.
  let widest = 0

  const place = (id, top) => {
    const node = lane.byID.get(id)
    const element = elements.get(id)
    const box = boxOf(id)
    const left = LANE_PAD + (node.rank || 0) * (COLUMN_WIDTH + COLUMN_GAP)
    // Top-aligned, not centred on the subtree. Centring means unfolding a node
    // pushes it — and everything above it — upwards, so the thing you just
    // clicked jumps out from under the cursor. Aligned to the top, an unfold
    // only ever grows downwards.
    const y = LANE_PAD + top
    if (element) {
      element.style.left = `${left}px`
      element.style.top = `${y}px`
    }
    positions.set(id, { left, top: y, width: box.width, height: box.height })
    widest = Math.max(widest, left + box.width)

    let childTop = top
    for (const child of lane.children.get(id) || []) {
      place(child, childTop)
      childTop += (heights.get(child) || 0) + ROW_GAP
    }
  }

  let top = 0
  for (const id of roots) {
    place(id, top)
    top += (heights.get(id) || 0) + ROW_GAP
  }

  return { height: Math.max(0, top - ROW_GAP) + LANE_PAD * 2, width: widest + LANE_PAD }
}

// ------------------------------------------------------------------ node HTML

function createNode(node, counts, laneRepo) {
  const element = document.createElement('article')
  element.dataset.id = node.id
  if (node.kind === 'issue') {
    element.className = `node ${issueClasses(node.issue)}`
    element.innerHTML = issueHTML(node, counts.prs.get(node.id) || 0, counts.subs.get(node.id) || 0, laneRepo)
  } else {
    element.className = `node pr ${node.pullRequest.state.toLowerCase()}${node.pullRequest.isDraft ? ' draft' : ''}`
    element.innerHTML = pullRequestHTML(node.pullRequest, laneRepo)
  }
  paintDataStyles(element)
  return element
}

// The server sends `style-src 'self'` with no 'unsafe-inline', so a style
// attribute inside generated HTML is dropped before it ever reaches the render
// tree. That is why every GitHub label was drawn in the same grey no matter
// what colour the repository had given it: the tint was written as a style
// attribute and silently discarded. CSSOM assignments are not covered by the
// directive — which is how node positions have always been set — so anything
// whose value comes from the data is applied here instead.
function paintDataStyles(root) {
  for (const element of root.querySelectorAll('[data-colour],[data-fill]')) {
    if (element.dataset.colour) element.style.background = `#${element.dataset.colour}`
    if (element.dataset.fill) element.style.width = `${element.dataset.fill}%`
  }
}

function issueClasses(issue) {
  const classes = [issue.relation || 'other']
  if (issue.state === 'CLOSED') classes.push('state-closed')
  if (issue.source === 'complement') classes.push('complement')
  if (issue.actionable) classes.push('actionable')
  return classes.join(' ')
}

// relativeTime keeps the summary line short: "3d" reads faster than a date when
// the only question is which repository moved last.
function relativeTime(value) {
  const then = Date.parse(value)
  if (!then) return ''
  const seconds = Math.max(0, (Date.now() - then) / 1000)
  const units = [
    ['y', 31536000],
    ['mo', 2592000],
    ['d', 86400],
    ['h', 3600],
    ['m', 60],
  ]
  for (const [suffix, size] of units) {
    if (seconds >= size) return `${Math.floor(seconds / size)}${suffix} ago`
  }
  return 'just now'
}

const ATTENTION_TEXT = {
  'children-done': 'every sub-issue is done, but this is still open',
  'merged-pr-open': 'a pull request was merged, but this is still open',
}

// A label colour is how people recognise a label at a glance, but tinting the
// whole chip with it sinks half of GitHub's palette into the page. Keeping the
// hue as a dot preserves the recognition and leaves the name at full contrast.
function labelColour(value) {
  const hex = String(value || '').replace(/[^0-9a-fA-F]/g, '').slice(0, 6)
  return hex.length === 6 ? hex : 'd0d7de'
}

function issueHTML(node, prCount, subCount, laneRepo) {
  const issue = node.issue
  const parts = []
  const closed = issue.state === 'CLOSED'
  const ready = !closed && Boolean(issue.actionable)

  // The first thing on the card is what state the work is in, as a shape.
  const glyph = closed ? ICON.done : ready ? ICON.ready : ICON.open
  const stateTitle = closed
    ? 'closed'
    : ready
      ? 'open, and ready to pick up: nothing blocks it and no sub-issue is unfinished'
      : 'open'

  const top = [
    `<span class="state" title="${stateTitle}">${glyph}</span>`,
    `<span class="number">#${issue.number}</span>`,
  ]
  // The word next to the glyph is what makes the colour unnecessary.
  if (ready) top.push('<span class="flag ready">ready</span>')
  if (issue.source === 'complement') {
    top.push('<span class="flag context" title="outside your search — pulled in so the tree makes sense">for context</span>')
  }
  if (issue.issueType) top.push(`<span class="chip type">${escapeHTML(issue.issueType.name)}</span>`)
  top.push(otherRepoHTML(issue.repository, laneRepo))
  parts.push(`<div class="top">${top.join('')}</div>`)

  parts.push(`<a class="title" href="${escapeHTML(issue.url)}" target="_blank" rel="noreferrer">${escapeHTML(issue.title)}</a>`)

  const meta = []
  for (const assignee of issue.assignees || []) {
    meta.push(`<img class="avatar" src="${escapeHTML(assignee.avatarUrl)}" alt="${escapeHTML(assignee.login)}" title="assigned to ${escapeHTML(assignee.login)}">`)
  }
  if (issue.subTotal > 0) {
    // The bare count makes you do the arithmetic; the meter answers "nearly
    // there?" before you have finished reading the numbers.
    const done = issue.subCompleted >= issue.subTotal
    const percent = Math.round((100 * issue.subCompleted) / issue.subTotal)
    const classes = `chip progress-chip${done ? ' is-done' : ''}`
    const body = `<span class="meter"><i data-fill="${percent}"></i></span>${issue.subCompleted}/${issue.subTotal}`
    const progressTitle = `${issue.subCompleted} of ${issue.subTotal} sub-issues done`
    if (subCount > 0) {
      // The same chip doubles as the fold control: it already stands for the
      // sub-issues, and a second chip beside it would cost a card's width to
      // say the same word twice.
      const open = !foldedSubs.has(node.id)
      const plural = subCount === 1 ? '' : 's'
      const what = `the ${subCount} sub-issue${plural} drawn below this one`
      const title = `${progressTitle} — click to ${open ? 'fold away' : 'bring back'} ${what}`
      meta.push(`<button type="button" class="${classes}" data-toggle-subs="${escapeHTML(node.id)}" aria-expanded="${open}" title="${escapeHTML(title)}"><span class="caret">${ICON.chevron}</span>${body}</button>`)
    } else {
      meta.push(`<span class="${classes}" title="${progressTitle}">${body}</span>`)
    }
  }
  // Both attention reasons say the same thing to the reader — close it — so
  // they share one chip and the tooltip carries whichever applies.
  const attention = issue.attention || []
  if (attention.length) {
    const why = attention.map((reason) => ATTENTION_TEXT[reason] || reason).join(' · ')
    meta.push(`<span class="chip attention" title="${escapeHTML(why)}">${ICON.warn}wrap up</span>`)
  }
  const blockers = (issue.blockedByIds || []).length
  if (blockers) {
    meta.push(`<span class="chip blocked" title="waiting on ${blockers} other issue${blockers === 1 ? '' : 's'}">${ICON.blocked}${blockers} blocker${blockers === 1 ? '' : 's'}</span>`)
  }
  for (const label of (issue.labels || []).slice(0, 3)) {
    meta.push(`<span class="chip label" title="label: ${escapeHTML(label.name)}"><i class="dot" data-colour="${labelColour(label.color)}"></i>${escapeHTML(label.name)}</span>`)
  }
  if (prCount > 0) {
    const open = !collapsed.has(node.id)
    const plural = prCount === 1 ? '' : 's'
    meta.push(`<button type="button" class="chip toggle" data-toggle="${escapeHTML(node.id)}" aria-expanded="${open}" title="${open ? 'hide' : 'show'} the ${prCount} pull request${plural} for this issue"><span class="caret">${ICON.chevron}</span>${prCount} PR${plural}</button>`)
  }
  if (meta.length) parts.push(`<div class="meta">${meta.join('')}</div>`)
  parts.push(reasonsHTML(issue.reasons))
  return parts.join('')
}

// Why this card is on the canvas. Always visible: without it a graph that
// pulled something in through a relation looks like a bug.
function reasonsHTML(reasons) {
  if (!reasons || reasons.length === 0) return ''
  return `<div class="reasons">${reasons.map((r) => escapeHTML(r)).join(' · ')}</div>`
}

// otherRepoHTML only names a repository when it is not the lane's own: the lane
// header already says which repository this is, and a long owner/name wraps a
// node to three lines for nothing.
function otherRepoHTML(repository, laneRepo) {
  if (!repository || repository === laneRepo) return ''
  return `<span class="repo">${escapeHTML(repository)}</span>`
}

function pullRequestHTML(pr, laneRepo) {
  const parts = []
  const state = pr.state === 'MERGED' ? 'merged' : pr.isDraft ? 'draft' : pr.state.toLowerCase()
  const glyph = pr.state === 'MERGED' ? ICON.merged : ICON.pr
  parts.push(`<div class="top">
    <span class="state" title="pull request, ${escapeHTML(state)}">${glyph}</span>
    <span class="number">PR #${pr.number}</span>
    <span class="chip state-pill">${escapeHTML(state)}</span>
    ${otherRepoHTML(pr.repository, laneRepo)}
  </div>`)
  parts.push(`<a class="title" href="${escapeHTML(pr.url)}" target="_blank" rel="noreferrer">${escapeHTML(pr.title)}</a>`)

  const meta = []
  if (pr.reviewRequested) {
    // The one thing on a pull request card that is about *you* rather than
    // about the pull request, so it leads the row and carries the word.
    meta.push(`<span class="chip review-requested" title="GitHub is waiting on your review">${ICON.eye}review requested</span>`)
  }
  if (pr.author && pr.author.avatarUrl) {
    meta.push(`<img class="avatar" src="${escapeHTML(pr.author.avatarUrl)}" alt="${escapeHTML(pr.author.login)}" title="${escapeHTML(pr.author.login)}">`)
  }
  if (pr.ciState) {
    const kind = pr.ciState === 'SUCCESS' ? 'ok' : pr.ciState === 'FAILURE' || pr.ciState === 'ERROR' ? 'bad' : 'warn'
    meta.push(`<span class="status ${kind}" title="continuous integration: ${escapeHTML(pr.ciState.toLowerCase())}">CI ${escapeHTML(pr.ciState.toLowerCase())}</span>`)
  }
  if (pr.reviewDecision) {
    const kind = pr.reviewDecision === 'APPROVED' ? 'ok' : pr.reviewDecision === 'CHANGES_REQUESTED' ? 'bad' : 'warn'
    const text = pr.reviewDecision.toLowerCase().replace(/_/g, ' ')
    meta.push(`<span class="status ${kind}" title="review: ${escapeHTML(text)}">${escapeHTML(text)}</span>`)
  }
  if (meta.length) parts.push(`<div class="meta">${meta.join('')}</div>`)
  parts.push(reasonsHTML(pr.reasons))
  return parts.join('')
}

// ------------------------------------------------------------------ edge paths

const SVG_NS = 'http://www.w3.org/2000/svg'

// Weight and dash come from CSS (`.edge-<kind>`), so the whole edge palette
// re-tunes for dark mode with the rest of the tokens instead of carrying a
// second copy of every colour here.
//
// The arrowhead has to be declared per colour — SVG markers do not inherit
// currentColor from the path that uses them. The old file had one marker,
// painted red, and used it for `duplicate` as well: every "duplicate of"
// arrow was drawn in the colour reserved for "blocked by".
const EDGE_ARROW = { blocked: 'arrow-blocked', duplicate: 'arrow-weak' }

// An unknown relation falls back to the weakest line, the same way it used to.
const EDGE_KINDS = new Set(['parent', 'pr-closes', 'pr-refs', 'pr-xref', 'pr-stack', 'blocked', 'duplicate'])

// What each line means, written on the line. A reader who has never seen the
// legend can still tell a certain relation from a guess, which is the whole
// point of drawing three weights of line in the first place. The sub-issue
// trunk is left unlabelled: it is the commonest edge by far, and a branching
// bracket already reads as a hierarchy.
const EDGE_LABEL = {
  'pr-closes': 'closes',
  'pr-refs': 'refs',
  'pr-xref': 'mentions?',
  'pr-stack': 'stacked',
  blocked: 'blocked by',
  duplicate: 'duplicate',
}

// How far left of a column a back edge bows. Small enough to stay inside the
// lane's own padding, so a blocking arrow between two cards in the same column
// does not swing out over the page behind the frame.
const BACK_EDGE_BOW = 20
const CORNER = 10

// Rounded steps, not beziers across the whole gap. Every child of one parent
// leaves along the same short stub and turns down the same vertical run, so a
// parent with six children draws one bracket with six arms instead of six
// curves overlapping into a smear at the parent's edge.
function roundedStep(x1, y1, x2, y2, turn) {
  const direction = y2 > y1 ? 1 : -1
  const radius = Math.max(0, Math.min(CORNER, Math.abs(y2 - y1) / 2, Math.abs(turn - x1), Math.abs(x2 - turn)))
  const back = turn < x1 ? -1 : 1
  return (
    `M${x1},${y1} H${turn - radius * back}` +
    ` Q${turn},${y1} ${turn},${y1 + radius * direction}` +
    ` V${y2 - radius * direction}` +
    ` Q${turn},${y2} ${turn + radius * back},${y2}` +
    ` H${x2}`
  )
}

// A forward edge runs right edge to left edge. Anything else — blocking,
// duplicate, a relation that points back up the tree — leaves the left of one
// card and enters the left of the other, bowing just clear of the column. The
// old routing dropped below both cards and steered by x2 - 46, which for two
// cards in the same column put the control point off the left of the canvas.
function edgeGeometry(from, to, offset) {
  const y1 = from.top + from.height / 2
  const y2 = to.top + to.height / 2 + (offset || 0)
  if (to.left >= from.left + from.width + 18) {
    const x1 = from.left + from.width
    const x2 = to.left
    return { x1, y1, x2, y2, turn: x1 + Math.min(Math.max((x2 - x1) / 2, 22), 46), back: false }
  }
  return {
    x1: from.left,
    y1,
    x2: to.left,
    y2,
    turn: Math.min(from.left, to.left) - BACK_EDGE_BOW,
    back: true,
  }
}

function edgePath(geometry) {
  const { x1, y1, x2, y2, turn } = geometry
  if (!geometry.back && Math.abs(y1 - y2) < 0.5) return `M${x1},${y1} H${x2}`
  return roundedStep(x1, y1, x2, y2, turn)
}

// Where the word goes. On a forward edge it sits just before the card the line
// runs into, so it reads as a property of that card ("this pull request closes
// it"). On a back edge it rides the vertical run, which is the only stretch
// clear of the cards.
function labelAnchor(geometry) {
  const { y1, x2, y2, turn } = geometry
  // A back edge bows into the narrow channel left of the first column, where a
  // horizontal word would run straight off the canvas. Turned on its side it
  // fits in the channel and reads along the line it belongs to.
  if (geometry.back) return { x: turn + 15, y: (y1 + y2) / 2, anchor: 'middle', upright: false }
  return { x: x2 - 7, y: y2 - 5, anchor: 'end', upright: true }
}

// Which card the pointer is over, so the edges attached to it can be picked
// out of the bundle. Null when the pointer is not on a card.
let litNodeID = null

// Several relations can land on the same card — a pull request both stacked on
// another and referencing an issue arrives twice at the same pixel, so one line
// hides the other and the two words printed on them overlap into nonsense.
// Fan the arrivals out along the card's left edge instead, ordered by where
// they came from so the lines do not cross on the way in.
function spreadEntries(edges) {
  const byTarget = new Map()
  for (const edge of edges) {
    if (!positions.has(edge.source) || !positions.has(edge.target)) continue
    if (!byTarget.has(edge.target)) byTarget.set(edge.target, [])
    byTarget.get(edge.target).push(edge)
  }
  const offsets = new Map()
  for (const [target, arriving] of byTarget) {
    if (arriving.length < 2) continue
    arriving.sort((a, b) => positions.get(a.source).top - positions.get(b.source).top)
    const height = positions.get(target).height
    const spread = Math.min(16, Math.max(0, height - 12) / arriving.length)
    arriving.forEach((edge, index) => {
      offsets.set(edge, (index - (arriving.length - 1) / 2) * spread)
    })
  }
  return offsets
}

function drawEdges(edges) {
  litNodeID = null
  edgesEl.setAttribute('width', canvas.scrollWidth)
  edgesEl.setAttribute('height', canvas.scrollHeight)
  edgesEl.classList.remove('is-focused')
  edgesEl.innerHTML = `<defs>
    <marker id="arrow-blocked" viewBox="0 0 8 8" refX="6.6" refY="4" markerWidth="5" markerHeight="5" orient="auto-start-reverse">
      <path d="M0.5,0.8 L7.4,4 L0.5,7.2 z" fill="var(--red)"></path>
    </marker>
    <marker id="arrow-weak" viewBox="0 0 8 8" refX="6.6" refY="4" markerWidth="5" markerHeight="5" orient="auto-start-reverse">
      <path d="M0.5,0.8 L7.4,4 L0.5,7.2 z" fill="var(--edge-weak)"></path>
    </marker>
  </defs>`

  const entryOffsets = spreadEntries(edges)

  for (const edge of edges) {
    const from = positions.get(edge.source)
    const to = positions.get(edge.target)
    if (!from || !to) continue

    const kind = EDGE_KINDS.has(edge.kind) ? edge.kind : 'pr-xref'
    const geometry = edgeGeometry(from, to, entryOffsets.get(edge))

    const path = document.createElementNS(SVG_NS, 'path')
    path.setAttribute('class', `edge edge-${kind}${geometry.back ? ' is-back' : ''}`)
    path.setAttribute('d', edgePath(geometry))
    path.dataset.source = edge.source
    path.dataset.target = edge.target
    if (EDGE_ARROW[kind]) path.setAttribute('marker-end', `url(#${EDGE_ARROW[kind]})`)
    edgesEl.appendChild(path)

    const word = EDGE_LABEL[kind]
    if (!word) continue
    const spot = labelAnchor(geometry)
    const label = document.createElementNS(SVG_NS, 'text')
    label.setAttribute('class', `edge-label label-${kind}`)
    label.setAttribute('x', String(spot.x))
    label.setAttribute('y', String(spot.y))
    label.setAttribute('text-anchor', spot.anchor)
    if (!spot.upright) label.setAttribute('transform', `rotate(-90 ${spot.x} ${spot.y})`)
    label.dataset.source = edge.source
    label.dataset.target = edge.target
    label.textContent = word
    edgesEl.appendChild(label)
  }
}

// Hovering a card dims every edge that is not attached to it. Once a column of
// cards has a dozen lines converging on it, this is the difference between
// seeing a bundle and seeing this card's three children.
function lightEdges(id) {
  if (litNodeID === id) return
  litNodeID = id
  edgesEl.classList.toggle('is-focused', Boolean(id))
  for (const element of edgesEl.querySelectorAll('[data-source]')) {
    const lit = Boolean(id) && (element.dataset.source === id || element.dataset.target === id)
    element.classList.toggle('is-lit', lit)
  }
}

// --------------------------------------------------------------- viewport keep

// Remember which node sits nearest the centre so a refresh does not throw the
// reader back to the top-left corner.
function captureViewport(preferred) {
  if (preferred && positions.has(preferred)) {
    const position = positions.get(preferred)
    return {
      id: preferred,
      offsetX: position.left - viewport.scrollLeft,
      offsetY: position.top - viewport.scrollTop,
    }
  }
  const centreX = viewport.scrollLeft + viewport.clientWidth / 2
  const centreY = viewport.scrollTop + viewport.clientHeight / 2
  let best = null
  let bestDistance = Infinity
  for (const [id, position] of positions) {
    const dx = position.left + position.width / 2 - centreX
    const dy = position.top + position.height / 2 - centreY
    const distance = dx * dx + dy * dy
    if (distance < bestDistance) {
      bestDistance = distance
      best = { id, offsetX: position.left - viewport.scrollLeft, offsetY: position.top - viewport.scrollTop }
    }
  }
  return best
}

function restoreViewport(anchor) {
  if (!anchor) return
  const position = positions.get(anchor.id)
  if (!position) return
  viewport.scrollLeft = position.left - anchor.offsetX
  viewport.scrollTop = position.top - anchor.offsetY
}

// ------------------------------------------------------------------- behaviour

lanesEl.addEventListener('click', (event) => {
  // A pan that ended on a node is not a click on it.
  if (dragMoved) return
  const fold = event.target.closest('[data-fold]')
  if (fold && !event.target.closest('a')) {
    event.preventDefault()
    const name = fold.dataset.fold
    if (unfolded.has(name)) unfolded.delete(name)
    else unfolded.add(name)
    storeFolds()
    // Hold the card you clicked still. Without this the anchor falls back to
    // whatever sits nearest the middle of the screen — a card in a lane below —
    // and unfolding pushes that card down, which scrolls the lane you just
    // opened clean off the top of the screen.
    anchorHint = fold.dataset.id || null
    if (sourceData) render(sourceData)
    return
  }

  const subs = event.target.closest('[data-toggle-subs]')
  if (subs) {
    event.preventDefault()
    const id = subs.dataset.toggleSubs
    if (foldedSubs.has(id)) foldedSubs.delete(id)
    else foldedSubs.add(id)
    // Hold the parent still; its subtree grows and shrinks below it.
    anchorHint = id
    if (sourceData) render(sourceData)
    return
  }

  const toggle = event.target.closest('[data-toggle]')
  if (!toggle) return
  event.preventDefault()
  const id = toggle.dataset.toggle
  if (collapsed.has(id)) collapsed.delete(id)
  else collapsed.add(id)
  // Hold the issue you clicked still; the pull requests appear beneath it.
  anchorHint = id
  if (sourceData) render(sourceData)
})

lanesEl.addEventListener('pointerover', (event) => {
  const node = event.target.closest ? event.target.closest('.node') : null
  lightEdges(node ? node.dataset.id : null)
})
lanesEl.addEventListener('pointerleave', () => lightEdges(null))

function everyRepositoryName() {
  if (!sourceData) return []
  return sourceData.nodes.filter((node) => node.kind === 'repository').map((node) => node.repository.nameWithOwner)
}

// Folding or unfolding everything moves every lane, so hold the topmost
// repository still rather than letting the anchor land somewhere arbitrary.
function firstRepositoryNodeID() {
  const header = lanesEl.querySelector('.lane-head[data-id]')
  return header ? header.dataset.id : null
}

foldAllButton.addEventListener('click', () => {
  // Only what is on the board. Clearing the whole set would also throw away the
  // choice made for a repository that the current search happens not to cover —
  // the opposite of what keying the set by owner/name is for, and asymmetric
  // with unfold all, which only ever touches the names it can see.
  for (const name of everyRepositoryName()) unfolded.delete(name)
  storeFolds()
  anchorHint = firstRepositoryNodeID()
  if (sourceData) render(sourceData)
})

unfoldAllButton.addEventListener('click', () => {
  for (const name of everyRepositoryName()) unfolded.add(name)
  // Unfold means unfold: a subtree folded inside a lane would otherwise stay
  // shut behind a button that claims to have opened everything. Fold all is
  // not the mirror of this — it hides the lanes, so what is folded within one
  // is out of sight anyway and worth keeping for when the lane comes back.
  foldedSubs.clear()
  storeFolds()
  anchorHint = firstRepositoryNodeID()
  if (sourceData) render(sourceData)
})

sortField.addEventListener('change', () => {
  storeSort(sortField.value)
  if (sourceData) render(sourceData)
})

form.addEventListener('submit', (event) => {
  event.preventDefault()
  load()
})

for (const name of ['assigned', 'authored', 'mentioned', 'closed', 'xref', 'review']) {
  fields[name].addEventListener('change', () => load())
}
fields.auto.addEventListener('change', schedule)

document.addEventListener('visibilitychange', () => {
  if (!document.hidden) schedule()
})
window.addEventListener('online', schedule)

// Drag to pan, except on things that are meant to be clicked.
let dragging = null
let dragMoved = false
const DRAG_SLOP = 4
// Anything clickable is excluded from panning. Capturing the pointer would
// otherwise retarget the click to the viewport, and the control would never see
// it — which is exactly how the repository card stopped folding.
const NOT_DRAGGABLE = 'a, button, input, label, select, [data-fold], [data-toggle]'
viewport.addEventListener('pointerdown', (event) => {
  dragMoved = false
  if (event.target.closest(NOT_DRAGGABLE)) return
  dragging = { x: event.clientX, y: event.clientY, left: viewport.scrollLeft, top: viewport.scrollTop }
  viewport.classList.add('grabbing')
  try {
    viewport.setPointerCapture(event.pointerId)
  } catch {
    // Synthetic pointer events carry no real pointer to capture.
  }
})
viewport.addEventListener('pointermove', (event) => {
  if (!dragging) return
  const dx = event.clientX - dragging.x
  const dy = event.clientY - dragging.y
  if (Math.abs(dx) > DRAG_SLOP || Math.abs(dy) > DRAG_SLOP) dragMoved = true
  viewport.scrollLeft = dragging.left - dx
  viewport.scrollTop = dragging.top - dy
})
const endDrag = () => {
  dragging = null
  viewport.classList.remove('grabbing')
}
viewport.addEventListener('pointerup', endDrag)
viewport.addEventListener('pointercancel', endDrag)

fetch('/api/v1/meta')
  .then((response) => response.json())
  .then((meta) => {
    brand.title = `gh issue-graph ${meta.version}`
  })
  .catch(() => {})

sortField.value = readStoredSort()
readQueryString()
load()
