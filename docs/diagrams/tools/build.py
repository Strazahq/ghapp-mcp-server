#!/usr/bin/env python3
"""Build the five diagrams in docs/diagrams.

    python3 docs/diagrams/tools/build.py

Writes overview.svg, tool-call.svg, git-grant.svg, scope.svg and audit.svg
into docs/diagrams. Edit this file, not the SVGs.

Rules the output keeps:
- One hue per actor, the same in every diagram: Agent machine blue, Straza
  green, ghapp amber, GitHub graphite. Light and dark colours swap with
  prefers-color-scheme inside each SVG.
- System fonts only, because a README image cannot load web fonts.
- CSS keyframes only, no scripts (GitHub strips them). The plain styles are
  the final state, so with prefers-reduced-motion: reduce nothing moves and
  every label shows.
- The build fails if a text colour is below 4.5:1 on a background it sits on.

Stdlib only.
"""
from __future__ import annotations

import pathlib
import sys

OUT = pathlib.Path(__file__).resolve().parent.parent
W = 960

SANS = ('system-ui,-apple-system,"Segoe UI",Roboto,"Noto Sans",'
        '"Helvetica Neue",Arial,sans-serif')
MONO = ('ui-monospace,SFMono-Regular,"SF Mono",Menlo,Consolas,'
        '"Liberation Mono","Noto Sans Mono",monospace')

LIGHT = {
    "bg": "#F5F6F8", "card": "#FFFFFF", "line": "#D7DBE1", "faint": "#C4CAD2",
    "ink": "#1A1D23", "ink2": "#596170",
    "agent": "#2C5F9E", "straza": "#17765A", "ghapp": "#8C5B09",
    "github": "#59616E", "no": "#B1271B",
    "agent-t": "#E2EBF7", "straza-t": "#DCF0E8", "ghapp-t": "#F6EAD3",
    "github-t": "#E8EBEF", "no-t": "#FBE3E0",
}
DARK = {
    "bg": "#14171C", "card": "#1B1F26", "line": "#2E343D", "faint": "#3B424D",
    "ink": "#E6E8EC", "ink2": "#A1A9B5",
    "agent": "#83AEE8", "straza": "#5DC3A0", "ghapp": "#E2AC52",
    "github": "#AEB6C2", "no": "#F08A7E",
    "agent-t": "#1C2B40", "straza-t": "#13322A", "ghapp-t": "#36290F",
    "github-t": "#272C34", "no-t": "#3D1D1A",
}
HUES = ("agent", "straza", "ghapp", "github", "no")

# The four actors, left to right in every diagram: key, name, x, width.
ACTORS = [
    ("agent", "Agent machine", 32, 248),
    ("straza", "Straza", 296, 176),
    ("ghapp", "ghapp", 488, 248),
    ("github", "GitHub", 752, 176),
]
COL = {k: (x, w) for k, _, x, w in ACTORS}
CX = {k: x + w / 2 for k, _, x, w in ACTORS}  # 156, 384, 612, 840


# ---------------------------------------------------------------- contrast

def _lum(h: str) -> float:
    h = h.lstrip("#")
    out = []
    for i in (0, 2, 4):
        c = int(h[i:i + 2], 16) / 255
        out.append(c / 12.92 if c <= 0.03928 else ((c + 0.055) / 1.055) ** 2.4)
    r, g, b = out
    return 0.2126 * r + 0.7152 * g + 0.0722 * b


def ratio(a: str, b: str) -> float:
    la, lb = sorted((_lum(a), _lum(b)), reverse=True)
    return (la + 0.05) / (lb + 0.05)


def check_contrast() -> None:
    bad = []
    for name, p in (("light", LIGHT), ("dark", DARK)):
        pairs = []
        for fg in ("ink", "ink2") + HUES:
            pairs += [(fg, "bg"), (fg, "card")]
        for h in HUES:
            pairs += [("ink", h + "-t"), ("ink2", h + "-t"), (h, h + "-t"), ("card", h)]
        for fg, bg in pairs:
            r = ratio(p[fg], p[bg])
            if r < 4.5:
                bad.append(f"{name}: {fg} on {bg} is {r:.2f}:1")
    if bad:
        sys.exit("contrast below 4.5:1\n" + "\n".join(bad))


# ---------------------------------------------------------------- css

def _vars(p: dict) -> str:
    return "".join(f"--{k}:{v};" for k, v in p.items())


BASE_CSS = (
    f"svg{{font-family:{SANS};{_vars(LIGHT)}}}"
    "@media (prefers-color-scheme:dark){svg{" + _vars(DARK) + "}}"
    "text{white-space:pre}"
    f".m{{font-family:{MONO}}}"
    ".panel{fill:var(--bg);stroke:var(--line);stroke-width:1.5}"
    ".card{fill:var(--card);stroke:var(--line);stroke-width:1.25}"
    ".z16{font-size:16px}.z17{font-size:17px}.z18{font-size:18px}"
    ".z19{font-size:19px}.z20{font-size:20px}.z21{font-size:21px}"
    ".z22{font-size:22px}.z28{font-size:28px}"
    ".w6{font-weight:600}.it{font-style:italic}"
    ".ink{fill:var(--ink)}.ink2{fill:var(--ink2)}.oncard{fill:var(--card)}"
    ".h{paint-order:stroke;stroke:var(--bg);stroke-width:8px;stroke-linejoin:round}"
    ".hc{paint-order:stroke;stroke:var(--card);stroke-width:8px;stroke-linejoin:round}"
    ".trk{fill:none;stroke:var(--faint);stroke-width:2}"
    ".trk10{fill:none;stroke:var(--faint);stroke-width:10;stroke-linecap:round}"
    ".life{fill:none;stroke:var(--faint);stroke-width:2;stroke-linecap:round;stroke-dasharray:0 7}"
    ".ln{fill:none;stroke-width:2.25;stroke-linecap:round;stroke-linejoin:round}"
    ".ln10{fill:none;stroke-width:10;stroke-linecap:round}"
    ".dash{fill:none;stroke:var(--ink2);stroke-width:1.5;stroke-dasharray:5 5}"
    ".frame{fill:none;stroke:var(--line);stroke-width:1.25}"
    ".pk,.o0{opacity:0}.mask{fill:var(--card)}"
    ".pkf{fill:var(--ink)}"
    + "".join(
        f".f-{h}{{fill:var(--{h})}}.s-{h}{{stroke:var(--{h})}}.t-{h}{{fill:var(--{h}-t)}}"
        for h in HUES)
    + ".mk-full{fill:var(--ink2)}.mk-line{fill:none;stroke:var(--ink2);stroke-width:1.5}"
)


