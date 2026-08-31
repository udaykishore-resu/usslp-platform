/*
 * The energy budget against labelsim.PowerProfile.Project.
 *
 * Eighteen configurations: three panels, three shelf temperatures, and both the
 * adaptive profile and the blueprint read literally. Every per-component
 * current is asserted to within a nanoamp of Go's float64 result, which is a
 * part in a thousand of the smallest term in the budget.
 *
 * The suite also asserts the two facts the README leans on, so that they cannot
 * quietly stop being true: adaptive duty cycling reaches the target band, and a
 * literal 250 ms beacon interval does not reach three months.
 */

#include "../src/power/usslp_budget.h"
#include "test_util.h"
#include "test_vectors.h"

static double ua(uint32_t na)
{
	return (double)na / 1000.0;
}

void test_power(void)
{
	struct usslp_power_profile adaptive, always_fast;
	struct usslp_workload w;
	struct usslp_projection pr;

	printf("test_power\n");

	usslp_power_profile_default(&adaptive);
	usslp_power_profile_always_fast(&always_fast);

	TEST("temperature derating matches labelsim.CapacityDerating");
	CHECK_EQ_I(usslp_capacity_derating_ppm(250), 1000000);
	CHECK_EQ_I(usslp_capacity_derating_ppm(200), 1000000);
	CHECK_EQ_I(usslp_capacity_derating_ppm(100), 925000);
	CHECK_EQ_I(usslp_capacity_derating_ppm(40), 880000);
	CHECK_EQ_I(usslp_capacity_derating_ppm(0), 850000);
	CHECK_EQ_I(usslp_capacity_derating_ppm(-100), 775000);
	CHECK_EQ_I(usslp_capacity_derating_ppm(-200), 700000);
	CHECK_EQ_I(usslp_capacity_derating_ppm(-300), 700000);

	TEST("the projection matches the Go model on every configuration");
	for (unsigned i = 0; i < USSLP_TEST_POWER_N; i++) {
		const struct power_vector *v = &power_vectors[i];
		const struct usslp_power_profile *p =
			strcmp(v->profile, "adaptive") == 0 ? &adaptive : &always_fast;

		usslp_workload_default(&w);
		w.ambient_deci_c = (int32_t)(v->ambient_c * 10.0);
		usslp_project(p, (enum usslp_display_tier)v->tier, &w, &pr);

		CHECK_NEAR(ua(pr.sleep_na), v->sleep_ua, 0.001);
		CHECK_NEAR(ua(pr.beacon_na), v->beacon_ua, 0.001);
		CHECK_NEAR(ua(pr.data_rx_na), v->rx_ua, 0.001);
		CHECK_NEAR(ua(pr.refresh_na), v->refresh_ua, 0.001);
		CHECK_NEAR(ua(pr.tx_na), v->tx_ua, 0.001);
		CHECK_NEAR(ua(pr.nfc_na), v->nfc_ua, 0.001);
		CHECK_NEAR(ua(pr.self_discharge_na), v->self_ua, 0.001);
		CHECK_NEAR(ua(pr.total_na), v->total_ua, 0.005);
		CHECK_NEAR((double)pr.usable_capacity_uah / 1000.0, v->usable_mah, 0.001);
		CHECK_NEAR((double)pr.life_milliyears / 1000.0, v->years, 0.005);
		CHECK_NEAR((double)pr.fast_fraction_ppm / 1e6, v->fast_fraction, 1e-5);
	}

	TEST("adaptive duty cycling reaches the seven-to-ten-year band");
	usslp_workload_default(&w);
	usslp_project(&adaptive, USSLP_TIER_29_BWR, &w, &pr);
	CHECK(pr.meets_target);
	CHECK(pr.life_milliyears >= 8000 && pr.life_milliyears <= 9000);
	/* And the breakdown says where the charge goes, which is the number an
	 * engineer needs rather than the aggregate. */
	CHECK(pr.beacon_na > pr.refresh_na);
	CHECK(pr.beacon_na < pr.total_na / 2 + pr.total_na / 10);

	TEST("a literal 250 ms beacon interval gives about ninety-nine days");
	usslp_project(&always_fast, USSLP_TIER_29_BWR, &w, &pr);
	CHECK(!pr.meets_target);
	/* 0.270 years is 98.6 days. The blueprint's 250 ms figure and its
	 * seven-to-ten-year figure do not fit in the same sentence. */
	CHECK(pr.life_milliyears > 260 && pr.life_milliyears < 280);
	CHECK(pr.beacon_na > 200000u);

	TEST("a freezer aisle misses the target and the model says so");
	usslp_workload_default(&w);
	w.ambient_deci_c = -200;
	usslp_project(&adaptive, USSLP_TIER_29_BWR, &w, &pr);
	CHECK(!pr.meets_target);
	CHECK(pr.life_milliyears > 6000 && pr.life_milliyears < 7000);

	TEST("the colour panel cannot run this workload on a coin cell");
	/* A 15-second waveform at 35 mA, ten times a day, is 60 uA on its own —
	 * nine times the whole rest of the budget. The 5.85-inch panel is a
	 * mains-powered or infrequently-updated fitting, and a planogram that puts
	 * one on a battery label with ten changes a day is a mistake the firmware
	 * should be able to report rather than discover. */
	usslp_workload_default(&w);
	usslp_project(&adaptive, USSLP_TIER_585_ACEP, &w, &pr);
	CHECK(!pr.meets_target);
	CHECK(pr.life_milliyears < 1000);
	CHECK(pr.refresh_na > 50000u);

	TEST("the colour panel is viable at a realistic promotional cadence");
	usslp_workload_default(&w);
	w.updates_per_day_milli = 1000; /* one change a day */
	w.telemetry_per_day_milli = 48000; /* one report every half hour */
	usslp_project(&adaptive, USSLP_TIER_585_ACEP, &w, &pr);
	CHECK(pr.life_milliyears > 4000);

	TEST("the partial fraction is capped by the panel's own ghosting budget");
	{
		struct usslp_projection honest, greedy;

		usslp_workload_default(&w);
		usslp_project(&adaptive, USSLP_TIER_29_BWR, &w, &honest);
		w.partial_fraction_ppm = 1000000; /* "every update is partial" */
		usslp_project(&adaptive, USSLP_TIER_29_BWR, &w, &greedy);
		/* Believing the caller would produce a battery life the hardware cannot
		 * deliver; the cap is 8/9. */
		CHECK(greedy.refresh_na > 0);
		CHECK(greedy.refresh_na < honest.refresh_na);
		/* The cap is 8/9, giving 0.8889*300 + 0.1111*1500 = 433 ms average and
		 * 1.304 uA rather than the 0.903 uA an uncapped 100% would claim. */
		CHECK_NEAR(ua(greedy.refresh_na), 1.304, 0.005);
	}

	TEST("the sustainable beacon interval is the inverse of the projection");
	{
		uint32_t interval;

		usslp_workload_default(&w);
		interval = usslp_sustainable_beacon_us(&adaptive, USSLP_TIER_29_BWR, &w, 8000);
		/* The Go model gives 13.727 s for eight years on the 2.9-inch panel. */
		CHECK(interval > 13600000u && interval < 13900000u);

		/* Feeding it back in as a constant interval should land on the target. */
		{
			struct usslp_power_profile check = adaptive;
			struct usslp_projection back;

			check.beacon_fast_us = interval;
			check.beacon_slow_us = interval;
			check.active_window_us = 0;
			usslp_project(&check, USSLP_TIER_29_BWR, &w, &back);
			CHECK_NEAR((double)back.life_milliyears, 8000.0, 40.0);
		}

		/* A workload whose non-listening terms already exceed the target admits
		 * no interval at all, and says so rather than returning something
		 * plausible. */
		usslp_workload_default(&w);
		CHECK_EQ_I(usslp_sustainable_beacon_us(&adaptive, USSLP_TIER_585_ACEP, &w, 8000),
			   0);
	}

	TEST("the discharge curve is flat then falls off a cliff");
	CHECK_EQ_I(usslp_battery_mv(0), 3050);
	CHECK_EQ_I(usslp_battery_mv(1000000), 1800);
	/* 3050 - 250*0.25 = 2987.5, and Go rounds half away from zero. */
	CHECK_EQ_I(usslp_battery_mv(500000), 2988);
	CHECK_EQ_I(usslp_battery_mv(900000), 1800 + (2848 - 1800));
	CHECK_EQ_I(usslp_battery_mv(950000), 2324);
	{
		/* Monotonic: an alert threshold on voltage has to mean one state of
		 * charge, not two. */
		uint16_t prev = 4000;

		for (uint32_t d = 0; d <= 1000000u; d += 1000u) {
			uint16_t mv = usslp_battery_mv(d);

			CHECK(mv <= prev);
			prev = mv;
		}
	}
}
