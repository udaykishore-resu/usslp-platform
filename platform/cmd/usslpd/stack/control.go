package stack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/usslp/usslp/edge/mesh"
	"github.com/usslp/usslp/edge/sec"
	"github.com/usslp/usslp/edge/sgu"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// The control surface is usslpd's own operations API.
//
// It answers the questions that belong to the *assembly* rather than to any one
// service — what is running, where, on which port, with which fleet — and it
// injects the faults an operator cannot inject through a service API because no
// service owns the network between them. Nothing here is a shortcut around a
// service: a price change goes through the UIG, a promotion through the
// Promotion Service. The endpoints are:
//
//	GET  /v1/status                 what is running, and where
//	GET  /v1/stores                 per-store mode, queue depth, controllers
//	GET  /v1/stores/{id}/labels     every label with what is on its glass
//	GET  /v1/stores/{id}/slo        measured latency against the 3 s budget
//	POST /v1/chaos/wan-outage       cut or restore a store's uplink
//	POST /v1/chaos/kill-sec         pull a controller's power
//	POST /v1/chaos/degrade-link     add latency and loss to a store's uplink
//	POST /v1/chaos/kill-relay       kill a mesh relay node
//
// It binds to the loopback interface and carries no authentication, which is
// stated plainly rather than hidden: it can cut a store's network and kill its
// controllers, so it must never be exposed. The distributed topology has no
// equivalent, by design — fault injection there is a chaos-engineering tool
// operating on the cluster, not an endpoint the platform ships.

func (s *Stack) startControl() error {
	ln, err := s.listen("control", s.cfg.Ports.Control)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", s.jsonHandler(s.handleStatus))
	mux.HandleFunc("GET /v1/stores", s.jsonHandler(s.handleStores))
	mux.HandleFunc("GET /v1/stores/{id}/labels", s.jsonHandler(s.handleStoreLabels))
	mux.HandleFunc("GET /v1/stores/{id}/slo", s.jsonHandler(s.handleStoreSLO))
	mux.HandleFunc("POST /v1/slo/reset", s.jsonHandler(s.handleSLOReset))
	mux.HandleFunc("POST /v1/chaos/wan-outage", s.jsonHandler(s.handleWANOutage))
	mux.HandleFunc("POST /v1/chaos/kill-sec", s.jsonHandler(s.handleKillSEC))
	mux.HandleFunc("POST /v1/chaos/degrade-link", s.jsonHandler(s.handleDegradeLink))
	mux.HandleFunc("POST /v1/chaos/kill-relay", s.jsonHandler(s.handleKillRelay))

	s.controlLn = ln
	s.control = s.serve("control", ln, mux, 60*time.Second)
	return nil
}

// ControlURL is the address of the runtime's own operations API.
func (s *Stack) ControlURL() string {
	if s.controlLn == nil {
		return ""
	}
	return "http://" + s.controlLn.Addr().String()
}

