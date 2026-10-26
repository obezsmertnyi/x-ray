#!/usr/bin/env python3
"""Split a Markdown spec into small sections so each one is its own graph node.

Usage: python3 hack/split-doc-map.py docs/spec.md > split.md

The docs extractor makes a node only for each "## " heading and embeds at most
2000 characters of its body, so a spec with long sections and "### " sub-headings
maps to a few coarse nodes with their tails unembedded. This rewrites every
"### Child" under "## Parent" as "## Parent: Child", and cuts a body that runs
past CAP characters at a blank line (or between table rows, repeating the
header) into "(1/n)" parts. The input file is not changed.
"""
import re, sys
CAP = 1700
src = open(sys.argv[1]).read().split("\n")
# 1. flatten: "## A" + "### B" -> "## A: B"
secs = []  # (title, [lines])
h1 = None; parent = None; cur = None; fence = False
for ln in src:
    if ln.startswith("```"): fence = not fence
    if not fence and ln.startswith("# "):
        h1 = ln[2:]; cur = ("__intro", []); secs.append(cur); continue
    if not fence and ln.startswith("## "):
        parent = ln[3:]; cur = (parent, []); secs.append(cur); continue
    if not fence and ln.startswith("### "):
        cur = (f"{parent}: {ln[4:]}", []); secs.append(cur); continue
    cur[1].append(ln)
# 2. split oversize bodies on blank lines / table rows
def blocks(lines):
    out, b, fence = [], [], False
    for ln in lines:
        if ln.startswith("```"): fence = not fence
        if ln == "" and not fence:
            if b: out.append(b); b = []
        else: b.append(ln)
    if b: out.append(b)
    return out
res = [f"# {h1}", ""]
for title, lines in secs:
    if title == "__intro":
        res += [l for l in lines]; continue
    bl = blocks(lines)
    # break oversize table blocks into row groups, repeating the header
    nb = []
    for b in bl:
        if sum(len(l)+1 for l in b) > CAP and b[0].startswith("|"):
            hdr, rows = b[:2], b[2:]; g = list(hdr)
            for r in rows:
                if sum(len(l)+1 for l in g) + len(r) > CAP and len(g) > 2:
                    nb.append(g); g = list(hdr)
                g.append(r)
            nb.append(g)
        else: nb.append(b)
    parts, cur_b, size = [], [], 0
    for b in nb:
        n = sum(len(l)+1 for l in b)
        if cur_b and size + n > CAP: parts.append(cur_b); cur_b, size = [], 0
        cur_b.append(b); size += n
    if cur_b: parts.append(cur_b)
    if not parts: parts = [[]]
    for i, p in enumerate(parts):
        t = title if len(parts) == 1 else f"{title} ({i+1}/{len(parts)})"
        res += ["", f"## {t}", ""]
        for b in p: res += b + [""]
print("\n".join(res).strip() + "\n")
