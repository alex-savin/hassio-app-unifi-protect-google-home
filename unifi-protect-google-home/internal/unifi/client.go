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

// Client talks to a UniFi Protect controller.
type Client struct {
	cfg  config.UniFi
	base *url.URL
	http *http.Client

	mu        sync.Mutex
	csrfToken string
	loggedIn  bool
}

// New constructs a Client. Call Login before any other method.
func New(cfg config.UniFi) *Client {
	base := &url.URL{Scheme: "https", Host: cfg.Host}
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{
		Timeout: 30 * time.Second,
		Jar:     jar,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: !cfg.VerifyTLS}, //nolint:gosec
			IdleConnTimeout: 90 * time.Second,
		},
	}
	return &Client{cfg: cfg, base: base, http: hc}
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
	defer resp.Body.Close()
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
			resp.Body.Close()
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
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, "", fmt.Errorf("bootstrap: status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var bs bootstrapJSON
	if err := json.NewDecoder(resp.Body).Decode(&bs); err != nil {
		return nil, "", fmt.Errorf("bootstrap: decode: %w", err)
	}
	return bs.toCameras(), bs.LastUpdateID, nil
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
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("snapshot: status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// --- bootstrap JSON shape ---

type bootstrapJSON struct {
	LastUpdateID string       `json:"lastUpdateId"`
	Cameras      []cameraJSON `json:"cameras"`
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
			cam.Channels = append(cam.Channels, Channel{
				ID:            ch.ID,
				Name:          ch.Name,
				Width:         ch.Width,
				Height:        ch.Height,
				FPS:           ch.FPS,
				Bitrate:       ch.Bitrate,
				RTSPAlias:     ch.RTSPAlias,
				IsRTSPEnabled: ch.IsRTSPEnabled,
			})
		}
		out = append(out, cam)
	}
	return out
}
