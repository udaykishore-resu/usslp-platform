#include "power.h"

#include "../app/telemetry.h"
#include "../display/eink.h"
#include "pmic.h"

#include <string.h>
#include <zephyr/kernel.h>
#include <zephyr/logging/log.h>
#include <zephyr/pm/device.h>
#include <zephyr/pm/pm.h>

LOG_MODULE_REGISTER(usslp_power, CONFIG_USSLP_LOG_LEVEL);

/* Current in nanoamps per state, from the hardware blueprint. The refresh
 * current comes from the panel table so that the three tiers are not written
 * down twice. */
static const uint32_t state_na[] = {
	[USSLP_POWER_DEEP_SLEEP] = 800,
	[USSLP_POWER_BEACON_RX] = 6500000,
	[USSLP_POWER_DATA_RX] = 12000000,
	[USSLP_POWER_TX] = 18000000,
	[USSLP_POWER_REFRESH] = 26000000, /* overridden per tier below */
	[USSLP_POWER_NFC] = 8000000,
	[USSLP_POWER_OTA] = 12000000,
};

static struct usslp_power_ledger ledger;
static struct usslp_power_profile profile;
static struct usslp_workload workload;
static struct usslp_projection projection;

static int64_t state_entered_ms[ARRAY_SIZE(state_na)];
static bool state_active[ARRAY_SIZE(state_na)];

/* The activity window, in uptime milliseconds. window_from is a separate field
 * from window_until, not a convenience: a zone wake instruction does not take
 * effect until the label hears it, and counting the intervening rest as fast
 * listening would overstate the energy the wake costs by an order of magnitude.
 * labelsim.beaconIntervalAt has the same pair for the same reason. */
static int64_t window_from;
static int64_t window_until;

static int64_t time_base_unix;   /* unix seconds at time_base_uptime */
static int64_t time_base_uptime; /* k_uptime_get() when the time was set */
static bool have_time;

static uint32_t rest_interval_ms = CONFIG_USSLP_BEACON_SLOW_MS;

static K_MUTEX_DEFINE(power_lock);

static uint32_t current_for(enum usslp_power_state s)
{
	if (s == USSLP_POWER_REFRESH) {
		return usslp_panel(usslp_eink_tier())->refresh_current_ua * 1000u;
	}
	return state_na[s];
}

static uint64_t *bucket_for(enum usslp_power_state s)
{
	switch (s) {
	case USSLP_POWER_BEACON_RX:
		return &ledger.beacon_nah;
	case USSLP_POWER_DATA_RX:
		return &ledger.data_rx_nah;
	case USSLP_POWER_TX:
		return &ledger.tx_nah;
	case USSLP_POWER_REFRESH:
		return &ledger.refresh_nah;
	case USSLP_POWER_NFC:
		return &ledger.nfc_nah;
	case USSLP_POWER_OTA:
		return &ledger.ota_nah;
	case USSLP_POWER_DEEP_SLEEP:
	default:
		return &ledger.sleep_nah;
	}
}

int usslp_power_init(void)
{
	int rc;

	memset(&ledger, 0, sizeof(ledger));
	usslp_power_profile_default(&profile);
	profile.beacon_fast_us = (uint32_t)CONFIG_USSLP_BEACON_FAST_MS * 1000u;
	profile.beacon_slow_us = (uint32_t)CONFIG_USSLP_BEACON_SLOW_MS * 1000u;
	profile.active_window_us = (uint32_t)CONFIG_USSLP_ACTIVE_WINDOW_MS * 1000u;
	usslp_workload_default(&workload);

	rc = usslp_pmic_init();
	if (rc != USSLP_OK) {
		LOG_ERR("PMIC init failed (%d); running without charge control", rc);
	}

	/* The mains-powered relay build keeps its receiver on and has no battery to
	 * spend, so it does not duty cycle at all. */
	if (IS_ENABLED(CONFIG_USSLP_ZIGBEE_ROUTER)) {
		rest_interval_ms = CONFIG_USSLP_BEACON_FAST_MS;
		LOG_INF("relay build: the receiver stays on");
	} else {
		usslp_power_retune();
	}
	usslp_power_enter(USSLP_POWER_DEEP_SLEEP);
	return USSLP_OK;
}

