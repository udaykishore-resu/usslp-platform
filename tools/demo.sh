#!/usr/bin/env bash
#
# tools/demo.sh — a scripted five-minute narrative through a running USSLP.
#
# It boots usslpd on ephemeral ports, drives the platform through usslpctl, and
# prints the latencies it actually measured. Everything it does goes through the
# platform's own surfaces: a price change is a signed Shopify webhook through
# the Universal Integration Gateway, a promotion goes through the Promotion
# Service, and the WAN outage is a severed TCP link rather than a flag.
#
# Run it with `make demo`.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="${ROOT}/bin"
WORK="$(mktemp -d)"
CONTROLLERS="${DEMO_CONTROLLERS:-3}"
LABELS="${DEMO_LABELS:-12}"

# Colours, but only when a person is watching. A log file full of escape
# sequences helps nobody.
if [[ -t 1 ]]; then
  B=$'\033[1m'; DIM=$'\033[2m'; OFF=$'\033[0m'
else
  B=''; DIM=''; OFF=''
fi

cleanup() {
  if [[ -n "${USSLPD_PID:-}" ]] && kill -0 "$USSLPD_PID" 2>/dev/null; then
    kill "$USSLPD_PID" 2>/dev/null || true
    wait "$USSLPD_PID" 2>/dev/null || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

say() { printf '\n%s==> %s%s\n' "$B" "$1" "$OFF"; }
note() { printf '%s    %s%s\n' "$DIM" "$1" "$OFF"; }
pause() { sleep "${1:-1}"; }

# await_price polls the fleet until one label is showing a price, or gives up.
#
# It is a poll rather than a sleep because the one step of this demo whose
# duration is genuinely unknown is the recovery from a WAN outage: the gateway
# has to notice the link is back, settle, reconcile and then flush, and a fixed
# sleep either makes the demo slower than it needs to be or prints a shelf that
# has not caught up yet and calls it a success. Every other pause here is
# cosmetic; this one is a fact being waited on.
await_price() { # label price timeout_seconds
  local label="$1" want="$2" limit="${3:-30}" i
  for ((i = 0; i < limit * 4; i++)); do
    if "$CTL" labels --store "$STORE" --limit 0 --json | python3 -c '
import json, sys
label, want = sys.argv[1], sys.argv[2]
for l in json.load(sys.stdin):
    if l["label_id"] == label and l["displayed_price"].lstrip("$") == want:
        sys.exit(0)
sys.exit(1)
' "$label" "$want"; then
      return 0
    fi
    sleep 0.25
  done
  return 1
}

# await_sec_offline polls until the gateway has marked a controller offline.
#
# The gateway learns this from the controller's last will, which the broker
# publishes only once it has decided the session is gone — a keep-alive timeout,
# not an instant. Waiting for the fact rather than sleeping past it is also the
# demonstration: the point of the step is that nobody told the gateway.
await_sec_offline() { # sec_id timeout_seconds
  local sec="$1" limit="${2:-30}" i
  for ((i = 0; i < limit * 4; i++)); do
    if "$CTL" stores --json | python3 -c '
import json, sys
sec = sys.argv[1]
for s in json.load(sys.stdin):
    for c in s.get("controllers") or []:
        if c["sec_id"] == sec and not c["online"]:
            sys.exit(0)
sys.exit(1)
' "$sec"; then
      return 0
    fi
    sleep 0.25
  done
  return 1
}

# await_measurement polls until the SLO window has recorded at least N
# deliveries.
#
# It is a separate wait from await_price because the two facts arrive in that
# order and not at the same instant: the controller marks the label delivered —
# which is when the glass reads correct — then persists the framebuffer, then
# publishes label.delivered at QoS 1 and waits for the broker to acknowledge it,
# and only then hands the timings to whatever is observing. A caller that drives
# changes back to back against the glass therefore runs several measurements
# ahead of the record, and asking for the report at that moment gets a report of
# only some of what it did.
#
# Waiting for the count rather than for the first one is also the assertion: a
# price change that reached a shelf and produced no measurement is a change
# nobody can prove happened.
await_measurement() { # wanted timeout_seconds
  local want="${1:-1}" limit="${2:-10}" i
  for ((i = 0; i < limit * 4; i++)); do
    if "$CTL" slo --store "$STORE" --json | python3 -c '
import json, sys
sys.exit(0 if json.load(sys.stdin)["measured"]["deliveries"] >= int(sys.argv[1]) else 1)
' "$want"; then
      return 0
    fi
    sleep 0.25
  done
  return 1
}

# ---------------------------------------------------------------------------
say "Building usslpd and usslpctl"
mkdir -p "$BIN"
( cd "$ROOT" && go build -o "$BIN/usslpd" ./platform/cmd/usslpd )
( cd "$ROOT" && go build -o "$BIN/usslpctl" ./tools/usslpctl )
note "built into $BIN"

# ---------------------------------------------------------------------------
say "Booting the whole platform in one process"
note "eight cloud services, a cloud MQTT broker, a certificate hierarchy,"
note "a store gateway, $CONTROLLERS shelf edge controllers and $((CONTROLLERS * LABELS)) simulated labels"

"$BIN/usslpd" --ephemeral \
  --status-file "$WORK/status.json" \
  --controllers "$CONTROLLERS" --labels "$LABELS" --log-level error \
  > "$WORK/usslpd.log" 2>&1 &
USSLPD_PID=$!

# usslpd writes its status file only once it is fully ready — every device
# provisioned, every shelf priced, every panel settled — so waiting for the file
# is waiting for a store that is open for trade. It is a file rather than
# stdout because every service's log goes to stdout too.
ready=0
for _ in $(seq 1 240); do
  if [[ -s "$WORK/status.json" ]]; then ready=1; break; fi
  if ! kill -0 "$USSLPD_PID" 2>/dev/null; then
    echo "usslpd exited during start-up:" >&2
    tail -20 "$WORK/usslpd.log" >&2
    exit 1
  fi
  sleep 0.5
done
if [[ "$ready" -ne 1 ]]; then
  echo "usslpd did not become ready within two minutes:" >&2
  tail -20 "$WORK/usslpd.log" >&2
  exit 1
fi

read -r CONTROL API BOOT_MS STORE_COUNT LABEL_COUNT < <(python3 - "$WORK/status.json" <<'PY'
import json, sys
s = json.load(open(sys.argv[1]))
print(s["endpoints"]["control"], s["endpoints"]["api-gateway"],
      s["boot_ms"], s["stores"], s["labels"])
PY
)
export USSLP_CONTROL_URL="$CONTROL" USSLP_API_URL="$API"
CTL="$BIN/usslpctl"

note "booted in ${BOOT_MS} ms: ${STORE_COUNT} store, ${LABEL_COUNT} labels, all priced"
note "control $CONTROL   api $API"

STORE="$("$CTL" stores --json | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["store_id"])')"

# ---------------------------------------------------------------------------
say "1. What is running"
"$CTL" status | sed -n '1,8p'
"$CTL" stores

# ---------------------------------------------------------------------------
say "2. The shelves, before"
"$CTL" labels --limit 5
read -r SKU LABEL WAS NEW < <("$CTL" labels --limit 1 --json | python3 -c '
import json, sys
l = json.load(sys.stdin)[0]
was = float(l["displayed_price"].lstrip("$"))
# 20% off. The new price is derived from the old one rather than picked out of
# the air because the Label Service refuses a change of more than five times the
# current price as a corrupt feed — which is correct, and which a demo that
# invented a price would trip over in front of an audience.
print(l["sku"], l["label_id"], "%.2f" % was, "%.2f" % round(was * 0.8, 2))
')

# ---------------------------------------------------------------------------
say "3. A price change, POS to glass"
note "this goes in through the real Shopify webhook adapter: HMAC verified,"
note "deduplicated, normalised, durably appended, signed with the tenant's"
note "Ed25519 price-authority key, and verified again by the controller."
note ""
note "The measurement window is reset first. The store has just opened, so its"
note "record so far is one store-wide fan-out — every label at once, which"
note "saturates the radio by construction. That is a real number and it belongs"
note "to step 4, not to a single price change."
"$CTL" slo --reset
"$CTL" price set --store "$STORE" --sku "$SKU" --price "$NEW" --was "$WAS"
pause 3
"$CTL" labels --limit 3
note "$LABEL was \$$WAS and is now showing \$$NEW"

say "   Measured latency, against the contract's budget"
"$CTL" slo --store "$STORE"

# ---------------------------------------------------------------------------
say "4. A store-wide repricing"
python3 - "$WORK/prices.json" <<PY
import json, subprocess, sys
out = subprocess.run(["$CTL", "labels", "--store", "$STORE", "--limit", "0", "--json"],
                     capture_output=True, text=True, check=True).stdout
labels = json.loads(out)
# Every price moves by a fifth, derived from what the shelf is showing, so the
# batch exercises the fan-out rather than the guard rail.
rows = []
for l in labels:
    cur = float(l["displayed_price"].lstrip("$"))
    rows.append({"sku": l["sku"], "price": "%.2f" % round(cur * 0.8, 2),
                 "was_price": "%.2f" % cur})
json.dump(rows, open(sys.argv[1], "w"))
print("  %d shelf positions" % len(rows))
PY
"$CTL" slo --reset
"$CTL" price batch --store "$STORE" --file "$WORK/prices.json"
note "waiting for every panel to finish its waveform..."
pause 8
"$CTL" labels --limit 5
"$CTL" slo --store "$STORE" | sed -n '2,5p'
note ""
note "Note the tail. A store-wide fan-out asks every panel in the store to run a"
note "waveform at once, and a controller transmits at most eight at a time, so the"
note "last labels in the queue wait for the ones ahead of them. The three-second"
note "budget is a statement about a price change, not about a store held at"
note "saturation; test/load measures exactly where that line is."

# ---------------------------------------------------------------------------
say "5. The claim itself: price changes on a settled store"
note "Twelve of them, one at a time, each on a different shelf position and each"
note "waited for before the next is sent. Twelve rather than one because a single"
note "sample is not a distribution: the mesh occasionally loses an acknowledgement"
note "and retries the delivery, which costs seconds, and with n=1 that tail event"
note "*is* the headline. Twelve is still small — test/e2e runs a thousand — but it"
note "is enough that one retry shows up as a tail rather than as the claim."
note ""
note "It is measured here, before anything is broken, because that is what the"
note "claim is about: a price change on a store that is working. What happens to"
note "the same number during an outage is step 9, and the two are not the same"
note "experiment."
"$CTL" slo --reset
sent=0
while read -r target_sku target_label target_price; do
  "$CTL" price set --store "$STORE" --sku "$target_sku" --price "$target_price" >/dev/null
  if ! await_price "$target_label" "$target_price" 30; then
    note "$target_label did not reach $target_price within 30 s; that is a failure"
    exit 1
  fi
  sent=$((sent + 1))
done < <("$CTL" labels --store "$STORE" --limit 12 --json | python3 -c '
import json, sys
# Each price moves by a tenth, derived from what the shelf is showing: the
# Label Service refuses a change of more than five times the current price as a
# corrupt feed, and a demo that invented prices would trip over that rule.
for l in json.load(sys.stdin):
    cur = float(l["displayed_price"].lstrip("$"))
    print(l["sku"], l["label_id"], "%.2f" % round(cur * 0.9, 2))
')
if await_measurement "$sent" 20; then
  note "$sent price changes delivered and measured, one at a time"
  "$CTL" slo --store "$STORE"
else
  note "fewer than $sent measurements were recorded; that is a failure"
  "$CTL" slo --store "$STORE"
  exit 1
fi

# ---------------------------------------------------------------------------
say "6. The WAN goes down"
note "the store's uplink is severed for real — every TCP connection closed and"
note "new ones refused — so the gateway has to notice on its own."
# A fresh window, so that what step 9 reports is the outage and nothing else.
# Step 5 measured the platform working; this measures what an outage costs, and
# averaging the two together would describe neither.
"$CTL" slo --reset >/dev/null
"$CTL" chaos wan-outage --store "$STORE"
pause 4
"$CTL" stores | sed -n '1,3p'
note "the store is autonomous; its labels have not changed and nothing is blank"
"$CTL" labels --limit 3

say "   A price change made while the store is unreachable"
OUTAGE_PRICE="$(python3 -c "print('%.2f' % round($NEW * 0.9, 2))")"
"$CTL" price set --store "$STORE" --sku "$SKU" --price "$OUTAGE_PRICE" || true
pause 3
"$CTL" stores | sed -n '1,3p'
note "the change is buffered upstream; the shelf still shows the last verified price"

say "7. The WAN comes back"
"$CTL" chaos wan-outage --store "$STORE" --restore
if await_price "$LABEL" "$OUTAGE_PRICE" 30 && await_measurement 1 20; then
  "$CTL" stores | sed -n '1,3p'
  "$CTL" labels --limit 3
  note "the buffer flushed, the store reconciled, and \$$OUTAGE_PRICE is on the glass"
else
  "$CTL" stores | sed -n '1,3p'
  "$CTL" labels --limit 3
  note "the buffered change had not reached the glass after 30 s; that is a failure"
  exit 1
fi

# ---------------------------------------------------------------------------
say "8. Killing a controller"
SEC="$("$CTL" stores --json | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["controllers"][-1]["sec_id"])')"
"$CTL" chaos kill-sec --sec "$SEC"
if await_sec_offline "$SEC" 30; then
  "$CTL" stores | sed -n '/CONTROLLERS/,$p' | head -8
  note "the gateway learned about it from the controller's retained last will"
else
  "$CTL" stores | sed -n '/CONTROLLERS/,$p' | head -8
  note "the gateway had not noticed after 30 s; that is a failure"
  exit 1
fi

# ---------------------------------------------------------------------------
say "9. Everything that just happened, measured"
note "A separate window from step 5, opened at the moment the WAN was cut, so"
note "what it holds is the price change that spent the outage buffered."
note ""
note "It is over three seconds, and it should be. The SLO clock starts when the"
note "cloud takes durable responsibility for a change, and the store could not be"
note "reached for most of what followed — so the outage is inside the number. The"
note "three-second claim is about a store the platform can reach; the claim about"
note "a store it cannot reach is the one steps 6 to 8 just demonstrated, which is"
note "that the shelves keep trading, nothing goes blank, and the change is held"
note "durably and applied in order the moment the link returns. Reporting this"
note "number rather than resetting the window is the difference between the two"
note "claims being separate and one of them quietly covering for the other."
"$CTL" slo


say "Done"
note "usslpd is still running on $CONTROL; it will be stopped when this script exits."
note "Run it yourself with:  make run     then:  bin/usslpctl status"
