#include "usslp_budget.h"

#include <string.h>

#define DAY_US 86400000000ll
#define DAY_NS 86400000000000ll
#define HOURS_PER_YEAR 8760ll /* 365*24, matching labelsim */

void usslp_power_profile_default(struct usslp_power_profile *p)
{
	memset(p, 0, sizeof(*p));
	p->deep_sleep_na = 800;              /* 0.8 uA */
	p->beacon_rx_na = 6500000;           /* 6.5 mA */
	p->beacon_rx_duration_us = 8000;     /* 8 ms */
	p->beacon_fast_us = 250000;          /* 250 ms */
	p->beacon_slow_us = 30000000;        /* 30 s */
	p->active_window_us = 60000000;      /* 60 s */
	p->data_rx_na = 12000000;            /* 12 mA */
	p->tx_na = 18000000;                 /* 18 mA */
	p->nfc_na = 8000000;                 /* 8 mA */
	p->nfc_tap_duration_us = 1500000;    /* 1.5 s */
	p->capacity_uah = 500000;            /* 500 mAh */
	p->self_discharge_ppm_per_year = 10000; /* 1% */
}

void usslp_power_profile_always_fast(struct usslp_power_profile *p)
{
	usslp_power_profile_default(p);
	p->beacon_slow_us = p->beacon_fast_us;
	p->active_window_us = 0;
}

void usslp_workload_default(struct usslp_workload *w)
{
	memset(w, 0, sizeof(*w));
	w->updates_per_day_milli = 10000;   /* 10 a day */
	w->partial_fraction_ppm = 875000;   /* seven partials then a forced full */
	w->nfc_taps_per_day_milli = 500;    /* one tap every other day */
	w->telemetry_per_day_milli = 288000; /* one every five minutes */
	w->ambient_deci_c = 200;            /* 20.0 C */
}

uint32_t usslp_capacity_derating_ppm(int32_t ambient_deci_c)
{
	if (ambient_deci_c >= 200) {
		return 1000000u;
	}
	if (ambient_deci_c >= 0) {
		/* 1.00 at 20 C, 0.85 at 0 C: 750 ppm per tenth of a degree. */
		return (uint32_t)(1000000 - 750 * (200 - ambient_deci_c));
	}
	if (ambient_deci_c >= -200) {
		/* 0.85 at 0 C, 0.70 at -20 C. */
		return (uint32_t)(850000 + 750 * ambient_deci_c);
	}
	return 700000u;
}

/* Average current in nanoamps for a device drawing `current_na` for
 * `on_time_ns` out of every day. */
static uint32_t duty_na(uint64_t current_na, int64_t on_time_ns)
{
	int64_t on_us, v;

	if (on_time_ns <= 0) {
		return 0;
	}
	/* Reduced to microseconds before the multiply. A label pinned to a 250 ms
	 * beacon spends 2,765 seconds a day with its receiver on, and 6.5 mA in
	 * nanoamps times that in nanoseconds overflows int64 — which is exactly the
	 * configuration this model exists to expose, so it must not be the one that
	 * silently wraps. The sub-microsecond remainder is at most one part in 10^7
	 * of any term. */
	on_us = usslp_round_div(on_time_ns, 1000ll);
	v = usslp_round_div((int64_t)current_na * on_us, DAY_US);
	if (v < 0) {
		return 0;
	}
	if (v > (int64_t)UINT32_MAX) {
		return UINT32_MAX;
	}
	return (uint32_t)v;
}

/* Receiver-on nanoseconds per day spent listening for beacons. Counted
 * arithmetically rather than simulated: over a decade a label wakes something
 * like ten million times, and nothing happens in a window where no frame
 * arrives. */
