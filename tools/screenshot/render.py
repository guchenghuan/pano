#!/usr/bin/env python3
"""Render docs/screenshots/json/*.json (produced by `go run ./tools/screenshot`)
into PNG images in docs/screenshots/. Uses Menlo at 2x and downsamples for
crisp edges. No third-party fonts needed; requires Pillow."""

import json
import os
import sys

from PIL import Image, ImageDraw, ImageFont

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.abspath(os.path.join(HERE, "..", ".."))
JSON_DIR = os.path.join(ROOT, "docs", "screenshots", "json")
OUT_DIR = os.path.join(ROOT, "docs", "screenshots")

FONT = "/System/Library/Fonts/Menlo.ttc"
DEFAULT_FG = (211, 215, 224)  # matches pano's textPrimary
DEFAULT_BG = (27, 27, 35)     # deep blue-black, matches the muted palette

# xterm 256-color palette.
PALETTE = [
    (0, 0, 0), (128, 0, 0), (0, 128, 0), (128, 128, 0),
    (0, 0, 128), (128, 0, 128), (0, 128, 128), (192, 192, 192),
    (128, 128, 128), (255, 0, 0), (0, 255, 0), (255, 255, 0),
    (0, 0, 255), (255, 0, 255), (0, 255, 255), (255, 255, 255),
]
_LEVELS = [0, 95, 135, 175, 215, 255]
for r in range(6):
    for g in range(6):
        for b in range(6):
            PALETTE.append((_LEVELS[r], _LEVELS[g], _LEVELS[b]))
for i in range(24):
    v = 8 + 10 * i
    PALETTE.append((v, v, v))


def color(spec, default):
    if spec == "def" or spec is None:
        return default
    if spec.startswith("#"):
        return tuple(int(spec[i:i + 2], 16) for i in (1, 3, 5))
    return PALETTE[int(spec)]


def render(name):
    with open(os.path.join(JSON_DIR, name + ".json")) as f:
        doc = json.load(f)
    cols, rows, cells = doc["cols"], doc["rows"], doc["cells"]

    scale = 2
    font = ImageFont.truetype(FONT, 21 * scale, index=0)
    try:
        bold = ImageFont.truetype(FONT, 21 * scale, index=1)
    except Exception:
        bold = font
    cw = int(font.getlength("M"))
    ascent, descent = font.getmetrics()
    ch = ascent + descent
    pad = 12 * scale

    img = Image.new("RGB", (cols * cw + 2 * pad, rows * ch + 2 * pad), DEFAULT_BG)
    draw = ImageDraw.Draw(img)
    for y, row in enumerate(cells):
        for x, cl in enumerate(row):
            fg = color(cl["fg"], DEFAULT_FG)
            bg = color(cl["bg"], DEFAULT_BG)
            x0, y0 = pad + x * cw, pad + y * ch
            if bg != DEFAULT_BG:
                draw.rectangle([x0, y0, x0 + cw, y0 + ch], fill=bg)
            if cl["c"] != " ":
                draw.text((x0, y0), cl["c"], font=bold if cl.get("b") else font, fill=fg)

    target_w = 1400
    img = img.resize((target_w, int(img.height * target_w / img.width)), Image.LANCZOS)
    out = os.path.join(OUT_DIR, name + ".png")
    img.save(out)
    print("wrote", out, img.size)


def main():
    names = sys.argv[1:] or ["grid", "focus", "notes"]
    for n in names:
        render(n)


if __name__ == "__main__":
    main()
