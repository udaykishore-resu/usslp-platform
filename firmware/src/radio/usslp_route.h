/*
 * usslp_route.h - the mesh cost model and the predictive link-failure model, in
 * fixed point.
 *
 * Two things live here.
 *
 * 1. The Zigbee link metric. LQI from RSSI, the specification's link cost, the
 *    parent-selection score, and the airtime model. Ports of mesh.LQIFromRSSI,
 *    mesh.LinkCost, Network.bestParentLocked and mesh.Airtime in edge/mesh.
 *
 * 2. The link-failure model the Shelf Edge Controller uses for predictive
 *    self-healing: a logistic regression over five features, sec.FailureRisk in
 *    edge/sec/predict.go. It runs on the label as well as on the controller
 *    because a mains-powered relay label routes for its children and has to make
 *    the same decision about its own uplink.
 *
 * Why fixed point
 * ---------------
 * The nRF52840 is a Cortex-M4F and does have a single-precision FPU, so this is
 * not a "no hardware" argument. It is a power argument. Touching the FPU sets
 * CONTROL.FPCA, and from then on every exception entry pushes 18 extra words
 * (or takes the lazy-stacking fault to do it later) — and this model is
 * evaluated on every link sample, which is to say inside the radio interrupt
 * path, tens of times a minute, for the whole seven-year life of the cell. The
 * FPU also has to be clocked, and the nRF52840's LDO regime means an enabled FPU
 * measurably raises the floor of an otherwise 0.8 uA sleep current.
 *
 * So everything is Q16.16 in int32/int64, the logistic is evaluated with an
 * integer exp2 and a fourth-order polynomial, and tests/test_route.c asserts
 * agreement with the Go model's output to within 1e-3 absolute probability —
 * which is four orders of magnitude finer than the decision threshold the model
 * feeds, so the quantisation cannot change a routing decision that was not
 * already on a knife edge.
 */

#ifndef USSLP_ROUTE_H
#define USSLP_ROUTE_H

#include "../usslp_portable.h"

/* Q16.16 fixed point. One unit is 1/65536. */
typedef int32_t usslp_q16;
#define USSLP_Q16_ONE 65536
#define USSLP_Q16_FROM_INT(x) ((usslp_q16)((x) * USSLP_Q16_ONE))

/* ---------------------------------------------------------------------- */
/* Link metric                                                             */
/* ---------------------------------------------------------------------- */

/*
 * mesh.LQIFromRSSI. Linear between the receiver's noise floor (-100 dBm, LQI 0)
 * and AGC saturation (-35 dBm, LQI 255), which is approximately what the TI and
 * Silicon Labs parts do.
 *
 * The argument is in hundredths of a dBm rather than a float so that the whole
 * path from the radio driver's raw reading to the routing decision is integer.
 * A CC2652P reports RSSI in whole dBm, so the conversion is a multiply by 100 at
 * the driver boundary; the finer unit exists because the *simulator* works in
 * fractional dBm and the tests compare against it.
 */
int usslp_lqi_from_rssi(int32_t rssi_centi_dbm);

/*
 * mesh.LinkCost. The Zigbee specification's own metric: the reciprocal of the
 * fourth power of the delivery probability, clamped to 1..7, so a marginal link
 * is seven times as expensive as a good one and a route with one bad hop loses
 * to a longer route made of good ones.
 */
int usslp_link_cost(int lqi);

/* mesh.Fragments: how many 802.15.4 frames a payload occupies. */
int usslp_fragments(int payload_bytes);

/*
 * mesh.Airtime in microseconds: how long a payload occupies the shared 250 kbps
 * channel for one hop, including fragmentation, acknowledgements and turnaround.
 * This is the number that makes a store-wide promotion expensive, and the label
 * needs it to charge its own energy accounting for a transmission.
 */
int64_t usslp_airtime_us(int payload_bytes);

/* Routing penalties, from edge/mesh/routing.go. avoidPenalty exceeds the worst
 * plausible path cost so that an avoided link is used only when there is
 * genuinely no alternative: the label keeps working, degraded, rather than going
 * dark because a prediction was pessimistic. */
#define USSLP_AVOID_PENALTY 64
#define USSLP_LOW_BATTERY_PENALTY 2
#define USSLP_LOW_BATTERY_THRESHOLD_PCT 20

/* One entry of the neighbour table. An end device keeps 24 of these, a
 * mains-powered relay 64 — those are the hardware's numbers, not a tuning
 * choice: an 802.15.4 stack on a small part simply does not remember more. */
#define USSLP_MAX_NEIGHBOURS 24
#define USSLP_MAX_ROUTER_NEIGHBOURS 64

enum usslp_node_kind {
	USSLP_NODE_END_DEVICE = 0,
	USSLP_NODE_ROUTER = 1,
	USSLP_NODE_COORDINATOR = 2,
};

struct usslp_neighbour {
	uint16_t short_addr;
	uint8_t kind;   /* enum usslp_node_kind */
	uint8_t depth;  /* hops from the coordinator */
	uint8_t lqi;    /* most recent */
	int8_t rssi_dbm;
	uint8_t battery_pct;
	uint8_t child_count;
	bool joined;
	bool avoided; /* the controller asked us to route around this link */
	bool alive;
};

/*
 * Total cost of using a neighbour as the next hop, including the avoidance and
 * low-battery penalties. Returns INT32_MAX for a link that must not be used.
 */
