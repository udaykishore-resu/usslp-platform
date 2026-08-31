package sgu

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/msgbus"
)

// ---------------------------------------------------------------------------
// Autonomous operation and reconciliation
// ---------------------------------------------------------------------------

// onModeChange is the detector's callback. It records the transition, announces
// it, and — on recovery — runs the reconciliation.
func (g *Gateway) onModeChange(mode Mode, reason string) {
	g.mu.Lock()
	prev := g.mode
	g.mode = mode
	now := g.cfg.Now().UTC()
	if mode == ModeAutonomous {
		g.autonomousAt = now
		// The divergence point is the last moment the two sides agreed. Anything
		// stamped after it happened while they could not see each other, and is
		// what reconciliation has to reason about.
		g.divergedAt = g.clock.Now()
		// Collection is armed for the whole of the outage, not just at recovery.
		// The MQTT client reconnects and restores its subscriptions on its own
		// backoff, so the cloud's retained state can arrive milliseconds after
		// the link returns and several seconds before the detector's hysteresis
		// admits the store is back. Anything downstream that arrives while this
		// store still considers itself autonomous is, by definition, the cloud's
		// view arriving across a link that has just come back — which is exactly
		// what the merge needs and exactly what must not be applied blindly over
		// the top of what the store decided while it was alone.
		g.collecting = true
		g.pendingCloud = map[string]Register{}
		g.pendingMsg = map[string]pendingPrice{}
	}
	g.mu.Unlock()
	if prev == mode {
		return
	}
	if g.mMode != nil {
		v := 0.0
		if mode == ModeAutonomous {
			v = 1
		}
		g.mMode.With(string(g.cfg.StoreID)).Set(v)
	}

	switch mode {
	case ModeAutonomous:
		g.cfg.Log.Warn("store is autonomous: serving from local state, buffering upstream",
			"store", g.cfg.StoreID, "reason", reason)
		g.announceMode(canon.EvtStoreWentAutonomous, "autonomous", reason, 0, 0, 0)
	case ModeConnected:
		g.cfg.Log.Info("cloud link restored: reconciling", "store", g.cfg.StoreID, "reason", reason)
		go g.Reconcile(context.Background(), reason)
	}
}

// announceMode publishes a canon.StoreModeChanged on the store mode topic.
//
// It goes through the local broker rather than straight to the cloud, so that
// the transition into autonomy is buffered by the same mechanism as everything
// else and arrives, in order, as the first thing the cloud hears after the
// outage.
func (g *Gateway) announceMode(eventType, mode, reason string, queued, conflicts int, outage time.Duration) {
	payload := canon.StoreModeChanged{
		StoreID: g.cfg.StoreID, SGUID: g.cfg.SGUID, Mode: mode, Reason: reason,
		At: g.cfg.Now().UTC(), QueuedUpdates: queued, Conflicts: conflicts,
		OutageSeconds: int64(outage.Seconds()),
	}
	env, err := canon.NewEnvelope(eventType, "store", string(g.cfg.StoreID), g.cfg.Scope.Tenant, payload)
	if err != nil {
		return
	}
	env.StoreID = g.cfg.StoreID
	env.Region = g.cfg.Scope.Region
	env.Source = "sgu/" + string(g.cfg.SGUID)
	env.IdempotencyKey = g.clock.Now().String()
	g.publishUpstream(context.Background(), g.cfg.Scope.StoreTopic(canon.LeafMode), env,
		ClassCritical, canon.QoSPrice, true)
}

// ---------------------------------------------------------------------------
// Scheduled promotions on local time
// ---------------------------------------------------------------------------

