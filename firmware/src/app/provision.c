#include "provision.h"

#include "../crypto/devcert.h"
#include "../crypto/usslp_sha256.h"
#include "../display/eink.h"
#include "../nfc/nfc.h"
#include "../power/power.h"
#include "../radio/radio.h"
#include "price.h"
#include "seq_store.h"

#include <string.h>
#include <zephyr/kernel.h>
#include <zephyr/logging/log.h>
#include <zephyr/settings/settings.h>

LOG_MODULE_REGISTER(usslp_provision, CONFIG_USSLP_LOG_LEVEL);

#define PROV_SETTINGS_KEY "usslp/prov"

static enum usslp_provision_state state = USSLP_PROV_FACTORY;
static struct usslp_assignment assignment;

const char *usslp_provision_state_str(enum usslp_provision_state s)
{
	switch (s) {
	case USSLP_PROV_FACTORY:
		return "FACTORY";
	case USSLP_PROV_ANNOUNCING:
		return "ANNOUNCING";
	case USSLP_PROV_ASSIGNED:
		return "ASSIGNED";
	case USSLP_PROV_ACTIVE:
		return "ACTIVE";
	case USSLP_PROV_RETIRED:
		return "RETIRED";
	}
	return "UNKNOWN";
}

struct __packed prov_record {
	uint8_t state;
	struct usslp_assignment assignment;
};

static int prov_settings_set(const char *name, size_t len, settings_read_cb read_cb, void *cb_arg)
{
	struct prov_record rec;

	if (!settings_name_steq(name, "rec", NULL) || len != sizeof(rec)) {
		return -ENOENT;
	}
	if (read_cb(cb_arg, &rec, sizeof(rec)) != (ssize_t)sizeof(rec)) {
		return -EIO;
	}
	state = (enum usslp_provision_state)rec.state;
	assignment = rec.assignment;
	/* ACTIVE is not persisted as ACTIVE. On a reboot the label has an image on
	 * the glass — the panel is bistable — but it has not yet verified a price in
	 * this boot, and the distinction between "assigned" and "has displayed a
	 * verified price" is the one the platform uses to tell a working label from
	 * one that is refusing everything. It re-earns ACTIVE. */
	if (state == USSLP_PROV_ACTIVE) {
		state = USSLP_PROV_ASSIGNED;
	}
	return 0;
}

static struct settings_handler prov_handler = {
	.name = "usslp/prov",
	.h_set = prov_settings_set,
};

static int prov_save(void)
{
	struct prov_record rec = { .state = (uint8_t)state, .assignment = assignment };

	return settings_save_one(PROV_SETTINGS_KEY "/rec", &rec, sizeof(rec));
}

int usslp_provision_init(void)
{
	const struct usslp_device_identity *id = usslp_devcert_identity();
	int rc;

	rc = settings_register(&prov_handler);
	if (rc != 0 && rc != -EEXIST) {
		return USSLP_ERR_IO;
	}
	(void)settings_load_subtree(PROV_SETTINGS_KEY);

	if (state == USSLP_PROV_FACTORY || state == USSLP_PROV_RETIRED) {
		/* Nothing valid to display. The commissioning screen is what a
		 * technician reads off the rail to see which labels are done. */
		(void)usslp_template_commissioning(id != NULL ? id->serial : "NO IDENTITY",
						   usslp_provision_state_str(state));
		{
			struct usslp_refresh_plan plan = {
				.partial = false,
				.duration_ms =
					usslp_panel(usslp_eink_tier())->full_refresh_ms,
				.forced_full = false,
			};

			(void)usslp_eink_refresh(&plan, NULL);
		}
	}
	LOG_INF("provisioning state %s", usslp_provision_state_str(state));
	return USSLP_OK;
}

enum usslp_provision_state usslp_provision_state(void)
{
	return state;
}

const struct usslp_assignment *usslp_provision_assignment(void)
{
	return state >= USSLP_PROV_ASSIGNED && state != USSLP_PROV_RETIRED ? &assignment : NULL;
}

/*
 * The signed assignment bundle.
 *
 *   0   magic "USPV"
 *   4   version (1)
 *   5   five length-prefixed identifiers: tenant, store, label, sec, slot
 *  ...  the label's EUI-64, so an assignment intended for one label cannot be
 *       replayed onto another
 *  ...  kid, 28 bytes
 *  ...  Ed25519 signature over everything before it
 */
#define PROV_MAGIC "USPV"

