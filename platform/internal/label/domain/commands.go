package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// ErrAlreadyApplied reports that the price currently on the glass was produced
// by exactly the source event being replayed.
//
// It is distinct from ErrStaleUpdate because the two demand opposite handling.
// A stale update is finished business. An already-applied one means the event
// store accepted the change but the caller may never have got as far as
// publishing it to the device — the MQTT hop is the step most likely to fail —
// so the handler must republish from the aggregate's pending update rather than
// silently dropping a price change that the audit log says was authorised.
var ErrAlreadyApplied = errors.New("label: update already applied, republish pending")

// PriceChange is the command that drives everything.
//
// Now is supplied rather than read so the aggregate has no clock of its own: a
// rejection that depended on the machine's wall clock could not be reproduced
// from the audit record, and "the grace window elapsed between the two
// replicas" is not an explanation anyone should have to give a regulator.
type PriceChange struct {
	// SKU is the product being repriced. It must match the label's assignment.
	SKU canon.SKU
	// Price is the new price in minor units.
	Price canon.Money
	// WasPrice, UnitPrice and UnitMeasure are the optional comparison prices.
	WasPrice    *canon.Money
	UnitPrice   *canon.Money
	UnitMeasure string
	// EffectiveAt is when the price takes effect. In the future it schedules;
	// in the past beyond the tenant's grace it is refused.
	EffectiveAt time.Time
	// ExpiresAt, when set, is when a promotional price lapses.
	ExpiresAt *time.Time
	// PromotionID, Reason and Attributes drive template selection.
	PromotionID canon.PromotionID
	Reason      string
	Attributes  map[string]string
	// PromotionPriority is the activating rule's priority, recorded on the
	// resulting event for the audit trail.
	PromotionPriority int
	// InitiatedBy names the operator or system behind the change.
	InitiatedBy string
	// Sequence, when positive, is an explicit per-label sequence that must
	// strictly exceed the displayed one. Stream-driven updates leave it zero
	// and let the aggregate allocate, because the price-updates stream carries
	// no per-label sequence — it is keyed store:sku and one record fans out to
	// every facing of the product.
	Sequence int64
	// OccurredAt is the source system's clock for the change. It is the
	// ordering reference for out-of-order detection.
	OccurredAt time.Time
	// Now is the decision instant.
	Now time.Time
	// SourceEventID is the causing envelope, kept for lineage and for
	// already-applied detection.
	SourceEventID canon.EventID
	// ScheduleID identifies the schedule entry a future-dated change creates.
	ScheduleID string
}

func (c PriceChange) structural() error {
	switch {
	case c.Now.IsZero():
		return fmt.Errorf("%w: PriceChange.Now is required", ErrInvalidCommand)
	case c.EffectiveAt.IsZero():
		return fmt.Errorf("%w: PriceChange.EffectiveAt is required", ErrInvalidCommand)
	case c.SKU == "":
		return fmt.Errorf("%w: PriceChange.SKU is required", ErrInvalidCommand)
	}
	return nil
}