// ActivateDue publishes every promotion whose activation time has arrived.
//
// It runs whether or not the store is connected, because a promotion the cloud
// already authorised is the store's to activate: the updates are attested, the
// controllers will verify them, and waiting for a cloud that may be hours away
// would mean a national campaign starting late in four hundred stores.
func (g *Gateway) ActivateDue(ctx context.Context) int {
	now := g.cfg.Now()
	due := g.schedule.Due(now)
	skew := g.clock.Skew()

	for _, p := range due {
		published := 0
		for _, upd := range p.Updates {
			if v := g.evaluateRules(upd); !v.Allowed {
				// Even a cloud-authorised promotion is checked: a promotional
				// price that breaches a statutory floor is still a breach, and
				// the store is the last place it can be stopped.
				g.mu.Lock()
				g.stats.RejectedByRules++
				g.mu.Unlock()
				g.cfg.Log.Error("refusing a scheduled promotional price that breaks a guard rail",
					"store", g.cfg.StoreID, "promotion", p.PromotionID,
					"label", upd.LabelID, "violations", v.Error())
				continue
			}
			sec := secOfLabel(g.replica, upd.LabelID)
			upd.StoreID = g.cfg.StoreID
			env, err := p.Envelope.Caused(canon.EvtPriceUpdated, "label", string(upd.LabelID), upd)
			if err != nil {
				continue
			}
			env.StoreID = g.cfg.StoreID
			env.Region = g.cfg.Scope.Region
			env.Source = "sgu/" + string(g.cfg.SGUID) + "/schedule"
			env.RecordedAt = now.UTC()
			body, err := json.Marshal(env)
			if err != nil {
				continue
			}
			if err := g.broker.Publish(msgbus.Message{
				Topic:   g.cfg.Scope.SECLabelTopic(sec, upd.LabelID, canon.LeafPrice),
				Payload: body, QoS: canon.QoSPrice, Retain: true,
			}); err != nil {
				g.cfg.Log.Error("could not publish a promotional price",
					"store", g.cfg.StoreID, "label", upd.LabelID, "error", err)
				continue
			}
			ts := g.clock.Now()
			if err := g.replica.PutLabel(LabelState{
				LabelID: upd.LabelID, SECID: sec, SKU: upd.SKU, Price: upd.Price,
				Sequence: upd.Sequence, PromotionID: p.PromotionID, Render: upd.Render,
				Attestation: upd.Attestation, EffectiveAt: upd.EffectiveAt,
				TS: ts, Origin: OriginLocal, UpdatedAt: now.UTC(),
			}); err != nil {
				g.cfg.Log.Error("could not replicate a promotional price", "label", upd.LabelID, "error", err)
			}
			published++
		}
		if err := g.schedule.MarkActivated(p.PromotionID, now, skew.Last); err != nil {
			g.cfg.Log.Error("could not record a promotion activation", "promotion", p.PromotionID, "error", err)
		}
		g.mu.Lock()
		g.stats.Activations++
		mode := g.mode
		g.mu.Unlock()
		g.cfg.Log.Info("activated a scheduled promotion on local time",
			"store", g.cfg.StoreID, "promotion", p.PromotionID, "labels", published,
			"mode", string(mode), "clock_skew", skew.Last.String(),
			"scheduled_for", p.ActivateAt, "activated_at", now.UTC())

		env, err := p.Envelope.Caused(canon.EvtPromotionActivated, "promotion", string(p.PromotionID),
			map[string]any{
				"promotion_id": p.PromotionID, "store_id": g.cfg.StoreID,
				"labels": published, "activated_at": now.UTC(),
				"scheduled_for": p.ActivateAt, "mode": string(mode),
				// The skew is carried because activation happened on this store's
				// clock, and how far that clock had drifted is the difference
				// between "on time" and "four minutes early".
				"clock_skew_ms": skew.Last.Milliseconds(),
			})
		if err == nil {
			env.StoreID = g.cfg.StoreID
			env.Source = "sgu/" + string(g.cfg.SGUID) + "/schedule"
			env.IdempotencyKey = g.clock.Now().String()
			g.publishUpstream(ctx, g.cfg.Scope.StoreTopic(canon.LeafPromotion), env,
				ClassCritical, canon.QoSPrice, false)
		}
	}

	if missed := g.schedule.Missed(now); len(missed) > 0 {
		g.mu.Lock()
		g.stats.MissedPromos = uint64(len(missed))
		g.mu.Unlock()
	}
	return len(due)
}

// ---------------------------------------------------------------------------
// Locally originated prices
// ---------------------------------------------------------------------------

