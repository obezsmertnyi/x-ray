# Usage: python3 hack/gen-recall-svg.py <path to xray-map.svg> docs/img/recall.svg
#
# Builds docs/img/recall.svg from the landing page's xray-map.svg: the same map,
# lit the way the landing page lights it (cells glow and fade, the kind's region
# washes, dots pulse, the label pulses), as a fixed looping schedule so it plays
# with no script. A fibre and spike layer (the web UI's optogenetic effect) is
# laid over the recalled region of each read.
import re, random, math, sys

SRC = sys.argv[1]   # the landing page's xray-map.svg
OUT = sys.argv[2]   # docs/img/recall.svg
T = 24.0
s = open(SRC).read()
W, H = 1100, 640
cx0, cy0 = W / 2, H / 2

cells = []   # dict(k,i,color,d,tag)
for m in re.finditer(r'<path class="c" data-k="(\d+)" data-i="(\d+)" style="color:([^"]+)" d="([^"]+)"[^>]*/>', s):
    cells.append(dict(k=int(m[1]), i=int(m[2]), color=m[3], d=m[4], tag=m[0]))
dots = []
for m in re.finditer(r'<circle class="d" data-k="(\d+)" data-i="(\d+)" style="color:([^"]+)" cx="([\d.]+)" cy="([\d.]+)" r="([\d.]+)"[^>]*/>', s):
    dots.append(dict(k=int(m[1]), i=int(m[2]), color=m[3], x=float(m[4]), y=float(m[5]), r=float(m[6]), tag=m[0]))
lines = []
for m in re.finditer(r'<line x1="([\d.]+)" y1="([\d.]+)" x2="([\d.]+)" y2="([\d.]+)"[^>]*/>', s):
    lines.append(dict(x1=float(m[1]), y1=float(m[2]), x2=float(m[3]), y2=float(m[4])))
labels = {}
for m in re.finditer(r'<g class="l" data-k="(\d+)">', s):
    labels[int(m[1])] = m[0]
byk = {}
for c in cells: byk.setdefault(c['k'], []).append(c)
dbyk = {}
for d in dots: dbyk.setdefault(d['k'], []).append(d)
dot_at = lambda x, y: next((d for d in dots if abs(d['x'] - x) < 1.2 and abs(d['y'] - y) < 1.2), None)
for ln in lines:
    ln['a'], ln['b'] = dot_at(ln['x1'], ln['y1']), dot_at(ln['x2'], ln['y2'])
linked = [ln for ln in lines if ln['a'] and ln['b']]
print('cells', len(cells), 'dots', len(dots), 'lines', len(lines), 'linked', len(linked), file=sys.stderr)

rnd = random.Random(11)
reads = [(1.2, [0, 4, 6]), (7.0, [1, 3]), (12.8, [2, 5, 7]), (18.6, [3, 5])]  # first kind = the recalled region
used_c, used_d, used_l = set(), set(), set()
cls_c, cls_d, cls_l = {}, {}, {}   # index -> (class, delay)
overlay, beams, defs_extra = [], [], []

def kf(name, pts, base=None):
    out = [f'@keyframes {name}{{']
    last = 0
    for t, props in pts:
        out.append(f'{t / T * 100:.3f}%{{{props}}}')
        last = t
    out.append('100%{' + (pts[-1][1] if base is None else base) + '}}')
    return ''.join(out)

