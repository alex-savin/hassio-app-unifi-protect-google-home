// Package bridge contains the runtime control plane of the add-on: the
// reconciler that mirrors UniFi Protect state into Google Home Graph, the
// camera source consumed by fulfillment and the stream servers, and the
// glue types the ingress setup UI uses to observe and hot-reconfigure the
// running bridge.
package bridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/ghome"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/rtsp"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/streams"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/unifi"
)

const bootstrapInterval = 10 * time.Second

// Reconciler refreshes the camera list from the UniFi bootstrap and pushes
// add/remove/state changes to Google Home Graph.
type Reconciler struct {
	unifi       *unifi.Client
	reg         *streams.Registry
	src         *CameraSource
	hg          *ghome.HomeGraph
	agentUserID string
	verifyTLS   bool
	// wsLogLevel is one of wsLogOff/wsLogInteresting/wsLogAll. Stored as
	// int32 so the per-event hot path can read it without locks.
	wsLogLevel atomic.Int32
	// firstRefreshDone flips on the first refresh, whose diff against the
	// empty snapshot would otherwise count every camera as "added".
	firstRefreshDone atomic.Bool
}

// NewReconciler wires a Reconciler. hg may be nil (HomeGraph disabled).
func NewReconciler(uc *unifi.Client, reg *streams.Registry, src *CameraSource, hg *ghome.HomeGraph, agentUserID string, verifyTLS bool, wsEventLog string) *Reconciler {
	r := &Reconciler{
		unifi:       uc,
		reg:         reg,
		src:         src,
		hg:          hg,
		agentUserID: agentUserID,
		verifyTLS:   verifyTLS,
	}
	r.wsLogLevel.Store(wsLogLevelFromString(wsEventLog))
	return r
}

// WSEventLog returns the current (hot-appliable) ws-event log verbosity.
func (r *Reconciler) WSEventLog() string {
	return wsLogLevelString(r.wsLogLevel.Load())
}

// Protect WS log verbosity levels. Persisted in bridge.ws_event_log and
// hot-applied via setup.WSLogApplier.
const (
	wsLogOff         int32 = 0
	wsLogInteresting int32 = 1
	wsLogAll         int32 = 2
)

// wsLogLevelFromString parses a user-facing value. Falls back to
// "interesting" for empty/unknown input — matches config.Load defaults.
func wsLogLevelFromString(s string) int32 {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "off":
		return wsLogOff
	case "all":
		return wsLogAll
	default:
		return wsLogInteresting
	}
}

func wsLogLevelString(v int32) string {
	switch v {
	case wsLogOff:
		return "off"
	case wsLogAll:
		return "all"
	default:
		return "interesting"
	}
}

// wsInterestingFields is the set of Protect camera fields the bridge
// actually reacts to (ring, motion, online state, name/channels). All
// other fields are pure telemetry noise (uptime, lastSeen, phyRate,
// stats, nvrMac, uplinkDevice, isRecording, …) that we never act on.
var wsInterestingFields = map[string]struct{}{
	"state":            {},
	"isConnected":      {},
	"isAdopted":        {},
	"name":             {},
	"channels":         {},
	"lastRing":         {},
	"lastMotion":       {},
	"isMotionDetected": {},
}

func wsHasInterestingField(fields map[string]json.RawMessage) bool {
	for k := range fields {
		if _, ok := wsInterestingFields[k]; ok {
			return true
		}
	}
	return false
}

// Poll runs the refresh loop: a periodic safety net plus debounced
// WS-triggered refreshes. Blocks until ctx is cancelled.
func (r *Reconciler) Poll(ctx context.Context) {
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
			if _, err := r.Refresh(ctx); err != nil {
				log.Printf("bootstrap refresh: %v", err)
			}
		case <-trigger:
			if pending == nil {
				pending = time.AfterFunc(debounce, func() {
					if _, err := r.Refresh(ctx); err != nil {
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
func (r *Reconciler) watchEvents(ctx context.Context, fire func()) {
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
		connectedAt := time.Now()

		for ev := range ch {
			if ev.NewUpdateID != "" {
				lastUpdateID = ev.NewUpdateID
				r.src.setLastUpdateID(ev.NewUpdateID)
			}
			if ev.ModelKey != "camera" {
				continue
			}
			if ev.Action == "update" || ev.Action == "add" {
				lvl := r.wsLogLevel.Load()
				if lvl == wsLogAll || (lvl == wsLogInteresting && wsHasInterestingField(ev.Fields)) {
					keys := make([]string, 0, len(ev.Fields))
					for k := range ev.Fields {
						keys = append(keys, k)
					}
					log.Printf("protect ws: camera %s action=%s fields=%v", ev.ID, ev.Action, keys)
				}
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
		// Only treat the connection as healthy — and reset the backoff —
		// if it actually survived a while. A dial that succeeds and drops
		// immediately (controller restarting, session rejected at the
		// Protect layer) must keep backing off or this loop becomes a
		// zero-sleep reconnect storm.
		if time.Since(connectedAt) > 30*time.Second {
			backoff = time.Second
		}
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
	}
}

// Refresh pulls the current bootstrap snapshot and reconciles the registry
// + the ghome.Source snapshot. On membership change calls RequestSync; on
// online-state change calls ReportState. Returns the controller's
// lastUpdateID so the WS subscription can resume from the right cursor.
func (r *Reconciler) Refresh(ctx context.Context) (string, error) {
	cams, lastUpdateID, err := r.unifi.Bootstrap(ctx)
	if err != nil {
		return "", err
	}

	// On the first refresh every camera diffs as "added" against the empty
	// snapshot. The startup RequestSync decision belongs exclusively to
	// StartupRequestSync's fingerprint gate — firing from here too would
	// burn HomeGraph's small per-day quota on every restart (the 0.3.21
	// 429 incident).
	firstRefresh := !r.firstRefreshDone.Swap(true)

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
		if _, had := prev[id]; !had && r.src.IsAllowed(id) {
			added++
		}
	}
	removed := 0
	for _, name := range r.reg.Names() {
		if _, ok := seen[name]; !ok {
			r.reg.Delete(name)
			log.Printf("removed camera %s", name)
			if r.src.IsAllowed(name) {
				removed++
			}
		}
	}

	r.src.set(ghomeCams)

	// Filter ReportState updates to exposed cameras only — HomeGraph will
	// reject (or silently drop) entries for IDs it never received via SYNC.
	for id := range onlineChanged {
		if !r.src.IsAllowed(id) {
			delete(onlineChanged, id)
		}
	}

	if r.hg != nil {
		if (added > 0 || removed > 0) && !firstRefresh {
			// Deliberately detached from the refresh ctx: a HomeGraph push
			// already in flight should complete even if the triggering
			// refresh is superseded.
			go func() { //nolint:gosec
				syncCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				if err := r.hg.RequestSync(syncCtx, r.agentUserID); err != nil {
					log.Printf("homegraph requestSync: %v", err)
				}
			}()
		}
		if len(onlineChanged) > 0 {
			go func() { //nolint:gosec // detached on purpose, see above
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
func (r *Reconciler) handleStateChange(camID string, online bool) {
	changed, cam, ok := r.src.setOnline(camID, online)
	if !ok || !changed {
		return
	}
	log.Printf("camera %s (%s) -> online=%v", camID, cam.Name, online)
	if r.hg == nil || !r.src.IsAllowed(camID) {
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
func (r *Reconciler) handleRing(camID string) {
	if r.hg == nil || !r.src.IsAllowed(camID) {
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
