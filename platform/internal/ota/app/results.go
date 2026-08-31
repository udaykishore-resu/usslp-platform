package app

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/usslp/usslp/platform/internal/ota/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/msgbus"
)

// SubscribeResults wires the controller to the upstream firmware-result topic.
//
// Results arrive at QoS 1 rather than QoS 2. A duplicated result is harmless —
// [Controller.RecordOutcome] is idempotent per device and status — while a lost
// one is a device that stays "dispatched" until the silence window expires and
// is then counted against the cohort's health. Getting that backwards would
// make a healthy rollout look like a failing one.
func (c *Controller) SubscribeResults(ctx context.Context) error {
	if c.cfg.Messenger == nil {
		return nil
	}
	if err := c.cfg.Messenger.Subscribe(ctx, domain.FilterAllOTAResults, msgbus.AtLeastOnce,
		func(ctx context.Context, m msgbus.Message) { c.onResultMessage(ctx, m) }); err != nil {
		return fmt.Errorf("ota: subscribe to firmware results: %w", err)
	}
	return nil
}

// onResultMessage decodes and records one device's firmware result.
func (c *Controller) onResultMessage(ctx context.Context, m msgbus.Message) {
	var env canon.Envelope
	if err := json.Unmarshal(m.Payload, &env); err != nil {
		c.log.Warn("undecodable firmware result envelope", "topic", m.Topic, "error", err)
		return
	}
	var update domain.DeviceUpdate
	if err := env.Decode(&update); err != nil {
		c.log.Warn("undecodable firmware result payload", "topic", m.Topic, "error", err)
		return
	}
	// The topic is authoritative for who is speaking. A device that reported a
	// different identifier in its payload than the topic it published on would
	// otherwise be able to report an outcome on another label's behalf, and the
	// broker's ACL already confines a device to its own topic.
	if scope, sec, label, _, ok := canon.ParseSECLabelTopic(m.Topic); ok {
		update.DeviceID = string(label)
		update.SECID = sec
		update.StoreID = scope.Store
		update.TenantID = scope.Tenant
	}
	if update.DeviceID == "" || update.JobID == "" {
		c.log.Warn("firmware result names no device or no rollout", "topic", m.Topic)
		return
	}
	if update.Status == "" {
		update.Status = domain.StatusSucceeded
	}
	if err := c.RecordOutcome(ctx, update); err != nil {
		c.log.Warn("could not record a firmware result",
			"topic", m.Topic, "job_id", update.JobID, "device_id", update.DeviceID, "error", err)
	}
}
