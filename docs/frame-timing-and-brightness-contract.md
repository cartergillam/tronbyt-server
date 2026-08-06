# Frame timing and brightness contract

The frame response carries image bytes and device control metadata as separate
parts of one poll response. Reusing unchanged image bytes must not suppress
fresh brightness or dwell metadata.

## Clock minute boundary

For Clock, the server renders in the device timezone and treats the app's
`TRONBYT-NEXT-RENDER` timestamp as the hard cache expiry. A request at or after
that instant must render the new visible minute; it must not serve a prior
minute from cache. The returned dwell is capped so an active Clock frame does
not remain selected beyond the next device-local minute boundary.

Structured render logs record the UTC render timestamp, device-local timestamp,
timezone, next-render timestamp, cache decision, returned dwell, visible minute,
and render-context hash. The cache context includes configuration, timezone,
location, locale, and display capability inputs.

## Brightness

A successful device brightness update changes server state immediately. The
next device poll returns the effective brightness in the response headers even
when the frame content and frame hash are unchanged. Diagnostics expose both
`requestedBrightness` and `effectiveBrightness`; they should match after a
successful update.

After firmware `c9e3d77` is flashed, physical validation should show the panel
brightness change on the next completed poll without waiting for rotation or a
new image. Older firmware may defer applying response metadata until a frame
decode or transition, so behavior on older firmware is not evidence that the
server failed to publish the new value.

Similarly, the server can guarantee that an overdue Clock poll receives the new
minute and a bounded dwell, but continuous on-panel minute rollover still needs
validation with firmware `c9e3d77` because the device owns polling, queueing,
and final frame handoff timing.
