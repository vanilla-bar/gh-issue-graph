#!/usr/bin/env python3
"""Drive the real frontend in Chrome with real mouse input.

Everything interesting in app.js only misbehaves once there is a document, and
some of it only misbehaves under a real pointer. Synthetic `element.click()`
misses those: it skips pointerdown, so it cannot see panning start on a control,
and it cannot see a captured pointer retarget the click away from it. An earlier
version of this file did exactly that and reported 19/19 while folding a lane
was, in fact, broken in the browser.

So this speaks the DevTools Protocol and asks Chrome to synthesise the input
itself, which produces genuine pointer events with genuine capture semantics.

Bugs this has caught, all invisible to `go test` and `node --test`:
  * a lane that would not fold, because node ids (`repo:R_1`) were compared
    against repository ids (`R_1`)
  * "no issues matched" showing whenever every lane happened to be folded
  * edges lagging one interaction behind, from deferring to requestAnimationFrame
  * pressing the repository card starting a pan, whose captured pointer then ate
    the click
  * a long owner/name wrapping to three lines, so the link covered the middle of
    the card and clicks there were treated as "open GitHub" instead of "fold"
  * a lane header background painted over the edge layer, which hid every line

Usage:
  internal/server/webtest/ui-check.py            # demo data, GitHub untouched
  GH_ISSUE_GRAPH_DEMO= internal/server/webtest/ui-check.py   # live data
"""
import base64
import json
import os
import socket
import struct
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.request

REPO = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
SERVER_PORT = int(os.environ.get("UI_CHECK_SERVER_PORT", "8796"))
CDP_PORT = int(os.environ.get("UI_CHECK_CDP_PORT", "9334"))
CHROME = os.environ.get("CHROME", "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome")


def find_chrome():
    if os.path.exists(CHROME) and os.access(CHROME, os.X_OK):
        return CHROME
    for name in ("google-chrome", "chromium", "chromium-browser"):
        found = subprocess.run(["which", name], capture_output=True, text=True).stdout.strip()
        if found:
            return found
    return None


class WS:
    """Minimal RFC 6455 client: enough for one DevTools session."""

    def __init__(self, url):
        _, rest = url.split("://", 1)
        hostport, path = rest.split("/", 1)
        host, port = hostport.split(":")
        self.sock = socket.create_connection((host, int(port)))
        self.sock.settimeout(30)
        key = base64.b64encode(os.urandom(16)).decode()
        self.sock.sendall(
            f"GET /{path} HTTP/1.1\r\nHost: {hostport}\r\nUpgrade: websocket\r\n"
            f"Connection: Upgrade\r\nSec-WebSocket-Key: {key}\r\n"
            "Sec-WebSocket-Version: 13\r\n\r\n".encode()
        )
        buf = b""
        while b"\r\n\r\n" not in buf:
            buf += self.sock.recv(4096)
        # Talking to a Chrome we started ourselves on loopback, so checking
        # Sec-WebSocket-Accept buys nothing; a 101 is proof enough of an upgrade.
        if not buf.startswith(b"HTTP/1.1 101"):
            raise RuntimeError(f"websocket handshake rejected: {buf[:120]!r}")
        self.rest = buf.split(b"\r\n\r\n", 1)[1]
        self.next_id = 0

    def _recv(self, n):
        while len(self.rest) < n:
            chunk = self.sock.recv(65536)
            if not chunk:
                raise ConnectionError("connection closed")
            self.rest += chunk
        out, self.rest = self.rest[:n], self.rest[n:]
        return out

    def send(self, payload):
        data = json.dumps(payload).encode()
        header = bytearray([0x81])
        mask = os.urandom(4)
        length = len(data)
        if length < 126:
            header.append(0x80 | length)
        elif length < 65536:
            header.append(0x80 | 126)
            header += struct.pack(">H", length)
        else:
            header.append(0x80 | 127)
            header += struct.pack(">Q", length)
        header += mask
        self.sock.sendall(bytes(header) + bytes(b ^ mask[i % 4] for i, b in enumerate(data)))

    def recv(self):
        _, second = self._recv(2)
        length = second & 0x7F
        if length == 126:
            length = struct.unpack(">H", self._recv(2))[0]
        elif length == 127:
            length = struct.unpack(">Q", self._recv(8))[0]
        return json.loads(self._recv(length))

    def call(self, method, params=None):
        self.next_id += 1
        wanted = self.next_id
        self.send({"id": wanted, "method": method, "params": params or {}})
        while True:
            message = self.recv()
            if message.get("id") == wanted:
                if "error" in message:
                    raise RuntimeError(f"{method}: {message['error']}")
                return message.get("result", {})

    def evaluate(self, expression):
        result = self.call(
            "Runtime.evaluate",
            {"expression": expression, "returnByValue": True, "awaitPromise": True},
        )
        if result.get("exceptionDetails"):
            details = result["exceptionDetails"]
            raise RuntimeError((details.get("exception") or {}).get("description") or details.get("text"))
        return result["result"].get("value")


