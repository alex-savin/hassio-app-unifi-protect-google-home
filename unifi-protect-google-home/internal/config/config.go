// with a YAML fallback for local development.
package config

import (
	"encoding/json"
	"fmt"
	"os"
)

type Config struct {
	UniFi  UniFi  `json:"unifi"  yaml:"unifi"`
	Google Google `json:"google" yaml:"google"`
	Bridge Bridge `json:"bridge" yaml:"bridge"`
}

type UniFi struct {
	Host      string `json:"host"`
	Username  string `json:"username"`
	Password  string `json:"password"`
	VerifyTLS bool   `json:"verify_tls"`
}

type Google struct {
	ProjectID          string `json:"project_id"`
	ServiceAccountJSON string `json:"service_account_json"`
	OAuthClientID      string `json:"oauth_client_id"`
	OAuthClientSecret  string `json:"oauth_client_secret"`
	// EnableHomeGraph controls whether RequestSync / ReportState calls are
	// issued. Pointer so we can distinguish "not set" (→ default true) from
	// an explicit false. When false the bridge still serves SYNC/EXECUTE
	// intents from Google, it just stops pushing updates to Home Graph.
	EnableHomeGraph *bool `json:"enable_homegraph,omitempty"`
}

type Bridge struct {
	PublicBaseURL string `json:"public_base_url"`
	ListenAddr    string `json:"listen_addr"`
	// StreamTokenSecret is the master signing secret. Leave blank to have
	// the bridge generate a strong random secret on first start and persist
	// it next to options.json — preferred over inventing one by hand.
	StreamTokenSecret string `json:"stream_token_secret"`
	ConsentPassword   string `json:"consent_password"`
	AgentUserID       string `json:"agent_user_id"`
	// LogLevel is one of "debug", "info", "warn", "error" (case-insensitive).
	// Empty defaults to "info".
	LogLevel string `json:"log_level"`
	// ExposedCameras is the allow-list of camera IDs that should be
	// advertised to Google Home. Empty or nil means "all cameras" for
	// backward compatibility with installs that pre-date this option.
	// Cameras not in the list are hidden from SYNC, return online=false
	// in QUERY, and EXECUTE rejects GetCameraStream for them.
	ExposedCameras []string `json:"exposed_cameras,omitempty"`
	// WSEventLog controls how chatty the Protect websocket logger is.
	// One of "off", "interesting" (default), "all".
	//   - off: never log per-camera ws events.
	//   - interesting: only log events that contain a field the bridge
	//     actually reacts to (state, isConnected, isAdopted, name,
	//     channels, lastRing, lastMotion, isMotionDetected). Filters out
	//     pure telemetry noise like uptime/lastSeen/phyRate/stats/nvrMac.
	//   - all: log every camera ws frame (legacy behavior, useful when
	//     diagnosing a regression on new firmware).
	WSEventLog string `json:"ws_event_log,omitempty"`
}

// HomeGraphEnabled returns true unless the option is explicitly set to false.
func (g Google) HomeGraphEnabled() bool {
	return g.EnableHomeGraph == nil || *g.EnableHomeGraph
}

// Load reads JSON config from path. Returns an error if required fields are missing.
func Load(path string) (*Config, error) {
	c, err := LoadPartial(path)
	if err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// LoadPartial reads JSON config from path and applies defaults but skips the
// strict required-field validation. It is used by the ingress setup UI so the
// add-on can start with empty/missing UniFi credentials and let the user fill
// them in via the browser.
func LoadPartial(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if c.Bridge.ListenAddr == "" {
		c.Bridge.ListenAddr = "0.0.0.0:8099"
	}
	if c.Bridge.AgentUserID == "" {
		c.Bridge.AgentUserID = "unifi-protect-bridge"
	}
	if c.Bridge.LogLevel == "" {
		c.Bridge.LogLevel = "info"
	}
	switch c.Bridge.WSEventLog {
	case "":
		c.Bridge.WSEventLog = "interesting"
	case "off", "interesting", "all":
		// ok
	default:
		c.Bridge.WSEventLog = "interesting"
	}
	return &c, nil
}

func (c *Config) validate() error {
	if c.UniFi.Host == "" {
		return fmt.Errorf("unifi.host is required")
	}
	if c.UniFi.Username == "" || c.UniFi.Password == "" {
		return fmt.Errorf("unifi credentials are required")
	}
	if c.Bridge.PublicBaseURL == "" {
		return fmt.Errorf("bridge.public_base_url is required (Google must reach the fulfillment URL)")
	}
	// StreamTokenSecret is intentionally NOT required: when blank the
	// bridge generates and persists a strong random secret itself.
	if c.Bridge.ConsentPassword == "" {
		return fmt.Errorf("bridge.consent_password is required (gates Google account linking)")
	}
	if len(c.Bridge.ConsentPassword) < 8 {
		return fmt.Errorf("bridge.consent_password must be at least 8 characters (it gates Google account linking on an internet-exposed endpoint)")
	}
	return nil
}
