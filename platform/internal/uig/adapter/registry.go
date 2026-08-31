package adapter

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// ErrNoAdapter is returned for an adapter name nothing is registered under.
var ErrNoAdapter = errors.New("uig/adapter: no such adapter")

// ErrDuplicateAdapter is returned when two adapters claim the same name.
// Registration is refused rather than resolved last-write-wins, because the
// name is a public contract — it appears in binding configuration, metric
// labels and Envelope.Source — and silently rebinding it would reroute a
// retailer's price traffic to a different parser on a deploy.
var ErrDuplicateAdapter = errors.New("uig/adapter: adapter already registered")

// Registry holds the adapters a gateway process can route to.
//
// It is populated at start-up and read on every delivery, so it is optimised
// for concurrent reads. Registration after start-up is supported — the file
// poller registers its adapter once its directories exist — but is rare.
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{adapters: make(map[string]Adapter)}
}

// Register adds an adapter under its own name.
func (r *Registry) Register(a Adapter) error {
	if a == nil {
		return errors.New("uig/adapter: nil adapter")
	}
	name := a.Name()
	if name == "" {
		return errors.New("uig/adapter: adapter has no name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.adapters[name]; dup {
		return fmt.Errorf("%w: %s", ErrDuplicateAdapter, name)
	}
	r.adapters[name] = a
	return nil
}

// MustRegister registers an adapter and panics on failure. It is for process
// start-up, where a duplicate adapter name is a build-time mistake that must
// stop the binary rather than degrade it.
func (r *Registry) MustRegister(a Adapter) {
	if err := r.Register(a); err != nil {
		panic(err)
	}
}

// Get returns the adapter registered under name.
func (r *Registry) Get(name string) (Adapter, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoAdapter, name)
	}
	return a, nil
}

// Names returns the registered adapter names in sorted order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.adapters))
	for n := range r.adapters {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
