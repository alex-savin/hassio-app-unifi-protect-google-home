package bridge

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"time"

	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/config"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/ghome"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/setup"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/unifi"
)

// syncStateFile is the on-disk shape of the persisted SYNC fingerprint.
type syncStateFile struct {
	Fingerprint string `json:"fingerprint"`
	UpdatedAt   string `json:"updated_at"`
}

// StartupRequestSync fires a HomeGraph RequestSync iff the current SYNC
// fingerprint differs from the previously persisted one. On success the
// new fingerprint is written to disk so subsequent restarts skip the call
// and stay well under HomeGraph's per-project quota.
func StartupRequestSync(hg *ghome.HomeGraph, fulfill *ghome.Handler, agentUserID, statePath string) {
	current := fulfill.SyncFingerprint()

	prev := ""
	if data, err := os.ReadFile(statePath); err == nil {
		var st syncStateFile
		if json.Unmarshal(data, &st) == nil {
			prev = st.Fingerprint
		}
	}

	if prev == current {
		log.Printf("homegraph requestSync (startup): skipped, fingerprint unchanged (%s)", current[:12])
		return
	}

	syncCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := hg.RequestSync(syncCtx, agentUserID); err != nil {
		log.Printf("homegraph requestSync (startup): %v", err)
		return
	}
	log.Printf("homegraph requestSync (startup): ok (fingerprint %s -> %s)", truncFP(prev), current[:12])

	out, _ := json.MarshalIndent(syncStateFile{
		Fingerprint: current,
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
	if err := os.WriteFile(statePath, out, 0o600); err != nil {
		log.Printf("homegraph requestSync (startup): warning, could not persist fingerprint to %s: %v", statePath, err)
	}
}

func truncFP(fp string) string {
	if fp == "" {
		return "(none)"
	}
	if len(fp) < 12 {
		return fp
	}
	return fp[:12]
}

// CameraApplier is the setup.CameraAllowlistApplier implementation: it
// swaps the in-memory allow-list and, when HomeGraph is wired, fires a
// RequestSync so Google re-pulls the SYNC device list immediately.
type CameraApplier struct {
	src         *CameraSource
	hg          *ghome.HomeGraph
	agentUserID string
}

// NewCameraApplier wires a CameraApplier. hg may be nil.
func NewCameraApplier(src *CameraSource, hg *ghome.HomeGraph, agentUserID string) *CameraApplier {
	return &CameraApplier{src: src, hg: hg, agentUserID: agentUserID}
}

// ApplyExposedCameras implements setup.CameraAllowlistApplier.
func (a *CameraApplier) ApplyExposedCameras(ids []string) {
	a.src.SetAllowed(ids)
	log.Printf("camera exposure: applied allow-list of %d camera(s) (empty=all)", len(ids))
	if a.hg == nil {
		return
	}
	// Re-report state for every exposed camera after the sync. A camera
	// that was hidden at startup had its initial ReportState filtered out;
	// re-exposing it fires no online transition, so without this push
	// Google's cache (Home app tiles, Test Suite) would stay stale until
	// the camera physically flaps.
	states := map[string]map[string]any{}
	for _, c := range a.src.Snapshot() {
		if a.src.IsAllowed(c.ID) {
			states[c.ID] = map[string]any{"online": c.Online}
		}
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := a.hg.RequestSync(ctx, a.agentUserID); err != nil {
			log.Printf("camera exposure: requestSync after allow-list change: %v", err)
			return
		}
		log.Printf("camera exposure: requestSync sent — Google will re-pull SYNC")
		if len(states) == 0 {
			return
		}
		if err := a.hg.ReportState(ctx, a.agentUserID, states); err != nil {
			log.Printf("camera exposure: reportState after allow-list change: %v", err)
			return
		}
		log.Printf("camera exposure: reported state for %d exposed camera(s)", len(states))
	}()
}

// WSLogApplier is the setup.WSLogApplier implementation: it swaps the
// reconciler's ws-event log level under sync/atomic so the change takes
// effect on the very next websocket frame, no restart needed. The atomic
// is the single source of truth at runtime — the config struct is not
// mutated (it is read concurrently by other goroutines).
type WSLogApplier struct {
	rec *Reconciler
}

// NewWSLogApplier wires a WSLogApplier.
func NewWSLogApplier(rec *Reconciler) *WSLogApplier {
	return &WSLogApplier{rec: rec}
}

// ApplyWSEventLog implements setup.WSLogApplier.
func (a *WSLogApplier) ApplyWSEventLog(level string) {
	v := wsLogLevelFromString(level)
	a.rec.wsLogLevel.Store(v)
	log.Printf("protect ws: event log level changed to %s", wsLogLevelString(v))
}

// Status is the setup.StatusProvider implementation backed by the
// running bridge. Every call materialises a fresh snapshot from the live
// CameraSource and config so the ingress UI shows up-to-date data.
type Status struct {
	cfg           *config.Config
	uc            *unifi.Client
	src           *CameraSource
	fulfill       *ghome.Handler
	hg            *ghome.HomeGraph
	rec           *Reconciler
	syncStatePath string
}

// NewStatus wires a Status provider.
func NewStatus(cfg *config.Config, uc *unifi.Client, src *CameraSource, fulfill *ghome.Handler, hg *ghome.HomeGraph, rec *Reconciler, syncStatePath string) *Status {
	return &Status{
		cfg:           cfg,
		uc:            uc,
		src:           src,
		fulfill:       fulfill,
		hg:            hg,
		rec:           rec,
		syncStatePath: syncStatePath,
	}
}

// Status implements setup.StatusProvider.
func (b *Status) Status() setup.StatusSnapshot {
	cams := b.src.Snapshot()
	nvrVer, nvrMAC := b.uc.NVRInfo()

	out := setup.StatusSnapshot{
		SetupMode: false,
		UniFi: setup.UniFiStatus{
			Host:       b.cfg.UniFi.Host,
			Connected:  nvrMAC != "",
			NVRMAC:     nvrMAC,
			NVRVersion: nvrVer,
		},
		Bridge: setup.BridgeStatus{
			PublicBaseURL: b.cfg.Bridge.PublicBaseURL,
			PublicURLSet:  b.cfg.Bridge.PublicBaseURL != "",
			AgentUserID:   b.cfg.Bridge.AgentUserID,
			ListenAddr:    b.cfg.Bridge.ListenAddr,
			// The reconciler's atomic is the runtime source of truth for
			// the hot-appliable ws log level; reading cfg here would race
			// with the setup-UI applier.
			WSEventLog: b.rec.WSEventLog(),
		},
		Google: setup.GoogleStatus{
			HomeGraphEnabled:    b.cfg.Google.HomeGraphEnabled(),
			HomeGraphConfigured: b.hg != nil,
			ProjectID:           b.cfg.Google.ProjectID,
			OAuthConfigured:     b.cfg.Google.OAuthClientID != "" && b.cfg.Google.OAuthClientSecret != "",
		},
	}
	if b.hg != nil {
		out.Google.ServiceAccountEmail = b.hg.ServiceAccountEmail()
		saProj := b.hg.ServiceAccountProjectID()
		out.Google.ProjectIDMismatch = b.cfg.Google.ProjectID != "" && saProj != "" && b.cfg.Google.ProjectID != saProj
	}
	out.Cameras = make([]setup.CameraInfo, 0, len(cams))
	for _, c := range cams {
		out.Cameras = append(out.Cameras, setup.CameraInfo{
			ID:       c.ID,
			Name:     c.Name,
			Model:    c.Model,
			Online:   c.Online,
			Doorbell: ghome.IsDoorbell(c),
			Exposed:  b.src.IsAllowed(c.ID),
		})
	}
	// Persisted SYNC fingerprint (so the UI can confirm Google has the
	// current device list and that we're under quota).
	if b.syncStatePath != "" {
		if data, err := os.ReadFile(b.syncStatePath); err == nil {
			var st syncStateFile
			if json.Unmarshal(data, &st) == nil && st.Fingerprint != "" {
				out.Bridge.SyncStateKnown = true
				out.Bridge.SyncFingerprint = st.Fingerprint
			}
		}
	}
	return out
}
