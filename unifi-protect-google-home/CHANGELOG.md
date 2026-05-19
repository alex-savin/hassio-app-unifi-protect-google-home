## 0.3.21

- **QUERY response logging.** v0.3.20 SYNC advertises `[progressive_mp4, webrtc]` correctly, but production logs show Google running SYNC + QUERY in quick succession and then never firing EXECUTE — strongly suggests QUERY is returning `online: false` for some cameras and the phone Home app then refuses to ask for a stream. The bridge now logs `ghome query: N device(s) [<id>=online|<id>=OFFLINE ...]` per QUERY so we can see exactly which devices Google thinks are unreachable.
- **`cameraStreamReceiverAppId` in EXECUTE.** Matched Scrypted exactly: progressive_mp4 EXECUTE responses now include `cameraStreamReceiverAppId: "00F7C5DD"` (the standard Google Cast Camera Stream receiver), in addition to `cameraStreamAccessUrl`, `cameraStreamAuthToken`, and `cameraStreamProtocol`. Phones use the Cast SDK to actually decode the MP4, so a missing receiver app id can silently break playback on some surfaces.
- **Lint cleanup.** Dropped use of the deprecated `fmp4.CodecH264` / `fmp4.NewSampleH264` symbols in `internal/mp4/muxer.go` in favour of the modern `mp4codecs.H264` type and `(*fmp4.Sample).FillH264` method. Unblocks `golangci-lint` (staticcheck SA1019) in CI.

## 0.3.20

- **Pivot from HLS to `progressive_mp4`.** The 0.3.17 HLS path turned out not to be reachable from the Pixel Home app: after v0.3.18 forced a HomeGraph RequestSync and v0.3.19 confirmed `cameraStreamSupportedProtocols: ["webrtc","hls"]` was making it into SYNC, the phone tile still never produced an EXECUTE — only the Hub Max did, and only ever for WebRTC. Cross-checked against the Scrypted google-home plugin and confirmed it advertises `["progressive_mp4","webrtc"]` with `cameraStreamNeedAuthToken: true` (hls/dash/smooth_stream are explicitly commented out as not consumed by cloud-to-cloud Smart Home actions). The bridge now matches that shape exactly.
- **`internal/mp4` package.** New on-demand RTSP→fragmented-MP4 muxer per camera, built on `mediacommon/v2` `pkg/formats/fmp4`. Opens an RTSPS session on the first subscriber, builds one ftyp+moov init segment from the discovered SPS/PPS, then fans an unbounded sequence of moof+mdat fragments out to every connected HTTP client. Auto-injects SPS/PPS in front of every IDR so the stream is seekable from any keyframe; closes the upstream session after 30 s of idle. Audio is dropped (consistent with Nest Cam preview behavior).
- **Bearer-token auth.** SYNC now declares `cameraStreamNeedAuthToken: true`. EXECUTE returns a clean URL (`/mp4/<camID>/stream.mp4`) plus a `cameraStreamAuthToken` of the form `<unix-exp>.<base64url-hmac>`. Google sends it as `Authorization: Bearer <token>` on the GET, the server verifies the HMAC against the camID embedded in the path, and the request is then handed to the muxer. This is the model the Smart Home spec actually documents — the path-token URLs we used for HLS were a fallback that some Google surfaces ignore.
- **EXECUTE routing.** Same rule as before but with `progressive_mp4` instead of `hls`: clients that list **only** `webrtc` (Cast / Hub Max) get WebRTC; everything else (phones, web) gets progressive_mp4. A request with no overlap returns `functionNotSupported`.
- **HLS route retained.** `internal/hls` and `/hls/...` are kept in the binary for now but are no longer advertised in SYNC or selectable from EXECUTE. They'll be removed in a future release if there is no use for them.

## 0.3.19

- **SYNC response logging.** Added `ghome sync: returning N device(s) cameraStreamSupportedProtocols=[...]` so we can confirm what attribute set the bridge actually advertises to Google on each SYNC. Without this it's impossible to tell from the logs whether HLS made it into the response or the running binary is still pre-0.3.17.

## 0.3.18