static int64_t beacon_on_ns_per_day(const struct usslp_power_profile *p, int64_t fast_span_us)
{
	int64_t slow_span_us = DAY_US - fast_span_us;
	int64_t on_ns = 0;

	if (p->beacon_fast_us > 0 && fast_span_us > 0) {
		/* windows = span / interval; multiply by the window duration first so
		 * the division is taken once, at the end, on a value big enough that
		 * truncation is below a nanosecond. */
		on_ns += usslp_round_div(fast_span_us * 1000ll * (int64_t)p->beacon_rx_duration_us,
					 (int64_t)p->beacon_fast_us);
	}
	if (p->beacon_slow_us > 0 && slow_span_us > 0) {
		on_ns += usslp_round_div(slow_span_us * 1000ll * (int64_t)p->beacon_rx_duration_us,
					 (int64_t)p->beacon_slow_us);
	}
	return on_ns;
}

/* Receiver-on time for one price update, and transmit time for one uplink.
 * A price update arrives as a compressed image — a few hundred bytes, which at
 * 250 kbps with MAC acknowledgements is tens of milliseconds of receiver-on
 * time. Both figures are labelsim's. */
#define UPDATE_RX_US 40000
#define UPLINK_TX_US 8000

void usslp_project(const struct usslp_power_profile *p, enum usslp_display_tier tier,
		   const struct usslp_workload *w, struct usslp_projection *out)
{
	const struct usslp_panel_spec *d = usslp_panel(tier);
	uint32_t partial_ppm = w->partial_fraction_ppm;
	uint32_t cap_ppm;
	int64_t events_milli, fast_span_us;
	int64_t avg_refresh_us, on_ns;
	uint64_t usable_uah;
	int64_t total_na;

	memset(out, 0, sizeof(*out));

	/* Ghosting caps how many updates can really be partial. Believing a caller
	 * who claims every update is partial is how a model produces a battery life
	 * the hardware cannot deliver. */
	if (!d->supports_partial) {
		partial_ppm = 0;
	} else {
		uint32_t max_ppm =
			(uint32_t)((uint64_t)d->max_partials * 1000000u / (d->max_partials + 1u));
		if (partial_ppm > max_ppm) {
			partial_ppm = max_ppm;
		}
	}

	/* Activity keeps the label on the fast interval for a window after each
	 * event. Overlapping windows are not double counted: the fraction is clamped
	 * at one, which is what happens to a label being hammered. */
	events_milli = (int64_t)w->updates_per_day_milli + (int64_t)w->nfc_taps_per_day_milli;
	fast_span_us = usslp_round_div(events_milli * (int64_t)p->active_window_us, 1000ll);
	if (fast_span_us > DAY_US) {
		fast_span_us = DAY_US;
	}
	if (p->beacon_slow_us <= p->beacon_fast_us) {
		fast_span_us = DAY_US;
	}
	out->fast_fraction_ppm = (uint32_t)usslp_round_div(fast_span_us * 1000000ll, DAY_US);

	out->sleep_na = p->deep_sleep_na;
	out->beacon_na = duty_na(p->beacon_rx_na, beacon_on_ns_per_day(p, fast_span_us));

	on_ns = (int64_t)w->updates_per_day_milli * (int64_t)UPDATE_RX_US;
	out->data_rx_na = duty_na(p->data_rx_na, on_ns);

	avg_refresh_us = usslp_round_div(
		(int64_t)partial_ppm * (int64_t)d->partial_refresh_ms * 1000ll +
			(int64_t)(1000000u - partial_ppm) * (int64_t)d->full_refresh_ms * 1000ll,
		1000000ll);
	on_ns = (int64_t)w->updates_per_day_milli * avg_refresh_us;
	out->refresh_na = duty_na(d->refresh_current_ua * 1000ull, on_ns);

	on_ns = ((int64_t)w->updates_per_day_milli + (int64_t)w->telemetry_per_day_milli) *
		(int64_t)UPLINK_TX_US;
	out->tx_na = duty_na(p->tx_na, on_ns);

	on_ns = (int64_t)w->nfc_taps_per_day_milli * (int64_t)p->nfc_tap_duration_us;
	out->nfc_na = duty_na(p->nfc_na, on_ns);

	cap_ppm = usslp_capacity_derating_ppm(w->ambient_deci_c);
	usable_uah = (uint64_t)p->capacity_uah * cap_ppm / 1000000ull;
	out->usable_capacity_uah = (uint32_t)usable_uah;

	/* usable [uAh] * ppm/1e6 per year, spread over 8760 hours, expressed in
	 * nanoamps: uAh * 1000 gives nAh, and nAh per hour is nA. */
	out->self_discharge_na = (uint32_t)usslp_round_div(
		(int64_t)usable_uah * 1000ll * (int64_t)p->self_discharge_ppm_per_year,
		1000000ll * HOURS_PER_YEAR);

	total_na = (int64_t)out->sleep_na + out->beacon_na + out->data_rx_na + out->refresh_na +
		   out->tx_na + out->nfc_na + out->self_discharge_na;
	out->total_na = (uint32_t)total_na;

	if (total_na > 0) {
		/* hours = usable_nAh / total_nA; years = hours / 8760. */
		int64_t milliyears = usslp_round_div((int64_t)usable_uah * 1000ll * 1000ll,
						     total_na * HOURS_PER_YEAR);

		if (milliyears > INT32_MAX) {
			milliyears = INT32_MAX;
		}
		out->life_milliyears = (int32_t)milliyears;
	}
	out->meets_target = out->life_milliyears >= 7000 && out->life_milliyears <= 10000;
}

