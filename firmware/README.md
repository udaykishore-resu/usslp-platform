# USSLP Tier 1 firmware — the smart shelf label

Zephyr RTOS 3.x application for the USSLP smart shelf label: a Nordic nRF52840
driving an E-Ink panel, a TI CC2652P Zigbee co-processor, an ST25DV NFC tag, a
LIS2DH12 accelerometer and a BQ25125 PMIC on a 500 mAh LiMnO₂ primary cell.

Two sentences describe what this firmware is for: it must show a price a shopper
can read for seven to ten years on one cell, and it must never display a price it
cannot cryptographically verify. Everything below is downstream of those.

---

## Status, honestly

**This firmware has not been compiled.** The Zephyr SDK, the ARM toolchain and
the nRF Connect SDK are not installed in the environment it was written in, so
the application half — every file that includes a `zephyr/` header — has been
written but never fed to a compiler, let alone run on hardware. Treat it as a
reference implementation that a Zephyr developer would recognise as buildable,
not as a build artefact.

**The parts that could be verified, were.** The firmware is deliberately split so
that everything whose correctness is unrecoverable in the field is portable C
with no Zephyr dependency, and that half builds and runs on the host under
`gcc -std=c11 -Wall -Wextra -Werror` with additional `-Wshadow -Wpointer-arith
-Wcast-qual -Wstrict-prototypes -Wmissing-prototypes -Wvla`:

```
$ cd firmware/tests && make
...
25961 checks, 0 failures
PASS

$ make asan                              # AddressSanitizer + UBSan
25961 checks, 0 failures
PASS

$ make CC=clang                          # a second front end, same -Werror
25961 checks, 0 failures
PASS

$ make OPT="-O0 -g -fsanitize=undefined -fno-sanitize-recover=all"
25961 checks, 0 failures
PASS
```

The trapping-UBSan run earns its place: it found a real defect, a left shift of a
negative value in the fixed-point trend fit, which is undefined behaviour that
happens to work on every compiler anyone has tried and is exactly the sort of
thing that surprises you when the target changes. Scaling up now multiplies by
`USSLP_Q16_ONE`; see the note at the top of `radio/usslp_route.c` for which
shifts remain and why they are safe.

**A test that passed for the wrong reason, and what it cost to find.** The
type-4 round-trip test used to hand-assemble its frame from byte offsets, and it
advanced past the promotion field without copying it. It passed anyway, because
the single vector it ran over has an *empty* promotion — so the nine bytes it
failed to write were nine bytes that were supposed to be absent. It took the edge
tier compiling this decoder against their own encoder to surface it.

The lesson generalises past the instance. A test that hand-builds the artefact it
is testing can agree with itself while both are wrong, and one vector of coverage
is exactly as wide as the bug it is hiding. Both halves are now fixed: the
firmware has a real encoder (`usslp_wire_encode_attested_update`), the test uses
it so no offset arithmetic lives in the test at all, and the loop runs over every
vector — the one with a promotion, the one with a UTF-8 SKU, the one with a
pre-epoch timestamp, the one at `INT64_MAX` sequence. Re-introducing the original
defect now fails on the first assertion that touches the promotion, by name,
rather than passing.

