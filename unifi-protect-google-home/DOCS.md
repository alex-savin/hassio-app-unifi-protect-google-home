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
| `stream_token_secret` | password | yes      | HMAC secret for signing per-camera signaling URLs                            |
| `consent_password`    | password | yes      | Password shown on the OAuth consent page                                     |
| `agent_user_id`       | string   | yes      | Stable opaque ID for the linked user (default `unifi-protect-bridge`)        |
| `log_level`           | enum     | no       | `debug`, `info`, `warn`, or `error` (default `info`)                         |

## Google Cloud / Actions Console setup

1. Create a project at <https://console.cloud.google.com>.
2. Enable the **HomeGraph API**.
3. In the **Actions on Google Console**, create a **Smart Home** project.
4. Set the **fulfillment URL** to `<public_base_url>/google/smarthome`.
5. Create OAuth credentials:
   - **Authorization URL**: `<public_base_url>/oauth/authorize`
   - **Token URL**: `<public_base_url>/oauth/token`
   - Scopes: leave default.
6. Create a service account, download the JSON key, paste it into
   `google.service_account_json`.
7. Link the account in the Google Home app via **Add device → Works with
   Google → search for your action name**.

## Ports

- `8099/tcp` — HTTP endpoint (fulfillment, OAuth, WebRTC signaling). Put a
  reverse proxy in front (NGINX Proxy Manager, Cloudflare Tunnel, HA's own
  reverse proxy, etc.) so that `public_base_url` resolves to it over HTTPS.

## Troubleshooting

- **Cameras don't appear** — Check `unifi.host`, credentials, and that the
  local account has Viewer access to Protect. Tail logs with `log_level:
  debug`.
- **Stream times out** — Verify your reverse proxy passes through WebSocket
  upgrades and that `public_base_url` is reachable from Google's servers.
- **OAuth fails** — Confirm the client ID/secret match the Actions Console
  values and that the redirect URI is exactly
  `<public_base_url>/oauth/callback`.