# ---------------------------------------------------------------- svg bits

def esc(s: str) -> str:
    return s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")


def n(v: float) -> str:
    return f"{v:g}" if isinstance(v, (int, float)) else str(v)


def cls(*parts) -> str:
    return " ".join(p for p in parts if p)


def T(x, y, parts, c, anchor=None, k=None) -> str:
    """Text. parts is a string, or a list of strings and (string, class)."""
    if isinstance(parts, str):
        parts = [parts]
    body = "".join(
        esc(p) if isinstance(p, str) else f'<tspan class="{p[1]}">{esc(p[0])}</tspan>'
        for p in parts)
    a = f' text-anchor="{anchor}"' if anchor else ""
    return f'<text x="{n(x)}" y="{n(y)}" class="{cls(c, k)}"{a}>{body}</text>'


def R(x, y, w, h, rx, c, k=None) -> str:
    return (f'<rect x="{n(x)}" y="{n(y)}" width="{n(w)}" height="{n(h)}" '
            f'rx="{n(rx)}" class="{cls(c, k)}"/>')


def P(points, c, k=None) -> str:
    d = "M" + " L".join(f"{n(x)} {n(y)}" for x, y in points)
    return f'<path d="{d}" class="{cls(c, k)}"/>'


def plen(points) -> float:
    return sum(abs(x2 - x1) + abs(y2 - y1)
               for (x1, y1), (x2, y2) in zip(points, points[1:]))


def chevron(x, y, direction, hue, k=None) -> str:
    """An open arrow tip whose point is at (x, y)."""
    s = 7
    pts = {
        "r": [(x - s, y - s), (x, y), (x - s, y + s)],
        "l": [(x + s, y - s), (x, y), (x + s, y + s)],
        "d": [(x - s, y - s), (x, y), (x + s, y - s)],
        "u": [(x - s, y + s), (x, y), (x + s, y + s)],
    }[direction]
    return P(pts, f"ln s-{hue}", k)


def g(content, c=None, k=None) -> str:
    c = cls(c, k)
    a = f' class="{c}"' if c else ""
    return f"<g{a}>" + "".join(content) + "</g>"


def badge(cx, cy, num, hue, k=None) -> list[str]:
    """A step number: a filled square in the actor's hue."""
    return [g([R(cx - 13, cy - 13, 26, 26, 5, f"f-{hue}"),
               T(cx, cy + 6, str(num), "m z17 w6 oncard", "middle")], k=k)]


def capsule(cx, y1, y2, hue, k=None) -> str:
    """A station where a lane meets an actor."""
    return R(cx - 8, y1, 16, y2 - y1, 8, f"t-{hue} s-{hue}", k).replace(
        'class="', 'stroke-width="2" class="')


def packet(k) -> str:
    return f'<g class="pk {k}"><rect x="-6" y="-6" width="12" height="12" rx="3" class="pkf"/></g>'


# ---------------------------------------------------------------- diagram

