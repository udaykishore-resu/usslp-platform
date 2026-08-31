// Package httpapi is the OTA service's HTTP surface.
//
// Like the registry's, it is an adapter: decode, call one application method,
// encode. The only judgement it exercises is mapping an error to a status code,
// and that mapping is a contract with its callers — a 409 means "re-read and
// retry", a 422 means "this will never work", and a 403 means the platform
// refused the artifact.
package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/usslp/usslp/platform/internal/ota/app"
	"github.com/usslp/usslp/platform/internal/ota/domain"
	"github.com/usslp/usslp/platform/pkg/eventstore"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// maxBodyBytes bounds a request body.
//
// Sixty-four megabytes is sized for the largest legitimate request: a firmware
// image, base64-encoded, which inflates it by a third. Real label firmware is
// measured in hundreds of kilobytes; the headroom is for a gateway image, and
// the limit exists because an unbounded body on a listener is a
// memory-exhaustion primitive.
const maxBodyBytes = 64 << 20

// API serves the OTA service's HTTP endpoints.
type API struct {
	ctrl *app.Controller
	log  *obs.Logger
	met  *obs.StandardMetrics
}

// New builds the HTTP surface over a rollout controller.
func New(ctrl *app.Controller, log *obs.Logger, met *obs.StandardMetrics) *API {
	if log == nil {
		log = obs.NopLogger()
	}
	return &API{ctrl: ctrl, log: log, met: met}
}

// Handler returns the router.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/firmware", a.wrap("upload_firmware", a.uploadFirmware))
	mux.HandleFunc("GET /v1/firmware", a.wrap("list_firmware", a.listFirmware))
	mux.HandleFunc("POST /v1/ota/jobs", a.wrap("create_job", a.createJob))
	mux.HandleFunc("GET /v1/ota/jobs", a.wrap("list_jobs", a.listJobs))
	mux.HandleFunc("GET /v1/ota/jobs/{id}", a.wrap("get_job", a.getJob))
	mux.HandleFunc("GET /v1/ota/jobs/{id}/devices", a.wrap("list_job_devices", a.jobDevices))
	mux.HandleFunc("POST /v1/ota/jobs/{id}/pause", a.wrap("pause_job", a.pauseJob))
	mux.HandleFunc("POST /v1/ota/jobs/{id}/resume", a.wrap("resume_job", a.resumeJob))
	mux.HandleFunc("POST /v1/ota/jobs/{id}/abort", a.wrap("abort_job", a.abortJob))
	mux.HandleFunc("POST /v1/ota/jobs/{id}/rollback", a.wrap("rollback_job", a.rollbackJob))
	mux.HandleFunc("POST /v1/ota/jobs/{id}/tick", a.wrap("tick_job", a.tickJob))
	mux.HandleFunc("POST /v1/ota/results", a.wrap("record_result", a.recordResult))
	return mux
}

type handlerFunc func(w http.ResponseWriter, r *http.Request) (int, any, error)

