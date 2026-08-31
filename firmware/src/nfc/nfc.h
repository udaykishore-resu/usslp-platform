/*
 * nfc.h - the ST25DV dynamic tag: what a shopper's phone reads.
 *
 * The ST25DV is a dual-interface EEPROM: an NFC Forum Type 5 tag on the RF side
 * and an I2C slave on the MCU side. The RF side is *field powered*, which is the
 * property that makes this worth having on a coin-cell device — a shopper can
 * tap a label whose battery is flat and still get the price, because the phone's
 * own field powers the tag.
 *
 * That is also the constraint that shapes this module. The MCU cannot be in the
 * loop for a read; it writes the record in advance and the tag serves it. So the
 * question is not "what does the label do when tapped" but "what is written in
 * the tag at the moment somebody taps it", and the answer has to be right at all
 * times, including while a price update is in flight.
 *
 * The invariant: the NFC record follows the glass, never leads it.
 *
 * A shopper who taps a label and gets a price the panel is not showing has been
 * shown two different prices by the same device, which is the exact
 * weights-and-measures failure the whole attestation apparatus exists to
 * prevent. So the record is rewritten only *after* usslp_eink_refresh returns,
 * never before, and during the 1.5 seconds of a refresh the tag still carries
 * the old price — which is the price still on the glass, and therefore correct.
 *
 * The MCU does wake for a tap, because the energy-harvest interrupt is how the
 * label knows a shopper is present: that opens the activity window, on the
 * reasoning that a label somebody is standing in front of is a label worth being
 * able to update quickly. But the read itself does not need it.
 */

#ifndef USSLP_NFC_H
#define USSLP_NFC_H

#include "../usslp_portable.h"

int usslp_nfc_init(void);

/*
 * Rewrites the tag's NDEF record with a price. Called from the price handler
 * *after* the panel has settled; see the invariant above.
 *
 * The record is a URI record pointing at the retailer's product page with the
 * SKU and the store as query parameters, plus a text record carrying the price
 * and the sequence. The URI is what a phone actually does something with — every
 * modern phone opens it without an app — and the text record is what a field
 * engineer with a reader uses to see what the label thinks it is showing.
 */
int usslp_nfc_publish_price(int64_t price_minor, const char *currency, int64_t sequence);

/* Writes the commissioning record: the serial and a provisioning URL. What a
 * technician's handheld reads before the label has ever joined a mesh. */
int usslp_nfc_publish_commissioning(void);

/* Number of taps seen since boot, reported in telemetry. Shopper interaction is
 * a real signal — a label with a hundred taps a day is on a product people are
 * uncertain about — and it costs nothing to count. */
uint32_t usslp_nfc_taps(void);

#endif /* USSLP_NFC_H */
