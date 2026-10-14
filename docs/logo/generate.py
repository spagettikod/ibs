#!/usr/bin/env python3
"""Generate every IBS logo asset from one definition.

The mark: a floppy disk on a steel shield, in greyscale. Run from the
repository root:

    python3 docs/logo/generate.py && rsvg-convert -w 1440 docs/logo/preview.svg -o docs/logo/preview.png

Writes docs/logo/*.svg, web/public/favicon.svg and web/src/logo-paths.ts.
"""
import math

ID = "ibsg-"  # prefix of gradient and filter ids; the web interface makes it unique per instance


def bez(p0, p1, p2, p3, n=24):
    return [tuple((1 - t) ** 3 * a + 3 * (1 - t) ** 2 * t * b + 3 * (1 - t) * t ** 2 * c + t ** 3 * d
                  for a, b, c, d in zip(p0, p1, p2, p3)) for t in [i / n for i in range(1, n + 1)]]


def shield(inset=0.0, w=36):
    """The shield outline (peaked top) as path data, optionally inset on every side."""
    w, y0, bottom, shoulder = w - inset, 12 + inset, 94 - inset * 1.75, 40
    l, r = 50 - w, 50 + w
    pts = [(l, y0 + 6), (50, y0 - 3), (r, y0 + 6)]
    pts += [(r, y0 + 6 + (shoulder - y0 - 6) * i / 10) for i in range(1, 11)]
    k = (bottom - shoulder) / 54
    pts += bez((r, shoulder), (r, shoulder + 30 * k), (50 + w * 0.5, shoulder + 42 * k), (50, bottom))
    pts += bez((50, bottom), (50 - w * 0.5, shoulder + 42 * k), (l, shoulder + 30 * k), (l, shoulder))
    pts += [(l, shoulder - (shoulder - y0 - 6) * i / 10) for i in range(1, 10)]
    return "M" + "L".join(f"{x:.2f} {y:.2f}" for x, y in pts) + "Z"


def disk_paths(x0=32, y0=27, s=36):
    x1, y1, c = x0 + s, y0 + s, s * 0.16
    body = (f"M{x0} {y0 + 2.5}Q{x0} {y0} {x0 + 2.5} {y0}L{x1 - c:.2f} {y0}L{x1} {y0 + c:.2f}L{x1} {y1 - 2.5}"
            f"Q{x1} {y1} {x1 - 2.5} {y1}L{x0 + 2.5} {y1}Q{x0} {y1} {x0} {y1 - 2.5}Z")
    sx0, sx1, sy1 = x0 + s * 0.24, x1 - s * 0.3, y0 + s * 0.34
    shutter = f"M{sx0:.2f} {y0}H{sx1:.2f}V{sy1 - 1.5:.2f}Q{sx1:.2f} {sy1:.2f} {sx1 - 1.5:.2f} {sy1:.2f}H{sx0 + 1.5:.2f}Q{sx0:.2f} {sy1:.2f} {sx0:.2f} {sy1 - 1.5:.2f}Z"
    hole = f"M{sx1 - s * 0.16:.2f} {y0 + s * 0.07:.2f}h{s * 0.09:.2f}v{s * 0.19:.2f}h{-s * 0.09:.2f}Z"
    lx0, lx1, ly0 = x0 + s * 0.13, x1 - s * 0.13, y0 + s * 0.48
    label = f"M{lx0:.2f} {y1}V{ly0 + 2:.2f}Q{lx0:.2f} {ly0:.2f} {lx0 + 2:.2f} {ly0:.2f}H{lx1 - 2:.2f}Q{lx1:.2f} {ly0:.2f} {lx1:.2f} {ly0 + 2:.2f}V{y1}Z"
    lines = "".join(f"M{lx0 + 4:.2f} {ly0 + s * k:.2f}H{lx1 - 4:.2f}" for k in (0.12, 0.22, 0.32))
    return body, shutter, hole, label, lines


def gradient(name, stops):
    return (f'<linearGradient id="{ID}{name}" x1="0" y1="0" x2="1" y2="1">'
            + "".join(f'<stop offset="{o}" stop-color="{c}"/>' for o, c in stops) + "</linearGradient>")


