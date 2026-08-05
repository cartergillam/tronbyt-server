# Production system-apps source

The server image does not contain the apps checkout. At container startup it
clones or refreshes the configured repository into the persistent data volume.
The Oracle VM should pull the GHCR server image built by GitHub Actions; it
should not build that image locally.

For the Phase 3 Clock and MLB fixes, set these values in the VM deployment
environment used by Docker Compose:

```dotenv
SYSTEM_APPS_REPO=https://github.com/cartergillam/apps.git
SYSTEM_APPS_REF=feature/mlb-clock-reliability
SYSTEM_APPS_EXPECTED_COMMIT=bbfcff4e02aae5ea50e60a961a5456e7af329da5
```

Commit `bbfcff4e0` contains Clock commit `bbfcff4e0` and has MLB commit
`4b284f197` as its parent. The expected-commit pin prevents production from
starting or refreshing against an unintended checkout. Move the pin only after
validating the replacement apps commit.

After GitHub Actions publishes the server image, deploy by pulling that image
and recreating the service with the same persistent data volume. Confirm one
startup record named `System apps checkout ready` contains:

- repository `https://github.com/cartergillam/apps.git`;
- ref `feature/mlb-clock-reliability`;
- commit `bbfcff4e02aae5ea50e60a961a5456e7af329da5`;
- `update_succeeded=true`.

Run the deployment assertion against the mounted checkout:

```sh
./scripts/verify-system-apps-commits.sh data/system-apps
```

The check fails unless both exact fix commits are ancestors of the resolved
checkout and both app entrypoints exist. It is safe to run in CI or after the
VM pulls the GHCR image; it does not build an image on the VM.

Then refresh Clock and MLB installations or clear their `LastRender` values
through the supported server action so they render from the verified checkout.
Do not edit the checkout inside the container or Oracle VM.
