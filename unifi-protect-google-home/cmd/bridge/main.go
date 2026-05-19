// Command bridge is the UniFi Protect → Google Home add-on entrypoint.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/api"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/config"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/discovery"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/ghome"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/hls"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/oauth"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/rtsp"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/setup"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/streams"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/unifi"
	wrtc "github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/webrtc"
)

const bootstrapInterval = 10 * time.Second

// slogWriter routes stdlib `log` output through slog so existing log.Printf
// call sites are subject to the configured level filter. Messages are emitted
// at Info — adjust individual call sites to use slog directly when finer
// granularity is needed.
type slogWriter struct{}

func (slogWriter) Write(p []byte) (int, error) {
	slog.Info(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func main() {
	os.Exit(run())
}

func run() int {
	optsPath := flag.String("options", "/data/options.json", "Path to HA add-on options.json")
	setupAddr := flag.String("setup-addr", "0.0.0.0:8100", "Listen address for the ingress setup UI")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Best-effort: start the ingress setup server whenever the Supervisor
	// token is available. It runs alongside the public bridge so users can
	// reconfigure credentials at any time. When the config below fails to
	// load (fresh install / missing UniFi creds), we stay alive on this
	// server alone until the user finishes the setup flow.
	setupSrv := setup.New()
	var setupHTTP *http.Server
	if setupSrv != nil {
		setupHTTP = &http.Server{
			Addr:              *setupAddr,
			Handler:           setupSrv.Routes(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			log.Printf("setup UI listening on %s (ingress)", *setupAddr)
			if err := setupHTTP.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("setup http: %v", err)
			}
		}()
		defer func() {
			shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = setupHTTP.Shutdown(shutCtx)
		}()
	}

	cfg, err := config.Load(*optsPath)
	if err != nil {
		log.Printf("config: %v", err)
		if setupSrv == nil {
			return 1
		}
		log.Printf("running in setup-only mode — open the add-on Web UI to configure UniFi Protect")
		<-ctx.Done()
		return 0
	}

	lvl := parseLogLevel(cfg.Bridge.LogLevel)
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})))
	log.SetFlags(0)
	log.SetOutput(slogWriter{})
	slog.Info("bridge starting", "log_level", lvl.String())

	uc := unifi.New(cfg.UniFi)
	if err := uc.Login(ctx); err != nil {
		log.Printf("unifi login: %v", err)
		return 1
	}

	registry := streams.NewRegistry()
	src := &cameraSource{}

	var hg *ghome.HomeGraph
	if cfg.Google.HomeGraphEnabled() {
		hg, err = ghome.NewHomeGraph(cfg.Google.ProjectID, []byte(cfg.Google.ServiceAccountJSON))
		if err != nil {
			log.Printf("homegraph: %v", err)
			return 1
		}
		if hg == nil {
			log.Printf("homegraph: no service account configured — RequestSync/ReportState disabled")
		} else {
			saProj := hg.ServiceAccountProjectID()
			log.Printf("homegraph: service account %s (sa project_id=%s, config project_id=%s)",
				hg.ServiceAccountEmail(), saProj, cfg.Google.ProjectID)
			if cfg.Google.ProjectID != "" && saProj != "" && cfg.Google.ProjectID != saProj {
				log.Printf("homegraph: WARNING google.project_id (%s) does not match service account project_id (%s) — requestSync will likely return 500 INTERNAL until they agree and that project owns the Smart Home action",
					cfg.Google.ProjectID, saProj)
			}
		}
	} else {
		log.Printf("homegraph: disabled via google.enable_homegraph=false")
	}
	rec := &reconciler{
		unifi:       uc,
		reg:         registry,
		src:         src,
		hg:          hg,
		agentUserID: cfg.Bridge.AgentUserID,
		verifyTLS:   cfg.UniFi.VerifyTLS,
	}

	if _, err := rec.refresh(ctx); err != nil {
		log.Printf("initial bootstrap: %v", err)
		return 1
	}
	if uc.AuthUserCloudOnly() {
		log.Printf("unifi: the configured account is a Ubiquiti cloud (SSO) user; " +
			"Protect rejects API access for cloud accounts. Create a local UniFi OS " +
			"admin account at https://<controller>/users and reconfigure the add-on.")
		return 1
	}
	if v, mac := uc.NVRInfo(); v != "" {
		log.Printf("unifi: connected to NVR %s (Protect %s)", mac, v)
	}
	log.Printf("loaded %d camera(s)", len(src.snapshot()))

	go rec.poll(ctx)

	oauthSrv := oauth.New(cfg.Google.OAuthClientID, cfg.Google.OAuthClientSecret, cfg.Bridge.ConsentPassword, []byte(cfg.Bridge.StreamTokenSecret))
	factory := wrtc.NewFactory()
	hlsSrv := hls.NewServer(src.rtspURLOf)
	defer hlsSrv.Shutdown()
	apiSrv := &api.Server{
		PublicBaseURL:     cfg.Bridge.PublicBaseURL,
		StreamTokenSecret: []byte(cfg.Bridge.StreamTokenSecret),
		OAuth:             oauthSrv,
		Fulfill:           &ghome.Handler{Source: src},
		Registry:          registry,
		WebRTC:            factory,
		HLS:               hlsSrv,
		Discover: func(ctx context.Context) ([]discovery.Device, error) {
			return discovery.Scan(ctx, 5*time.Second)
		},
	}
	src.signaling = apiSrv

	httpSrv := &http.Server{
		Addr:              cfg.Bridge.ListenAddr,
		Handler:           apiSrv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	srvErr := make(chan error, 1)
	go func() {
		log.Printf("listening on %s", cfg.Bridge.ListenAddr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			srvErr <- err
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-srvErr:
		log.Printf("http: %v", err)
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
		return 1
	}

	log.Printf("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutCtx)
	return 0
}

// reconciler refreshes the camera list from the UniFi bootstrap and pushes
// add/remove/state changes to Google Home Graph.
type reconciler struct {
	unifi       *unifi.Client
	reg         *streams.Registry
	src         *cameraSource
	hg          *ghome.HomeGraph
	agentUserID string
	verifyTLS   bool
}

func (r *reconciler) poll(ctx context.Context) {
	// Periodic safety net in case the WS misses an event.
	safety := time.NewTicker(bootstrapInterval)
	defer safety.Stop()

	// Debounced refresh trigger fed by the WS goroutine.
	trigger := make(chan struct{}, 1)
	fire := func() {
		select {
		case trigger <- struct{}{}:
		default:
		}
	}

	go r.watchEvents(ctx, fire)

	const debounce = 300 * time.Millisecond
	var pending *time.Timer
	for {
		select {
		case <-ctx.Done():
			if pending != nil {
				pending.Stop()
			}
			return
		case <-safety.C:
			if _, err := r.refresh(ctx); err != nil {
				log.Printf("bootstrap refresh: %v", err)
			}
		case <-trigger:
			if pending == nil {
				pending = time.AfterFunc(debounce, func() {
					if _, err := r.refresh(ctx); err != nil {
						log.Printf("bootstrap refresh (ws-triggered): %v", err)
					}
				})
			} else {
				pending.Reset(debounce)
			}
		}
	}
}

// watchEvents subscribes to the Protect updates WebSocket and signals fire()
// whenever a camera-relevant event arrives. It reconnects with exponential
// backoff (capped) until ctx is canceled.
func (r *reconciler) watchEvents(ctx context.Context, fire func()) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	lastUpdateID := r.src.lastUpdateID()

	for {
		if ctx.Err() != nil {
			return
		}
		ch, err := r.unifi.SubscribeEvents(ctx, lastUpdateID)
		if err != nil {
			log.Printf("protect ws: dial: %v (retry in %s)", err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
			continue
		}
		log.Printf("protect ws: connected (lastUpdateId=%q)", lastUpdateID)
		backoff = time.Second

		for ev := range ch {
			if ev.NewUpdateID != "" {
				lastUpdateID = ev.NewUpdateID
				r.src.setLastUpdateID(ev.NewUpdateID)
			}
			if ev.ModelKey != "camera" {
				continue
			}
			if ev.Action == "update" || ev.Action == "add" {
				keys := make([]string, 0, len(ev.Fields))
				for k := range ev.Fields {
					keys = append(keys, k)
				}
				log.Printf("protect ws: camera %s action=%s fields=%v", ev.ID, ev.Action, keys)
			}
			// Detect doorbell ring events. Protect signals these as an
			// update to the camera's `lastRing` field (an epoch-ms
			// timestamp). Push a Google Home ObjectDetection
			// notification so the Home app rings the user's phone.
			if ev.Action == "update" {
				if _, ok := ev.Fields["lastRing"]; ok {
					r.handleRing(ev.ID)
				}
			}
			// Connectivity transitions can show up under several field
			// names depending on Protect firmware: `state` ("CONNECTED"
			// / "DISCONNECTED"), `isConnected` (bool), or come in with
			// an "add" action when a camera re-adopts after a longer
			// outage. Handle them all.
			if ev.Action == "update" || ev.Action == "add" {
				if online, ok := decodeOnline(ev.Fields); ok {
					r.handleStateChange(ev.ID, online)
				}
			}
			// add/remove always triggers a refresh; for "update" only react if
			// fields we care about changed (state / name / channels / isConnected).
			if ev.Action == "update" {
				if _, ok := ev.Fields["state"]; !ok {
					if _, ok := ev.Fields["isConnected"]; !ok {
						if _, ok := ev.Fields["name"]; !ok {
							if _, ok := ev.Fields["channels"]; !ok {
								continue
							}
						}
					}
				}
			}
			fire()
		}
		log.Printf("protect ws: disconnected")
	}
}

// refresh pulls the current bootstrap snapshot and reconciles the registry
// + the ghome.Source snapshot. On membership change calls RequestSync; on
// online-state change calls ReportState. Returns the controller's
// lastUpdateID so the WS subscription can resume from the right cursor.
func (r *reconciler) refresh(ctx context.Context) (string, error) {
	cams, lastUpdateID, err := r.unifi.Bootstrap(ctx)
	if err != nil {
		return "", err
	}

	prev := r.src.snapshotMap()
	seen := make(map[string]struct{}, len(cams))
	ghomeCams := make([]ghome.Camera, 0, len(cams))
	onlineChanged := map[string]map[string]any{}

	for _, cam := range cams {
		seen[cam.ID] = struct{}{}
		next := ghome.Camera{
			ID:           cam.ID,
			Name:         cam.Name,
			Manufacturer: "Ubiquiti",
			Model:        cam.Model,
			Online:       cam.Online,
		}
		ghomeCams = append(ghomeCams, next)
		if p, had := prev[cam.ID]; had {
			if p.Online != next.Online {
				onlineChanged[cam.ID] = map[string]any{"online": next.Online}
			}
		} else {
			// First time we see this camera in this process — push initial
			// state so HomeGraph's cache is populated. The Test Suite reads
			// online from that cache, not from QUERY.
			onlineChanged[cam.ID] = map[string]any{"online": next.Online}
		}

		if _, exists := r.reg.Get(cam.ID); exists {
			continue
		}
		ch := cam.BestRTSPChannel()
		if ch == nil {
			log.Printf("camera %s (%s): no RTSP-enabled channel, skipping", cam.ID, cam.Name)
			continue
		}
		url, err := r.unifi.StreamURL(cam, *ch)
		if err != nil {
			log.Printf("camera %s: stream url: %v", cam.ID, err)
			continue
		}
		prod := rtsp.NewProducer(cam.ID, url, r.verifyTLS)
		r.reg.Put(streams.NewStream(cam.ID, prod))
		r.src.setRTSPURL(cam.ID, url, r.verifyTLS)
		log.Printf("registered camera %s (%s) %dx%d", cam.ID, cam.Name, ch.Width, ch.Height)
	}

	added := 0
	for id := range seen {
		if _, had := prev[id]; !had {
			added++
		}
	}
	removed := 0
	for _, name := range r.reg.Names() {
		if _, ok := seen[name]; !ok {
			r.reg.Delete(name)
			log.Printf("removed camera %s", name)
			removed++
		}
	}

	r.src.set(ghomeCams)

	if r.hg != nil {
		if added > 0 || removed > 0 {
			go func() {
				syncCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				if err := r.hg.RequestSync(syncCtx, r.agentUserID); err != nil {
					log.Printf("homegraph requestSync: %v", err)
				}
			}()
		}
		if len(onlineChanged) > 0 {
			go func() {
				rsCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				if err := r.hg.ReportState(rsCtx, r.agentUserID, onlineChanged); err != nil {
					log.Printf("homegraph reportState: %v", err)
				}
			}()
		}
	}
	r.src.setLastUpdateID(lastUpdateID)
	return lastUpdateID, nil
}

// decodeOnline extracts the online flag from a Protect WS update payload.
// It accepts either `state` (string "CONNECTED"/"DISCONNECTED"/...) or
// `isConnected` (bool). Returns (online, true) when the payload carries a
// connectivity signal and (false, false) otherwise.
func decodeOnline(fields map[string]json.RawMessage) (bool, bool) {
	if raw, ok := fields["state"]; ok {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return strings.EqualFold(s, "CONNECTED"), true
		}
	}
	if raw, ok := fields["isConnected"]; ok {
		var b bool
		if err := json.Unmarshal(raw, &b); err == nil {
			return b, true
		}
	}
	return false, false
}

// handleStateChange propagates a camera online/offline transition observed
// on the Protect updates WebSocket directly to HomeGraph, without waiting
// for the debounced bootstrap refresh. The Google Home Test Suite's
// OnlineOffline test polls QUERY and expects state to flip within a few
// seconds of the camera physically going offline.
func (r *reconciler) handleStateChange(camID string, online bool) {
	changed, cam, ok := r.src.setOnline(camID, online)
	if !ok || !changed {
		return
	}
	log.Printf("camera %s (%s) -> online=%v", camID, cam.Name, online)
	if r.hg == nil {
		return
	}
	states := map[string]map[string]any{camID: {"online": online}}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := r.hg.ReportState(ctx, r.agentUserID, states); err != nil {
			log.Printf("homegraph reportState (state %s): %v", cam.Name, err)
		}
	}()
}

