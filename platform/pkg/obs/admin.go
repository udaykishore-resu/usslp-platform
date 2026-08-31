package obs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"sort"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Health
//
// Liveness and readiness are deliberately different questions. Liveness asks
// "is this process wedged?" — a failing liveness probe restarts the pod.
// Readiness asks "should traffic come here?" — a failing readiness probe just
// removes it from the endpoint list.
//
// Getting this backwards is how an event-bus blip turns into a cluster-wide
// restart storm, so USSLP services register dependency checks as *readiness*
// only, and liveness stays a simple "the process is scheduling goroutines".
// ---------------------------------------------------------------------------

// Check reports the health of one dependency.
type Check func(ctx context.Context) error

// Health is a registry of readiness checks.
type Health struct {
	mu     sync.RWMutex
	checks map[string]Check
	// ready is flipped once start-up completes. Until then readiness fails even
	// if every dependency is reachable, so that a pod is not sent traffic while
	// it is still rebuilding a read model.
	ready bool
}

// NewHealth creates a health registry in the not-ready state.
func NewHealth() *Health { return &Health{checks: map[string]Check{}} }

// Register adds a readiness check.
func (h *Health) Register(name string, c Check) {
	h.mu.Lock()
	h.checks[name] = c
	h.mu.Unlock()
}

// SetReady marks start-up complete.
func (h *Health) SetReady(ready bool) {
	h.mu.Lock()
	h.ready = ready
	h.mu.Unlock()
}

// Status runs every check and returns the results.
func (h *Health) Status(ctx context.Context) (ok bool, results map[string]string) {
	h.mu.RLock()
	ready := h.ready
	names := make([]string, 0, len(h.checks))
	for n := range h.checks {
		names = append(names, n)
	}
	checks := make(map[string]Check, len(h.checks))
	for n, c := range h.checks {
		checks[n] = c
	}
	h.mu.RUnlock()
	sort.Strings(names)

	results = make(map[string]string, len(names)+1)
	ok = ready
	if !ready {
		results["_startup"] = "not complete"
	} else {
		results["_startup"] = "ok"
	}
	for _, n := range names {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := checks[n](cctx)
		cancel()
		if err != nil {
			results[n] = err.Error()
			ok = false
		} else {
			results[n] = "ok"
		}
	}
	return ok, results
}

// ---------------------------------------------------------------------------
// Admin server
// ---------------------------------------------------------------------------

// AdminServer exposes /metrics, /healthz, /readyz and, when enabled, pprof on a
// port separate from the service's business traffic. Keeping them apart means a
// service can shed customer load while remaining scrapeable and debuggable —
// exactly when you most need it to be.
type AdminServer struct {
	srv      *http.Server
	ln       net.Listener
	registry *Registry
	health   *Health
	log      *Logger
}

// AdminConfig configures the admin surface.
type AdminConfig struct {
	// Addr is the listen address, e.g. ":9090". An empty port lets the OS pick,
	// which the test suite relies on to run services in parallel.
	Addr     string
	Registry *Registry
	Health   *Health
	Log      *Logger
	// EnablePprof exposes /debug/pprof. On in non-production environments; in
	// production it is gated behind a network policy, never left open.
	EnablePprof bool
	// BuildInfo is reported by /healthz for support triage.
	BuildInfo map[string]string
}

// NewAdminServer binds the admin listener. It returns as soon as the socket is
// open so that callers know the port before Serve is running.
func NewAdminServer(cfg AdminConfig) (*AdminServer, error) {
	if cfg.Addr == "" {
		cfg.Addr = ":0"
	}
	if cfg.Log == nil {
		cfg.Log = NopLogger()
	}
	if cfg.Health == nil {
		cfg.Health = NewHealth()
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("obs: bind admin %s: %w", cfg.Addr, err)
	}
	mux := http.NewServeMux()
	a := &AdminServer{ln: ln, registry: cfg.Registry, health: cfg.Health, log: cfg.Log}

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if a.registry != nil {
			a.registry.WriteText(w)
		}
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "alive", "build": cfg.BuildInfo})
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ok, results := a.health.Status(r.Context())
		code := http.StatusOK
		status := "ready"
		if !ok {
			code = http.StatusServiceUnavailable
			status = "not ready"
		}
		writeJSON(w, code, map[string]any{"status": status, "checks": results})
	})
	if cfg.EnablePprof {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}

	a.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return a, nil
}

// Addr returns the bound address.
func (a *AdminServer) Addr() string { return a.ln.Addr().String() }

// Serve blocks until the server is shut down.
func (a *AdminServer) Serve() error {
	err := a.srv.Serve(a.ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Start serves in the background.
func (a *AdminServer) Start() {
	go func() {
		if err := a.Serve(); err != nil {
			a.log.Error("admin server stopped", "error", err)
		}
	}()
}

// Shutdown stops the admin server.
func (a *AdminServer) Shutdown(ctx context.Context) error { return a.srv.Shutdown(ctx) }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
