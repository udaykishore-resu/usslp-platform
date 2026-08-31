#include "usslp_route.h"

#include <string.h>

/* ---------------------------------------------------------------------- */
/* Q16.16 arithmetic                                                       */
/*                                                                         */
/* Two portability notes, because fixed-point code is where they bite.     */
/*                                                                         */
/* Scaling up is done by multiplying by USSLP_Q16_ONE, never by a left     */
/* shift: shifting a negative signed value is undefined behaviour, and the */
/* values here are routinely negative — an LQI trend is negative exactly   */
/* when the model has something to say. Building the host tests with       */
/* -fsanitize=undefined -fno-sanitize-recover catches a regression here.   */
/*                                                                         */
/* Scaling down *is* done with a right shift, deliberately. Right-shifting */
/* a negative value is implementation-defined rather than undefined, and   */
/* every compiler this firmware will ever meet — GCC, Clang, armclang —    */
/* defines it as an arithmetic shift, which is the floor semantics the     */
/* fixed-point rounding depends on. Dividing by 65536 instead would round  */
/* toward zero and quietly change the model's output near zero.            */
/* ---------------------------------------------------------------------- */

usslp_q16 usslp_q16_mul(usslp_q16 a, usslp_q16 b)
{
	int64_t p = (int64_t)a * (int64_t)b;

	/* Round half away from zero so that the fixed-point path and the Go
	 * double-precision path agree on a boundary rather than drifting by one
	 * ULP per operation. */
	if (p >= 0) {
		return (usslp_q16)((p + (USSLP_Q16_ONE / 2)) >> 16);
	}
	return (usslp_q16)(-((-p + (USSLP_Q16_ONE / 2)) >> 16));
}

usslp_q16 usslp_q16_div(usslp_q16 a, usslp_q16 b)
{
	int64_t n;

	if (b == 0) {
		return a >= 0 ? INT32_MAX : INT32_MIN;
	}
	/* Scaled by multiplication rather than by a left shift: shifting a negative
	 * signed value is undefined behaviour, and a is routinely negative here (an
	 * LQI trend is negative exactly when the model has something to say). */
	n = (int64_t)a * (int64_t)USSLP_Q16_ONE;
	return (usslp_q16)usslp_round_div(n, (int64_t)b);
}

usslp_q16 usslp_q16_sqrt(usslp_q16 x)
{
	uint64_t v, r, bit;

	if (x <= 0) {
		return 0;
	}
	/* sqrt(x/2^16) * 2^16 = sqrt(x * 2^16). Integer Newton on the widened
	 * value; the classic bit-by-bit method is used because it has no division
	 * and terminates in a fixed 32 iterations. */
	v = ((uint64_t)(uint32_t)x) << 16;
	r = 0;
	bit = 1ull << 62;
	while (bit > v) {
		bit >>= 2;
	}
	while (bit != 0) {
		if (v >= r + bit) {
			v -= r + bit;
			r = (r >> 1) + bit;
		} else {
			r >>= 1;
		}
		bit >>= 2;
	}
	if (r > (uint64_t)INT32_MAX) {
		return INT32_MAX;
	}
	return (usslp_q16)r;
}

/*
 * exp2 in Q16.16 for a Q16.16 argument, returned as a Q16.16 value.
 *
 * The fractional part is evaluated with the standard fourth-order minimax
 * polynomial for 2^f on [0,1), whose maximum error is about 2e-6 — three orders
 * of magnitude finer than the 1/65536 the representation can hold, so the
 * polynomial is not the limiting factor.
 */