css = []
css.append(kf('xh', [(0, 'fill-opacity:.55;stroke-width:2;filter:drop-shadow(0 0 6px currentColor)'), (12, 'fill-opacity:.26;stroke-width:1;filter:drop-shadow(0 0 0 transparent)')]))
css.append(kf('xr', [(0, 'fill-opacity:.4;filter:drop-shadow(0 0 3px currentColor)'), (8, 'fill-opacity:.26;filter:drop-shadow(0 0 0 transparent)')]))
css.append(kf('xdh', [(0, 'transform:scale(1);opacity:1'), (.42, 'transform:scale(2.6);opacity:.9'), (1.12, 'transform:scale(1.8);opacity:.8'), (1.68, 'transform:scale(2.2);opacity:.7'), (2.8, 'transform:scale(1);opacity:.5'), (4.8, 'transform:scale(1);opacity:1')]))
css.append(kf('xdr', [(0, 'transform:scale(1);opacity:.9'), (.52, 'transform:scale(1.8);opacity:.85'), (1.15, 'transform:scale(1.4);opacity:.65'), (2.1, 'transform:scale(1);opacity:.5'), (3.8, 'transform:scale(1);opacity:1')]))
css.append(kf('xl', [(0, 'opacity:1'), (.77, 'opacity:.5'), (1.54, 'opacity:.85'), (2.2, 'opacity:1')]))
css.append(kf('ob', [(0, 'opacity:0'), (.12, 'opacity:1'), (.57, 'opacity:.8'), (.9, 'opacity:.55'), (1.2, 'opacity:.8'), (1.9, 'opacity:0')]))
css.append(kf('ow', [(0, 'opacity:0'), (.13, 'opacity:.5'), (.47, 'opacity:.3'), (.78, 'opacity:.38'), (2.6, 'opacity:0')]))
css.append(kf('of', [(0, 'opacity:0'), (.1, 'opacity:1'), (.34, 'opacity:.45'), (.58, 'opacity:.9'), (3.4, 'opacity:0')]))
css.append(kf('oh', [(0, 'opacity:0;transform:scale(.5)'), (.18, 'opacity:1;transform:scale(1.35)'), (6, 'opacity:0;transform:scale(1)')]))
css.append(kf('or', [(0, 'opacity:.95;transform:scale(1)'), (1.1, 'opacity:0;transform:scale(8.8)')]))
css.append(kf('os', [(0, 'opacity:0;stroke-dashoffset:22'), (.09, 'opacity:1;stroke-dashoffset:12'), (.75, 'opacity:0;stroke-dashoffset:-100')]))
css.append(kf('ol', [(0, 'opacity:0'), (.4, 'opacity:.9'), (2.5, 'opacity:0')]))

