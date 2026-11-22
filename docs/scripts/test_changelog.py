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
        self.log.write_text("# Changelog\n\n## Unreleased\n\n### Improvements\n\n- Existing\n\n## 1.0\n<small>2020-01-01</small>\n")

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

    def test_branches_release_in_merge_order(self):
        self.init()
        self.git("checkout", "-b", "first")
        first = self.note("z.md", "### Improvements\n\n- First\n")
        self.git("add", ".")
        self.git("commit", "-m", "first")
        self.git("checkout", "main")
        self.git("checkout", "-b", "second")
        second = self.note("a.md", "### Improvements\n\n- Second\n")
        self.git("add", ".")
        self.git("commit", "-m", "second")
        self.git("checkout", "main")
        self.git("merge", "--no-ff", "first", "-m", "merge first")
        self.git("merge", "--no-ff", "second", "-m", "merge second")
        self.assertEqual((0, ""), self.run_command("release", "2.0", "2026-01-01"))
        self.assertIn("## Unreleased\n\n## 2.0\n<small>2026-01-01</small>\n\n### Improvements\n\n- Existing\n- First\n- Second\n", self.log.read_text())
        self.assertFalse(first.exists())
        self.assertFalse(second.exists())

    def test_render_without_git_sorts_names_and_handles_missing_directory(self):
        output = self.root / "out.md"
        self.assertEqual((0, ""), self.run_command("render", output))
        self.assertIn("- Existing", output.read_text())
        self.note("z.md", "### New features\n\n- Z\n")
        self.note("a.md", "### New features\n\n- A\n")
        self.assertEqual((0, ""), self.run_command("render", output))
        self.assertIn("### New features\n\n- A\n- Z\n\n### Improvements", output.read_text())

    def test_release_uses_latest_filename_addition(self):
        self.init()
        self.note("été.md", "### Bug fixes\n\n- Unicode\n")
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
        self.note("later.md", "### Bug fixes\n\n- Re-added\n")
        self.git("add", ".")
        self.git("commit", "-m", "re-add")
        self.assertEqual((0, ""), self.run_command("release", "2.0", "2026-01-01"))
        self.assertIn("- Unicode\n- Cafe\n- Re-added", self.log.read_text())

    def test_invalid_sections_report_filename(self):
        for text in ("### Unknown\n\n- Note\n", "### Bug fixes\n\n- Note\n### Improvements\n- Other\n"):
            with self.subTest(text=text):
                self.note("invalid.md", text)
                result, error = self.run_command("render", self.root / "out.md")
                self.assertEqual(1, result)
                self.assertIn("invalid.md", error)

    def test_existing_version_and_write_failure_preserve_fragments(self):
        self.init()
        fragment = self.note("note.md", "### Bug fixes\n\n- Note\n")
        result, error = self.run_command("release", "1.0", "2026-01-01")
        self.assertEqual(1, result)
        self.assertIn("already exists", error)
        self.assertTrue(fragment.exists())
        with patch.object(Path, "write_text", side_effect=OSError("write failed")):
            result, error = self.run_command("release", "2.0", "2026-01-01")
        self.assertEqual(1, result)
        self.assertIn("write failed", error)
        self.assertTrue(fragment.exists())

    def test_release_rolls_back_after_partial_fragment_deletion(self):
        self.init()
        first = self.note("first.md", "### Bug fixes\n\n- First\n")
        second = self.note("second.md", "### Bug fixes\n\n- Second\n")
        original = self.log.read_bytes()
        unlink = Path.unlink

        def fail_second(path):
            if path == second:
                raise OSError("delete failed")
            unlink(path)

        with patch.object(Path, "unlink", fail_second):
            result, error = self.run_command("release", "2.0", "2026-01-01")
        self.assertEqual(1, result)
        self.assertIn("delete failed", error)
        self.assertEqual(original, self.log.read_bytes())
        self.assertEqual(b"### Bug fixes\n\n- First\n", first.read_bytes())
        self.assertEqual(b"### Bug fixes\n\n- Second\n", second.read_bytes())
        self.assertEqual((0, ""), self.run_command("release", "2.0", "2026-01-01"))

    def test_shallow_clone_refuses_release(self):
        self.init()
        with tempfile.TemporaryDirectory() as clone_dir:
            clone = Path(clone_dir) / "clone"
            self.git("clone", "--depth=1", f"file://{self.root}", str(clone))
            with patch.object(changelog, "ROOT", clone):
                result, error = self.run_command("release", "2.0", "2026-01-01")
        self.assertEqual(1, result)
        self.assertIn("release needs full Git history", error)


if __name__ == "__main__":
    unittest.main()