void usslp_power_enter(enum usslp_power_state s)
{
	k_mutex_lock(&power_lock, K_FOREVER);
	__ASSERT(!state_active[s], "re-entering power state %d", (int)s);
	state_active[s] = true;
	state_entered_ms[s] = k_uptime_get();
	if (s == USSLP_POWER_BEACON_RX) {
		ledger.beacon_windows++;
		if (window_from <= state_entered_ms[s] &&
		    state_entered_ms[s] < window_until) {
			ledger.fast_beacon_windows++;
		}
	}
	k_mutex_unlock(&power_lock);
}

void usslp_power_exit(enum usslp_power_state s)
{
	int64_t held_ms;
	uint64_t nah;

	k_mutex_lock(&power_lock, K_FOREVER);
	if (!state_active[s]) {
		k_mutex_unlock(&power_lock);
		return;
	}
	state_active[s] = false;
	held_ms = k_uptime_get() - state_entered_ms[s];
	if (held_ms < 0) {
		held_ms = 0;
	}
	/* nAh = nA * ms / 3,600,000. Integer throughout: this accumulator is read
	 * by the fleet planner and a float here would drift over a decade of
	 * additions. */
	nah = ((uint64_t)current_for(s) * (uint64_t)held_ms) / 3600000ull;
	*bucket_for(s) += nah;
	k_mutex_unlock(&power_lock);
}

void usslp_power_note_activity(void)
{
	int64_t now = k_uptime_get();

	k_mutex_lock(&power_lock, K_FOREVER);
	window_from = now;
	if (now + CONFIG_USSLP_ACTIVE_WINDOW_MS > window_until) {
		window_until = now + CONFIG_USSLP_ACTIVE_WINDOW_MS;
	}
	k_mutex_unlock(&power_lock);
}

void usslp_power_open_window(uint32_t duration_ms)
{
	int64_t now = k_uptime_get();

	k_mutex_lock(&power_lock, K_FOREVER);
	if (IS_ENABLED(CONFIG_USSLP_ZIGBEE_ROUTER) || now < window_until) {
		/* Already open, or already scheduled to open: extend, never restart.
		 * Restarting would be a real bug and a subtle one — a controller
		 * re-broadcasting the flag every few seconds during a price load would
		 * push the opening moment forward on every repeat and the zone would
		 * never wake at all. */
		if (now + (int64_t)duration_ms > window_until) {
			window_until = now + duration_ms;
		}
		k_mutex_unlock(&power_lock);
		return;
	}
	/* Resting with no window pending. The label does not hear this instruction
	 * until its next receive window, up to one resting interval away, and keeps
	 * resting until then. Pretending otherwise is how a simulator — or a
	 * firmware — reports a wake latency an order of magnitude better than the
	 * hardware delivers. */
	window_from = now + rest_interval_ms;
	window_until = window_from + duration_ms;
	k_mutex_unlock(&power_lock);
}

bool usslp_power_active(void)
{
	bool active;

	if (IS_ENABLED(CONFIG_USSLP_ZIGBEE_ROUTER)) {
		return true;
	}
	k_mutex_lock(&power_lock, K_FOREVER);
	active = k_uptime_get() < window_until;
	k_mutex_unlock(&power_lock);
	return active;
}

uint32_t usslp_power_beacon_interval_ms(void)
{
	int64_t now;
	uint32_t interval;

	if (IS_ENABLED(CONFIG_USSLP_ZIGBEE_ROUTER)) {
		return CONFIG_USSLP_BEACON_FAST_MS;
	}
	k_mutex_lock(&power_lock, K_FOREVER);
	now = k_uptime_get();
	interval = (now >= window_from && now < window_until)
			   ? (uint32_t)CONFIG_USSLP_BEACON_FAST_MS
			   : rest_interval_ms;
	k_mutex_unlock(&power_lock);
	return interval;
}

void usslp_power_ledger(struct usslp_power_ledger *out)
{
	k_mutex_lock(&power_lock, K_FOREVER);
	*out = ledger;
	k_mutex_unlock(&power_lock);
}

void usslp_power_battery(uint16_t *millivolts, uint8_t *percent)
{
	usslp_gauge_read(millivolts, percent);
}