// ApplyPriceChange decides what a proposed price change does to the label.
//
// The order of the checks is itself a design decision. Identity and assignment
// come first because a price for the wrong product is wrong regardless of its
// value; sequencing comes before the value rules because a stale record must
// not generate a rejection event on every redelivery; the guard rail comes last
// because it is the only check that compares against what is currently on the
// glass, and it should not fire on an update that would have been refused for a
// more fundamental reason.
//
// Returns:
//   - (events, nil) when the change is accepted or scheduled;
//   - (events, ErrRejected) when it is refused — the events are the audit
//     record and must still be persisted;
//   - (nil, ErrStaleUpdate | ErrOutOfOrder) when it changes nothing;
//   - (nil, ErrAlreadyApplied) when this exact source event is already on the
//     glass and only the device publish may be outstanding.
func (l *Label) ApplyPriceChange(cmd PriceChange, policy Policy) ([]Event, error) {
	policy = policy.WithDefaults()
	if err := cmd.structural(); err != nil {
		return nil, err
	}
	now := cmd.Now.UTC()
	occurred := cmd.OccurredAt
	if occurred.IsZero() {
		occurred = now
	}
	occurred = occurred.UTC()

	rejected := func(reason, detail string) ([]Event, error) {
		ev := PriceRejected{
			LabelID: l.ID, StoreID: l.StoreID, SKU: l.SKU, RequestedSKU: cmd.SKU,
			Price: cmd.Price, CurrentPrice: l.Price, EffectiveAt: cmd.EffectiveAt,
			PromotionID: cmd.PromotionID, Reason: reason, Detail: detail,
			SourceEventID: cmd.SourceEventID, OccurredAt: now,
		}
		return []Event{ev}, reject(reason, detail)
	}

	// --- identity and assignment ------------------------------------------
	switch l.State {
	case StateUnprovisioned:
		return rejected(ReasonNotAssigned, "label has not been provisioned")
	case StateRetired:
		return rejected(ReasonNotAssigned, "label is retired")
	}
	if l.SKU == "" {
		return rejected(ReasonNotAssigned, "label has no SKU assignment")
	}
	if l.SKU != cmd.SKU {
		return rejected(ReasonSKUMismatch, fmt.Sprintf(
			"label is assigned to %s, price is for %s", l.SKU, cmd.SKU))
	}

	// --- structural validity of the money ---------------------------------
	if !cmd.Price.Valid() {
		return rejected(ReasonInvalidPrice, fmt.Sprintf("currency %q is not an ISO 4217 code", cmd.Price.Currency))
	}
	if cmd.Price.Amount < 0 {
		return rejected(ReasonInvalidPrice, "negative price")
	}
	if l.Currency != "" && !strings.EqualFold(l.Currency, cmd.Price.Currency) {
		return rejected(ReasonCurrencyMismatch, fmt.Sprintf(
			"store trades in %s, price is in %s", l.Currency, cmd.Price.Currency))
	}
	if cmd.WasPrice != nil && cmd.WasPrice.Currency != cmd.Price.Currency {
		return rejected(ReasonCurrencyMismatch, fmt.Sprintf(
			"was-price is in %s, price is in %s", cmd.WasPrice.Currency, cmd.Price.Currency))
	}

	// --- sequencing --------------------------------------------------------
	if cmd.Sequence > 0 && cmd.Sequence <= l.Sequence {
		return nil, fmt.Errorf("%w: label %s is at sequence %d, update carries %d",
			ErrStaleUpdate, l.ID, l.Sequence, cmd.Sequence)
	}
	if cmd.SourceEventID != "" && l.Pending != nil && l.Pending.SourceEventID == cmd.SourceEventID {
		return nil, fmt.Errorf("%w: label %s sequence %d", ErrAlreadyApplied, l.ID, l.Pending.Sequence)
	}
	if l.Displaying() && !l.PriceOccurredAt.IsZero() && occurred.Before(l.PriceOccurredAt) {
		return nil, fmt.Errorf("%w: label %s shows a price from %s, update is from %s",
			ErrOutOfOrder, l.ID, l.PriceOccurredAt.Format(time.RFC3339), occurred.Format(time.RFC3339))
	}
	if l.isNoOp(cmd) {
		return nil, fmt.Errorf("%w: label %s already shows %s for %s",
			ErrStaleUpdate, l.ID, cmd.Price.String(), cmd.SKU)
	}

	// --- temporal validity -------------------------------------------------
	if cmd.EffectiveAt.Before(now.Add(-policy.EffectiveGrace)) {
		return rejected(ReasonEffectiveInPast, fmt.Sprintf(
			"effective_at %s is more than %s in the past",
			cmd.EffectiveAt.UTC().Format(time.RFC3339), policy.EffectiveGrace))
	}
	if cmd.EffectiveAt.After(now.Add(policy.ScheduleHorizon)) {
		return rejected(ReasonScheduleTooFar, fmt.Sprintf(
			"effective_at %s is more than %s ahead",
			cmd.EffectiveAt.UTC().Format(time.RFC3339), policy.ScheduleHorizon))
	}

	// --- guard rail --------------------------------------------------------
	if detail, ok := l.guardrailBreach(cmd.Price, policy); ok {
		return rejected(ReasonGuardrail, detail)
	}

	// --- decision ----------------------------------------------------------
	if cmd.EffectiveAt.After(now) {
		return l.scheduleChange(cmd, now), nil
	}
	return l.applyNow(cmd, policy, now, occurred), nil
}

