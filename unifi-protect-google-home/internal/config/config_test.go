package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig marshals m to a JSON file under t.TempDir() and returns its path.
func writeConfig(t *testing.T, m map[string]any) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	path := filepath.Join(t.TempDir(), "options.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// validOptions returns the minimal set of options that passes strict
// validation. Tests mutate a copy to knock out individual fields.
func validOptions() map[string]any {
	return map[string]any{
		"unifi": map[string]any{
			"host":     "192.168.1.1",
			"username": "bridge",
			"password": "protect-pass",
		},
		"google": map[string]any{
			"project_id": "test-project",
		},
		"bridge": map[string]any{
			"public_base_url":  "https://bridge.example.com",
			"consent_password": "longenough",
		},
	}
}

func TestLoadPartial_AppliesDefaults(t *testing.T) {
	c, err := LoadPartial(writeConfig(t, map[string]any{}))
	if err != nil {
		t.Fatalf("LoadPartial: %v", err)
	}
	if c.Bridge.ListenAddr != "0.0.0.0:8099" {
		t.Errorf("ListenAddr=%q, want 0.0.0.0:8099", c.Bridge.ListenAddr)
	}
	if c.Bridge.AgentUserID != "unifi-protect-bridge" {
		t.Errorf("AgentUserID=%q, want unifi-protect-bridge", c.Bridge.AgentUserID)
	}
	if c.Bridge.LogLevel != "info" {
		t.Errorf("LogLevel=%q, want info", c.Bridge.LogLevel)
	}
	if c.Bridge.WSEventLog != "interesting" {
		t.Errorf("WSEventLog=%q, want interesting", c.Bridge.WSEventLog)
	}
}

func TestLoadPartial_WSEventLogNormalization(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", "interesting"},
		{"off", "off"},
		{"interesting", "interesting"},
		{"all", "all"},
		{"verbose", "interesting"}, // unknown values normalize to the default
	}
	for _, tc := range tests {
		t.Run("ws_event_log="+tc.in, func(t *testing.T) {
			c, err := LoadPartial(writeConfig(t, map[string]any{
				"bridge": map[string]any{"ws_event_log": tc.in},
			}))
			if err != nil {
				t.Fatalf("LoadPartial: %v", err)
			}
			if c.Bridge.WSEventLog != tc.want {
				t.Errorf("WSEventLog=%q, want %q", c.Bridge.WSEventLog, tc.want)
			}
		})
	}
}

func TestLoad_RequiredFields(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(m map[string]any)
		wantErr string
	}{
		{
			name:    "missing unifi.host",
			mutate:  func(m map[string]any) { delete(m["unifi"].(map[string]any), "host") },
			wantErr: "unifi.host is required",
		},
		{
			name:    "missing unifi credentials",
			mutate:  func(m map[string]any) { delete(m["unifi"].(map[string]any), "username") },
			wantErr: "unifi credentials are required",
		},
		{
			name:    "missing bridge.public_base_url",
			mutate:  func(m map[string]any) { delete(m["bridge"].(map[string]any), "public_base_url") },
			wantErr: "bridge.public_base_url is required",
		},
		{
			name:    "missing bridge.consent_password",
			mutate:  func(m map[string]any) { delete(m["bridge"].(map[string]any), "consent_password") },
			wantErr: "bridge.consent_password is required",
		},
		{
			name:    "consent_password too short",
			mutate:  func(m map[string]any) { m["bridge"].(map[string]any)["consent_password"] = "short" },
			wantErr: "at least 8 characters",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := validOptions()
			tc.mutate(m)
			_, err := Load(writeConfig(t, m))
			if err == nil {
				t.Fatalf("Load succeeded, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err=%q, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoad_ValidConfig(t *testing.T) {
	// stream_token_secret is deliberately absent: blank is allowed because
	// the bridge generates and persists its own secret on first start.
	c, err := Load(writeConfig(t, validOptions()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Bridge.StreamTokenSecret != "" {
		t.Errorf("StreamTokenSecret=%q, want empty", c.Bridge.StreamTokenSecret)
	}
	if c.UniFi.Host != "192.168.1.1" {
		t.Errorf("UniFi.Host=%q, want 192.168.1.1", c.UniFi.Host)
	}
	if c.Bridge.ListenAddr != "0.0.0.0:8099" {
		t.Errorf("ListenAddr=%q, want default 0.0.0.0:8099", c.Bridge.ListenAddr)
	}
}

func TestHomeGraphEnabled(t *testing.T) {
	tests := []struct {
		name   string
		google map[string]any
		want   bool
	}{
		{"unset defaults to true", map[string]any{"project_id": "p"}, true},
		{"explicit false", map[string]any{"project_id": "p", "enable_homegraph": false}, false},
		{"explicit true", map[string]any{"project_id": "p", "enable_homegraph": true}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := validOptions()
			m["google"] = tc.google
			c, err := Load(writeConfig(t, m))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := c.Google.HomeGraphEnabled(); got != tc.want {
				t.Errorf("HomeGraphEnabled()=%v, want %v", got, tc.want)
			}
		})
	}
}
