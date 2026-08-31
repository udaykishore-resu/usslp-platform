/*
 * usslp_render_policy.h - partial versus full refresh, and the ghosting counter.
 *
 * Two policies meet here and they are not the same policy.
 *
 * The cloud's policy is domain.DecideRender in
 * platform/internal/label/domain/render.go. It decides whether a partial
 * refresh is *offered*, based on what changed: the digits moved but the layout
 * did not, the rendered width is the same, the currency is the same, and fewer
 * than Policy.FullRefreshEvery partials have run since the last full one. The
 * result travels to the label as FlagRequestPartial in the air frame.
 *
 * The label's policy is labelsim.planRefresh, and it is the one implemented
 * here. It treats the flag as a *request, not an instruction*:
 *
 *   - if the controller did not ask for partial, run a full waveform;
 *   - if the panel cannot do partial at all (the 5.85" seven-colour panel
 *     sequences its pigments through the whole stack and has no shortened
 *     form), run a full waveform;
 *   - if the ghosting budget is spent, run a full waveform and tell the
 *     controller it happened.
 *
 * The label has the last word because the label is the only party that knows how
 * many partials have actually reached the glass. The controller's count is an
 * estimate that a lost frame, a reboot or a manual refresh invalidates, and a
 * disagreement in the controller's favour means a shopper can read the previous
 * price ghosted behind the current one. That is a weights-and-measures problem,
 * not a cosmetic one.
 *
 * ForcedFull is reported upward in the ack because a controller that keeps
 * triggering it has mis-estimated its diff threshold and is spending five times
 * the energy it planned to. That number is the fastest way to find the mistake.
 */

#ifndef USSLP_RENDER_POLICY_H
#define USSLP_RENDER_POLICY_H

#include "../usslp_portable.h"

/* The three panels in the USSLP hardware range, numbered as labelsim.DisplayTier
 * so that a tier in a planogram, in the simulator and in this firmware are the
 * same integer. */
enum usslp_display_tier {
	USSLP_TIER_29_BWR = 0,   /* 2.9" 296x128 black/white/red */
	USSLP_TIER_42_BW = 1,    /* 4.2" 400x300 black/white */
	USSLP_TIER_585_ACEP = 2, /* 5.85" 600x448 seven colour */
	USSLP_TIER_COUNT = 3,
};

/* The physical behaviour of one panel. Every figure is labelsim.DisplaySpec's,
 * because the simulator's battery and latency numbers are the ones the platform
 * publishes and firmware that disagreed with them would make those numbers
 * fiction. */
struct usslp_panel_spec {
	const char *name;
	uint16_t width;
	uint16_t height;
	uint8_t colors;
	uint16_t full_refresh_ms;
	uint16_t partial_refresh_ms;
	bool supports_partial;
	/* How many partial refreshes may run before a full one is forced. Beyond it
	 * the residue is readable, and on a price label the residue is a price. */
	uint8_t max_partials;
	/* Average draw while the waveform runs, in microamps. The charge pump
	 * driving the +/-15 V rails into a capacitive panel is the largest single
	 * current the device ever draws. */
	uint32_t refresh_current_ua;
	/* Uncompressed framebuffer size at one byte per pixel. */
	uint32_t image_bytes;
};

const struct usslp_panel_spec *usslp_panel(enum usslp_display_tier tier);

/* The refresh decision. */
struct usslp_refresh_plan {
	bool partial;
	uint16_t duration_ms;
	/* A partial was requested and a full ran anyway to clear ghosting. */
	bool forced_full;
};

/* The ghosting counter. It is persisted alongside the sequence, because a label
 * that reboots with the counter cleared will happily run another full budget of
 * partials on a panel that is already carrying residue. */
struct usslp_ghost_state {
	uint8_t partials_since_full;
};

void usslp_ghost_init(struct usslp_ghost_state *g);

/*
 * Decides how to draw. Pure, and the reference is labelsim.planRefresh: it
 * takes the flag as advisory, checks the panel, checks the budget.
 */
struct usslp_refresh_plan usslp_plan_refresh(enum usslp_display_tier tier, bool request_partial,
					     const struct usslp_ghost_state *g);

/* Records that a plan was executed, advancing or clearing the ghosting counter.
 * Split from usslp_plan_refresh so that a caller that decides not to render —
 * because the attestation failed, or the sequence was stale — cannot spend
 * ghosting budget it did not use. */
void usslp_ghost_apply(struct usslp_ghost_state *g, const struct usslp_refresh_plan *plan);

/*
 * The controller-side half of the policy, implemented on the label as well
 * because a mains-powered relay renders for its children when the mesh is
 * partitioned from the controller, and because the label uses it to sanity-check
 * a request: a controller that asks for a partial when the price width changed
 * has a bug, and the label logs the disagreement.
 *
 * This is domain.partialSafe. price_width and prev_price_width are the widths of
 * the *rendered* price strings, which is what the Go code compares.
 */
struct usslp_partial_safety_input {
	bool prev_has_price;
	uint8_t prev_partials_since_full;
	uint8_t full_refresh_every;
	bool template_changed;
	bool badge_changed;
	bool led_changed;
	bool show_was_changed;
	bool currency_changed;
	bool price_unchanged;
	uint8_t price_width;
	uint8_t prev_price_width;
};

bool usslp_partial_safe(const struct usslp_partial_safety_input *in);

#endif /* USSLP_RENDER_POLICY_H */
