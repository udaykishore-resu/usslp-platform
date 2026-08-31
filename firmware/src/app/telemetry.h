/*
 * telemetry.h - the label's periodic health report.
 *
 * Sent uplink on the label's own cadence rather than polled. Polling 40,000
 * labels would cost a zone's whole channel; letting each label speak once every
 * five minutes costs under one per cent of it. The Shelf Edge Controller batches
 * them per zone before they reach the cloud, which is why
 * INTERFACE-CONTRACTS section 3 says telemetry is batched per controller:
 * forwarding per label would be thirteen million messages a second across the
 * fleet.
 *
 * The frame is labelsim.TelemetryFrame, byte for byte, so a controller written
 * against the simulator parses a real label's report unchanged.
 *
 * What goes in it, and why each field earns its place on a channel this scarce:
 *
 *   battery mV + %       the replacement schedule, and the critical alert
 *   temperature          selects the E-Ink waveform and derates the capacity;
 *                        also the only thermometer in most of these aisles
 *   parent LQI + RSSI    the half of the mesh picture the controller cannot see
 *   refresh count        against the panel's rated cycles
 *   NFC taps             shopper interest, free to count
 *   uptime               distinguishes a label that rebooted from one that did
 *                        not answer
 *   tamper               a shelf edge with no price on it is a compliance
 *                        failure the moment a customer looks at it
 *
 * Jitter matters and is not decoration. Without it every label in a zone reports
 * on the same second and puts a five-minute spike into a channel that is
 * otherwise idle; labelsim.scheduleTelemetry applies 20% jitter for exactly this
 * reason and so does this module.
 */

#ifndef USSLP_TELEMETRY_H
#define USSLP_TELEMETRY_H

#include "../crypto/usslp_attest.h"
#include "../usslp_portable.h"

int usslp_telemetry_init(void);

/* Sends a report immediately, out of cadence. For a technician standing in the
 * aisle who does not want to wait five minutes. */
void usslp_telemetry_report_now(void);

/* Hooks from the rest of the firmware. All of them are cheap counters; the
 * reporting decisions are made here so that no other module has to know the
 * uplink's shape. */
void usslp_telemetry_note_attestation_failure(enum usslp_attest_verdict verdict);
void usslp_telemetry_note_uplink_drop(void);
void usslp_telemetry_note_join(void);

/*
 * The label's observed price-update rate, in thousandths of an update per day.
 *
 * Used by power.c to retune the resting beacon interval from what this label is
 * actually asked to do rather than from the platform's planning figure. A label
 * being repriced ten times more often than the plan assumed has to slow its
 * listening or it will not last; one that is never repriced can afford to listen
 * more often and hit its latency SLO more reliably.
 */
uint32_t usslp_telemetry_updates_per_day_milli(void);

#endif /* USSLP_TELEMETRY_H */
