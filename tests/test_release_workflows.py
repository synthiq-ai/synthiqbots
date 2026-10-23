"""Check binary publication without a GitHub token or installer builds."""

import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[1]
INSTALLERS = ("WoW-Voice-Command.dmg", "WoWVoiceCommand.exe")
PUBLISH_CONDITION = "github.ref == 'refs/heads/main' && github.event_name != 'pull_request'"


def workflow(name):
    return yaml.safe_load((ROOT / ".github/workflows" / name).read_text())


def step(job, name):
    return next(item for item in job["steps"] if item.get("name") == name)


class PublicationPolicyTests(unittest.TestCase):
    def test_binary_workflows_do_not_use_artifact_storage(self):
        for name in ("build-voice-command.yml", "build-feedback-daemon.yml"):
            with self.subTest(workflow=name):
                data = workflow(name)
                for job in data["jobs"].values():
                    for item in job["steps"]:
                        for action in ("upload-artifact", "download-artifact"):
                            self.assertNotIn("actions/" + action + "@", item.get("uses", ""))
                self.assertEqual(
                    data["concurrency"]["cancel-in-progress"],
                    "${{ github.event_name == 'pull_request' }}",
                )

    def test_only_main_can_publish(self):
        jobs = workflow("build-voice-command.yml")["jobs"]
        self.assertEqual(jobs["prepare-release"]["if"], PUBLISH_CONDITION)
        self.assertEqual(jobs["release"]["if"], PUBLISH_CONDITION)
        self.assertCountEqual(jobs["release"]["needs"], ["prepare-release", "build"])
        upload = step(jobs["build"], "Upload installers to staging release")
        self.assertEqual(upload["if"], PUBLISH_CONDITION)
        self.assertEqual(jobs["build"]["needs"], "prepare-release")
        self.assertIn("!cancelled()", jobs["build"]["if"])
        self.assertIn("needs.prepare-release.result == 'skipped'", jobs["build"]["if"])
        daemon = workflow("build-feedback-daemon.yml")["jobs"]["build"]
        for name in ("Stage release notes", "Publish rolling release feedback-daemon-latest"):
            self.assertEqual(step(daemon, name)["if"], PUBLISH_CONDITION)


class InstallerTransferTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.assets = self.root / "staging"
        self.assets.mkdir()
        for name in INSTALLERS:
            content = (name + " installer").encode()
            (self.assets / name).write_bytes(content)
            digest = hashlib.sha256(content).hexdigest()
            (self.assets / (name + ".sha256")).write_text(f"{digest}  {name}\n")
        self.log = self.root / "calls.jsonl"
        fake_gh = self.root / "gh"
        fake_gh.write_text(
            "#!/usr/bin/env python3\n"
            "import json, os, pathlib, shutil, sys\n"
            "args = sys.argv[1:]\n"
            "with open(os.environ['GH_CALLS'], 'a') as log:\n"
            "    log.write(json.dumps(args) + '\\n')\n"
            "if args[:2] == ['release', 'download']:\n"
            "    if os.environ.get('FAIL_DOWNLOAD'):\n"
            "        sys.exit(1)\n"
            "    dest = pathlib.Path(args[args.index('--dir') + 1])\n"
            "    dest.mkdir()\n"
            "    for asset in pathlib.Path(os.environ['STAGING_ASSETS']).iterdir():\n"
            "        shutil.copy(asset, dest / asset.name)\n"
            "elif args[:2] == ['release', 'create']:\n"
            "    if os.environ.get('FAIL_PUBLISH'):\n"
            "        sys.exit(1)\n"
            "    for asset in args[3:]:\n"
            "        if asset.startswith('dist/') and not pathlib.Path(asset).is_file():\n"
            "            sys.exit(1)\n"
        )
        fake_gh.chmod(0o755)
        self.env = {
            **os.environ,
            "PATH": str(self.root) + os.pathsep + os.environ["PATH"],
            "STAGING_TAG": "voice-command-build-test-1",
            "STAGING_ASSETS": str(self.assets),
            "GH_CALLS": str(self.log),
            "GITHUB_SHA": "a" * 40,
        }

    def run_publication(self):
        release = workflow("build-voice-command.yml")["jobs"]["release"]
        names = ("Download built installers", "Publish rolling release voice-command-latest")
        # Actions gives each step its own shell. Subshells preserve that boundary.
        script = "set -euo pipefail\n" + "\n".join(
            "(\n" + step(release, name)["run"] + "\n)" for name in names
        )
        result = subprocess.run(
            ["bash", "-c", script], cwd=self.root, env=self.env,
            capture_output=True, text=True,
        )
        calls = [json.loads(line) for line in self.log.read_text().splitlines()]
        return result, calls

    def assert_no_rolling_change(self):
        result, calls = self.run_publication()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual([call[:2] for call in calls], [["release", "download"]])

    def test_complete_installers_publish_and_remove_staging(self):
        result, calls = self.run_publication()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(
            [call[:3] for call in calls],
            [
                ["release", "download", self.env["STAGING_TAG"]],
                ["release", "delete", "voice-command-latest"],
                ["release", "create", "voice-command-latest"],
                ["release", "delete", self.env["STAGING_TAG"]],
            ],
        )
        for name in INSTALLERS:
            self.assertIn("dist/" + name, calls[2])
            self.assertIn("dist/" + name + ".sha256", calls[2])

    def test_missing_installer_preserves_rolling_release(self):
        (self.assets / INSTALLERS[1]).unlink()
        self.assert_no_rolling_change()

    def test_corrupt_installer_preserves_rolling_release(self):
        (self.assets / INSTALLERS[0]).write_bytes(b"corrupt")
        self.assert_no_rolling_change()

    def test_empty_checksum_preserves_rolling_release(self):
        (self.assets / (INSTALLERS[1] + ".sha256")).write_text("")
        self.assert_no_rolling_change()

    def test_failed_download_preserves_rolling_release(self):
        self.env["FAIL_DOWNLOAD"] = "1"
        self.assert_no_rolling_change()

    def test_failed_publication_keeps_staging_for_recovery(self):
        self.env["FAIL_PUBLISH"] = "1"
        result, calls = self.run_publication()
        self.assertNotEqual(result.returncode, 0)
        deletes = [call[2] for call in calls if call[:2] == ["release", "delete"]]
        self.assertNotIn(self.env["STAGING_TAG"], deletes)


if __name__ == "__main__":
    unittest.main()
