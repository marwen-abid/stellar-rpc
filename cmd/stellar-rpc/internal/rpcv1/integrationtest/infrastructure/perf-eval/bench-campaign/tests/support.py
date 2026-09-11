"""Offline shell tests. Child processes get only explicitly allowed commands."""

import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

CAMPAIGN = Path(__file__).resolve().parents[1]
ROOT = next(p for p in CAMPAIGN.parents if (p / "go.mod").exists())


class ShellTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.work = Path(self.tmp.name)
        self.bin = self.work / "bin"
        self.bin.mkdir()
        # No inherited PATH, credentials, Git settings, or notification URLs.
        for name in ("bash", "jq", "date", "mktemp", "base64", "tr", "dirname",
                     "cat", "gzip", "wc", "grep", "tail", "sed", "tee", "cut",
                     "basename", "rm", "mkdir"):
            command = shutil.which(name)
            if command is None:
                self.fail(f"required test tool missing: {name}")
            (self.bin / name).symlink_to(command)
        self.env = {
            "PATH": str(self.bin), "HOME": str(self.work), "TMPDIR": str(self.work),
            "LC_ALL": "C", "AWS_EC2_METADATA_DISABLED": "true",
            "GITHUB_OUTPUT": str(self.work / "outputs"),
        }

    def stub(self, name, body):
        path = self.bin / name
        if path.is_symlink():
            path.unlink()
        path.write_text("#!/usr/bin/env bash\nset -euo pipefail\n" + body + "\n")
        path.chmod(0o755)

    def run_script(self, name, **env):
        return subprocess.run(
            [str(self.bin / "bash"), str(CAMPAIGN / name)],
            env=self.env | env, cwd=self.work, text=True, capture_output=True,
            timeout=15,
        )

    def outputs(self):
        path = self.work / "outputs"
        return dict(line.split("=", 1) for line in path.read_text().splitlines())
