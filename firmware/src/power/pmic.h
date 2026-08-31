/*
 * pmic.h - the BQ25125 PMIC, the battery gauge and the RF harvester.
 *
 * Three closely related jobs that share one chip and one I2C transaction, so
 * they share a header:
 *
 *   - the PMIC itself, which owns the boost that drives the E-Ink rails and the
 *     load switches for the radio co-processor and the NFC front end;
 *   - the fuel gauge, which on a primary cell is not a coulomb counter but a
 *     voltage lookup, for the reason set out below;
 *   - the RF harvester, which is measured and reported rather than relied on.
 *
 * Why there is no coulomb counter
 * -------------------------------
 * A LiMnO2 primary cell has a famously flat discharge curve — that is the point
 * of the chemistry — so voltage is a poor state-of-charge signal for most of the
 * cell's life. The obvious answer is to integrate current instead. USSLP does
 * not, for two reasons. A coulomb counter costs 3-5 uA continuously, which is
 * most of this device's entire budget and would shorten the life it is measuring
 * by roughly half. And the accumulated error over ten years of a counter with
 * even 1% offset is larger than the answer.
 *
 * So the gauge is: a voltage reading, mapped through the same discharge curve
 * the platform's model uses (labelsim.batteryMillivolts, ported in
 * usslp_budget.c), cross-checked against the firmware's own charge ledger from
 * power.h. The two disagreeing by more than about 15% is itself the useful
 * signal — it means the cell is not behaving like the model, which is what a
 * cold-damaged or counterfeit cell looks like — and it is reported rather than
 * averaged away.
 *
 * The voltage has to be read under a known load, and specifically not during an
 * E-Ink refresh: the charge pump's 30 mA pulse drops the cell several hundred
 * millivolts through its internal resistance, and a gauge that sampled then
 * would declare a healthy cell critical every time a price changed.
 */

#ifndef USSLP_PMIC_H
#define USSLP_PMIC_H

#include "../usslp_portable.h"

int usslp_pmic_init(void);

/* The boosted rail that drives the E-Ink panel. Enabled for the duration of a
 * waveform and never longer: leaving it up costs about 40 uA, six times the
 * whole sleep budget, and is invisible on a bench supply. */
int usslp_pmic_panel_rail(bool on);

/* The load switch for the radio co-processor. Cut only for a hard reset of a
 * wedged CC2652P, because a cold radio start costs a full mesh rejoin. */
int usslp_pmic_radio_power(bool on);

/* Reads the cell voltage under a known light load, in millivolts. Refuses with
 * USSLP_ERR_BUSY while the panel is refreshing rather than returning a reading
 * depressed by the charge pump's pulse. */
int usslp_pmic_read_vbat(uint16_t *millivolts);

/*
 * The gauge. Returns the cell voltage and the estimated remaining percentage.
 *
 * Both are needed and they are not redundant: device.battery.critical is raised
 * off the voltage, because that is what actually predicts the cliff, while the
 * percentage is what a fleet dashboard shows and what a replacement schedule is
 * built from.
 */
void usslp_gauge_read(uint16_t *millivolts, uint8_t *percent);

/* How far the voltage-derived state of charge and the ledger-derived one
 * disagree, in percentage points. Reported in telemetry: a large and growing
 * divergence is what a cold-damaged or counterfeit cell looks like, and it is
 * information that averaging the two away would destroy. */
int8_t usslp_gauge_model_divergence(void);

/* Harvesting. usslp_harvest_poll is called from the telemetry cadence rather
 * than continuously — sampling the rectifier costs more than most labels will
 * ever harvest. */
int usslp_harvest_init(void);
void usslp_harvest_poll(void);

#endif /* USSLP_PMIC_H */
