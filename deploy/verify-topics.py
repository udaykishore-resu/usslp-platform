#!/usr/bin/env python3
"""Verify that every transcription of the stream catalogue matches canon.AllStreams().

platform/pkg/canon/topics.go says it in its own comment: partition counts and
retention live with the topic definition rather than in a wiki, so that the
local development broker, the docker-compose profile and the Terraform that
provisions MSK all derive from one source of truth.

Go source cannot be imported into YAML, so the catalogue is transcribed into
four places. This script is what keeps the transcriptions honest:

    platform/pkg/canon/topics.go                          <- the source of truth
    deploy/helm/usslp/values.yaml                          topicProvisioning.streams
    deploy/compose/docker-compose.prod-like.yml            the kafka-topics job
    deploy/terraform/modules/msk/main.tf                   variable "streams"

A partition count that drifts is not a cosmetic problem. Changing it re-maps
every key to a different partition and silently destroys the per-key ordering
the whole platform is built on — which is why pkg/eventlog refuses a changed
partition count outright with ErrPartitionsChanged rather than applying it.

Run:  python3 deploy/verify-topics.py
"""

from __future__ import annotations

import pathlib
import re
import sys

import yaml

REPO = pathlib.Path(__file__).resolve().parents[1]

FAILURES: list[str] = []


def fail(msg: str) -> None:
    FAILURES.append(msg)


# ---------------------------------------------------------------------------
# The source of truth
#
# StreamPriceUpdates = Stream{"price-updates", 1024, 7 * 24, "...", false}
#
# The struct is positional: Name, Partitions, RetentionHours, Description,
# Compacted. The retention is written as an arithmetic expression, which is
# evaluated rather than pattern-matched, so `7 * 24` and `168` are the same
# thing to this script as they are to the compiler.
# ---------------------------------------------------------------------------

STREAM_LITERAL = re.compile(
    r'Stream\{\s*"(?P<name>[^"]+)"\s*,\s*'
    r"(?P<partitions>\d+)\s*,\s*"
    r"(?P<retention>[0-9*\s+]+?)\s*,\s*"
    r'"(?P<description>(?:[^"\\]|\\.)*)"\s*,\s*'
    r"(?P<compacted>true|false)\s*\}"
)


def canon_streams() -> dict[str, dict]:
    path = REPO / "platform/pkg/canon/topics.go"
    text = path.read_text()

    streams: dict[str, dict] = {}
    for m in STREAM_LITERAL.finditer(text):
        expr = m.group("retention")
        if not re.fullmatch(r"[0-9*\s+]+", expr):
            fail(f"topics.go: unexpected retention expression {expr!r}")
            continue
        streams[m.group("name")] = {
            "partitions": int(m.group("partitions")),
            "retention_hours": eval(expr, {"__builtins__": {}}, {}),  # digits and * + only
            "compacted": m.group("compacted") == "true",
        }

    # AllStreams() is what the platform actually provisions. A Stream literal
    # that is defined but not in AllStreams() would be a topic nothing creates.
    all_streams_body = re.search(
        r"func AllStreams\(\) \[\]Stream \{(.*?)\n\}", text, re.S
    )
    if not all_streams_body:
        fail("topics.go: could not find func AllStreams()")
        return streams

    referenced = set(re.findall(r"\bStream([A-Z][A-Za-z]*)\b", all_streams_body.group(1)))
    defined = dict(re.findall(r"\b(Stream[A-Z][A-Za-z]*)\s*=\s*Stream\{\s*\"([^\"]+)\"", text))

    in_all = {name for var, name in defined.items() if var[len("Stream"):] in referenced}
    for name in streams:
        if name not in in_all:
            fail(f"topics.go: stream {name!r} is defined but not returned by AllStreams()")

    return streams


# ---------------------------------------------------------------------------
# The transcriptions
# ---------------------------------------------------------------------------


def helm_streams() -> dict[str, dict]:
    path = REPO / "deploy/helm/usslp/values.yaml"
    doc = yaml.safe_load(path.read_text())
    return {
        s["name"]: {
            "partitions": s["partitions"],
            "retention_hours": s["retentionHours"],
            "compacted": s["compacted"],
        }
        for s in doc["topicProvisioning"]["streams"]
    }