class Diagram:
    def __init__(self, name, height, loop, title, desc):
        self.name, self.h, self.loop = name, height, loop
        self.reset = loop - 0.7
        self.title, self.desc = title, desc
        self.body: list[str] = []
        self.rules: list[str] = []
        self.frames: list[str] = []
        self.count = 0

    def add(self, *items):
        for it in items:
            if isinstance(it, list):
                self.body.extend(it)
            else:
                self.body.append(it)

    def pct(self, t: float) -> str:
        return f"{max(0.0, min(100.0, 100 * t / self.loop)):.2f}".rstrip("0").rstrip(".")

    def anim(self, points, rule=""):
        """Keyframes from (seconds, declarations) pairs. Returns the class."""
        self.count += 1
        k = f"k{self.count}"
        merged: dict[str, str] = {}
        for t, decl in sorted(points, key=lambda p: p[0]):
            merged[self.pct(t)] = decl
        body = "".join(f"{p}%{{{d}}}" for p, d in merged.items())
        self.frames.append(f"@keyframes {k}{{{body}}}")
        self.rules.append(f".{k}{{{rule}animation:{k} {self.loop:g}s ease-in-out infinite}}")
        return k

    def light(self, t, dim=0.25, ramp=0.4):
        """Dim until t, then lit and held until the loop resets."""
        return self.anim([(0, f"opacity:{dim}"), (t, f"opacity:{dim}"),
                          (t + ramp, "opacity:1"), (self.reset, "opacity:1"),
                          (self.loop, f"opacity:{dim}")])

    def states(self, values, times, ramp=0.4):
        """Opacity that steps through values at times, with crossfades.
        values[-1] is the final, held state; the loop fades back to values[0]."""
        pts = [(0, f"opacity:{values[0]}")]
        for i, t in enumerate(times):
            pts += [(t, f"opacity:{values[i]}"), (t + ramp, f"opacity:{values[i + 1]}")]
        pts += [(self.reset, f"opacity:{values[-1]}"), (self.loop, f"opacity:{values[0]}")]
        k = self.anim(pts)
        # The plain style is the held final state, for reduced motion.
        return k if values[-1] == 1 else f"{k} o0"

    def draw(self, points, t0, t1):
        """A line that draws itself from its first point between t0 and t1."""
        L = round(plen(points), 1)
        return self.anim([
            (0, f"stroke-dashoffset:{L:g};opacity:1"), (t0, f"stroke-dashoffset:{L:g};opacity:1"),
            (t1, "stroke-dashoffset:0;opacity:1"), (self.reset, "stroke-dashoffset:0;opacity:1"),
            (self.reset + 0.5, "stroke-dashoffset:0;opacity:0"),
            (self.loop, f"stroke-dashoffset:{L:g};opacity:0")],
            rule=f"stroke-dasharray:{L:g} {L:g};")

    def move(self, pts):
        """A packet through (seconds, x, y) waypoints, hidden otherwise."""
        def at(x, y):
            return f"transform:translate({n(x)}px,{n(y)}px)"
        t0, x0, y0 = pts[0]
        tn, xn, yn = pts[-1]
        frames = [(0, f"opacity:0;{at(x0, y0)}"), (t0 - 0.05, f"opacity:0;{at(x0, y0)}"),
                  (t0, f"opacity:1;{at(x0, y0)}")]
        frames += [(t, f"opacity:1;{at(x, y)}") for t, x, y in pts[1:]]
        frames += [(tn + 0.3, f"opacity:0;{at(xn, yn)}"), (self.loop, f"opacity:0;{at(xn, yn)}")]
        return self.anim(frames)

    # shared furniture

    def heading(self, fig, title, y=62):
        self.add(T(32, y, fig, "m z18 ink2"), T(66, y, title, "z28 w6 ink"))

    def actor_header(self, y, keys=None):
        for k, name, x, w in ACTORS:
            on = keys is None or k in keys
            self.add(g([R(x, y, w, 4, 2, f"f-{k}"),
                        T(x + w / 2, y + 30, name, "z20 w6 ink", "middle")],
                       None if on else None))

    def status(self, y, rows):
        """Status lines: (kind, text) with kind built, building or designed."""
        word = {"built": "Built", "building": "Being built",
                "designed": "Designed, not built"}
        for i, (kind, text) in enumerate(rows):
            yy = y + i * 27
            if kind == "built":
                self.add(R(32, yy - 12, 12, 12, 2, "mk-full"))
            elif kind == "building":
                self.add(R(32.75, yy - 11.25, 10.5, 10.5, 2, "mk-line"),
                         R(32, yy - 12, 6, 12, 1, "mk-full"))
            else:
                self.add(R(32.75, yy - 11.25, 10.5, 10.5, 2, "mk-line"))
            parts = text if isinstance(text, list) else [text]
            self.add(T(54, yy, [(word[kind] + ": ", "w6 ink")] + parts, "z17 ink2"))

    def svg(self) -> str:
        css = (BASE_CSS
               + "@media (prefers-reduced-motion:no-preference){" + "".join(self.rules) + "}"
               + "".join(self.frames)
               + "@media (prefers-reduced-motion:reduce){*{animation:none!important}}")
        tid, did = f"{self.name}-title", f"{self.name}-desc"
        head = (f'<svg xmlns="http://www.w3.org/2000/svg" width="{W}" height="{self.h}" '
                f'viewBox="0 0 {W} {self.h}" role="img" aria-labelledby="{tid} {did}">\n'
                f'<title id="{tid}">{esc(self.title)}</title>\n'
                f'<desc id="{did}">{esc(self.desc)}</desc>\n'
                f"<style>{css}</style>\n"
                f'<rect x="0.75" y="0.75" width="{W - 1.5}" height="{self.h - 1.5}" rx="20" class="panel"/>\n')
        return head + "\n".join(self.body) + "\n</svg>\n"

    def write(self):
        (OUT / f"{self.name}.svg").write_text(self.svg(), encoding="utf-8")


def card(x, y, w, h, hue, name) -> list[str]:
    """An actor card: white panel, a top edge in the actor's hue, its name."""
    r = 14
    cap = (f'<path d="M{n(x)} {n(y + r)} A{r} {r} 0 0 1 {n(x + r)} {n(y)} '
           f'H{n(x + w - r)} A{r} {r} 0 0 1 {n(x + w)} {n(y + r)}" '
           f'class="s-{hue}" fill="none" stroke-width="5"/>')
    return [R(x, y, w, h, r, "card"), cap, T(x + 20, y + 46, name, "z22 w6 ink")]


# ---------------------------------------------------------------- 1 overview

