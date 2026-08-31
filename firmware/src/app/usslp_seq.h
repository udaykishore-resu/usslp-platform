/*
 * usslp_seq.h - the monotonic sequence rule (INTERFACE-CONTRACTS section 6).
 *
 * "A label discards any update whose sequence is not greater than the one it is
 * displaying." That one sentence is what makes at-least-once mesh delivery safe:
 * a duplicated frame is a no-op, and a reordered one cannot roll a price
 * backwards. edge/labelsim/label.go implements the same rule in the simulator.
 *
 * The part the simulator does not have to worry about and this firmware does:
 * it has to survive a reboot. A label whose cell is momentarily browned out by
 * an E-Ink charge-pump surge comes back with RAM cleared. If the displayed
 * sequence came back as zero, the very next retained MQTT price on the broker —
 * possibly an old one, since retained messages are whatever was last published
 * — would be accepted, and the glass would move backwards. So the record lives
 * in NVS, and the *commit ordering* around it is the whole design:
 *
 *   1. verify the attestation
 *   2. persist the new sequence         <-- before the waveform, not after
 *   3. drive the panel
 *   4. acknowledge
 *
 * Persisting before the refresh means a crash mid-waveform loses at worst one
 * price change, and the label recovers showing a partially drawn image with a
 * sequence that will accept the retry. Persisting *after* would mean a crash
 * mid-waveform leaves the old sequence with new pixels on the glass, and the
 * retry would then be discarded as stale — a label showing a price it has told
 * the platform it is not showing. That is the failure this ordering exists to
 * prevent, and it is why usslp_seq_commit is a separate call from
 * usslp_seq_check rather than one function that does both.
 *
 * This header is portable and is covered by tests/test_seq.c. The NVS binding
 * is in app/seq_store.c.
 */

#ifndef USSLP_SEQ_H
#define USSLP_SEQ_H

#include "../usslp_portable.h"

/* The persisted record. Sixteen bytes, which is one NVS entry and well inside
 * the write granularity of the nRF52840's flash. */
#define USSLP_SEQ_RECORD_LEN 16
#define USSLP_SEQ_MAGIC 0x53514E31u /* "SQN1" */

struct usslp_seq_state {
	/* The sequence of the price currently on the glass. INT64_MIN means "this
	 * label has never displayed a price", which is distinct from 0: canon
	 * sequences are int64 and a platform is entitled to start at 0. */
	int64_t displayed;
	uint32_t accepted;
	uint32_t discarded;
};

enum usslp_seq_verdict {
	USSLP_SEQ_ACCEPT = 0,
	/* Not strictly greater than what is displayed. The expected outcome of a
	 * duplicated mesh frame; acknowledged as AckStaleSequence, not an error. */
	USSLP_SEQ_STALE = 1,
};

void usslp_seq_init(struct usslp_seq_state *s);

/* True when the label has never displayed a price. */
bool usslp_seq_never_displayed(const struct usslp_seq_state *s);

/*
 * Applies the rule. Pure: it updates only the discarded counter, and only on a
 * rejection, so that a caller cannot accidentally advance the displayed
 * sequence by asking whether it should.
 */
enum usslp_seq_verdict usslp_seq_check(struct usslp_seq_state *s, int64_t candidate);

/*
 * Records that `candidate` is now the sequence on the glass. Refuses to move
 * backwards even if called with a stale value, because a caller that has got
 * the ordering wrong must not be able to corrupt the invariant.
 */
int usslp_seq_commit(struct usslp_seq_state *s, int64_t candidate);

/*
 * Encodes the state for NVS: a 4-byte magic, the 8-byte displayed sequence and
 * a 4-byte CRC-32C, all big endian. The accept/discard counters are *not*
 * persisted: they are uptime-scoped telemetry, and writing them would turn one
 * flash write per price change into one per frame received, including the
 * duplicates the rule exists to absorb.
 *
 * The CRC is not

 * for security — an attacker with flash write access has already won — but for
 * the half-written record a brownout during the commit produces. A record that
 * fails its CRC is treated as absent, and absent means "never displayed", which
 * is the safe direction: the label accepts the next update rather than
 * rejecting everything forever.
 */
void usslp_seq_encode(const struct usslp_seq_state *s, uint8_t out[USSLP_SEQ_RECORD_LEN]);

/* Decodes and validates. Returns USSLP_ERR_MALFORMED for a bad magic or CRC, in
 * which case *s is initialised to the never-displayed state. */
int usslp_seq_decode(struct usslp_seq_state *s, const uint8_t in[USSLP_SEQ_RECORD_LEN]);

#endif /* USSLP_SEQ_H */