int32_t usslp_next_hop_cost(const struct usslp_neighbour *n);

/*
 * Network.bestParentLocked. Picks the parent a joining node would choose: the
 * strongest link among joined routers with spare capacity and room in the tree,
 * breaking ties toward the shallower node so the tree stays wide rather than
 * deep. Returns the index into the table, or -1 if nothing is eligible.
 *
 * max_children and max_depth are the zone's configuration; the defaults below
 * are what edge/mesh uses.
 */
#define USSLP_DEFAULT_MAX_CHILDREN 20
#define USSLP_DEFAULT_MAX_DEPTH 5
/* Below this the association exchange itself would not complete. */
#define USSLP_MIN_PARENT_LQI 40

int usslp_select_parent(const struct usslp_neighbour *table, size_t count, uint8_t max_children,
			uint8_t max_depth);

/* ---------------------------------------------------------------------- */
/* Predictive link failure                                                 */
/* ---------------------------------------------------------------------- */

/* sec.RerouteThreshold: the reactive rule, always armed in both healing modes.
 * Prediction is an addition to it, never a replacement — a link that has already
 * failed must be moved whether or not a model saw it coming. */
#define USSLP_REROUTE_THRESHOLD 100
/* sec.MinDegradationTrend, in LQI units per minute, Q16.16. */
#define USSLP_MIN_DEGRADATION_TREND_Q16 ((usslp_q16)(-5 * USSLP_Q16_ONE))
/* sec.TrendSignificance: standard errors below zero a slope must clear. */
#define USSLP_TREND_SIGNIFICANCE_Q16 ((usslp_q16)(2 * USSLP_Q16_ONE))

/* Features, all Q16.16, all derivable from what a node samples anyway. */
struct usslp_link_features {
	usslp_q16 lqi;
	usslp_q16 lqi_trend_per_minute;
	usslp_q16 rssi_stddev;
	usslp_q16 battery_fraction;
	usslp_q16 depth;
};

/*
 * sec.FailureRisk: the model's probability, as Q16.16 in [0,1], that the link
 * degrades below the reroute threshold within the five-minute prediction
 * horizon.
 *
 * The coefficients are a smooth version of "extrapolate the LQI trend three
 * minutes forward and compare with the reroute threshold", adjusted by the three
 * secondary signals the fleet data says matter: RSSI variance (a link fluttering
 * between good and bad is about to settle on bad), the relay's remaining
 * battery, and the node's depth in the tree.
 */
usslp_q16 usslp_failure_risk(const struct usslp_link_features *f);

/* Bounded sample history for one link. Bounded because a relay with sixty-four
 * neighbours cannot keep unbounded history, and because the model only looks at
 * the recent trend: a link that was bad an hour ago and is fine now is fine. */
#define USSLP_LINK_HISTORY 10

struct usslp_link_history {
	/* Sample times in seconds since an arbitrary local origin, LQI and RSSI in
	 * Q16.16. A ring, oldest first after normalisation. */
	int32_t at_s[USSLP_LINK_HISTORY];
	usslp_q16 lqi[USSLP_LINK_HISTORY];
	usslp_q16 rssi[USSLP_LINK_HISTORY];
	uint8_t count;
};

void usslp_link_history_init(struct usslp_link_history *h);
void usslp_link_history_add(struct usslp_link_history *h, int32_t at_s, usslp_q16 lqi,
			    usslp_q16 rssi);

/*
 * Least-squares slope of LQI against time, in LQI per minute, with the standard
 * error of that slope.
 *
 * The standard error is what turns "the numbers went down" into "the numbers are
 * going down". Without it, three noisy samples that happen to descend look
 * exactly like a clean ramp, and an untested version of this model rerouted a
 * fifth of a healthy store in its first minute. Returns false when there are
 * fewer than three samples or they span no time, in which case no trend claim
 * may be made.
 */
bool usslp_link_trend(const struct usslp_link_history *h, usslp_q16 *slope_per_min,
		      usslp_q16 *stderr_per_min);

/* Sample standard deviation of recent received power, Q16.16 dB. */
usslp_q16 usslp_link_rssi_stddev(const struct usslp_link_history *h);

enum usslp_heal_mode {
	USSLP_HEAL_PREDICTIVE = 0,
	USSLP_HEAL_REACTIVE = 1,
	USSLP_HEAL_OFF = 2,
};

struct usslp_link_assessment {
	struct usslp_link_features features;
	usslp_q16 risk;
	bool act;
	const char *why;
};

/*
 * sec.assess. Applies the configured healing policy to one link's history.
 * risk_threshold is Q16.16.
 */
struct usslp_link_assessment usslp_assess_link(enum usslp_heal_mode mode,
					       const struct usslp_link_history *h,
					       usslp_q16 battery_fraction, uint8_t depth,
					       usslp_q16 risk_threshold);

/* Fixed-point helpers, exposed because the tests exercise them directly and
 * because the telemetry reporter formats Q16.16 values for the uplink. */
usslp_q16 usslp_q16_mul(usslp_q16 a, usslp_q16 b);
usslp_q16 usslp_q16_div(usslp_q16 a, usslp_q16 b);
usslp_q16 usslp_q16_sqrt(usslp_q16 x);
/* e^-x / (1 + e^-x) evaluated as 1/(1+e^-z); z is Q16.16 and so is the result. */
usslp_q16 usslp_q16_logistic(usslp_q16 z);

#endif /* USSLP_ROUTE_H */
