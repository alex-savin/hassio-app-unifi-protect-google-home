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
