#include "usslp_render_policy.h"

/* labelsim's display catalogue, verbatim. */
static const struct usslp_panel_spec panels[USSLP_TIER_COUNT] = {
	[USSLP_TIER_29_BWR] = {
		.name = "2.9in-296x128-BWR",
		.width = 296, .height = 128, .colors = 3,
		.full_refresh_ms = 1500, .partial_refresh_ms = 300,
		.supports_partial = true, .max_partials = 8,
		.refresh_current_ua = 26000,
		.image_bytes = 296u * 128u,
	},
	[USSLP_TIER_42_BW] = {
		.name = "4.2in-400x300-BW",
		.width = 400, .height = 300, .colors = 2,
		.full_refresh_ms = 2000, .partial_refresh_ms = 300,
		.supports_partial = true, .max_partials = 8,
		.refresh_current_ua = 30000,
		.image_bytes = 400u * 300u,
	},
	[USSLP_TIER_585_ACEP] = {
		.name = "5.85in-600x448-7C",
		.width = 600, .height = 448, .colors = 7,
		/* Fifteen seconds, and no partial form at all. The seven-colour
		 * waveform sequences every pigment through the whole stack; there is
		 * nothing to shorten. This is the entire reason the colour panel is not
		 * the default fit. */
		.full_refresh_ms = 15000, .partial_refresh_ms = 15000,
		.supports_partial = false, .max_partials = 0,
		.refresh_current_ua = 35000,
		.image_bytes = 600u * 448u,
	},
};

const struct usslp_panel_spec *usslp_panel(enum usslp_display_tier tier)
{
	if ((unsigned)tier >= (unsigned)USSLP_TIER_COUNT) {
		return &panels[USSLP_TIER_29_BWR];
	}
	return &panels[tier];
}

void usslp_ghost_init(struct usslp_ghost_state *g)
{
	g->partials_since_full = 0;
}

struct usslp_refresh_plan usslp_plan_refresh(enum usslp_display_tier tier, bool request_partial,
					     const struct usslp_ghost_state *g)
{
	const struct usslp_panel_spec *d = usslp_panel(tier);
	struct usslp_refresh_plan plan = { .partial = false,
					   .duration_ms = d->full_refresh_ms,
					   .forced_full = false };

	if (!request_partial || !d->supports_partial) {
		return plan;
	}
	if (g->partials_since_full >= d->max_partials) {
		plan.forced_full = true;
		return plan;
	}
	plan.partial = true;
	plan.duration_ms = d->partial_refresh_ms;
	return plan;
}

void usslp_ghost_apply(struct usslp_ghost_state *g, const struct usslp_refresh_plan *plan)
{
	if (plan->partial) {
		if (g->partials_since_full < 0xffu) {
			g->partials_since_full++;
		}
	} else {
		g->partials_since_full = 0;
	}
}

bool usslp_partial_safe(const struct usslp_partial_safety_input *in)
{
	if (!in->prev_has_price) {
		/* Nothing cached and nothing on the glass: the first price a label
		 * shows is always drawn with a full waveform. */
		return false;
	}
	if (in->full_refresh_every == 0u ||
	    in->prev_partials_since_full >= in->full_refresh_every) {
		return false;
	}
	if (in->template_changed || in->badge_changed || in->led_changed || in->show_was_changed) {
		/* Any of those alters regions outside the price field, and the cached
		 * framebuffer for those regions is no longer valid. */
		return false;
	}
	if (in->currency_changed) {
		/* The symbol moves and therefore the whole field does. */
		return false;
	}
	if (in->price_unchanged) {
		/* Re-publishing an identical price — a controller replacement, a
		 * retained-message rebuild — must redraw from a known state rather than
		 * partially rewriting nothing. */
		return false;
	}
	if (in->price_width != in->prev_price_width) {
		/* 9.99 to 10.99 re-lays out the digit field, and the partial window the
		 * controller computed no longer covers the change. */
		return false;
	}
	return true;
}