// isNoOp reports whether the command asks for exactly what is already on the
// glass.
//
// Re-applying it would burn a sequence number and a 1.5-second refresh to
// change nothing, which on a store-wide replay is 40,000 pointless waveforms
// and a visible flash across every aisle.
//
// "Exactly what is on the glass" is SKU, price and promotion — deliberately not
// effective_at. A promotion re-activated at a new instant, or a POS replaying
// the same price with a fresher timestamp, changes no pixel a shopper can see
// and no claim the platform is making, so it is not worth a refresh. The one
// exception is a future-dated command: that is a request to change the shelf
// *later*, and something may well change it in between, so it is always
// scheduled rather than dismissed.
func (l *Label) isNoOp(cmd PriceChange) bool {
	if !l.Displaying() || l.Sequence == 0 {
		return false
	}
	if cmd.EffectiveAt.After(cmd.Now) {
		return false
	}
	if l.SKU != cmd.SKU || l.PromotionID != cmd.PromotionID {
		return false
	}
	return l.Price.Amount == cmd.Price.Amount && l.Price.Currency == cmd.Price.Currency
}

// guardrailBreach reports whether a price movement exceeds the tenant's
// configured factor, and why.
//
// The comparison is a ratio rather than an absolute delta because retail prices
// span four orders of magnitude within one store: an absolute limit that lets a
// television move by £200 would let a pint of milk move by £200 too, which is
// exactly the failure this exists to catch.
func (l *Label) guardrailBreach(next canon.Money, policy Policy) (string, bool) {
	if !l.Displaying() || l.Sequence == 0 {
		return "", false
	}
	current := l.Price
	if current.Currency != next.Currency {
		return "", false
	}
	if current.Amount <= 0 {
		// No meaningful ratio against zero or a credit. A label sitting at zero
		// is being priced for the first time in practice.
		return "", false
	}
	hi, lo := current.Amount, next.Amount
	if lo > hi {
		hi, lo = lo, hi
	}
	if hi < policy.GuardrailFloorMinor {
		return "", false
	}
	if lo <= 0 {
		return fmt.Sprintf("price moves from %s to %s, which exceeds the %.3gx limit",
			current.String(), next.String(), policy.GuardrailFactor), true
	}
	// Integer comparison: hi/lo > factor  ⇔  hi > lo*factor. Done in integers
	// scaled by 1000 so the same arithmetic is reproducible on an edge MCU with
	// no hardware floating point, which matters because the SGU applies the
	// same guard rail while the store is autonomous.
	factorMilli := int64(policy.GuardrailFactor*1000 + 0.5)
	if hi*1000 > lo*factorMilli {
		return fmt.Sprintf("price moves from %s to %s, a %.1fx change against a %.3gx limit",
			current.String(), next.String(), float64(hi)/float64(lo), policy.GuardrailFactor), true
	}
	return "", false
}

func (l *Label) scheduleChange(cmd PriceChange, now time.Time) []Event {
	id := cmd.ScheduleID
	if id == "" {
		id = canon.NewULID()
	}
	var events []Event
	// A newer decision for the same product supersedes anything already
	// scheduled at or after its effective time. Leaving both would let the
	// older one fire afterwards and roll the shelf back to a price nobody
	// currently authorises.
	for _, s := range l.Scheduled {
		if s.SKU == cmd.SKU && !s.EffectiveAt.Before(cmd.EffectiveAt) {
			events = append(events, ScheduleCancelled{
				LabelID: l.ID, ScheduleID: s.ScheduleID,
				Reason: "superseded by " + id, OccurredAt: now,
			})
		}
	}
	events = append(events, PriceScheduled{
		LabelID: l.ID, StoreID: l.StoreID, SKU: cmd.SKU, Price: cmd.Price,
		WasPrice: cmd.WasPrice, UnitPrice: cmd.UnitPrice, UnitMeasure: cmd.UnitMeasure,
		EffectiveAt: cmd.EffectiveAt.UTC(), ExpiresAt: cmd.ExpiresAt,
		PromotionID: cmd.PromotionID, Reason: cmd.Reason, ScheduleID: id,
		InitiatedBy: cmd.InitiatedBy, OccurredAt: now,
	})
	return events
}

