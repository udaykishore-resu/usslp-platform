// Package domain defines what the analytics service stores and the retail
// intelligence it computes from it.
//
// The four tables mirror the four event streams the service consumes, one
// column per field the platform's event contracts define. They are flat and
// denormalised on purpose: a column store's join story is poor and its
// wide-table story is excellent, and every question the platform's reports ask
// is answerable from one table plus a time range.
package domain

import (
	"time"

	"github.com/usslp/usslp/platform/internal/analytics/columnar"
)

// Table names. They are constants because they appear in the on-disk directory
// layout, in the query API and in the retention policies.
const (
	// TableTelemetry holds label health reports from `label-telemetry`.
	TableTelemetry = "label_telemetry"
	// TableDelivery holds delivery confirmations from `label-delivery`. It is
	// the table the SLO is computed from.
	TableDelivery = "label_delivery"
	// TablePrices holds accepted price changes from `price-updates`.
	TablePrices = "price_updates"
	// TablePromotions holds promotion lifecycle events from
	// `promotion-events`.
	TablePromotions = "promotion_events"
)

// TelemetrySchema is the label health table.
//
// Note the two timestamps. ReportedAt is the label's own clock, which drifts and
// which a technician's watch disagrees with; RecordedAt is when the platform
// took durable responsibility. Analytics that confuse them produce negative
// latencies after a store's gateway resyncs its clock, so both are kept and the
// reports say which they use.
func TelemetrySchema() columnar.Schema {
	return columnar.Schema{
		Table:      TableTelemetry,
		TimeColumn: "reported_at",
		Columns: []columnar.Column{
			{Name: "reported_at", Type: columnar.TypeTimestamp},
			{Name: "recorded_at", Type: columnar.TypeTimestamp},
			{Name: "tenant_id", Type: columnar.TypeString},
			{Name: "store_id", Type: columnar.TypeString},
			{Name: "sec_id", Type: columnar.TypeString},
			{Name: "label_id", Type: columnar.TypeString},
			{Name: "firmware_version", Type: columnar.TypeString},
			{Name: "battery_mv", Type: columnar.TypeInt64},
			{Name: "battery_pct", Type: columnar.TypeInt64},
			{Name: "temperature_c", Type: columnar.TypeFloat64},
			{Name: "rssi", Type: columnar.TypeInt64},
			{Name: "lqi", Type: columnar.TypeInt64},
			{Name: "mesh_hops", Type: columnar.TypeInt64},
			{Name: "refresh_count", Type: columnar.TypeInt64},
			// NFCTapCount is the label-interaction signal: a shopper tapping a
			// phone against a label is the only direct measurement the platform
			// has of shelf-edge engagement, and it is what the interaction
			// report is built on.
			{Name: "nfc_tap_count", Type: columnar.TypeInt64},
			{Name: "uptime_seconds", Type: columnar.TypeInt64},
			{Name: "tamper", Type: columnar.TypeBool},
		},
	}
}

// DeliverySchema is the table the SLO is computed from.
func DeliverySchema() columnar.Schema {
	return columnar.Schema{
		Table:      TableDelivery,
		TimeColumn: "delivered_at",
		Columns: []columnar.Column{
			{Name: "delivered_at", Type: columnar.TypeTimestamp},
			{Name: "recorded_at", Type: columnar.TypeTimestamp},
			{Name: "tenant_id", Type: columnar.TypeString},
			{Name: "store_id", Type: columnar.TypeString},
			{Name: "sec_id", Type: columnar.TypeString},
			{Name: "label_id", Type: columnar.TypeString},
			// Outcome is "delivered" or "failed". Keeping failures in the same
			// table as successes is what makes the success rate a single query
			// rather than a join across two.
			{Name: "outcome", Type: columnar.TypeString},
			{Name: "failure_reason", Type: columnar.TypeString},
			// LatencyMS is measured from the envelope's RecordedAt to the
			// moment the pixels settled. It is the number the 3-second SLO is
			// written against, because it is the only one a retailer can verify
			// by looking at a shelf.
			{Name: "latency_ms", Type: columnar.TypeFloat64},
			{Name: "mesh_hops", Type: columnar.TypeInt64},
			{Name: "refresh_ms", Type: columnar.TypeInt64},
			{Name: "partial_refresh", Type: columnar.TypeBool},
			{Name: "attempts", Type: columnar.TypeInt64},
		},
	}
}