// handleRing pushes a Google Home ObjectDetection notification for a
// doorbell press. Non-doorbell cameras are ignored. Errors are logged but
// not returned — a missed ring shouldn't crash the bridge.
func (r *reconciler) handleRing(camID string) {
	if r.hg == nil {
		return
	}
	cam, ok := r.src.snapshotMap()[camID]
	if !ok || !ghome.IsDoorbell(cam) {
		return
	}
	var nonce [8]byte
	_, _ = rand.Read(nonce[:])
	eventID := hex.EncodeToString(nonce[:])
	notifications := map[string]map[string]any{
		camID: {
			"ObjectDetection": map[string]any{
				"priority":           0,
				"detectionTimestamp": time.Now().UnixMilli(),
				"objects":            map[string]any{"named": []string{"Doorbell Press"}},
			},
		},
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := r.hg.Notify(ctx, r.agentUserID, eventID, notifications); err != nil {
			log.Printf("homegraph notify (ring %s): %v", cam.Name, err)
			return
		}
		log.Printf("doorbell ring %s -> notified", cam.Name)
	}()
}

// cameraSource is the ghome.Source implementation backed by an atomic
// snapshot of the latest bootstrap result.
type cameraSource struct {
	mu           sync.RWMutex
	cameras      []ghome.Camera
	rtspURLs     map[string]rtspEntry
	lastUpdateId string
	signaling    *api.Server
}

