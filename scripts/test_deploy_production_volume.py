#!/usr/bin/env python3
"""Static checks for production deployment's mounted Apps checkout discovery."""

from pathlib import Path
import unittest


WORKFLOW = Path(__file__).resolve().parents[1] / ".github/workflows/deploy-production.yml"
APPS_COMMIT = "0b2395e8e8a9ce071c5689e6e3b4ff8af0690a09"


def remote_script() -> str:
    workflow = WORKFLOW.read_text(encoding="utf-8")
    start = workflow.index("<<'REMOTE'\n") + len("<<'REMOTE'\n")
    end = workflow.index("\n          REMOTE", start)
    return workflow[start:end]


class DeploymentVolumeValidationTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.workflow = WORKFLOW.read_text(encoding="utf-8")
        cls.remote = remote_script()

    def test_default_apps_revision_is_preserved(self) -> None:
        self.assertIn(f"default: {APPS_COMMIT}", self.workflow)
        self.assertIn('APPS_COMMIT="$(printf \'%s\' "$APPS_COMMIT" | tr', self.remote)

    def test_mount_source_is_discovered_and_validated(self) -> None:
        self.assertIn('docker compose ps -q web', self.remote)
        self.assertIn('eq .Destination "/app/data"', self.remote)
        self.assertIn('data_mount_type" != "volume"', self.remote)
        self.assertIn('sudo realpath -- "$data_dir"', self.remote)
        self.assertIn('sudo test -e "$system_apps_dir/.git"', self.remote)
        self.assertNotIn('test -d data/system-apps/.git', self.remote)

    def test_git_trust_is_limited_to_canonical_checkout(self) -> None:
        self.assertIn('sudo git -c "safe.directory=$system_apps_dir"', self.remote)
        self.assertNotIn("git config --global safe.directory", self.remote)
        self.assertNotIn("safe.directory=*", self.remote)
        self.assertIn('remote get-url origin)', self.remote)
        self.assertIn('branch --show-current)', self.remote)

    def test_existing_deployment_guards_remain(self) -> None:
        for check in (
            "docker compose pull web",
            "docker compose up -d --no-deps --force-recreate web",
            'health" = "healthy"',
            'status" = "running"',
            "System apps checkout ready",
            'commit=$APPS_COMMIT',
            "update_succeeded=true",
            "System apps checkout failed",
        ):
            self.assertIn(check, self.remote)


if __name__ == "__main__":
    unittest.main()