fmt = lambda v: f'{v:.1f}'
gid = 0
for r_i, (t0, kinds) in enumerate(reads):
    for n, k in enumerate(kinds):
        tk = t0 + n * (0.2 + 0.35 * rnd.random())
        members = [c for c in byk[k] if (k, c['i']) not in used_c]
        mdots = {d['i']: d for d in dbyk[k]}
        primary = n == 0
        # exact recalled cells: for the recalled region, ones that have a synapse
        cand = [ln for ln in linked if ln['a']['k'] == k and ln['a']['i'] not in used_d and ln['b']['i'] not in used_d] if primary else []
        hit_d = []
        syn_lines = []
        if primary and cand:
            for ln in rnd.sample(cand, min(2, len(cand))):
                if ln['a']['i'] not in used_d:
                    hit_d.append(ln['a']); syn_lines.append(ln)
                    used_d.add(ln['a']['i']); used_d.add(ln['b']['i'])
        else:
            pool = [d for d in dbyk[k] if d['i'] not in used_d]
            for d in rnd.sample(pool, min(1 + rnd.randrange(3), len(pool))):
                hit_d.append(d); used_d.add(d['i'])
        hit_ids = {d['i'] for d in hit_d}
        # landing page flash: hit cells glow for 12 s, the rest of the kind washes for 8 s
        for c in byk[k]:
            if (k, c['i']) in used_c: continue
            used_c.add((k, c['i']))
            cls_c[c['i']] = ('hit' if c['i'] in hit_ids else 'region', tk)
        for d in dbyk[k]:
            if d['i'] in cls_d: continue
            cls_d[d['i']] = ('dh' if d['i'] in hit_ids else 'dr', tk)
        if k not in cls_l or True:
            cls_l.setdefault(k, []).append(tk)
        if not primary: continue
        # ---- the optogenetic layer on the recalled region ----
        mem = dbyk[k]
        mx = sum(d['x'] for d in mem) / len(mem); my = sum(d['y'] for d in mem) / len(mem)
        dx, dy = mx - cx0, my - cy0; L = math.hypot(dx, dy) or 1; dx, dy = dx / L, dy / L
        rad = max(70, min(190, max(math.hypot(d['x'] - mx, d['y'] - my) for d in mem) + 36))
        reach = min(W, H) * 0.34
        # keep the fibre tip in frame
        while reach > 20 and not (24 <= mx + dx * reach <= W - 24 and 24 <= my + dy * reach <= H - 24): reach -= 6
        tipx, tipy = mx + dx * reach, my + dy * reach
        endx, endy = mx - dx * rad * .7, my - dy * rad * .7
        px, py = -dy, dx
        gid += 1
        defs_extra.append(f'<linearGradient id="bg{gid}" gradientUnits="userSpaceOnUse" x1="{fmt(tipx)}" y1="{fmt(tipy)}" x2="{fmt(endx)}" y2="{fmt(endy)}"><stop offset="0" stop-color="#eaf8ff" stop-opacity=".9"/><stop offset=".25" stop-color="#8fdcff" stop-opacity=".38"/><stop offset="1" stop-color="#3fa9ff" stop-opacity="0"/></linearGradient>')
        beam_d = f'M{fmt(tipx + px * 4)} {fmt(tipy + py * 4)}L{fmt(endx + px * rad * .5)} {fmt(endy + py * rad * .5)}L{fmt(endx - px * rad * .5)} {fmt(endy - py * rad * .5)}L{fmt(tipx - px * 4)} {fmt(tipy - py * 4)}Z'
        beams.append(f'<path class="o ob" style="--d:{tk:.2f}" d="{beam_d}" fill="url(#bg{gid})" filter="url(#soft)"/>')
        beams.append(f'<circle class="o ob" style="--d:{tk:.2f}" cx="{fmt(tipx)}" cy="{fmt(tipy)}" r="7" fill="#eaf8ff" filter="url(#bloom)"/>')
        near = min(math.hypot(d['x'] - tipx, d['y'] - tipy) for d in mem)
        far = max(math.hypot(d['x'] - tipx, d['y'] - tipy) for d in mem)
        for c in byk[k]:
            dd = mdots.get(c['i'])
            # the cell and its dot share data-i; the light reaches the nearest first
            dl = ((math.hypot(dd['x'] - tipx, dd['y'] - tipy) - near) / max(1, far - near) * .32) if dd else 0
            fired = c['i'] in hit_ids
            overlay.append(f'<path class="o {"of" if fired else "ow"}" style="--d:{tk + dl:.2f}" d="{c["d"]}" fill="{"#eaf8ff" if fired else "#3fa9ff"}"/>')
        for d in hit_d:
            overlay.append(f'<circle class="o oh" style="--d:{tk:.2f}" cx="{fmt(d["x"])}" cy="{fmt(d["y"])}" r="22" fill="url(#halo)"/>')
            for q in range(3):
                overlay.append(f'<circle class="o or" style="--d:{tk + q * .15:.2f}" cx="{fmt(d["x"])}" cy="{fmt(d["y"])}" r="5" fill="none" stroke="#8fdcff" stroke-width="1.6"/>')
        for ln in syn_lines:
            a, b = ln['a'], ln['b']
            overlay.append(f'<line class="o os" style="--d:{tk + .18:.2f}" x1="{fmt(a["x"])}" y1="{fmt(a["y"])}" x2="{fmt(b["x"])}" y2="{fmt(b["y"])}" pathLength="100" stroke="#8fdcff" stroke-width="2.6" stroke-linecap="round" filter="url(#bloom)"/>')
            overlay.append(f'<circle class="o ol" style="--d:{tk + .63:.2f}" cx="{fmt(b["x"])}" cy="{fmt(b["y"])}" r="12" fill="url(#halo)"/>')

