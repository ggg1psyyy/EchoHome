"""tools/echomuse_watch.py: EchoMuse's new commits, sorted for review."""
import os
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "tools"))
import echomuse_watch as w  # noqa: E402


def test_the_marker_is_found_and_missing_means_none():
    assert w.last_seen("text\n<!-- echomuse-watch last=abc1234def -->\n") == "abc1234def"
    assert w.last_seen("no marker here") is None
    assert w.last_seen("") is None


def test_classify():
    here = {"device/cmd/server.go", "docs/history/device.md"}.__contains__
    assert w.classify(["device/cmd/server.go"], here, taken=False) == "review"
    assert w.classify(["controller/em_speechgate.py"], here, taken=False) == "gone"
    # A record alone does not make a commit ours.
    assert w.classify(["docs/history/device.md", "controller/em_barge.py"], here, taken=False) == "gone"
    assert w.classify(["device/cmd/server.go"], here, taken=True) == "taken"


def test_a_subject_is_plain_text_and_mentions_no_one():
    out = w.md("Fix [link](x) for @someone with `code`*")
    assert "@​someone" in out
    assert "\\[link\\]" in out and "\\`code\\`" in out and "\\*" in out


def _git(cwd, *args):
    env = {**os.environ, "GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t",
           "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t"}
    return subprocess.run(["git", *args], cwd=cwd, check=True, capture_output=True,
                          text=True, env=env).stdout.strip()


def test_report_sorts_new_commits(tmp_path, monkeypatch):
    up, here = tmp_path / "up", tmp_path / "here"
    for d in (up, here):
        d.mkdir()
        _git(d, "init", "-q")
    # Upstream: a base, then one change to a file EchoHome has, one to a file
    # it removed, and one EchoHome has already taken.
    (up / "kept.go").write_text("a\n")
    (up / "removed.py").write_text("a\n")
    _git(up, "add", "-A"); _git(up, "commit", "-qm", "base")
    base = _git(up, "rev-parse", "HEAD")
    (up / "kept.go").write_text("b\n"); _git(up, "commit", "-qam", "Kept: a change")
    (up / "removed.py").write_text("b\n"); _git(up, "commit", "-qam", "Removed: a change")
    (up / "kept.go").write_text("c\n"); _git(up, "commit", "-qam", "Taken: a change")
    taken = _git(up, "rev-parse", "HEAD")
    # EchoHome: has kept.go only, and a commit saying it took the last one.
    (here / "kept.go").write_text("c\n")
    _git(here, "add", "-A"); _git(here, "commit", "-qm", f"Taken\n\n(cherry picked from commit {taken})")
    _git(here, "fetch", "-q", str(up), "HEAD")
    monkeypatch.chdir(here)
    out = w.report(base, taken, root=here)
    review = out.split("**Touch only")[0]
    assert "3 new EchoMuse commits" in out
    assert "Kept: a change" in review and "`kept.go`" in review
    assert "Removed: a change" in out.split("**Touch only")[1].split("**Already")[0]
    assert "Taken: a change" in out.split("**Already in EchoHome**")[1]
    assert out.rstrip().endswith(f"<!-- echomuse-watch last={taken} -->")
    # Nothing new: nothing to say.
    assert w.report(taken, taken, root=here) == ""
