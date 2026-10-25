#!/usr/bin/env python3
"""Static checks for production deployment's mounted Apps checkout discovery."""

from pathlib import Path
import unittest


WORKFLOW = Path(__file__).resolve().parents[1] / ".github/workflows/deploy-production.yml"
APPS_COMMIT = "a0746987a714c18632ba10169d809b51ea191cd5"


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
            'docker pull "$image_ref"',
            'docker compose -f docker-compose.yaml -f "$override_file" -f "$image_override" up -d --no-deps --force-recreate web',
            'health" = "healthy"',
            'status" = "running"',
            "System apps checkout ready",
            'commit=$APPS_COMMIT',
            "update_succeeded=true",
            "System apps checkout failed",
            "deployed_image_id",
            "NEW_IMAGE_DIGEST",
        ):
            self.assertIn(check, self.remote)

    def test_immutable_digest_is_required_and_verified(self) -> None:
        self.assertIn("image_digest:", self.workflow)
        self.assertIn("sha256:acfab7dfc4abaecae4701aac33d375a1bb5ace5cbb79256882e1461d202d2001", self.workflow)
        self.assertIn("SERVER_IMAGE_DIGEST", self.workflow)
        self.assertIn("image_digest_ref", self.remote)
        self.assertNotIn("docker compose pull web", self.remote)

    def test_database_and_asset_backups_are_verified(self) -> None:
        for check in (
            "source.backup(target)",
            "PRAGMA integrity_check",
            "persistent-assets.tar.gz",
            "tar -tzf",
            "sha256sum -c SHA256SUMS",
            "compose-resolved.yaml",
            "current_apps_sha",
            "device_poll_state",
            "mode 600",
        ):
            self.assertIn(check, self.remote)


if __name__ == "__main__":
    unittest.main()