// LocalPriceChange applies a price change from the store's own point of sale.
//
// This is the path a store manager uses during an outage, and it is the one
// place the platform's cryptographic guarantee has to be handled explicitly
// rather than inherited. A label refuses any price it cannot verify, and the
// gateway holds no signing key unless one has been delegated to it. So:
//
//   - With a delegated authority, the gateway attests the price with its
//     store-scoped key, whose public half is in every local controller's key
//     ring, and the change reaches the glass. The delegation is what buys the
//     store its autonomy, and it is deliberately narrow: one store, a short
//     validity, revocable from the cloud without touching a single label.
//   - Without one, the change is recorded and reported upstream but is not
//     displayed, and the caller is told so. That is the correct failure. The
//     alternative — displaying an unattested price — would trade the entire
//     weights-and-measures guarantee for the convenience of one till.
func (g *Gateway) LocalPriceChange(ctx context.Context, req canon.PriceChangeRequested) ([]canon.LabelID, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	labels := g.replica.LabelsForSKU(req.SKU)
	if len(labels) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrUnknownSKU, req.SKU)
	}
	if v := g.evaluateRules(canon.PriceUpdated{SKU: req.SKU, Price: req.Price}); !v.Allowed {
		g.mu.Lock()
		g.stats.RejectedByRules++
		g.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrRuleViolation, v.Error())
	}
	if g.cfg.LocalAuthority == nil {
		return nil, ErrNoLocalAuthority
	}

	now := g.cfg.Now().UTC()
	updated := make([]canon.LabelID, 0, len(labels))
	for _, id := range labels {
		prev, _ := g.replica.Label(id)
		seq, err := g.replica.NextSequence(id)
		if err != nil {
			return updated, err
		}
		upd := canon.PriceUpdated{
			LabelID: id, SKU: req.SKU, StoreID: g.cfg.StoreID, Price: req.Price,
			WasPrice: req.WasPrice, UnitPrice: req.UnitPrice, UnitMeasure: req.UnitMeasure,
			EffectiveAt: req.EffectiveAt, PromotionID: req.PromotionID,
			Render: prev.Render, Sequence: seq,
		}
		if upd.EffectiveAt.IsZero() {
			upd.EffectiveAt = now
		}
		if upd.Render.Template == "" {
			upd.Render.Template = "standard"
		}
		att, err := g.cfg.LocalAuthority.Sign(canon.AttestationInputFrom(g.cfg.Scope.Tenant, upd))
		if err != nil {
			return updated, fmt.Errorf("sgu: attesting a local price for %s: %w", id, err)
		}
		upd.Attestation = att

		env, err := canon.NewEnvelope(canon.EvtPriceUpdated, "label", string(id), g.cfg.Scope.Tenant, upd)
		if err != nil {
			return updated, err
		}
		env.StoreID = g.cfg.StoreID
		env.Region = g.cfg.Scope.Region
		env.Source = "sgu/" + string(g.cfg.SGUID) + "/local-pos"
		env.RecordedAt = now
		env.OccurredAt = now
		ts := g.clock.Now()
		env.IdempotencyKey = ts.String()
		body, err := json.Marshal(env)
		if err != nil {
			return updated, err
		}
		if err := g.broker.Publish(msgbus.Message{
			Topic:   g.cfg.Scope.SECLabelTopic(prev.SECID, id, canon.LeafPrice),
			Payload: body, QoS: canon.QoSPrice, Retain: true,
		}); err != nil {
			return updated, fmt.Errorf("sgu: publishing a local price for %s: %w", id, err)
		}
		if err := g.replica.PutLabel(LabelState{
			LabelID: id, SECID: prev.SECID, SKU: req.SKU, Price: req.Price,
			Sequence: seq, PromotionID: req.PromotionID, Render: upd.Render,
			Attestation: att, EffectiveAt: upd.EffectiveAt,
			TS: ts, Origin: OriginLocal, UpdatedAt: now,
		}); err != nil {
			return updated, err
		}
		// Report the local origination upstream so the cloud learns what this
		// store did while it was on its own, in order, when the link returns.
		posEnv, err := env.Caused(canon.EvtPriceChangeRequested, "label", string(id), req)
		if err == nil {
			posEnv.StoreID = g.cfg.StoreID
			posEnv.Source = env.Source
			posEnv.IdempotencyKey = ts.String()
			g.publishUpstream(ctx, g.cfg.Scope.SECLabelTopic(prev.SECID, id, canon.LeafACK),
				posEnv, ClassCritical, canon.QoSPrice, false)
		}
		updated = append(updated, id)
	}
	g.mu.Lock()
	g.stats.LocalPrices++
	g.mu.Unlock()
	g.cfg.Log.Info("applied a local point-of-sale price change",
		"store", g.cfg.StoreID, "sku", req.SKU, "price", req.Price.String(), "labels", len(updated))
	return updated, nil
}

// SetInventory records a local stock count, which is the store's own to own.
func (g *Gateway) SetInventory(sku canon.SKU, onHand int64) error {
	return g.replica.PutInventory(InventoryState{
		SKU: sku, OnHand: onHand, TS: g.clock.Now(),
		Origin: OriginLocal, AsOf: g.cfg.Now().UTC(),
	})
}

// ---------------------------------------------------------------------------
// Reconciliation
// ---------------------------------------------------------------------------

// pendingPrice is a cloud price held over the settle window, with everything
// needed to publish it if it wins the merge.
type pendingPrice struct {
	route Route
	env   canon.Envelope
	upd   canon.PriceUpdated
	msg   msgbus.Message
}