int usslp_provision_apply(const void *payload, size_t len)
{
	const uint8_t *p = (const uint8_t *)payload;
	const struct usslp_device_identity *id = usslp_devcert_identity();
	const struct usslp_ring_key *signer;
	struct usslp_assignment staged;
	char kid[USSLP_KID_BUF];
	uint8_t digest[USSLP_SHA256_DIGEST_LEN];
	uint64_t target_eui = 0;
	size_t off = 5;
	char *fields[5];
	size_t caps[5];
	int rc;

	if (p == NULL || len < 5u + 8u + USSLP_KID_LEN + USSLP_ED25519_SIGNATURE_LEN) {
		return USSLP_ERR_MALFORMED;
	}
	if (memcmp(p, PROV_MAGIC, 4) != 0 || p[4] != 1u) {
		return USSLP_ERR_MALFORMED;
	}
	if (id == NULL) {
		return USSLP_ERR_UNSUPPORTED;
	}

	memset(&staged, 0, sizeof(staged));
	fields[0] = staged.tenant;
	caps[0] = sizeof(staged.tenant);
	fields[1] = staged.store;
	caps[1] = sizeof(staged.store);
	fields[2] = staged.label_id;
	caps[2] = sizeof(staged.label_id);
	fields[3] = staged.sec_id;
	caps[3] = sizeof(staged.sec_id);
	fields[4] = staged.slot;
	caps[4] = sizeof(staged.slot);

	for (unsigned i = 0; i < 5; i++) {
		uint8_t n;

		if (off >= len) {
			return USSLP_ERR_MALFORMED;
		}
		n = p[off++];
		if (n == 0u || (size_t)n >= caps[i] || off + n > len) {
			return USSLP_ERR_MALFORMED;
		}
		memcpy(fields[i], &p[off], n);
		fields[i][n] = '\0';
		off += n;
	}
	if (off + 8u + USSLP_KID_LEN + USSLP_ED25519_SIGNATURE_LEN != len) {
		return USSLP_ERR_MALFORMED;
	}
	for (unsigned i = 0; i < 8; i++) {
		target_eui = (target_eui << 8) | p[off + i];
	}
	if (target_eui != id->eui64) {
		/* An assignment for a different label. Without this check an attacker
		 * could replay a legitimate assignment onto every label in an aisle and
		 * have forty of them believe they are the same shelf edge. */
		LOG_ERR("assignment is for EUI %016llx, this label is %016llx",
			(unsigned long long)target_eui, (unsigned long long)id->eui64);
		return USSLP_ERR_AUTH;
	}
	off += 8u;

	memcpy(kid, &p[off], USSLP_KID_LEN);
	kid[USSLP_KID_LEN] = '\0';
	signer = usslp_keyring_find(usslp_price_keyring(), kid);
	if (signer == NULL) {
		LOG_ERR("assignment signed by an unknown key %s", kid);
		return USSLP_ERR_AUTH;
	}
	usslp_sha256(p, off + USSLP_KID_LEN, digest);
	rc = usslp_ed25519_verify(signer->pub, digest, sizeof(digest),
				  &p[off + USSLP_KID_LEN]);
	if (rc != USSLP_OK) {
		LOG_ERR("assignment signature does not verify");
		return USSLP_ERR_AUTH;
	}

	assignment = staged;
	state = USSLP_PROV_ASSIGNED;
	rc = prov_save();
	if (rc != 0) {
		LOG_ERR("persisting the assignment failed (%d)", rc);
		return USSLP_ERR_IO;
	}
	LOG_INF("assigned to %s/%s as %s on slot %s", assignment.tenant, assignment.store,
		assignment.label_id, assignment.slot);
	(void)usslp_nfc_publish_commissioning();
	usslp_provision_report_projection();
	return USSLP_OK;
}

void usslp_provision_note_price_displayed(void)
{
	if (state == USSLP_PROV_ASSIGNED) {
		state = USSLP_PROV_ACTIVE;
		(void)prov_save();
		LOG_INF("active: a verified price is on the glass");
	}
}

int usslp_provision_retire(void)
{
	struct usslp_ghost_state ghost;
	int rc;

	memset(&assignment, 0, sizeof(assignment));
	state = USSLP_PROV_RETIRED;
	(void)prov_save();
	rc = usslp_seq_store_reset(&ghost);
	if (rc == USSLP_OK) {
		rc = usslp_eink_clear();
	}
	LOG_WRN("retired");
	return rc;
}

void usslp_provision_report_projection(void)
{
	struct usslp_projection pr;

	usslp_power_retune();
	usslp_power_projection(&pr);

	if (pr.life_milliyears < 7000) {
		/*
		 * The planogram has put this label somewhere it cannot last. A colour
		 * panel on a high-churn promotional end projects under a year; a
		 * 2.9-inch label in a freezer case projects six. Both are decisions
		 * somebody may have made deliberately, so this is a report rather than a
		 * refusal — but it is reported now, while a technician is still standing
		 * in the aisle, rather than discovered in year one of the deployment.
		 */
		LOG_WRN("device.battery.projection.short: %d.%03d years projected on this "
			"fitting and workload, against a %d.%03d-year target",
			pr.life_milliyears / 1000, pr.life_milliyears % 1000,
			CONFIG_USSLP_TARGET_LIFE_MILLIYEARS / 1000,
			CONFIG_USSLP_TARGET_LIFE_MILLIYEARS % 1000);
	} else {
		LOG_INF("projected life %d.%03d years at %u nA average",
			pr.life_milliyears / 1000, pr.life_milliyears % 1000, pr.total_na);
	}
}
