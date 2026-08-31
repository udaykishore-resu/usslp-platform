/*
 * The fixed-point mesh model against the Go model's numbers.
 *
 * The tolerances are the interesting part. The link metric is integer on both
 * sides and must match exactly. The link-failure model is a logistic evaluated
 * in Q16.16 against Go's float64, and it is asserted to 1e-3 absolute
 * probability — four orders of magnitude finer than any threshold the model
 * feeds, so the quantisation cannot change a routing decision that was not
 * already on a knife edge.
 */

#include "../src/radio/usslp_route.h"
#include "test_util.h"
#include "test_vectors.h"

static double q16_to_d(usslp_q16 v)
{
	return (double)v / 65536.0;
}

static usslp_q16 d_to_q16(double v)
{
	return (usslp_q16)(v * 65536.0 + (v >= 0 ? 0.5 : -0.5));
}

void test_route(void)
{
	printf("test_route\n");

	TEST("LQI from RSSI matches mesh.LQIFromRSSI exactly");
	for (unsigned i = 0; i < USSLP_TEST_ROUTE_N; i++) {
		int32_t centi = (int32_t)(route_rssi[i] * 100.0);

		CHECK_EQ_I(usslp_lqi_from_rssi(centi), route_lqi[i]);
	}
	/* The reroute threshold of 100 lands at about -74.5 dBm, roughly 10 dB above
	 * the effective sensitivity floor. If this drifts, the whole self-healing
	 * story moves with it. */
	CHECK_EQ_I(usslp_lqi_from_rssi(-7450), 100);
	CHECK_EQ_I(usslp_lqi_from_rssi(-20000), 0);
	CHECK_EQ_I(usslp_lqi_from_rssi(0), 255);

	TEST("link cost matches mesh.LinkCost exactly");
	for (unsigned i = 0; i < USSLP_TEST_ROUTE_N; i++) {
		CHECK_EQ_I(usslp_link_cost(route_lqi[i]), route_cost[i]);
	}
	CHECK_EQ_I(usslp_link_cost(0), 7);
	CHECK_EQ_I(usslp_link_cost(-1), 7);
	CHECK_EQ_I(usslp_link_cost(255), 1);
	CHECK_EQ_I(usslp_link_cost(1000), 1);
	/* Every LQI in range must give a cost in 1..7 and be monotonically
	 * non-increasing: a better link is never more expensive. */
	{
		int prev = 8;

		for (int lqi = 1; lqi <= 255; lqi++) {
			int c = usslp_link_cost(lqi);

			CHECK(c >= 1 && c <= 7);
			CHECK(c <= prev);
			prev = c;
		}
	}

	TEST("fragmentation and airtime match mesh.Fragments and mesh.Airtime");
	for (unsigned i = 0; i < USSLP_TEST_AIR_N; i++) {
		CHECK_EQ_I(usslp_fragments(air_bytes[i]), air_frags[i]);
		CHECK_EQ_I(usslp_airtime_us(air_bytes[i]), air_us[i]);
	}
	CHECK_EQ_I(usslp_fragments(0), 1);
	CHECK_EQ_I(usslp_fragments(-1), 1);

	TEST("the routing penalties are applied");
	{
		struct usslp_neighbour n;

		memset(&n, 0, sizeof(n));
		n.alive = true;
		n.joined = true;
		n.kind = USSLP_NODE_ROUTER;
		n.lqi = 255;
		n.battery_pct = 90;
		CHECK_EQ_I(usslp_next_hop_cost(&n), 1);

		n.avoided = true;
		CHECK_EQ_I(usslp_next_hop_cost(&n), 1 + USSLP_AVOID_PENALTY);
		n.avoided = false;

		n.battery_pct = 10;
		CHECK_EQ_I(usslp_next_hop_cost(&n), 1 + USSLP_LOW_BATTERY_PENALTY);
		/* An end device is never on a route, so its battery is irrelevant. */
		n.kind = USSLP_NODE_END_DEVICE;
		CHECK_EQ_I(usslp_next_hop_cost(&n), 1);

		n.kind = USSLP_NODE_ROUTER;
		n.battery_pct = 90;
		n.lqi = 19;
		CHECK_EQ_I(usslp_next_hop_cost(&n), INT32_MAX);
		n.lqi = 20;
		CHECK(usslp_next_hop_cost(&n) < INT32_MAX);
		n.alive = false;
		CHECK_EQ_I(usslp_next_hop_cost(&n), INT32_MAX);
		n.alive = true;
		n.joined = false;
		CHECK_EQ_I(usslp_next_hop_cost(&n), INT32_MAX);
	}

	TEST("parent selection prefers the strongest link, shallower on a tie");
	{
		struct usslp_neighbour table[4];

		memset(table, 0, sizeof(table));
		for (unsigned i = 0; i < 4; i++) {
			table[i].alive = true;
			table[i].joined = true;
			table[i].kind = USSLP_NODE_ROUTER;
			table[i].battery_pct = 100;
		}
		table[0].lqi = 120;
		table[0].depth = 1;
		table[1].lqi = 180;
		table[1].depth = 1;
		table[2].lqi = 190;
		table[2].depth = 3; /* 190 - 36 = 154, worse than 180 - 12 = 168 */
		table[3].lqi = 200;
		table[3].depth = 1;
		table[3].child_count = USSLP_DEFAULT_MAX_CHILDREN; /* full */
		CHECK_EQ_I(usslp_select_parent(table, 4, USSLP_DEFAULT_MAX_CHILDREN,
					       USSLP_DEFAULT_MAX_DEPTH),
			   1);

		/* An end device cannot parent anything: it sleeps. */
		table[1].kind = USSLP_NODE_END_DEVICE;
		CHECK_EQ_I(usslp_select_parent(table, 4, USSLP_DEFAULT_MAX_CHILDREN,
					       USSLP_DEFAULT_MAX_DEPTH),
			   2);

		/* Below LQI 40 the association exchange itself would not complete. */
		for (unsigned i = 0; i < 4; i++) {
			table[i].lqi = 39;
			table[i].kind = USSLP_NODE_ROUTER;
			table[i].child_count = 0;
		}
		CHECK_EQ_I(usslp_select_parent(table, 4, USSLP_DEFAULT_MAX_CHILDREN,
					       USSLP_DEFAULT_MAX_DEPTH),
			   -1);

		/* Nothing at the network radius may be extended. */
		for (unsigned i = 0; i < 4; i++) {
			table[i].lqi = 200;
			table[i].depth = USSLP_DEFAULT_MAX_DEPTH;
		}
		CHECK_EQ_I(usslp_select_parent(table, 4, USSLP_DEFAULT_MAX_CHILDREN,
					       USSLP_DEFAULT_MAX_DEPTH),
			   -1);
	}

	TEST("the fixed-point logistic tracks the Go model");
	for (unsigned i = 0; i < USSLP_TEST_RISK_N; i++) {
		const struct risk_vector *v = &risk_vectors[i];
		struct usslp_link_features f = {
			.lqi = d_to_q16(v->lqi),
			.lqi_trend_per_minute = d_to_q16(v->trend),
			.rssi_stddev = d_to_q16(v->rssi_sd),
			.battery_fraction = d_to_q16(v->battery),
			.depth = d_to_q16(v->depth),
		};

		CHECK_NEAR(q16_to_d(usslp_failure_risk(&f)), v->risk, 1e-3);
	}

	TEST("the logistic saturates cleanly rather than wrapping");
	CHECK_EQ_I(usslp_q16_logistic(d_to_q16(40.0)), USSLP_Q16_ONE);
	CHECK_EQ_I(usslp_q16_logistic(d_to_q16(-40.0)), 0);
	CHECK_NEAR(q16_to_d(usslp_q16_logistic(0)), 0.5, 1e-4);
	{
		/* Monotonic across the whole usable range: a worse link is never
		 * assessed as safer. */
		usslp_q16 prev = -1;

		for (int z = -16; z <= 16; z++) {
			usslp_q16 v = usslp_q16_logistic(d_to_q16((double)z));

			CHECK(v >= prev);
			prev = v;
		}
	}

	TEST("fixed-point square root");
	CHECK_NEAR(q16_to_d(usslp_q16_sqrt(d_to_q16(4.0))), 2.0, 1e-3);
	CHECK_NEAR(q16_to_d(usslp_q16_sqrt(d_to_q16(2.0))), 1.4142136, 1e-3);
	CHECK_NEAR(q16_to_d(usslp_q16_sqrt(d_to_q16(0.25))), 0.5, 1e-3);
	CHECK_EQ_I(usslp_q16_sqrt(0), 0);
	CHECK_EQ_I(usslp_q16_sqrt(-5), 0);

	TEST("the least-squares trend fit matches a double-precision fit");
	{
		/* A clean ramp: LQI falling 6 per minute over five samples 30 s apart. */
		struct usslp_link_history h;
		usslp_q16 slope, se;
		double dx[5], dy[5];
		double sx = 0, sy = 0, sxx = 0, sxy = 0, want_slope;

		usslp_link_history_init(&h);
		for (int i = 0; i < 5; i++) {
			double lqi = 160.0 - 3.0 * i;

			usslp_link_history_add(&h, i * 30, d_to_q16(lqi), d_to_q16(-70.0));
			dx[i] = (double)(i * 30) / 60.0;
			dy[i] = lqi;
			sx += dx[i];
			sy += dy[i];
			sxx += dx[i] * dx[i];
			sxy += dx[i] * dy[i];
		}
		want_slope = (5 * sxy - sx * sy) / (5 * sxx - sx * sx);
		CHECK(usslp_link_trend(&h, &slope, &se));
		CHECK_NEAR(q16_to_d(slope), want_slope, 0.01);
		CHECK_NEAR(q16_to_d(slope), -6.0, 0.01);
		/* A perfect fit has no residual, so the error is the floor. */
		CHECK_NEAR(q16_to_d(se), 0.1, 0.01);
	}

	TEST("fewer than three samples make no trend claim");
	{
		struct usslp_link_history h;
		usslp_q16 slope, se;

		usslp_link_history_init(&h);
		CHECK(!usslp_link_trend(&h, &slope, &se));
		usslp_link_history_add(&h, 0, d_to_q16(150), d_to_q16(-70));
		CHECK(!usslp_link_trend(&h, &slope, &se));
		usslp_link_history_add(&h, 30, d_to_q16(140), d_to_q16(-71));
		CHECK(!usslp_link_trend(&h, &slope, &se));
		usslp_link_history_add(&h, 60, d_to_q16(130), d_to_q16(-72));
		CHECK(usslp_link_trend(&h, &slope, &se));
	}

	TEST("the history ring keeps the newest ten samples");
	{
		struct usslp_link_history h;

		usslp_link_history_init(&h);
		for (int i = 0; i < 25; i++) {
			usslp_link_history_add(&h, i * 30, d_to_q16(100 + i), d_to_q16(-70));
		}
		CHECK_EQ_I(h.count, USSLP_LINK_HISTORY);
		CHECK_NEAR(q16_to_d(h.lqi[USSLP_LINK_HISTORY - 1]), 124.0, 1e-3);
		CHECK_NEAR(q16_to_d(h.lqi[0]), 115.0, 1e-3);
	}

	TEST("RSSI standard deviation");
	{
		struct usslp_link_history h;

		usslp_link_history_init(&h);
		/* -70, -74, -66, -70: mean -70, sample sd = sqrt((0+16+16+0)/3) */
		usslp_link_history_add(&h, 0, d_to_q16(150), d_to_q16(-70));
		usslp_link_history_add(&h, 30, d_to_q16(150), d_to_q16(-74));
		usslp_link_history_add(&h, 60, d_to_q16(150), d_to_q16(-66));
		usslp_link_history_add(&h, 90, d_to_q16(150), d_to_q16(-70));
		CHECK_NEAR(q16_to_d(usslp_link_rssi_stddev(&h)), 3.265986, 0.01);
	}

	TEST("the reactive rule fires below the threshold in every mode but off");
	{
		struct usslp_link_history h;
		struct usslp_link_assessment a;

		usslp_link_history_init(&h);
		for (int i = 0; i < 4; i++) {
			usslp_link_history_add(&h, i * 30, d_to_q16(80), d_to_q16(-80));
		}
		a = usslp_assess_link(USSLP_HEAL_PREDICTIVE, &h, d_to_q16(1.0), 1,
				      d_to_q16(0.7));
		CHECK(a.act);
		a = usslp_assess_link(USSLP_HEAL_REACTIVE, &h, d_to_q16(1.0), 1, d_to_q16(0.7));
		CHECK(a.act);
		a = usslp_assess_link(USSLP_HEAL_OFF, &h, d_to_q16(1.0), 1, d_to_q16(0.7));
		CHECK(!a.act);
	}

	TEST("prediction fires on a link that is moving and not on one that is merely poor");
	{
		struct usslp_link_history moving, flat;
		struct usslp_link_assessment a;

		/* Falling 10 LQI a minute from 160: crosses 100 in six minutes and the
		 * slope is far outside measurement noise. */
		usslp_link_history_init(&moving);
		for (int i = 0; i < 6; i++) {
			usslp_link_history_add(&moving, i * 30, d_to_q16(160.0 - 5.0 * i),
					       d_to_q16(-70.0 - 1.5 * i));
		}
		a = usslp_assess_link(USSLP_HEAL_PREDICTIVE, &moving, d_to_q16(1.0), 2,
				      d_to_q16(0.5));
		CHECK(a.act);
		/* Reactive mode ignores it: the link is still above the threshold. This
		 * is the difference the predictive healer exists to make. */
		a = usslp_assess_link(USSLP_HEAL_REACTIVE, &moving, d_to_q16(1.0), 2,
				      d_to_q16(0.5));
		CHECK(!a.act);

		/* A link sitting at 105 with a flat trend and a little noise. A good
		 * proportion of a real store's links look like this, and rerouting them
		 * all gains nothing. */
		usslp_link_history_init(&flat);
		for (int i = 0; i < 10; i++) {
			double jitter = ((i % 3) - 1) * 2.0;

			usslp_link_history_add(&flat, i * 30, d_to_q16(105.0 + jitter),
					       d_to_q16(-74.0 + jitter * 0.3));
		}
		a = usslp_assess_link(USSLP_HEAL_PREDICTIVE, &flat, d_to_q16(1.0), 2,
				      d_to_q16(0.5));
		CHECK(!a.act);
	}
}