uint32_t usslp_sustainable_beacon_us(const struct usslp_power_profile *p,
				     enum usslp_display_tier tier, const struct usslp_workload *w,
				     uint32_t target_milliyears)
{
	struct usslp_power_profile stripped = *p;
	struct usslp_projection base;
	uint64_t usable_uah;
	int64_t budget_na, available_na, per_window_na_us, interval_us;

	if (target_milliyears == 0) {
		return 0;
	}
	/* Everything except the listening. */
	stripped.beacon_rx_na = 0;
	usslp_project(&stripped, tier, w, &base);

	usable_uah = (uint64_t)p->capacity_uah *
		     usslp_capacity_derating_ppm(w->ambient_deci_c) / 1000000ull;
	/* The average current that exhausts exactly `target` years:
	 * nA = usable_nAh / (years * 8760 h). */
	budget_na = usslp_round_div((int64_t)usable_uah * 1000ll * 1000ll,
				    (int64_t)target_milliyears * HOURS_PER_YEAR);
	available_na = budget_na - (int64_t)base.total_na;
	if (available_na <= 0) {
		return 0;
	}
	/* One window costs beacon_rx_na for beacon_rx_duration_us; averaged over an
	 * interval I that is beacon_rx_na * duration / I. Solve for I. */
	per_window_na_us = (int64_t)p->beacon_rx_na * (int64_t)p->beacon_rx_duration_us;
	interval_us = per_window_na_us / available_na;
	if (interval_us <= 0) {
		return 0;
	}
	if (interval_us > (int64_t)UINT32_MAX) {
		return UINT32_MAX;
	}
	return (uint32_t)interval_us;
}

uint16_t usslp_battery_mv(uint32_t depth_of_discharge_ppm)
{
	uint64_t d = depth_of_discharge_ppm;

	if (d == 0u) {
		return 3050;
	}
	if (d >= 1000000u) {
		return 1800;
	}
	if (d < 900000u) {
		/* 3050 - 250*d^2, with d as a fraction.
		 *
		 * The whole expression is rounded, not the drop: Go rounds
		 * 3050 - 62.5 to 2988, and rounding the drop first would give 2987.
		 * One millivolt sounds like nothing until it is the difference between
		 * two labels on the same shelf reporting different battery states from
		 * the same charge. */
		uint64_t scaled = 3050ull * 1000000000000ull - 250ull * d * d;

		return (uint16_t)((scaled + 500000000000ull) / 1000000000000ull);
	}
	/* The last tenth falls from 2848 mV to 1800 mV. */
	{
		uint64_t f = d - 900000u; /* 0..100000 */
		uint64_t scaled = 2848ull * 100000ull - 1048ull * f;

		return (uint16_t)((scaled + 50000ull) / 100000ull);
	}
}
