package setup

// StatusSnapshot is the JSON payload returned by GET /api/status. It is a
// best-effort, point-in-time view of the running bridge. Fields stay zero
// when the bridge has not finished bootstrapping yet (setup-only mode).
type StatusSnapshot struct {
	// Version is the add-on version reported by Supervisor (`/addons/self/info`).
	// Filled in by the status handler, not the provider.
	Version string `json:"version,omitempty"`
	// SetupMode is true when the bridge has not loaded a valid config and
	// is only serving the setup UI. The frontend uses this to highlight
	// the configure-credentials section.
	SetupMode bool         `json:"setup_mode"`
	UniFi     UniFiStatus  `json:"unifi"`
	Cameras   []CameraInfo `json:"cameras"`
	Google    GoogleStatus `json:"google"`
	Bridge    BridgeStatus `json:"bridge"`
}

// UniFiStatus captures the configured controller and live connection state.
type UniFiStatus struct {
	Host       string `json:"host,omitempty"`
	Connected  bool   `json:"connected"`
	NVRMAC     string `json:"nvr_mac,omitempty"`
	NVRVersion string `json:"nvr_version,omitempty"`
}

// CameraInfo is one entry in StatusSnapshot.Cameras.
type CameraInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Model    string `json:"model,omitempty"`
	Online   bool   `json:"online"`
	Doorbell bool   `json:"doorbell"`
	// Exposed reports whether this camera is currently advertised to
	// Google Home. When false the camera is hidden from SYNC and refused
	// at EXECUTE — the user has explicitly opted it out of cloud
	// playback via the ingress "Camera exposure" panel.
	Exposed bool `json:"exposed"`
}

// GoogleStatus describes the Google Home / HomeGraph integration.
type GoogleStatus struct {
	// HomeGraphEnabled mirrors options.google.enable_homegraph (defaults true).
	HomeGraphEnabled bool `json:"homegraph_enabled"`
	// HomeGraphConfigured is true when a service-account JSON parsed successfully.
	HomeGraphConfigured bool   `json:"homegraph_configured"`
	ProjectID           string `json:"project_id,omitempty"`
	ServiceAccountEmail string `json:"service_account_email,omitempty"`
	// ProjectIDMismatch is true when google.project_id does not equal the
	// service-account's embedded project_id. RequestSync fails in this case.
	ProjectIDMismatch bool `json:"project_id_mismatch"`
	// OAuthConfigured is true when both client_id and client_secret are set.
	OAuthConfigured bool `json:"oauth_configured"`
}

// BridgeStatus carries bridge-level options that affect Google reachability.
type BridgeStatus struct {
	PublicBaseURL  string `json:"public_base_url,omitempty"`
	PublicURLSet   bool   `json:"public_url_set"`
	AgentUserID    string `json:"agent_user_id,omitempty"`
	ListenAddr     string `json:"listen_addr,omitempty"`
	SyncStateKnown bool   `json:"sync_state_known"`
	SyncFingerprint string `json:"sync_fingerprint,omitempty"`
	// WSEventLog mirrors the live bridge.ws_event_log setting so the UI
	// can pre-select the dropdown. One of "off", "interesting", "all".
	WSEventLog string `json:"ws_event_log,omitempty"`
}

// StatusProvider is implemented by the bridge to expose live runtime state
// to the setup UI. It is registered via Server.SetStatus and queried on
// every GET /api/status. Returning a fresh value each call is fine — the
// implementation can read from atomic snapshots cheaply.
type StatusProvider interface {
	Status() StatusSnapshot
}

// CameraAllowlistApplier is implemented by the running bridge so the
// setup UI can hot-apply changes to bridge.exposed_cameras without
// restarting the add-on. Implementations should update the in-memory
// allow-list (so SYNC/QUERY/EXECUTE filter immediately) and, when
// HomeGraph is wired, fire a RequestSync so Google re-pulls the device
// list. Persistence of the change to options.json is the responsibility
// of the setup server, not the applier.
type CameraAllowlistApplier interface {
	ApplyExposedCameras(ids []string)
}

// WSLogApplier is implemented by the running bridge so the setup UI can
// change bridge.ws_event_log (Protect websocket log verbosity) without
// a restart. Level is one of "off", "interesting", "all"; unknown values
// fall back to "interesting".
type WSLogApplier interface {
	ApplyWSEventLog(level string)
}