# rewrite the map's own elements with their class and schedule
for c in cells:
    if c['i'] in cls_c:
        cl, t = cls_c[c['i']]
        s = s.replace(c['tag'], c['tag'].replace('class="c"', f'class="c a {cl}"').replace('style="color:', f'style="--d:{t:.2f};color:', 1), 1)
for d in dots:
    if d['i'] in cls_d:
        cl, t = cls_d[d['i']]
        s = s.replace(d['tag'], d['tag'].replace('class="d"', f'class="d a {cl}"').replace('style="color:', f'style="--d:{t:.2f};color:', 1), 1)
for k, tag in labels.items():
    # a kind lit by two reads pulses at its first; one animation per element
    s = s.replace(tag, f'<g class="l a lh" data-k="{k}" style="--d:{min(cls_l[k]):.2f}">', 1) if k in cls_l else s

style = '''<style>
.a,.o{animation-duration:24s;animation-iteration-count:infinite;animation-delay:calc(var(--d,0) * 1s)}
.o{opacity:0;pointer-events:none;mix-blend-mode:screen;transform-box:fill-box;transform-origin:center}
.d{transform-box:fill-box;transform-origin:center}
.c.hit{animation-name:xh;animation-timing-function:cubic-bezier(.33,1,.68,1)}
.c.region{animation-name:xr;animation-timing-function:cubic-bezier(.33,1,.68,1)}
.d.dh{animation-name:xdh;animation-timing-function:cubic-bezier(.22,1,.36,1)}
.d.dr{animation-name:xdr;animation-timing-function:cubic-bezier(.22,1,.36,1)}
.l.lh{animation-name:xl;animation-timing-function:cubic-bezier(.22,1,.36,1)}
.ob{animation-name:ob;animation-timing-function:ease-out}
.ow{animation-name:ow;animation-timing-function:cubic-bezier(.2,.7,.2,1)}
.of{animation-name:of;animation-timing-function:cubic-bezier(.2,.7,.2,1)}
.oh{animation-name:oh;animation-timing-function:ease-out}
.or{animation-name:or;animation-timing-function:ease-out;vector-effect:non-scaling-stroke}
.os{animation-name:os;animation-timing-function:ease-in-out;stroke-dasharray:22 100}
.ol{animation-name:ol}
@media (prefers-reduced-motion:reduce){.a,.o{animation:none}.o{display:none}}
''' + ''.join(css) + '</style>'

defs_add = ('<radialGradient id="halo"><stop offset="0" stop-color="#eaf8ff" stop-opacity=".95"/><stop offset=".35" stop-color="#8fdcff" stop-opacity=".55"/><stop offset="1" stop-color="#3fa9ff" stop-opacity="0"/></radialGradient>'
            '<filter id="soft" x="-50%" y="-50%" width="200%" height="200%"><feGaussianBlur stdDeviation="9"/></filter>'
            '<filter id="bloom" x="-80%" y="-80%" width="260%" height="260%"><feGaussianBlur stdDeviation="4"/></filter>'
            + ''.join(defs_extra))
s = s.replace('</defs>', defs_add + '</defs>', 1)
s = s.replace('<defs>', style + '<defs>', 1)
# overlay inside the masked group (edges fade as the map's own do); beams outside it
assert s.rstrip().endswith('</g></svg>')
s = s.rstrip()[:-len('</g></svg>')] + ''.join(overlay) + '</g><g>' + ''.join(beams) + '</g></svg>\n'
s = s.replace('role="img" aria-label="The memory map: every memory cell, grouped by kind, with the links between them"', 'role="img" aria-label="The memory map, as a recall lights it: cells glow and fade, the recalled region is lit by a blue fibre, the recalled cells fire with spike rings, and a pulse runs along their links."')
open(OUT, 'w').write(s)
print('wrote', OUT, len(s), 'bytes', file=sys.stderr)
