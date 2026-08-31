/*
 * power.h - the power state machine.
 *
 * The honest version of the platform's battery story, in one paragraph.
 *
 * The hardware blueprint quotes a 250 ms beacon interval and a seven-to-ten-year
 * battery life. Those two numbers do not fit in the same sentence. A 6.5 mA
 * listen for 8 ms every 250 ms is a 3.2% duty cycle on the receiver, which
 * averages 208 uA; against a 500 mAh cell that is 0.27 years — ninety-nine days.
 * The arithmetic is worked through in usslp_budget.h and the host tests assert
 * it, precisely so that the figure cannot quietly stop being true.
 *
 * What makes the target reachable is adaptive duty cycling: the label listens at
 * 250 ms only inside an activity window and rests at a much slower interval the
 * rest of the time. With a 60-second window and a 30-second resting interval the
 * average is 3.24 uA of beacon against a 6.58 uA total, and the projection is
 * 8.7 years on the 2.9-inch panel at 20 C.
 *
 * The cost, stated plainly because it is a real cost and the platform's latency
 * budget depends on it: a resting label is on average fifteen seconds from being
 * reachable at all. The platform's 300 ms SEC-to-label budget
 * (INTERFACE-CONTRACTS section 4) is therefore a statement about a zone in its
 * active window, not about a label asleep at three in the morning. A price load
 * is planned as a window — the controller broadcasts a wake instruction, the
 * zone comes up over one resting interval, and then the prices flow inside the
 * budget. edge/labelsim/label.go models exactly this with OpenActiveWindow, and
 * this firmware implements the same behaviour, including the subtlety that a
 * resting label does not hear the wake instruction until its own next window.
 *
 * The resting interval is not a constant. usslp_power_retune derives it from the
 * measured workload and the configured target life, so a label being repriced
 * hourly, or one in a freezer case with 30% less usable capacity, chooses a
 * slower rest than one on an ambient shelf. That is the difference between a
 * fleet where the freezer aisle dies three years early and one where it does
 * not.
 */

#ifndef USSLP_POWER_H
#define USSLP_POWER_H

#include "../usslp_portable.h"
#include "usslp_budget.h"

enum usslp_power_state {
	/* System OFF with RAM retention: 0.8 uA, and where the label spends
	 * essentially all of its life. */
	USSLP_POWER_DEEP_SLEEP = 0,
	/* The receiver is on for a beacon window: 6.5 mA for 8 ms. */
	USSLP_POWER_BEACON_RX,
	/* Receiving a data frame: 12 mA. */
	USSLP_POWER_DATA_RX,
	/* Transmitting: 18 mA for about 5 ms. */
	USSLP_POWER_TX,
	/* Driving a waveform: 26-35 mA for 1.5-2.0 s, or 15 s on the colour panel.
	 * The largest current the device ever draws, and the radio is off. */
	USSLP_POWER_REFRESH,
	/* Serving an NFC tap: 8 mA. */
	USSLP_POWER_NFC,
	/* An OTA transfer: sustained receive, which is why a rollout is scheduled
	 * against a budget rather than run whenever an image is ready. */
	USSLP_POWER_OTA,
};

int usslp_power_init(void);

/* Enters and leaves a state, accumulating the charge. Nested calls are not
 * supported and are an assertion failure in a debug build: a device that thinks
 * it is transmitting and refreshing at once has lost track of its own hardware.
 */
void usslp_power_enter(enum usslp_power_state state);
void usslp_power_exit(enum usslp_power_state state);

/*
 * Records that something happened, which opens or extends the activity window.
 *
 * Called from the price path, from an NFC tap, and from a zone wake broadcast.
 * A label somebody is standing in front of, or one whose aisle is being
 * repriced, is a label worth being able to reach quickly.
 */
void usslp_power_note_activity(void);

/* The listen interval in force right now, in milliseconds. The radio layer asks
 * before arming its next receive window. */
uint32_t usslp_power_beacon_interval_ms(void);

/* True while the activity window is open. */
bool usslp_power_active(void);

/*
 * Opens the activity window for a duration, as instructed by a zone broadcast.
 *
 * Mirrors labelsim.OpenActiveWindow, including the part that is easy to get
 * wrong: a window already open is *extended*, never restarted. A controller
 * re-broadcasting the flag every few seconds during a price load would otherwise
 * push the opening moment forward on every repeat and the zone would never
 * actually wake.
 */
void usslp_power_open_window(uint32_t duration_ms);

/* Charge drawn since boot, in nanoamp-hours, by state. Reported in telemetry:
 * an aggregate "the battery is at 82%" tells an operator nothing, while
 * "83% of the draw is beacons" tells them which knob to turn. */
struct usslp_power_ledger {
	uint64_t sleep_nah;
	uint64_t beacon_nah;
	uint64_t data_rx_nah;
	uint64_t tx_nah;
	uint64_t refresh_nah;
	uint64_t nfc_nah;
	uint64_t ota_nah;
	uint32_t beacon_windows;
	uint32_t fast_beacon_windows;
};

void usslp_power_ledger(struct usslp_power_ledger *out);

/* Battery voltage and remaining percentage, from the gauge. */
void usslp_power_battery(uint16_t *millivolts, uint8_t *percent);

/*
 * Recomputes the resting beacon interval from the observed workload.
 *
 * Called daily and after any large change in behaviour. It uses the label's
 * *own* measured update rate and shelf temperature rather than the platform's
 * planning figures, because a label that is being repriced ten times more often
 * than the plan assumed has to slow its listening down or it will not last, and
 * a label that is never repriced can afford to listen more often and reach its
 * SLO more reliably.
 */
void usslp_power_retune(void);

/*
 * The current projection, for telemetry and for the commissioning report. A
 * planogram that has put a colour panel on a high-churn shelf shows up here as a
 * projection under a year, and app/provision.c raises it at commissioning rather
 * than letting the fleet discover it in year one.
 */
void usslp_power_projection(struct usslp_projection *out);

/*
 * Wall-clock time in seconds since the epoch, or 0 when the label has no trusted
 * time.
 *
 * A label has no RTC crystal it can trust across a decade and no network time
 * protocol; it takes time from the controller's beacon and holds it in the RTC
 * across sleep. Returning 0 rather than a guess matters: the attestation
 * verifier treats 0 as "no clock" and skips the key validity window rather than
 * rejecting every key because the label thinks it is 1970.
 */
int64_t usslp_power_unix_time(void);
void usslp_power_set_unix_time(int64_t seconds);

/* Energy harvesting: the RF rectifier's contribution since boot, in nanoamp
 * hours. Sampled rather than relied on — a label under a reader gate harvests
 * usefully and one in the middle of an aisle harvests nothing — and reported so
 * a fleet planner can see which stores actually benefit. */
uint64_t usslp_harvest_nah(void);

#endif /* USSLP_POWER_H */
