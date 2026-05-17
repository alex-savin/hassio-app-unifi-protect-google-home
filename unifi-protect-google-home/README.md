# UniFi Protect → Google Home

Bridge UniFi Protect cameras to Google Home via WebRTC (Cloud-to-Cloud
`CameraStream`). Cameras appear as native devices in the Google Home app with
live video delivered through RTP passthrough — no transcoding.

## Highlights

- Native Google Home cameras (voice + tile).
- WebRTC passthrough straight from Protect's RTSPS feed.
- Auto-discovery via Protect's updates WebSocket → Google `RequestSync`.
- Built-in OAuth authorization-code server with password-gated consent.
- Signed signaling URLs (HMAC-SHA256, 2-minute TTL).

## Requirements

- A Google Cloud project with a Smart Home action configured (Actions Console).
- An OAuth client (ID + secret) tied to that action.
- A service account JSON key for Home Graph (optional but recommended).
- A publicly reachable HTTPS URL for the add-on (reverse-proxy or HA external
  URL).

See the **Documentation** tab and the
[repository README](https://github.com/alex-savin/hassio-app-unifi-protect-google-home)
for full Actions Console setup.
