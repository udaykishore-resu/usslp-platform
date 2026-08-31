/*
 * usslp_budget.h - the label's energy accounting, in integers.
 *
 * This is a port of labelsim.PowerProfile.Project in edge/labelsim/power.go,
 * and the reason the firmware carries it at all is that the label makes power
 * decisions on its own: the adaptive beacon interval, the telemetry cadence and
 * the willingness to accept a colour-panel workload are all things the device
 * decides locally, and it has to decide them against the same arithmetic the
 * platform's projections use. A firmware whose model disagreed with the
 * simulator's would let the fleet planner and the device reach opposite
 * conclusions about the same shelf.
 *
 * All integer. Currents are nanoamps, times are microseconds, charges are
 * nanoamp-hours, capacity is microamp-hours. tests/test_power.c asserts
 * agreement with the Go model on eighteen configurations to within a nanoamp.
 *
 * The arithmetic, worked through, for the platform's planning workload on the
 * 2.9-inch panel at 20 C
 * ----------------------------------------------------------------------------
 * Ten price updates a day, seven eighths of them partial, half an NFC tap, 288
 * telemetry reports (one every five minutes), a 500 mAh LiMnO2 cell.
 *
 *   activity      10 updates + 0.5 taps = 10.5 events/day
 *   fast window   10.5 * 60 s = 630 s/day on the 250 ms interval
 *                 630 / 86400 = 0.73% of the day
 *   beacons       630/0.25 = 2,520 fast windows
 *                 (86400-630)/30 = 2,859 slow windows
 *                 5,379 windows * 8 ms = 43.0 s of receiver-on per day
 *                 6.5 mA * 43.0/86400 = 3.237 uA
 *   refresh       0.875*300 ms + 0.125*1500 ms = 450 ms average
 *                 10 * 0.45 s = 4.5 s/day * 26 mA / 86400 = 1.354 uA
 *   data RX       10 * 40 ms * 12 mA / 86400 = 0.056 uA
 *   TX            298 * 8 ms * 18 mA / 86400 = 0.497 uA
 *   NFC           0.5 * 1.5 s * 8 mA / 86400 = 0.069 uA
 *   deep sleep    0.800 uA
 *   self discharge 500 mAh * 1%/yr / 8760 h = 0.571 uA
 *                 ------
 *   total          6.584 uA  ->  500 mAh / 6.584 uA = 75,940 h = 8.67 years
 *
 * And the number that matters more than any of those: with the beacon interval
 * held at a literal 250 ms all day, with no duty cycling, the beacon term alone
 * is 208 uA, the total is 211 uA, and the cell lasts 0.27 years — ninety-nine
 * days. The blueprint's 250 ms figure and its seven-to-ten-year figure do not
 * fit in the same sentence. Adaptive duty cycling is not a refinement of the
 * design, it is the design; see power/usslp_power.h for the state machine that
 * implements it and README.md for what that means for the latency budget.
 */

#ifndef USSLP_BUDGET_H
#define USSLP_BUDGET_H

#include "../display/usslp_render_policy.h"
#include "../usslp_portable.h"

/* The per-state currents from the USSLP hardware blueprint, and the cell.
 * Held as configuration rather than as constants because a deployment with a
 * different cell or a relay build with a different radio duty cycle has to be
 * modelled without editing code. */
struct usslp_power_profile {
	uint32_t deep_sleep_na;
	uint32_t beacon_rx_na;
	uint32_t beacon_rx_duration_us;
	uint32_t beacon_fast_us;
	uint32_t beacon_slow_us;
	/* How long the label stays on the fast interval after any activity. This
	 * single field is what reconciles the latency budget with the battery
	 * budget. */
	uint32_t active_window_us;
	uint32_t data_rx_na;
	uint32_t tx_na;
	uint32_t nfc_na;
	uint32_t nfc_tap_duration_us;
	uint32_t capacity_uah;
	/* LiMnO2 leaks about 1% a year. Over a decade that is a tenth of the
	 * budget, which cannot be ignored by a projection trying to distinguish
	 * seven years from ten. */
	uint32_t self_discharge_ppm_per_year;
};