// collectCloudPrice records a cloud price arriving during the settle window.
func (g *Gateway) collectCloudPrice(r Route, env canon.Envelope, upd canon.PriceUpdated, m msgbus.Message) {
	body, err := json.Marshal(upd.Price)
	if err != nil {
		return
	}
	key := PriceKey(upd.LabelID)
	g.mu.Lock()
	g.pendingCloud[key] = Register{
		Key: key, Kind: KindPricing, Value: body,
		TS: envelopeHLC(env), Origin: OriginCloud, WrittenAt: env.RecordedAt,
	}
	g.pendingMsg[key] = pendingPrice{route: r, env: env, upd: upd,
		msg: msgbus.Message{Topic: m.Topic, Payload: append([]byte(nil), m.Payload...)}}
	g.mu.Unlock()
}

// Reconcile rejoins the cloud: merge divergent state, then flush the buffer in
// order, then announce.
//
// The order is not arbitrary. Merging first means the buffer is flushed against
// a store whose state already agrees with the cloud's, so the acknowledgements
// and rejections in it describe a world the cloud can make sense of. Flushing
// first would mean replaying, in order, a series of events about prices the
// merge is about to overturn.
func (g *Gateway) Reconcile(ctx context.Context, reason string) ReconciliationReport {
	g.mu.Lock()
	if g.reconcileRunning {
		g.mu.Unlock()
		return ReconciliationReport{}
	}
	g.reconcileRunning = true
	g.collecting = true
	diverged := g.divergedAt
	outage := time.Duration(0)
	if !g.autonomousAt.IsZero() {
		outage = g.cfg.Now().Sub(g.autonomousAt)
	}
	g.mu.Unlock()

	report := ReconciliationReport{
		StoreID: string(g.cfg.StoreID), SGUID: string(g.cfg.SGUID),
		StartedAt: g.cfg.Now().UTC(), OutageSeconds: int64(outage.Seconds()),
		DivergedAt: diverged.String(),
	}

	// The cloud republishes its retained state on our re-subscription. Waiting
	// for it is what gives the merge something to merge against; there is no
	// query interface, and inventing one would put a synchronous dependency in
	// the middle of the recovery path.
	select {
	case <-time.After(g.cfg.ReconcileSettle):
	case <-ctx.Done():
	case <-g.stopCh:
	}

	g.mu.Lock()
	cloudState := g.pendingCloud
	cloudMsgs := g.pendingMsg
	g.pendingCloud = map[string]Register{}
	g.pendingMsg = map[string]pendingPrice{}
	// Collection stops here: from now on a downstream price is a new
	// instruction again and takes the ordinary path.
	g.collecting = false
	g.mu.Unlock()

	local := g.replica.Registers()
	keys := map[string]struct{}{}
	for k := range local {
		keys[k] = struct{}{}
	}
	for k := range cloudState {
		keys[k] = struct{}{}
	}

	for key := range keys {
		report.KeysCompared++
		res := Merge(local[key], cloudState[key], diverged)
		if res.Conflict {
			report.Conflicts++
			rec := ConflictRecord{
				Key: key, Kind: res.Winner.Kind, Resolution: res.Resolution,
				WinnerTS: res.Winner.TS.String(), WinnerFrom: res.Winner.Origin,
			}
			if res.Loser != nil {
				rec.LoserTS = res.Loser.TS.String()
				rec.Discarded = res.Loser.Value
			}
			report.ConflictDetail = append(report.ConflictDetail, rec)
			if g.mConflicts != nil {
				g.mConflicts.With(string(g.cfg.StoreID), string(res.Resolution)).Inc()
			}
		}
		if applied := g.applyMerged(ctx, key, res, cloudMsgs[key]); applied {
			report.KeysChanged++
		}
	}
	sortConflicts(report.ConflictDetail)

	flushed, deduped, failed := g.flush(ctx)
	report.Flushed = flushed
	report.Deduplicated = deduped
	report.FlushFailed = failed
	qs := g.queue.Stats()
	report.Dropped = int(qs.DroppedBulk + qs.DroppedOther)
	report.Lossy = qs.Lossy
	report.ClockSkew = g.clock.Skew()
	report.FinishedAt = g.cfg.Now().UTC()

	g.mu.Lock()
	g.reconcileRunning = false
	g.stats.Reconciliations++
	g.stats.Conflicts += uint64(report.Conflicts)
	g.lastReport = &report
	g.mu.Unlock()
	g.queue.ClearLossy()

	g.cfg.Log.Info("reconciled", "store", g.cfg.StoreID, "summary", report.Summary())
	g.announceMode(canon.EvtStoreReconciled, "connected", reason, flushed, report.Conflicts, outage)
	return report
}