def overview():
    d = Diagram("overview", 752, 10.0, "Who holds what",
                "Four parts, left to right: the agent machine, Straza, ghapp and GitHub. "
                "The agent machine holds the agent, git through Straza's holder and its "
                "Straza session, and never a GitHub token. Straza holds roles, approvals and "
                "audit. ghapp holds the App private key and the GitHub tokens, which stay in "
                "ghapp, and checks the exact name and the repo id. GitHub holds the repos and "
                "its audit log, which shows ghapp[bot] and the token hash. One call goes from "
                "the agent through Straza and ghapp to GitHub and back.")
    d.heading("01", "Who holds what")
    top, bot = 96, 482
    for k, name, x, w in ACTORS:
        d.add(card(x, top, w, bot - top, k, name))

    # Agent machine
    x = 52
    d.add(T(x, 184, "the agent", "z17 ink2"),
          T(x, 211, "joe-macos-agent", "m z19 ink"),
          T(x, 254, "git, through Straza's holder", "z17 ink2"),
          T(x, 281, "straza service", "m z18 ink"),
          T(x, 324, "its Straza session", "z17 ink2"),
          T(x, 351, "straza mcp ghapp", "m z18 ink"),
          T(x, 394, "GitHub token", "z17 ink2"),
          R(50, 406, 212, 54, 8, "dash"),
          T(156, 441, "never", "z22 w6 ink", "middle"))

    # Straza
    x = 316
    k_roles = d.light(1.4, dim=0)
    d.add(R(306, 190, 156, 60, 6, "t-straza", k_roles),
          T(x, 184, "roles", "z17 ink2"),
          T(x, 211, "tools", "z20 ink"),
          T(x, 237, ["+ repos (", ("covers", "m w6"), ")"], "z20 ink"),
          T(x, 300, "approvals", "z17 ink2"),
          T(x, 327, "a person", "z20 ink"),
          T(x, 353, "says yes", "z20 ink"),
          T(x, 396, "audit", "z17 ink2"),
          T(x, 423, "every call", "z20 ink"))

    # ghapp: the key and the tokens sit inside a boundary
    k_key = d.light(2.8, dim=0)
    k_tok = d.light(3.1, dim=0)
    d.add(R(500, 164, 224, 194, 10, "ln s-ghapp"),
          T(516, 170, "stays in ghapp", "z17 w6 f-ghapp hc"),
          R(510, 184, 204, 40, 6, "t-ghapp", k_key),
          T(522, 210, "App private key", "z20 ink"),
          R(510, 232, 204, 114, 6, "t-ghapp", k_tok),
          T(522, 259, "GitHub tokens", "z20 ink"),
          T(522, 285, "one repo, one agent,", "z17 ink2"),
          T(522, 309, "only the tool's", "z17 ink2"),
          T(522, 333, "permissions", "z17 ink2"),
          T(508, 398, "checks", "z17 ink2"),
          T(508, 425, "exact name", "z20 ink"),
          T(508, 451, "repo id matches", "z20 ink"))

    # GitHub
    x = 772
    k_repos = d.light(4.2, dim=0)
    d.add(R(762, 190, 156, 60, 6, "t-github", k_repos),
          T(x, 184, "repos", "z17 ink2"),
          T(x, 211, "the code,", "z20 ink"),
          T(x, 237, "PRs, issues", "z20 ink"),
          T(x, 300, "audit log", "z17 ink2"),
          T(x, 327, "ghapp[bot]", "m z18 ink"),
          T(x, 353, "+ token hash", "z19 ink"))

    # The call lane: out on top, back below.
    yo, yb = 556, 584
    order = ["agent", "straza", "ghapp", "github"]
    for k in order:
        d.add(P([(CX[k], bot), (CX[k], 540)], "life"))
    d.add(P([(CX["agent"], yo), (CX["github"], yo)], "trk"),
          P([(CX["agent"], yb), (CX["github"], yb)], "trk"))
    t_arrive = {"agent": 0.2, "straza": 1.4, "ghapp": 2.7, "github": 4.2}
    out_times = [(0.5, 1.4), (1.8, 2.7), (3.3, 4.2)]
    for (a, b), (t0, t1) in zip(zip(order, order[1:]), out_times):
        pts = [(CX[a] + 9, yo), (CX[b] - 9, yo)]
        d.add(P(pts, f"ln s-{a}", d.draw(pts, t0, t1)),
              chevron(CX[b] - 10, yo, "r", a, d.light(t1 - 0.1, dim=0)))
    back = list(reversed(order))
    back_times = [(4.5, 5.4), (5.4, 6.3), (6.3, 7.2)]
    for (a, b), (t0, t1) in zip(zip(back, back[1:]), back_times):
        pts = [(CX[a] - 9, yb), (CX[b] + 9, yb)]
        d.add(P(pts, f"ln s-{a}", d.draw(pts, t0, t1)),
              chevron(CX[b] + 10, yb, "l", a, d.light(t1 - 0.1, dim=0)))
    for k in order:
        d.add(capsule(CX[k], 540, 600, k, d.light(t_arrive[k], dim=0.3)))
    d.add(T(270, 544, "pr_create", "m z17 ink", "middle"),
          T(498, 544, "allowed", "z17 ink2", "middle"),
          T(726, 544, "API call", "z17 ink2", "middle"),
          T(498, 622, "the result comes back the same way", "z17 ink2", "middle"))
    d.add(packet(d.move([(0.5, CX["agent"], yo), (1.4, CX["straza"], yo),
                         (1.8, CX["straza"], yo), (2.7, CX["ghapp"], yo),
                         (3.3, CX["ghapp"], yo), (4.2, CX["github"], yo),
                         (4.5, CX["github"], yb), (5.4, CX["ghapp"], yb),
                         (6.3, CX["straza"], yb), (7.2, CX["agent"], yb)])))

    d.status(672, [
        ("built", "ghapp's tools and checks, who may call ghapp, its git endpoint, one token per agent."),
        ("built", "Straza's role field covers, for every repo or named repos."),
    ])
    d.write()


# ---------------------------------------------------------------- 2 tool call

