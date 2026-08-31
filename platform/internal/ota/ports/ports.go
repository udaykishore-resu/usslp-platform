// Package ports declares the interfaces the OTA service's application layer
// depends on.
//
// The OTA service talks to five things it does not own: a durable event stream,
// an MQTT broker, a store for firmware images, the Device Registry, and a
// clock. Each is an interface here so that the rollout controller — the part
// whose bugs cost service visits — can be driven through a complete four-stage
// rollout, a quiet-hours suppression and an automatic rollback in a unit test.
package ports

import (
	"context"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/msgbus"
)

// EventStreamPublisher publishes envelopes onto a platform event stream.
type EventStreamPublisher interface {
	PublishEvents(ctx context.Context, stream string, envs ...canon.Envelope) error
}

// DeviceMessenger is the MQTT path to the stores.
type DeviceMessenger interface {
	Publish(ctx context.Context, m msgbus.Message) error
	Subscribe(ctx context.Context, filter string, qos msgbus.QoS, h msgbus.Handler) error
}

// Target is one device a rollout may address, as the Device Registry describes
// it.
//
// It is a projection of the registry's device rather than the whole thing,
// carrying exactly what a rollout decision needs. Keeping it narrow is what
// stops the OTA service from growing its own opinion about device lifecycle:
// the registry has already decided this device is addressable, and the only
// remaining questions here are which firmware it runs, which controller's
// airtime it will consume, and what time it is where it lives.
type Target struct {
	DeviceID string        `json:"device_id"`
	StoreID  canon.StoreID `json:"store_id"`
	SECID    canon.SECID   `json:"sec_id"`
	Zone     string        `json:"zone,omitempty"`
	// HardwareTier must match the artifact's, or the image will not boot.
	HardwareTier string `json:"hardware_tier"`
	// FirmwareVersion is what the device is running now. It decides whether the
	// device is eligible at all and whether a delta can be used.
	FirmwareVersion string `json:"firmware_version"`
	// BatteryPct is the device's last reported charge. A firmware download is
	// the most expensive thing a label ever does with its radio, so a label with
	// a nearly flat cell is skipped rather than finished off by the update.
	BatteryPct int `json:"battery_pct"`
	// TimeZone is the store's IANA location, used to evaluate quiet hours in
	// the store's own local time rather than the platform's.
	TimeZone string `json:"time_zone,omitempty"`
}

// FleetDirectory is the Device Registry as the OTA service sees it.
//
// The registry decides which devices exist and which may be addressed; this
// service decides which of those to update and when. Keeping the first decision
// on the registry's side of an interface is what stops the two from disagreeing
// about a quarantined label — a disagreement whose failure mode is a firmware
// download pushed at a device the platform has decided it cannot trust.
type FleetDirectory interface {
	// Targets returns every addressable device matching a tenant, an optional
	// set of stores, and a hardware tier. An empty store list means every store
	// the registry knows for that tenant.
	Targets(ctx context.Context, tenant canon.TenantID, stores []canon.StoreID, hardwareTier string) ([]Target, error)
}

// ArtifactStore holds firmware images, addressed by content.
//
// Content addressing is the whole design: an artifact identifier is the SHA-256
// of the bytes, so two uploads of the same image are the same artifact, a
// corrupted write cannot masquerade as a good one, and the identifier a device
// is told to fetch is itself the integrity check.
type ArtifactStore interface {
	// Put stores an image and returns its content address. Storing an image
	// that is already present is a no-op that returns the same address.
	Put(image []byte) (string, error)
	// Get returns an image by content address.
	Get(artifactID string) ([]byte, error)
	// Has reports whether an image is stored.
	Has(artifactID string) bool
}

// Clock is the OTA service's view of time.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production clock.
type SystemClock struct{}

// Now returns the current UTC time. Everything internal is UTC; a store's local
// time is derived from its IANA zone at the one place it matters, the
// quiet-hours check.
func (SystemClock) Now() time.Time { return time.Now().UTC() }
