package sgu

import (
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/msgbus"
)

// ---------------------------------------------------------------------------
// The bridge
//
// INTERFACE-CONTRACTS section 3: the gateway runs the store's MQTT broker and
// is a client of the cloud's, bridging the downstream table one way and the
// upstream table the other. The routes are configuration rather than hard-coded
// strings because a tenant with a bespoke integration adds topics, and because
// a deployment that needs to stop bridging one class of traffic — a store under
// investigation whose telemetry is being withheld pending a data request, say —
// should be able to do it without a new binary.
//
// The two route sets are disjoint by construction, which is what makes the
// bridge loop-free without any message tagging: nothing the gateway republishes
// locally can match an upstream filter, and nothing it publishes to the cloud
// can match a downstream one.
// ---------------------------------------------------------------------------

// Route is one bridged topic filter.
type Route struct {
	// Filter is the MQTT topic filter to subscribe to on the source broker.
	Filter string `json:"filter"`
	// QoS is the quality of service to subscribe and republish at.
	QoS msgbus.QoS `json:"qos"`
	// Retain sets the retain flag on republication. Price and configuration
	// topics are retained so a controller rebooting after a power cut recovers
	// its zone's current state from the local broker without a round trip to a
	// cloud that may be unreachable.
	Retain bool `json:"retain"`
	// Class is the buffering class for upstream routes, which decides what
	// happens to this traffic when the queue fills during an outage.
	Class Class `json:"class,omitempty"`
	// Kind marks upstream routes whose payload participates in reconciliation.
	Kind Kind `json:"kind,omitempty"`
}

// BridgeConfig is the two route tables.
type BridgeConfig struct {
	// Downstream is bridged cloud to local: prices, configuration, firmware,
	// planograms, promotions.
	Downstream []Route `json:"downstream"`
	// Upstream is bridged local to cloud, and buffered when the cloud is not
	// reachable: acknowledgements, heartbeats, mesh topology, telemetry, store
	// mode.
	Upstream []Route `json:"upstream"`
}

// DefaultBridgeConfig builds the route tables from the contract's topic tables,
// scoped to one store.
//
// The QoS and retain settings are the contract's, not a preference: price
// updates are QoS 1 and retained because a duplicate is harmless and a loss is a
// compliance incident, telemetry is QoS 0 because at fleet scale the cost of
// acknowledging it exceeds the value of any single reading, and firmware
// triggers are QoS 2 because starting a download twice costs a battery-powered
// device an entire redundant transfer.
func DefaultBridgeConfig(scope canon.TopicScope) BridgeConfig {
	return BridgeConfig{
		Downstream: []Route{
			{Filter: scope.SubscribeZoneLabels("+", canon.LeafPrice), QoS: canon.QoSPrice, Retain: true},
			{Filter: scope.SubscribeZoneLabels("+", canon.LeafConfig), QoS: canon.QoSConfig, Retain: true},
			{Filter: scope.SubscribeZoneLabels("+", canon.LeafOTA), QoS: canon.QoSOTA, Retain: false},
			{Filter: scope.ZoneTopic("+", canon.LeafZonePrice), QoS: canon.QoSPrice, Retain: false},
			{Filter: scope.StoreTopic(canon.LeafPlanogram), QoS: canon.QoSConfig, Retain: true},
			{Filter: scope.StoreTopic(canon.LeafPromotion), QoS: canon.QoSPrice, Retain: false},
		},
		Upstream: []Route{
			{Filter: scope.SubscribeZoneLabels("+", canon.LeafACK), QoS: canon.QoSPrice,
				Class: ClassCritical, Kind: KindPricing},
			{Filter: scope.SECTopic("+", canon.LeafHeartbeat), QoS: canon.QoSTelemetry,
				Retain: true, Class: ClassLatest},
			{Filter: scope.SECTopic("+", canon.LeafStatus), QoS: canon.QoSPrice,
				Retain: true, Class: ClassLatest},
			{Filter: scope.SECTopic("+", canon.LeafMesh), QoS: canon.QoSTelemetry,
				Retain: true, Class: ClassLatest},
			{Filter: scope.SECTopic("+", canon.LeafTelemetry), QoS: canon.QoSTelemetry,
				Class: ClassBulk},
			{Filter: scope.StoreTopic(canon.LeafMode), QoS: canon.QoSPrice,
				Retain: true, Class: ClassCritical},
		},
	}
}

// withDefaults fills an empty configuration from the contract.
func (b BridgeConfig) withDefaults(scope canon.TopicScope) BridgeConfig {
	if len(b.Downstream) == 0 && len(b.Upstream) == 0 {
		return DefaultBridgeConfig(scope)
	}
	for i := range b.Upstream {
		if b.Upstream[i].Class == "" {
			b.Upstream[i].Class = ClassCritical
		}
	}
	return b
}