static usslp_q16 q16_exp2(usslp_q16 x)
{
	int32_t k = x >> 16;                             /* arithmetic shift: floor */
	usslp_q16 f = (usslp_q16)((uint32_t)x & 0xffffu); /* [0, 1) in Q16.16 */
	int64_t p;

	if (k > 30) {
		return INT32_MAX;
	}
	if (k < -31) {
		return 0;
	}
	/* 2^f ~= 1 + f(0.6931472 + f(0.2402265 + f(0.0555041 + f*0.0096181))) */
	p = 630; /* 0.0096181 * 65536 = 630.4 */
	p = ((p * f) >> 16) + 3637;  /* 0.0555041 */
	p = ((p * f) >> 16) + 15743; /* 0.2402265 */
	p = ((p * f) >> 16) + 45426; /* 0.6931472 */
	p = ((p * f) >> 16) + 65536; /* 1.0 */

	if (k >= 0) {
		p <<= k;
		if (p > INT32_MAX) {
			return INT32_MAX;
		}
	} else {
		p >>= -k;
	}
	return (usslp_q16)p;
}

/* log2(e) in Q16.16. */
#define Q16_LOG2E 94548 /* 1.4426950409 * 65536 = 94548.46 */

usslp_q16 usslp_q16_logistic(usslp_q16 z)
{
	usslp_q16 t, e;
	int64_t num, den;

	/* Saturate outside +/-16, where the result is within one ULP of 0 or 1 and
	 * the intermediate exponential would overflow the representation. */
	if (z > (16 * USSLP_Q16_ONE)) {
		return USSLP_Q16_ONE;
	}
	if (z < -(16 * USSLP_Q16_ONE)) {
		return 0;
	}
	/* e^-z = 2^(-z * log2 e) */
	t = usslp_q16_mul(-z, Q16_LOG2E);
	e = q16_exp2(t);

	den = (int64_t)USSLP_Q16_ONE + (int64_t)e;
	if (den <= 0) {
		return 0;
	}
	num = (int64_t)USSLP_Q16_ONE * (int64_t)USSLP_Q16_ONE;
	return (usslp_q16)usslp_round_div(num, den);
}

/* ---------------------------------------------------------------------- */
/* Link metric                                                             */
/* ---------------------------------------------------------------------- */

int usslp_lqi_from_rssi(int32_t rssi_centi_dbm)
{
	/* floor -100 dBm, ceiling -35 dBm, linear 0..255 between them. */
	const int32_t floor_c = -10000;
	const int32_t ceiling_c = -3500;
	int64_t num;

	if (rssi_centi_dbm <= floor_c) {
		return 0;
	}
	if (rssi_centi_dbm >= ceiling_c) {
		return 255;
	}
	num = 255ll * (int64_t)(rssi_centi_dbm - floor_c);
	return (int)usslp_round_div(num, (int64_t)(ceiling_c - floor_c));
}

int usslp_link_cost(int lqi)
{
	/* cost = round(1 / p^4) with p = lqi/255, clamped to 1..7. Evaluated as
	 * round(255^4 / lqi^4); 255^4 is 4,228,250,625 and lqi^4 has the same
	 * bound, so int64 is comfortable. */
	const int64_t full = 4228250625ll; /* 255^4 */
	int64_t l, l4, cost;

	if (lqi <= 0) {
		return 7;
	}
	if (lqi > 255) {
		lqi = 255;
	}
	l = (int64_t)lqi;
	l4 = l * l * l * l;
	cost = usslp_round_div(full, l4);
	if (cost < 1) {
		return 1;
	}
	if (cost > 7) {
		return 7;
	}
	return (int)cost;
}

/* 802.15.4 PHY constants, from edge/mesh/radio.go. */
#define USSLP_DATA_RATE_BPS 250000
#define USSLP_PHY_OVERHEAD_BYTES 6
/* MAC, NWK and APS headers plus the message integrity code, with Zigbee
 * network-layer security enabled. Security is not optional in USSLP, so its
 * cost is in the baseline rather than in a footnote. */
#define USSLP_MAC_OVERHEAD_BYTES 25
#define USSLP_MAX_FRAME_BYTES 127
#define USSLP_ACK_FRAME_BYTES 11
#define USSLP_TURNAROUND_US 192

static int per_frame_payload(void)
{
	return USSLP_MAX_FRAME_BYTES - USSLP_PHY_OVERHEAD_BYTES - USSLP_MAC_OVERHEAD_BYTES;
}

