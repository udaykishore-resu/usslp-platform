# USSLP edge deployment

The Store Gateway Unit and the Shelf Edge Controller are appliances, not pods.

```
systemd/          the primary deployment: hardened units, a target, an update timer
k3s/              the alternative, for boxes running other managed workloads too
config/           annotated environment templates
install.sh        installer, idempotent, versioned-directory layout
update.sh         update and rollback, with automatic revert
RUNBOOK.md        failure recovery, written for someone who has been woken up
```

---

## systemd or K3s?

Both are supported; neither is a fallback for the other.

**systemd** is right for the common case: one box, one gateway, a store with no
on-site technical staff. systemd is already there, the hardening directives are
stronger than anything a PodSecurity standard gives, and there is no cluster to
go wrong at 2 a.m. in a store in a different time zone.

**K3s** is right when the gateway hardware also runs other workloads the retailer
manages — shrink analytics, camera inference, queue management — and they want
one scheduler over all of it. Also for a large-format store with an
active/standby gateway pair, where the lease-based leader election in
`k3s/usslp-sgu.yaml` stops both bridging to the cloud at once.

---

## Installing

```bash
make build
sudo deploy/edge/install.sh \
  --store store-0001 --tenant acme --region us-east-1 \
  --controllers 25 \
  --cloud-broker tls://mqtt.us-east-1.usslp.example:8883
```

The installer will **not** generate keys. The key ring and the local price
authority come from the platform's key ceremony, because a box that mints its own
price-authority key can authorise its own prices — which defeats attestation
entirely. It warns loudly when they are absent, and the controllers will not
start without a key ring (`edge/cmd/sec` declares `USSLP_KEYRING_FILE` required).

It also will not overwrite an existing `/etc/usslp/*.env`. Re-running it on a
configured store must not silently reset its store id.

---

## The layout that makes rollback work

```
/usr/local/lib/usslp/
  v1.4.1/          usslp-sgu, usslp-sec
  v1.4.2/          usslp-sgu, usslp-sec
  current -> v1.4.2
  update.sh
  .previous        the rollback target
/usr/local/bin/usslp-sgu -> /usr/local/lib/usslp/current/usslp-sgu
```

Updating is: download, verify the SHA-256, install to a new directory, swap the
symlink atomically, restart, poll `/readyz`. Rolling back is: swap the symlink
back and restart.

The old version is still on disk, so **a rollback needs no network** — which
matters, because the most common reason to roll back a store gateway is that it
can no longer reach the network.

`update.sh` refuses to install a binary with no published digest, under any
circumstances including `--force`: there is no way to distinguish a corrupt
download from a hostile one. It also refuses to update a store that is currently
autonomous, because restarting the gateway then takes down the only thing pricing
that store's shelves.

---

## The randomised update window

`usslp-update.timer` fires at 02:00 local with up to four hours of jitter, and
`FixedRandomDelay=true` so a given store's slot is stable across reboots.

A fleet of 100,000 stores that all check at 03:00 is a thundering herd against
the artifact endpoint and — far worse — a fleet that all applies the same bad
update within the same minute, with no window in which anyone notices the first
few hundred failing and stops the rest.

Spread over four hours, a bad version reaches about 1% of the fleet in the first
two and a half minutes. The version pin in the manifest at
`USSLP_UPDATE_MANIFEST_URL` is what stops the rest: one central change, not
100,000 timers.

---

## Hardening

The threat model is a hostile store network and a physically accessible box.
Somebody can plug a laptop into the same switch; the cleaner can reach the
cabinet.

Both units run as `usslp` with `ProtectSystem=strict`, no capabilities at all,
`PrivateUsers`, `MemoryDenyWriteExecute`, a `@system-service` syscall filter, and
`RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX` — no raw sockets, so a
compromised gateway cannot become a sniffer for the store's network.

The controller unit is stricter in one specific way: `IPAddressAllow` permits
only RFC1918 space. A controller talks to exactly one thing — the store's broker,
inside the building — and has no business reaching the internet. A compromised
controller cannot exfiltrate a store's prices.

Neither unit needs `CAP_NET_BIND_SERVICE`: every port is above 1024.

---

## Ports

| | |
|---|---|
| 1883 | the store's MQTT broker, bound `0.0.0.0` so controllers on the LAN can reach it |
| 8090 | SGU diagnostics — `/status` `/mode` `/queue` `/secs` `/labels` `/rules` `/pos/price` |
| 9090 | obs admin — `/metrics` `/healthz` `/readyz` |

The diagnostics port defaults to `127.0.0.1` in the binary. Binding it to the LAN
is a decision with consequences: `/pos/price` accepts price changes and `/mode`
changes whether the store prices autonomously.

---

## Recovery

[RUNBOOK.md](RUNBOOK.md). It opens with a ninety-second triage table and the
distinction that matters most: **a store whose cloud link is down is not an
outage.** Find out whether the shelves are wrong or only the cloud's picture of
them, because those need completely different responses and the second one has no
urgency at all.