// jsonHandler adapts a handler that returns a value and an error.
func (s *Stack) jsonHandler(fn func(*http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := fn(r)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(v)
	}
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// StatusView is the whole runtime in one document.
type StatusView struct {
	Version      string            `json:"version"`
	StartedAt    time.Time         `json:"started_at"`
	UptimeS      int64             `json:"uptime_seconds"`
	BootMS       int64             `json:"boot_ms"`
	DataDir      string            `json:"data_dir"`
	Ephemeral    bool              `json:"ephemeral"`
	Tenants      []canon.TenantID  `json:"tenants"`
	Stores       int               `json:"stores"`
	Controllers  int               `json:"controllers"`
	Labels       int               `json:"labels"`
	Partitions   int               `json:"stream_partitions"`
	PriceKeyID   string            `json:"price_authority_key_id"`
	CloudBroker  string            `json:"cloud_broker"`
	Endpoints    map[string]string `json:"endpoints"`
	AdminSurface map[string]string `json:"admin"`
	APIKeys      map[string]string `json:"api_keys"`
}

func (s *Stack) handleStatus(*http.Request) (any, error) { return s.Status(), nil }

// Status is the same document the control surface serves, for callers inside
// the process.
func (s *Stack) Status() StatusView {
	controllers := 0
	for _, st := range s.stores {
		controllers += len(st.Zones)
	}
	keys := map[string]string{}
	for t, k := range s.cloudSvcs.tenantKeys {
		keys[string(t)] = k
	}
	return StatusView{
		Version:     s.bootRT.Version,
		StartedAt:   s.startedAt.UTC(),
		UptimeS:     int64(time.Since(s.startedAt).Seconds()),
		BootMS:      s.bootDur.Milliseconds(),
		DataDir:     s.cfg.DataDir,
		Ephemeral:   s.cfg.Ephemeral,
		Tenants:     s.cfg.Tenants,
		Stores:      len(s.stores),
		Controllers: controllers,
		Labels:      s.LabelCount(),
		Partitions:  s.cfg.DevPartitions,
		PriceKeyID:  s.authority.KeyID(),
		CloudBroker: s.cloudAddr,
		Endpoints: map[string]string{
			"control":            s.ControlURL(),
			"api-gateway":        s.cloudSvcs.gatewayURL,
			"console":            s.cloudSvcs.gatewayURL + "/console",
			"openapi":            s.cloudSvcs.gatewayURL + "/openapi.json",
			"uig":                s.cloudSvcs.uigURL,
			"label-service":      s.cloudSvcs.labelURL,
			"device-registry":    s.cloudSvcs.registryURL,
			"ota-service":        s.cloudSvcs.otaURL,
			"pricing-service":    s.cloudSvcs.pricingURL,
			"promotion-service":  s.cloudSvcs.promotionURL,
			"analytics-service":  s.cloudSvcs.analyticsURL,
			"cloud-mqtt":         "tcp://" + s.cloudAddr,
			"shopify-hmac-key":   s.cloudSvcs.uig.HMACKey(),
			"shopify-ingest-url": s.cloudSvcs.uigURL + "/v1/ingest/{tenant}/" + ShopifyBindingID,
		},
		AdminSurface: s.cloudSvcs.admin,
		APIKeys:      keys,
	}
}

// ---------------------------------------------------------------------------
// Stores
// ---------------------------------------------------------------------------

// StoreView is one store's operational state.
type StoreView struct {
	StoreID     canon.StoreID    `json:"store_id"`
	TenantID    canon.TenantID   `json:"tenant_id"`
	Mode        string           `json:"mode"`
	Broker      string           `json:"broker"`
	Diagnostics string           `json:"diagnostics"`
	Admin       string           `json:"admin"`
	Controllers []ControllerView `json:"controllers"`
	Labels      int              `json:"labels"`
	Queue       QueueView        `json:"upstream_queue"`
	Link        linkStats        `json:"wan_link"`
	Stats       sgu.Stats        `json:"gateway"`
}

// ControllerView is one controller's state.
type ControllerView struct {
	SECID             canon.SECID `json:"sec_id"`
	Labels            int         `json:"labels"`
	Online            bool        `json:"online"`
	Applied           uint64      `json:"updates_applied"`
	AttestationFailed uint64      `json:"attestation_failures"`
	SequenceDiscarded uint64      `json:"sequence_discarded"`
	DeliveryFailed    uint64      `json:"delivery_failures"`
	MeshNodes         int         `json:"mesh_nodes"`
	MeshJoined        int         `json:"mesh_nodes_joined"`
	ChannelUtil       string      `json:"channel_utilisation"`
	Spare             string      `json:"uncommissioned_spare,omitempty"`
}

// QueueView is the durable upstream buffer.
type QueueView struct {
	Depth int   `json:"depth"`
	Bytes int64 `json:"bytes"`
}

func (s *Stack) handleStores(*http.Request) (any, error) { return s.StoreViews(), nil }

// StoreViews renders every store.
func (s *Stack) StoreViews() []StoreView {
	out := make([]StoreView, 0, len(s.stores))
	for _, st := range s.Stores() {
		out = append(out, s.storeView(st))
	}
	return out
}

func (s *Stack) storeView(st *Store) StoreView {
	online := map[canon.SECID]bool{}
	for _, sc := range st.Gateway.SECs() {
		online[sc.SECID] = sc.Online
	}
	qs := st.Gateway.Queue().Stats()
	v := StoreView{
		StoreID: st.ID, TenantID: st.Tenant, Mode: string(st.Gateway.Mode()),
		Broker: st.BrokerAddr, Diagnostics: st.DiagnoseURL, Admin: st.AdminURL,
		Labels: st.LabelCount(),
		Queue:  QueueView{Depth: qs.Depth, Bytes: qs.Bytes},
		Link:   st.Link.stats(), Stats: st.Gateway.Stats(),
	}
	for _, z := range st.Zones {
		cv := ControllerView{SECID: z.SECID, Labels: len(z.Labels()), Online: online[z.SECID]}
		if z.Controller != nil {
			cs := z.Controller.Stats()
			cv.Applied, cv.AttestationFailed = cs.Applied, cs.AttestationFailed
			cv.SequenceDiscarded, cv.DeliveryFailed = cs.SequenceDiscarded, cs.DeliveryFailed
		}
		ns := z.Sim.Net.Stats()
		cv.MeshNodes, cv.MeshJoined = ns.Nodes, ns.Joined
		cv.ChannelUtil = fmt.Sprintf("%.2f%%", 100*z.Sim.Net.ChannelUtilisation())
		cv.Spare = string(z.Spare())
		v.Controllers = append(v.Controllers, cv)
	}
	sort.Slice(v.Controllers, func(i, j int) bool { return v.Controllers[i].SECID < v.Controllers[j].SECID })
	return v
}

// LabelView is one label as the platform and the glass both see it.
//
// Two prices, deliberately. The controller's record is what the cloud believes
// reached the shelf; the simulated panel's is what a shopper would read. They
// differ exactly when something is in flight or something has gone wrong, and
// collapsing them into one field would hide the failure this platform exists to
// prevent.
type LabelView struct {
	LabelID     canon.LabelID     `json:"label_id"`
	SECID       canon.SECID       `json:"sec_id"`
	SKU         canon.SKU         `json:"sku"`
	Controller  string            `json:"controller_price"`
	Glass       string            `json:"displayed_price"`
	Sequence    int64             `json:"sequence"`
	Displayed   int64             `json:"displayed_sequence"`
	Attested    bool              `json:"attested"`
	KeyID       string            `json:"attestation_key_id,omitempty"`
	Refreshes   int64             `json:"panel_refreshes"`
	Partials    int64             `json:"partial_refreshes"`
	Discarded   int64             `json:"stale_updates_discarded"`
	BatteryPct  int               `json:"battery_percent"`
	Dead        bool              `json:"dead"`
	PromotionID canon.PromotionID `json:"promotion_id,omitempty"`
	LastError   string            `json:"last_error,omitempty"`
}

func (s *Stack) handleStoreLabels(r *http.Request) (any, error) {
	st, ok := s.Store(canon.StoreID(r.PathValue("id")))
	if !ok {
		return nil, fmt.Errorf("no such store: %s", r.PathValue("id"))
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, _ = strconv.Atoi(raw)
	}
	views := s.LabelViews(st)
	if limit > 0 && limit < len(views) {
		views = views[:limit]
	}
	return views, nil
}

// LabelViews renders every label in a store.
func (s *Stack) LabelViews(st *Store) []LabelView {
	var out []LabelView
	for _, z := range st.Zones {
		for _, id := range z.Labels() {
			out = append(out, s.LabelView(st, z, id))
		}
	}
	return out
}

// LabelView renders one label.
func (s *Stack) LabelView(st *Store, z *Zone, id canon.LabelID) LabelView {
	v := LabelView{LabelID: id, SECID: z.SECID, SKU: st.skuOf[id]}
	var rec sec.LabelRecord
	var known bool
	if z.Controller != nil {
		rec, known = z.Controller.Record(id)
	}
	if known {
		v.Controller = rec.Price.Display()
		v.Sequence, v.Displayed = rec.Sequence, rec.DisplayedSequence
		v.Attested = rec.Attestation.Signature != ""
		v.KeyID = rec.Attestation.KeyID
		v.PromotionID = rec.PromotionID
		v.LastError = rec.LastError
	}
	if l, ok := z.Sim.Label(id); ok {
		ls := l.Stats()
		v.Refreshes, v.Partials, v.Discarded = ls.RefreshCount, ls.PartialRefreshes, ls.Discarded
		v.BatteryPct = ls.BatteryPct
		v.Dead = l.Dead()
		v.Glass = glassOf(rec, known, ls.Sequence)
	}
	return v
}

// glassOf renders what a shopper would read off the panel.
//
// # Why this is inferred rather than read
//
// edge/labelsim.Label holds the price it applied but exposes no accessor for
// it, so the runtime cannot ask the panel directly. What it can do is compare
// two facts that come from opposite ends of the radio link: the sequence the
// label itself reports having applied (labelsim.Stats.Sequence, incremented
// only inside the refresh path) and the price the controller signed and sent at
// that sequence. When they agree, the price on the glass is the controller's
// price, because the sequence is monotonic and a label applies a frame or
// discards it whole.
//
// When they disagree the panel is showing an *older* price — an update is in
// flight, or one was refused — and saying so is more useful than a number that
// might be wrong. That is exactly the state the attestation test asserts on.
func glassOf(rec sec.LabelRecord, known bool, applied int64) string {
	switch {
	case !known || applied == 0:
		return "(blank)"
	case applied == rec.Sequence:
		return rec.Price.Display()
	default:
		return fmt.Sprintf("(showing sequence %d; the controller has sent %d)", applied, rec.Sequence)
	}
}

// GlassMatches reports whether a label's panel is showing exactly the price the
// controller last authorised. It is the assertion the end-to-end suite makes
// about "the price is on the glass".
func (s *Stack) GlassMatches(z *Zone, id canon.LabelID, want canon.Money) (bool, string) {
	rec, known := z.Controller.Record(id)
	if !known {
		return false, "the controller has no record of this label"
	}
	l, ok := z.Sim.Label(id)
	if !ok {
		return false, "there is no simulated label with this identifier"
	}
	applied := l.Stats().Sequence
	if applied != rec.Sequence {
		return false, fmt.Sprintf("the panel has applied sequence %d, the controller has sent %d",
			applied, rec.Sequence)
	}
	if rec.Price.Cmp(want) != 0 {
		return false, fmt.Sprintf("the glass shows %s, not %s", rec.Price.Display(), want.Display())
	}
	return true, ""
}

// ---------------------------------------------------------------------------
// Fault injection
// ---------------------------------------------------------------------------

type chaosRequest struct {
	StoreID canon.StoreID `json:"store_id"`
	SECID   canon.SECID   `json:"sec_id"`
	// Restore reverses a wan-outage instead of causing one.
	Restore bool `json:"restore"`
	// DelayMS and LossPercent parameterise degrade-link.
	DelayMS     int    `json:"delay_ms"`
	LossPercent int    `json:"loss_percent"`
	Seconds     int    `json:"seconds"`
	Node        string `json:"node"`
}

func decodeChaos(r *http.Request) (chaosRequest, error) {
	var req chaosRequest
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<16)).Decode(&req); err != nil {
		return req, fmt.Errorf("the request body is not a chaos request: %w", err)
	}
	return req, nil
}

