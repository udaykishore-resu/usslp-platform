// Package ports declares the interfaces the Device Registry's application layer
// depends on, in the registry's own vocabulary rather than any vendor's.
//
// The registry talks to four things it does not own: a durable event stream, an
// MQTT broker, a certificate authority and a clock. Each is a one-method-family
// interface here so that the application can be exercised end to end — real
// provisioning, real planogram diffs, real rollbacks — against fakes, and so
// that swapping the embedded event log for Kafka or the in-tree broker for EMQX
// is an adapter change and not a rewrite.
//
// The clock is a port for the same reason the others are. Half of this
// service's behaviour is time-dependent — three missed beacons, a battery
// runway fitted over hours, an OTA quiet window — and a test that has to sleep
// to observe any of it is a test that will eventually fail on a loaded machine.
package ports

import (
	"context"
	"crypto/x509"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/msgbus"
	"github.com/usslp/usslp/platform/pkg/pki"
)

// EventStreamPublisher publishes envelopes onto a named platform event stream
// (`device-events`, `label-telemetry`).
//
// It takes a batch because telemetry arrives batched per controller — one
// message carrying up to a few thousand labels' worth of readings — and
// publishing those one at a time would multiply the platform's highest-volume
// path by its per-call overhead.
type EventStreamPublisher interface {
	// PublishEvents appends envelopes to a stream. It returns only once they are
	// durable, so a caller that has persisted its own state may treat a
	// successful return as "the rest of the platform will see this".
	PublishEvents(ctx context.Context, stream string, envs ...canon.Envelope) error
}

// DeviceMessenger is the MQTT side: the path to the store's broker and, through
// its bridge, to every controller and label in the building.
type DeviceMessenger interface {
	// Publish sends one message. Retained publishes are how the registry pushes
	// configuration that must survive a controller reboot, and a zero-length
	// retained publish is how it clears one.
	Publish(ctx context.Context, m msgbus.Message) error
	// Subscribe registers a handler for a topic filter.
	Subscribe(ctx context.Context, filter string, qos msgbus.QoS, h msgbus.Handler) error
}

// DeviceAuthenticator turns a certificate chain presented at provisioning into
// a verified platform identity.
//
// It exists as a port because the registry must be able to state, in a test,
// that a certificate from a foreign hierarchy is refused — and doing that
// against the real pki.Hierarchy is the only way that assertion means anything.
// The production adapter is exactly that: a thin wrapper over
// pki.Hierarchy.VerifyPeer with revocation checking left on.
type DeviceAuthenticator interface {
	// Authenticate verifies the chain at instant now and returns the identity it
	// asserts together with the verified leaf certificate. The leaf is returned
	// because the registry compares its public key against the manufacturing
	// manifest, which is the check that a second certificate minted for the same
	// identity cannot pass.
	Authenticate(chain []*x509.Certificate, now time.Time) (pki.Identity, *x509.Certificate, error)
}

// DeviceIssuer mints a device certificate for an already-authorised identity.
//
// It is a separate port from DeviceAuthenticator because the two capabilities
// must be separately grantable. A production Device Registry verifies
// certificates all day and issues none: enrolment authenticates hardware that
// a factory already flashed. Only the development seeding path needs to
// manufacture devices out of nothing, so only that deployment is given an
// issuer, and a registry configured without one cannot mint an identity even if
// its HTTP surface is reachable.
type DeviceIssuer interface {
	// IssueDevice returns a certificate and its issuing chain for the identity,
	// issued as at the instant now. The instant is a parameter rather than the
	// wall clock so that a seeded store's certificates are valid in the same
	// timeline as the registry that will verify them — a test running against a
	// fake clock would otherwise mint certificates from the future and then
	// refuse every one of them.
	IssueDevice(id pki.Identity, now time.Time) (*pki.Issued, error)
}

// Clock is the registry's view of time.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production clock.
type SystemClock struct{}

// Now returns the current UTC time. UTC everywhere is deliberate: a store's
// local time matters exactly once, in the OTA quiet-hours calculation, and it
// is converted there from an explicit time zone rather than inherited from
// whichever host the process happens to run on.
func (SystemClock) Now() time.Time { return time.Now().UTC() }
