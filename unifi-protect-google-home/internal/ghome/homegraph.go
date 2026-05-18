// Package ghome — Home Graph client.
//
// Authenticates with a Google service account (JSON key) using the OAuth 2.0
// JWT bearer flow (RFC 7523), caches the resulting access token, and posts to:
//
//	POST https://homegraph.googleapis.com/v1/devices:requestSync
//	POST https://homegraph.googleapis.com/v1/devices:reportStateAndNotification
//
// Stdlib-only: RSA-SHA256 signing via crypto/rsa, x509, encoding/pem.
package ghome

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	homeGraphScope     = "https://www.googleapis.com/auth/homegraph"
	googleTokenURL     = "https://oauth2.googleapis.com/token"
	homeGraphSyncURL   = "https://homegraph.googleapis.com/v1/devices:requestSync"
	homeGraphReportURL = "https://homegraph.googleapis.com/v1/devices:reportStateAndNotification"
	jwtBearerGrant     = "urn:ietf:params:oauth:grant-type:jwt-bearer"
)

// HomeGraph posts state updates / device additions to Google's Home Graph API.
type HomeGraph struct {
	ProjectID string

	sa   serviceAccount
	key  *rsa.PrivateKey
	http *http.Client

	mu          sync.Mutex
	accessToken string
	expiry      time.Time
}

// NewHomeGraph parses a Google service-account JSON key and returns a client.
// Returns nil and a nil error if saJSON is empty — callers can then skip
// Home Graph integration without crashing.
func NewHomeGraph(projectID string, saJSON []byte) (*HomeGraph, error) {
	if len(bytes.TrimSpace(saJSON)) == 0 {
		return nil, nil
	}
	var sa serviceAccount
	if err := json.Unmarshal(saJSON, &sa); err != nil {
		return nil, fmt.Errorf("homegraph: parse service account: %w", err)
	}
	if sa.ClientEmail == "" || sa.PrivateKey == "" {
		return nil, errors.New("homegraph: service account missing client_email or private_key")
	}
	key, err := parseRSAKey([]byte(sa.PrivateKey))
	if err != nil {
		return nil, fmt.Errorf("homegraph: parse private key: %w", err)
	}
	return &HomeGraph{
		ProjectID: projectID,
		sa:        sa,
		key:       key,
		http:      &http.Client{Timeout: 15 * time.Second},
	}, nil
}

// ServiceAccountEmail returns the client_email of the loaded service account,
// useful for startup logging and IAM diagnostics.
func (h *HomeGraph) ServiceAccountEmail() string { return h.sa.ClientEmail }

// ServiceAccountProjectID returns the project_id embedded in the service
// account JSON (which is the GCP project the SA authenticates against).
// This MUST match the Cloud Project ID of the Smart Home action in the
// Actions Console, otherwise requestSync returns 500 INTERNAL.
func (h *HomeGraph) ServiceAccountProjectID() string { return h.sa.ProjectID }

// RequestSync asks Google to re-run our SYNC intent (after add/remove).
func (h *HomeGraph) RequestSync(ctx context.Context, agentUserID string) error {
	body, _ := json.Marshal(map[string]any{
		"agentUserId": agentUserID,
		"async":       false,
	})
	return h.postAuthed(ctx, homeGraphSyncURL, body)
}

// ReportState pushes per-device state to Google.
// states: { deviceID: { "online": true, ... } }
func (h *HomeGraph) ReportState(ctx context.Context, agentUserID string, states map[string]map[string]any) error {
	if len(states) == 0 {
		return nil
	}
	body, _ := json.Marshal(map[string]any{
		"requestId":   newRequestID(),
		"agentUserId": agentUserID,
		"payload": map[string]any{
			"devices": map[string]any{
				"states": states,
			},
		},
	})
	return h.postAuthed(ctx, homeGraphReportURL, body)
}

// --- internals ---

func (h *HomeGraph) postAuthed(ctx context.Context, url string, body []byte) error {
	tok, err := h.token(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("homegraph: %s: %d %s", url, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// token returns a cached access token, refreshing 60s before expiry.
func (h *HomeGraph) token(ctx context.Context) (string, error) {
	h.mu.Lock()
	if h.accessToken != "" && time.Until(h.expiry) > time.Minute {
		t := h.accessToken
		h.mu.Unlock()
		return t, nil
	}
	h.mu.Unlock()

	assertion, err := h.signAssertion()
	if err != nil {
		return "", err
	}

	form := url.Values{}
	form.Set("grant_type", jwtBearerGrant)
	form.Set("assertion", assertion)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, googleTokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := h.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("homegraph: token exchange: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("homegraph: token exchange: %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("homegraph: token decode: %w", err)
	}
	if tr.AccessToken == "" {
		return "", errors.New("homegraph: empty access_token")
	}
	h.mu.Lock()
	h.accessToken = tr.AccessToken
	if tr.ExpiresIn > 0 {
		h.expiry = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	} else {
		h.expiry = time.Now().Add(50 * time.Minute)
	}
	h.mu.Unlock()
	return tr.AccessToken, nil
}

// signAssertion builds and RS256-signs the JWT bearer assertion.
func (h *HomeGraph) signAssertion() (string, error) {
	now := time.Now()
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	if h.sa.PrivateKeyID != "" {
		header["kid"] = h.sa.PrivateKeyID
	}
	claims := map[string]any{
		"iss":   h.sa.ClientEmail,
		"scope": homeGraphScope,
		"aud":   googleTokenURL,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
	hBytes, _ := json.Marshal(header)
	cBytes, _ := json.Marshal(claims)
	signingInput := base64.RawURLEncoding.EncodeToString(hBytes) + "." +
		base64.RawURLEncoding.EncodeToString(cBytes)

	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, h.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func parseRSAKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an RSA private key")
	}
	return rk, nil
}

func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// --- JSON shapes ---

type serviceAccount struct {
	Type         string `json:"type"`
	ProjectID    string `json:"project_id"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	ClientEmail  string `json:"client_email"`
	TokenURI     string `json:"token_uri"`
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
}
