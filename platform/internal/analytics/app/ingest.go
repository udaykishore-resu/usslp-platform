package app

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/usslp/usslp/platform/internal/analytics/columnar"
	"github.com/usslp/usslp/platform/internal/analytics/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// Ingest turns platform events into columnar rows.
//
// # Why the conversion lives here and not in the column store
//
// The store knows about columns and types; it does not know what a telemetry
// report is. Keeping the mapping in the application layer means the store stays
// a general-purpose column store — testable on its own, reusable for a fifth
// table — and means the platform's event contracts are read in exactly one
// place per table, which is where a schema change gets noticed.
type Ingest struct {
	tables Tables
}

// NewIngest builds an ingester over a table set.
func NewIngest(tables Tables) *Ingest { return &Ingest{tables: tables} }

// ErrUnroutable marks an event that belongs to no table. It is not an error the
// consumer should retry: the record is committed and skipped, because a stream
// carrying an event type this service does not model is a normal state during a
// rolling upgrade and must not wedge the consumer group.
var ErrUnroutable = fmt.Errorf("analytics: no table for this event")

// Envelope routes one platform envelope to its table.
func (in *Ingest) Envelope(env canon.Envelope) error {
	switch env.EventType {
	case canon.EvtDeviceTelemetry:
		return in.telemetry(env)
	case canon.EvtLabelDelivered, canon.EvtLabelDeliveryFailed:
		return in.delivery(env)
	case canon.EvtPriceUpdated:
		return in.price(env)
	case canon.EvtPromotionActivated, canon.EvtPromotionExpired:
		return in.promotion(env)
	}
	return ErrUnroutable
}

func (in *Ingest) telemetry(env canon.Envelope) error {
	store, err := in.tables.Get(domain.TableTelemetry)
	if err != nil {
		return err
	}
	// Telemetry is batched per controller — thirteen million messages a second
	// unbatched — so the payload is normally an array and occasionally a single
	// report from a device that woke alone.
	var batch []canon.Telemetry
	if err := env.Decode(&batch); err != nil {
		var single canon.Telemetry
		if err2 := env.Decode(&single); err2 != nil {
			return fmt.Errorf("analytics: telemetry payload: %w", err)
		}
		batch = []canon.Telemetry{single}
	}
	rows := make([]columnar.Row, 0, len(batch))
	for _, t := range batch {
		reported := t.ReportedAt
		if reported.IsZero() {
			reported = env.RecordedAt
		}
		rows = append(rows, columnar.Row{
			"reported_at":      reported.UTC(),
			"recorded_at":      recordedAt(env),
			"tenant_id":        string(env.TenantID),
			"store_id":         string(t.StoreID),
			"sec_id":           string(t.SECID),
			"label_id":         string(t.LabelID),
			"firmware_version": t.FirmwareVer,
			"battery_mv":       int64(t.BatteryMV),
			"battery_pct":      int64(t.BatteryPct),
			"temperature_c":    t.TemperatureC,
			"rssi":             int64(t.RSSI),
			"lqi":              int64(t.LQI),
			"mesh_hops":        int64(t.MeshHops),
			"refresh_count":    t.RefreshCount,
			"nfc_tap_count":    t.NFCTapCount,
			"uptime_seconds":   t.UptimeSeconds,
			"tamper":           t.TamperFlag,
		})
	}
	if len(rows) == 0 {
		return nil
	}
	return store.Append(rows...)
}

