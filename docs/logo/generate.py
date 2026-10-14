#!/usr/bin/env python3
"""Generate every IBS logo asset from one definition.

The logo is a round mission patch: a blue planet below the band of the Milky
Way, with one bright star, two retro stripes (red and yellow) arcing over the
planet, and the name around the rim. Run from the repository root:

    python3 docs/logo/generate.py && rsvg-convert -w 1440 docs/logo/preview.svg -o docs/logo/preview.png

Writes docs/logo/*.svg, web/public/favicon.svg and web/src/logo-paths.ts:

    ibs-patch.svg  the full patch with "INTERGALACTIC BACKUP SYSTEM" around the
                   rim, for large places (the README)
    ibs-mark.svg   the badge without the rim text, for the web interface
    favicon.svg    a simpler badge for 32 px and below (the browser tab)
"""
import math
import random

ID = "ibsg-"  # prefix of gradient and clip ids; the web interface makes it unique per instance

BLUE, NAVY, RED, YELLOW, WHITE, SKY = "#2456d6", "#0d1b4d", "#e3342f", "#f7c31a", "#ffffff", "#7fb2ff"
FONT = 'font-family="Helvetica Neue, Helvetica, Arial, sans-serif" font-weight="800"'


def stars(n, inside, seed, size):
    """n stars at random spots where inside(x, y), the same every run."""
    rnd, out = random.Random(seed), []
    while len(out) < n:
        x, y = rnd.uniform(4, 96), rnd.uniform(4, 96)
        if inside(x, y):
            out.append(f'<circle cx="{x:.1f}" cy="{y:.1f}" r="{rnd.uniform(*size):.2f}" fill="{WHITE}" opacity="{rnd.uniform(.55, 1):.2f}"/>')
    return "".join(out)


def star4(x, y, s):
    """The bright four-pointed star."""
    p = [(x, y - s), (x + s * .28, y - s * .28), (x + s, y), (x + s * .28, y + s * .28),
         (x, y + s), (x - s * .28, y + s * .28), (x - s, y), (x - s * .28, y - s * .28)]
    return f'<path d="M{"L".join(f"{a:.2f} {b:.2f}" for a, b in p)}Z" fill="{YELLOW}"/>'


def scene(r, small=False):
    """The view through the badge's window, a circle of radius r around (50, 50).
    Drawn for r=31 and scaled to fit; small has fewer, larger stars."""
    k = r / 31

    def s(v):  # scale a coordinate about the centre
        return 50 + (v - 50) * k

    inside = lambda x, y: math.hypot(x - 50, y - 50) < r - 2
    sky = lambda x, y: inside(x, y) and y < s(50)
    band = lambda x, y: sky(x, y) and abs((y - s(34)) - 0.45 * (x - 50)) < 5 * k
    out = [f'<defs><linearGradient id="{ID}mw" x1="0" y1="0" x2="1" y2="1"><stop offset="0" stop-color="{SKY}" stop-opacity="0"/>'
           f'<stop offset=".5" stop-color="#cfe0ff" stop-opacity="{.65 if small else .5}"/><stop offset="1" stop-color="{SKY}" stop-opacity="0"/></linearGradient>'
           f'<clipPath id="{ID}win"><circle cx="50" cy="50" r="{r}"/></clipPath></defs>'
           f'<circle cx="50" cy="50" r="{r}" fill="{NAVY}"/><g clip-path="url(#{ID}win)">',
           f'<path d="M{s(10):.2f} {s(16):.2f}L{s(90):.2f} {s(52):.2f}L{s(90):.2f} {s(63):.2f}L{s(10):.2f} {s(27):.2f}Z" fill="url(#{ID}mw)"/>']
    if small:
        out.append(stars(7, band, 29, (0.9 * k, 1.4 * k)))
    else:
        out.append(stars(int(34 * k), band, 29, (0.3 * k, 0.8 * k)))
        out.append(stars(int(6 * k), lambda x, y: sky(x, y) and not band(x, y), 31, (0.4 * k, 1.0 * k)))
    out.append(star4(s(63), s(31), (5.5 if small else 4.5) * k))
    out.append(f'<circle cx="50" cy="{s(104):.2f}" r="{44 * k:.2f}" fill="{BLUE}"/>'
               f'<circle cx="50" cy="{s(104):.2f}" r="{44 * k:.2f}" fill="none" stroke="{SKY}" stroke-width="{1.5 * k:.2f}"/>')
    w = (4.4 if small else 3.6) * k
    for color, dy in ((RED, 0), (YELLOW, 5 if small else 5.5)):
        out.append(f'<path d="M{s(14):.2f} {s(62 + dy):.2f}Q50 {s(26 + dy):.2f} {s(86):.2f} {s(54 + dy):.2f}" '
                   f'fill="none" stroke="{color}" stroke-width="{w:.2f}" stroke-linecap="round"/>')
    out.append("</g>")
    return "".join(out)