void usslp_power_projection(struct usslp_projection *out)
{
	k_mutex_lock(&power_lock, K_FOREVER);
	*out = projection;
	k_mutex_unlock(&power_lock);
}

void usslp_power_retune(void)
{
	struct usslp_power_profile tuned;
	uint32_t sustainable_us;
	int16_t centi_c = 2000;
	uint64_t uptime_days_milli;

	(void)usslp_eink_temperature(&centi_c);

	k_mutex_lock(&power_lock, K_FOREVER);

	/* The label's own observed rate, not the platform's planning figure. A
	 * label being repriced ten times more often than the plan assumed has to
	 * slow its listening or it will not last; one that is never repriced can
	 * afford to listen more often and hit its SLO more reliably. */
	uptime_days_milli = ((uint64_t)k_uptime_get() * 1000ull) / 86400000ull;
	if (uptime_days_milli >= 1000ull) { /* at least one day of evidence */
		uint64_t refreshes = ledger.refresh_nah;
		(void)refreshes;
		/* Updates per day in thousandths, from the actual applied count held by
		 * the price module's telemetry hook. Until a day has passed the
		 * planning figure stands, because a label that took three updates in its
		 * first hour is being commissioned, not repriced hourly. */
		workload.updates_per_day_milli = usslp_telemetry_updates_per_day_milli();
	}
	workload.ambient_deci_c = (int32_t)(centi_c / 10);

	tuned = profile;
	usslp_project(&tuned, usslp_eink_tier(), &workload, &projection);

	sustainable_us = usslp_sustainable_beacon_us(&profile, usslp_eink_tier(), &workload,
						     CONFIG_USSLP_TARGET_LIFE_MILLIYEARS);
	if (sustainable_us == 0u) {
		/* The rest of the budget already exceeds the target and no listen
		 * interval can save it. That is a real answer, not a failure: it means
		 * the panel and the workload are mismatched, and the right response is
		 * to rest as slowly as the latency budget tolerates and report the
		 * projection so somebody looks at the planogram. */
		rest_interval_ms = 60000u;
		LOG_WRN("no beacon interval reaches the %d.%03d-year target on this "
			"workload; resting at %u ms and reporting a %d.%03d-year "
			"projection",
			CONFIG_USSLP_TARGET_LIFE_MILLIYEARS / 1000,
			CONFIG_USSLP_TARGET_LIFE_MILLIYEARS % 1000, rest_interval_ms,
			projection.life_milliyears / 1000, projection.life_milliyears % 1000);
	} else {
		uint32_t ms = sustainable_us / 1000u;

		/* Never rest faster than the configured floor — that would spend the
		 * budget the activity window needs — and never slower than a minute,
		 * beyond which a zone wake takes longer than a store manager will wait
		 * before deciding the shelf is broken. */
		if (ms < CONFIG_USSLP_BEACON_SLOW_MS) {
			ms = CONFIG_USSLP_BEACON_SLOW_MS;
		}
		if (ms > 60000u) {
			ms = 60000u;
		}
		rest_interval_ms = ms;
	}
	profile.beacon_slow_us = rest_interval_ms * 1000u;
	k_mutex_unlock(&power_lock);

	LOG_INF("power retune: rest %u ms, projection %d.%03d years, %u nA average "
		"(%u nA of it beacons)",
		rest_interval_ms, projection.life_milliyears / 1000,
		projection.life_milliyears % 1000, projection.total_na, projection.beacon_na);
}

int64_t usslp_power_unix_time(void)
{
	int64_t t;

	k_mutex_lock(&power_lock, K_FOREVER);
	if (!have_time) {
		k_mutex_unlock(&power_lock);
		return 0;
	}
	t = time_base_unix + (k_uptime_get() - time_base_uptime) / 1000;
	k_mutex_unlock(&power_lock);
	return t;
}

void usslp_power_set_unix_time(int64_t seconds)
{
	k_mutex_lock(&power_lock, K_FOREVER);
	time_base_unix = seconds;
	time_base_uptime = k_uptime_get();
	have_time = seconds > 0;
	k_mutex_unlock(&power_lock);
	if (seconds > 0) {
		LOG_INF("clock set from the controller beacon");
	}
}
