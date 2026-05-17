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
	PublicBaseURL     string `json:"public_base_url"`
	ListenAddr        string `json:"listen_addr"`
	StreamTokenSecret string `json:"stream_token_secret"`
	ConsentPassword   string `json:"consent_password"`
	AgentUserID       string `json:"agent_user_id"`
	// LogLevel is one of "debug", "info", "warn", "error" (case-insensitive).
	// Empty defaults to "info".
	LogLevel string `json:"log_level"`
}

// HomeGraphEnabled returns true unless the option is explicitly set to false.
func (g Google) HomeGraphEnabled() bool {
	return g.EnableHomeGraph == nil || *g.EnableHomeGraph
}

// Load reads JSON config from path. Returns an error if required fields are missing.
func Load(path string) (*Config, error) {
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
	if err := c.validate(); err != nil {
		return nil, err
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
	if c.Bridge.StreamTokenSecret == "" {
		return fmt.Errorf("bridge.stream_token_secret is required (used to sign per-request stream URLs)")
	}
	if c.Bridge.ConsentPassword == "" {
		return fmt.Errorf("bridge.consent_password is required (gates Google account linking)")
	}
	return nil
}
