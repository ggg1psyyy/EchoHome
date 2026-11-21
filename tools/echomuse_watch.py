#!/usr/bin/env python3
"""What EchoMuse has committed since we last looked, for one issue in EchoHome.

Run by .github/workflows/echomuse-watch.yml once a day. It lists EchoMuse's
new commits on main and marks each by what it touches here:

  taken    already in EchoHome (a commit carries "cherry picked from commit
           <sha>" for it), nothing to do
  review   touches files EchoHome still has: may apply
  gone     touches only files EchoHome removed (the old voice path, emOS,
           oww_forge): most likely not ours

Only a hint: whether a change applies is still decided by reading it. The
last commit looked at is kept in the issue's body (MARKER), so the workflow
needs no write access to the repository.

  echomuse_watch.py last ISSUE_BODY_FILE        print the marker's sha, if any
  echomuse_watch.py report SINCE HEAD           print the markdown report
"""
from __future__ import annotations

import re
import subprocess
import sys
from pathlib import Path

UPSTREAM = "https://github.com/wilbowes/EchoMuse"
MARKER = "echomuse-watch last="
# Three commits before EchoMuse's main as reviewed by hand on 2026-11-21
# (f19864c62; 25 commits since the split at c953097, docs/history/device.md
# says what was taken), so the first run shows one commit of each kind.
SEED = "86fe51dba"
# Paths whose changes are records, not code: a commit touching only these is
# "gone" unless something else it touches is still here.
RECORDS = ("docs/history/", "JOURNAL.md", "CHANGELOG.md")


def git(*args: str) -> str:
    return subprocess.run(["git", *args], check=True, capture_output=True, text=True).stdout


def last_seen(body: str) -> str | None:
    m = re.search(re.escape(MARKER) + r"([0-9a-f]{7,40})", body or "")
    return m.group(1) if m else None


def classify(files: list[str], exists, taken: bool) -> str:
    """taken, review or gone, for a commit touching `files`; `exists(path)`
    says whether EchoHome has the path."""
    if taken:
        return "taken"
    code = [f for f in files if not f.startswith(RECORDS)]
    return "review" if any(exists(f) for f in code) else "gone"


def commits(since: str, head: str) -> list[str]:
    """New commits, oldest first. A since that is not an ancestor of head (a
    rewritten upstream) gives the last 20 rather than everything."""
    try:
        git("merge-base", "--is-ancestor", since, head)
        rng = f"{since}..{head}"
        return git("rev-list", "--reverse", rng).split()
    except subprocess.CalledProcessError:
        return git("rev-list", "--reverse", "-n", "20", head).split()


def taken_shas() -> set[str]:
    out = git("log", "--format=%B", "HEAD")
    return set(re.findall(r"cherry picked from commit ([0-9a-f]{40})", out))


def report(since: str, head: str, root: Path = Path(".")) -> str:
    taken = taken_shas()
    rows = {"review": [], "gone": [], "taken": []}
    for sha in commits(since, head):
        subject = git("log", "-1", "--format=%s", sha).strip()
        files = [f for f in git("show", "--format=", "--name-only", sha).split("\n") if f]
        kind = classify(files, lambda p: (root / p).exists(), sha in taken)
        here = [f for f in files if (root / f).exists() and not f.startswith(RECORDS)]
        rows[kind].append((sha, subject, here))
    n = sum(len(v) for v in rows.values())
    if n == 0:
        return ""
    out = [f"{n} new EchoMuse commit{'s' if n != 1 else ''} on main "
           f"([{since[:9]}...{head[:9]}]({UPSTREAM}/compare/{since}...{head})).", ""]
    titles = {"review": "Touch files EchoHome has: review",
              "gone": "Touch only files EchoHome removed: most likely not ours",
              "taken": "Already in EchoHome"}
    for kind in ("review", "gone", "taken"):
        if not rows[kind]:
            continue
        out.append(f"**{titles[kind]}**")
        out.append("")
        for sha, subject, here in rows[kind]:
            line = f"- [{sha[:9]}]({UPSTREAM}/commit/{sha}) {md(subject)}"
            if kind == "review":
                shown = ", ".join(f"`{f}`" for f in here[:4]) + (" …" if len(here) > 4 else "")
                line += f" — {shown}"
            out.append(line)
        out.append("")
    out.append(f"<!-- {MARKER}{head} -->")
    return "\n".join(out) + "\n"


def md(text: str) -> str:
    """A commit subject as plain text in GitHub's Markdown: no links, no
    mentions of people (an @ would notify them)."""
    text = text.replace("@", "@​")
    return "".join("\\" + c if c in "\\`*_[]<>|" else c for c in text)


def main(argv: list[str]) -> int:
    if len(argv) == 3 and argv[1] == "last":
        print(last_seen(Path(argv[2]).read_text()) or SEED)
        return 0
    if len(argv) == 4 and argv[1] == "report":
        sys.stdout.write(report(argv[2], argv[3]))
        return 0
    print(__doc__, file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv))
