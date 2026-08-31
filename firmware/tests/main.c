/*
 * The host test runner for the USSLP label firmware's portable core.
 *
 * Build and run with `make` in this directory. It needs a C11 compiler and
 * nothing else — no Zephyr, no SDK, no board. What it covers, and what it
 * deliberately does not, is set out in ../README.md under "What has and has not
 * been verified".
 */

#include "test_util.h"

int usslp_test_failures;
int usslp_test_checks;
const char *usslp_test_current = "(none)";

int main(void)
{
	printf("USSLP label firmware - portable core tests\n\n");

	test_sha256();
	test_canon();
	test_keyring();
	test_attest();
	test_seq();
	test_wire();
	test_rle();
	test_render_policy();
	test_route();
	test_power();
	test_patch();
	test_chunkmap();

	printf("\n%d checks, %d failures\n", usslp_test_checks, usslp_test_failures);
	if (usslp_test_failures != 0) {
		printf("FAILED\n");
		return 1;
	}
	printf("PASS\n");
	return 0;
}