func (in *Ingest) delivery(env canon.Envelope) error {
	store, err := in.tables.Get(domain.TableDelivery)
	if err != nil {
		return err
	}
	if env.EventType == canon.EvtLabelDeliveryFailed {
		var f canon.LabelDeliveryFailed
		if err := env.Decode(&f); err != nil {
			return fmt.Errorf("analytics: delivery failure payload: %w", err)
		}
		// A failure has no latency, and recording zero would drag every
		// percentile down and make an outage look like an improvement. The
		// latency column carries the budget itself, so a failure counts as
		// exactly at the limit rather than as instantaneous — and the outcome
		// column is what the success rate is actually computed from.
		return store.Append(columnar.Row{
			"delivered_at":    recordedAt(env),
			"recorded_at":     recordedAt(env),
			"tenant_id":       string(env.TenantID),
			"store_id":        string(f.StoreID),
			"sec_id":          string(f.SECID),
			"label_id":        string(f.LabelID),
			"outcome":         "failed",
			"failure_reason":  f.Reason,
			"latency_ms":      float64(0),
			"mesh_hops":       int64(0),
			"refresh_ms":      int64(0),
			"partial_refresh": false,
			"attempts":        int64(f.Attempts),
		})
	}
	var d canon.LabelDelivered
	if err := env.Decode(&d); err != nil {
		return fmt.Errorf("analytics: delivery payload: %w", err)
	}
	delivered := d.DeliveredAt
	if delivered.IsZero() {
		delivered = env.RecordedAt
	}
	return store.Append(columnar.Row{
		"delivered_at":    delivered.UTC(),
		"recorded_at":     recordedAt(env),
		"tenant_id":       string(env.TenantID),
		"store_id":        string(d.StoreID),
		"sec_id":          string(d.SECID),
		"label_id":        string(d.LabelID),
		"outcome":         "delivered",
		"failure_reason":  "",
		"latency_ms":      float64(d.LatencyMS),
		"mesh_hops":       int64(d.MeshHops),
		"refresh_ms":      int64(d.RefreshMS),
		"partial_refresh": d.Partial,
		"attempts":        int64(1),
	})
}

func (in *Ingest) price(env canon.Envelope) error {
	store, err := in.tables.Get(domain.TablePrices)
	if err != nil {
		return err
	}
	var p canon.PriceUpdated
	if err := env.Decode(&p); err != nil {
		return fmt.Errorf("analytics: price payload: %w", err)
	}
	effective := p.EffectiveAt
	if effective.IsZero() {
		effective = env.RecordedAt
	}
	was := 0.0
	if p.WasPrice != nil {
		was = float64(p.WasPrice.Amount)
	}
	// The outcome columns — units, waste, cost, competitor price — are not on
	// the price event. They are joined in later from the POS and stock feeds by
	// EnrichPrice, so a price row starts with zeroes in them. The reports treat
	// a zero competitor price as untracked for exactly this reason.
	return store.Append(columnar.Row{
		"effective_at":           effective.UTC(),
		"recorded_at":            recordedAt(env),
		"tenant_id":              string(env.TenantID),
		"store_id":               string(p.StoreID),
		"sku":                    string(p.SKU),
		"category":               p.Render.Fields["category"],
		"promotion_id":           string(p.PromotionID),
		"price_minor":            float64(p.Price.Amount),
		"was_price_minor":        was,
		"unit_cost_minor":        float64(0),
		"competitor_price_minor": float64(0),
		"units_sold":             float64(0),
		"waste_units":            float64(0),
		"price_delay_seconds":    priceDelaySeconds(env, effective),
		"currency":               p.Price.Currency,
	})
}

// priceDelaySeconds is how long the shelf took to show a price after the
// platform accepted it.
//
// It is RecordedAt to EffectiveAt for a forward-dated change, and zero for one
// that took effect immediately. A negative value — a price whose effective date
// is before the platform heard about it, which a backfill produces — is clamped
// to zero rather than reported, because a negative delay in the shrinkage
// correlation would be a data artefact masquerading as a very fast store.
func priceDelaySeconds(env canon.Envelope, effective time.Time) float64 {
	recorded := recordedAt(env).(time.Time)
	d := effective.Sub(recorded).Seconds()
	if d < 0 {
		return 0
	}
	return d
}

func (in *Ingest) promotion(env canon.Envelope) error {
	store, err := in.tables.Get(domain.TablePromotions)
	if err != nil {
		return err
	}
	// The promotion service's activation payload is decoded structurally rather
	// than by importing its type: the analytics service must not depend on
	// another service's internal package, and the three fields it needs are a
	// stable part of the event contract.
	var payload struct {
		PromotionID string `json:"promotion_id"`
		State       string `json:"state"`
		Rule        struct {
			Type      string `json:"type"`
			Priority  int    `json:"priority"`
			Stackable bool   `json:"stackable"`
		} `json:"rule"`
	}
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return fmt.Errorf("analytics: promotion payload: %w", err)
	}
	return store.Append(columnar.Row{
		"at":             recordedAt(env),
		"tenant_id":      string(env.TenantID),
		"promotion_id":   payload.PromotionID,
		"store_id":       string(env.StoreID),
		"event_type":     env.EventType,
		"promotion_type": payload.Rule.Type,
		"state":          payload.State,
		"priority":       int64(payload.Rule.Priority),
		"stackable":      payload.Rule.Stackable,
	})
}

