// Command bridge is the UniFi Protect → Google Home add-on entrypoint.
// All domain logic lives in internal/bridge; this file is flags, config,
// construction, and lifecycle.
package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/api"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/bridge"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/config"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/ghome"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/hls"
	mp4srv "github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/mp4"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/oauth"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/setup"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/streams"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/unifi"
	wrtc "github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/webrtc"
)

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
	src := bridge.NewCameraSource(cfg.Bridge.ExposedCameras)
	if n := len(cfg.Bridge.ExposedCameras); n > 0 {
		log.Printf("camera exposure: %d camera(s) explicitly allowed via bridge.exposed_cameras", n)
	} else {
		log.Printf("camera exposure: bridge.exposed_cameras is empty — all cameras advertised to Google Home")
	}

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

	rec := bridge.NewReconciler(uc, registry, src, hg, cfg.Bridge.AgentUserID, cfg.UniFi.VerifyTLS, cfg.Bridge.WSEventLog)
	log.Printf("protect ws: event log level = %s", rec.WSEventLog())

	if _, err := rec.Refresh(ctx); err != nil {
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
	log.Printf("loaded %d camera(s)", len(src.Snapshot()))

	fulfill := &ghome.Handler{Source: src, AgentUserID: cfg.Bridge.AgentUserID}

	// Conditionally fire HomeGraph RequestSync on startup. We only call it
	// when the SYNC fingerprint (device list + advertised capabilities)
	// differs from the one we persisted at the last successful sync —
	// HomeGraph's per-project RequestSync quota is small (a few hundred
	// per day) and restart loops would otherwise exhaust it and earn a
	// 429 RESOURCE_EXHAUSTED, which is exactly what production logged on
	// 0.3.21. The reconciler still fires RequestSync on live add/remove
	// while the bridge is running.
	syncStatePath := filepath.Join(filepath.Dir(*optsPath), "sync_state.json")
	if hg != nil {
		go bridge.StartupRequestSync(hg, fulfill, cfg.Bridge.AgentUserID, syncStatePath)
	}

	// Expose live runtime state to the ingress setup UI so the side-panel
	// page can render NVR info, the live camera list, and Google linkage.
	if setupSrv != nil {
		setupSrv.SetStatus(bridge.NewStatus(cfg, uc, src, fulfill, hg, rec, syncStatePath))
		setupSrv.SetCameraApplier(bridge.NewCameraApplier(src, hg, cfg.Bridge.AgentUserID))
		setupSrv.SetWSLogApplier(bridge.NewWSLogApplier(rec))
	}

	go rec.Poll(ctx)

	// The master secret is auto-generated (32 random bytes persisted next
	// to options.json) when bridge.stream_token_secret is left blank, so
	// users are never pushed to invent a weak one. OAuth tokens and stream
	// URLs are signed with separate keys derived from it: a leaked stream
	// URL can never be parlayed into an OAuth token (or vice versa), even
	// though both ultimately come from one configured secret.
	master, err := bridge.LoadOrCreateSecret(cfg.Bridge.StreamTokenSecret,
		filepath.Join(filepath.Dir(*optsPath), "stream_secret"))
	if err != nil {
		log.Printf("stream secret: %v", err)
		return 1
	}
	oauthKey := bridge.DeriveKey(master, "oauth-tokens-v1")
	streamKey := bridge.DeriveKey(master, "stream-urls-v1")

	oauthSrv := oauth.New(cfg.Google.OAuthClientID, cfg.Google.OAuthClientSecret, cfg.Bridge.ConsentPassword, oauthKey)
	factory := wrtc.NewFactory()
	hlsSrv := hls.NewServer(src.RTSPURLOf)
	defer hlsSrv.Shutdown()
	mp4Srv := mp4srv.NewServer(src.RTSPURLOf)
	defer mp4Srv.Shutdown()
	apiSrv := &api.Server{
		PublicBaseURL:     cfg.Bridge.PublicBaseURL,
		StreamTokenSecret: streamKey,
		OAuth:             oauthSrv,
		Fulfill:           fulfill,
		Registry:          registry,
		WebRTC:            factory,
		HLS:               hlsSrv,
		MP4:               mp4Srv,
	}
	src.SetSignaling(apiSrv)

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
