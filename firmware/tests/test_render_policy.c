/*
 * Partial versus full refresh, and the ghosting budget.
 */

#include "../src/display/usslp_render_policy.h"
#include "test_util.h"

void test_render_policy(void)
{
	struct usslp_ghost_state g;
	struct usslp_refresh_plan p;

	printf("test_render_policy\n");

	TEST("panel specifications match labelsim's catalogue");
	{
		const struct usslp_panel_spec *d = usslp_panel(USSLP_TIER_29_BWR);

		CHECK_EQ_I(d->width, 296);
		CHECK_EQ_I(d->height, 128);
		CHECK_EQ_I(d->full_refresh_ms, 1500);
		CHECK_EQ_I(d->partial_refresh_ms, 300);
		CHECK_EQ_I(d->max_partials, 8);
		CHECK_EQ_I(d->refresh_current_ua, 26000);
		CHECK(d->supports_partial);

		d = usslp_panel(USSLP_TIER_42_BW);
		CHECK_EQ_I(d->width, 400);
		CHECK_EQ_I(d->full_refresh_ms, 2000);
		CHECK_EQ_I(d->refresh_current_ua, 30000);

		d = usslp_panel(USSLP_TIER_585_ACEP);
		CHECK_EQ_I(d->width, 600);
		CHECK_EQ_I(d->height, 448);
		CHECK_EQ_I(d->colors, 7);
		CHECK_EQ_I(d->full_refresh_ms, 15000);
		CHECK(!d->supports_partial);
		/* An out-of-range tier resolves to the volume product rather than
		 * indexing off the end of the table. */
		CHECK(usslp_panel((enum usslp_display_tier)99) == usslp_panel(USSLP_TIER_29_BWR));
	}

	TEST("a full refresh is used when none was requested");
	usslp_ghost_init(&g);
	p = usslp_plan_refresh(USSLP_TIER_29_BWR, false, &g);
	CHECK(!p.partial);
	CHECK(!p.forced_full);
	CHECK_EQ_I(p.duration_ms, 1500);

	TEST("a requested partial is honoured while budget remains");
	p = usslp_plan_refresh(USSLP_TIER_29_BWR, true, &g);
	CHECK(p.partial);
	CHECK_EQ_I(p.duration_ms, 300);

	TEST("the ghosting budget forces a full refresh on the ninth partial");
	usslp_ghost_init(&g);
	for (unsigned i = 0; i < 8; i++) {
		p = usslp_plan_refresh(USSLP_TIER_29_BWR, true, &g);
		CHECK(p.partial);
		CHECK(!p.forced_full);
		usslp_ghost_apply(&g, &p);
	}
	CHECK_EQ_I(g.partials_since_full, 8);
	p = usslp_plan_refresh(USSLP_TIER_29_BWR, true, &g);
	CHECK(!p.partial);
	CHECK(p.forced_full);
	CHECK_EQ_I(p.duration_ms, 1500);
	usslp_ghost_apply(&g, &p);
	CHECK_EQ_I(g.partials_since_full, 0);
	/* And the budget is available again. */
	p = usslp_plan_refresh(USSLP_TIER_29_BWR, true, &g);
	CHECK(p.partial);

	TEST("a refresh that did not happen does not spend ghosting budget");
	/* The attestation failed or the sequence was stale: nothing reached the
	 * glass, so the counter must not move. */
	usslp_ghost_init(&g);
	(void)usslp_plan_refresh(USSLP_TIER_29_BWR, true, &g);
	(void)usslp_plan_refresh(USSLP_TIER_29_BWR, true, &g);
	CHECK_EQ_I(g.partials_since_full, 0);

	TEST("the colour panel never runs a partial, whatever is asked");
	usslp_ghost_init(&g);
	for (unsigned i = 0; i < 12; i++) {
		p = usslp_plan_refresh(USSLP_TIER_585_ACEP, true, &g);
		CHECK(!p.partial);
		CHECK(!p.forced_full); /* it was never eligible, so nothing was forced */
		CHECK_EQ_I(p.duration_ms, 15000);
		usslp_ghost_apply(&g, &p);
		CHECK_EQ_I(g.partials_since_full, 0);
	}

	TEST("domain.partialSafe: the first price a label shows is always full");
	{
		struct usslp_partial_safety_input in;

		memset(&in, 0, sizeof(in));
		in.full_refresh_every = 8;
		in.prev_has_price = false;
		in.price_width = 5;
		in.prev_price_width = 5;
		CHECK(!usslp_partial_safe(&in));

		in.prev_has_price = true;
		CHECK(usslp_partial_safe(&in));
	}

	TEST("domain.partialSafe: any layout change forces a full refresh");
	{
		struct usslp_partial_safety_input base;

		memset(&base, 0, sizeof(base));
		base.prev_has_price = true;
		base.full_refresh_every = 8;
		base.price_width = 5;
		base.prev_price_width = 5;
		CHECK(usslp_partial_safe(&base));

#define ONE(field)                                                                                 \
	do {                                                                                       \
		struct usslp_partial_safety_input in = base;                                       \
		in.field = true;                                                                   \
		CHECK(!usslp_partial_safe(&in));                                                   \
	} while (0)
		ONE(template_changed);
		ONE(badge_changed);
		ONE(led_changed);
		ONE(show_was_changed);
		ONE(currency_changed);
		/* Re-publishing an identical price must redraw from a known state
		 * rather than partially rewriting nothing. */
		ONE(price_unchanged);
#undef ONE

		/* 9.99 to 10.99 re-lays out the digit field. */
		{
			struct usslp_partial_safety_input in = base;

			in.price_width = 6;
			CHECK(!usslp_partial_safe(&in));
		}
		/* The periodic clear. */
		{
			struct usslp_partial_safety_input in = base;

			in.prev_partials_since_full = 7;
			CHECK(usslp_partial_safe(&in));
			in.prev_partials_since_full = 8;
			CHECK(!usslp_partial_safe(&in));
			in.prev_partials_since_full = 0;
			in.full_refresh_every = 0;
			CHECK(!usslp_partial_safe(&in));
		}
	}
}
