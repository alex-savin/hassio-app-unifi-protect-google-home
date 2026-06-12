# UniFi Protect → Google Home Bridge

> This repository is a **Home Assistant add-on repository**. Add it from
> **Settings → Add-ons → Add-on Store → ⋮ → Repositories**:
>
> ```
> https://github.com/alex-savin/hassio-app-unifi-protect-google-home
> ```
>
> Then install the **UniFi Protect → Google Home** add-on. The add-on source
> lives in [`unifi-protect-google-home/`](unifi-protect-google-home/); see
> [its DOCS](unifi-protect-google-home/DOCS.md) for configuration.

A Home Assistant add-on, written in Go, that exposes UniFi Protect cameras to
Google Home as **native cameras** through the Smart Home Cloud-to-Cloud
`action.devices.traits.CameraStream` trait. Live video is delivered over
**WebRTC** with **zero transcoding** — RTP packets are forwarded directly from
the camera's H.264 elementary stream to the Google Home / Nest Hub /
Chromecast peer.

Inspired by [go2rtc](https://github.com/AlexxIT/go2rtc) (zero-copy stream
core) and [Scrypted](https://github.com/koush/scrypted) (Smart Home Action
fulfillment), but purpose-built for the UniFi Protect ↔ Google Home pair.

## Features

- **Native Google Home cameras.** Cameras appear as standard `CAMERA` devices;
  voice ("Hey Google, show the front door") and the Home app tile both work.
- **WebRTC passthrough.** No FFmpeg, no re-encode. H.264 NAL units from
  Protect's RTSPS feed are repackaged as WebRTC RTP and fanned out.
- **Auto-discovery.** A subscription to the Protect updates WebSocket fires
  `RequestSync` to Google Home Graph within ~½ second when a camera is added,
  renamed, or removed.
- **Online state push.** Online/offline transitions are pushed to Google via
  `ReportState` so the tile reflects connectivity without a poll cycle.
- **Single-tenant OAuth.** A built-in OAuth 2.0 authorization-code server
  brokers Google account linking. A password-gated consent page prevents
  arbitrary linkers.
- **Signed signaling URLs.** Each EXECUTE response includes a per-camera HMAC-
  SHA256-signed `cameraStreamSignalingUrl` with a 2-minute TTL.
- **Profile-aware track negotiation.** `profile-level-id` is derived from each
  camera's SPS NAL, so Main/High-profile streams (most G4/G5 cameras) negotiate
  successfully — no hardcoded baseline fallback.

## Limitations

- **amd64 only** for the published add-on image.
- **Video only.** Protect cameras emit AAC; WebRTC does not accept AAC, and
  transcoding is intentionally out of scope. Two-way talk is not implemented.
- **Single tenant.** One Protect controller, one Google project, one linked
  user. The consent password is the only gate.
- **Public reachability required.** Google must be able to reach your
  fulfillment URL over HTTPS.

## Architecture

```
                 ┌──────────────────────────────┐
                 │     UniFi Protect controller │
                 │   /api/bootstrap (cameras)   │
   REST + WSS    │   /ws/updates    (events)    │   RTSPS (H.264)
        ┌────────┤   :7441/<rtspAlias>          │◀───────────────┐
        │        └──────────────────────────────┘                │
        │                                                        │
        ▼                                                        │
┌───────────────┐   reconciler   ┌─────────────┐   RTP-passthru ┌┴───────────┐
│  unifi.Client │───────────────▶│ stream core │───────────────▶│ rtsp.Prod  │
└───────────────┘                └─────────────┘                └────────────┘
                                        │                              ▲
                                        │ TrackLocalStaticRTP fan-out  │
                                        ▼                              │
                                 ┌──────────────┐                      │
                                 │ pion WebRTC  │                      │
                                 │ peer factory │                      │
                                 └──────────────┘                      │
                                        ▲                              │
        HTTPS                           │                              │
   ┌────┴───────────────────────────────┴─────────────────────┐        │
   │  api.Server  /oauth/* /smarthome /webrtc/signal /healthz │        │
   └────┬─────────────────────────────────────────────────────┘        │
        │                              ▲                               │
        ▼                              │  RequestSync / ReportState    │
 ┌──────────────┐           ┌──────────┴────────────┐                  │
 │ Google Home  │──EXECUTE─▶│ ghome.HomeGraph (JWT) │                  │
 │ (SH C2C)     │           └───────────────────────┘                  │
 │   ─ SDP ──── signed signaling URL ── offer/answer ──────────────────┘
 └──────────────┘
```