func (l *Label) applyNow(cmd PriceChange, policy Policy, now, occurred time.Time) []Event {
	seq := cmd.Sequence
	if seq <= 0 {
		seq = l.Sequence + 1
	}
	render := DecideRender(RenderInput{
		Price: cmd.Price, WasPrice: cmd.WasPrice, UnitPrice: cmd.UnitPrice,
		UnitMeasure: cmd.UnitMeasure, PromotionID: cmd.PromotionID, Reason: cmd.Reason,
		Locale: l.Render.Locale, Attributes: cmd.Attributes,
		Previous: l.previousRender(),
	}, policy)

	var prev *canon.Money
	if l.Displaying() {
		p := l.Price
		prev = &p
	}
	// Merchandising attributes ride on the price change because the POS feed is
	// the only place they enter the platform: the planogram names the product,
	// but only the pricing feed says what kind of product it is. Carrying them
	// forward when a change omits them keeps a label's category from being
	// erased by the next price update that happens not to repeat it.
	category := firstNonEmpty(cmd.Attributes["category"], l.Category)
	brand := firstNonEmpty(cmd.Attributes["brand"], l.Brand)

	return []Event{PriceApplied{
		LabelID: l.ID, StoreID: l.StoreID, SECID: l.SECID, SKU: cmd.SKU,
		Price: cmd.Price, PreviousPrice: prev, WasPrice: cmd.WasPrice,
		UnitPrice: cmd.UnitPrice, UnitMeasure: cmd.UnitMeasure,
		EffectiveAt: cmd.EffectiveAt.UTC(), ExpiresAt: cmd.ExpiresAt,
		PromotionID: cmd.PromotionID, PromotionPriority: cmd.PromotionPriority,
		Category: category, Brand: brand,
		Render: render, Sequence: seq,
		SourceEventID: cmd.SourceEventID, InitiatedBy: cmd.InitiatedBy,
		OccurredAt: occurred,
	}}
}

func (l *Label) previousRender() PreviousRender {
	if !l.Displaying() || l.Sequence == 0 {
		return PreviousRender{}
	}
	return PreviousRender{
		HasPrice: true, Price: l.Price,
		Template: l.Render.Template, Badge: l.Render.Badge,
		LEDColor: l.Render.LEDColor, ShowWas: l.Render.ShowWas,
		PartialsSinceFull: l.PartialsSinceFull,
	}
}

// ---------------------------------------------------------------------------
// Lifecycle commands
// ---------------------------------------------------------------------------

// Provision is the command that brings a label into service at a placement.
type Provision struct {
	// TenantID, StoreID, Region and SECID are the placement.
	TenantID canon.TenantID
	StoreID  canon.StoreID
	Region   canon.Region
	SECID    canon.SECID
	// Currency is the store's trading currency, which becomes the label's.
	Currency string
	// Template and Locale are the display defaults.
	Template string
	Locale   string
	// HardwareTier records the display generation for render capability.
	HardwareTier string
	// Now is the decision instant.
	Now time.Time
}

// Provision registers the label. Provisioning an already-provisioned label is a
// no-op rather than an error, because the Device Registry replays its stream on
// every read-model rebuild and a rebuild must not fail on facts it has already
// seen.
func (l *Label) Provision(cmd Provision) ([]Event, error) {
	switch {
	case cmd.TenantID == "":
		return nil, fmt.Errorf("%w: Provision.TenantID is required", ErrInvalidCommand)
	case cmd.StoreID == "":
		return nil, fmt.Errorf("%w: Provision.StoreID is required", ErrInvalidCommand)
	case cmd.SECID == "":
		return nil, fmt.Errorf("%w: Provision.SECID is required", ErrInvalidCommand)
	case cmd.Currency == "":
		return nil, fmt.Errorf("%w: Provision.Currency is required", ErrInvalidCommand)
	case cmd.Now.IsZero():
		return nil, fmt.Errorf("%w: Provision.Now is required", ErrInvalidCommand)
	}
	if l.Exists() {
		return nil, nil
	}
	template := cmd.Template
	if template == "" {
		template = TemplateStandard
	}
	return []Event{LabelProvisioned{
		LabelID: l.ID, TenantID: cmd.TenantID, StoreID: cmd.StoreID,
		Region: cmd.Region, SECID: cmd.SECID,
		Currency: strings.ToUpper(cmd.Currency), Template: template,
		Locale: cmd.Locale, HardwareTier: cmd.HardwareTier, OccurredAt: cmd.Now.UTC(),
	}}, nil
}

