# HTTPS readiness with Caddy

Keep the existing HTTP endpoint active until both the installed iOS build and
MatrixPortal S3 firmware have completed the TLS checklist below. The Oracle VM
continues to pull the GitHub Actions-built GHCR image; do not build the server
image on the VM.

## Network and DNS

1. Create an `A`/`AAAA` record for a dedicated hostname pointing to the Oracle
   VM. Do not place the current numeric IP in application code.
2. Allow inbound TCP 80 and TCP/UDP 443 in both the Oracle network security
   list and the VM firewall. Port 80 is needed for ACME HTTP validation and
   redirect; 443 serves HTTPS and optional HTTP/3.
3. Bind the Tronbyt container only on the private Compose network. Caddy is the
   public listener.

Example Caddy configuration:

```caddyfile
display.example.com {
    reverse_proxy web:8000
}
```

Caddy obtains and renews certificates automatically. Persist `/data` and
`/config`, monitor renewal logs, and back up the Caddy data volume separately
from Tronbyt's paired SQLite/WebP backup.

Set a narrow proxy trust boundary, matching the Compose network rather than the
public internet:

```dotenv
TRONBYT_TRUSTED_PROXIES=172.16.0.0/12
```

The server honors `X-Forwarded-For`, `X-Forwarded-Proto`,
`X-Forwarded-Host`, and `X-Forwarded-Port` only when the direct peer is in this
allowlist. With no allowlist, forwarded headers are ignored. `*` remains
available for controlled development but is not appropriate for production.

## Client validation before cutover

- Add the HTTPS hostname as a separate iOS server profile, confirm device and
  user keys, previews, mutations, location, and background/foreground refresh.
- Build firmware with the production CA bundle, configure the HTTPS Image URL
  on one USB-connected canary, and test certificate validation, DNS, HTTP 401,
  timeout recovery, polling, OTA URL handling, and power cycles.
- Confirm server request URLs and firmware logs contain no authorization data.
- Keep HTTP available on a restricted debug path until the canary has remained
  stable. Switch production URLs only after both clients pass; rollback by
  restoring the previous saved Image URL and iOS profile.
