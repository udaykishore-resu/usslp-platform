package sgu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// ---------------------------------------------------------------------------
// The gateway's diagnostics surface
//
// This is what a field engineer opens on a laptop, plugged into a switch in a
// back office, when a store manager has rung to say the labels are wrong. It is
// separate from the obs.Runtime admin port — that carries /metrics, /healthz
// and /readyz, which are for the fleet — because the questions asked here are
// about one store and are asked by a human: is this store talking to the cloud,
// how far behind is it, which controllers are alive, what is this label
// supposed to be showing, and what happened the last time the link came back.
// ---------------------------------------------------------------------------

// Status is the whole-gateway view the diagnostics page leads with.
type Status struct {
	SGUID   canon.SGUID    `json:"sgu_id"`
	StoreID canon.StoreID  `json:"store_id"`
	Mode    Mode           `json:"mode"`
	Since   time.Time      `json:"mode_since"`
	Broker  string         `json:"broker_addr"`
	Link    DetectorStats  `json:"link"`
	Queue   QueueStats     `json:"queue"`
	Rules   RulesStats     `json:"rules"`
	Clock   SkewReport     `json:"clock"`
	Labels  int            `json:"labels"`
	SECs    []SECStatus    `json:"controllers"`
	Stats   Stats          `json:"counters"`
	Pending []PromoSummary `json:"pending_promotions"`
	// LocalAuthority reports whether this store can attest a price of its own,
	// which decides whether a local point-of-sale change can reach a shelf
	// during an outage.
	LocalAuthority bool   `json:"local_price_authority"`
	AutonomousFor  string `json:"autonomous_for,omitempty"`
}

// PromoSummary is a scheduled promotion as the diagnostics page shows it.
type PromoSummary struct {
	PromotionID canon.PromotionID `json:"promotion_id"`
	ActivateAt  time.Time         `json:"activate_at"`
	ExpireAt    time.Time         `json:"expire_at,omitempty"`
	Labels      int               `json:"labels"`
	Activated   *time.Time        `json:"activated_at,omitempty"`
}

// Status assembles the gateway's current state.
func (g *Gateway) Status() Status {
	g.mu.Lock()
	mode, since, stats := g.mode, g.autonomousAt, g.stats
	g.mu.Unlock()

	s := Status{
		SGUID: g.cfg.SGUID, StoreID: g.cfg.StoreID, Mode: mode, Since: since,
		Broker: g.brokerAt, Link: g.detector.Stats(), Queue: g.queue.Stats(),
		Rules: g.rules.Stats(), Clock: g.clock.Skew(), Labels: g.replica.LabelCount(),
		SECs: g.SECs(), Stats: stats, LocalAuthority: g.cfg.LocalAuthority != nil,
	}
	if mode == ModeAutonomous && !since.IsZero() {
		s.AutonomousFor = g.cfg.Now().Sub(since).Round(time.Second).String()
	}
	for _, p := range g.schedule.All() {
		s.Pending = append(s.Pending, PromoSummary{
			PromotionID: p.PromotionID, ActivateAt: p.ActivateAt, ExpireAt: p.ExpireAt,
			Labels: len(p.Updates), Activated: p.ActivatedAt,
		})
	}
	return s
}

// adminServer is the gateway's HTTP diagnostics surface.
type adminServer struct {
	g   *Gateway
	srv *http.Server
	ln  net.Listener
}

func newAdminServer(g *Gateway, addr string) (*adminServer, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("sgu: binding the diagnostics surface on %s: %w", addr, err)
	}
	a := &adminServer{g: g, ln: ln}
	mux := http.NewServeMux()
	mux.HandleFunc("/status", a.handleStatus)
	mux.HandleFunc("/mode", a.handleMode)
	mux.HandleFunc("/queue", a.handleQueue)
	mux.HandleFunc("/secs", a.handleSECs)
	mux.HandleFunc("/labels", a.handleLabels)
	mux.HandleFunc("/inventory", a.handleInventory)
	mux.HandleFunc("/reconciliation", a.handleReconciliation)
	mux.HandleFunc("/promotions", a.handlePromotions)
	mux.HandleFunc("/rules", a.handleRules)
	mux.HandleFunc("/pos/price", a.handlePOSPrice)
	a.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return a, nil
}