// rtspEntry is the per-camera info the HLS muxer needs to open upstream.
type rtspEntry struct {
	URL       string
	VerifyTLS bool
}

func (s *cameraSource) lastUpdateID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastUpdateId
}

func (s *cameraSource) setLastUpdateID(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	s.lastUpdateId = id
	s.mu.Unlock()
}

func (s *cameraSource) set(cams []ghome.Camera) {
	s.mu.Lock()
	s.cameras = cams
	s.mu.Unlock()
}

// setOnline flips the cached online flag for a single camera. Returns
// (changed, camera, exists). When the camera is not in the current snapshot
// exists is false and the caller should fall back to a full bootstrap.
func (s *cameraSource) setOnline(camID string, online bool) (bool, ghome.Camera, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, c := range s.cameras {
		if c.ID != camID {
			continue
		}
		if c.Online == online {
			return false, c, true
		}
		s.cameras[i].Online = online
		return true, s.cameras[i], true
	}
	return false, ghome.Camera{}, false
}

func (s *cameraSource) snapshot() []ghome.Camera {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ghome.Camera, len(s.cameras))
	copy(out, s.cameras)
	return out
}

// snapshotMap returns the current cameras keyed by ID for diff-friendly lookup.
func (s *cameraSource) snapshotMap() map[string]ghome.Camera {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]ghome.Camera, len(s.cameras))
	for _, c := range s.cameras {
		out[c.ID] = c
	}
	return out
}

func (s *cameraSource) ListCameras() []ghome.Camera { return s.snapshot() }

func (s *cameraSource) SignalingURL(camID string) (string, error) {
	return s.signaling.SignalingURL(camID)
}

func (s *cameraSource) HLSURL(camID string) (string, error) {
	return s.signaling.HLSURL(camID)
}

// setRTSPURL records the RTSP url + TLS-verify flag for a camera so the
// HLS muxer can look it up by ID.
func (s *cameraSource) setRTSPURL(camID, url string, verifyTLS bool) {
	s.mu.Lock()
	if s.rtspURLs == nil {
		s.rtspURLs = make(map[string]rtspEntry)
	}
	s.rtspURLs[camID] = rtspEntry{URL: url, VerifyTLS: verifyTLS}
	s.mu.Unlock()
}

// rtspURLOf is the hls.Source signature: returns the upstream RTSP URL
// and whether the upstream TLS certificate should be verified.
func (s *cameraSource) rtspURLOf(camID string) (string, bool, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.rtspURLs[camID]
	if !ok {
		return "", false, false
	}
	return e.URL, e.VerifyTLS, true
}
