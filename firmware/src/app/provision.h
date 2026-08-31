/*
 * provision.h - the provisioning state machine.
 *
 * A label leaves the factory knowing three things: its own key pair, its EUI-64,
 * and which panel is fitted. It does not know which store it is in, which shelf
 * it belongs to, or which SKU it is showing. Provisioning is how it learns, and
 * the state machine matters because commissioning is done by a technician
 * clipping four hundred labels onto rails in an afternoon, not by an engineer at
 * a bench.
 *
 *   FACTORY      an identity record and nothing else. The panel shows the
 *                commissioning screen with the serial, so a technician can see
 *                which labels have been done without a phone.
 *   ANNOUNCING   joined a mesh, has told the coordinator its serial and tier,
 *                waiting for an assignment. A label sits here if it has been
 *                clipped to a rail nobody has assigned yet, which is the normal
 *                state during a fit-out.
 *   ASSIGNED     has a tenant, store, label id and planogram slot, verified
 *                against the price-authority ring. Ready to take a price.
 *   ACTIVE       has displayed at least one verified price.
 *   RETIRED      decommissioned: the panel is cleared and the sequence forgotten.
 *
 * The assignment is signed, and verified against the same key ring the price
 * attestation uses. That is the point: without it, anyone who can reach the mesh
 * or stand next to the label with a phone could assign it to a slot and start
 * feeding it prices. With it, an attacker who owns the mesh can stop a label
 * being commissioned but cannot commission one themselves.
 *
 * The transition that is easy to get wrong is ASSIGNED to ACTIVE. It is not
 * "received a price"; it is "displayed a price that verified". A label that has
 * been assigned and is refusing every price it is sent looks identical to one
 * that is working, from the platform's point of view, unless the distinction is
 * made here.
 */

#ifndef USSLP_PROVISION_H
#define USSLP_PROVISION_H

#include "../usslp_portable.h"

enum usslp_provision_state {
	USSLP_PROV_FACTORY = 0,
	USSLP_PROV_ANNOUNCING,
	USSLP_PROV_ASSIGNED,
	USSLP_PROV_ACTIVE,
	USSLP_PROV_RETIRED,
};

struct usslp_assignment {
	char tenant[64];
	char store[64];
	char label_id[64];
	char sec_id[64];
	/* The planogram slot, for the technician's tool and for the mis-fit report:
	 * a label assigned to a chiller slot that is reporting 20 C is on the wrong
	 * shelf. */
	char slot[32];
};

int usslp_provision_init(void);

enum usslp_provision_state usslp_provision_state(void);
const struct usslp_assignment *usslp_provision_assignment(void);
const char *usslp_provision_state_str(enum usslp_provision_state s);

/*
 * Applies a signed assignment, from BLE commissioning or from the mesh.
 *
 * Verified against the price-authority key ring before anything is stored, so a
 * phone in the aisle cannot assign a label to a slot it was not authorised for.
 */
int usslp_provision_apply(const void *payload, size_t len);

/* Called by the price path the first time a verified price reaches the glass. */
void usslp_provision_note_price_displayed(void);

/* Decommissioning: clears the assignment, the sequence and the panel. */
int usslp_provision_retire(void);

/*
 * Reports the label's projected life against its assignment at commissioning.
 *
 * This is where a planogram mistake becomes visible while somebody is still
 * standing in the aisle rather than in year one of the deployment: a colour
 * panel assigned to a high-churn promotional end projects under a year, and a
 * 2.9-inch label assigned to a freezer case projects six. Raised as
 * device.battery.projection.short rather than silently accepted.
 */
void usslp_provision_report_projection(void);

#endif /* USSLP_PROVISION_H */