### Packages

| Path | Role |
|---|---|
| [cmd/bridge](cmd/bridge) | Entrypoint, signal handling, reconcile loop |
| [internal/config](internal/config) | HA `options.json` loader + validation |
| [internal/unifi](internal/unifi) | Protect REST + WS updates client |
| [internal/streams](internal/streams) | Refcounted stream registry, RTP fan-out |
| [internal/rtsp](internal/rtsp) | gortsplib RTSPS client, SPS-driven track |
| [internal/webrtc](internal/webrtc) | pion peer factory, SDP offer/answer |
| [internal/oauth](internal/oauth) | OAuth 2.0 authorization-code server |
| [internal/ghome](internal/ghome) | SYNC/QUERY/EXECUTE + Home Graph client |
| [internal/api](internal/api) | HTTP server, bearer middleware, signed URLs |

## Public reachability

Google must reach your fulfillment URL over the public internet. The add-on
listens inside the container on `0.0.0.0:8099`, mapped to host port
`8199` by default; expose that host port via the same
external URL Home Assistant is already using (Nabu Casa, your own domain
behind nginx/Caddy/Traefik, etc.).

Reverse-proxy rule (Caddy example):

```caddy
your-ha-domain.example.com {
    handle_path /protect-gh/* {
        reverse_proxy http://homeassistant.local:8199
    }
}
```

The three URLs you'll register with Google are:

| Purpose | URL |
|---|---|
| Fulfillment | `https://your-ha-domain.example.com/protect-gh/smarthome` |
| OAuth authorization | `https://your-ha-domain.example.com/protect-gh/oauth/authorize` |
| OAuth token | `https://your-ha-domain.example.com/protect-gh/oauth/token` |

Set `bridge.public_base_url` to `https://your-ha-domain.example.com/protect-gh`.

## Google Cloud / Actions Console setup