func (s *Stack) handleWANOutage(r *http.Request) (any, error) {
	req, err := decodeChaos(r)
	if err != nil {
		return nil, err
	}
	targets, err := s.chaosTargets(req.StoreID)
	if err != nil {
		return nil, err
	}
	for _, st := range targets {
		if req.Restore {
			st.Link.Restore()
		} else {
			st.Link.Cut()
		}
	}
	// A duration turns "cut the WAN" into a self-healing experiment, which is
	// what a scripted demo wants: nobody watching wants to remember to restore
	// it, and a forgotten outage looks like a bug.
	if !req.Restore && req.Seconds > 0 {
		d := time.Duration(req.Seconds) * time.Second
		go func(ts []*Store) {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-s.backgroundCtx().Done():
			case <-t.C:
				for _, st := range ts {
					st.Link.Restore()
				}
			}
		}(targets)
	}
	return s.chaosResult(targets, map[string]any{
		"action": map[bool]string{true: "restored", false: "cut"}[req.Restore],
	}), nil
}

func (s *Stack) handleDegradeLink(r *http.Request) (any, error) {
	req, err := decodeChaos(r)
	if err != nil {
		return nil, err
	}
	targets, err := s.chaosTargets(req.StoreID)
	if err != nil {
		return nil, err
	}
	for _, st := range targets {
		st.Link.Degrade(time.Duration(req.DelayMS)*time.Millisecond, req.LossPercent)
	}
	return s.chaosResult(targets, map[string]any{
		"action": "degraded", "delay_ms": req.DelayMS, "loss_percent": req.LossPercent,
	}), nil
}