def tool_call():
    d = Diagram("tool-call", 1038, 12.0, "One tool call",
                "joe-macos-agent calls pr_create on Strazahq/pay-api. Step 1, Straza: a role "
                "covers the repo. Step 2, ghapp: who is calling, a Keycloak token from a listed "
                "client. Step 3, ghapp: the name is exact and the id matches. Step 4, ghapp: a "
                "token for repo 812345678 with pull_requests write and contents read. Step 5, "
                "GitHub: the pull "
                "request is created, as ghapp[bot]. The result comes back the same way. Then the "
                "same agent calls pr_create on Strazahq/straza, and Straza refuses: none of your "
                "roles on ghapp covers that repo. ghapp and GitHub never see that call.")
    d.heading("02", "One tool call")
    d.add(T(32, 104, [("joe-macos-agent", "m"), " calls ", ("pr_create", "m"), " on ",
                      ("Strazahq/pay-api", "m")], "z20 ink"))
    d.actor_header(128)
    life_top, life_bot = 176, 730
    for k, *_ in ACTORS:
        d.add(P([(CX[k], life_top), (CX[k], life_bot)], "life"))

    ya1, ys1 = 222, 222
    ya2 = 306
    ys3, ys4 = 396, 486
    ya5 = 602
    yr = 702
    A, S, G, H = CX["agent"], CX["straza"], CX["ghapp"], CX["github"]

    # Step lines, each drawn as the packet passes.
    segs = [
        ([(A + 6, ya1), (S - 14, ya1)], "agent", 0.4, 1.1, ("r", S - 15, ya1)),
        ([(S, ys1 + 13), (S, ya2)], "straza", 1.5, 1.9, None),
        ([(S, ya2), (G - 14, ya2)], "straza", 1.9, 2.6, ("r", G - 15, ya2)),
        ([(G, ya2 + 13), (G, ys3 - 13)], "ghapp", 3.1, 3.5, None),
        ([(G, ys3 + 13), (G, ys4 - 13)], "ghapp", 4.0, 4.4, None),
        ([(G, ys4 + 13), (G, ya5)], "ghapp", 4.9, 5.2, None),
        ([(G, ya5), (H - 14, ya5)], "ghapp", 5.2, 5.9, ("r", H - 15, ya5)),
        ([(H, ya5 + 13), (H, yr), (A + 2, yr)], "github", 6.3, 7.7, ("l", A + 1, yr)),
    ]
    for pts, hue, t0, t1, tip in segs:
        d.add(P(pts, f"ln s-{hue}", d.draw(pts, t0, t1)))
        if tip:
            d.add(chevron(tip[1], tip[2], tip[0], hue, d.light(t1 - 0.1, dim=0)))
    d.add(f'<circle cx="{n(A)}" cy="{ya1}" r="6" class="f-agent {d.light(0.2)}"/>')
    d.add(T(270, ya1 - 12, "pr_create", "m z18 ink h", "middle"))

    # The five steps.
    steps = [
        (S, ys1, 1, "straza", 1.1, [
            ("a role covers the repo", "z22 ink"),
            ([("ghapp-write-payments", "m"), ", where team is payments"], "z19 ink2")]),
        (G, ya2, 2, "ghapp", 2.6, [
            ("who is calling", "z22 ink"),
            ("a Keycloak token", "z19 ink2"),
            ("from a listed client", "z19 ink2")]),
        (G, ys3, 3, "ghapp", 3.5, [
            ("exact name, same id", "z22 ink"),
            ("Strazahq/pay-api", "m z19 ink2"),
            ("id 812345678", "m z19 ink2")]),
        (G, ys4, 4, "ghapp", 4.4, [
            ("a token for one repo", "z22 ink"),
            ("repo 812345678", "m z19 ink2"),
            ("pull_requests: write", "m z19 ink2"),
            ("contents: read", "m z19 ink2")]),
    ]
    for x, y, num, hue, t, lines in steps:
        k = d.light(t)
        items = badge(x, y, num, hue)
        for i, (txt, c) in enumerate(lines):
            items.append(T(x + 26, y + 8 + i * 26, txt, c + " h"))
        d.add(g(items, k=k))
    k5 = d.light(5.9)
    d.add(g(badge(H, ya5, 5, "github")
            + [T(H - 18, ya5 + 44, "PR created", "z22 ink h", "end"),
               T(H - 18, ya5 + 70, "as ghapp[bot]", "m z19 ink2 h", "end")], k=k5))
    d.add(T(400, yr - 12, "the result comes back the same way", "z19 ink2 h", "middle"))
    d.add(packet(d.move([(0.4, A, ya1), (1.1, S, ya1), (1.5, S, ya1), (1.9, S, ya2),
                         (2.6, G, ya2), (3.1, G, ya2), (3.5, G, ys3), (4.0, G, ys3),
                         (4.4, G, ys4), (4.9, G, ys4), (5.2, G, ya5), (5.9, H, ya5),
                         (6.3, H, ya5), (6.6, H, yr), (7.7, A, yr)])))

    # Second pass: another repo, refused at Straza.
    yd = 768
    d.add(P([(32, yd), (928, yd)], "trk"),
          T(32, yd + 42, "Same agent, another repo", "z20 w6 ink"))
    y6 = yd + 100
    d.add(P([(A, y6 - 30), (A, y6 + 30)], "life"), P([(S, y6 - 30), (S, y6 + 30)], "life"))
    pts = [(A + 6, y6), (S - 14, y6)]
    d.add(P(pts, "ln s-agent", d.draw(pts, 8.2, 8.9)),
          chevron(S - 15, y6, "r", "agent", d.light(8.8, dim=0)),
          f'<circle cx="{n(A)}" cy="{y6}" r="6" class="f-agent {d.light(8.0)}"/>',
          T(270, y6 - 12, "pr_create", "m z18 ink h", "middle"),
          T(270, y6 + 28, "Strazahq/straza", "m z18 ink h", "middle"))
    kno = d.light(8.9)
    d.add(g([R(S - 13, y6 - 13, 26, 26, 5, "t-no s-no").replace('class="', 'stroke-width="2" class="'),
             P([(S - 6, y6 - 6), (S + 6, y6 + 6)], "ln s-no"),
             P([(S - 6, y6 + 6), (S + 6, y6 - 6)], "ln s-no"),
             T(S + 26, y6 + 8, "“None of your roles on ghapp", "z22 ink"),
             T(S + 26, y6 + 36, "covers that repo.”", "z22 ink"),
             T(S + 26, y6 + 64, "ghapp and GitHub never see the call.", "z19 ink2")], k=kno))
    d.add(packet(d.move([(8.2, A, y6), (8.9, S, y6)])))

    d.status(988, [
        ("built", "every step. Straza checks the repo in step 1 through the role field covers."),
    ])
    d.write()


# ---------------------------------------------------------------- 4 scope

