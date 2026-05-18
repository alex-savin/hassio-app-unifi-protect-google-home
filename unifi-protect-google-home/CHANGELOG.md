## 0.3.7

- **Fix: HomeGraph `requestSync` 500 / cameras disappearing after restart.** OAuth access and refresh tokens were previously kept only in memory, so every add-on restart silently invalidated the tokens Google held for our integration. The first SYNC callback after restart would return `401` from `/smarthome`, the follow-up refresh would `400` from `/oauth/token`, and Google would purge the `agentUserId` binding — causing `requestSync` to fail with `500 INTERNAL` and the cameras to drop out of the Home app. Tokens are now stateless HMAC-signed strings (using `bridge.stream_token_secret`) with an embedded expiry; they survive restarts. Refresh tokens are valid for ten years, matching Google's expectation that they only expire when explicitly revoked.

## 0.3.6

- **HomeGraph startup diagnostics.** Bridge now logs the service account's `client_email` and `project_id` at startup, and warns when `google.project_id` (add-on option) disagrees with the `project_id` inside the service account JSON. The `requestSync: 500 INTERNAL` error from HomeGraph almost always means the service account belongs to a different GCP project than the one that owns the Smart Home action in the Actions Console — this surfaces the mismatch without needing to inspect the JSON manually.

## 0.3.5

- **Drop invalid `cameraStreamIceServers: ""` field from EXECUTE response.** Google's schema for `GetCameraStream` requires `cameraStreamIceServers` to be either omitted or a JSON-encoded array string. Sending an empty string is treated as a malformed state by some Home app builds and can cause the camera tile to silently fall back to "device details" UI instead of the live-preview tile.

## 0.3.4

- **Cameras now show live preview tiles in the Google Home app.** Set `cameraStreamSupportsPreview: true` in the SYNC attributes so each camera renders as an interactive tile with a live WebRTC preview when tapped in the Home app (previously they appeared as devices but could only be streamed via voice to a Cast display). Same signaling pipeline — no additional endpoints required.
- After updating, trigger a re-SYNC: unlink and re-link the service in the Home app (Settings → Works with Google → Unifi Protect Bridge → Unlink, then Add), or wait for HomeGraph's periodic refresh.

## 0.3.3

- **Fix black-screen streams (`write rtp: short buffer` flood).** The bridge previously forwarded RTSP RTP packets verbatim through pion's `TrackLocalStaticRTP`. pion's outbound RTP path has a hardcoded `outboundMTU = 1200` byte buffer, so UniFi Protect's ~1450-byte RTP packets failed every SRTP/ICE write with `io.ErrShortBuffer`. The display would show the green LIVE indicator but no frames. Switched to `TrackLocalStaticSample` fed by gortsplib's `rtph264.Decoder`: incoming RTP is depacketized into H.264 access units, SPS/PPS are injected before each IDR, and pion's built-in H.264 payloader re-fragments NAL units into FU-A packets that respect the 1200-byte MTU.
- Producer interface generalized from `[]*webrtc.TrackLocalStaticRTP` to `[]webrtc.TrackLocal` to support sample-based tracks.

## 0.3.2

- **WebRTC signaling: handle CORS preflight.** Chromecast / Nest Hub clients issue an `OPTIONS` request from `https://www.gstatic.com` before POSTing the SDP offer. The signaling handler now answers preflights with `Access-Control-Allow-*` headers and returns `204`, so streams start playing on Google smart displays. Without this, displays reported "Camera feed is not available".

## 0.3.1

- **Default host port changed from 8099 → 8199** to avoid collisions on hosts where 8099 is already taken. Update your reverse-proxy `proxy_pass` accordingly. The container still listens internally on 8099 (configurable via `bridge.listen_addr`).
- **Setup save fix**: `writeOptions` now drops the optional `bridge.public_base_url` from the payload when it's blank, so Supervisor's `url?` schema validator no longer rejects the save with `expected a URL`.

## 0.3.0

- **Ingress setup UI.** The add-on now ships an in-Home-Assistant configuration panel (Web UI button on the add-on page). It scans the local network for UniFi consoles via UBNT UDP discovery, lets the user pick one, validates credentials against the Protect API, then writes the result back to the add-on options through the Supervisor REST API and triggers a restart — no manual YAML editing required.
- New `internal/setup` package (`setup.html` + `Server`) serves `GET /`, `GET /api/discover`, `POST /api/validate`, `POST /api/save` on a dedicated ingress port (default `8100`). The public bridge port (`8099`) is untouched, so the option-mutating endpoints are reachable only through HA's authenticated ingress proxy.
- `config.yaml`: enabled `ingress: true` + `ingress_port: 8100` + `panel_icon`/`panel_title`, and added `hassio_api: true` / `hassio_role: manager` so the add-on can call `/addons/self/{info,options,restart}`.
- Setup-only mode: when `config.Load` fails (fresh install, blank UniFi credentials) and `$SUPERVISOR_TOKEN` is set, the bridge stays alive serving only the setup UI instead of exiting. Once the user saves valid settings the Supervisor restarts the add-on into normal operation.
- The save flow refuses to persist credentials that fail validation (bad login, bootstrap failure, or Ubiquiti cloud/SSO account), preventing crash-restart loops.

## 0.2.0

- UBNT UDP discovery (`internal/discovery`): pure-Go translation of the upstream `unifi-discovery` library — broadcasts V1+V2 to `255.255.255.255:10001` and `233.89.188.1:10001`, parses TLV responses (hw_addr, hostname, platform, model, product_name, version, uptime).
- New `GET /admin/discover` endpoint returns discovered UniFi consoles on the local network as JSON — useful for finding the correct `unifi.host` before reconfiguring the add-on.
- Cloud-user guardrail: after the initial bootstrap the bridge refuses to start when the configured UniFi account is a Ubiquiti SSO/cloud user (Protect rejects API access for cloud accounts). Clear error message points the user at creating a local admin account.
- DirectConnect (`*.ui.direct`) hostnames now automatically enable TLS verification regardless of `unifi.verify_tls` — those endpoints terminate on a publicly-trusted certificate.
- Bootstrap parser now also captures the NVR version + MAC and logs `connected to NVR <mac> (Protect <version>)` at startup.

## 0.1.1

- Make `bridge.public_base_url` optional in the schema (`url?`) so settings can be saved before a public URL is wired up. The bridge still validates the URL at startup when needed.

## 0.1.0

- Restructured as a Home Assistant add-on repository (add-on lives in `unifi-protect-google-home/`).
- Switched to s6-overlay supervision via `rootfs/etc/services.d/bridge` (`run` + `finish`).
- Converted all add-on manifests (`config`, `build`, `repository`) from JSON to YAML.
- Migrated CI/Release pipelines to the new Home Assistant Builder composite actions (`@2026.03.2`).
- Resolved all `golangci-lint` findings (errcheck, gocritic, staticcheck SA1019/S1016); `cmd/bridge` refactored into a `run() int` entrypoint.
- Removed deprecated `boot` field from `config.yaml` to satisfy the add-on linter.

## 0.0.1

- Initial release.
- WebRTC bridge between UniFi Protect cameras and Google Home (Cloud-to-Cloud CameraStream).
- OAuth fulfillment endpoint, SYNC + EXECUTE handlers, RTSP → WebRTC offer flow.
- Optional Home Graph SYNC requests via service account JWT.
- Configurable log level (`debug`, `info`, `warn`, `error`).