// PricesSchema is the accepted-price-change table. It is what the elasticity
// curve, the competitive position report and the shrinkage correlation all read.
func PricesSchema() columnar.Schema {
	return columnar.Schema{
		Table:      TablePrices,
		TimeColumn: "effective_at",
		Columns: []columnar.Column{
			{Name: "effective_at", Type: columnar.TypeTimestamp},
			{Name: "recorded_at", Type: columnar.TypeTimestamp},
			{Name: "tenant_id", Type: columnar.TypeString},
			{Name: "store_id", Type: columnar.TypeString},
			{Name: "sku", Type: columnar.TypeString},
			{Name: "category", Type: columnar.TypeString},
			{Name: "promotion_id", Type: columnar.TypeString},
			{Name: "price_minor", Type: columnar.TypeFloat64},
			{Name: "was_price_minor", Type: columnar.TypeFloat64},
			{Name: "unit_cost_minor", Type: columnar.TypeFloat64},
			{Name: "competitor_price_minor", Type: columnar.TypeFloat64},
			// UnitsSold and WasteUnits are the outcome columns, joined in by the
			// ingest layer from the POS and stock feeds. They live in this table
			// rather than their own because every question that uses them also
			// uses the price, and a column store answers that in one scan.
			{Name: "units_sold", Type: columnar.TypeFloat64},
			{Name: "waste_units", Type: columnar.TypeFloat64},
			// PriceDelaySeconds is how long the shelf took to show this price
			// after the decision was made. It is the column that lets the
			// shrinkage report correlate waste with pricing latency, which is
			// the argument the whole platform is sold on.
			{Name: "price_delay_seconds", Type: columnar.TypeFloat64},
			{Name: "currency", Type: columnar.TypeString},
		},
	}
}

// PromotionsSchema is the promotion lifecycle table.
func PromotionsSchema() columnar.Schema {
	return columnar.Schema{
		Table:      TablePromotions,
		TimeColumn: "at",
		Columns: []columnar.Column{
			{Name: "at", Type: columnar.TypeTimestamp},
			{Name: "tenant_id", Type: columnar.TypeString},
			{Name: "promotion_id", Type: columnar.TypeString},
			{Name: "store_id", Type: columnar.TypeString},
			{Name: "event_type", Type: columnar.TypeString},
			{Name: "promotion_type", Type: columnar.TypeString},
			{Name: "state", Type: columnar.TypeString},
			{Name: "priority", Type: columnar.TypeInt64},
			{Name: "stackable", Type: columnar.TypeBool},
		},
	}
}

// AllSchemas is the catalogue the service provisions on start-up.
func AllSchemas() []columnar.Schema {
	return []columnar.Schema{
		TelemetrySchema(), DeliverySchema(), PricesSchema(), PromotionsSchema(),
	}
}

// SchemaFor resolves a table name.
func SchemaFor(table string) (columnar.Schema, bool) {
	for _, s := range AllSchemas() {
		if s.Table == table {
			return s, true
		}
	}
	return columnar.Schema{}, false
}

// RetentionPolicy is how long a table's data stays in each tier.
type RetentionPolicy struct {
	Table string `json:"table"`
	// Hot is how long data stays on the fastest storage. Queries that a
	// dashboard makes every thirty seconds live entirely inside it.
	Hot time.Duration `json:"hot"`
	// Warm is how long it stays on cheaper storage after that.
	Warm time.Duration `json:"warm"`
	// Cold is how long it is kept at all. Zero means forever, which is correct
	// for nothing except an audit table and is not the default for any of these.
	Cold time.Duration `json:"cold"`
}

// DefaultRetention is the platform's policy set.
//
// The numbers come from what each table is used for rather than from a uniform
// rule:
//
//   - Telemetry is enormous and its value decays fastest. A week hot covers
//     every operational question; a quarter warm covers a battery-life trend;
//     beyond that it is kept only long enough to characterise a hardware
//     revision.
//   - Delivery is the SLO evidence. The error budget is monthly, so a month hot
//     covers the live budget and thirteen months warm covers year-on-year
//     comparisons and a contractual dispute.
//   - Prices are the compliance record. Several of the platform's markets
//     require a price history for years, so cold is long and deliberate.
//   - Promotions are small and are read for year-on-year planning, so the whole
//     lot stays warm cheaply.
func DefaultRetention() []RetentionPolicy {
	const day = 24 * time.Hour
	return []RetentionPolicy{
		{Table: TableTelemetry, Hot: 7 * day, Warm: 90 * day, Cold: 365 * day},
		{Table: TableDelivery, Hot: 31 * day, Warm: 396 * day, Cold: 3 * 365 * day},
		{Table: TablePrices, Hot: 90 * day, Warm: 2 * 365 * day, Cold: 7 * 365 * day},
		{Table: TablePromotions, Hot: 90 * day, Warm: 3 * 365 * day, Cold: 7 * 365 * day},
	}
}

// Validate checks a policy is monotone.
//
// Hot must be shorter than warm and warm shorter than cold, because the tiers
// are a sequence and a policy that says otherwise would move data to warm and
// then immediately delete it — which is a data-loss bug expressed as a
// configuration mistake, and is worth catching at start-up.
func (p RetentionPolicy) Validate() error {
	switch {
	case p.Hot <= 0:
		return &PolicyError{Table: p.Table, Reason: "the hot window must be positive"}
	case p.Warm > 0 && p.Warm <= p.Hot:
		return &PolicyError{Table: p.Table, Reason: "the warm window must be longer than the hot one"}
	case p.Cold > 0 && p.Warm > 0 && p.Cold <= p.Warm:
		return &PolicyError{Table: p.Table, Reason: "the cold window must be longer than the warm one"}
	}
	return nil
}

// PolicyError is an invalid retention policy.
type PolicyError struct {
	Table  string
	Reason string
}

// Error implements error.
func (e *PolicyError) Error() string {
	return "analytics: retention policy for " + e.Table + ": " + e.Reason
}
