# Host tests for the USSLP label firmware's portable core.
#
# These build with a plain C11 compiler and no Zephyr anywhere in sight. That is
# the point: the modules under test are the ones where a divergence from the
# platform would be unrecoverable in the field — the attestation digest, the
# sequence rule, the air protocol, the OTA patch applier — and a test somebody
# can run in ten seconds on any machine is worth more than one that needs an
# SDK, a toolchain and a board.
#
#   make          build and run
#   make run      the same
#   make asan     rebuild with the address and undefined-behaviour sanitisers
#   make clean

CC      ?= gcc
CSTD    ?= -std=c11
WARN     = -Wall -Wextra -Werror -Wshadow -Wpointer-arith -Wcast-qual \
           -Wstrict-prototypes -Wmissing-prototypes -Wvla
OPT     ?= -O2 -g
CFLAGS   = $(CSTD) $(WARN) $(OPT)
LDFLAGS  =

SRC_DIR = ../src

# The portable core. Everything here compiles outside Zephyr by construction;
# anything that needs a peripheral lives in a file that is not on this list.
CORE = \
	$(SRC_DIR)/usslp_crc32c.c \
	$(SRC_DIR)/crypto/usslp_sha256.c \
	$(SRC_DIR)/crypto/usslp_canon.c \
	$(SRC_DIR)/crypto/usslp_keyring.c \
	$(SRC_DIR)/crypto/usslp_attest.c \
	$(SRC_DIR)/app/usslp_seq.c \
	$(SRC_DIR)/display/usslp_rle.c \
	$(SRC_DIR)/display/usslp_render_policy.c \
	$(SRC_DIR)/radio/usslp_wire.c \
	$(SRC_DIR)/radio/usslp_route.c \
	$(SRC_DIR)/power/usslp_budget.c \
	$(SRC_DIR)/ota/usslp_inflate.c \
	$(SRC_DIR)/ota/usslp_patch.c \
	$(SRC_DIR)/ota/usslp_chunkmap.c

TESTS = \
	main.c \
	fake_ed25519.c \
	test_sha256.c \
	test_canon.c \
	test_attest.c \
	test_seq.c \
	test_wire.c \
	test_rle.c \
	test_render_policy.c \
	test_route.c \
	test_power.c \
	test_patch.c

BIN = usslp_tests

.PHONY: all run asan clean

all: run

$(BIN): $(CORE) $(TESTS) test_util.h test_vectors.h vectors.h
	$(CC) $(CFLAGS) $(CORE) $(TESTS) -o $(BIN) $(LDFLAGS)

run: $(BIN)
	./$(BIN)

# The patch applier and the image decoder both walk attacker-influenced buffers.
# Running them under the sanitisers is how "every bound is checked" stops being
# a claim in a comment.
asan:
	$(CC) $(CSTD) $(WARN) -O1 -g -fsanitize=address,undefined \
		-fno-omit-frame-pointer $(CORE) $(TESTS) -o $(BIN)_asan
	./$(BIN)_asan

clean:
	rm -f $(BIN) $(BIN)_asan
