# GitHub Actions production deployment

`Deploy Production` is a manual GitHub Actions workflow for the Oracle web
service. It deploys the prebuilt image
`ghcr.io/cartergillam/tronbyt-server:mobile-api`; it does not build or compile
the server on Oracle.

## Required GitHub configuration

Create a GitHub Environment named `production` for the server repository. Add
these environment secrets through **GitHub → Settings → Environments →
production → Environment secrets**:

- `ORACLE_HOST`: Oracle VM hostname or IP address.
- `ORACLE_USER`: SSH deployment user, normally `ubuntu`.
- `ORACLE_SSH_KEY`: the complete private key for that deployment user.
- `ORACLE_KNOWN_HOSTS`: the full, verified `known_hosts` line for the Oracle
  host key.

GitHub does not expose the private-key value after it is saved. Carter should
copy the private key directly from the secure local key file into the GitHub
secret field; it must never be committed, pasted into workflow YAML, sent to
the display, or added to the Compose override.

The VM must already be authenticated to pull the private GHCR image if the
package is private. The workflow does not send a registry credential to Oracle.

The Environment can later require reviewers before a deployment begins.

## Running a deployment

1. Ensure GitHub Actions has built and pushed the reviewed server image for
   `feature/mobile-api`.
2. Open **GitHub → Actions → Deploy Production → Run workflow**.
3. Select the reviewed server branch and paste the full 40-character apps SHA
   into `apps_commit`, for example
   `1b82c12d7259c46513cf4b181b291dacdb4e44ff`.
4. Run the workflow and wait for its `Tronbyt production deployment` summary.

The workflow accepts only a full hexadecimal Git SHA. It refuses to run if the
Oracle Compose directory, override file, or persistent apps checkout is
missing. The checkout is stored in Docker's named volume at
`/var/lib/docker/volumes/server_data/_data/system-apps`, mounted into the
container as `/app/data/system-apps`; it is not under the Compose directory.

Because Docker owns that volume, the deployment user reads it through narrowly
scoped `sudo -n` checks. Every Git inspection uses a one-shot
`-c safe.directory=/var/lib/docker/volumes/server_data/_data/system-apps`
argument and never changes global Git configuration. The workflow verifies the
checkout origin and branch before restarting the server, but deliberately does
not require its current commit to equal `apps_commit`: deployment is how that
checkout advances. These preflights prevent the workflow from initiating an
apps clone on the resource-constrained VM.

## What the workflow does

On Oracle it:

1. Backs up `docker-compose.override.yaml`.
2. Verifies the configured apps repository and branch, without printing the
   Compose environment.
3. Replaces only `SYSTEM_APPS_EXPECTED_COMMIT` with the supplied SHA.
4. Validates Compose quietly with `docker compose config -q`.
5. Records the running web image ID, pulls the prebuilt GHCR image, and records
   the pulled image ID and creation time.
6. Recreates only `web` with `--no-deps --force-recreate`.
7. Waits up to five minutes for Docker health, checks restart count, prints
   sanitized startup diagnostics, and requires the startup log to confirm the
   requested apps commit.

It never runs `docker build`, compiles Go, runs a repository-wide `git pull`,
or clones the apps repository. Oracle must remain a pull-and-recreate host;
GitHub Actions and GHCR perform all server-image builds.

## Reading the result

A successful run reports deployment time, old/new image IDs, apps commit,
health, service status, restart count, and override-backup path. It does not
print private keys, resolved Compose environment values, or provider secrets.

On failure, the workflow leaves persistent server data untouched and preserves
the override backup. It reports the previous image ID and explicit image-only
rollback commands. Review the failure before executing them; rollback is never
automatic.

## Manual rollback

Use the exact commands shown in the failed job summary. They restore the saved
Compose override and use a temporary Compose file that points `web` at the
recorded old image ID:

```sh
cd /home/ubuntu/tronbyt/server
cp -p docker-compose.override.yaml.deploy-<timestamp>.bak docker-compose.override.yaml
printf 'services:\n  web:\n    image: <old-image-id>\n' > .tronbyt-rollback-image.yaml
docker compose -f docker-compose.yaml -f docker-compose.override.yaml -f .tronbyt-rollback-image.yaml up -d --no-deps --force-recreate web
rm -f .tronbyt-rollback-image.yaml
```

Replace the placeholders only with values from the failed deployment summary.
Do not run image pruning before deciding whether a rollback is needed.