// applyMerged writes a merge winner back into the replica, reporting whether it
// changed anything.
func (g *Gateway) applyMerged(ctx context.Context, key string, res MergeResult, held pendingPrice) bool {
	if !res.Winner.Exists() {
		return false
	}
	switch res.Winner.Kind {
	case KindPricing:
		id := canon.LabelID(key[len("price/"):])
		st, ok := g.replica.Label(id)
		if !ok {
			return false
		}
		var price canon.Money
		if err := json.Unmarshal(res.Winner.Value, &price); err != nil {
			return false
		}
		if price == st.Price && res.Winner.Origin == st.Origin {
			return false
		}
		if res.Winner.Origin == OriginCloud && held.upd.LabelID != "" {
			// The cloud won, so the shelf is currently showing something the
			// platform no longer considers current. Replaying the cloud's own
			// attested update is the only way to correct it: the gateway cannot
			// re-sign someone else's price, and a label will not display one it
			// cannot verify. This is also what updates the replica, through the
			// ordinary path, so there is one code path for a price reaching a
			// controller rather than two that can drift apart.
			g.applyCloudPrice(ctx, held.route, held.env, held.upd, held.msg)
			return true
		}
		st.Price = price
		st.TS = res.Winner.TS
		st.Origin = res.Winner.Origin
		st.UpdatedAt = g.cfg.Now().UTC()
		if err := g.replica.PutLabel(st); err != nil {
			g.cfg.Log.Error("could not write a merged price", "label", id, "error", err)
			return false
		}
		return true
	case KindInventory:
		sku := canon.SKU(key[len("inventory/"):])
		var onHand int64
		if err := json.Unmarshal(res.Winner.Value, &onHand); err != nil {
			return false
		}
		cur, ok := g.replica.Inventory(sku)
		if ok && cur.OnHand == onHand && cur.Origin == res.Winner.Origin {
			return false
		}
		if err := g.replica.PutInventory(InventoryState{
			SKU: sku, OnHand: onHand, TS: res.Winner.TS,
			Origin: res.Winner.Origin, AsOf: g.cfg.Now().UTC(),
		}); err != nil {
			return false
		}
		return true
	}
	return false
}

// flush drains the upstream buffer to the cloud, in order, skipping anything
// the cloud already has.
//
// In order because the cloud's consumers key on (store, SKU) and rely on that
// ordering; deduplicated because the one duplicate this design can actually
// produce is the message that was published successfully and then not removed
// from the queue before the process died, and the durable sent-index is exactly
// the record of which those are.
func (g *Gateway) flush(ctx context.Context) (flushed, deduped, failed int) {
	const batch = 128
	for {
		select {
		case <-ctx.Done():
			return
		case <-g.stopCh:
			return
		default:
		}
		entries, err := g.queue.Peek(batch)
		if err != nil {
			g.cfg.Log.Error("could not read the upstream buffer", "store", g.cfg.StoreID, "error", err)
			return
		}
		if len(entries) == 0 {
			return
		}
		g.mu.Lock()
		c := g.cloud
		g.mu.Unlock()
		if c == nil || !c.Connected() {
			// The link went away mid-flush. Everything still queued stays
			// queued, in order, and the next recovery picks up where this left
			// off — which is precisely why the queue is durable and ordered.
			g.cfg.Log.Warn("cloud link lost during flush; the remainder stays buffered",
				"store", g.cfg.StoreID, "remaining", g.queue.Depth())
			return
		}
		progressed := false
		for _, e := range entries {
			if e.IdempotencyKey != "" && g.queue.AlreadySent(e.IdempotencyKey) {
				g.queue.NoteDeduplicated()
				_ = g.queue.Remove(e.Seq)
				deduped++
				progressed = true
				continue
			}
			if err := c.Publish(ctx, e.Message()); err != nil {
				failed++
				g.cfg.Log.Warn("could not flush a buffered message",
					"store", g.cfg.StoreID, "topic", e.Topic, "error", err)
				return
			}
			if e.IdempotencyKey != "" {
				_ = g.queue.MarkSent(e.IdempotencyKey)
			}
			_ = g.queue.Remove(e.Seq)
			flushed++
			progressed = true
		}
		if !progressed {
			return
		}
	}
}

// LastReconciliation returns the most recent reconciliation report.
func (g *Gateway) LastReconciliation() (ReconciliationReport, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lastReport == nil {
		return ReconciliationReport{}, false
	}
	return *g.lastReport, true
}

// AutonomousSince returns when the store went autonomous, if it is.
func (g *Gateway) AutonomousSince() (time.Time, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.mode != ModeAutonomous {
		return time.Time{}, false
	}
	return g.autonomousAt, true
}