- **Startup HomeGraph RequestSync.** After 0.3.17 added HLS to `cameraStreamSupportedProtocols`, existing users saw no change in the phone Home app because Google's cached SYNC still listed only `webrtc` — and the reconciler only triggers RequestSync on camera add/remove, never on pure attribute changes. The bridge now fires one RequestSync per process startup (idempotent, well below HomeGraph's per-day budget) so capability changes propagate without requiring `"Hey Google, sync my devices"`. Look for `homegraph requestSync (startup): ok` in the log.

## 0.3.17

- **HLS support for the Google Home phone app.** Tapping a camera tile in the phone Home app previously returned `functionNotSupported` (as of 0.3.15) because the only protocol we advertised — WebRTC — is not in the phone's `SupportedStreamProtocols` list. The bridge now ships an embedded RTSP→HLS muxer (built on `gohlslib/v2`) and a new HMAC-signed route `/hls/<camID>/<exp>/<sig>/index.m3u8`. SYNC declares `cameraStreamSupportedProtocols: ["webrtc","hls"]` and EXECUTE branches per Scrypted's rule:
  - clients that list **only** `webrtc` (Cast / Hub Max) → existing WebRTC path;
  - everything else (phones, web, fallback surfaces) → HLS, returned as `cameraStreamProtocol: "hls"` with a per-camera signed `cameraStreamAccessUrl`.
- **Pragmatic HLS pipeline.** Each first request to a camera's playlist spins up a dedicated `gortsplib` session against the upstream Protect RTSP URL (TCP transport, honouring `unifi.verify_tls`), pulls H.264 access units, re-injects SPS/PPS in front of every IDR, and emits MPEG-TS segments via gohlslib (7-segment window, 1 s min duration). Per-camera muxers shut themselves down after 30 s of idle so we don't pin a Protect session for cameras nobody is watching. Token lives in the URL path so the relative segment URIs in the playlist inherit auth automatically.
- **No regression for Hub Max / Cast.** The WebRTC path is untouched — Hub Max still gets its existing `cameraStreamSignalingUrl`. Only surfaces that explicitly declined WebRTC are routed to HLS.



- **Intent-level log line.** `Handler.handle()` now logs `ghome intent: <intent> (reqID=...)` for every Smart Home POST so SYNC / QUERY / EXECUTE / DISCONNECT traffic is visible in the add-on log alongside the existing nginx access log. Closes a diagnostic gap discovered while investigating the blank phone tile.

## 0.3.15

- **Diagnostic logging for `GetCameraStream` requests.** `execute()` now parses and logs the `SupportedStreamProtocols` array Google sends per EXECUTE — the field that tells us what the calling surface (Hub Max vs. phone Home app vs. Chromecast) can play back. Logged as `ghome execute: GetCameraStream devices=[...] SupportedStreamProtocols=[...]`.
- **Honest error when client can't play WebRTC.** Previously we always returned `cameraStreamProtocol: "webrtc"` regardless of what the client said it supported. The Google Home phone app sends `SupportedStreamProtocols=["hls","dash",...]` — it does **not** list `webrtc` — so handing it a WebRTC signaling URL silently failed and the tile fell back to showing only the device info pane. EXECUTE now returns `errorCode: "functionNotSupported"` for those devices so the Home app stops trying instead of opening a dead tile. (Adding HLS support is the next step — see roadmap.)
- **`cameraStreamAuthToken: ""`** added to the WebRTC success states, matching Scrypted's google-home plugin shape; some receivers reportedly require the field even when empty.

## 0.3.14

