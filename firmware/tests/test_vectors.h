/*
 * The shapes the generated vectors in vectors.h are poured into.
 *
 * vectors.h is produced by running the Go reference implementations
 * (platform/pkg/canon, platform/pkg/pki, platform/internal/ota/domain,
 * edge/sec, edge/mesh, edge/labelsim) and printing their output as C
 * initialisers. It is checked in rather than generated at build time so that the
 * firmware tests need nothing but a C compiler; regenerate it with
 * scripts/regen_vectors.md's recipe when the Go side changes.
 */

#ifndef USSLP_TEST_VECTORS_H
#define USSLP_TEST_VECTORS_H

#include <stdint.h>

struct attest_vector {
	const char *name;
	const char *tenant;
	const char *store;
	const char *label;
	const char *sku;
	int64_t amount_minor;
	const char *currency;
	int64_t effective_at_unix;
	int64_t sequence;
	const char *promotion;
	const char *canonical;
	const char *digest_hex;
};

struct sha_vector {
	int len;
	const char *fill;
	const char *digest_hex;
};

struct risk_vector {
	double lqi;
	double trend;
	double rssi_sd;
	double battery;
	double depth;
	double risk;
};

struct power_vector {
	const char *profile;
	int tier;
	double ambient_c;
	double sleep_ua;
	double beacon_ua;
	double rx_ua;
	double refresh_ua;
	double tx_ua;
	double nfc_ua;
	double self_ua;
	double total_ua;
	double usable_mah;
	double years;
	double fast_fraction;
};

#include "vectors.h"

#endif /* USSLP_TEST_VECTORS_H */
