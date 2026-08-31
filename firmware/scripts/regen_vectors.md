# Regenerating `tests/vectors.h`

`firmware/tests/vectors.h` holds the values the host tests compare against, and
every one of them was produced by *running the Go reference implementations*
rather than by transcribing them. That is the point of the file: the assertion
"the C canonical string is byte-identical to `canon.AttestationInput.
CanonicalString`" is only worth anything if the expected bytes came from that
function.

The file is checked in rather than generated at build time so the firmware tests
need nothing but a C compiler. Regenerate it whenever the Go side changes.

## What is in it, and which Go code produced it

| Vectors | Produced by |
|---|---|
| canonical strings, digests | `platform/pkg/canon.AttestationInput` |
| key ids, public keys, signatures | `platform/pkg/pki.KeyIDFor`, `crypto/ed25519` |
| SHA-256 known answers | `crypto/sha256` |
| UFB2 image and expected pixels | `edge/sec.Framebuffer.EncodeRLE` |
| update / ack / telemetry frames | `edge/labelsim.EncodeUpdate`, `EncodeAck`, `EncodeTelemetry` |
| USDELTA1 patch and its target | `platform/internal/ota/domain.Diff` |
| LQI, link cost, fragments, airtime | `edge/mesh.LQIFromRSSI`, `LinkCost`, `Fragments`, `Airtime` |
| link failure risks | `edge/sec.FailureRisk` |
| battery projections | `edge/labelsim.PowerProfile.Project` |

## The recipe

The generator lives outside the repository, because it must not become a Go file
inside a tree the firmware is not allowed to modify. Create it in a scratch
directory with its own module and a `replace` pointing at this checkout:

```
mkdir -p /tmp/usslp-vecgen && cd /tmp/usslp-vecgen
cat > go.mod <<'EOF'
module vecgen

go 1.24

require github.com/usslp/usslp v0.0.0

replace github.com/usslp/usslp => /path/to/usslp-build
EOF
```

`platform/internal/ota/domain` cannot be imported from another module — Go's
`internal` rule forbids it — so copy `delta.go` in as a local package. It depends
only on the standard library:

```
mkdir -p otadelta
sed 's/^package domain$/package otadelta/' \
  /path/to/usslp-build/platform/internal/ota/domain/delta.go > otadelta/delta.go
```

Then write `main.go` importing `canon`, `pki`, `edge/sec`, `edge/mesh`,
`edge/labelsim` and the local `vecgen/otadelta`, and print each value as a C
initialiser matching the structs in `tests/test_vectors.h`. Two details that
matter:

- **Non-ASCII bytes.** Identifiers can carry UTF-8 (`Käse-350g`), and a C hex
  escape swallows following hex digits. Emit each byte as `\xNN""` — the empty
  string literal terminates the escape — so `Käse` becomes
  `"K\xc3""\xa4""se"`.
- **Timestamps.** Emit `EffectiveAt.UTC().Unix()`, not a formatted string. The C
  side formats it itself, and the test's whole job is to check that the two
  formatters agree.

Finally:

```
go run . > vectors.h
cp vectors.h /path/to/usslp-build/firmware/tests/vectors.h
cd /path/to/usslp-build/firmware/tests && make
```

If a vector changes, the tests will say which one and print both the C output and
the Go expectation. A change in `vectors.h` that is not accompanied by a
deliberate change on the Go side is a wire break, not a test failure to paper
over.