- **Phone Home app camera feed.** Tapping a camera in the Google Home app on Android/iOS previously opened only the device info pane — no live video — even though the same camera streamed fine on Nest Hub Max. Root cause: the WebRTC signaling endpoint only accepted the RFC-style `{"type":"offer","sdp":"..."}` shape (what Cast/Hub Max uses). The phone Home app instead posts the Google Smart Home shape `{"action":"offer","sdp":"..."}` (matching how Scrypted's `google-home` plugin handles it). The signaling handler now accepts all three documented offer shapes (`type+sdp`, `action+sdp`, and `{"offer":"<sdp>"}`) and mirrors the matching response shape (`{"type":"answer",...}`, `{"action":"answer",...}`, or `{"answer":"..."}`) so each client decodes the answer it expects. Logs now record `signaling: camera <id> offer shape=<...> (sdp N bytes)` for visibility.
- **Dropped the bogus `cameraStreamSupportsPreview` SYNC attribute** — not part of the official `CameraStream` trait spec and may confuse some clients.

## 0.3.13

- **Camera “back online” detection hardened.** The Test Suite's OnlineOffline “make it online” step was timing out at ~40s because Protect doesn't always emit a `state` field on reconnect — some firmwares send `isConnected: true` or re-emit the camera under an `"add"` action instead of `"update"`. The WS handler now decodes online state via a `decodeOnline()` helper that accepts both `state` (`"CONNECTED"`/`"DISCONNECTED"`) and `isConnected` (bool), and reacts to both `"update"` and `"add"` actions. Also widened the field-filter so `isConnected`-only updates still trigger a refresh.
- **Safety-net bootstrap loop shortened from 60s to 10s** so the fallback path converges well within the Test Suite's window even when the WS misses an event entirely.
- **WS event logging.** Every camera-scoped WS frame now logs `protect ws: camera <id> action=<add|update> fields=[...]`, making it possible to diagnose which field Protect actually toggles on your firmware if any future regression appears.

## 0.3.12

- **Immediate online/offline propagation.** When the UniFi Protect updates WebSocket emits a camera `state` change, the bridge now decodes it directly from the WS frame and pushes a `ReportState({online})` to HomeGraph synchronously — no longer waiting for the debounced bootstrap re-fetch. The in-memory snapshot is also updated in place so subsequent `QUERY` intents see the new value within milliseconds. Fixes Google Home Test Suite “OnlineOffline” failing to observe the device going offline.
- **Safety-net bootstrap loop reduced from 5 min to 60 s** as a defence-in-depth fallback in case a WS event is missed.

## 0.3.11

- **Doorbell ring notifications.** The bridge now watches the UniFi Protect updates WebSocket for changes to a camera's `lastRing` field and pushes a Google Home `ObjectDetection` notification (`objects.named: ["Doorbell Press"]`) to HomeGraph. Combined with the `DOORBELL` device type and `notificationSupportedByAgent: true` from 0.3.10, this gives you a phone push and a doorbell-tile ring badge in the Home app when someone presses the G4 Doorbell. New `HomeGraph.Notify()` helper posts to `reportStateAndNotification` with an `eventId` for dedupe, and doorbells now also declare `action.devices.traits.ObjectDetection` in SYNC so Google knows to accept these events.

## 0.3.10

- **Polish pass against Scrypted's `google-home` plugin.** Added three things that plugin gets right and we were missing:
  - **`action.devices.DISCONNECT` intent handler.** When the user unlinks UniFi Protect from the Home app, Google posts a DISCONNECT to `/smarthome`. We were returning an empty body with no `requestId`; Google would log it as malformed. Now we 200 with the proper requestId echo.
  - **`notificationSupportedByAgent: true`** on every device in SYNC. Required for doorbell ring notifications and any future ObjectDetection events.
  - **`action.devices.types.DOORBELL`** for cameras whose model or name contains "doorbell" (your G4 Doorbell). The Home app renders these as a doorbell tile with a ring badge instead of a generic camera tile.

## 0.3.9

- **Fix: `reportStateAndNotification` 400 `INVALID_ARGUMENT`.** The reconciler was including `"status": "SUCCESS"` alongside `online` in each per-device state map. That key is part of the QUERY response schema, not ReportState — HomeGraph rejects the whole batch when it appears. ReportState payloads now contain only the actual state fields.

## 0.3.8

- **Fix: Test Suite "Device is not online before the test" failure.** The Google Smart Home Test Suite reads device online status from HomeGraph's ReportState cache, not by calling QUERY. Previously the bridge only pushed `reportStateAndNotification` when a camera's online state *changed*, so on a freshly linked account the cache was empty and every device appeared offline. The reconciler now pushes an initial ReportState entry for every camera the first time it observes it, populating the cache as soon as the bridge starts.

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
