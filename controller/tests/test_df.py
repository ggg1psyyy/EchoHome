"""em_df: free space read off an Echo's busybox df."""

import pytest

import em_df


@pytest.mark.parametrize("line,want", [
    # Wrapped filesystem name — what a real device actually returned.
    ("                  1010       648       346  65% /data", 346),
    # Unwrapped, the shape the column index was originally written for.
    ("/dev/block/platform/mtk-msdc.0/by-name/userdata 1010 648 346 65% /data", 346),
    ("tmpfs 100 0 100 0% /data", 100),
])
def test_free_space_is_read_from_the_percentage_anchor_not_a_column_index(line, want):
    """
    busybox wraps a long filesystem name onto its own line, so the available
    column is $3 sometimes and $4 others. awk '{print $4}' returned "65%" on a
    real device — which parsed as no reading, silently disabling the
    free-space check rather than failing visibly.
    """
    assert em_df.parse_free_mb(line) == want


@pytest.mark.parametrize("line", ["", "garbage", "df: /data: No such file or directory"])
def test_unreadable_df_yields_no_measurement(line):
    """None means 'unknown', which a free-space check treats as 'do not
    check' — the opposite of 0, which would block every install."""
    assert em_df.parse_free_mb(line) is None
