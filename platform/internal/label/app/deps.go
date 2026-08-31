package app

import (
	"errors"
	"fmt"

	"github.com/usslp/usslp/platform/internal/label/domain"
	"github.com/usslp/usslp/platform/internal/label/ports"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// SourceName is the value every envelope this service produces carries in
// Envelope.Source. Audit reviewers ask "who wrote this" first, so it is a
// constant rather than a configurable string that could drift between
// deployments.
const SourceName = "label-service"

// Deps is the set of collaborators every Label Service use case shares.
//
// Grouping them keeps constructor signatures honest — a handler that needs the
// attestor and the device publisher takes the whole set rather than growing an
// eleventh positional parameter — and it makes the wiring in service.go one
// literal instead of five.
type Deps struct {
	// Repo is the aggregate repository.
	Repo ports.Repository
	// Directory resolves which labels a price change touches.
	Directory ports.Directory
	// Attestor signs authorised prices.
	Attestor ports.Attestor
	// Device publishes updates to the store's broker.
	Device ports.DevicePublisher
	// Streams publishes envelopes onto the event streams.
	Streams ports.StreamPublisher
	// State is the query-side read model.
	State ports.StateStore
	// Schedules is the due-index for future-dated changes.
	Schedules ports.ScheduleStore
	// Policies resolves the per-tenant guard rails.
	Policies *domain.PolicySet
	// Clock is the only source of time.
	Clock ports.Clock
	// Metrics are the service's series.
	Metrics *Metrics
	// Log is the structured logger.
	Log *obs.Logger
	// Tracer starts the always-sampled spans on the price path.
	Tracer *obs.Tracer
}

// ErrMissingDependency reports an incompletely wired use case. It is returned
// from constructors rather than panicking so that a misconfiguration fails the
// process at start-up with a readable message instead of at the first price
// change with a stack trace.
var ErrMissingDependency = errors.New("label/app: missing dependency")

// withDefaults fills the optional collaborators so that handlers never
// nil-check the ones that have sensible no-op forms.
func (d Deps) withDefaults() Deps {
	if d.Policies == nil {
		d.Policies = domain.NewPolicySet()
	}
	if d.Clock == nil {
		d.Clock = ports.SystemClock{}
	}
	if d.Metrics == nil {
		d.Metrics = NewMetrics(nil)
	}
	if d.Log == nil {
		d.Log = obs.NopLogger()
	}
	if d.Tracer == nil {
		d.Tracer = obs.NewTracer(SourceName, 1)
	}
	return d
}

// requirePricePath checks the collaborators without which a price cannot
// safely reach a shelf. There is no degraded mode for any of them: a price with
// no attestation must not be published, and a price published to a device but
// not durably recorded is one no audit can explain.
func (d Deps) requirePricePath() error {
	switch {
	case d.Repo == nil:
		return fmt.Errorf("%w: Repo", ErrMissingDependency)
	case d.Attestor == nil:
		return fmt.Errorf("%w: Attestor", ErrMissingDependency)
	case d.Device == nil:
		return fmt.Errorf("%w: Device", ErrMissingDependency)
	case d.Streams == nil:
		return fmt.Errorf("%w: Streams", ErrMissingDependency)
	}
	return nil
}
