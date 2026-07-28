# Future custom deployment flow

This is preparation only. Do not run deployment commands until the Oracle VM,
its Compose/service configuration, volume mounts, database engine, image
registry, and health-check URL have been audited.

1. Create a reviewed, versioned Git commit and record its full SHA. Tagging and
   pushing require confirmation.
2. Record the currently running image digest and service configuration.
3. Back up the database using the database engine's consistent backup procedure.
   Also snapshot the existing persistent data volume. Verify both artifacts are
   readable before changing the service.
4. Build a custom image from that commit, or have CI build and publish an
   immutable image. Record the resulting digest; deploy by digest rather than a
   mutable tag.
5. Reuse the existing persistent data volume and existing secret injection. Do
   not copy a development `.env` or database to the VM.
6. Start the new image using the audited orchestration command.
7. Wait for the existing health check to pass, then run authenticated smoke tests:
   `/health`, existing `/v0/devices`, catalogue listing, one device preview, and
   a read-only installation config request. Confirm Manager login and the device
   `/next` route separately.
8. Watch application/database logs, render failures, device polling, and
   WebSocket reconnects.
9. If checks fail, restore the previous image digest without changing the volume.
   If a migration is not backward compatible, stop the service and restore the
   verified database/volume backup before starting the prior digest.

Commands for backup, image build/publish, service update, health checks, and
rollback are intentionally not specified yet. They require confirmation after
the VM audit establishes the real database and deployment topology.

