package unifi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/config"
)

// IsDirectConnect reports whether host looks like a Ubiquiti DirectConnect
// cloud hostname (e.g. "abc123.ui.direct"). Those endpoints terminate on a
// publicly-trusted certificate, so TLS verification should always be on.
func IsDirectConnect(host string) bool {
	h := host
	if i := strings.IndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	return strings.HasSuffix(strings.ToLower(h), ".ui.direct")
}

// Client talks to a UniFi Protect controller.
type Client struct {
	cfg  config.UniFi
	base *url.URL
	http *http.Client
	// tlsVerify is the effective TLS-verification policy: cfg.VerifyTLS,
	// force-enabled for DirectConnect hosts. Shared by the HTTP transport
	// and the events websocket dialer so the two can never diverge.
	tlsVerify bool

	mu                sync.Mutex
	csrfToken         string
	loggedIn          bool
	authUserCloudOnly bool
	nvrVersion        string
	nvrMAC            string
}

// AuthUserCloudOnly returns true when the user authenticated against the
// controller is a Ubiquiti cloud (SSO) account rather than a local account.
// Protect rejects most API actions for cloud-only users, so the bridge
// refuses to start when this is the case. Populated by Bootstrap.
func (c *Client) AuthUserCloudOnly() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.authUserCloudOnly
}

// NVRInfo returns the controller version and MAC last seen during Bootstrap.
func (c *Client) NVRInfo() (version, mac string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nvrVersion, c.nvrMAC
}

// New constructs a Client. Call Login before any other method.
//
// When the configured host is a DirectConnect (*.ui.direct) cloud hostname,
// TLS verification is force-enabled regardless of cfg.VerifyTLS — those
// endpoints present a publicly-trusted cert.
func New(cfg config.UniFi) *Client {
	base := &url.URL{Scheme: "https", Host: cfg.Host}
	jar, _ := cookiejar.New(nil)
	verify := cfg.VerifyTLS || IsDirectConnect(cfg.Host)
	hc := &http.Client{
		Timeout: 30 * time.Second,
		Jar:     jar,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: !verify}, //nolint:gosec
			IdleConnTimeout: 90 * time.Second,
		},
	}
	return &Client{cfg: cfg, base: base, http: hc, tlsVerify: verify}
}

// Host returns the controller host (without scheme).
func (c *Client) Host() string { return c.cfg.Host }

// Login performs the UniFi OS login and captures session + CSRF tokens.
func (c *Client) Login(ctx context.Context) error {
	body, _ := json.Marshal(map[string]any{
		"username":   c.cfg.Username,
		"password":   c.cfg.Password,
		"token":      "",
		"rememberMe": false,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base.String()+"/api/auth/login", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("login: status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	c.mu.Lock()
	c.csrfToken = resp.Header.Get("X-CSRF-Token")
	c.loggedIn = true
	c.mu.Unlock()
	return nil
}

// ensureAuth re-logins if the session has been invalidated.
func (c *Client) ensureAuth(ctx context.Context) error {
	c.mu.Lock()
	ok := c.loggedIn
	c.mu.Unlock()
	if ok {
		return nil
	}
	return c.Login(ctx)
}

// do performs an authenticated request, refreshing the session once on 401.
func (c *Client) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	if err := c.ensureAuth(ctx); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, c.base.String()+path, body)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		if c.csrfToken != "" {
			req.Header.Set("X-CSRF-Token", c.csrfToken)
		}
		c.mu.Unlock()
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			_ = resp.Body.Close()
			c.mu.Lock()
			c.loggedIn = false
			c.mu.Unlock()
			if err := c.Login(ctx); err != nil {
				return nil, err
			}
			continue
		}
		// capture rotated CSRF token if present
		if t := resp.Header.Get("X-CSRF-Token"); t != "" {
			c.mu.Lock()
			c.csrfToken = t
			c.mu.Unlock()
		}
		return resp, nil
	}
	return nil, fmt.Errorf("unifi: exhausted retries")
}