/* labelsim.DefaultPower: the blueprint's currents with the duty cycling that
 * makes them survivable. */
void usslp_power_profile_default(struct usslp_power_profile *p);
/* labelsim.AlwaysFastPower: the blueprint read literally. Not a shippable
 * configuration; it exists so the difference is measurable rather than
 * asserted. */
void usslp_power_profile_always_fast(struct usslp_power_profile *p);

/* What a label is asked to do in a day. Fractional quantities are in
 * thousandths so that "half an NFC tap a day" is expressible without floats. */
struct usslp_workload {
	uint32_t updates_per_day_milli;
	uint32_t partial_fraction_ppm;
	uint32_t nfc_taps_per_day_milli;
	uint32_t telemetry_per_day_milli;
	int32_t ambient_deci_c;
};

/* labelsim.DefaultWorkload: the platform's planning workload, the one the
 * seven-to-ten-year claim is made against. */
void usslp_workload_default(struct usslp_workload *w);

/* The projection, broken down by what spends the charge. The breakdown is the
 * point: an aggregate "eight years" tells an engineer nothing, while "half of it
 * goes on listening for beacons" tells them which knob matters. */
struct usslp_projection {
	uint32_t sleep_na;
	uint32_t beacon_na;
	uint32_t data_rx_na;
	uint32_t refresh_na;
	uint32_t tx_na;
	uint32_t nfc_na;
	uint32_t self_discharge_na;
	uint32_t total_na;
	uint32_t usable_capacity_uah;
	/* Projected life in thousandths of a year. */
	int32_t life_milliyears;
	/* Share of wall time on the fast beacon interval, in parts per million. */
	uint32_t fast_fraction_ppm;
	bool meets_target;
};

/*
 * labelsim.CapacityDerating: the fraction of a LiMnO2 cell's rated capacity
 * available at a temperature, in parts per million.
 *
 * Primary lithium holds up far better in the cold than alkaline but is not
 * immune: internal resistance rises and the pulse the E-Ink charge pump demands
 * becomes harder to deliver. A freezer-case label gets about seventy per cent of
 * its rating, and a fleet plan that ignores that finds its freezer aisle dying
 * three years early.
 */
uint32_t usslp_capacity_derating_ppm(int32_t ambient_deci_c);

/* Computes the projection. */
void usslp_project(const struct usslp_power_profile *p, enum usslp_display_tier tier,
		   const struct usslp_workload *w, struct usslp_projection *out);

/*
 * The inverse question, and the useful one: given everything else in the budget,
 * how often can this label afford to listen?
 *
 * Returns the slowest-case listen interval in microseconds at which the label
 * just reaches target_milliyears, or 0 when the rest of the budget already
 * exceeds the target and no beacon interval can save it. This is what the power
 * state machine calls when the fleet planner changes a label's target life.
 */
uint32_t usslp_sustainable_beacon_us(const struct usslp_power_profile *p,
				     enum usslp_display_tier tier, const struct usslp_workload *w,
				     uint32_t target_milliyears);

/*
 * labelsim.batteryMillivolts: the LiMnO2 discharge curve.
 *
 * Flat by design — that is the point of the chemistry — and then it falls off a
 * cliff. Modelling the cliff matters because device.battery.critical is raised
 * off the voltage, not off a charge counter the label does not keep, and an
 * alert at the wrong point either floods the operator or arrives after the label
 * is already blank.
 *
 * depth_of_discharge_ppm is 0 for a fresh cell, 1,000,000 for an exhausted one.
 */
uint16_t usslp_battery_mv(uint32_t depth_of_discharge_ppm);

#endif /* USSLP_BUDGET_H */
