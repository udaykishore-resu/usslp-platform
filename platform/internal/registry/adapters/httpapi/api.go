// Package httpapi is the Device Registry's HTTP surface.
//
// It is an adapter and nothing more: it decodes a request, calls one
// application method, and encodes the result. No rule about devices lives here.
// That discipline is what lets the same behaviour be exercised from a unit test
// against the application layer and from an end-to-end test through this
// package without either being a weaker version of the other.
//
// Errors are mapped to status codes by the sentinel the application returned,
// in [statusFor]. The mapping is the API's contract with its callers: a 409
// means "your view of the world is stale, re-read and try again", a 422 means
// "this will never work", and a 403 means "the platform has stopped trusting
// this device". A caller that retries on the wrong one either gives up on a
// transient problem or hammers a permanent one.
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/usslp/usslp/platform/internal/registry/app"
	"github.com/usslp/usslp/platform/internal/registry/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventstore"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// maxBodyBytes bounds a request body.
//
// Sixteen megabytes is sized for the largest legitimate request the API takes:
// a full planogram for a hypermarket, which is on the order of 40,000 positions
// of a couple of hundred bytes each. Everything else is orders of magnitude
// smaller, and an unbounded body on a public listener is a memory-exhaustion
// primitive.
const maxBodyBytes = 16 << 20

// API serves the registry's HTTP endpoints.
type API struct {
	svc *app.Service
	log *obs.Logger
	met *obs.StandardMetrics
}

// New builds the HTTP surface over an application service.
func New(svc *app.Service, log *obs.Logger, met *obs.StandardMetrics) *API {
	if log == nil {
		log = obs.NopLogger()
	}
	return &API{svc: svc, log: log, met: met}
}

// Handler returns the router. Patterns use the method-and-wildcard syntax of
// net/http's own multiplexer, so the routing table is the whole routing layer —
// there is no framework here to disagree with the standard library about what a
// path means.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/provision", a.wrap("provision", a.provision))
	mux.HandleFunc("POST /v1/manifests", a.wrap("ingest_manifest", a.ingestManifest))
	mux.HandleFunc("GET /v1/devices/{id}", a.wrap("get_device", a.getDevice))
	mux.HandleFunc("POST /v1/devices/{id}/retire", a.wrap("retire_device", a.retireDevice))
	mux.HandleFunc("POST /v1/devices/{id}/quarantine", a.wrap("quarantine_device", a.quarantineDevice))
	mux.HandleFunc("POST /v1/devices/{id}/release", a.wrap("release_device", a.releaseDevice))
	mux.HandleFunc("GET /v1/stores/{id}/devices", a.wrap("list_store_devices", a.storeDevices))
	mux.HandleFunc("GET /v1/stores/{id}/mesh", a.wrap("get_store_mesh", a.storeMesh))
	mux.HandleFunc("GET /v1/stores/{id}/health", a.wrap("get_store_health", a.storeHealth))
	mux.HandleFunc("GET /v1/stores/{id}/runway", a.wrap("get_store_runway", a.storeRunway))
	mux.HandleFunc("POST /v1/stores/{id}/planogram", a.wrap("upload_planogram", a.uploadPlanogram))
	mux.HandleFunc("GET /v1/stores/{id}/planogram", a.wrap("get_planogram", a.getPlanogram))
	mux.HandleFunc("GET /v1/fleet/summary", a.wrap("fleet_summary", a.fleetSummary))
	mux.HandleFunc("POST /v1/dev/seed", a.wrap("seed", a.seed))
	return mux
}

// handlerFunc is a handler that returns a status, a body and an error, so that
// every endpoint's success and failure paths are written once.
type handlerFunc func(w http.ResponseWriter, r *http.Request) (int, any, error)

func (a *API) wrap(operation string, h handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		status, body, err := h(w, r)
		if err != nil {
			status = statusFor(err)
			a.log.Warn("registry request failed",
				"operation", operation, "status", status, "error", err)
			writeJSON(w, status, errorBody{Error: err.Error(), Operation: operation})
		} else if body != nil {
			writeJSON(w, status, body)
		} else {
			w.WriteHeader(status)
		}
		if a.met != nil {
			a.met.ObserveRequest("http", operation, err, time.Since(started))
		}
	}
}

type errorBody struct {
	Error     string `json:"error"`
	Operation string `json:"operation"`
}