func (a *API) wrap(operation string, h handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		status, body, err := h(w, r)
		if err != nil {
			status = statusFor(err)
			a.log.Warn("ota request failed", "operation", operation, "status", status, "error", err)
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
//
// An unsigned or mis-signed artifact is a 403 rather than a 400: the request
// was well formed and the platform refused it, which is a materially different
// thing for a release pipeline to log and alert on than a malformed upload.
func statusFor(err error) int {
	switch {
	case errors.Is(err, app.ErrJobNotFound), errors.Is(err, domain.ErrArtifactNotFound):
		return http.StatusNotFound
	case errors.Is(err, domain.ErrUnsigned), errors.Is(err, domain.ErrBadSignature),
		errors.Is(err, domain.ErrDigestMismatch):
		return http.StatusForbidden
	case errors.Is(err, domain.ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, domain.ErrIllegalTransition), errors.Is(err, eventstore.ErrConcurrency):
		return http.StatusConflict
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
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%w: request body: %s", domain.ErrInvalid, err)
	}
	return nil
}

// uploadRequest is a firmware upload.
//
// The image travels base64-encoded inside JSON rather than as a multipart body.
// A firmware upload happens a few times a week from a build pipeline, so the
// 33% encoding overhead costs nothing anyone will notice, and a single JSON
// document keeps the image and the claims about it — version, tier, signature —
// in one atomic request that cannot be half-parsed.
type uploadRequest struct {
	Version      domain.Version `json:"version"`
	HardwareTier string         `json:"hardware_tier"`
	SHA256       string         `json:"sha256,omitempty"`
	Signature    string         `json:"signature"`
	ReleaseNotes string         `json:"release_notes,omitempty"`
	UploadedBy   string         `json:"uploaded_by,omitempty"`
	// Image is the firmware, base64 (standard encoding).
	Image string `json:"image"`
}

func (a *API) uploadFirmware(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	var req uploadRequest
	if err := decode(r, &req); err != nil {
		return 0, nil, err
	}
	image, err := base64.StdEncoding.DecodeString(req.Image)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: image is not valid base64: %s", domain.ErrInvalid, err)
	}
	artifact, err := a.ctrl.UploadFirmware(r.Context(), domain.Artifact{
		Version:      req.Version,
		HardwareTier: req.HardwareTier,
		SHA256:       req.SHA256,
		Signature:    req.Signature,
		ReleaseNotes: req.ReleaseNotes,
		UploadedBy:   req.UploadedBy,
	}, image)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, artifact, nil
}

func (a *API) listFirmware(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	artifacts, err := a.ctrl.Artifacts()
	if err != nil {
		return 0, nil, err
	}
	if tier := r.URL.Query().Get("hardware_tier"); tier != "" {
		filtered := artifacts[:0]
		for _, art := range artifacts {
			if art.HardwareTier == tier {
				filtered = append(filtered, art)
			}
		}
		artifacts = filtered
	}
	return http.StatusOK, map[string]any{"count": len(artifacts), "artifacts": artifacts}, nil
}

func (a *API) createJob(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	var spec app.JobSpec
	if err := decode(r, &spec); err != nil {
		return 0, nil, err
	}
	job, err := a.ctrl.CreateJob(r.Context(), spec)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, job, nil
}

func (a *API) listJobs(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	jobs := a.ctrl.Jobs()
	if state := r.URL.Query().Get("state"); state != "" {
		filtered := jobs[:0]
		for _, j := range jobs {
			if string(j.State) == state {
				filtered = append(filtered, j)
			}
		}
		jobs = filtered
	}
	return http.StatusOK, map[string]any{"count": len(jobs), "jobs": jobs}, nil
}

func (a *API) getJob(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	job, err := a.ctrl.Job(r.PathValue("id"))
	if err != nil {
		return 0, nil, err
	}
	// The live cohort progress is what an operator watching a rollout is here
	// for, so it is surfaced beside the job rather than requiring a second call
	// to the device listing.
	return http.StatusOK, map[string]any{
		"job":          job,
		"current_wave": job.CurrentWave,
		"waves":        job.Waves,
	}, nil
}

func (a *API) jobDevices(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	status := domain.DeviceStatus(r.URL.Query().Get("status"))
	devices, err := a.ctrl.JobDevices(r.PathValue("id"), status)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{
		"job_id":  r.PathValue("id"),
		"status":  string(status),
		"count":   len(devices),
		"devices": devices,
	}, nil
}

type actorBody struct {
	Actor  string `json:"actor,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func (a *API) readActor(r *http.Request) (actorBody, error) {
	var body actorBody
	if r.ContentLength > 0 {
		if err := decode(r, &body); err != nil {
			return body, err
		}
	}
	if body.Actor == "" {
		body.Actor = "operator"
	}
	return body, nil
}

func (a *API) pauseJob(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	body, err := a.readActor(r)
	if err != nil {
		return 0, nil, err
	}
	if err := a.ctrl.Pause(r.Context(), r.PathValue("id"), body.Actor); err != nil {
		return 0, nil, err
	}
	return a.getJob(nil, r)
}

func (a *API) resumeJob(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	body, err := a.readActor(r)
	if err != nil {
		return 0, nil, err
	}
	if err := a.ctrl.Resume(r.Context(), r.PathValue("id"), body.Actor); err != nil {
		return 0, nil, err
	}
	return a.getJob(nil, r)
}

func (a *API) abortJob(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	body, err := a.readActor(r)
	if err != nil {
		return 0, nil, err
	}
	if err := a.ctrl.Abort(r.Context(), r.PathValue("id"), body.Actor, body.Reason); err != nil {
		return 0, nil, err
	}
	return a.getJob(nil, r)
}

func (a *API) rollbackJob(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	body, err := a.readActor(r)
	if err != nil {
		return 0, nil, err
	}
	if err := a.ctrl.Rollback(r.Context(), r.PathValue("id"), body.Actor); err != nil {
		return 0, nil, err
	}
	return a.getJob(nil, r)
}

// tickJob advances one rollout on demand.
//
// The controller runs its own loop on a timer; this endpoint exists so that an
// operator resuming a paused job does not wait a whole interval to see it move,
// and so that an end-to-end demo can drive a four-stage rollout in a second
// rather than in real time.
func (a *API) tickJob(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	res, err := a.ctrl.TickJob(r.Context(), r.PathValue("id"))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, res, nil
}

// recordResult accepts a device's firmware outcome over HTTP.
//
// Devices report over MQTT; this endpoint is for a Store Gateway relaying on
// behalf of a mesh that cannot reach the broker, and for the end-to-end test.
func (a *API) recordResult(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	var update domain.DeviceUpdate
	if err := decode(r, &update); err != nil {
		return 0, nil, err
	}
	if err := a.ctrl.RecordOutcome(r.Context(), update); err != nil {
		return 0, nil, err
	}
	return http.StatusAccepted, map[string]any{
		"job_id": update.JobID, "device_id": update.DeviceID, "status": update.Status,
	}, nil
}
