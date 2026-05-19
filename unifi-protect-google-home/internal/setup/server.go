// Package setup serves an ingress-only configuration UI that lets the user
// discover a UniFi Protect controller on the LAN, validate credentials, and
// write the result back to the add-on options via the Supervisor API.
//
// The handlers are bound to a dedicated port (not the public 8099) so that
// the option-mutating endpoints are only reachable through Home Assistant's
// authenticated ingress proxy — never directly from the network.
package setup

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/config"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/discovery"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/unifi"
)

//go:embed setup.html
var indexHTML []byte

// Server hosts the ingress setup UI.
type Server struct {
	// SupervisorURL is the base URL for the Supervisor REST API. Defaults
	// to http://supervisor (the in-cluster DNS name used inside add-on
	// containers); override for tests.
	SupervisorURL string
	// Token is the Bearer token Supervisor injects via $SUPERVISOR_TOKEN.
	Token string
	// HTTP is the client used to call Supervisor. Defaults to a 15s client.
	HTTP *http.Client
	// ScanTimeout caps each UDP discovery run. Defaults to 5s.
	ScanTimeout time.Duration

	// status is a swappable provider of live runtime state. Wired via
	// SetStatus from cmd/bridge once bootstrap completes. nil until then
	// (the handler returns SetupMode=true).
	status atomic.Value // holds StatusProvider
}

// SetStatus registers a live status provider. Safe to call concurrently
// with HTTP handlers — the value is swapped atomically.
func (s *Server) SetStatus(p StatusProvider) {
	if s == nil {
		return
	}
	s.status.Store(statusBox{p})
}

// statusBox lets us store a typed nil through atomic.Value without panic.
type statusBox struct{ p StatusProvider }

func (s *Server) loadStatus() StatusProvider {
	v, _ := s.status.Load().(statusBox)
	return v.p
}

// New returns a Server configured from the standard add-on environment.
// Returns nil when $SUPERVISOR_TOKEN is unset — i.e. when running outside
// the HA Supervisor (local dev, tests) — so callers can branch on nil.
func New() *Server {
	tok := os.Getenv("SUPERVISOR_TOKEN")
	if tok == "" {
		return nil
	}
	return &Server{
		SupervisorURL: "http://supervisor",
		Token:         tok,
		HTTP:          &http.Client{Timeout: 15 * time.Second},
		ScanTimeout:   5 * time.Second,
	}
}

// Routes returns the http.Handler with all setup endpoints mounted.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.indexHandler)
	mux.HandleFunc("/api/status", s.statusHandler)
	mux.HandleFunc("/api/discover", s.discoverHandler)
	mux.HandleFunc("/api/validate", s.validateHandler)
	mux.HandleFunc("/api/save", s.saveHandler)
	return mux
}