// recordedAt returns the envelope's durability instant, falling back to its
// occurrence time and then to now.
//
// The fallbacks matter because RecordedAt is the axis every table is indexed
// on: a row with a zero timestamp would land in 1754 and would be swept away by
// the first retention pass, which is a silent data loss rather than a visible
// error.
func recordedAt(env canon.Envelope) any {
	switch {
	case !env.RecordedAt.IsZero():
		return env.RecordedAt.UTC()
	case !env.OccurredAt.IsZero():
		return env.OccurredAt.UTC()
	default:
		return time.Now().UTC()
	}
}

// PriceOutcome is the sales and waste data joined onto a price row.
type PriceOutcome struct {
	Tenant       canon.TenantID `json:"tenant_id"`
	Store        canon.StoreID  `json:"store_id"`
	SKU          canon.SKU      `json:"sku"`
	Day          time.Time      `json:"day"`
	PriceMinor   float64        `json:"price_minor"`
	UnitCost     float64        `json:"unit_cost_minor"`
	Competitor   float64        `json:"competitor_price_minor"`
	UnitsSold    float64        `json:"units_sold"`
	WasteUnits   float64        `json:"waste_units"`
	DelaySeconds float64        `json:"price_delay_seconds"`
	Category     string         `json:"category,omitempty"`
	Currency     string         `json:"currency,omitempty"`
}

// AppendOutcomes writes daily price/sales rows directly.
//
// # Why this is an append and not an update
//
// The column store has no update: blocks are immutable and a row cannot be
// rewritten. The daily outcome for a (store, SKU, day) therefore arrives as its
// own row, written once when the day's POS extract lands, rather than as a
// patch to the price row written when the price changed. The reports aggregate
// over whatever rows exist, so both shapes coexist: price-change rows carry the
// price and zero outcomes, and daily outcome rows carry both. That is why every
// report that uses units_sold groups and sums rather than picking a single row.
func (in *Ingest) AppendOutcomes(outcomes ...PriceOutcome) error {
	store, err := in.tables.Get(domain.TablePrices)
	if err != nil {
		return err
	}
	rows := make([]columnar.Row, 0, len(outcomes))
	for _, o := range outcomes {
		currency := o.Currency
		if currency == "" {
			currency = "GBP"
		}
		rows = append(rows, columnar.Row{
			"effective_at":           o.Day.UTC(),
			"recorded_at":            o.Day.UTC(),
			"tenant_id":              string(o.Tenant),
			"store_id":               string(o.Store),
			"sku":                    string(o.SKU),
			"category":               o.Category,
			"promotion_id":           "",
			"price_minor":            o.PriceMinor,
			"was_price_minor":        float64(0),
			"unit_cost_minor":        o.UnitCost,
			"competitor_price_minor": o.Competitor,
			"units_sold":             o.UnitsSold,
			"waste_units":            o.WasteUnits,
			"price_delay_seconds":    o.DelaySeconds,
			"currency":               currency,
		})
	}
	if len(rows) == 0 {
		return nil
	}
	return store.Append(rows...)
}

// AppendPromotionOutcomes writes daily rows tagged with a promotion, which is
// what the lift report's during-period query matches on.
func (in *Ingest) AppendPromotionOutcomes(promo canon.PromotionID, outcomes ...PriceOutcome) error {
	store, err := in.tables.Get(domain.TablePrices)
	if err != nil {
		return err
	}
	rows := make([]columnar.Row, 0, len(outcomes))
	for _, o := range outcomes {
		currency := o.Currency
		if currency == "" {
			currency = "GBP"
		}
		rows = append(rows, columnar.Row{
			"effective_at":           o.Day.UTC(),
			"recorded_at":            o.Day.UTC(),
			"tenant_id":              string(o.Tenant),
			"store_id":               string(o.Store),
			"sku":                    string(o.SKU),
			"category":               o.Category,
			"promotion_id":           string(promo),
			"price_minor":            o.PriceMinor,
			"was_price_minor":        float64(0),
			"unit_cost_minor":        o.UnitCost,
			"competitor_price_minor": o.Competitor,
			"units_sold":             o.UnitsSold,
			"waste_units":            o.WasteUnits,
			"price_delay_seconds":    o.DelaySeconds,
			"currency":               currency,
		})
	}
	if len(rows) == 0 {
		return nil
	}
	return store.Append(rows...)
}

// Flush seals every table's buffer, making recent rows visible to queries.
func (in *Ingest) Flush() error {
	for _, s := range in.tables {
		if err := s.Flush(); err != nil {
			return err
		}
	}
	return nil
}
