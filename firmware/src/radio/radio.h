/*
 * radio.h - the boundary the rest of the firmware sees.
 *
 * Two radios sit behind this header and the application must not care which one
 * carried a frame:
 *
 *   - Zigbee 3.0 through the CC2652P co-processor. The primary uplink, and the
 *     only one that carries prices.
 *   - BLE 5.0 on the nRF52840's own radio. Commissioning, field diagnostics and
 *     the shopper beacon. It never carries a price, because a price has to
 *     arrive over a path the platform's mesh accounting can see.
 *
 * Both share one antenna, which is the reason this is one header rather than
 * two: transmissions have to be arbitrated, and a BLE advertisement that
 * collided with the beacon window is a price update delayed by a resting
 * interval.
 */

#ifndef USSLP_RADIO_H
#define USSLP_RADIO_H

#include "../usslp_portable.h"
#include "usslp_route.h"

/* Delivered to the price thread. The buffer is owned by the radio layer and is
 * valid only for the duration of the call. */
typedef void (*usslp_radio_rx_cb)(const uint8_t *frame, size_t len);

int usslp_radio_init(usslp_radio_rx_cb on_frame);

/* Called by the radio stack's own thread for each inbound application frame.
 * Declared here rather than left as an implicit external so that the stack
 * binding and this layer cannot disagree about the signature. */
void usslp_radio_on_frame(const uint8_t *frame, size_t len);

/* Sends an uplink frame — an ack or a telemetry report — to the coordinator.
 * Non-blocking; the transmission is queued and charged to the power ledger when
 * it completes. */
int usslp_radio_send_uplink(const uint8_t *frame, size_t len);

/* True once the label has associated with a coordinator. */
bool usslp_radio_joined(void);

/* The parent's measured link quality and received power, which is the half of
 * the mesh picture the controller cannot see for itself. */
int usslp_radio_parent_link(uint8_t *lqi, int8_t *rssi_dbm);

/* Forces a rejoin. Used when the predictive healer says this label's uplink is
 * about to fail, and by the field engineer's shell command. */
int usslp_radio_rejoin(void);

/*
 * The label's own view of its neighbours, for the routing model. An end device
 * uses it to choose a parent; a mains-powered relay build uses the full cost
 * model to route for its children.
 */
size_t usslp_radio_neighbours(struct usslp_neighbour *out, size_t cap);

/* Runs the link assessment over the current parent and acts on it. Called on
 * the telemetry cadence, which is also the sampling cadence the model's
 * coefficients were fitted against. */
void usslp_radio_assess_links(void);

/* BLE. */
int usslp_ble_init(void);
int usslp_ble_advertise(bool on);
/* Puts the label into commissioning mode for a bounded window: a connectable
 * advertisement with the provisioning service. Bounded because a label
 * advertising a commissioning service forever is a label anybody can walk up to.
 */
int usslp_ble_commissioning_window(uint32_t seconds);

#endif /* USSLP_RADIO_H */
