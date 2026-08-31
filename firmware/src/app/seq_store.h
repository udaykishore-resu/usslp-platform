/*
 * seq_store.h - the durable half of the monotonic sequence rule.
 *
 * usslp_seq.h holds the rule; this holds the flash. They are separate files
 * because the rule is portable and tested on the host and the flash is not, and
 * because the interesting property — that the sequence is committed before the
 * waveform — is a property of the *call order* in price.c rather than of either
 * module alone.
 *
 * What is persisted, and what is not
 * ----------------------------------
 * Persisted: the displayed sequence, and the ghosting counter. Both survive a
 * reboot because both are properties of the glass, and the glass survives a
 * reboot: an E-Ink panel is bistable and holds its image with no power at all.
 * A label that came back with a cleared ghosting counter would happily run
 * another full budget of partials on a panel already carrying residue.
 *
 * Not persisted: the accept and discard counters. They are uptime-scoped
 * telemetry, and writing them would turn one flash write per price change into
 * one per frame received — including the duplicates the sequence rule exists to
 * absorb, which on a busy zone is the majority of frames.
 *
 * Write endurance
 * ---------------
 * The nRF52840's flash is rated at 10,000 erase cycles. At the platform's
 * planning workload of 10 price changes a day for ten years that is 36,500
 * writes, which is why this uses NVS rather than a fixed location: NVS wear
 * levels across the sector, so the 24 KiB storage partition at 16 bytes a record
 * spreads those writes over hundreds of pages. A naive "write the same address
 * every time" design would exhaust one page in under three years.
 */

#ifndef USSLP_SEQ_STORE_H
#define USSLP_SEQ_STORE_H

#include "../display/usslp_render_policy.h"
#include "../usslp_portable.h"
#include "usslp_seq.h"

/* Loads the persisted state, or initialises it to never-displayed. The ghosting
 * counter is filled in from the same record. */
int usslp_seq_store_init(struct usslp_ghost_state *ghost);

/* The in-memory state. Never NULL after init. */
const struct usslp_seq_state *usslp_seq_store_state(void);

/* Applies the sequence rule without persisting anything. */
enum usslp_seq_verdict usslp_seq_store_check(int64_t candidate);

/*
 * Commits a sequence and the ghosting counter to flash, and only then returns.
 *
 * Called immediately before the waveform. A failure here must abort the render:
 * displaying a price whose sequence did not persist is how a label ends up
 * showing something it will later reject a correction for.
 */
int usslp_seq_store_commit(int64_t sequence, struct usslp_ghost_state *ghost,
			   const struct usslp_refresh_plan *plan);

/*
 * Resets to never-displayed. Used at decommissioning and by the field
 * engineer's shell command, and it clears the panel as well — a label that has
 * forgotten what it is showing must not keep showing it.
 */
int usslp_seq_store_reset(struct usslp_ghost_state *ghost);

#endif /* USSLP_SEQ_STORE_H */