// Assign is the command that places a label against a product.
type Assign struct {
	// SKU is the product. An empty SKU unassigns.
	SKU canon.SKU
	// SECID and StoreID, when set, also move the label to a new controller or
	// store — which is what a planogram reset does.
	SECID   canon.SECID
	StoreID canon.StoreID
	// Template overrides the label's default template.
	Template string
	// Now is the decision instant.
	Now time.Time
}

// Assign places the label against a SKU. Assigning the SKU it already holds
// with no placement change is a no-op.
func (l *Label) Assign(cmd Assign) ([]Event, error) {
	if cmd.Now.IsZero() {
		return nil, fmt.Errorf("%w: Assign.Now is required", ErrInvalidCommand)
	}
	if !l.Exists() {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, l.ID)
	}
	if l.State == StateRetired {
		return nil, fmt.Errorf("%w: %s is retired", ErrInvalidCommand, l.ID)
	}
	sameSEC := cmd.SECID == "" || cmd.SECID == l.SECID
	sameStore := cmd.StoreID == "" || cmd.StoreID == l.StoreID
	if l.SKU == cmd.SKU && sameSEC && sameStore && (cmd.Template == "" || cmd.Template == l.Render.Template) {
		return nil, nil
	}
	return []Event{LabelAssigned{
		LabelID: l.ID, SKU: cmd.SKU, SECID: cmd.SECID, StoreID: cmd.StoreID,
		Template: cmd.Template, OccurredAt: cmd.Now.UTC(),
	}}, nil
}

// ActivateSchedule turns a due scheduled change into a displayed price.
//
// The sequence is allocated here, not when the change was scheduled, so that an
// urgent price change made in the meantime still wins at the label: sequences
// are handed out in the order updates actually reach the glass, which is the
// order the label enforces.
func (l *Label) ActivateSchedule(scheduleID string, policy Policy, now time.Time) ([]Event, error) {
	if now.IsZero() {
		return nil, fmt.Errorf("%w: ActivateSchedule requires now", ErrInvalidCommand)
	}
	s, ok := l.Schedule(scheduleID)
	if !ok {
		return nil, fmt.Errorf("%w: schedule %s on label %s", ErrNotFound, scheduleID, l.ID)
	}
	if s.EffectiveAt.After(now) {
		return nil, fmt.Errorf("%w: schedule %s is not due until %s",
			ErrInvalidCommand, scheduleID, s.EffectiveAt.Format(time.RFC3339))
	}
	if s.ExpiresAt != nil && !s.ExpiresAt.After(now) {
		// The promotion window closed before the runner reached it — a
		// multi-hour outage, or a schedule created for a window that has since
		// passed. Displaying it now would put an expired promotional price on
		// the shelf, so it is cancelled instead.
		return []Event{ScheduleCancelled{
			LabelID: l.ID, ScheduleID: scheduleID,
			Reason: "expired before activation", OccurredAt: now.UTC(),
		}}, nil
	}
	return l.ApplyPriceChange(PriceChange{
		SKU: s.SKU, Price: s.Price, WasPrice: s.WasPrice, UnitPrice: s.UnitPrice,
		UnitMeasure: s.UnitMeasure, EffectiveAt: s.EffectiveAt, ExpiresAt: s.ExpiresAt,
		PromotionID: s.PromotionID, Reason: s.Reason, Attributes: s.Attributes,
		InitiatedBy: s.InitiatedBy, OccurredAt: now, Now: now,
	}, policy)
}

