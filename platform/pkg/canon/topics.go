package canon

import (
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// Event stream (Kafka) topic names
//
// Partition counts and retention live with the topic definition rather than in
// a wiki, so that the local development broker, the docker-compose profile and
// the Terraform that provisions MSK all derive from one source of truth.
// ---------------------------------------------------------------------------

// Stream is an event-stream topic definition.
type Stream struct {
	Name           string
	Partitions     int
	RetentionHours int
	Description    string
	// Compacted topics keep the latest value per key forever, which is how a
	// restarting service rebuilds its read model without replaying 7 days of
	// history.
	Compacted bool
}

// The canonical stream catalogue. Partition counts are sized from the capacity
// model: 52k price updates/sec peak and 167k telemetry events/sec at 50M labels.
var (
	StreamPriceUpdates = Stream{"price-updates", 1024, 7 * 24, "Accepted price changes, keyed store:sku", false}
	StreamDeviceEvents = Stream{"device-events", 512, 30 * 24, "Device lifecycle: provisioned, online, offline", false}
	StreamTelemetry    = Stream{"label-telemetry", 2048, 3 * 24, "Battery, RSSI, LQI, temperature heartbeats", false}
	StreamInventory    = Stream{"inventory-sync", 256, 14 * 24, "Stock level changes from POS/ERP", false}
	StreamPromotions   = Stream{"promotion-events", 128, 90 * 24, "Promotion lifecycle", false}
	StreamAudit        = Stream{"audit-log", 64, 365 * 24, "Immutable compliance record", false}
	StreamOTA          = Stream{"ota-commands", 128, 7 * 24, "Firmware rollout commands", false}
	StreamPOSIngress   = Stream{"pos-integration", 256, 3 * 24, "Raw normalised POS traffic", false}
	StreamDelivery     = Stream{"label-delivery", 512, 7 * 24, "Delivery confirmations from the edge", false}
	StreamDLQ          = Stream{"dead-letter", 32, 14 * 24, "Poison messages for human triage", false}
	StreamLabelState   = Stream{"label-state", 512, 0, "Compacted latest state per label", true}
)

// AllStreams is the catalogue used to create topics on start-up.
func AllStreams() []Stream {
	return []Stream{
		StreamPriceUpdates, StreamDeviceEvents, StreamTelemetry, StreamInventory,
		StreamPromotions, StreamAudit, StreamOTA, StreamPOSIngress,
		StreamDelivery, StreamDLQ, StreamLabelState,
	}
}

// ---------------------------------------------------------------------------
// MQTT topic namespace
//
//	usslp/{tenant}/{region}/{store}/labels/{label}/price
//	usslp/{tenant}/{region}/{store}/sec/{sec}/heartbeat
//
// The tenant segment sits immediately below the root so that a single ACL rule
// — subscribe only to usslp/{your-tenant}/# — is sufficient isolation. Every
// broker credential in the platform is issued with exactly that constraint.
// ---------------------------------------------------------------------------

const MQTTRoot = "usslp"

// MQTT leaf names.
const (
	LeafPrice     = "price"
	LeafDisplay   = "display"
	LeafConfig    = "config"
	LeafStatus    = "status"
	LeafOTA       = "ota"
	LeafACK       = "ack"
	LeafHeartbeat = "heartbeat"
	LeafMesh      = "mesh/status"
	LeafPlanogram = "planogram/update"
	LeafPromotion = "promotion/activate"
	LeafMode      = "mode"
)

// TopicScope is the tenant/region/store triple that prefixes every MQTT topic.
type TopicScope struct {
	Tenant TenantID
	Region Region
	Store  StoreID
}

func (s TopicScope) prefix() string {
	region := string(s.Region)
	if region == "" {
		region = "global"
	}
	return strings.Join([]string{MQTTRoot, string(s.Tenant), region, string(s.Store)}, "/")
}

// Validate rejects a scope whose components would break out of the namespace.
func (s TopicScope) Validate() error {
	if !ValidID(string(s.Tenant)) {
		return fmt.Errorf("%w: tenant", ErrEnvelopeInvalid)
	}
	if s.Region != "" && !ValidID(string(s.Region)) {
		return fmt.Errorf("%w: region", ErrEnvelopeInvalid)
	}
	if !ValidID(string(s.Store)) {
		return fmt.Errorf("%w: store", ErrEnvelopeInvalid)
	}
	return nil
}

// LabelTopic builds a per-label topic, e.g. .../labels/{id}/price.
func (s TopicScope) LabelTopic(label LabelID, leaf string) string {
	return s.prefix() + "/labels/" + string(label) + "/" + leaf
}

// SECTopic builds a per-controller topic, e.g. .../sec/{id}/heartbeat.
func (s TopicScope) SECTopic(sec SECID, leaf string) string {
	return s.prefix() + "/sec/" + string(sec) + "/" + leaf
}

// StoreTopic builds a store-wide topic, e.g. .../store/planogram/update.
func (s TopicScope) StoreTopic(leaf string) string {
	return s.prefix() + "/store/" + leaf
}

// ZoneTopic addresses every label managed by one SEC. Fanning a store-wide
// promotion out per zone rather than per label turns 40,000 publishes into 25.
func (s TopicScope) ZoneTopic(sec SECID, leaf string) string {
	return s.prefix() + "/sec/" + string(sec) + "/zone/" + leaf
}

// SubscribeAllLabels is the wildcard a SEC uses for its own zone.
func (s TopicScope) SubscribeAllLabels(leaf string) string {
	return s.prefix() + "/labels/+/" + leaf
}

// SECLabelTopic addresses one label through the controller that owns it:
//
//	usslp/{tenant}/{region}/{store}/sec/{sec}/labels/{label}/{leaf}
//
// Routing a label update through its controller's namespace rather than a flat
// per-label one is what keeps fan-out affordable. A store has ~25 controllers
// and up to 40,000 labels; if every controller subscribed to a flat
// `labels/+/price` it would receive — and discard — 39,000 messages it does not
// own. With this shape each controller subscribes to exactly its own zone.
//
// Those two blueprint figures do not reconcile with each other, and the
// argument does not need them to: 40,000 labels at a realistic shelf density
// needs on the order of a kilometre of shelving, which at the one-controller-
// per-8-m spacing in INTERFACE-CONTRACTS §1 is roughly 125 controllers rather
// than 25. Either reading makes the flat filter absurd. The inconsistency is
// catalogued in docs/architecture/scalability.md §1; it is repeated here rather
// than resolved because resolving it means choosing a blueprint number this
// repository has no authority to choose.
//
// The cost is that a label reassigned to a different controller leaves a stale
// retained message behind, which the Device Registry clears by publishing a
// zero-length retained message to the old topic when it emits the reassignment.
func (s TopicScope) SECLabelTopic(sec SECID, label LabelID, leaf string) string {
	return s.prefix() + "/sec/" + string(sec) + "/labels/" + string(label) + "/" + leaf
}

// SubscribeZoneLabels is the filter a controller uses for every label it owns.
func (s TopicScope) SubscribeZoneLabels(sec SECID, leaf string) string {
	return s.prefix() + "/sec/" + string(sec) + "/labels/+/" + leaf
}

// ParseSECLabelTopic extracts scope, controller and label from a zone topic.
func ParseSECLabelTopic(topic string) (scope TopicScope, sec SECID, label LabelID, leaf string, ok bool) {
	parts := strings.Split(topic, "/")
	// usslp/tenant/region/store/sec/{sec}/labels/{label}/{leaf...}
	if len(parts) < 9 || parts[0] != MQTTRoot || parts[4] != "sec" || parts[6] != "labels" {
		return TopicScope{}, "", "", "", false
	}
	return TopicScope{
			Tenant: TenantID(parts[1]),
			Region: Region(parts[2]),
			Store:  StoreID(parts[3]),
		},
		SECID(parts[5]),
		LabelID(parts[7]),
		strings.Join(parts[8:], "/"),
		true
}

// Cloud-side subscription filters. A cloud service is authorised across every
// tenant, so its filters begin with a wildcard where a device's begin with its
// own tenant.
const (
	// FilterAllACKs matches every delivery acknowledgement from every store.
	FilterAllACKs = MQTTRoot + "/+/+/+/sec/+/labels/+/ack"
	// FilterAllHeartbeats matches every controller heartbeat.
	FilterAllHeartbeats = MQTTRoot + "/+/+/+/sec/+/heartbeat"
	// FilterAllMesh matches every mesh topology report.
	FilterAllMesh = MQTTRoot + "/+/+/+/sec/+/mesh/status"
	// FilterAllTelemetry matches every batched telemetry report.
	FilterAllTelemetry = MQTTRoot + "/+/+/+/sec/+/telemetry"
	// FilterAllStoreMode matches store autonomous-mode transitions.
	FilterAllStoreMode = MQTTRoot + "/+/+/+/store/mode"
)

// Additional leaves used between the tiers.
const (
	// LeafTelemetry carries a batch of label telemetry from a controller. The
	// controller aggregates rather than forwarding per label: 40,000 labels
	// reporting every five minutes is 133 messages/second per store, which is
	// 13 million/second across 100,000 stores. Batched per controller it is
	// 0.08 messages/second per store.
	//
	// That 13 million is a worst-case store applied uniformly to every store,
	// not the estate: 100,000 stores of 40,000 labels is 4 billion labels,
	// eighty times the 50 million fleet the rest of the model assumes. At the
	// estate's actual average of 500 labels per store the unbatched figure is
	// 167,000/second, which is still the reason to batch. See
	// docs/architecture/scalability.md §1.
	LeafTelemetry = "telemetry"
	// LeafZonePrice is a zone-wide price broadcast used for promotions that
	// touch every label in a controller's zone.
	LeafZonePrice = "price"
)

// SubscribeTenant is the wildcard granted to a tenant's own credentials.
func SubscribeTenant(t TenantID) string { return MQTTRoot + "/" + string(t) + "/#" }

// ParseLabelTopic extracts the scope and label from a per-label topic. Returns
// ok=false for any topic that does not match the expected shape, which is how
// the SEC rejects traffic that was mis-routed to it.
func ParseLabelTopic(topic string) (scope TopicScope, label LabelID, leaf string, ok bool) {
	parts := strings.Split(topic, "/")
	// usslp/tenant/region/store/labels/{label}/{leaf...}
	if len(parts) < 7 || parts[0] != MQTTRoot || parts[4] != "labels" {
		return TopicScope{}, "", "", false
	}
	return TopicScope{
			Tenant: TenantID(parts[1]),
			Region: Region(parts[2]),
			Store:  StoreID(parts[3]),
		},
		LabelID(parts[5]),
		strings.Join(parts[6:], "/"),
		true
}

// ParseSECTopic extracts the scope and controller from a per-SEC topic.
func ParseSECTopic(topic string) (scope TopicScope, sec SECID, leaf string, ok bool) {
	parts := strings.Split(topic, "/")
	if len(parts) < 7 || parts[0] != MQTTRoot || parts[4] != "sec" {
		return TopicScope{}, "", "", false
	}
	return TopicScope{
			Tenant: TenantID(parts[1]),
			Region: Region(parts[2]),
			Store:  StoreID(parts[3]),
		},
		SECID(parts[5]),
		strings.Join(parts[6:], "/"),
		true
}

// QoS levels used by the platform, mirroring the MQTT specification.
//
// Price updates are QoS 1 (at least once) because a duplicated update is
// harmless — the sequence number makes it idempotent — while a lost one is a
// compliance incident. Telemetry is QoS 0 because at 167k events/sec the cost
// of acknowledgement exceeds the value of any single heartbeat. OTA triggers are
// QoS 2 because starting a firmware download twice wastes mesh bandwidth that
// battery-powered devices cannot spare.
const (
	QoSTelemetry = 0
	QoSPrice     = 1
	QoSConfig    = 1
	QoSOTA       = 2
)