// statusHandler returns a JSON snapshot of the running bridge. When no
// StatusProvider has been registered (bridge still initializing or running
// in setup-only mode), SetupMode is true and the other fields are zero.
func (s *Server) statusHandler(w http.ResponseWriter, r *http.Request) {
	var snap StatusSnapshot
	if p := s.loadStatus(); p != nil {
		snap = p.Status()
	} else {
		snap.SetupMode = true
	}
	// Best-effort: enrich with the add-on version from Supervisor. Failures
	// here are non-fatal — the rest of the snapshot is still useful.
	if s.Token != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if info, err := s.supervisorJSON(ctx, http.MethodGet, "/addons/self/info", nil); err == nil {
			if data, ok := info["data"].(map[string]any); ok {
				if v, _ := data["version"].(string); v != "" {
					snap.Version = v
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(indexHTML)
}

func (s *Server) discoverHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.ScanTimeout+5*time.Second)
	defer cancel()
	devs, err := discovery.Scan(ctx, s.ScanTimeout)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if devs == nil {
		devs = []discovery.Device{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": devs})
}

// validateRequest carries credentials submitted from the setup UI.
type validateRequest struct {
	Host      string `json:"host"`
	Username  string `json:"username"`
	Password  string `json:"password"`
	VerifyTLS bool   `json:"verify_tls"`
}

// validateResponse is returned by /api/validate and /api/save (on the
// validation pass that happens before the write).
type validateResponse struct {
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
	NVRVersion string `json:"nvr_version,omitempty"`
	NVRMAC     string `json:"nvr_mac,omitempty"`
	Cameras    int    `json:"cameras"`
	CloudUser  bool   `json:"cloud_user"`
}

// probe logs in and bootstraps the controller. Returns a populated
// validateResponse — ok=false means a user-visible error is in resp.Error.
func probe(ctx context.Context, req validateRequest) validateResponse {
	if req.Host == "" || req.Username == "" || req.Password == "" {
		return validateResponse{Error: "host, username and password are required"}
	}
	cli := unifi.New(config.UniFi{
		Host:      req.Host,
		Username:  req.Username,
		Password:  req.Password,
		VerifyTLS: req.VerifyTLS,
	})
	if err := cli.Login(ctx); err != nil {
		return validateResponse{Error: "login: " + err.Error()}
	}
	cams, _, err := cli.Bootstrap(ctx)
	if err != nil {
		return validateResponse{Error: "bootstrap: " + err.Error()}
	}
	v, mac := cli.NVRInfo()
	resp := validateResponse{
		OK:         true,
		NVRVersion: v,
		NVRMAC:     mac,
		Cameras:    len(cams),
		CloudUser:  cli.AuthUserCloudOnly(),
	}
	if resp.CloudUser {
		resp.OK = false
		resp.Error = "this is a Ubiquiti cloud (SSO) account; create a local UniFi OS admin user with at least Viewer access to Protect"
	}
	return resp
}

func (s *Server) validateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req validateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, validateResponse{Error: "bad request: " + err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, probe(ctx, req))
}

// saveRequest is the body of /api/save. When Restart is true the add-on
// container is restarted via Supervisor after the options write succeeds.
type saveRequest struct {
	validateRequest
	Restart bool `json:"restart"`
}

type saveResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func (s *Server) saveHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req saveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, saveResponse{Error: "bad request: " + err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Always validate before writing — refuse to persist credentials that
	// would put the add-on into a failure loop.
	pr := probe(ctx, req.validateRequest)
	if !pr.OK {
		writeJSON(w, http.StatusOK, saveResponse{Error: pr.Error})
		return
	}

	if err := s.writeOptions(ctx, req.validateRequest); err != nil {
		writeJSON(w, http.StatusInternalServerError, saveResponse{Error: err.Error()})
		return
	}
	if req.Restart {
		if err := s.restartAddon(ctx); err != nil {
			writeJSON(w, http.StatusInternalServerError, saveResponse{Error: "restart: " + err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, saveResponse{OK: true})
}

// writeOptions merges the supplied UniFi credentials into the current
// add-on options and POSTs the result back to Supervisor. Other option
// groups (google.*, bridge.*) are left untouched.
func (s *Server) writeOptions(ctx context.Context, req validateRequest) error {
	info, err := s.supervisorJSON(ctx, http.MethodGet, "/addons/self/info", nil)
	if err != nil {
		return fmt.Errorf("supervisor info: %w", err)
	}
	data, _ := info["data"].(map[string]any)
	opts, _ := data["options"].(map[string]any)
	if opts == nil {
		opts = map[string]any{}
	}
	unifiOpts, _ := opts["unifi"].(map[string]any)
	if unifiOpts == nil {
		unifiOpts = map[string]any{}
	}
	unifiOpts["host"] = req.Host
	unifiOpts["username"] = req.Username
	unifiOpts["password"] = req.Password
	unifiOpts["verify_tls"] = req.VerifyTLS
	opts["unifi"] = unifiOpts

	// Supervisor validates optional URL fields (url?) against a URL regex
	// when the value is an empty string instead of treating "" as unset.
	// Strip known url? fields when blank so the POST passes validation.
	if bridgeOpts, ok := opts["bridge"].(map[string]any); ok {
		if v, ok := bridgeOpts["public_base_url"].(string); ok && v == "" {
			delete(bridgeOpts, "public_base_url")
		}
	}

	body, _ := json.Marshal(map[string]any{"options": opts})
	if _, err := s.supervisorJSON(ctx, http.MethodPost, "/addons/self/options", body); err != nil {
		return fmt.Errorf("save options: %w", err)
	}
	return nil
}

func (s *Server) restartAddon(ctx context.Context) error {
	_, err := s.supervisorJSON(ctx, http.MethodPost, "/addons/self/restart", nil)
	return err
}

// supervisorJSON performs a Supervisor REST call and decodes the response.
// Returns the decoded JSON body or an error containing the upstream status.
func (s *Server) supervisorJSON(ctx context.Context, method, path string, body []byte) (map[string]any, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.SupervisorURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(bytes.TrimSpace(raw)))
	}
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return out, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ErrSupervisorUnavailable indicates the running process has no $SUPERVISOR_TOKEN.
var ErrSupervisorUnavailable = errors.New("supervisor token not set")