int usslp_fragments(int payload_bytes)
{
	int per = per_frame_payload();

	if (payload_bytes <= 0) {
		return 1;
	}
	return (payload_bytes + per - 1) / per;
}

/* Time on air for n bytes, in whole nanoseconds. Go computes this in float64
 * nanoseconds and truncates when it converts to a Duration; the byte count times
 * 32000 ns is exact at 250 kbps (8 bits / 250000 bps = 32 us per byte), so no
 * rounding question arises. */
static int64_t bytes_on_air_ns(int n)
{
	return (int64_t)n * 32000ll;
}

int64_t usslp_airtime_us(int payload_bytes)
{
	int frags = usslp_fragments(payload_bytes);
	int per = per_frame_payload();
	int remaining = payload_bytes;
	int64_t total_ns = 0;

	for (int i = 0; i < frags; i++) {
		int body = remaining;
		int on_air;

		if (body > per) {
			body = per;
		}
		if (body < 0) {
			body = 0;
		}
		remaining -= body;
		on_air = USSLP_PHY_OVERHEAD_BYTES + USSLP_MAC_OVERHEAD_BYTES + body;
		total_ns += bytes_on_air_ns(on_air) + (int64_t)USSLP_TURNAROUND_US * 1000ll +
			    bytes_on_air_ns(USSLP_ACK_FRAME_BYTES) +
			    (int64_t)USSLP_TURNAROUND_US * 1000ll;
	}
	return total_ns / 1000ll;
}

int32_t usslp_next_hop_cost(const struct usslp_neighbour *n)
{
	int32_t cost;

	if (n == NULL || !n->alive) {
		return INT32_MAX;
	}
	if (n->kind != USSLP_NODE_COORDINATOR && !n->joined) {
		return INT32_MAX;
	}
	if (n->lqi < 20u) {
		/* Unusable: the MAC would never get an acknowledgement. */
		return INT32_MAX;
	}
	cost = (int32_t)usslp_link_cost((int)n->lqi);
	if (n->avoided) {
		cost += USSLP_AVOID_PENALTY;
	}
	if (n->kind == USSLP_NODE_ROUTER && n->battery_pct < USSLP_LOW_BATTERY_THRESHOLD_PCT) {
		/* A router about to die is a route about to break, and paying two cost
		 * units to avoid it is cheaper than a repair. */
		cost += USSLP_LOW_BATTERY_PENALTY;
	}
	return cost;
}

int usslp_select_parent(const struct usslp_neighbour *table, size_t count, uint8_t max_children,
			uint8_t max_depth)
{
	int best = -1;
	int32_t best_score = INT32_MIN;

	if (table == NULL) {
		return -1;
	}
	for (size_t i = 0; i < count; i++) {
		const struct usslp_neighbour *n = &table[i];
		int32_t score;

		if (!n->alive || !n->joined) {
			continue;
		}
		if (n->kind == USSLP_NODE_END_DEVICE) {
			continue; /* an end device sleeps; it cannot parent anything */
		}
		if (n->child_count >= max_children) {
			continue;
		}
		if ((int)n->depth + 1 > (int)max_depth) {
			continue;
		}
		if (n->lqi < USSLP_MIN_PARENT_LQI) {
			continue;
		}
		/* Network.bestParentLocked: strongest link, penalised twelve LQI per
		 * level of depth so the tree stays wide rather than deep. */
		score = (int32_t)n->lqi - 12 * (int32_t)n->depth;
		if (score > best_score) {
			best_score = score;
			best = (int)i;
		}
	}
	return best;
}

/* ---------------------------------------------------------------------- */
/* Predictive link failure                                                 */
/* ---------------------------------------------------------------------- */

/*
 * Coefficients from sec/predict.go, converted to Q16.16 by rounding the decimal
 * constants the Go file spells out. They are the decimal values, not the exact
 * rationals they approximate (8.3333, not 25/3), because matching the Go
 * model matters more than matching the algebra it was derived from.
 */