func (a *adminServer) addr() string { return a.ln.Addr().String() }

func (a *adminServer) start() {
	go func() {
		if err := a.srv.Serve(a.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.g.cfg.Log.Error("gateway diagnostics stopped", "error", err)
		}
	}()
}

func (a *adminServer) stop(ctx context.Context) error { return a.srv.Shutdown(ctx) }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func (a *adminServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.g.Status())
}

// handleMode reports the mode and, on POST, forces one.
//
// Forcing exists because an engineer who is about to unplug the WAN for
// scheduled work should be able to put the store into autonomy deliberately,
// rather than let it discover the outage fifteen seconds later and emit an
// alarm that wakes somebody.
func (a *adminServer) handleMode(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var body struct {
			Mode   string `json:"mode"`
			Reason string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		mode := Mode(body.Mode)
		if mode != ModeAutonomous && mode != ModeConnected {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("mode must be %q or %q", ModeAutonomous, ModeConnected)})
			return
		}
		reason := body.Reason
		if reason == "" {
			reason = "operator request"
		}
		a.g.detector.ForceMode(mode, reason)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"mode": a.g.Mode(), "link": a.g.detector.Stats(),
	})
}

func (a *adminServer) handleQueue(w http.ResponseWriter, r *http.Request) {
	stats := a.g.queue.Stats()
	head, err := a.g.queue.Peek(20)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	type headEntry struct {
		Seq        uint64    `json:"seq"`
		Topic      string    `json:"topic"`
		Class      Class     `json:"class"`
		EnqueuedAt time.Time `json:"enqueued_at"`
		Bytes      int       `json:"bytes"`
	}
	view := make([]headEntry, 0, len(head))
	for _, e := range head {
		view = append(view, headEntry{Seq: e.Seq, Topic: e.Topic, Class: e.Class,
			EnqueuedAt: e.EnqueuedAt, Bytes: len(e.Payload)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"stats": stats, "head": view})
}

func (a *adminServer) handleSECs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.g.SECs())
}

func (a *adminServer) handleLabels(w http.ResponseWriter, r *http.Request) {
	if id := r.URL.Query().Get("id"); id != "" {
		st, ok := a.g.replica.Label(canon.LabelID(id))
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such label in this store"})
			return
		}
		writeJSON(w, http.StatusOK, st)
		return
	}
	writeJSON(w, http.StatusOK, a.g.replica.Labels())
}

func (a *adminServer) handleInventory(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.g.replica.InventoryAll())
}

func (a *adminServer) handleReconciliation(w http.ResponseWriter, r *http.Request) {
	report, ok := a.g.LastReconciliation()
	if !ok {
		writeJSON(w, http.StatusOK, map[string]string{
			"status": "this gateway has not reconciled since it started"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"summary": report.Summary(), "report": report})
}

func (a *adminServer) handlePromotions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.g.schedule.All())
}

func (a *adminServer) handleRules(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var rules ProductRules
		if err := json.NewDecoder(r.Body).Decode(&rules); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := a.g.rules.Set(rules); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"stats": a.g.rules.Stats(), "rules": a.g.rules.All()})
}

// handlePOSPrice is the local point-of-sale ingress.
//
// It is the endpoint a store's till system posts to when the WAN is down, and
// its failure modes are as much a part of the contract as its success: an
// unknown product, a price that breaks a guard rail, and a store with no
// delegated signing key are three different answers and the caller needs to
// tell them apart.
func (a *adminServer) handlePOSPrice(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST a price change"})
		return
	}
	var req canon.PriceChangeRequested
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.StoreID == "" {
		req.StoreID = a.g.cfg.StoreID
	}
	if req.EffectiveAt.IsZero() {
		req.EffectiveAt = a.g.cfg.Now().UTC()
	}
	labels, err := a.g.LocalPriceChange(r.Context(), req)
	switch {
	case errors.Is(err, ErrUnknownSKU):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrRuleViolation):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrNoLocalAuthority):
		writeJSON(w, http.StatusPreconditionFailed, map[string]string{
			"error": err.Error(),
			"note": "a label refuses any price it cannot verify. Provision a delegated, " +
				"store-scoped price authority for this gateway to accept local changes.",
		})
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"updated_labels": labels, "count": len(labels)})
	}
}
