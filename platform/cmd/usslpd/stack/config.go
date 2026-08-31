package stack

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// DefaultPorts are the ports documented in deploy/README.md, so that a person
// who has read the deployment guide finds every service where they expect it.
//
// Control is the one addition: usslpd owns a small operations surface of its
// own (fleet introspection and fault injection) that no single service can
// answer, because the questions it answers — "is this store autonomous", "cut
// the WAN" — are about the assembly rather than about any component.
type Ports struct {
	Control      int
	APIGateway   int
	UIG          int
	Label        int
	Registry     int
	OTA          int
	Pricing      int
	Promotion    int
	Analytics    int
	CloudMQTT    int
	AdminBase    int // api-gateway admin; each service takes AdminBase+n
	StoreMQTTBse int // first store's broker; store i takes StoreMQTTBse+i
	StoreAdmnBse int // first store's diagnostics surface
}

// DefaultPorts returns the documented layout.
func DefaultPorts() Ports {
	return Ports{
		Control: 8079, APIGateway: 8080, UIG: 8081, Label: 8082, Registry: 8083,
		OTA: 8084, Pricing: 8085, Promotion: 8086, Analytics: 8087,
		CloudMQTT: 1884, AdminBase: 9080,
		StoreMQTTBse: 1883, StoreAdmnBse: 8090,
	}
}

// EphemeralPorts returns a layout where the operating system chooses every
// port. It is what the end-to-end suite uses so that tests may run in parallel
// and so that a developer's own `make run` is not a reason for a test to fail.
func EphemeralPorts() Ports { return Ports{} }

// Config parameterises the whole runtime.
type Config struct {
	// DataDir is where the event log, the key/value stores and the certificate
	// hierarchy live. Empty means ./data/usslpd.
	DataDir string
	// Ephemeral puts the data directory in a temporary location that Stop
	// removes, and lets the operating system choose every port.
	Ephemeral bool

	// Tenants are the tenants to create. Empty means one, "demo-retail". Each
	// tenant gets its own POS binding, its own stores, and its own slice of the
	// MQTT namespace.
	Tenants []canon.TenantID
	// Stores is the number of stores *per tenant*.
	Stores int
	// ControllersPerStore is the number of Shelf Edge Controllers per store. A
	// real supermarket runs about 25.
	ControllersPerStore int
	// LabelsPerController is the number of labels on each controller's zone. A
	// real controller covers an 8 m shelf section: 200 to 1,600 labels.
	LabelsPerController int
	// Seed fixes every pseudo-random draw in the edge simulation, so the same
	// seed produces the same store: the same mesh parents, the same battery
	// levels, the same join order.
	Seed int64

	// SimSpeed is the ratio of simulated to real time in the edge model. It
	// must be 1 for any latency measurement to mean anything — at 60 the
	// E-Ink waveform that really takes 1.5 s appears to take 25 ms — so the
	// default is 1 and raising it is a deliberate choice for a battery study.
	SimSpeed float64

	// Region stamps every envelope and forms the region segment of every MQTT
	// topic.
	Region string
	// Currency is the trading currency of the generated stores.
	Currency string

	// Ports is the listener layout. The zero value means every port is chosen
	// by the operating system.
	Ports Ports

	// DevPartitions overrides the partition count every stream is provisioned
	// with. Zero means DefaultDevPartitions. See devStreams for why.
	DevPartitions int

	// LogLevel and LogFormat are passed to every service's obs.Runtime.
	LogLevel  string
	LogFormat string

	// StartTimeout bounds start-up. Exceeding it is a failure rather than a
	// hang, because the most common cause is a controller that never became
	// ready and the second most common is a port already in use.
	StartTimeout time.Duration

	// RegistrySweepInterval turns on the Device Registry's periodic health
	// derivation — the sweep that marks a silent device offline. Zero, the
	// default, leaves it off.
	//
	// It is off because regapp.Service.SweepHealth races the registry's own
	// mesh-report ingest under the race detector; see Stack.registrySweep for
	// the details and for what turning it off does and does not cost.
	RegistrySweepInterval time.Duration
}