#define Q16_INTERCEPT 546124        /* 8.3333 */
#define Q16_W_LQI (-5461)           /* -0.08333 */
#define Q16_W_TREND (-16384)        /* -0.25 */
#define Q16_W_RSSI_SD 5243          /* 0.08 */
#define Q16_W_BATTERY_DEFICIT 78643 /* 1.2 */
#define Q16_W_DEPTH 9830            /* 0.15 */

usslp_q16 usslp_failure_risk(const struct usslp_link_features *f)
{
	int64_t z;

	/* Accumulate in int64 Q16.16 so that a large LQI times a small weight does
	 * not lose the bits an intermediate usslp_q16_mul would round away. */
	z = (int64_t)Q16_INTERCEPT;
	z += ((int64_t)Q16_W_LQI * (int64_t)f->lqi) >> 16;
	z += ((int64_t)Q16_W_TREND * (int64_t)f->lqi_trend_per_minute) >> 16;
	z += ((int64_t)Q16_W_RSSI_SD * (int64_t)f->rssi_stddev) >> 16;
	z += ((int64_t)Q16_W_BATTERY_DEFICIT * (int64_t)(USSLP_Q16_ONE - f->battery_fraction)) >> 16;
	z += ((int64_t)Q16_W_DEPTH * (int64_t)f->depth) >> 16;

	if (z > INT32_MAX) {
		z = INT32_MAX;
	}
	if (z < INT32_MIN) {
		z = INT32_MIN;
	}
	return usslp_q16_logistic((usslp_q16)z);
}

void usslp_link_history_init(struct usslp_link_history *h)
{
	memset(h, 0, sizeof(*h));
}

void usslp_link_history_add(struct usslp_link_history *h, int32_t at_s, usslp_q16 lqi,
			    usslp_q16 rssi)
{
	if (h->count == USSLP_LINK_HISTORY) {
		memmove(&h->at_s[0], &h->at_s[1], sizeof(h->at_s[0]) * (USSLP_LINK_HISTORY - 1));
		memmove(&h->lqi[0], &h->lqi[1], sizeof(h->lqi[0]) * (USSLP_LINK_HISTORY - 1));
		memmove(&h->rssi[0], &h->rssi[1], sizeof(h->rssi[0]) * (USSLP_LINK_HISTORY - 1));
		h->count--;
	}
	h->at_s[h->count] = at_s;
	h->lqi[h->count] = lqi;
	h->rssi[h->count] = rssi;
	h->count++;
}

