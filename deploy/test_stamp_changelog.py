"""Tests for deploy/stamp_changelog.py."""

import pathlib
import tempfile
import unittest

import stamp_changelog


SAMPLE_CHANGELOG = """# Changelog

All notable changes to EveSynapse are documented here.

## [Unreleased]

## [v1.0.3.0] - 2026-11-26

### Maintenance
* Upgrade ranvia.io/go to v0.6.0 by @natemsz in #635

## [v1.0.2.14]

### Bug Fixes
* Example fix by @natemsz in #633
"""


class StampChangelogTest(unittest.TestCase):
    def test_prepends_new_release_and_strips_old_date_and_comments(self) -> None:
        raw_notes = "<!-- generated -->\r\nAutomated build of EveSynapse v1.0.3.1.\r\n\r\n## What's Changed\r\n* Fix thing by @natemsz in #640\r\n"
        updated, changed = stamp_changelog.stamp_text(SAMPLE_CHANGELOG, "1.0.3.1", raw_notes, "2026-11-27")
        self.assertTrue(changed)
        self.assertNotIn("<!--", updated)
        self.assertIn("## [Unreleased]\n\n## [v1.0.3.1] - 2026-11-27\n\nAutomated build of EveSynapse v1.0.3.1.", updated)
        self.assertIn("\n## [v1.0.3.0]\n", updated)
        self.assertNotIn("## [v1.0.3.0] - 2026-11-26", updated)

    def test_idempotent_when_version_exists_and_syncs_embedded(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp) / "CHANGELOG.md"
            emb = pathlib.Path(tmp) / "changelog.md"
            notes = pathlib.Path(tmp) / "release-notes.md"
            root.write_text(SAMPLE_CHANGELOG, encoding="utf-8")
            emb.write_text("stale embedded copy", encoding="utf-8")
            notes.write_text("Automated build of EveSynapse v1.0.3.0.\n", encoding="utf-8")

            rc = stamp_changelog.main([
                "--version", "1.0.3.0",
                "--notes", str(notes),
                "--date", "2026-11-27",
                "--changelog", str(root),
                "--embedded", str(emb),
            ])
            self.assertEqual(rc, 0)
            self.assertEqual(root.read_text(encoding="utf-8"), SAMPLE_CHANGELOG)
            self.assertEqual(emb.read_text(encoding="utf-8"), SAMPLE_CHANGELOG)

    def test_cli_writes_identical_root_and_embedded_files(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp) / "CHANGELOG.md"
            emb = pathlib.Path(tmp) / "changelog.md"
            notes = pathlib.Path(tmp) / "release-notes.md"
            root.write_text(SAMPLE_CHANGELOG, encoding="utf-8")
            emb.write_text(SAMPLE_CHANGELOG, encoding="utf-8")
            notes.write_text("Automated build of EveSynapse v1.0.3.1.\n", encoding="utf-8")

            rc = stamp_changelog.main([
                "--version", "1.0.3.1",
                "--notes", str(notes),
                "--date", "2026-11-27",
                "--changelog", str(root),
                "--embedded", str(emb),
            ])
            self.assertEqual(rc, 0)
            self.assertEqual(root.read_text(encoding="utf-8"), emb.read_text(encoding="utf-8"))
            self.assertIn("## [v1.0.3.1] - 2026-11-27", emb.read_text(encoding="utf-8"))

    def test_errors_on_missing_marker_or_empty_notes(self) -> None:
        with self.assertRaises(ValueError):
            stamp_changelog.stamp_text("# Changelog\n\n## [v1.0.0]\n", "1.0.1", "Notes", "2026-11-27")
        with self.assertRaises(ValueError):
            stamp_changelog.stamp_text(SAMPLE_CHANGELOG, "1.0.3.1", "<!-- comment only -->\n", "2026-11-27")


if __name__ == "__main__":
    unittest.main()
