# Documentation

## Configuration

All options live under three top-level groups: `unifi`, `google`, and
`bridge`.

### `unifi`

| Option       | Type     | Required | Description                                    |
|--------------|----------|----------|------------------------------------------------|
| `host`       | string   | yes      | Hostname or IP of the UniFi Protect controller |
| `username`   | string   | yes      | Local Protect account (NOT a Ubiquiti SSO user)|
| `password`   | password | yes      | Password for the above account                 |
| `verify_tls` | bool     | yes      | Verify the controller TLS certificate          |

Create a dedicated local user in **UniFi OS → Users → Add User → Limited
Admin → Viewer (Protect)**. SSO accounts will not authenticate via the API.

### `google`

| Option                  | Type     | Required | Description                                            |
|-------------------------|----------|----------|--------------------------------------------------------|
| `project_id`            | string   | yes      | Google Cloud project ID hosting the Smart Home action  |
| `service_account_json`  | string   | no       | Service account key (JSON) for Home Graph API          |
| `oauth_client_id`       | string   | yes      | OAuth 2.0 client ID configured in Actions Console      |
| `oauth_client_secret`   | password | yes      | OAuth 2.0 client secret                                |
| `enable_homegraph`      | bool     | no       | Send `RequestSync` / `ReportState` (default `true`)    |

If `enable_homegraph` is `false` (or `service_account_json` is empty),
discovery still works via cold `SYNC` requests, but online/offline pushes and
post-rename re-syncs are skipped.

### `bridge`

| Option                | Type     | Required | Description                                                                  |
|-----------------------|----------|----------|------------------------------------------------------------------------------|
| `public_base_url`     | url      | yes      | Public HTTPS URL (used in signed signaling URLs and OAuth redirect)          |
| `listen_addr`         | string   | yes      | Listen address (default `0.0.0.0:8099`)                                      |
| `stream_token_secret` | password | no       | Master HMAC secret for signing stream URLs and OAuth tokens. Leave blank — the bridge generates and persists a strong random secret on first start |
| `consent_password`    | password | yes      | Password shown on the OAuth consent page (minimum 8 characters)              |
| `agent_user_id`       | string   | yes      | Stable opaque ID for the linked user (default `unifi-protect-bridge`)        |
| `log_level`           | enum     | no       | `debug`, `info`, `warn`, or `error` (default `info`)                         |
| `exposed_cameras`     | list     | no       | Allow-list of camera IDs advertised to Google Home; empty = all cameras. Manageable from the ingress setup UI without a restart |
| `ws_event_log`        | enum     | no       | Protect websocket log verbosity: `off`, `interesting` (default), or `all`    |

## Google Cloud / Actions Console setup

1. Create a project at <https://console.cloud.google.com>.
2. Enable the **HomeGraph API**.
3. In the **Actions on Google Console**, create a **Smart Home** project.
4. Set the **fulfillment URL** to `<public_base_url>/smarthome`.
5. Create OAuth credentials:
   - **Authorization URL**: `<public_base_url>/oauth/authorize`
   - **Token URL**: `<public_base_url>/oauth/token`
   - Scopes: leave default.
6. Create a service account, download the JSON key, paste it into
   `google.service_account_json`.
7. Link the account in the Google Home app via **Add device → Works with
   Google → search for your action name**.

## Ports

- `8099/tcp` (container) — HTTP endpoint (fulfillment, OAuth, WebRTC
  signaling). Mapped to host port **8199** by default. Put a reverse proxy
  in front (NGINX Proxy Manager, Cloudflare Tunnel, HA's own reverse proxy,
  etc.) pointing at host port 8199 so that `public_base_url` resolves to it
  over HTTPS.

## Finding your UniFi console

Use the **built-in setup UI**. Open the add-on page in Home Assistant and
click **Open Web UI** (or use the *UniFi Protect* entry in the sidebar).
The page scans the local network via UBNT UDP discovery, lists every
console it finds, lets you pick one, validates the credentials against the
Protect API, and writes the result back into the add-on options — followed
by an automatic restart. No YAML editing required.

Discovery is only available through the ingress UI: the public port serves
exclusively the internet-facing endpoints Google needs (fulfillment, OAuth,
signed stream URLs), so it deliberately exposes no LAN-scanning routes.

## Troubleshooting

- **Cameras don't appear** — Check `unifi.host`, credentials, and that the
  local account has Viewer access to Protect. Tail logs with `log_level:
  debug`.
- **Stream times out** — Verify your reverse proxy passes through WebSocket
  upgrades and that `public_base_url` is reachable from Google's servers.
- **OAuth fails** — Confirm the client ID/secret match the Actions Console
  values. The redirect URI is Google's own
  (`https://oauth-redirect.googleusercontent.com/r/<project-id>`); the
  bridge only accepts redirects to that host, so no redirect URI needs to
  be configured on the bridge side.
