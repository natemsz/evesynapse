#!/usr/bin/env python3
"""Stamp release notes into CHANGELOG.md and internal/app/changelog.md.

Prepends a versioned release section immediately after the Unreleased heading,
drops trailing dates from prior version headings, strips HTML comments from
the notes, and writes identical bytes to both the root changelog and the
embedded Go copy. Idempotent when the target version heading already exists.
"""

import argparse
import datetime
import pathlib
import re
import sys

UNRELEASED_MARKER = "## [Unreleased]\n\n"
DATED_HEADING_RE = re.compile(r"^(## \[v[^\]]+\]) - \d{4}-\d{2}-\d{2}$", re.M)
SELF_UPDATE_RE = re.compile(
    r"^(?:docs\(changelog\):\s*)?(?:updated?\s+changelog(?:\.md)?(?:\s+and\s+embedded\s+changelog)?|review\s+changelog)\s+(?:for|to)\s+(?:version\s+)?v?\d+\.\d+",
    re.I,
)


def is_self_update_bullet(line: str) -> bool:
    stripped = line.lstrip()
    if not (stripped.startswith("* ") or stripped.startswith("- ")):
        return False
    return bool(SELF_UPDATE_RE.match(stripped[2:].strip()))


def strip_self_updates(text: str) -> str:
    """Drop automated changelog-update bullets and any category headings left empty."""
    lines = [ln for ln in text.splitlines() if not is_self_update_bullet(ln)]
    kept: list[str] = []
    i = 0
    while i < len(lines):
        if lines[i].lstrip().startswith("### "):
            j = i + 1
            while j < len(lines) and not lines[j].strip():
                j += 1
            if j >= len(lines) or lines[j].lstrip().startswith(("## ", "### ", "**Full Changelog**")):
                i = j
                continue
        kept.append(lines[i])
        i += 1
    collapsed: list[str] = []
    for ln in kept:
        if not ln.strip() and (not collapsed or not collapsed[-1].strip()):
            continue
        collapsed.append(ln)
    while collapsed and not collapsed[-1].strip():
        collapsed.pop()
    return "\n".join(collapsed)


def clean_notes(raw: str) -> str:
    text = raw.replace("\r\n", "\n").strip()
    lines = [ln for ln in text.splitlines() if not ln.lstrip().startswith("<!--")]
    return strip_self_updates("\n".join(lines)).strip()


def stamp_text(existing: str, version: str, raw_notes: str, date: str) -> tuple[str, bool]:
    """Return (updated_text, changed) for the given changelog text."""
    version = version.strip().lstrip("v")
    if not version:
        raise ValueError("version must not be empty")
    if f"## [v{version}]" in existing:
        return existing, False
    notes = clean_notes(raw_notes)
    if not notes:
        raise ValueError("release notes are empty")
    if UNRELEASED_MARKER not in existing:
        raise ValueError("CHANGELOG.md is missing the Unreleased heading")
    stripped = DATED_HEADING_RE.sub(r"\1", existing)
    section = f"## [v{version}] - {date}\n\n{notes}\n\n"
    updated = stripped.replace(UNRELEASED_MARKER, UNRELEASED_MARKER + section, 1)
    return updated, True


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Stamp release notes into root and embedded changelogs.")
    parser.add_argument("--version", required=True, help="Release version without leading v (e.g. 1.0.3.1)")
    parser.add_argument("--notes", required=True, help="Path to markdown release notes file")
    parser.add_argument(
        "--date",
        default=datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%d"),
        help="UTC release date (YYYY-MM-DD)",
    )
    parser.add_argument("--changelog", default="CHANGELOG.md", help="Path to repository root CHANGELOG.md")
    parser.add_argument(
        "--embedded",
        default="internal/app/changelog.md",
        help="Path to embedded internal/app/changelog.md",
    )
    args = parser.parse_args(argv)

    changelog_path = pathlib.Path(args.changelog)
    embedded_path = pathlib.Path(args.embedded)
    existing = changelog_path.read_text(encoding="utf-8")
    raw_notes = pathlib.Path(args.notes).read_text(encoding="utf-8")

    try:
        updated, changed = stamp_text(existing, args.version, raw_notes, args.date)
    except ValueError as exc:
        print(f"stamp_changelog: {exc}", file=sys.stderr)
        return 1

    if changed:
        changelog_path.write_text(updated, encoding="utf-8")
        embedded_path.write_text(updated, encoding="utf-8")
        print(f"Stamped v{args.version.lstrip('v')} into {changelog_path} and {embedded_path}.")
    else:
        if not embedded_path.exists() or embedded_path.read_text(encoding="utf-8") != existing:
            embedded_path.write_text(existing, encoding="utf-8")
        print(f"{changelog_path} already documents v{args.version.lstrip('v')}; nothing to stamp.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