// statusFor maps an application error onto an HTTP status.
func statusFor(err error) int {
	switch {
	case errors.Is(err, domain.ErrNotFound), errors.Is(err, app.ErrUnknownDevice):
		return http.StatusNotFound
	case errors.Is(err, domain.ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, domain.ErrAlreadyExists):
		return http.StatusConflict
	case errors.Is(err, eventstore.ErrConcurrency):
		return http.StatusConflict
	case errors.Is(err, domain.ErrIllegalTransition):
		return http.StatusConflict
	case errors.Is(err, domain.ErrQuarantined), errors.Is(err, app.ErrCloneDetected),
		errors.Is(err, app.ErrManifestMismatch), errors.Is(err, app.ErrRetired):
		return http.StatusForbidden
	case errors.Is(err, app.ErrSeedingDisabled):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}

func decode(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	// Unknown fields are rejected rather than ignored. A planogram upload with
	// a misspelled "facing_count" that silently defaulted to one facing would be
	// discovered by a customer looking at a shelf, which is the most expensive
	// possible place to discover a typo.
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%w: request body: %s", domain.ErrInvalid, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (a *API) provision(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	var req app.ProvisionRequest
	if err := decode(r, &req); err != nil {
		return 0, nil, err
	}
	res, err := a.svc.Provision(r.Context(), req)
	if err != nil {
		return 0, nil, err
	}
	status := http.StatusCreated
	if res.Reprovisioned {
		status = http.StatusOK
	}
	return status, res, nil
}

func (a *API) ingestManifest(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	var m domain.Manifest
	if err := decode(r, &m); err != nil {
		return 0, nil, err
	}
	stored, err := a.svc.IngestManifest(r.Context(), &m)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusAccepted, map[string]any{
		"manifest_id": m.ManifestID,
		"records":     len(m.Records),
		"stored":      stored,
	}, nil
}

func (a *API) getDevice(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	id := r.PathValue("id")
	dev := a.svc.Device(id)
	if dev == nil {
		// A scanner reads the serial printed on the unit, not the platform
		// identifier, so the endpoint accepts either. Falling back rather than
		// exposing a second route keeps the technician's tool from having to
		// know which kind of string it is holding.
		dev = a.svc.DeviceBySerial(id)
	}
	if dev == nil {
		return 0, nil, fmt.Errorf("%w: device %s", domain.ErrNotFound, id)
	}
	runway, hasRunway := a.svc.DeviceRunway(dev.ID)
	body := map[string]any{"device": dev}
	if hasRunway {
		body["battery_runway_hours"] = runway
	}
	return http.StatusOK, body, nil
}

type reasonBody struct {
	Reason string `json:"reason,omitempty"`
}

func (a *API) retireDevice(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	var body reasonBody
	if r.ContentLength > 0 {
		if err := decode(r, &body); err != nil {
			return 0, nil, err
		}
	}
	if body.Reason == "" {
		body.Reason = "decommissioned"
	}
	if err := a.svc.Retire(r.Context(), r.PathValue("id"), body.Reason); err != nil {
		return 0, nil, err
	}
	return http.StatusOK, a.svc.Device(r.PathValue("id")), nil
}

func (a *API) quarantineDevice(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	var body reasonBody
	if r.ContentLength > 0 {
		if err := decode(r, &body); err != nil {
			return 0, nil, err
		}
	}
	if err := a.svc.Quarantine(r.Context(), r.PathValue("id"), body.Reason); err != nil {
		return 0, nil, err
	}
	return http.StatusOK, a.svc.Device(r.PathValue("id")), nil
}

func (a *API) releaseDevice(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	var body reasonBody
	if r.ContentLength > 0 {
		if err := decode(r, &body); err != nil {
			return 0, nil, err
		}
	}
	if body.Reason == "" {
		body.Reason = "released by operator"
	}
	if err := a.svc.Release(r.Context(), r.PathValue("id"), body.Reason); err != nil {
		return 0, nil, err
	}
	return http.StatusOK, a.svc.Device(r.PathValue("id")), nil
}

func (a *API) storeDevices(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	store := canon.StoreID(r.PathValue("id"))
	devices := a.svc.StoreDevices(store)
	if state := r.URL.Query().Get("state"); state != "" {
		filtered := devices[:0]
		for _, d := range devices {
			if string(d.State) == state {
				filtered = append(filtered, d)
			}
		}
		devices = filtered
	}
	if kind := r.URL.Query().Get("kind"); kind != "" {
		filtered := devices[:0]
		for _, d := range devices {
			if string(d.Kind) == kind {
				filtered = append(filtered, d)
			}
		}
		devices = filtered
	}
	return http.StatusOK, map[string]any{
		"store_id": store,
		"count":    len(devices),
		"devices":  devices,
	}, nil
}

func (a *API) storeMesh(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	store := canon.StoreID(r.PathValue("id"))
	trees := a.svc.StoreMesh(store)
	orphans := 0
	for _, t := range trees {
		orphans += len(t.Orphans)
	}
	return http.StatusOK, map[string]any{
		"store_id":      store,
		"controllers":   len(trees),
		"total_orphans": orphans,
		"mesh":          trees,
	}, nil
}

func (a *API) storeHealth(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	return http.StatusOK, a.svc.StoreHealth(canon.StoreID(r.PathValue("id"))), nil
}

func (a *API) storeRunway(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	store := canon.StoreID(r.PathValue("id"))
	entries := a.svc.StoreRunway(store)
	if limit := r.URL.Query().Get("limit"); limit != "" {
		n, err := strconv.Atoi(limit)
		if err != nil || n < 0 {
			return 0, nil, fmt.Errorf("%w: limit %q", domain.ErrInvalid, limit)
		}
		if n < len(entries) {
			entries = entries[:n]
		}
	}
	return http.StatusOK, map[string]any{
		"store_id": store,
		"count":    len(entries),
		"labels":   entries,
	}, nil
}

func (a *API) uploadPlanogram(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	var pg domain.Planogram
	if err := decode(r, &pg); err != nil {
		return 0, nil, err
	}
	store := canon.StoreID(r.PathValue("id"))
	if pg.StoreID == "" {
		pg.StoreID = store
	}
	if pg.StoreID != store {
		return 0, nil, fmt.Errorf("%w: planogram is for store %s but was uploaded to %s",
			domain.ErrInvalid, pg.StoreID, store)
	}
	res, err := a.svc.UploadPlanogram(r.Context(), &pg)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, res, nil
}

func (a *API) getPlanogram(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	store := canon.StoreID(r.PathValue("id"))
	pg := a.svc.Planogram(store)
	if pg == nil {
		return 0, nil, fmt.Errorf("%w: planogram for store %s", domain.ErrNotFound, store)
	}
	return http.StatusOK, pg, nil
}

func (a *API) fleetSummary(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	return http.StatusOK, a.svc.FleetSummary(), nil
}

func (a *API) seed(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	var req app.SeedRequest
	if err := decode(r, &req); err != nil {
		return 0, nil, err
	}
	res, err := a.svc.Seed(r.Context(), req)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, res, nil
}
