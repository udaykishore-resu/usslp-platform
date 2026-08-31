package labelsim

import (
	"fmt"
	"math"
	"time"

	"github.com/usslp/usslp/edge/mesh"
	"github.com/usslp/usslp/edge/sim"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/pki"
)

// ZoneSpec describes one Shelf Edge Controller's worth of hardware: the eight
// metres or so of shelving it owns, the relays down the rail and the labels
// clipped to it.
type ZoneSpec struct {
	StoreID canon.StoreID
	SECID   canon.SECID
	// Labels is how many battery labels to create.
	Labels int
	// Relays is how many mains-powered relay units sit along the rail. Zero
	// sizes them from the label count and the mesh's child limit, which is what
	// a site survey tool does.
	Relays int
	// AisleLengthM is the run of shelving the zone covers. The default of 40 m
	// is a full supermarket aisle, which is deliberately longer than one radio
	// hop so that the topology is a real mesh rather than a star.
	AisleLengthM float64
	// Tier is the panel fitted to most labels.
	Tier DisplayTier
	// ColourEvery makes every Nth label a colour panel, modelling the promotion
	// ends and fresh counters that get one. Zero means none.
	ColourEvery int
	// ChillerFraction is the share of labels on refrigerated shelving, which run
	// at 4 degrees and lose capacity for it.
	ChillerFraction float64
	Power           PowerProfile
	// KeyRing is the price-authority ring every label in the zone verifies
	// against. A zone whose labels require end-to-end attestation and have no
	// ring refuses every price, which is fail-closed and deliberate.
	KeyRing *pki.KeyRing
	// Attestation selects whether the zone's labels insist on verifying for
	// themselves. The zero value requires it.
	Attestation AttestationMode
	// StrictClock refuses an attestation when a label has no trusted time.
	StrictClock bool
	// TelemetryInterval is the label health-report cadence. Zero disables it.
	TelemetryInterval time.Duration
	Mesh              mesh.Config
	// FirmwareVersion is stamped on every label in the zone.
	FirmwareVersion string
}

// Zone is a formed mesh network plus the labels on it.
type Zone struct {
	Spec ZoneSpec
	Net  *mesh.Network

	labels []*Label
	byID   map[canon.LabelID]*Label
	relays []mesh.NodeID
}

// NewZone builds a zone: the coordinator, the relay backbone and the labels,
// positioned along an aisle.
//
// The geometry matters. Placing every label within one hop of the controller
// would make every latency number in the platform optimistic by a factor of
// three, so labels are spread along the full aisle and relays are sized from
// the mesh's child limit exactly as a deployment's site survey would size them.
func NewZone(eng *sim.Engine, spec ZoneSpec) (*Zone, error) {
	if spec.Labels <= 0 {
		return nil, fmt.Errorf("labelsim: zone %q needs at least one label", spec.SECID)
	}
	if spec.SECID == "" {
		return nil, fmt.Errorf("labelsim: zone needs a controller id")
	}
	if spec.AisleLengthM <= 0 {
		spec.AisleLengthM = 40
	}
	if spec.Power.CapacityMAH == 0 {
		spec.Power = DefaultPower()
	}
	cfg := spec.Mesh.Defaults()
	if spec.Relays <= 0 {
		// Leave a quarter of each relay's child capacity spare so that a relay
		// failing does not immediately strand the labels that have to move.
		usable := int(float64(cfg.MaxChildren) * 0.75)
		if usable < 1 {
			usable = 1
		}
		spec.Relays = (spec.Labels + usable - 1) / usable
	}

	z := &Zone{Spec: spec, byID: make(map[canon.LabelID]*Label, spec.Labels)}
	z.Net = mesh.NewNetwork(eng, spec.Mesh)

	coord := mesh.NodeID(spec.SECID)
	if err := z.Net.AddNode(mesh.NodeSpec{ID: coord, Kind: mesh.KindCoordinator, Pos: mesh.Point{X: 0, Y: 0}}); err != nil {
		return nil, err
	}
	for r := 0; r < spec.Relays; r++ {
		x := spec.AisleLengthM * float64(r+1) / float64(spec.Relays+1)
		id := mesh.NodeID(fmt.Sprintf("%s-relay-%02d", spec.SECID, r))
		if err := z.Net.AddNode(mesh.NodeSpec{ID: id, Kind: mesh.KindRouter, Pos: mesh.Point{X: x, Y: 0}}); err != nil {
			return nil, err
		}
		z.relays = append(z.relays, id)
	}

	chillerCount := int(math.Round(spec.ChillerFraction * float64(spec.Labels)))
	for i := 0; i < spec.Labels; i++ {
		id := canon.LabelID(fmt.Sprintf("%s-lbl-%05d", spec.SECID, i))
		// Labels alternate between the two sides of the aisle and run its length.
		x := spec.AisleLengthM * (float64(i) + 0.5) / float64(spec.Labels)
		y := 1.2
		if i%2 == 1 {
			y = -1.2
		}
		tier := spec.Tier
		if spec.ColourEvery > 0 && i%spec.ColourEvery == 0 {
			tier = Tier585Color
		}
		ambient := 20.0
		if i < chillerCount {
			ambient = 4
		}
		lbl := New(eng, Config{
			ID: id, StoreID: spec.StoreID, SECID: spec.SECID,
			Tier: tier, Power: spec.Power, AmbientC: ambient,
			TelemetryInterval: spec.TelemetryInterval,
			FirmwareVersion:   spec.FirmwareVersion,
			KeyRing:           spec.KeyRing,
			Attestation:       spec.Attestation,
			StrictClock:       spec.StrictClock,
		})
		if err := z.Net.AddNode(mesh.NodeSpec{ID: lbl.NodeID(), Kind: mesh.KindEndDevice,
			Pos: mesh.Point{X: x, Y: y}, BatteryFraction: 1}); err != nil {
			return nil, err
		}
		if err := lbl.Attach(z.Net); err != nil {
			return nil, err
		}
		z.labels = append(z.labels, lbl)
		z.byID[id] = lbl
	}
	return z, nil
}

// Coordinator returns the zone's coordinator node, which is the controller's
// own radio.
func (z *Zone) Coordinator() mesh.NodeID { return mesh.NodeID(z.Spec.SECID) }

// Labels returns every label in the zone, in creation order.
func (z *Zone) Labels() []*Label { return z.labels }

// Relays returns the mains-powered relay nodes.
func (z *Zone) Relays() []mesh.NodeID { return z.relays }

// Label looks one up by identifier.
func (z *Zone) Label(id canon.LabelID) (*Label, bool) {
	l, ok := z.byID[id]
	return l, ok
}

// Form brings the zone's mesh up.
func (z *Zone) Form(done func(time.Duration)) { z.Net.Form(done) }

// OpenActiveWindow puts every label in the zone on its fast listen interval for
// d.
//
// This is what a controller does before a price load: it sets the flag in its
// beacon, every label picks it up in its own next receive window, and for the
// duration of the window the zone is reachable inside the platform's
// SEC-to-label budget instead of inside its 30-second resting interval. It is the mechanism
// that reconciles the latency budget with the battery budget, and it is why the
// battery projection charges only a few minutes a day at the fast rate.
func (z *Zone) OpenActiveWindow(d time.Duration) {
	for _, l := range z.labels {
		l.OpenActiveWindow(d)
	}
}

// Alive counts labels whose cells are not yet exhausted.
func (z *Zone) Alive() int {
	n := 0
	for _, l := range z.labels {
		if !l.Dead() {
			n++
		}
	}
	return n
}