def scope():
    d = Diagram("scope", 912, 12.0, "What a role covers",
                "The Straza role ghapp-write-payments holds pr_create, pr_merge and "
                "git_push. Its field covers takes one of three forms. every covers all nine "
                "repos. objects: [Strazahq/pay-api] covers one. where: {team: [payments]} covers "
                "the three repos whose team is payments: pay-api, pay-ledger and refunds. The team "
                "comes from a GitHub custom property, which ghapp's repo_list hands to Straza. "
                "midPoint imports the role as AR:ghapp-write-payments with the covers line "
                "repo where team is payments, the rule in words.")
    d.heading("04", "What a role covers")
    d.add(T(32, 104, ["A Straza role holds tools. Its field ", ("covers", "m w6"),
                      " says which repos."], "z20 ink"))
    t_b, t_c = 3.6, 7.2  # every until t_b, then objects, then where (held)

    # The role.
    x0, top, w = 32, 132, 420
    d.add(card(x0, top, w, 488, "straza", "Straza role"))
    x = 52
    d.add(T(x, 218, "name", "z17 ink2"),
          T(x, 246, "ghapp-write-payments", "m z21 ink"),
          T(x, 290, "tools", "z17 ink2"),
          T(x, 317, "pr_create", "m z19 ink"),
          T(x, 343, "pr_merge", "m z19 ink"),
          T(x, 369, "git_push", "m z19 ink"),
          T(x, 412, ["", ("covers", "m w6 ink"), ", one of three forms"], "z17 ink2"))
    forms = [
        ("where: {team: [payments]}", "3 repos, by their team", [0, 0, 1]),
        ("every", "all 9 repos", [1, 0, 0]),
        ("objects: [Strazahq/pay-api]", "1 repo, by name", [0, 1, 0]),
    ]
    for i, (code, note, vals) in enumerate(forms):
        y = 426 + i * 62
        k = d.states(vals, [t_b, t_c])
        d.add(g([R(44, y, 396, 56, 8, "t-straza"), R(44, y + 8, 4, 40, 2, "f-straza")], k=k),
              T(62, y + 25, code, "m z18 ink"),
              T(62, y + 47, note, "z17 ink2"))

    # The repos.
    repos = [("pay-api", "payments"), ("straza", "platform"), ("straza-site", "web"),
             ("console", "platform"), ("pay-ledger", "payments"), ("docs", "web"),
             ("infra", "platform"), ("mobile", "mobile"), ("refunds", "payments")]
    gx, gy, tw, th, gap = 484, 166, 140, 96, 12
    d.add(T(gx, 150, "repos in Strazahq, with their team", "z17 ink2"))
    for i, (name, team) in enumerate(repos):
        x = gx + (i % 3) * (tw + gap)
        y = gy + (i // 3) * (th + gap)
        if name == "pay-api":
            vals = None
        elif team == "payments":
            vals = [1, 0, 1]
        else:
            vals = [1, 0, 0]
        lit = R(x, y, tw, th, 10, "t-straza s-straza").replace('class="', 'stroke-width="2" class="')
        if vals is None:
            d.add(R(x, y, tw, th, 10, "card"), lit)
        else:
            d.add(R(x, y, tw, th, 10, "card"), g([lit], k=d.states(vals, [t_b, t_c])))
        d.add(T(x + 14, y + 40, name, "m z18 ink"), T(x + 14, y + 70, team, "z17 ink2"))
    d.add(T(gx, 514, ["team is a GitHub custom property. ghapp's"], "z17 ink2"),
          T(gx, 538, [("repo_list", "m"), " hands it to Straza with each repo."], "z17 ink2"))

    # midPoint.
    my = 668
    d.add(P([(242, 620), (242, my - 2)], "ln", None).replace('class="ln"', 'class="ln" style="stroke:var(--ink2)"'),
          P([(235, my - 9), (242, my - 2), (249, my - 9)], "ln").replace('class="ln"', 'class="ln" style="stroke:var(--ink2)"'),
          T(258, 650, "imported as a role", "z17 ink2"),
          R(32, my, 896, 148, 14, "card"),
          T(52, my + 42, "midPoint", "z22 w6 ink"),
          T(52, my + 78, "role", "z17 ink2"),
          T(52, my + 106, "AR:ghapp-write-payments", "m z20 ink"),
          T(400, my + 78, "covers", "z17 ink2"))
    d.add(T(400, my + 106, "repo where team is payments", "z20 ink", k=d.states([0.25, 0, 1], [t_b, t_c])),
          g([R(392, my + 84, 260, 32, 6, "mask"),
             T(400, my + 106, ["repo ", ("Strazahq/pay-api", "m")], "z20 ink")],
            k=d.states([0, 1, 0], [t_b, t_c])))
    d.add(T(712, my + 78, "the rule in words.", "z17 ink2"),
          T(712, my + 102, "The repo list stays", "z17 ink2"),
          T(712, my + 126, "in Straza.", "z17 ink2"))

    d.status(860, [
        ("built", [("repo_list", "m"), " in ghapp, Straza's field ", ("covers", "m"), " with every and objects, its check, and the line in midPoint."]),
        ("designed", [("where", "m"), ", covering repos by their team, in Straza."]),
    ])
    d.write()


# ---------------------------------------------------------------- 5 audit

def audit():
    d = Diagram("audit", 904, 10.0, "Following one push",
                "Three log lines for one push. Straza's audit: agent joe-macos-agent, tool "
                "git_push, repo Strazahq/pay-api, allowed. ghapp's log: agent "
                "joe-macos-agent, ref refs/heads/fix-login, repo Strazahq/pay-api, token hash. "
                "GitHub's audit log: actor ghapp[bot], action git.push, repo Strazahq/pay-api, "
                "hashed_token. The hash in GitHub's line matches the hash in ghapp's line, and "
                "that line names the agent, which matches Straza's line.")
    d.heading("05", "Following one push")
    d.add(T(32, 104, ["GitHub shows ", ("ghapp[bot]", "m"),
                      " for every agent. The token hash leads back to one."], "z20 ink"))

    def log_card(y, hue, title, time, fields, k):
        r, h = 14, 150
        edge = (f'<path d="M{32 + r} {y} A{r} {r} 0 0 0 32 {y + r} V{y + h - r} '
                f'A{r} {r} 0 0 0 {32 + r} {y + h}" class="s-{hue}" fill="none" stroke-width="5"/>')
        items = [R(32, y, 896, h, r, "card"), edge,
                 T(60, y + 42, title, "z20 w6 ink"),
                 T(900, y + 42, time, "m z18 ink2", "end")]
        for (lx, vx, row), (label, value, lab_mono) in zip(
                [(60, 170, 0), (500, 640, 0), (60, 170, 1), (500, 640, 1)], fields):
            yy = y + 88 + row * 38
            items.append(T(lx, yy, label, ("m " if lab_mono else "") + "z17 ink2"))
            items.append(T(vx, yy, value, "m z20 ink"))
        d.add(g(items, k=k))

    def mark(x, y, text, hue, t):
        """Highlight a value: a tint with the value drawn again on top, lit at t."""
        w = len(text) * 12 + 12
        box = R(x - 6, y - 22, w, 31, 6, f"t-{hue} s-{hue}").replace(
            'class="', 'stroke-width="1.5" class="')
        d.add(g([box, T(x, y, text, "m z20 ink")], k=d.light(t, dim=0)))

    ys, yg, yh = 132, 342, 552
    hash_ = "Xk2d…Qp8="
    log_card(ys, "straza", "Straza audit", "10:42:07", [
        ("agent", "joe-macos-agent", False), ("tool", "git_push", False),
        ("repo", "Strazahq/pay-api", False), ("decision", "allowed", False)], d.light(4.0, dim=0.35))
    log_card(yg, "ghapp", "ghapp log", "10:42:09", [
        ("agent", "joe-macos-agent", False), ("ref", "refs/heads/fix-login", False),
        ("repo", "Strazahq/pay-api", False), ("token hash", hash_, False)], d.light(2.1, dim=0.35))
    log_card(yh, "github", "GitHub audit log", "10:42:10", [
        ("actor", "ghapp[bot]", True), ("action", "git.push", True),
        ("repo", "Strazahq/pay-api", True), ("hashed_token", hash_, True)], d.light(0.3, dim=0.35))
    d.add(T(306, yh + 88, "for every agent", "z17 it ink2"))

    # The links, read from the bottom up.
    hx = 640 + len(hash_) * 6
    ax = 170 + len("joe-macos-agent") * 6
    for x, y1, y2, hue, t0, t1, label in [
        (hx, yh, yg + 150, "ghapp", 1.4, 2.1, "same hash"),
        (ax, yg, ys + 150, "agent", 3.3, 4.0, "same agent"),
    ]:
        pts = [(x, y1 - 2), (x, y2 + 3)]
        d.add(P(pts, f"ln s-{hue}", d.draw(pts, t0, t1)),
              chevron(x, y2 + 3, "u", hue, d.light(t1 - 0.1, dim=0)),
              T(x + 16, (y1 + y2) / 2 + 6, label, "z17 ink2", k=d.light(t1 - 0.2, dim=0.2)))
    for x, y, text, hue, t in [
        (640, yh + 126, hash_, "ghapp", 0.9),
        (640, yg + 126, hash_, "ghapp", 2.2),
        (170, yg + 88, "joe-macos-agent", "agent", 2.8),
        (170, ys + 88, "joe-macos-agent", "agent", 4.2),
        (640, ys + 126, "allowed", "straza", 4.7),
    ]:
        mark(x, y, text, hue, t)

    d.add(T(32, 750, ["Read it from the bottom. GitHub's event names only ",
                      ("ghapp[bot]", "m"), "."], "z19 ink"),
          T(32, 777, ["Its ", ("hashed_token", "m"),
                      " finds one line in ghapp's log, and that line names the agent."], "z19 ink"))
    d.status(824, [
        ("built", "ghapp's log: each token made, with its hash, and each git operation."),
        ("built", "the repo in Straza's line, through the role field covers."),
    ])
    d.write()


# ---------------------------------------------------------------- 3 git through the holder

def git_grant():
    d = Diagram("git-grant", 1140, 13.0, "git push through Straza's holder",
                "1: the agent types plain git push origin "
                "fix-login, and one git setting sends it to Straza's holder, a service on the same "
                "machine under its own OS account. 2: the holder reads the update before any code "
                "moves: refs/heads/fix-login from a1b2c3d to e4f5a6b. 3: the holder asks Straza for "
                "a grant; Straza checks that a role holds git_push and covers the repo, and a "
                "person may approve, seeing the branch and the commits. Straza returns a grant: "
                "signed, 5 minutes, this branch and these commits, bound to the holder's key. 4: "
                "the holder sends the push to ghapp with the grant and a proof signed by its key, "
                "not through Straza; ghapp checks Straza's signature, the proof, and that the push "
                "is exactly what was granted, then makes a token for one repo with contents write, "
                "which stays in ghapp. 5: GitHub takes the pack and answers per ref, and git prints "
                "it. The agent can read nothing that opens ghapp or GitHub: a copied grant is "
                "useless without the holder's key.")
    d.heading("03", "git push through Straza's holder")
    d.add(T(32, 104, ["The agent types ", ("git push origin fix-login", "m"),
                      ". No token, no tool call."], "z20 ink"))
    d.actor_header(128)
    A, S, G, H = CX["agent"], CX["straza"], CX["ghapp"], CX["github"]
    life_top, life_bot = 176, 940
    for k, *_ in ACTORS:
        d.add(P([(CX[k], life_top), (CX[k], life_bot)], "life"))

    y1, y2 = 222, 330
    ya, ys = 450, 450        # holder asks Straza, Straza's step
    yg = 600                 # Straza's grant back to the holder
    yp = 712                 # the push to ghapp, ghapp's step
    yh, yr = 852, 930        # ghapp to GitHub, the answer back

    # 1 and 2, on the agent machine.
    k1 = d.light(0.6)
    d.add(f'<circle cx="{n(A)}" cy="{y1}" r="6" class="f-agent {d.light(0.2)}"/>')
    d.add(g(badge(A, y1, 1, "agent")
            + [T(A + 26, y1 + 8, "plain git", "z22 ink h"),
               T(A + 26, y1 + 34, "to the holder,", "z19 ink2 h"),
               T(A + 26, y1 + 60, "its own OS account", "z19 ink2 h")], k=k1))
    d.add(P([(A, y1 + 13), (A, y2 - 13)], "ln s-agent", d.draw([(A, y1 + 13), (A, y2 - 13)], 1.0, 1.4)))
    k2 = d.light(1.4)
    d.add(g(badge(A, y2, 2, "agent")
            + [T(A + 26, y2 + 8, "reads the update", "z22 ink h"),
               T(A + 26, y2 + 34, "refs/heads/fix-login", "m z17 ink2 h"),
               T(A + 26, y2 + 58, "a1b2c3d..e4f5a6b", "m z17 ink2 h"),
               T(A + 26, y2 + 84, "before any code moves", "z17 ink2 h")], k=k2))

    # 3, the grant from Straza.
    pts = [(A, y2 + 13), (A, ya), (S - 14, ya)]
    d.add(P(pts, "ln s-agent", d.draw(pts, 2.0, 2.8)),
          chevron(S - 15, ya, "r", "agent", d.light(2.7, dim=0)),
          T(270, ya - 12, "asks for a grant", "z17 ink2 h", "middle"))
    k3 = d.light(2.8)
    d.add(g(badge(S, ys, 3, "straza")
            + [T(S + 26, ys + 8, "a role holds git_push", "z22 ink h"),
               T(S + 26, ys + 34, "and covers the repo", "z19 ink2 h")], k=k3))
    k_ap = d.light(3.4)
    d.add(g([R(S + 16, ys + 52, 190, 82, 8, "dash"),
             T(S + 30, ys + 78, "optional:", "z17 w6 ink"),
             T(S + 30, ys + 101, "a person approves,", "z17 ink2"),
             T(S + 30, ys + 124, "sees the commits", "z17 ink2")], k=k_ap))
    pts = [(S, ys + 13), (S, yg), (A + 2, yg)]
    d.add(P(pts, "ln s-straza", d.draw(pts, 4.2, 5.0)),
          chevron(A + 1, yg, "l", "straza", d.light(4.9, dim=0)))
    k_gr = d.light(5.0)
    d.add(g([R(A + 14, yg + 14, 270, 64, 8, "t-straza s-straza").replace('class="', 'stroke-width="1.5" class="'),
             T(A + 28, yg + 38, "grant: signed, 5 minutes", "z17 w6 ink"),
             T(A + 28, yg + 62, "this branch, these commits", "z17 ink2")], k=k_gr))

    # 4, the push to ghapp, not through Straza.
    pts = [(A, yg + 2), (A, yp)]
    d.add(P(pts, "ln s-agent", d.draw(pts, 5.6, 5.9)))
    pts = [(A, yp), (G - 14, yp)]
    d.add(P(pts, "ln10 s-agent", d.draw(pts, 5.9, 6.6)),
          chevron(G - 15, yp, "r", "agent", d.light(6.5, dim=0)),
          T(S, yp - 14, "not through Straza", "z17 it ink2 h", "middle"),
          T(S, yp + 28, "grant + proof by the holder's key", "z17 ink2 h", "middle"))
    k4 = d.light(6.6)
    d.add(g(badge(G, yp, 4, "ghapp")
            + [T(G + 26, yp + 8, "checks", "z22 ink h"),
               T(G + 26, yp + 34, "Straza's signature", "z17 ink2 h"),
               T(G + 26, yp + 58, "the holder's proof", "z17 ink2 h"),
               T(G + 26, yp + 82, "push = the grant", "z17 ink2 h")], k=k4))

    # 5, GitHub, and the answer back.
    pts = [(G, yp + 13), (G, yh)]
    d.add(P(pts, "ln s-ghapp", d.draw(pts, 7.4, 7.7)))
    pts = [(G, yh), (H - 14, yh)]
    d.add(P(pts, "ln10 s-ghapp", d.draw(pts, 7.7, 8.2)),
          chevron(H - 15, yh, "r", "ghapp", d.light(8.1, dim=0)))
    k_tok = d.light(7.4)
    d.add(g([R(G - 114, yh + 20, 228, 36, 8, "t-ghapp s-ghapp").replace('class="', 'stroke-width="1.5" class="'),
             T(G, yh + 44, "GitHub token stays here", "z16 w6 ink", "middle")], k=k_tok))
    k5 = d.light(8.2)
    d.add(g(badge(H, yh, 5, "github")
            + [T(H - 18, yh - 26, "takes the pack", "z19 ink h", "end")], k=k5))
    pts = [(H, yh + 13), (H, yr + 6), (A + 2, yr + 6)]
    d.add(P(pts, "ln s-github", d.draw(pts, 8.8, 9.8)),
          chevron(A + 1, yr + 6, "l", "github", d.light(9.7, dim=0)),
          T(400, yr - 6, "git prints the answer per ref", "z17 ink2 h", "middle"))

    d.add(packet(d.move([(0.2, A, y1), (1.0, A, y1), (1.4, A, y2), (2.0, A, y2), (2.4, A, ya),
                         (2.8, S, ya), (4.2, S, ya), (4.6, S, yg), (5.0, A, yg), (5.6, A, yg),
                         (6.0, A, yp), (6.6, G, yp), (7.4, G, yp), (7.8, G, yh), (8.2, H, yh),
                         (8.8, H, yh), (9.0, H, yr + 6), (9.8, A, yr + 6)])))

    # What the agent can read.
    yb = 980
    k_end = d.light(10.2)
    d.add(g([R(32, yb, 896, 72, 12, "card"),
             T(52, yb + 30, "The agent can read nothing that opens ghapp or GitHub.", "z19 w6 ink"),
             T(52, yb + 56, "A grant copied off the machine is useless without the holder's key.", "z17 ink2")], k=k_end))

    d.status(1092, [
        ("built", "ghapp's checks of the grant and the proof, and Straza's grants and holder."),
        ("built", "Run through ghapp on Linux, Windows and macOS."),
    ])
    d.write()


if __name__ == "__main__":
    check_contrast()
    for build in (overview, tool_call, git_grant, scope, audit):
        build()
    print("wrote", ", ".join(sorted(p.name for p in OUT.glob("*.svg"))))
