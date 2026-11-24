#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0

"""Every local Markdown link with a #fragment must name a heading that exists.

A link to a section that was renamed still opens the right page, so nobody
notices that it lands at the top instead of the section. Four such links in
the operator docs were found by an audit, not by a check; this is the check.

Fragments are compared with GitHub's heading anchors: the heading's text,
lower-cased, punctuation other than `-` and `_` dropped, spaces turned into
`-`, and `-1`, `-2`, … appended to repeats. Explicit `<a id>` and `<a name>`
anchors count too. Links inside code are not links; external URLs and links
to non-Markdown files are not checked.

Usage: verify-doc-anchors.py [file.md ...]   (default: README.md docs/*.md)
"""
import glob, os, re, sys
from urllib.parse import unquote

root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

FENCE = re.compile(r"^\s{0,3}(`{3,}|~{3,})")
ATX = re.compile(r"^\s{0,3}(#{1,6})\s+(.*?)(?:\s+#+)?\s*$")
SETEXT = re.compile(r"^\s{0,3}(=+|-+)\s*$")
HTML_ANCHOR = re.compile(r"""<a\s[^>]*\b(?:id|name)\s*=\s*["']([^"']+)["']""", re.I)
INLINE_LINK = re.compile(r"!?\[(?:[^\[\]]|\[[^\[\]]*\])*\]\(\s*<?([^)\s>]*)>?(?:\s+[\"'(][^)]*)?\)")
REF_DEF = re.compile(r"^\s{0,3}\[[^\]]+\]:\s*<?(\S+?)>?(?:\s|$)")
CODE_SPAN = re.compile(r"(`+)(.+?)\1")


def lines_outside_fences(path):
    """Yield (number, line) for every line that is not inside a code fence."""
    fence = None
    with open(path, encoding="utf-8") as f:
        for number, line in enumerate(f, 1):
            line = line.rstrip("\n")
            m = FENCE.match(line)
            if fence:
                if m and m.group(1)[0] == fence[0] and len(m.group(1)) >= len(fence):
                    fence = None
                continue
            if m:
                fence = m.group(1)
                continue
            yield number, line


def heading_text(raw):
    """The text GitHub renders for a heading: code kept, markup dropped."""
    out = []
    pos = 0
    for m in CODE_SPAN.finditer(raw):
        out.append(strip_markup(raw[pos:m.start()]))
        out.append(m.group(2).strip())
        pos = m.end()
    out.append(strip_markup(raw[pos:]))
    return "".join(out)


def strip_markup(text):
    text = re.sub(r"!?\[([^\]]*)\]\([^)]*\)", r"\1", text)
    text = re.sub(r"<[^>]+>", "", text)
    text = text.replace("*", "")
    # `_` is emphasis only at a word edge; inside a word GitHub keeps it.
    return re.sub(r"(?<!\w)_+|_+(?!\w)", "", text)


def slug(text):
    text = text.strip().lower()
    text = re.sub(r"[^\w\- ]", "", text)
    return text.replace(" ", "-")


_anchors = {}


def anchors(path):
    if path in _anchors:
        return _anchors[path]
    found, seen = set(), {}
    previous = None
    for _, line in lines_outside_fences(path):
        for m in HTML_ANCHOR.finditer(line):
            found.add(m.group(1))
        text = None
        m = ATX.match(line)
        if m:
            text = m.group(2)
        elif previous and previous.strip() and SETEXT.match(line) and not ATX.match(previous):
            text = previous
        previous = line
        if text is None:
            continue
        base = slug(heading_text(text))
        n = seen.get(base, 0)
        seen[base] = n + 1
        found.add(base if n == 0 else f"{base}-{n}")
    _anchors[path] = found
    return found


def links(line):
    line = CODE_SPAN.sub("", line)
    for m in INLINE_LINK.finditer(line):
        yield m.group(1)
    m = REF_DEF.match(line)
    if m:
        yield m.group(1)


def check(path):
    problems = []
    rel = os.path.relpath(path, root)
    for number, line in lines_outside_fences(path):
        for target in links(line):
            if "#" not in target or re.match(r"^[a-z][a-z0-9+.-]*:", target, re.I):
                continue
            file_part, fragment = target.split("#", 1)
            if not fragment:
                continue
            if file_part:
                dest = os.path.normpath(os.path.join(os.path.dirname(path), unquote(file_part)))
                if not dest.endswith(".md"):
                    continue
                if not os.path.isfile(dest):
                    problems.append(f"{rel}:{number}: {target}: {os.path.relpath(dest, root)} does not exist")
                    continue
            else:
                dest = path
            if unquote(fragment) not in anchors(dest):
                problems.append(f"{rel}:{number}: {target}: no heading or anchor #{fragment} in {os.path.relpath(dest, root)}")
    return problems


def main(argv):
    files = argv or [os.path.join(root, "README.md")] + sorted(glob.glob(os.path.join(root, "docs", "*.md")))
    problems = []
    for f in files:
        problems += check(os.path.abspath(f))
    for p in problems:
        print(f"BROKEN ANCHOR: {p}", file=sys.stderr)
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
