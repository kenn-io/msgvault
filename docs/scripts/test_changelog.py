import contextlib
import io
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import changelog
import check_built_site
import check_markdown_sources


class ChangelogTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.env = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
        self.env.update(GIT_CONFIG_GLOBAL=str(self.root / "gitconfig"), GIT_CONFIG_NOSYSTEM="1")
        self.env_patch = patch.dict(os.environ, self.env, clear=True)
        self.env_patch.start()
        self.addCleanup(self.env_patch.stop)
        self.root_patch = patch.object(changelog, "ROOT", self.root)
        self.root_patch.start()
        self.addCleanup(self.root_patch.stop)
        (self.root / "docs").mkdir()
        self.log = self.root / "docs/changelog.md"
        self.log.write_text("# Changelog\n\n## Unreleased\n\n### Improvements\n\n- Existing\n\n## 1.0.0\n<small>2020-01-01</small>\n")

    def git(self, *args):
        return subprocess.check_output(["git", *args], cwd=self.root, stderr=subprocess.PIPE).decode().strip()

    def init(self):
        self.git("init", "-b", "main")
        self.git("config", "user.name", "Test")
        self.git("config", "user.email", "test@example.com")
        self.git("add", ".")
        self.git("commit", "-m", "initial")

    def note(self, name, text):
        directory = self.root / "docs/changelog.d"
        directory.mkdir(exist_ok=True)
        path = directory / name
        path.write_text(text, encoding="utf-8")
        return path

    def run_command(self, *args):
        stderr = io.StringIO()
        with patch.object(sys, "argv", ["changelog.py", *map(str, args)]), contextlib.redirect_stderr(stderr):
            result = changelog.main()
        return result, stderr.getvalue()

    def test_render_sorts_names_and_handles_missing_directory(self):
        output = self.root / "out.md"
        self.assertEqual((0, ""), self.run_command("render", output))
        self.assertIn("- Existing", output.read_text())
        for name in ("z", "a", "m", "b", "y"):
            self.note(f"{name}.md", f"### New features\n\n- {name.upper()}\n")
        self.assertEqual((0, ""), self.run_command("render", output))
        self.assertIn("### New features\n\n- A\n- B\n- M\n- Y\n- Z\n\n### Improvements", output.read_text())
        self.note(".draft.md", "### Bug fixes\n\n- Private draft\n")
        self.note("note.md", "### Bug fixes\n\n- Public note\n")
        self.assertEqual((0, ""), self.run_command("render", output))
        self.assertIn("- Public note", output.read_text())
        self.assertNotIn("- Private draft", output.read_text())

    def test_render_reports_missing_unreleased_heading(self):
        self.log.write_text("# Changelog\n\n## 1.0.0\n")
        result, error = self.run_command("render", self.root / "out.md")
        self.assertEqual(1, result)
        self.assertIn("requires a ## Unreleased heading", error)

    def test_fragment_source_checks(self):
        self.note("invalid.md", "### Bug fixes\n\n!!! note broken\n\n<Card>\n")
        self.note(".draft.md", "### Bug fixes\n\n<Card>\n")
        stderr = io.StringIO()
        with patch.object(check_markdown_sources, "ROOT", self.root / "docs"), patch.object(check_markdown_sources, "PUBLIC_MARKDOWN", set()), contextlib.redirect_stderr(stderr), self.assertRaises(SystemExit):
            check_markdown_sources.main()
        self.assertIn("changelog.d/invalid.md:3: malformed or collapsed admonition", stderr.getvalue())
        self.assertIn("changelog.d/invalid.md: contains unsupported MDX/Starlight marker '<Card'", stderr.getvalue())
        self.assertNotIn(".draft.md", stderr.getvalue())

    def test_built_site_checks_assembled_changelog(self):
        self.note("note.md", "### Bug fixes\n\n- Fixed a problem\n")
        published = self.root / "published.md"
        published.write_text(changelog.render())
        check_built_site.check_markdown_source("/docs/changelog.md", published)
        published.write_text(self.log.read_text())
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr), self.assertRaises(SystemExit):
            check_built_site.check_markdown_source("/docs/changelog.md", published)
        self.assertIn("published Markdown differs from its source: /docs/changelog.md", stderr.getvalue())

    def test_release_output_and_latest_filename_addition_order(self):
        self.init()
        unicode_note = self.note("été.md", "### Bug fixes\n\n- Unicode\n")
        self.note("café.md", "### Bug fixes\n\n- Cafe\n")
        self.git("add", ".")
        self.git("commit", "-m", "cafe")
        self.note("later.md", "### Bug fixes\n\n- Later\n")
        self.git("add", ".")
        self.git("commit", "-m", "later")
        self.git("mv", "docs/changelog.d/café.md", "docs/changelog.d/renamed.md")
        self.git("commit", "-m", "rename")
        self.git("rm", "docs/changelog.d/later.md")
        self.git("commit", "-m", "remove")
        later = self.note("later.md", "### Bug fixes\n\n- Re-added\n")
        self.git("add", ".")
        self.git("commit", "-m", "re-add")
        self.assertEqual((0, ""), self.run_command("release", "2.0.0", "2026-01-01"))
        self.assertIn("## Unreleased\n\n## 2.0.0\n<small>2026-01-01</small>\n\n### Improvements\n\n- Existing\n", self.log.read_text())
        self.assertIn("- Unicode\n- Cafe\n- Re-added", self.log.read_text())
        self.assertFalse(unicode_note.exists())
        self.assertFalse(later.exists())
        self.assertFalse((self.root / "docs/changelog.d/renamed.md").exists())

    def test_fragment_sections(self):
        cases = (
            ("### Bug fixes\n\n", False),
            ("### Unknown\n\n- Note\n", False),
            ("### Bug fixes\n\n- Note\n### Improvements\n- Other\n", False),
            ("### Bug fixes\n\n- Note\n## Oops\n", False),
            ("### Bug fixes\n\n- Note\n# Oops\n", False),
            ("### Bug fixes\n\n```sh\n# comment\n```\n### Other\n- Note\n", False),
            ("### Improvements\n\nRun this:\n\n```sh\n# comment\necho hello\n```\n\n~~~sh\n## comment\n~~~\n", True),
        )
        for text, valid in cases:
            with self.subTest(text=text):
                self.note("fragment.md", text)
                output = self.root / "out.md"
                result, error = self.run_command("render", output)
                if valid:
                    self.assertEqual((0, ""), (result, error))
                    self.assertIn("```sh\n# comment\necho hello\n```", output.read_text())
                    self.assertIn("~~~sh\n## comment\n~~~", output.read_text())
                else:
                    self.assertEqual(1, result)
                    self.assertIn("fragment.md", error)

    def test_failed_release_leaves_files_untouched(self):
        original_log = self.log.read_text()
        empty_log = "# Changelog\n\n## Unreleased\n\n## 1.0.0\n<small>2020-01-01</small>\n"
        cases = (
            (empty_log, False, "2.0.0", "2026-01-01", "no release notes"),
            (original_log, True, "1.0.0", "2026-01-01", "already exists"),
            (original_log, True, "", "2026-01-01", "version X.Y.Z and date YYYY-MM-DD"),
            (original_log, True, "2.0", "2026-01-01", "version X.Y.Z and date YYYY-MM-DD"),
            (original_log, True, "2.0.0", "", "version X.Y.Z and date YYYY-MM-DD"),
            (original_log, True, "2.0.0", "2026-1-01", "version X.Y.Z and date YYYY-MM-DD"),
        )
        self.init()
        for log, with_fragment, version, date, message in cases:
            with self.subTest(version=version, date=date, message=message):
                self.log.write_text(log)
                fragments = [self.note("valid.md", "### Improvements\n\n- Note\n")] if with_fragment else []
                original = self.log.read_bytes()
                contents = {path: path.read_bytes() for path in fragments}
                result, error = self.run_command("release", version, date)
                self.assertEqual(1, result)
                self.assertIn(message, error)
                self.assertEqual(original, self.log.read_bytes())
                for path, content in contents.items():
                    self.assertEqual(content, path.read_bytes())

    def test_shallow_clone_refuses_release(self):
        self.init()
        with tempfile.TemporaryDirectory() as clone_dir:
            clone = Path(clone_dir) / "clone"
            self.git("clone", "--depth=1", f"file://{self.root}", str(clone))
            with patch.object(changelog, "ROOT", clone):
                result, error = self.run_command("release", "2.0.0", "2026-01-01")
        self.assertEqual(1, result)
        self.assertIn("release needs full Git history", error)


if __name__ == "__main__":
    unittest.main()