def steel(small=False):
    """The full-colour mark. small drops the label lines and the shadow, for 32 px and below."""
    body, shutter, hole, label, lines = disk_paths()
    defs = (gradient("face", [(0, "#dfe2e6"), (0.55, "#a8aeb6"), (1, "#6c727b")])
            + gradient("rim", [(0, "#f4f5f7"), (1, "#3f444c")])
            + gradient("disk", [(0, "#4a5059"), (1, "#22262c")])
            + gradient("shutter", [(0, "#f3f4f6"), (1, "#a9aeb6")]))
    if not small:
        defs += (f'<filter id="{ID}shadow" x="-20%" y="-20%" width="140%" height="140%">'
                 '<feGaussianBlur in="SourceAlpha" stdDeviation="1.4"/><feOffset dx="1.2" dy="1.8"/>'
                 '<feComponentTransfer><feFuncA type="linear" slope="0.45"/></feComponentTransfer>'
                 '<feMerge><feMergeNode/><feMergeNode in="SourceGraphic"/></feMerge></filter>')
    edge = "#15181c"
    o = [f"<defs>{defs}</defs>",
         f'<path d="{shield()}" fill="url(#{ID}face)" stroke="url(#{ID}rim)" stroke-width="{3.6 if small else 3}"/>']
    if not small:
        o.append(f'<path d="{shield(3.2)}" fill="none" stroke="#ffffff" stroke-opacity="0.55" stroke-width="0.8"/>')
    o += [f'<g{"" if small else f" filter=\"url(#{ID}shadow)\""}>',
          f'<path d="{body}" fill="url(#{ID}disk)" stroke="{edge}" stroke-width="0.8"/>',
          f'<path d="{shutter}" fill="url(#{ID}shutter)" stroke="{edge}" stroke-width="0.6"/>',
          f'<path d="{hole}" fill="#22262c"/>',
          f'<path d="{label}" fill="#f1f2f4" stroke="{edge}" stroke-width="0.6"/>']
    if not small:
        o.append(f'<path d="{lines}" stroke="#c7cbd1" stroke-width="0.9" stroke-linecap="round"/>')
    o.append("</g>")
    return "".join(o)


def flat():
    """One colour (currentColor), for places that can't show greys."""
    body, shutter, hole, label, _ = disk_paths()
    line = 'fill="none" stroke="currentColor" stroke-linejoin="round"'
    return (f'<path d="{shield()}" {line} stroke-width="2.6"/>'
            f'<path d="{body}" {line} stroke-width="2.4"/>'
            f'<path d="{shutter}" {line} stroke-width="1.9"/><path d="{hole}" fill="currentColor"/>'
            f'<path d="{label}" {line} stroke-width="1.9"/>')


def svg(body, w=100, h=100, size=""):
    return f'<svg xmlns="http://www.w3.org/2000/svg"{size} viewBox="0 0 {w} {h}">{body}</svg>\n'


def place(m, x, y, size, color=None, tag=""):
    m = m.replace(ID, f"{ID}{tag}")
    style = f' style="color:{color}"' if color else ""
    return f'<g transform="translate({x},{y}) scale({size / 100})"{style}>{m}</g>'


def main():
    full, small = steel(), steel(small=True)
    files = {
        "docs/logo/ibs-mark.svg": svg(full),
        "docs/logo/ibs-mark-small.svg": svg(small),
        "docs/logo/ibs-mark-flat.svg": svg(f'<g style="color:#111318">{flat()}</g>'),
        "docs/logo/favicon.svg": svg(small),
        "web/public/favicon.svg": svg(small),
        "web/src/logo-paths.ts": "// Generated by docs/logo/generate.py — do not edit.\n"
                                 f"export const idPrefix = {ID!r}\nexport const fullMark = {full!r}\nexport const smallMark = {small!r}\n",
    }

    # Preview sheet: on dark and light, wordmark lockups, small sizes, and the flat version.
    dark, ink = "#0b1020", "#111318"
    font = 'font-family="Helvetica Neue, Helvetica, Arial, sans-serif"'
    o = ['<rect width="720" height="520" fill="#fff"/>',
         f'<rect x="30" y="20" width="320" height="220" rx="18" fill="{dark}"/>' + place(full, 110, 50, 160, tag="a"),
         '<rect x="370" y="20" width="320" height="220" rx="18" fill="#fff" stroke="#d5d8de"/>' + place(full, 450, 50, 160, tag="b")]
    for i, (x, bg, fg, st) in enumerate(((30, dark, "#fff", ""), (370, "#fff", ink, ' stroke="#d5d8de"'))):
        o += [f'<rect x="{x}" y="260" width="320" height="120" rx="18" fill="{bg}"{st}/>', place(full, x + 40, 280, 80, tag=f"c{i}"),
              f'<text x="{x + 136}" y="334" {font} font-size="48" font-weight="700" letter-spacing="5" fill="{fg}">IBS</text>',
              f'<text x="{x + 138}" y="356" {font} font-size="12" fill="{fg}" opacity="0.7">Intergalactic Backup System</text>']
    x = 40
    for i, size in enumerate((64, 48, 32, 24, 16)):
        m = full if size > 32 else small
        o += [f'<rect x="{x}" y="{430 - size / 2}" width="{size}" height="{size}" rx="{size * 0.22}" fill="{dark}"/>',
              place(m, x + size * 0.06, 430 - size / 2 + size * 0.06, size * 0.88, tag=f"d{i}"),
              place(m, x + size + 12, 430 - size / 2, size, tag=f"e{i}")]
        x += 2 * size + 40
    o += [place(flat(), 600, 398, 64, ink),
          f'<text x="40" y="490" {font} font-size="12" fill="#687082">Tab icon and mark at 64 / 48 / 32 / 24 / 16 px; flat one-colour version on the right</text>']
    files["docs/logo/preview.svg"] = svg("".join(o), 720, 520, ' width="1440" height="1040"')

    for path, content in files.items():
        with open(path, "w") as f:
            f.write(content)
        print("wrote", path)


if __name__ == "__main__":
    main()