You'll go through three consoles. Have these tabs open:
[Google Cloud](https://console.cloud.google.com/),
[Actions Console](https://console.actions.google.com/),
[Home Graph API](https://console.cloud.google.com/apis/library/homegraph.googleapis.com).

### 1. Create a Google Cloud project

1. Cloud Console → **New Project** → note the **Project ID**
   (e.g. `unifi-protect-gh`). This goes into `google.project_id`.
2. **APIs & Services → Library** → search **HomeGraph API** → **Enable**.

### 2. Create a service account for Home Graph

1. **IAM & Admin → Service Accounts → Create**.
   Name it `homegraph`. Skip the optional steps.
2. Open the account → **Keys → Add key → Create new key → JSON**.
   Download the JSON file. Its full contents go into
   `google.service_account_json` (paste the file's text, including the
   `-----BEGIN PRIVATE KEY-----` block).

> This service account is used only to mint short-lived OAuth tokens via
> RFC 7523 JWT-bearer; the bridge calls `homegraph.googleapis.com` with
> scope `https://www.googleapis.com/auth/homegraph` to push `RequestSync`
> and `ReportState`.

### 3. Create the Smart Home Action

1. [Actions Console](https://console.actions.google.com/) → **New project** →
   select the same Cloud project.
2. **Smart Home** → **Start building**.
3. **Develop → Actions** → set **Fulfillment URL** to
   `https://your-ha-domain.example.com/protect-gh/smarthome`.
4. **Develop → Account linking**:
   - Linking type: **OAuth → Authorization code**.
   - Client ID: pick any opaque string (e.g. `gh-link-`+random). This is the
     value of `google.oauth_client_id`.
   - Client secret: pick any long random string. This is
     `google.oauth_client_secret`.
   - Authorization URL: `https://your-ha-domain.example.com/protect-gh/oauth/authorize`
   - Token URL: `https://your-ha-domain.example.com/protect-gh/oauth/token`
   - Scopes: leave empty.
5. **Test → Reset sync** is your friend while debugging — it forces the next
   `RequestSync` to overwrite Home Graph state.

## Home Assistant add-on installation

1. In Home Assistant: **Settings → Add-ons → Add-on store → ⋮ → Repositories**
   and add this Git URL.
2. Open the new repository entry and install **UniFi Protect ↔ Google Home
   Bridge**.
3. Switch to the **Configuration** tab and fill in:

```yaml
unifi:
  host: "udm.local"          # UDM / UNVR hostname or IP
  username: "protect-bridge"
  password: "..."
  verify_tls: false          # most home setups have self-signed cert

google:
  project_id: "unifi-protect-gh"
  service_account_json: |
    {
      "type": "service_account",
      "project_id": "unifi-protect-gh",
      ...
    }
  oauth_client_id: "gh-link-9f2c…"
  oauth_client_secret: "…long-random…"

bridge:
  public_base_url: "https://your-ha-domain.example.com/protect-gh"
  listen_addr: "0.0.0.0:8099"
  stream_token_secret: ""  # leave blank: a strong secret is auto-generated and persisted
  consent_password: "…the password you'll type when linking…"
  agent_user_id: "unifi-protect-bridge"
```

> Tip: create a dedicated UniFi Protect user with **View Live Streams** and
> RTSP enabled. Don't reuse your admin password.

4. **Start** the add-on. Watch logs — you should see:
   ```
   loaded 4 camera(s)
   registered camera <id> (Front Door) 1600x1200
   protect ws: connected (lastUpdateId="…")
   ```

## Linking your Google account

1. Google Home app → **+ Add → Set up device → Works with Google**.
2. Find your action by name. Tap it.
3. You'll be redirected to
   `https://your-ha-domain.example.com/protect-gh/oauth/authorize?...` — a
   simple HTML consent page asking for the `consent_password` you set above.
4. Submit. The page redirects back to Google with an authorization code.
5. Google calls `/oauth/token`, then immediately fires a SYNC intent — your
   cameras appear in the Home app.

After this, any new camera you add to Protect appears in Google Home within
seconds (WS event → debounced bootstrap → `RequestSync`).

## Verifying a stream

```
"Hey Google, show the front door on the Living Room display."
```

If the Nest Hub stays black:

- **Profile mismatch.** Older code hardcoded `profile-level-id=42e01f`; this
  build derives it from the SPS. If you're still seeing failures, capture the
  Hub's `chrome://webrtc-internals` and check the inbound RTP stats.
- **NAT / ICE.** The bridge advertises `stun:stun.l.google.com:19302` by
  default. Cloud-to-Cloud WebRTC works best when the bridge has a routable
  UDP path; if you're double-NATed, add a TURN server to the factory config.
- **Signed URL expired.** Signaling URLs have a 2-minute TTL by design;
  Google should never hand one to a client late, but if you're testing by
  hand make sure you POST the offer within the window.

## Development

```bash
go build ./cmd/bridge        # ~19 MB static-ish binary
go test ./...                # decoders + integration tests
go vet ./...
```

Run locally with a JSON options file:

```bash
./bridge --options dev-options.json
```

`dev-options.json` mirrors the HA add-on config (same field names).

### Tests

- `internal/unifi/events_test.go` — Protect binary frame decoder (plain,
  zlib-deflated, empty-data variants).
- `internal/unifi/integration_test.go` — in-process fake Protect controller
  exercising the full login → bootstrap → WS subscribe → event round-trip
  path against the real `unifi.Client`.
- `internal/rtsp/h264_test.go` — SPS → `profile-level-id` table.

## Security notes

- The `/smarthome` endpoint requires a valid OAuth bearer token issued by
  the built-in OAuth server. Bearer issuance requires the `consent_password`.
- Signaling URLs are HMAC-SHA256-signed with `bridge.stream_token_secret` and
  expire after 2 minutes; they are single-use in practice because each
  EXECUTE mints a new one.
- The Home Graph service-account JSON is never logged. Keep your add-on
  configuration backup encrypted.
- TLS verification on the Protect controller is opt-in via
  `unifi.verify_tls`. Most UDM setups use self-signed certs; flip this on
  only if you've installed a trusted cert on the controller.

## License

MIT.