What that covers, and what it does not, is in
[What has and has not been verified](#what-has-and-has-not-been-verified) below.
Read it before trusting anything here.

---

## Hardware

| Part | Role | Why |
|---|---|---|
| nRF52840 | Application MCU, BLE 5.0 | 1 MB flash, 256 KB RAM, Cortex-M4F at 64 MHz |
| CC2652P | 802.15.4 / Zigbee 3.0 co-processor | Holds a receive window at 6.5 mA with the nRF in System OFF at 0.8 µA |
| E-Ink panel | The product | Three tiers, below |
| ST25DV04K | Dynamic NFC tag | Field powered: a shopper can tap a label with a flat cell and still get the price |
| LIS2DH12 | Tamper detection | Interrupt driven at ~0.8 µA; polling it would cost a third of the battery |
| BQ25125 | PMIC | Boost for the ±15 V E-Ink rails, load switches, RF harvest input |

**On the co-processor.** The nRF52840 has a perfectly good 802.15.4 radio and a
single-chip design would be cheaper. The CC2652P is there because beacon
listening is more than half of this device's entire energy budget, and it can do
that listening with the application MCU in System OFF. A single-chip build would
be cheaper in bill of materials and would not reach the battery target.

### Display tiers

| Tier | Panel | Full / partial | Max partials | Refresh current | Config |
|---|---|---|---|---|---|
| 0 | 2.9″ 296×128 BWR | 1500 ms / 300 ms | 8 | 26 mA | `boards/tier_29bwr.conf` |
| 1 | 4.2″ 400×300 BW | 2000 ms / 300 ms | 8 | 30 mA | `boards/tier_42bw.conf` |
| 2 | 5.85″ 600×448 ACeP | 15 s / — | 0 | 35 mA | `boards/tier_585acep.conf` |

Every figure is `labelsim.DisplaySpec`'s, because the platform publishes battery
and latency numbers derived from that table and a firmware that disagreed with it
would make those numbers fiction.

The colour panel cannot do a partial refresh, for three independent reasons: its
waveform sequences the pigments through the whole stack and there is nothing to
shorten; its controller exposes no LUT override; and at 4 bits per pixel its
134 KB framebuffer does not fit in RAM, so the firmware streams it and has no
local copy to diff against.

---

## Build and flash

Requires a `west` workspace whose manifest includes the **nRF Connect SDK** —
the ZBOSS Zigbee stack ships there, not in upstream Zephyr. Vanilla Zephyr will
configure and fail to link the radio.

```
# all three tiers
./scripts/build.sh

# one tier, pristine
./scripts/build.sh 29bwr -p

# what fits, per subsystem
./scripts/memory_report.sh build/29bwr

# bench programming over SWD (production labels have no SWD header)
./scripts/flash.sh 29bwr --with-boot --identity factory_id.hex
```

Underneath, per tier:

```
west build -b usslp_label_nrf52840 firmware \
  -- -DCONF_FILE="prj.conf;boards/tier_29bwr.conf" \
     -DDTC_OVERLAY_FILE="boards/usslp_label_nrf52840.overlay"
```

The host tests need none of that:

```
cd firmware/tests && make          # build and run
cd firmware/tests && make asan     # again under ASan/UBSan
```

---

## The battery arithmetic, and the thing it says

The hardware blueprint quotes a 250 ms beacon interval **and** a seven-to-ten
year battery life. Those two numbers do not fit in the same sentence, and this
firmware says so rather than repeating them together.

A 6.5 mA listen for 8 ms every 250 ms is a 3.2% duty cycle on the receiver:

```
  6.5 mA × (8 ms / 250 ms)                                = 208 µA
  + everything else (sleep, refresh, TX, NFC, leakage)    =   3.3 µA
                                                            -------
                                                            211.3 µA

  500 mAh / 211.3 µA = 2,366 h = 0.270 years = 99 days
```

**Ninety-nine days, not eight years.** The simulator work on this platform found
the same thing, and `firmware/tests/test_power.c` asserts it against
`labelsim.AlwaysFastPower` so it cannot quietly stop being true.

What makes the target reachable is adaptive duty cycling: 250 ms **only inside an
activity window**, and a much slower interval the rest of the time. For the
platform's planning workload — 10 price updates a day, 87.5% of them partial,
half an NFC tap, 288 telemetry reports, 20 °C, 2.9″ panel:

```
  activity        10 updates + 0.5 taps = 10.5 events/day
  fast window     10.5 × 60 s = 630 s/day on the 250 ms interval  (0.73% of the day)
  beacons         630/0.25   = 2,520 fast windows
                  85,770/30  = 2,859 slow windows
                  5,379 × 8 ms = 43.0 s of receiver-on per day
                  6.5 mA × 43.0/86,400                         =  3.237 µA
  refresh         0.875×300 ms + 0.125×1500 ms = 450 ms average
                  10 × 0.45 s × 26 mA / 86,400                 =  1.354 µA
  deep sleep                                                   =  0.800 µA
  self-discharge  500 mAh × 1%/yr / 8,760 h                    =  0.571 µA
  TX              298 × 8 ms × 18 mA / 86,400                  =  0.497 µA
  NFC             0.5 × 1.5 s × 8 mA / 86,400                  =  0.069 µA
  data RX         10 × 40 ms × 12 mA / 86,400                  =  0.056 µA
                                                                 ------
                                                                  6.584 µA

  500 mAh / 6.584 µA = 75,940 h = 8.67 years
```

That is the number, and the breakdown is the point: **half the budget is spent
listening for beacons.** Nothing else is worth optimising until that is.

### What the same arithmetic says about the rest of the fleet

| Fitting | Projection | Meets 7–10 y? |
|---|---|---|
| 2.9″ BWR, ambient 20 °C | **8.67 y** | yes |
| 2.9″ BWR, chiller 4 °C | 7.71 y | yes |
| 2.9″ BWR, freezer −20 °C | 6.23 y | **no** |
| 4.2″ BW, ambient 20 °C | 8.14 y | yes |
| 4.2″ BW, freezer −20 °C | 5.84 y | **no** |
| 5.85″ colour, ambient, 10 updates/day | **0.86 y** | **no** |
| 5.85″ colour, ambient, 1 update/day | 4.4 y | no |
| any tier, literal 250 ms beacon | 0.27 y | **no** |

Three findings the firmware reports rather than hides:

1. **A freezer case costs about 30% of the cell** and takes a 2.9″ label from
   8.7 years to 6.2. The chemistry derating is real (`CapacityDerating`), and a
   fleet plan that ignores it finds its freezer aisle dying three years early.
2. **The colour panel cannot run the planning workload on a coin cell.** A 15 s
   waveform at 35 mA, ten times a day, is 60.8 µA on its own — nine times the
   whole rest of the budget. It is a mains-powered or low-cadence fitting.
3. **The resting interval is not a constant.** `usslp_power_retune` derives it
   from the label's *own* measured update rate and shelf temperature against the
   configured target. For 8 years on the 2.9″ panel at the planning workload the
   sustainable interval is 13.7 s; the firmware clamps to
   `CONFIG_USSLP_BEACON_SLOW_MS` as a floor and 60 s as a ceiling.

`app/provision.c` computes this at commissioning and raises
`device.battery.projection.short` while a technician is still standing in the
aisle, rather than letting the fleet discover it in year one.

### What this costs

A resting label is, on average, **half a resting interval from being reachable at
all** — fifteen seconds at the default. The platform's 300 ms SEC-to-label budget
(INTERFACE-CONTRACTS §4) is therefore a statement about a zone *in its active
window*, not about a label asleep at 03:00. A price load is planned as a window:
the controller broadcasts a wake, the zone comes up over one resting interval,
and then prices flow inside the budget. `usslp_power_open_window` reproduces
`labelsim.OpenActiveWindow` including the subtlety that a resting label does not
hear the wake until its own next window — and that a window already open is
*extended*, never restarted, because a controller re-broadcasting the flag would
otherwise push the opening moment forward forever and the zone would never wake.

---

## Memory budget

Against 1 MB flash / 256 KB RAM. The flash figure that matters is not the part,
it is the **424 KiB OTA slot**: a build that fits the part and not the slot
cannot be updated over the air, which on a shelf label means it cannot be updated
at all.

```
  0x000000   48 KiB  MCUboot
  0x00c000  424 KiB  slot0 (running)
  0x076000  424 KiB  slot1 (staged)
  0x0e0000   96 KiB  ota-scratch (staged delta patch, memory mapped)
  0x0f8000   24 KiB  storage (NVS)
  0x0fe000    8 KiB  identity (device key, ACL locked at boot)
```

Two full-size slots rather than swap-move: swap-move rewrites the *running* slot
during the swap, so a power loss mid-swap leaves the device depending on
MCUboot's recovery path. On a device that must be physically retrieved if it does
not boot, 424 KiB of flash to keep the running image immutable until the new one
is verified is the right trade. The label needs the space for nothing else.

### Flash, per subsystem (2.9″ BWR build)

| Subsystem | Est. flash | Basis |
|---|---:|---|
| ZBOSS Zigbee stack | ~120 KB | NCS library, end-device profile with security |
| BLE (controller + host, peripheral, 1 conn) | ~90 KB | Largest single thing that is optional |
| Zephyr kernel + drivers (GPIO/SPI/I²C/ADC/WDT/flash/NVS/settings/log) | ~55 KB | |
| mbedTLS / PSA, Ed25519 verify + SHA-256 only | ~30 KB | Curve25519 field arithmetic dominates |
| USSLP Zephyr layer (drivers, app, waveform LUTs, font, templates) | ~28 KB | |
| **USSLP portable core** | **~20 KB** | **Measured, see below** |
| **Total** | **~343 KB** | **81% of the 424 KiB slot** |

The portable core row is not an estimate. Compiled at `-Os` on the host:

```
usslp_inflate  2,963   usslp_route    2,648   usslp_wire     2,581
usslp_patch    2,504   usslp_canon    2,350   usslp_sha256   1,693
usslp_budget   1,611   usslp_keyring  1,113   usslp_chunkmap   971
usslp_rle        869   usslp_attest     790   usslp_seq        526
usslp_render_policy 443+96 rodata      usslp_crc32c           234
                                                    total  21,392 B
```

That is x86-64 text; ARM Thumb-2 for this kind of integer code is typically
10–30% smaller. The estimate above rounds it to 20 KB rather than claiming the
saving. The other rows are engineering estimates from published NCS footprints
and have **not** been measured, because nothing here has been linked.

Turning off `CONFIG_USSLP_BLE` frees ~90 KB and is a real option for a store that
commissions over NFC only.

### RAM, static (2.9″ BWR build)

| Allocation | Bytes | Why it is that size |
|---|---:|---|
| ZBOSS stack (buffers, NIB/AIB, neighbour table) | ~28,672 | 24-entry neighbour table is the hardware's limit, not a choice |
| BLE host + controller | ~18,432 | Peripheral, one connection, minimum buffers |
| **OTA DEFLATE window** | 32,768 | Heap-allocated **only during an update**; see below |
| mbedTLS heap | 8,192 | Ed25519 verification's largest transient |
| Price decode scratch | 8,192 | Largest partial-refresh window, not a whole panel |
| **E-Ink framebuffer** | 9,472 | 296×128 × 2 planes ÷ 8. Packed, not 1 byte/pixel |
| Template band scratch | 4,800 | 600 px × 8 rows, the widest panel |
| Thread stacks (main 2K, price 4K, uplink 1K, sysworkq 2K, ZBOSS 2K, BLE 2K, ISR 1K, idle 320) | ~14,656 | |
| Message queues (price 4×160, uplink 4×96) | 1,024 | Shallow on purpose; see below |
| NVS cache, settings, key ring, provisioning, stats | ~3,072 | |
| Logging buffer | 1,024 | Deferred mode |
| **Total** | **~129 KB** | **50% of 256 KB** |

Four of those are worth their own sentence.

- **The framebuffer is packed** — two 1-bit planes, 9,472 bytes — where the Shelf
  Edge Controller keeps one byte per pixel. At one byte per pixel this panel is
  37,888 bytes and the 4.2″ tier is 120,000, which does not fit alongside two
  radio stacks. The controller can afford clarity; the label cannot.
- **The DEFLATE window is 32 KiB and there is no way around it.** RFC 1951
  back-references reach 32,768 bytes by definition, and a decoder with a smaller
  window fails on *some valid streams* — a rollout that works in test and fails
  in the field on whichever build happens to produce a long match. It is
  allocated from a dedicated `k_heap` for the ninety seconds an update takes
  rather than held statically for a decade.
- **The price decode scratch is 8 KiB, not a panel.** A partial refresh carries
  one band; only a full refresh carries the panel, and that path decodes and
  packs band by band.
- **The message queues are deliberately shallow.** Four price frames: a label
  more than four updates behind has a problem a deeper queue will not fix, and
  the sequence rule makes dropping the *oldest* safe, because only the highest
  sequence ever reaches the glass.

The 4.2″ tier swaps 9,472 bytes of framebuffer for 15,000 and is otherwise
identical. The 5.85″ tier has no framebuffer at all: 600×448 at 4 bits per pixel
is 134,400 bytes, which does not fit, so it streams bands to the panel
controller's own RAM.

---

## Design notes on the load-bearing parts

### The attestation digest

`crypto/usslp_canon.c` is a byte-for-byte port of
`AttestationInput.CanonicalString` in `platform/pkg/canon/attestation.go`. If
the two disagree by one character, every label in the fleet computes a digest
that does not match what the platform signed, refuses every update, and keeps
showing yesterday's price — correctly, quietly and forever. There is no telemetry
signature that distinguishes that from an attack.

So the port is literal, and `tests/test_canon.c` compares the **bytes**, not just
the hash of them, against vectors the Go implementation produced — including
pre-epoch timestamps, `INT64_MIN`, non-ASCII SKUs, zero- and three-decimal
currencies, and the separator-collision case where store `"ab"`/label `"c"` must
not hash the same as store `"a"`/label `"bc"`.

SHA-256 is compiled in from `crypto/usslp_sha256.c` rather than taken from PSA,
even though PSA has it. Two kilobytes of flash buys the guarantee that the code
the host tests hash with is the code that runs on the shelf.

### Where attestation is verified — a real tension, stated

INTERFACE-CONTRACTS §5 places attestation verification at the **Shelf Edge
Controller**, and `edge/labelsim/wire.go` says so explicitly: the JSON envelope
and the 64-byte signature stop at Tier 2, and what crosses the mesh is a sequence
number, a price and compressed pixels. That is a sound design and this firmware
implements it — frame type 1 is accepted on that basis.

It also leaves one hole. The contract's threat model says an attacker with write
access to the store's broker cannot change a displayed price; a controller that
has been *replaced or rooted* is inside the trust boundary that claim depends on.

So the firmware additionally defines **frame type 4**
(`USSLP_FRAME_ATTESTED_UPDATE`), which carries the signed tuple end to end and is
verified on the glass against a key ring the label syncs itself.

The edge tier has since implemented frame type 4 in `edge/sec` and
`edge/labelsim` and proved interop by compiling this firmware's actual decoder
(`usslp_wire.c`, `usslp_canon.c`, `usslp_sha256.c`, `usslp_crc32c.c`) against a
harness and feeding it a frame from the Go encoder: `attest_vectors[1]`
round-trips with the digest matching byte for byte. `CONFIG_USSLP_REQUIRE_
ATTESTATION` therefore defaults to `y`. A deployment whose controllers predate
that work sets it to `n`, which is a deliberate decision to trust Tier 2; the
label logs that loudly at every boot and exposes it as a cluster attribute so a
fleet audit can find every label running that way.

That interop work also found a real defect in this firmware's own test — see
[Protocol changes the controller must match](#protocol-changes-the-controller-must-match)
and the note on `usslp_wire_encode_attested_update`.

### Protocol changes the controller must match

Two changes to the ack frame. Both are **additive and backward compatible** — the
frame is still 20 bytes, and a controller that has not been updated behaves
exactly as it did before them — and `edge/` has adopted both:
`edge/labelsim/wire.go` defines the statuses and the verdict field,
`edge/labelsim/verdict.go` classifies a verification failure into a verdict,
`edge/sec/coordinator.go` carries them back on the ack, and
`edge/sec/controller.go` routes the two refusals to their opposite runbooks. The
inference described below survives only as a secondary fallback for firmware
that predates the codes.

**1. Two new ack status codes.**

| Code | Meaning | Runbook |
|---:|---|---|
| 0–2 | `applied`, `stale-sequence`, `bad-frame` — unchanged | |
| **3** | `refused: attestation did not verify` | Compliance incident |
| **4** | `refused: unattested frame, this build requires end-to-end` | Fleet configuration |

Previously the label reported both refusals as `bad-frame` (2), which is
indistinguishable from a corrupted radio frame. The edge tier worked around that
by inferring — a bad-frame ack to a frame the controller had already verified
must be a label refusal. `sec.Controller` still runs that inference, but only
after both status branches have been tried, and it marks the alert it raises
`label (inferred)` with an empty verdict. It is sound and lossy in both
directions:

- It **raises false compliance alerts.** A frame that is genuinely corrupted in
  flight, in a way that survives the mesh CRC and fails the image CRC-32C, looks
  identical to a refusal. The controller escalates a radio problem into a
  weights-and-measures process.
- It **cannot see the third case at all.** A label running
  `REQUIRE_ATTESTATION=y` that is sent a type-1 frame refuses every price in the
  zone. Under the inference that is a compliance incident per label per update,
  which buries the real ones — when the actual fault is that the controller needs
  updating. It is not a compliance problem at all.

The two failures have opposite runbooks, so they need different codes.

**2. The attestation verdict, in bits 2–4 of the existing flags byte.**

```
bit 0     partial refresh ran            (unchanged, labelsim's)
bit 1     forced full, ghosting budget   (unchanged, labelsim's)
bits 2-4  attestation verdict            (new)
bits 5-7  reserved, sent as zero
```

Eight verdicts: `ok`, `unsupported-algorithm`, `unknown-key-id`,
`key-outside-validity-window`, `digest-mismatch`, `bad-signature`,
`malformed-price-tuple`, `crypto-unavailable`. This is what lets an operator tell
a **stale key ring** (`unknown-key-id` — the label missed a rotation, fix the
ring) from **actual tampering** (`digest-mismatch` — the price on the wire is not
the price that was signed) without a round trip to ask. A `_Static_assert` breaks
the build if a ninth verdict is ever added, rather than letting it truncate into
one of the existing eight.

**Why it is safe in a mixed fleet.** The status is a byte with 251 unused values
and `labelsim.AckStatus.String` already maps everything it does not recognise to
`"bad-frame"` — so an un-updated controller sees precisely today's behaviour. The
verdict occupies bits of an existing byte that `labelsim.DecodeAck` masks off, so
the frame length does not change and an un-updated decoder is unaffected.
`tests/test_wire.c` asserts both properties, including that an *applied* ack still
encodes byte-identically to the Go-produced vector.

**The edge tier needs to match this** to read the codes. Until it does, nothing
breaks.

### The sequence rule and its commit ordering

`app/usslp_seq.c` holds the rule (strictly greater, or discard) and
`app/seq_store.c` holds the flash. The interesting property is neither: it is the
**order** in `app/price.c`.

```
1. decode          2. sequence check     3. attestation
4. decode + load   5. PERSIST SEQUENCE   6. drive the panel   7. ack
```

Step 5 is before step 6, and that is the whole design. A brownout during a 1.5 s
refresh is a real event on a coin cell driving a charge pump. Persisting first
means such a crash loses one price change and the retry is accepted. Persisting
*after* would leave new pixels on the glass with the old sequence in NVS — and
the retry would then be discarded as stale, leaving a label showing a price it
has told the platform it is not showing. That is precisely the state the whole
attestation apparatus exists to make impossible.

Step 2 is before step 3 for a cheaper reason: a duplicate is the common case
under at-least-once delivery, and an Ed25519 verification is 13 ms of a coin
cell's life. Checking the free invariant first costs nothing in safety.

### Partial refresh and the ghosting counter

`domain.DecideRender` decides whether a partial is *offered*; `labelsim.
planRefresh`, implemented in `display/usslp_render_policy.c`, decides whether one
*happens*. The label has the last word because only the label knows how many
partials actually reached the glass — the controller's count is an estimate that a
lost frame, a reboot or a manual refresh invalidates.

A disagreement in the controller's favour means a shopper can read the previous
price ghosted behind the current one. That is a weights-and-measures problem, not
a cosmetic one. The counter is persisted alongside the sequence for the same
reason: an E-Ink panel is bistable, so the residue survives a reboot even though
RAM does not.

The driver adds one thing the policy cannot know: **below about −10 °C there is
no partial waveform at all.** A single-phase drive does not complete and produces
a smeared digit. The driver falls back to a full refresh and reports `ForcedFull`,
so the controller's energy model learns it did not get what it asked for.

### OTA

Order: manifest → tier check → version check → chunks → delta apply → SHA-256 vs
manifest → structural check → mark → reset → confirm-or-revert.

The **tier check is first, before any flash is erased.** An image built for the
4.2″ panel flashed onto a 2.9″ one drives the wrong waveform tables, and a wrong
waveform can bake a permanent shadow into a panel — a failure not visible for
weeks and not recoverable at all.

One claim deliberately *not* made: the pre-swap check in
`usslp_slot_verify_signature` is **not** an authenticity check. Zephyr does not
expose MCUboot's verifier to the application, and moving it there would mean
moving the key there, which would defeat it. What that function checks is that
the assembled image is structurally an image — header magic, declared size
against bytes written, TLV trailer — plus the SHA-256 against the manifest. That
catches a corrupt transfer while the label is still awake instead of after two
reboots and a failed swap of a coin cell. A *forged* image passes it and fails at
boot, where MCUboot checks the signature, which is the correct division of
labour.

"Confirmed" means **joined the mesh and applied one price update**, not "booted".
An image that boots and cannot join is exactly as unreachable as one that does
not boot. Until both have happened the watchdog is armed and a reset reverts.

Delta patches use the platform's own USDELTA1 format, DEFLATE and all. The
applier is a streaming state machine fed by the inflater, because the
decompressed instruction stream is tens of kilobytes and the device has nowhere
to put it.

### The one probabilistic structure, and why it is allowed there

`ota/usslp_chunkmap.h` has both an exact bitmap and a Bloom filter, and the split
is the point. The bitmap tracks *which chunks this label holds* and must be
exact: a false positive there means a hole in the image, discovered only after the
whole transfer has been paid for. The Bloom filter tracks *which announcements
have already been relayed*, where a false positive suppresses one redundant
gossip path and costs nothing. 4,096 bits and 3 hashes gives 0.25% at 200
announcements; the tempting 1,024 bits gives 8.7%, which is 384 bytes saved for
nine per cent of the gossip suppressed.

### Fixed point, and why the FPU stays off

The nRF52840 *has* an FPU. It is not used, and the argument is power, not
capability: touching it sets `CONTROL.FPCA`, and from then on every exception
entry pushes 18 extra words — on a device that takes a radio interrupt tens of
times a minute for seven years. So the link-failure model
(`sec.FailureRisk`, a logistic regression over five features) is evaluated in
Q16.16 with an integer `exp2` and a fourth-order polynomial, and
`tests/test_route.c` asserts agreement with the Go model's float64 output to
within 1e-3 absolute probability — four orders of magnitude finer than the
decision threshold it feeds.

---

## What has and has not been verified

### Verified — built and executed on the host

25,961 assertions, zero failures, clean under ASan and UBSan. Every expected
value came from *running* the Go reference, not from transcribing it (see
`scripts/regen_vectors.md`).

| Module | What the tests actually establish |
|---|---|
| `crypto/usslp_canon.c` | Canonical string is byte-identical to `canon.CanonicalString` on 6 vectors incl. pre-epoch, `INT64_MIN`, UTF-8 SKUs, JPY/BHD; digest matches; streaming digest == hash of the built string; every signed field changes the digest; separators are unambiguous; malformed input refused rather than encoded differently |
| `crypto/usslp_sha256.c` | Known answers from `crypto/sha256`; incremental == one-shot at all 20,301 split points for lengths 0–200; context wiped |
| `crypto/usslp_keyring.c` | `pki.KeyIDFor` reproduced exactly; a key offered under an id its bytes do not produce is refused; validity windows; four slots |
| `crypto/usslp_attest.c` | Genuine attestation verifies; price change, sequence change, every single-bit signature flip (64) and digest flip (32) refused; a *genuine* signature by the wrong known key refused; unknown kid; expired key; strict vs lenient clock; distinct verdict strings |
| `app/usslp_seq.c` | Strictly-greater rule; duplicates counted not errored; check never advances; commit refuses to go backwards; NVS record round-trips incl. `INT64_MAX`/`INT64_MIN`; every single-bit corruption of the record (16×8) reads as never-displayed; erased flash likewise |
| `radio/usslp_wire.c` | Frames the Go encoder produced decode field-for-field and re-encode byte-identically; CRC-32C matches Castagnoli incl. incremental; truncation refused at every length; **all six** attestation vectors round-trip through encode→decode→canonicalise and produce the platform's digest, with per-identifier length and content checks; re-encoding a decoded frame is byte-identical; two vectors verify end-to-end against real Go signatures and fail on a one-bit price change; the encoder refuses every identifier the decoder would refuse; the new ack statuses and the verdict field survive a round trip without disturbing the existing wire |
| `display/usslp_rle.c` | An image `edge/sec` encoded decodes pixel-for-pixel; uvarint matches `binary.Uvarint` incl. overlong; row overrun, run overflow, zero repeat count, non-existent ink, trailing bytes all refused |
| `display/usslp_render_policy.c` | Panel table matches `labelsim`; ghosting budget forces a full on the 9th; a refresh that did not happen spends no budget; colour panel never partial; `domain.partialSafe` on every branch |
| `radio/usslp_route.c` | `LQIFromRSSI`, `LinkCost`, `Fragments`, `Airtime` match exactly; cost monotonic over all 255 LQI values; parent selection incl. depth tie-break, full parents, LQI floor, radius; fixed-point logistic within 1e-3 of `sec.FailureRisk` on 6 vectors; trend fit matches a double-precision least-squares fit; prediction fires on a moving link and not on a merely poor one |
| `power/usslp_budget.c` | All 18 Go projections (3 tiers × 3 temperatures × 2 profiles) to within 1 nA per component; the 8.67-year and 99-day figures; freezer miss; colour-panel miss; partial-fraction cap; sustainable-interval inverse; discharge curve monotone over 1,001 points |
| `ota/usslp_patch.c`, `usslp_inflate.c` | A patch `domain.Diff` produced reconstructs the target byte-for-byte; wrong base refused before any write; **every single-bit corruption of all 185 patch bytes**; truncation at every length; flash-write failure; hand-built stream exercising the interpreter across staging boundaries; copy past end of base, unknown opcode, over- and under-production all refused |
| `ota/usslp_chunkmap.c` | Bitmap exactness; a filter from a previous rollout cannot suppress this one; observed false-positive rate matches the design |

### Not verified

- **Nothing that includes a `zephyr/` header has been compiled.** That is
  `main.c`, the E-Ink driver, the waveform tables, the framebuffer packer, the
  templates, the Zigbee and BLE bindings, the PSA backend, the device
  certificate, the PMIC/gauge/harvest code, the NVS store, the OTA controller
  and slot handling, NFC, tamper, telemetry and provisioning. They are written
  against real Zephyr 3.x APIs but there will be compile errors.
- **Ed25519 curve arithmetic is not exercised.** The host backend is an oracle
  over triples that Go's `crypto/ed25519` actually produced, so the verifier's
  *decisions* are tested against genuine signatures — but field arithmetic on
  Curve25519 is PSA's job and PSA's test suite. `tests/fake_ed25519.c` says so at
  the top.
- **No timing has been measured.** The 13 ms Ed25519 figure, the 1.5 s waveform,
  the 8 ms beacon window and the airtime model are the platform's and the
  datasheets'; none has been observed on this firmware.
- **No power measurement.** Every current in this document is a blueprint or
  datasheet figure. The arithmetic over them is verified; the figures themselves
  are not.
- **The waveform LUTs have not been run on glass.** They are transcribed from
  the panel datasheets' recommended tables with one deliberate change (a fourth
  clear pass). A wrong waveform does not merely look bad — over-driving bakes a
  permanent shadow that is invisible for weeks — so any change there needs a
  temperature-chamber run and a thousand-cycle soak.
- **Flash and RAM figures other than the portable core's are estimates.**
  Nothing has been linked. `scripts/memory_report.sh` produces the real numbers
  once it can.
- **The ZBOSS calls in `radio/zigbee.c`** are written against the nRF Connect
  SDK's API surface and are the least confidently correct code here, because
  ZBOSS's API changes between NCS releases and there was no SDK to check against.

---

## Layout

```
firmware/
  CMakeLists.txt  Kconfig  prj.conf  VERSION
  boards/         devicetree overlay + one .conf per display tier
  src/
    usslp_portable.h   the Zephyr/portable boundary, and what it means
    crypto/            canonical string, SHA-256, key ring, verifier, PSA, identity
    display/           E-Ink driver, waveforms, framebuffer, image codec, policy, templates
    radio/             Zigbee, the USSLP cluster, BLE, air protocol, routing model
    power/             state machine, energy budget, PMIC, gauge, harvesting
    ota/               controller, slots, delta applier, inflater, chunk map
    nfc/  sensor/  app/
  tests/          host-buildable tests + Makefile (these run)
  scripts/        build.sh, flash.sh, memory_report.sh, regen_vectors.md
```

Files named `usslp_*.c` under `src/` are the portable core: no Zephyr, covered by
`tests/`. The `CORE` list in `tests/Makefile` and the first `target_sources` block
in `CMakeLists.txt` are the same list, deliberately — a file in one and not the
other is a review finding.
