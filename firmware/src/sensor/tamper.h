/*
 * tamper.h - LIS2DH12 tamper detection, and the locator LED.
 *
 * What the accelerometer is for, and what it is not for.
 *
 * It is for: detecting that a label has been taken off its rail. That is a real
 * retail problem — label swapping is a shoplifting technique, and a label found
 * on the floor is a shelf edge with no price on it, which is a compliance
 * failure the moment a customer looks at it.
 *
 * It is not for: motion sensing, footfall, or anything that requires the part to
 * be sampled continuously. A LIS2DH12 in low-power mode at 10 Hz draws about
 * 2 uA, which against this device's 6.6 uA total budget would cost roughly a
 * third of the battery life. So the part is configured once, at boot, into
 * interrupt-driven any-motion mode with the interrupt threshold set high enough
 * that shelf vibration and a passing trolley do not trigger it, and the MCU
 * never polls it at all. In that configuration it draws about 0.8 uA — still one
 * of the larger standing costs on the board, and the reason the threshold is set
 * where it is rather than lower.
 *
 * The threshold is deliberately not sensitive. A label being restocked around,
 * a freezer door slamming, and a trolley hitting a gondola end all produce
 * transients; a label being unclipped produces a sustained reorientation. The
 * detector therefore requires both a shock *and* a change in the gravity vector
 * that persists, which is what distinguishes "somebody knocked the shelf" from
 * "somebody took this label".
 */

#ifndef USSLP_TAMPER_H
#define USSLP_TAMPER_H

#include "../usslp_portable.h"

int usslp_tamper_init(void);

/* True while the label believes it has been removed or reoriented. Latched: it
 * stays set until a technician clears it, because the useful question is "has
 * this label been interfered with since anyone last looked", not "is it moving
 * right now". */
bool usslp_tamper_active(void);

/* Clears the latch. Called from the cluster's tamper attribute write, which is
 * the only writable attribute the label exposes. */
void usslp_tamper_clear(void);

/* Number of tamper events since boot. A label with one event has been moved
 * once; a label with forty is on a shelf that is being restocked constantly and
 * the threshold is wrong for that fitting. */
uint32_t usslp_tamper_events(void);

/*
 * Pulses the locator LED.
 *
 * The LED is how a picker finds one shelf edge among four hundred, and it is
 * also a measurable battery cost: a 2 mA indicator lit for ten minutes is 0.33
 * mAh, which is a fortnight of this label's entire budget. So it is pulsed at a
 * low duty cycle rather than lit, it is bounded, and it only ever runs on an
 * explicit request from somebody who is standing there.
 */
void usslp_locator_pulse(uint16_t duration_ms);

#endif /* USSLP_TAMPER_H */
