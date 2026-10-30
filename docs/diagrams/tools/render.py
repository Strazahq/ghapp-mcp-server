#!/usr/bin/env python3
"""Render the diagrams to PNG so a person can look at them.

    python3 docs/diagrams/tools/render.py OUT_DIR [name ...]

For each SVG in docs/diagrams (or the names given) it:
1. parses the file as XML and fails if it is not well formed, or if it holds
   a script;
2. with Playwright, pauses every CSS animation at several points in the loop
   and screenshots at 960 and 400 px wide, light and dark, plus the
   reduced-motion state;
3. with the plain headless Chromium CLI, loads the SVG as an <img>, as a
   README does, and screenshots with --virtual-time-budget, light and dark.
   Virtual time only nudges CSS animations forward, so these frames show
   the image path works, not exact loop points.

Writes OUT_DIR/<name>-<theme>-<width>-<frame>.png. Keep OUT_DIR outside the
repository. Needs Python Playwright and its Chromium.
"""
from __future__ import annotations

import glob
import os
import pathlib
import re
import subprocess
import sys
import xml.etree.ElementTree as ET

HERE = pathlib.Path(__file__).resolve().parent
SRC = HERE.parent
FRACTIONS = (0.12, 0.3, 0.5, 0.7)


def check_xml(path: pathlib.Path) -> float:
    root = ET.parse(path).getroot()
    if any(el.tag.endswith("script") for el in root.iter()):
        sys.exit(f"{path.name}: holds a script")
    m = re.search(r"animation:k\d+ ([\d.]+)s", path.read_text())
    return float(m.group(1)) if m else 0.0


def headless_shell() -> str | None:
    found = sorted(glob.glob(os.path.expanduser(
        "~/.cache/ms-playwright/chromium_headless_shell-*/chrome-headless-shell-linux64/chrome-headless-shell")))
    return found[-1] if found else None


def page_html(svg: str, width: int) -> str:
    svg = re.sub(r'width="\d+" height="\d+"',
                 f'style="display:block;width:{width}px;height:auto"', svg, count=1)
    return f'<!doctype html><html><body style="margin:0;background:#888">{svg}</body></html>'


def render_frames(names, out: pathlib.Path) -> None:
    from playwright.sync_api import sync_playwright
    with sync_playwright() as p:
        browser = p.chromium.launch()
        for name in names:
            path = SRC / f"{name}.svg"
            loop = check_xml(path)
            svg = path.read_text()
            frames = [(f"t{f * loop:04.1f}s", f * loop * 1000) for f in FRACTIONS]
            frames.append(("final", (loop - 1.0) * 1000))
            for theme in ("light", "dark"):
                for width in (960, 400):
                    for motion in ("no-preference", "reduce"):
                        page = browser.new_page(viewport={"width": width, "height": 600},
                                                color_scheme=theme, reduced_motion=motion)
                        page.set_content(page_html(svg, width))
                        shots = frames if motion == "no-preference" else [("reduced", None)]
                        for label, ms in shots:
                            if ms is not None:
                                page.evaluate(
                                    "t => document.getAnimations().forEach(a => { a.pause(); a.currentTime = t; })", ms)
                            page.screenshot(path=str(out / f"{name}-{theme}-{width}-{label}.png"),
                                            full_page=True)
                        page.close()
            print(f"{name}: XML ok, loop {loop:g}s, frames written")
        browser.close()


def render_img(names, out: pathlib.Path) -> None:
    shell = headless_shell()
    if not shell:
        print("no chrome-headless-shell found, skipping the <img> pass")
        return
    for name in names:
        path = SRC / f"{name}.svg"
        h = int(re.search(r'height="(\d+)"', path.read_text()).group(1))
        for width in (960, 400):
            wrapper = out / f"_img-{name}-{width}.html"
            wrapper.write_text(f'<!doctype html><body style="margin:0;background:#888">'
                               f'<img src="{path.as_uri()}" style="width:{width}px;display:block"></body>')
            height = round(h * width / 960)
            for theme, flag in (("light", "preferredColorScheme=1"), ("dark", "preferredColorScheme=0")):
                for budget in (2000, 30000):
                    png = out / f"{name}-{theme}-{width}-img-vt{budget}.png"
                    subprocess.run([shell, "--headless", "--disable-gpu", "--hide-scrollbars",
                                    "--run-all-compositor-stages-before-draw",
                                    f"--blink-settings={flag}",
                                    f"--window-size={width},{height}",
                                    f"--virtual-time-budget={budget}",
                                    f"--screenshot={png}", wrapper.as_uri()],
                                   check=True, capture_output=True)
            wrapper.unlink()
        print(f"{name}: <img> pass written")


def main() -> None:
    if len(sys.argv) < 2:
        sys.exit(__doc__)
    out = pathlib.Path(sys.argv[1]).resolve()
    out.mkdir(parents=True, exist_ok=True)
    names = sys.argv[2:] or sorted(p.stem for p in SRC.glob("*.svg"))
    render_frames(names, out)
    render_img(names, out)


if __name__ == "__main__":
    main()
