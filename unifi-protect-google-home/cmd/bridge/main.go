// Command bridge is the UniFi Protect → Google Home add-on entrypoint.
package main

import (
	"context"
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
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/ghome"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/oauth"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/rtsp"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/streams"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/unifi"
	wrtc "github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/webrtc"
)

const bootstrapInterval = 5 * time.Minute

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
	flag.Parse()

	cfg, err := config.Load(*optsPath)
	if err != nil {
		log.Printf("config: %v", err)
		return 1
	}

	lvl := parseLogLevel(cfg.Bridge.LogLevel)
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})))
	log.SetFlags(0)
	log.SetOutput(slogWriter{})
	slog.Info("bridge starting", "log_level", lvl.String())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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
	log.Printf("loaded %d camera(s)", len(src.snapshot()))

	go rec.poll(ctx)

	oauthSrv := oauth.New(cfg.Google.OAuthClientID, cfg.Google.OAuthClientSecret, cfg.Bridge.ConsentPassword)
	factory := wrtc.NewFactory()
	apiSrv := &api.Server{
		PublicBaseURL:     cfg.Bridge.PublicBaseURL,
		StreamTokenSecret: []byte(cfg.Bridge.StreamTokenSecret),
		OAuth:             oauthSrv,
		Fulfill:           &ghome.Handler{Source: src},
		Registry:          registry,
		WebRTC:            factory,
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
			// add/remove always triggers a refresh; for "update" only react if
			// fields we care about changed (state / name / channels).
			if ev.Action == "update" {
				if _, ok := ev.Fields["state"]; !ok {
					if _, ok := ev.Fields["name"]; !ok {
						if _, ok := ev.Fields["channels"]; !ok {
							continue
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
		if p, had := prev[cam.ID]; had && p.Online != next.Online {
			onlineChanged[cam.ID] = map[string]any{"online": next.Online, "status": "SUCCESS"}
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

// cameraSource is the ghome.Source implementation backed by an atomic
// snapshot of the latest bootstrap result.
type cameraSource struct {
	mu           sync.RWMutex
	cameras      []ghome.Camera
	lastUpdateId string
	signaling    *api.Server
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