def mouse(ws, kind, x, y, buttons=0):
    ws.call("Input.dispatchMouseEvent", {
        "type": kind, "x": x, "y": y, "button": "left", "buttons": buttons,
        "clickCount": 1 if kind in ("mousePressed", "mouseReleased") else 0,
        "pointerType": "mouse",
    })


def click(ws, x, y, settle=0.35):
    mouse(ws, "mouseMoved", x, y)
    mouse(ws, "mousePressed", x, y, buttons=1)
    time.sleep(0.05)
    mouse(ws, "mouseReleased", x, y)
    time.sleep(settle)


def drag(ws, x, y, dx, dy):
    mouse(ws, "mouseMoved", x, y)
    mouse(ws, "mousePressed", x, y, buttons=1)
    for step in range(1, 6):
        mouse(ws, "mouseMoved", x + dx * step / 5, y + dy * step / 5, buttons=1)
        time.sleep(0.02)
    mouse(ws, "mouseReleased", x + dx, y + dy)
    time.sleep(0.35)


STATE = """({
  nodes: document.querySelectorAll('#lanes .node').length,
  issues: document.querySelectorAll('#lanes .node:not(.pr)').length,
  prs: document.querySelectorAll('#lanes .node.pr').length,
  paths: document.querySelectorAll('#edges path').length,
  lanes: document.querySelectorAll('.lane').length,
  empty: !document.getElementById('empty').hidden,
  panning: document.getElementById('viewport').classList.contains('grabbing'),
})"""

CENTRE = """(selector) => {
  const node = document.querySelector(selector)
  if (!node) return null
  const box = node.getBoundingClientRect()
  return {x: box.left + box.width / 2, y: box.top + box.height / 2, text: node.textContent.trim().slice(0, 40)}
}"""


def centre(ws, selector):
    return ws.evaluate(f"({CENTRE})({json.dumps(selector)})")


