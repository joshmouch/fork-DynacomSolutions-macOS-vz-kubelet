"""Exercise the existing workflow's release shell against a fake GitHub boundary."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
import re
import textwrap

WORKFLOW = Path(__file__).parent / "workflows/release.yml"
BASH = shutil.which("bash") if os.name != "nt" else "C:/Program Files/Git/bin/bash.exe"


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.workflow = WORKFLOW.read_text()
        self.steps = [
            {"name": name, "run": textwrap.dedent(body)}
            for name, body in re.findall(
                r"      - name: ([^\n]+)\n(?:        [^\n]+\n)*?        run: \|\n((?:          [^\n]*\n)+)",
                self.workflow)
        ]

    def test_shell_stages_are_bounded(self):
        for step in self.steps:
            if "run" in step:
                self.assertTrue(step["run"].startswith("with-limits -t 118s -- "))
                subprocess.run([BASH, "-n"], input=step["run"], text=True, check=True)
        self.assertGreaterEqual(len(self.steps), 6)
        self.assertIn("cancel-in-progress: false", self.workflow)
        self.assertIn("version: '=0.2.0'", self.workflow)

    def test_recovery_builds_the_original_immutable_tag(self):
        self.assertIn("workflow_dispatch:", self.workflow)
        self.assertIn("ref: ${{ inputs.release_tag || github.ref }}", self.workflow)
        verification = next(s for s in self.steps if s["name"] == "Verify immutable release source")
        self.assertIn('git rev-parse "refs/tags/${VERSION}^{commit}"', verification["run"])
        self.assertIn("release-${{ inputs.release_tag || github.ref_name }}", self.workflow)

    def test_release_uses_native_arm64_pool_without_extending_deadlines(self):
        self.assertIn("runs-on: macos-26", self.workflow)
        self.assertIn("timeout-minutes: 15", self.workflow)
        self.assertNotIn("GOARCH: amd64", self.workflow)

    def run_resume(self, response, exit_code=0, error=""):
        with tempfile.TemporaryDirectory() as root:
            root = Path(root)
            (root / "dist").mkdir()
            (root / "bin").mkdir()
            (root / "response").write_text("DRAFT=" + str(response.get("draft", False)).lower() + "\n")
            (root / "error").write_text(error)
            stub = root / "bin/gh"
            stub.write_text("#!/bin/bash\n"
                            'echo "$*" >> calls\n'
                            'if [ "$1" = api ]; then cat response; cat error >&2; exit '
                            + str(exit_code) + "; fi\n")
            stub.chmod(0o755)
            step = next(s for s in self.steps if s.get("name") == "Resume or create release draft")
            # Run the exact command body; production's limiter is tested by its owner.
            command = step["run"].removeprefix("with-limits -t 118s -- ")
            env = dict(os.environ, VERSION="v-test", GITHUB_REPOSITORY="owner/repo",
                       GITHUB_ENV="output", PATH=str(root / "bin") + os.pathsep + os.environ["PATH"])
            result = subprocess.run([BASH, "-c", command], cwd=root, env=env, capture_output=True, text=True)
            return result.returncode, (root / "output").read_text() if (root / "output").exists() else "", (root / "calls").read_text()

    def test_existing_published_release_is_not_recreated(self):
        status, output, calls = self.run_resume({"draft": False})
        self.assertEqual(status, 0)
        self.assertEqual(output.strip(), "DRAFT=false")
        self.assertNotIn("release create", calls)

    def test_existing_draft_resumes(self):
        status, output, calls = self.run_resume({"draft": True})
        self.assertEqual(status, 0)
        self.assertEqual(output.strip(), "DRAFT=true")
        self.assertNotIn("release create", calls)

    def test_only_not_found_creates_draft(self):
        status, output, calls = self.run_resume({}, 1, "gh: Not Found (HTTP 404)")
        self.assertEqual(status, 0)
        self.assertEqual(output.strip(), "DRAFT=true")
        self.assertIn("--draft --verify-tag", calls)

    def test_auth_failure_is_not_absence(self):
        status, output, calls = self.run_resume({}, 1, "gh: Unauthorized (HTTP 401)")
        self.assertNotEqual(status, 0)
        self.assertEqual(output, "")
        self.assertNotIn("release create", calls)


if __name__ == "__main__":
    unittest.main()
