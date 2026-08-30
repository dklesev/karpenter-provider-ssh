#!/usr/bin/env python3
# Copyright The karpenter-provider-ssh Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Check every relative markdown link in the repo: the file exists, and the
#anchor exists in it.

Two renderers read these files. GitHub renders README.md and everything someone
browses in the repo; zensical (Python-Markdown) renders docs/ into the site.
They slugify headings differently — GitHub turns each space into a hyphen,
Python-Markdown collapses runs of whitespace into one — so a heading containing
punctuation between words gets two different anchors:

    "Labels & annotations"  ->  github: labels--annotations
                                site:   labels-annotations

A link to one of those is dead in the other renderer, and nothing else here
would notice: the site builds fine, GitHub renders fine, only the reader who
clicks finds out. So a heading is treated as offering BOTH slugs, and a link
into a heading whose two slugs disagree is reported as AMBIGUOUS — it works
where you tested it and nowhere else. Rewording the heading ("and" for "&")
fixes it for good.

No external dependencies on purpose: this runs in CI next to `make verify`.
"""

import os
import re
import sys
import urllib.parse

SKIP_DIRS = {".git", "site", "bin", "node_modules", "vendor"}
# zensical publishes docs/ and nothing else, so a relative link from inside it
# to a repo file outside it (../ROADMAP.md) resolves on GitHub and 404s on the
# site. docs/ links those absolutely; this is that convention, enforced.
SITE_ROOT = "docs"
REPO_BLOB = "https://github.com/dklesev/karpenter-provider-ssh/blob/main"
LINK = re.compile(r"\[([^\]]*)\]\(([^)\s]+)(?:\s+\"[^\"]*\")?\)")
HEADING = re.compile(r"^#{1,6}\s+(.*?)\s*$")
FENCE = re.compile(r"^\s*```")


def slugs(heading):
    """Every anchor this heading answers to, across both renderers."""
    base = re.sub(r"[^\w\s-]", "", re.sub(r"[`*_]", "", heading).strip().lower())
    return {base.replace(" ", "-"), re.sub(r"\s+", "-", base).strip("-")}


def anchors(path):
    """Map anchor -> heading text. A fenced ``` block is code, not headings."""
    found, in_fence = {}, False
    with open(path, encoding="utf-8") as fh:
        for line in fh:
            if FENCE.match(line):
                in_fence = not in_fence
                continue
            if in_fence:
                continue
            m = HEADING.match(line)
            if m:
                for s in slugs(m.group(1)):
                    found.setdefault(s, m.group(1))
    return found


def escapes_site(src, dest):
    """True if a page the site publishes links to something it doesn't."""
    under = lambda p: os.path.relpath(p, SITE_ROOT).split(os.sep)[0] != ".."  # noqa: E731
    return under(src) and not under(dest)


def markdown_files(root="."):
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS]
        for name in filenames:
            if name.endswith(".md"):
                yield os.path.join(dirpath, name)


def main():
    files = sorted(markdown_files())
    cache, problems = {}, []
    for path in files:
        # Links inside fenced code are examples, not navigation.
        body = re.sub(r"```.*?```", "", open(path, encoding="utf-8").read(), flags=re.S)
        for _, target in LINK.findall(body):
            if re.match(r"^(https?:|mailto:|tel:)", target):
                continue  # network checking is a different job with different flakiness
            rel, _, frag = target.partition("#")
            dest = path if not rel else os.path.normpath(os.path.join(os.path.dirname(path), rel))
            if not os.path.exists(dest):
                problems.append(("BROKEN", path, target, "no such file"))
                continue
            if escapes_site(path, dest):
                problems.append(
                    ("SITE-DEAD", path, target,
                     f"resolves outside docs/ ({dest}), which the site does not ship — "
                     f"link it absolutely ({REPO_BLOB}/{os.path.relpath(dest)}) as the rest "
                     f"of docs/ does, or it is dead for every reader of the published site")
                )
                continue
            if not frag or not dest.endswith(".md"):
                continue
            if dest not in cache:
                cache[dest] = anchors(dest)
            frag = urllib.parse.unquote(frag)
            if frag not in cache[dest]:
                problems.append(("BROKEN", path, target, f"no such anchor in {dest}"))
            elif len(slugs(cache[dest][frag])) > 1:
                problems.append(
                    ("AMBIGUOUS", path, target,
                     f"heading {cache[dest][frag]!r} slugs differently on GitHub vs the docs "
                     f"site — reword it (e.g. 'and' for '&') so one link works in both")
                )

    for kind, path, target, why in problems:
        # ::error:: makes CI annotate the file inline.
        print(f"::error file={path}::{kind}: {target} — {why}" if os.environ.get("CI")
              else f"{kind:10} {path} -> {target}\n{'':10} {why}")
    print(f"checked {len(files)} markdown files, {len(problems)} problem(s)")
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main())