// Confirm is the command carrying an edge acknowledgement.
type Confirm struct {
	// SECID is the controller that reported the confirmation.
	SECID canon.SECID
	// Sequence is the update the label confirmed.
	Sequence int64
	// DeliveredAt is when the pixels settled.
	DeliveredAt time.Time
	// LatencyMS is the edge's own end-to-end measurement. Zero means the edge
	// did not measure and the caller supplies its own.
	LatencyMS int64
	// MeshHops and RefreshMS are the edge's diagnostics.
	MeshHops  int
	RefreshMS int
	// Partial reports whether a partial waveform was used.
	Partial bool
}

// ConfirmDelivery records that a label displayed an update.
//
// A confirmation for a sequence older than the last one confirmed is discarded:
// mesh delivery is at-least-once and duplicate ACKs are routine, and letting an
// old one overwrite the latest would make the SLO report a latency for an
// update that has since been superseded.
func (l *Label) ConfirmDelivery(cmd Confirm) ([]Event, error) {
	if !l.Exists() {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, l.ID)
	}
	if cmd.Sequence <= 0 {
		return nil, fmt.Errorf("%w: Confirm.Sequence must be positive", ErrInvalidCommand)
	}
	if l.LastDelivery != nil && cmd.Sequence <= l.LastDelivery.Sequence {
		return nil, fmt.Errorf("%w: label %s already confirmed sequence %d",
			ErrStaleUpdate, l.ID, l.LastDelivery.Sequence)
	}
	at := cmd.DeliveredAt
	if at.IsZero() {
		at = time.Now()
	}
	return []Event{DeliveryConfirmed{
		LabelID: l.ID, StoreID: l.StoreID, SECID: pick(cmd.SECID, l.SECID),
		Sequence: cmd.Sequence, DeliveredAt: at.UTC(), LatencyMS: cmd.LatencyMS,
		MeshHops: cmd.MeshHops, RefreshMS: cmd.RefreshMS, Partial: cmd.Partial,
		OccurredAt: at.UTC(),
	}}, nil
}

// FailDelivery records that the edge gave up on an update.
func (l *Label) FailDelivery(sec canon.SECID, sequence int64, reason string, attempts int, now time.Time) ([]Event, error) {
	if !l.Exists() {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, l.ID)
	}
	if sequence <= 0 {
		return nil, fmt.Errorf("%w: FailDelivery requires a positive sequence", ErrInvalidCommand)
	}
	if l.LastFailure != nil && l.LastFailure.Sequence == sequence {
		return nil, nil
	}
	return []Event{DeliveryFailed{
		LabelID: l.ID, StoreID: l.StoreID, SECID: pick(sec, l.SECID),
		Sequence: sequence, Reason: reason, Attempts: attempts, OccurredAt: now.UTC(),
	}}, nil
}

// MarkOffline records the label dropping out of the mesh.
func (l *Label) MarkOffline(reason string, now time.Time) ([]Event, error) {
	if !l.Exists() {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, l.ID)
	}
	if l.State == StateOffline || l.State == StateRetired {
		return nil, nil
	}
	return []Event{LabelWentOffline{
		LabelID: l.ID, StoreID: l.StoreID, SECID: l.SECID,
		Reason: reason, OccurredAt: now.UTC(),
	}}, nil
}

// MarkOnline records the label rejoining the mesh.
func (l *Label) MarkOnline(now time.Time) ([]Event, error) {
	if !l.Exists() {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, l.ID)
	}
	if l.State != StateOffline {
		return nil, nil
	}
	return []Event{LabelCameOnline{
		LabelID: l.ID, StoreID: l.StoreID, SECID: l.SECID, OccurredAt: now.UTC(),
	}}, nil
}

// Retire takes a label permanently out of service. Its stream is kept: a
// retired label's price history is exactly what a weights-and-measures audit
// asks for.
func (l *Label) Retire(reason string, now time.Time) ([]Event, error) {
	if !l.Exists() || l.State == StateRetired {
		return nil, nil
	}
	return []Event{LabelRetired{LabelID: l.ID, Reason: reason, OccurredAt: now.UTC()}}, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func pick(a, b canon.SECID) canon.SECID {
	if a != "" {
		return a
	}
	return b
}