COMPOSE_CREATE = re.compile(
    r"^\s*create\s+(?P<name>[a-z-]+)\s+"
    r"(?P<partitions>\d+)\s+"
    r"(?P<retention>\$\$\(\([^)]*\)\)|0)\s+"
    r"(?P<compacted>yes|no)\s*$",
    re.M,
)
COMPOSE_RETENTION = re.compile(r"\$\$\(\(\s*([0-9\s*]+?)\s*\)\)")


def compose_streams() -> dict[str, dict]:
    path = REPO / "deploy/compose/docker-compose.prod-like.yml"
    text = path.read_text()

    streams: dict[str, dict] = {}
    for m in COMPOSE_CREATE.finditer(text):
        raw = m.group("retention")
        if raw == "0":
            hours = 0
        else:
            inner = COMPOSE_RETENTION.match(raw)
            if not inner:
                fail(f"compose: could not parse retention {raw!r} for {m.group('name')}")
                continue
            # The compose job writes milliseconds: hours * 3600000.
            ms = eval(inner.group(1), {"__builtins__": {}}, {})
            if ms % 3_600_000 != 0:
                fail(f"compose: retention for {m.group('name')} is not a whole number of hours")
                continue
            hours = ms // 3_600_000
        streams[m.group("name")] = {
            "partitions": int(m.group("partitions")),
            "retention_hours": hours,
            "compacted": m.group("compacted") == "yes",
        }
    return streams


TF_STREAM = re.compile(
    r'\{\s*name\s*=\s*"(?P<name>[^"]+)"\s*,\s*'
    r"partitions\s*=\s*(?P<partitions>\d+)\s*,\s*"
    r"retention_hours\s*=\s*(?P<retention>\d+)\s*,\s*"
    r"compacted\s*=\s*(?P<compacted>true|false)\s*\}"
)


def terraform_streams() -> dict[str, dict]:
    path = REPO / "deploy/terraform/modules/msk/main.tf"
    text = path.read_text()
    return {
        m.group("name"): {
            "partitions": int(m.group("partitions")),
            "retention_hours": int(m.group("retention")),
            "compacted": m.group("compacted") == "true",
        }
        for m in TF_STREAM.finditer(text)
    }


# ---------------------------------------------------------------------------


def compare(source: dict[str, dict], other: dict[str, dict], label: str) -> None:
    missing = set(source) - set(other)
    extra = set(other) - set(source)

    for name in sorted(missing):
        fail(f"{label}: missing stream {name!r} (canon.AllStreams() has it)")
    for name in sorted(extra):
        fail(f"{label}: has stream {name!r}, which canon.AllStreams() does not")

    for name in sorted(set(source) & set(other)):
        for field in ("partitions", "retention_hours", "compacted"):
            if source[name][field] != other[name][field]:
                fail(
                    f"{label}: {name}.{field} is {other[name][field]!r}, "
                    f"canon says {source[name][field]!r}"
                )


def main() -> int:
    source = canon_streams()
    if not source:
        print("could not read any streams from platform/pkg/canon/topics.go", file=sys.stderr)
        return 1

    print(f"canon.AllStreams():  {len(source)} streams, {sum(s['partitions'] for s in source.values())} partitions")

    for label, fn in (
        ("deploy/helm/usslp/values.yaml", helm_streams),
        ("deploy/compose/docker-compose.prod-like.yml", compose_streams),
        ("deploy/terraform/modules/msk/main.tf", terraform_streams),
    ):
        try:
            other = fn()
        except Exception as exc:  # noqa: BLE001 — the failure is the report
            fail(f"{label}: could not be parsed: {exc}")
            continue
        print(f"  {label}: {len(other)} streams")
        compare(source, other, label)

    if FAILURES:
        print(f"\n{len(FAILURES)} discrepancy/ies:")
        for f in FAILURES:
            print(f"  {f}")
        print(
            "\nplatform/pkg/canon/topics.go is the source of truth. A partition count that "
            "drifts re-maps every key to a different partition and destroys the per-key "
            "ordering the platform is built on."
        )
        return 1

    print("\nevery transcription matches canon.AllStreams()")
    return 0


if __name__ == "__main__":
    sys.exit(main())