func (s *Stack) handleKillSEC(r *http.Request) (any, error) {
	req, err := decodeChaos(r)
	if err != nil {
		return nil, err
	}
	if req.SECID == "" {
		return nil, fmt.Errorf("sec_id is required")
	}
	for _, st := range s.stores {
		if z, ok := st.Zone(req.SECID); ok {
			if err := z.Kill(); err != nil {
				return nil, err
			}
			return map[string]any{
				"action": "killed", "sec_id": req.SECID, "store_id": st.ID,
				"note": "the controller's link was severed without a clean disconnect, " +
					"so the gateway learns about it from the retained last will",
			}, nil
		}
	}
	return nil, fmt.Errorf("no such controller: %s", req.SECID)
}

func (s *Stack) handleKillRelay(r *http.Request) (any, error) {
	req, err := decodeChaos(r)
	if err != nil {
		return nil, err
	}
	for _, st := range s.stores {
		if req.StoreID != "" && st.ID != req.StoreID {
			continue
		}
		for _, z := range st.Zones {
			if req.SECID != "" && z.SECID != req.SECID {
				continue
			}
			node := mesh.NodeID(req.Node)
			if node == "" {
				n, ok := z.Relay()
				if !ok {
					continue
				}
				node = n
			}
			z.Sim.Net.KillNode(node)
			return map[string]any{
				"action": "relay killed", "store_id": st.ID, "sec_id": z.SECID,
				"node": string(node),
				"note": "every label parented to it must find a new route before its " +
					"next update can be delivered",
			}, nil
		}
	}
	return nil, fmt.Errorf("no relay node matched the request")
}

func (s *Stack) chaosTargets(id canon.StoreID) ([]*Store, error) {
	if id == "" {
		return s.Stores(), nil
	}
	st, ok := s.Store(id)
	if !ok {
		return nil, fmt.Errorf("no such store: %s", id)
	}
	return []*Store{st}, nil
}

func (s *Stack) chaosResult(targets []*Store, extra map[string]any) map[string]any {
	stores := make([]map[string]any, 0, len(targets))
	for _, st := range targets {
		stores = append(stores, map[string]any{
			"store_id": st.ID, "mode": string(st.Gateway.Mode()), "wan_link": st.Link.stats(),
		})
	}
	out := map[string]any{"stores": stores}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// AwaitMode blocks until a store reaches an operating mode, or the deadline
// passes. It is what a test uses instead of sleeping: the detector's transition
// is driven by probe round trips, not by a timer, so the only honest way to
// wait for it is to watch for it.
func (s *Stack) AwaitMode(ctx context.Context, st *Store, want sgu.Mode, within time.Duration) error {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if st.Gateway.Mode() == want {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return fmt.Errorf("usslpd: %s was %s rather than %s after %s",
		st.ID, st.Gateway.Mode(), want, within)
}