# Advance widths of Helvetica Bold capitals, in 1/1000 em, to space the rim letters.
WIDTHS = {"A": 722, "B": 722, "C": 722, "E": 667, "G": 778, "I": 278, "K": 722, "L": 611,
          "M": 833, "N": 722, "P": 667, "R": 722, "S": 667, "T": 611, "U": 722, "Y": 667, " ": 278}


def rim_text(text, r=36.3, size=7, spacing=1.1):
    """text around the top of a circle, each letter placed and turned on its own,
    so it shows in every SVG viewer (curved text, textPath, isn't supported everywhere)."""
    adv = [WIDTHS[c] / 1000 * size + spacing for c in text]
    angle = -(sum(adv) - spacing) / 2 / r  # radians; 0 is straight up
    out = []
    for c, a in zip(text, adv):
        mid = angle + (a - spacing) / 2 / r
        if c != " ":
            x, y, deg = 50 + r * math.sin(mid), 50 - r * math.cos(mid), math.degrees(mid)
            out.append(f'<text x="{x:.2f}" y="{y:.2f}" transform="rotate({deg:.2f} {x:.2f} {y:.2f})" text-anchor="middle">{c}</text>')
        angle += a / r
    return f'<g {FONT} font-size="{size}" fill="{WHITE}">{"".join(out)}</g>'


def patch():
    """The full patch: white edge, the name around the rim, the window."""
    return (f'<circle cx="50" cy="50" r="48" fill="{WHITE}"/><circle cx="50" cy="50" r="45.5" fill="{NAVY}"/>'
            + rim_text("INTERGALACTIC BACKUP SYSTEM")
            + scene(31) + f'<circle cx="50" cy="50" r="31" fill="none" stroke="{WHITE}" stroke-width="2"/>')


def badge(small=False):
    """The badge without the rim text: just the window with a white edge."""
    edge = 4.5 if small else 3
    return f'<circle cx="50" cy="50" r="48" fill="{WHITE}"/>' + scene(48 - edge, small)


def svg(body, w=100, h=100, size=""):
    return f'<svg xmlns="http://www.w3.org/2000/svg"{size} viewBox="0 0 {w} {h}">{body}</svg>\n'


def place(m, x, y, size, tag=""):
    m = m.replace(ID, f"{ID}{tag}")
    return f'<g transform="translate({x},{y}) scale({size / 100})">{m}</g>'


def main():
    full, mark, small = patch(), badge(), badge(small=True)
    files = {
        "docs/logo/ibs-patch.svg": svg(full),
        "docs/logo/ibs-mark.svg": svg(mark),
        "docs/logo/favicon.svg": svg(small),
        "web/public/favicon.svg": svg(small),
        "web/src/logo-paths.ts": "// Generated by docs/logo/generate.py — do not edit.\n"
                                 f"export const idPrefix = {ID!r}\nexport const fullMark = {mark!r}\nexport const smallMark = {small!r}\n",
    }

    # Preview sheet: the patch on dark and light, the sidebar lockup, and the small sizes.
    dark, ink = "#0b1020", "#111318"
    font = 'font-family="Helvetica Neue, Helvetica, Arial, sans-serif"'
    o = ['<rect width="720" height="520" fill="#fff"/>',
         f'<rect x="30" y="20" width="320" height="220" rx="18" fill="{dark}"/>' + place(full, 100, 30, 180, tag="a"),
         '<rect x="370" y="20" width="320" height="220" rx="18" fill="#fff" stroke="#d5d8de"/>' + place(full, 440, 30, 180, tag="b")]
    for i, (x, bg, fg, st) in enumerate(((30, dark, "#fff", ""), (370, "#fff", ink, ' stroke="#d5d8de"'))):
        o += [f'<rect x="{x}" y="260" width="320" height="120" rx="18" fill="{bg}"{st}/>', place(mark, x + 40, 280, 80, tag=f"c{i}"),
              f'<text {font} font-size="21" font-weight="700" letter-spacing="1" fill="{fg}">'
              f'<tspan x="{x + 136}" y="300">Intergalactic</tspan><tspan x="{x + 136}" y="326">Backup</tspan><tspan x="{x + 136}" y="352">System</tspan></text>']
    x = 40
    for i, size in enumerate((64, 48, 32, 24, 16)):
        m = mark if size > 32 else small
        o += [f'<rect x="{x}" y="{430 - size / 2}" width="{size}" height="{size}" rx="{size * 0.22}" fill="{dark}"/>',
              place(m, x + size * 0.06, 430 - size / 2 + size * 0.06, size * 0.88, tag=f"d{i}"),
              place(m, x + size + 12, 430 - size / 2, size, tag=f"e{i}")]
        x += 2 * size + 40
    o += [f'<text x="40" y="490" {font} font-size="12" fill="#687082">Badge and tab icon at 64 / 48 / 32 / 24 / 16 px; the patch with the rim text is for large places</text>']
    files["docs/logo/preview.svg"] = svg("".join(o), 720, 520, ' width="1440" height="1040"')

    for path, content in files.items():
        with open(path, "w") as f:
            f.write(content)
        print("wrote", path)


if __name__ == "__main__":
    main()