def main():
    chrome_path = find_chrome()
    if not chrome_path:
        print("ui-check: no Chrome found; set CHROME=/path/to/chrome. Skipping.", file=sys.stderr)
        return 0

    binary = os.path.join(REPO, "gh-issue-graph")
    subprocess.run(["go", "build", "-o", binary, "./cmd/gh-issue-graph"], cwd=REPO, check=True)

    # Start the server from this process. The extension exits when its parent
    # goes away, so a backgrounded shell job would die with the shell.
    env = {**os.environ}
    env.setdefault("GH_ISSUE_GRAPH_DEMO", "1")
    server = subprocess.Popen(
        [binary, "-port", str(SERVER_PORT), "-no-open"],
        cwd=REPO, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )
    profile = tempfile.mkdtemp(prefix="ui-check-")
    chrome = None
    try:
        for _ in range(40):
            try:
                urllib.request.urlopen(f"http://127.0.0.1:{SERVER_PORT}/healthz", timeout=1)
                break
            except Exception:
                time.sleep(0.25)
        else:
            raise RuntimeError("server did not start")

        chrome = subprocess.Popen(
            [chrome_path, "--headless=new", "--disable-gpu", "--no-sandbox",
             f"--remote-debugging-port={CDP_PORT}", f"--user-data-dir={profile}",
             "--window-size=1680,1050", f"http://127.0.0.1:{SERVER_PORT}/"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )

        endpoint = None
        for _ in range(60):
            try:
                targets = json.loads(
                    urllib.request.urlopen(f"http://127.0.0.1:{CDP_PORT}/json/list", timeout=2).read())
                pages = [t for t in targets if t["type"] == "page" and t["url"].startswith("http")]
                if pages:
                    endpoint = pages[0]["webSocketDebuggerUrl"]
                    break
            except Exception:
                pass
            time.sleep(0.5)
        if not endpoint:
            raise RuntimeError("could not attach to Chrome")

        ws = WS(endpoint)
        ws.call("Page.enable")
        ws.call("Runtime.enable")

        # Folds are persisted, so a previous run would otherwise start this one
        # with lanes already collapsed.
        ws.evaluate("try { localStorage.clear() } catch (e) {}")
        ws.call("Page.reload", {"ignoreCache": True})
        time.sleep(1.0)

        for _ in range(60):
            if ws.evaluate("document.querySelectorAll('.lane').length"):
                break
            time.sleep(0.5)
        time.sleep(0.5)

        checks = []

        def check(name, ok, detail=""):
            checks.append(("PASS " if ok else "FAIL ") + name + (f" — {detail}" if detail else ""))

        # Repositories start folded: the first screen is an index of what there
        # is, not every card in every repository at once.
        opening = ws.evaluate(STATE)
        check("repositories start folded", opening["lanes"] > 0 and opening["nodes"] == 0,
              json.dumps(opening))
        check("a board of folded lanes does not claim nothing matched", not opening["empty"])

        unfold_button = centre(ws, "#unfold-all")
        click(ws, unfold_button["x"], unfold_button["y"], settle=0.6)

        # ...and a repository you have opened shows its pull requests without
        # being asked a second time.
        check("opening a repository brings its pull requests with it",
              ws.evaluate(STATE)["prs"] > 0, json.dumps(ws.evaluate(STATE)))
        toggles = ws.evaluate("[...document.querySelectorAll('[data-toggle]')]"
                              ".map((b) => b.getAttribute('aria-expanded'))")
        check("every pull request toggle reads as already open",
              len(toggles) > 0 and all(state == "true" for state in toggles),
              f"{len(toggles)} toggle(s): {sorted(set(toggles))}")

        # Sub-issues fold away too, and start open for the same reason the pull
        # requests do: you opened the repository to see what is in it.
        sub_toggles = ws.evaluate("[...document.querySelectorAll('[data-toggle-subs]')]"
                                  ".map((b) => b.getAttribute('aria-expanded'))")
        check("every sub-issue toggle reads as already open",
              len(sub_toggles) > 0 and all(state == "true" for state in sub_toggles),
              f"{len(sub_toggles)} toggle(s): {sorted(set(sub_toggles))}")

        parent = ws.evaluate("""(() => {
          const b = document.querySelector('[data-toggle-subs]')
          if (!b) return null
          const r = b.getBoundingClientRect()
          const card = b.closest('.node')
          return {
            x: r.left + r.width / 2, y: r.top + r.height / 2,
            id: card.dataset.id, top: card.getBoundingClientRect().top,
          }
        })()""")
        check("a parent with sub-issues on the canvas offers the fold", bool(parent),
              json.dumps(parent))
        if parent:
            card_top = ("(() => { const c = [...document.querySelectorAll('#lanes .node')]"
                        f".find((n) => n.dataset.id === {json.dumps(parent['id'])});"
                        " return c ? c.getBoundingClientRect().top : null })()")
            open_state = ws.evaluate(STATE)
            click(ws, parent["x"], parent["y"], settle=0.6)
            shut = ws.evaluate(STATE)
            check("folding a parent takes its sub-issues away",
                  shut["issues"] < open_state["issues"],
                  f"{open_state['issues']} -> {shut['issues']} issue(s)")
            check("and the pull requests that hung off them",
                  shut["prs"] < open_state["prs"],
                  f"{open_state['prs']} -> {shut['prs']} pull request(s)")
            check("the toggle now reads as shut",
                  ws.evaluate("document.querySelector('[data-toggle-subs]')"
                              ".getAttribute('aria-expanded')") == "false")
            # Folding must hold the card you pressed, the same way unfolding a
            # lane does: the subtree is below it, so it has no reason to move.
            shut_top = ws.evaluate(card_top)
            check("the parent stays under the cursor",
                  shut_top is not None and abs(shut_top - parent["top"]) < 2,
                  f"{parent['top']:.0f} -> {shut_top}")

            click(ws, parent["x"], parent["y"], settle=0.6)
            back = ws.evaluate(STATE)
            check("clicking again brings the whole subtree back",
                  back["issues"] == open_state["issues"] and back["prs"] == open_state["prs"]
                  and back["paths"] == open_state["paths"],
                  f"{json.dumps(shut)} -> {json.dumps(back)}, want {json.dumps(open_state)}")

        # A pull request waiting on the reader's review is somebody else's pull
        # request, so no issue search returns it. The issue it references is
        # reverse-looked-up and becomes its parent, which is the whole point:
        # without that the card would float in column zero saying nothing.
        review = ws.evaluate("""(() => {
          const card = document.querySelector('#lanes .node.pr:has(.review-requested)')
          if (!card) return null
          const box = card.getBoundingClientRect()
          const edge = [...document.querySelectorAll('#edges path.edge')]
            .find((p) => p.dataset.target === card.dataset.id)
          const parent = edge && document.querySelector(
            `#lanes .node[data-id="${CSS.escape(edge.dataset.source)}"]`)
          return {
            left: box.left,
            reason: card.querySelector('.reasons').textContent.trim(),
            parentLeft: parent ? parent.getBoundingClientRect().left : null,
            parentReason: parent ? parent.querySelector('.reasons').textContent.trim() : null,
          }
        })()""")
        check("a pull request waiting on your review is on the canvas", bool(review),
              json.dumps(review))
        if review:
            check("it hangs off the issue it references, not off nothing",
                  review["parentLeft"] is not None and review["parentLeft"] < review["left"],
                  f"parent at {review['parentLeft']}, pull request at {review['left']:.0f}")
            check("the reverse-looked-up issue says why it was pulled in",
                  "reviewing #" in (review["parentReason"] or ""), str(review["parentReason"]))

        # Turning the scope off has to take the pull request *and* the issue
        # that was fetched only for it away together, and turning it back on has
        # to restore exactly what was there — not merely more than nothing.
        reviewing_issues = """[...document.querySelectorAll('#lanes .node:not(.pr) .reasons')]
          .filter((r) => r.textContent.includes('reviewing #')).length"""
        with_review = ws.evaluate(STATE)
        reviewed_before = ws.evaluate(reviewing_issues)
        check("the reverse lookup put its issue on the canvas", reviewed_before > 0,
              f"{reviewed_before} issue(s) say 'reviewing #'")

        def set_review(on):
            ws.evaluate("(() => { const b = document.getElementById('review');"
                        f" b.checked = {'true' if on else 'false'};"
                        " b.dispatchEvent(new Event('change')) })()")
            time.sleep(1.2)
            for _ in range(40):
                present = ws.evaluate("document.querySelectorAll('.review-requested').length")
                if (present > 0) == on:
                    break
                time.sleep(0.25)

        set_review(False)
        without = ws.evaluate(STATE)
        check("unticking the review scope drops those pull requests",
              ws.evaluate("document.querySelectorAll('.review-requested').length") == 0,
              json.dumps(without))
        check("and drops the issues that were fetched only for them",
              ws.evaluate(reviewing_issues) == 0,
              f"{ws.evaluate(reviewing_issues)} left")

        set_review(True)
        restored = ws.evaluate(STATE)
        check("ticking it again restores exactly what was there",
              restored["nodes"] == with_review["nodes"] and restored["issues"] == with_review["issues"]
              and restored["prs"] == with_review["prs"],
              f"{json.dumps(without)} -> {json.dumps(restored)}, want {json.dumps(with_review)}")
        check("and brings the reverse-looked-up issues back too",
              ws.evaluate(reviewing_issues) == reviewed_before,
              f"{ws.evaluate(reviewing_issues)} vs {reviewed_before}")

        initial = ws.evaluate(STATE)
        check("renders issues", initial["issues"] > 0, json.dumps(initial))
        check("draws edges", initial["paths"] > 0, f"paths={initial['paths']}")
        check("no empty message with results", not initial["empty"])

        # The repository is the lane's frame now, so it is always above its graph.
        framed = ws.evaluate("""(() => {
          const head = document.querySelector('.lane-head[data-id]')
          const body = head.parentElement.querySelector('.lane-body')
          return head.getBoundingClientRect().bottom <= body.getBoundingClientRect().top + 1
        })()""")
        check("the repository frames its lane from the top", framed)
        check("no repository is drawn as a node",
              ws.evaluate("document.querySelectorAll('#lanes .node.repository').length") == 0)
        # This used to assert `#edges path > 0` — byte-identical to "draws
        # edges", and it never looked at where an edge came from. Every path now
        # carries its endpoints, so ask the actual question: a line from a
        # repository to each of its issues is true of every node and therefore
        # says nothing, which is why the repository became a frame instead.
        edge_ends = ws.evaluate("[...document.querySelectorAll('#edges path[data-source]')]"
                                ".map((p) => [p.dataset.source, p.dataset.target])")
        from_repo = [pair for pair in edge_ends
                     if pair[0].startswith("repo:") or pair[1].startswith("repo:")]
        check("no edge is drawn from a repository",
              len(edge_ends) > 0 and not from_repo,
              f"{len(edge_ends)} edge(s), {len(from_repo)} touching a repository")

        # Pressing the card must fold, not pan. A captured pointer would steal
        # the click, and a link covering the card would steal it too.
        card = centre(ws, ".lane-head[data-id]")
        mouse(ws, "mouseMoved", card["x"], card["y"])
        mouse(ws, "mousePressed", card["x"], card["y"], buttons=1)
        time.sleep(0.1)
        during = ws.evaluate(STATE)
        mouse(ws, "mouseReleased", card["x"], card["y"])
        time.sleep(0.35)
        check("pressing the lane header does not start a pan", not during["panning"])

        folded = ws.evaluate(STATE)
        check("a real click on the header folds the lane", folded["nodes"] < initial["nodes"],
              f"{initial['nodes']} -> {folded['nodes']}")
        check("edges follow the fold", folded["paths"] < initial["paths"],
              f"{initial['paths']} -> {folded['paths']}")
        check("folding keeps every lane header", folded["lanes"] == initial["lanes"],
              f"{initial['lanes']} -> {folded['lanes']}")

        click(ws, card["x"], card["y"])
        check("clicking again unfolds it", ws.evaluate(STATE)["nodes"] == initial["nodes"])

        # Unfolding must hold the card you clicked. Anchoring on whatever sat
        # nearest the middle of the screen instead sent the lane you just opened
        # 1885px off the top.
        repo_top = "document.querySelector('.lane-head[data-id]').getBoundingClientRect().top"
        click(ws, card["x"], card["y"])            # fold
        folded_top = ws.evaluate(repo_top)
        again = centre(ws, ".lane-head[data-id]")
        click(ws, again["x"], again["y"])          # unfold
        unfolded_top = ws.evaluate(repo_top)
        check("unfolding a lane holds the card you clicked",
              abs(unfolded_top - folded_top) < 4,
              f"top {folded_top:.0f} -> {unfolded_top:.0f}")

        # fold all / unfold all move every lane, so they need an anchor too.
        top_before = ws.evaluate(repo_top)
        fold_button = centre(ws, "#fold-all")
        click(ws, fold_button["x"], fold_button["y"])
        unfold_button = centre(ws, "#unfold-all")
        click(ws, unfold_button["x"], unfold_button["y"])
        check("fold all then unfold all keeps the first lane in place",
              abs(ws.evaluate(repo_top) - top_before) < 4,
              f"top {top_before:.0f} -> {ws.evaluate(repo_top):.0f}")

        toggle = centre(ws, "[data-toggle]")
        if toggle:
            ws.evaluate("document.querySelector('[data-toggle]').scrollIntoView({block: 'center'})")
            time.sleep(0.2)
            toggle = centre(ws, "[data-toggle]")
            before = ws.evaluate(STATE)
            click(ws, toggle["x"], toggle["y"])
            after = ws.evaluate(STATE)
            check("a real click folds the pull requests away", after["prs"] < before["prs"],
                  f"{before['prs']} -> {after['prs']}")
            check("edges follow that fold", after["paths"] < before["paths"],
                  f"{before['paths']} -> {after['paths']}")
            click(ws, centre(ws, "[data-toggle]")["x"], centre(ws, "[data-toggle]")["y"])
            check("clicking again brings the pull requests back",
                  ws.evaluate(STATE)["prs"] == before["prs"])

            # Unfolding must grow downwards. If the card moves up under the
            # cursor, the thing you just clicked is no longer where you left it.
            # They start open now, so close them first and measure the reopen.
            owner = """(() => {
              const b = document.querySelector('[data-toggle]')
              const card = b.closest('.node')
              const r = card.getBoundingClientRect()
              return {top: r.top, id: card.dataset.id}
            })()"""
            toggle = centre(ws, "[data-toggle]")
            click(ws, toggle["x"], toggle["y"])          # fold away
            was = ws.evaluate(owner)
            toggle = centre(ws, "[data-toggle]")
            click(ws, toggle["x"], toggle["y"])          # and open again
            now = ws.evaluate(owner)
            check("unfolding opens downwards, not upwards",
                  now["id"] == was["id"] and abs(now["top"] - was["top"]) < 4,
                  f"top {was['top']:.0f} -> {now['top']:.0f}")

            reasons = ws.evaluate(
                "document.querySelectorAll('#lanes .node .reasons').length")
            total = ws.evaluate("document.querySelectorAll('#lanes .node:not(.repository)').length")
            check("every card says why it is here", reasons == total, f"{reasons}/{total}")
        else:
            check("a real click folds the pull requests away", False, "no toggle rendered")

        fold_all = centre(ws, "#fold-all")
        click(ws, fold_all["x"], fold_all["y"])
        every = ws.evaluate(STATE)
        check("fold all leaves only the headers", every["issues"] == 0 and every["prs"] == 0,
              json.dumps(every))
        check("fold all does not claim nothing matched", not every["empty"])

        unfold_all = centre(ws, "#unfold-all")
        click(ws, unfold_all["x"], unfold_all["y"])
        # Compare issues, not nodes: a pull request left unfolded by an earlier
        # step would otherwise make this look like a regression.
        check("unfold all restores everything",
              ws.evaluate(STATE)["issues"] == initial["issues"],
              f"{ws.evaluate(STATE)['issues']} vs {initial['issues']}")

        lane_order = "[...document.querySelectorAll('.lane-head .title')].map((n) => n.textContent.trim())"
        recent = ws.evaluate(lane_order)
        ws.evaluate("(() => { const s = document.getElementById('sort'); s.value = 'name'; s.dispatchEvent(new Event('change')) })()")
        time.sleep(0.4)
        by_name = ws.evaluate(lane_order)
        check("sorting by name orders the lanes", by_name == sorted(by_name), ", ".join(by_name))
        ws.evaluate("(() => { const s = document.getElementById('sort'); s.value = 'recent'; s.dispatchEvent(new Event('change')) })()")
        time.sleep(0.4)
        check("switching back restores the recent order", ws.evaluate(lane_order) == recent)

        room = ws.evaluate(
            "(() => { const v = document.getElementById('viewport');"
            " return {x: v.scrollWidth - v.clientWidth, y: v.scrollHeight - v.clientHeight}; })()")
        ws.evaluate("(() => { const v = document.getElementById('viewport'); v.scrollLeft = 0; v.scrollTop = 0 })()")
        time.sleep(0.15)
        drag(ws, 1300, 700, -260, -60)
        moved = ws.evaluate("(() => { const v = document.getElementById('viewport');"
                            " return v.scrollLeft !== 0 || v.scrollTop !== 0 })()")
        check("dragging blank canvas pans the view", moved or (room["x"] == 0 and room["y"] == 0),
              f"room {room['x']}x{room['y']}")
        check("panning stops on release", not ws.evaluate(STATE)["panning"])

        # A refresh that returns the same board must not tear the board down.
        # Marking a live card and seeing the mark survive is the only way to
        # tell a skipped render from a fast one.
        ws.evaluate("document.querySelector('#lanes .node').__uiCheckMark = 'kept'")
        ws.evaluate("load()")
        time.sleep(2.5)
        check("an unchanged refresh does not rebuild the board",
              ws.evaluate("document.querySelector('#lanes .node').__uiCheckMark === 'kept'"))
        # ...and the one thing that does go stale on its own still moves.
        check("but the lane header's relative time is still refreshed",
              ws.evaluate("""(() => {
                const age = document.querySelector('.lane-head .age')
                if (!age) return false
                age.textContent = 'STALE'
                load()
                return true
              })()"""))
        time.sleep(2.5)
        check("the relative time was rewritten by the skipped refresh",
              ws.evaluate("document.querySelector('.lane-head .age').textContent !== 'STALE'"),
              ws.evaluate("document.querySelector('.lane-head .age').textContent"))

        # Tucking, and what a reload remembers. Last, because it is the only
        # part that deliberately leaves something in localStorage.
        def reload_board():
            ws.call("Page.reload", {"ignoreCache": True})
            time.sleep(1.5)
            for _ in range(60):
                if ws.evaluate("document.querySelectorAll('#lanes .node').length"):
                    break
                time.sleep(0.3)
            time.sleep(0.7)

        def shut_counts():
            return ws.evaluate("""(() => {
              const shut = (sel) => [...document.querySelectorAll(sel)]
                .filter((b) => b.getAttribute('aria-expanded') === 'false').length
              return { tucked: document.querySelectorAll('#lanes .node.tucked').length,
                       prs: shut('[data-toggle]'), subs: shut('[data-toggle-subs]') }
            })()""")

        # Pick a control that is on screen and not inside a tucked card, whose
        # chips are display:none and therefore have no box to click.
        def clickable(selector):
            return ws.evaluate("""(() => {
              const el = [...document.querySelectorAll(%s)].find((b) => {
                const r = b.getBoundingClientRect()
                return r.width > 0 && r.top > 140 && r.bottom < window.innerHeight - 140
              })
              if (!el) return null
              const r = el.getBoundingClientRect()
              return {x: r.left + r.width / 2, y: r.top + r.height / 2, text: el.textContent.trim()}
            })()""" % json.dumps(selector))

        # Folding inside a lane used to be forgotten on reload while the lanes
        # themselves were remembered. Sub-issues first, on their own: folding a
        # parent takes its children's own toggles off the board with them.
        sub_toggle = clickable(".node [data-toggle-subs]")
        check("a sub-issue toggle is reachable", bool(sub_toggle), json.dumps(sub_toggle))
        click(ws, sub_toggle["x"], sub_toggle["y"], settle=0.6)
        reload_board()
        check("a reload keeps the sub-issues folded away", shut_counts()["subs"] == 1,
              json.dumps(shut_counts()))
        unfold = centre(ws, "#unfold-all")
        click(ws, unfold["x"], unfold["y"], settle=0.9)
        check("unfold all opens them again", shut_counts()["subs"] == 0, json.dumps(shut_counts()))

        pr_toggle = clickable(".node [data-toggle]")
        click(ws, pr_toggle["x"], pr_toggle["y"], settle=0.6)
        check("a pull request fold takes hold", shut_counts()["prs"] == 1,
              f"clicked {json.dumps(pr_toggle)}")

        tuck_card = """(() => {
          const card = [...document.querySelectorAll('#lanes .node:not(.pr)')]
            .find((n) => n.querySelector('.number').textContent.includes('#120'))
          if (!card) return null
          const r = card.getBoundingClientRect()
          const b = card.querySelector('.tuck')
          const bb = b.getBoundingClientRect()
          return {
            id: card.dataset.id, height: r.height, top: r.top,
            tucked: card.classList.contains('tucked'),
            opacity: getComputedStyle(b).opacity,
            meta: !!card.querySelector('.meta') && getComputedStyle(card.querySelector('.meta')).display,
            x: r.left + 60, y: r.top + 14,
            tuckX: bb.left + bb.width / 2, tuckY: bb.top + bb.height / 2,
          }
        })()"""
        # The control is pinned to the corner, so anything the row pushes to its
        # right edge lands underneath it. `for context` does exactly that.
        overlap = ws.evaluate("""(() => {
          const bad = []
          for (const card of document.querySelectorAll('#lanes .node:not(.pr)')) {
            const button = card.querySelector('.tuck')
            if (!button) continue
            const b = button.getBoundingClientRect()
            for (const el of card.querySelectorAll('.top > *:not(.tuck)')) {
              const r = el.getBoundingClientRect()
              if (!r.width) continue
              if (r.right > b.left && r.left < b.right && r.bottom > b.top && r.top < b.bottom) {
                bad.push({ card: card.querySelector('.number').textContent.trim(),
                           on: el.className, text: el.textContent.trim().slice(0, 24) })
              }
            }
          }
          return bad
        })()""")
        check("the tuck control sits clear of everything in the row",
              not overlap, json.dumps(overlap))

        resting = ws.evaluate(tuck_card)
        check("an issue card carries a tuck control", bool(resting), json.dumps(resting))
        if resting:
            check("the control is out of sight until you point at the card",
                  resting["opacity"] == "0", f"opacity {resting['opacity']}")
            mouse(ws, "mouseMoved", resting["x"], resting["y"])
            time.sleep(0.4)
            check("pointing at the card brings it out",
                  ws.evaluate(tuck_card)["opacity"] == "1",
                  f"opacity {ws.evaluate(tuck_card)['opacity']}")

            before_tuck = ws.evaluate(STATE)
            click(ws, resting["tuckX"], resting["tuckY"], settle=0.7)
            mouse(ws, "mouseMoved", 1500, 860)
            time.sleep(0.3)
            small = ws.evaluate(tuck_card)
            check("clicking it tucks the card into a line",
                  small["tucked"] and small["height"] < resting["height"] / 2,
                  f"{resting['height']:.0f}px -> {small['height']:.0f}px")
            check("the chips and the reason line go with it", small["meta"] == "none",
                  f"meta display: {small['meta']}")
            check("the way back stays visible on a tucked card",
                  small["opacity"] == "1", f"opacity {small['opacity']}")

            # Tucking takes the subtree with it: an issue you have put aside is
            # not one whose pull requests you still want spread across a column.
            check("tucking takes its pull requests off the board too",
                  ws.evaluate(STATE)["prs"] < before_tuck["prs"],
                  f"{before_tuck['prs']} -> {ws.evaluate(STATE)['prs']} pull request(s)")

            # ...and the line says what went with it, or a tucked parent reads
            # as an issue with nothing attached.
            check("the line says what it carried",
                  ws.evaluate("""(() => {
                    const card = [...document.querySelectorAll('#lanes .node.tucked')][0]
                    const el = card && card.querySelector('.carried')
                    return el ? el.textContent.trim() : null
                  })()""") is not None,
                  str(ws.evaluate("""(() => {
                    const el = document.querySelector('.node.tucked .carried')
                    return el ? el.textContent.trim() : null
                  })()""")))

            # A line set 16px from its neighbours reads as a scattered card
            # rather than as an entry in a list, and gives back the space the
            # tuck just saved.
            gaps = ws.evaluate("""(() => {
              const cards = [...document.querySelectorAll('#lanes .node')].map((n) => {
                const r = n.getBoundingClientRect()
                return { top: r.top, bottom: r.bottom, left: r.left,
                         tucked: n.classList.contains('tucked') }
              }).filter((c) => c.left < 400).sort((a, b) => a.top - b.top)
              const touching = []
              for (let i = 1; i < cards.length; i += 1) {
                if (cards[i].tucked || cards[i - 1].tucked) {
                  touching.push(Math.round(cards[i].top - cards[i - 1].bottom))
                }
              }
              return touching
            })()""")
            check("a tucked card sits close to its neighbours",
                  len(gaps) > 0 and max(gaps) < 12, f"gaps of {gaps}px")

            stored = ws.evaluate("localStorage.getItem('gh-issue-graph:cards')")
            check("the folds are written down", bool(stored) and "issue:" in stored, str(stored))

            reload_board()
            back = shut_counts()
            check("a reload keeps the card tucked", back["tucked"] == 1, json.dumps(back))
            check("and keeps the pull requests folded away", back["prs"] == 1, json.dumps(back))

            unfold = centre(ws, "#unfold-all")
            click(ws, unfold["x"], unfold["y"], settle=0.9)
            after_unfold = shut_counts()
            check("unfold all opens every fold inside the lanes",
                  after_unfold["prs"] == 0 and after_unfold["subs"] == 0, json.dumps(after_unfold))
            check("but leaves a tucked card tucked — that is a different decision",
                  after_unfold["tucked"] == 1, json.dumps(after_unfold))

        check("no javascript errors", not ws.evaluate("(window.__errors || []).length"))

        for line in checks:
            print("  " + line)
        failed = [line for line in checks if line.startswith("FAIL")]
        print(f"\n{len(checks) - len(failed)}/{len(checks)} passed")
        return 1 if failed else 0
    finally:
        for process in (chrome, server):
            if process:
                process.terminate()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
        shutil.rmtree(profile, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