bool usslp_link_trend(const struct usslp_link_history *h, usslp_q16 *slope_per_min,
		      usslp_q16 *stderr_per_min)
{
	int n = (int)h->count;
	int64_t sx = 0, sy = 0, sxx = 0, sxy = 0;
	int64_t den, slope_num;
	usslp_q16 xs[USSLP_LINK_HISTORY];
	usslp_q16 slope, intercept;
	int64_t sse = 0, sxx_centred = 0, mean_x;

	if (n < 3) {
		return false;
	}
	/* x in minutes since the first sample, Q16.16. */
	for (int i = 0; i < n; i++) {
		int64_t secs = (int64_t)h->at_s[i] - (int64_t)h->at_s[0];

		xs[i] = (usslp_q16)usslp_round_div(secs * USSLP_Q16_ONE, 60);
		sx += xs[i];
		sy += h->lqi[i];
		sxx += ((int64_t)xs[i] * (int64_t)xs[i]) >> 16;
		sxy += ((int64_t)xs[i] * (int64_t)h->lqi[i]) >> 16;
	}
	den = (int64_t)n * sxx - ((sx * sx) >> 16);
	if (den == 0) {
		return false; /* every sample at the same instant: no slope exists */
	}
	slope_num = (int64_t)n * sxy - ((sx * sy) >> 16);
	slope = (usslp_q16)usslp_round_div(slope_num * (int64_t)USSLP_Q16_ONE, den);
	intercept = (usslp_q16)usslp_round_div((sy - (((int64_t)slope * sx) >> 16)), n);

	mean_x = usslp_round_div(sx, n);
	for (int i = 0; i < n; i++) {
		int64_t fit = (int64_t)intercept + (((int64_t)slope * (int64_t)xs[i]) >> 16);
		int64_t r = (int64_t)h->lqi[i] - fit;
		int64_t d = (int64_t)xs[i] - mean_x;

		sse += (r * r) >> 16;
		sxx_centred += (d * d) >> 16;
	}
	if (sxx_centred <= 0) {
		return false;
	}
	if (slope_per_min != NULL) {
		*slope_per_min = slope;
	}
	if (stderr_per_min != NULL) {
		/* stderr = sqrt(sse / (n-2) / sxx_centred), floored at 0.1.
		 *
		 * The floor is not cosmetic: a perfectly collinear fit has no residual
		 * and would report infinite certainty, which makes the significance
		 * test below vacuous. "Infinitely certain" is never true of a radio
		 * measurement. */
		int64_t num = usslp_round_div(sse, n - 2);
		usslp_q16 var =
			(usslp_q16)usslp_round_div(num * (int64_t)USSLP_Q16_ONE, sxx_centred);
		usslp_q16 se = usslp_q16_sqrt(var);

		*stderr_per_min = se + (USSLP_Q16_ONE / 10);
	}
	return true;
}

usslp_q16 usslp_link_rssi_stddev(const struct usslp_link_history *h)
{
	int n = (int)h->count;
	int64_t sum = 0, ss = 0, mean;
	usslp_q16 var;

	if (n < 2) {
		return 0;
	}
	for (int i = 0; i < n; i++) {
		sum += h->rssi[i];
	}
	mean = usslp_round_div(sum, n);
	for (int i = 0; i < n; i++) {
		int64_t d = (int64_t)h->rssi[i] - mean;

		ss += (d * d) >> 16;
	}
	var = (usslp_q16)usslp_round_div(ss, n - 1);
	return usslp_q16_sqrt(var);
}

struct usslp_link_assessment usslp_assess_link(enum usslp_heal_mode mode,
					       const struct usslp_link_history *h,
					       usslp_q16 battery_fraction, uint8_t depth,
					       usslp_q16 risk_threshold)
{
	struct usslp_link_assessment a;
	usslp_q16 slope = 0, se = 0;

	memset(&a, 0, sizeof(a));
	a.why = "";
	a.features.lqi = (h->count > 0) ? h->lqi[h->count - 1] : 0;
	if (usslp_link_trend(h, &slope, &se)) {
		a.features.lqi_trend_per_minute = slope;
	}
	a.features.rssi_stddev = usslp_link_rssi_stddev(h);
	a.features.battery_fraction = battery_fraction;
	a.features.depth = (usslp_q16)((int32_t)depth * USSLP_Q16_ONE);
	a.risk = usslp_failure_risk(&a.features);

	if (mode == USSLP_HEAL_OFF) {
		return a;
	}
	/* The reactive rule is always armed, in both modes. */
	if (a.features.lqi < (usslp_q16)(USSLP_REROUTE_THRESHOLD * USSLP_Q16_ONE)) {
		a.act = true;
		a.why = "link quality below the reroute threshold";
		return a;
	}
	if (mode == USSLP_HEAL_PREDICTIVE && h->count >= 3 && a.risk >= risk_threshold) {
		if (usslp_link_trend(h, &slope, &se)) {
			int64_t significance = -(((int64_t)USSLP_TREND_SIGNIFICANCE_Q16 *
						  (int64_t)se) >> 16);

			if (slope <= USSLP_MIN_DEGRADATION_TREND_Q16 &&
			    (int64_t)slope <= significance) {
				a.act = true;
				a.why = "predicted to degrade below the threshold within "
					"the horizon";
			}
		}
	}
	return a;
}