// Bootstrap fetches the controller state.
func (c *Client) Bootstrap(ctx context.Context) ([]Camera, string, error) {
	resp, err := c.do(ctx, http.MethodGet, "/proxy/protect/api/bootstrap", nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, "", fmt.Errorf("bootstrap: status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var bs bootstrapJSON
	if err := json.NewDecoder(resp.Body).Decode(&bs); err != nil {
		return nil, "", fmt.Errorf("bootstrap: decode: %w", err)
	}

	cloudOnly := false
	if bs.AuthUserID != "" {
		for _, u := range bs.Users {
			if u.ID == bs.AuthUserID && hasCloudAccount(u.CloudAccount) {
				cloudOnly = true
				break
			}
		}
	}
	c.mu.Lock()
	c.authUserCloudOnly = cloudOnly
	c.nvrVersion = bs.NVR.Version
	c.nvrMAC = bs.NVR.MAC
	c.mu.Unlock()

	return bs.toCameras(), bs.LastUpdateID, nil
}

// hasCloudAccount returns true when the cloudAccount field is a non-null
// object. Local users have it set to JSON null (or absent).
func hasCloudAccount(raw json.RawMessage) bool {
	s := bytes.TrimSpace([]byte(raw))
	return len(s) > 0 && !bytes.Equal(s, []byte("null"))
}

// StreamURL builds the RTSPS URL for a camera+channel.
func (c *Client) StreamURL(_ Camera, ch Channel) (string, error) {
	if !ch.IsRTSPEnabled || ch.RTSPAlias == "" {
		return "", fmt.Errorf("rtsp disabled on channel %d", ch.ID)
	}
	host := c.cfg.Host
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	return fmt.Sprintf("rtsps://%s:7441/%s", host, ch.RTSPAlias), nil
}

// Snapshot fetches a JPEG snapshot for the camera.
func (c *Client) Snapshot(ctx context.Context, camID string) ([]byte, error) {
	resp, err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/proxy/protect/api/cameras/%s/snapshot?ts=%d", camID, time.Now().UnixMilli()), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("snapshot: status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// --- bootstrap JSON shape ---

type bootstrapJSON struct {
	LastUpdateID string       `json:"lastUpdateId"`
	AuthUserID   string       `json:"authUserId"`
	Cameras      []cameraJSON `json:"cameras"`
	Users        []userJSON   `json:"users"`
	NVR          nvrJSON      `json:"nvr"`
}

type userJSON struct {
	ID           string          `json:"id"`
	CloudAccount json.RawMessage `json:"cloudAccount"`
}

type nvrJSON struct {
	Version string `json:"version"`
	MAC     string `json:"mac"`
	Name    string `json:"name"`
}

type cameraJSON struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	MAC          string        `json:"mac"`
	ModelKey     string        `json:"modelKey"`
	Type         string        `json:"type"`
	State        string        `json:"state"`
	FeatureFlags featureFlags  `json:"featureFlags"`
	Channels     []channelJSON `json:"channels"`
}

type featureFlags struct {
	HasMic     bool `json:"hasMic"`
	HasSpeaker bool `json:"hasSpeaker"`
}

type channelJSON struct {
	ID            int    `json:"id"`
	Name          string `json:"name"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	FPS           int    `json:"fps"`
	Bitrate       int    `json:"bitrate"`
	RTSPAlias     string `json:"rtspAlias"`
	IsRTSPEnabled bool   `json:"isRtspEnabled"`
}

func (b bootstrapJSON) toCameras() []Camera {
	out := make([]Camera, 0, len(b.Cameras))
	for _, c := range b.Cameras {
		cam := Camera{
			ID:         c.ID,
			Name:       c.Name,
			MAC:        c.MAC,
			Model:      c.Type, // Protect uses "type" for friendly model name
			Type:       c.ModelKey,
			Online:     strings.EqualFold(c.State, "CONNECTED"),
			HasMic:     c.FeatureFlags.HasMic,
			HasSpeaker: c.FeatureFlags.HasSpeaker,
			Channels:   make([]Channel, 0, len(c.Channels)),
		}
		for _, ch := range c.Channels {
			cam.Channels = append(cam.Channels, Channel(ch))
		}
		out = append(out, cam)
	}
	return out
}