// Defaults for a laptop-sized store.
const (
	// DefaultStores is one: the interesting multi-store behaviour (autonomy,
	// reconciliation) is per store, so a second one costs time without proving
	// anything a test does not already prove.
	DefaultStores = 1
	// DefaultControllers is four rather than a real store's twenty-five. Each
	// controller is a separate mesh network with its own radio model; four is
	// enough for the mesh, the fan-out and the OTA cohort behaviour to be real
	// and few enough that the whole platform boots in seconds.
	DefaultControllers = 4
	// DefaultLabels is 25 per controller, so a default store is 100 labels.
	DefaultLabels = 25
	// DefaultDevPartitions is the partition count every stream is created with
	// in this deployment shape.
	DefaultDevPartitions = 4
)

// withDefaults fills in everything unset and validates the rest.
func (c Config) withDefaults() (Config, error) {
	if len(c.Tenants) == 0 {
		c.Tenants = []canon.TenantID{"demo-retail"}
	}
	for _, t := range c.Tenants {
		if !canon.ValidID(string(t)) {
			return c, fmt.Errorf("usslpd: %q is not a usable tenant id", t)
		}
	}
	if c.Stores <= 0 {
		c.Stores = DefaultStores
	}
	if c.ControllersPerStore <= 0 {
		c.ControllersPerStore = DefaultControllers
	}
	if c.LabelsPerController <= 0 {
		c.LabelsPerController = DefaultLabels
	}
	if c.SimSpeed <= 0 {
		c.SimSpeed = 1
	}
	if c.Region == "" {
		c.Region = "local"
	}
	if c.Currency == "" {
		c.Currency = "USD"
	}
	if c.DevPartitions <= 0 {
		c.DevPartitions = DefaultDevPartitions
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.LogFormat == "" {
		c.LogFormat = "text"
	}
	if c.StartTimeout <= 0 {
		c.StartTimeout = 90 * time.Second
	}
	if c.Ephemeral {
		c.Ports = EphemeralPorts()
		if c.DataDir == "" {
			dir, err := os.MkdirTemp("", "usslpd-")
			if err != nil {
				return c, fmt.Errorf("usslpd: creating an ephemeral data directory: %w", err)
			}
			c.DataDir = dir
		}
	}
	if c.DataDir == "" {
		c.DataDir = filepath.Join("data", "usslpd")
	}
	if c.Ports == (Ports{}) && !c.Ephemeral {
		// A caller who set no ports and did not ask for ephemeral wants the
		// documented layout; a caller who wants ephemeral ports says so.
		c.Ports = DefaultPorts()
	}
	return c, nil
}

// StoreIDFor is the deterministic store identifier for one tenant's nth store.
//
// It embeds the tenant because store identifiers are global in the MQTT
// namespace and in the registry's anti-cloning check: two tenants each calling
// their first store "store-01" would be two tenants sharing one radio-address
// space.
func StoreIDFor(tenant canon.TenantID, n int) canon.StoreID {
	return canon.StoreID(fmt.Sprintf("%s-store-%02d", tenant, n+1))
}

// SECIDFor is the deterministic controller identifier.
func SECIDFor(store canon.StoreID, n int) canon.SECID {
	return canon.SECID(fmt.Sprintf("%s-sec-%02d", store, n+1))
}

// LabelIDFor mirrors the identifier edge/labelsim assigns inside a zone. The
// two must agree: the registry provisions the identity, the simulator owns the
// hardware, and a mismatch is a label the platform can address but not reach.
func LabelIDFor(sec canon.SECID, n int) canon.LabelID {
	return canon.LabelID(fmt.Sprintf("%s-lbl-%05d", sec, n))
}

// SKUFor is the deterministic product code at one shelf position. Distinct per
// controller and position so that a price change touches exactly one label
// unless a promotion is involved.
func SKUFor(store canon.StoreID, sec, pos int) canon.SKU {
	return canon.SKU(fmt.Sprintf("SKU-%s-%02d-%03d", strings.ToUpper(shortHash(string(store))), sec+1, pos+1))
}

// shortHash is an FNV-1a fold rendered as six hex characters. It exists so a
// SKU can carry its store's identity without carrying its whole name, and it
// does not need to be a cryptographic hash: collisions cost a duplicated SKU
// between two stores, which the platform already treats as legitimate.
func shortHash(s string) string {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return fmt.Sprintf("%06x", h&0xFFFFFF)
}

// ShopDomainFor is the Shopify shop domain the generated binding maps to a
// store. The webhook adapter reads the store from X-Shopify-Shop-Domain, so a
// test that wants to reprice a specific store sends this header.
func ShopDomainFor(store canon.StoreID) string {
	return string(store) + ".myshopify.com"
}
