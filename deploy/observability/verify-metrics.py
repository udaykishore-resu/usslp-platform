#!/usr/bin/env python3
"""Verify that every metric referenced by the Prometheus rules and the Grafana
dashboards is actually registered somewhere in the Go tree.

This exists because "do not invent metric names" is easy to say and easy to
violate: a plausible-looking name in an alert rule produces an expression that
evaluates to nothing, forever, silently. An alert that can never fire is worse
than no alert, because it looks like coverage.

What it does
------------
1.  Extracts every metric name registered through obs.Registry's Counter,
    Gauge and Histogram constructors, plus the string constants those calls
    reference (platform/pkg/mqtt/metrics.go registers via constants) and the
    namespaced families platform/pkg/kvstore builds from a caller-supplied
    prefix.
2.  Extracts every identifier that looks like a metric name from the rule files
    and dashboard JSON.
3.  Reports anything in (2) that is not explained by (1).

Known-good names that are not Go-registered are listed in ALLOWED below with
the reason: Prometheus' own `up`, the recording rules' own outputs, and the
third-party series exported by EMQX and Kafka Connect.

Run:  python3 deploy/observability/verify-metrics.py
      python3 deploy/observability/verify-metrics.py --list
"""

from __future__ import annotations

import json
import pathlib
import re
import sys

REPO = pathlib.Path(__file__).resolve().parents[2]

# ---------------------------------------------------------------------------
# 1. What the Go code registers
# ---------------------------------------------------------------------------

# r.Counter("name", ...) / r.Gauge("name", ...) / r.Histogram("name", ...)
DIRECT = re.compile(r'\.(?:Counter|Gauge|Histogram)\(\s*"([a-zA-Z_][a-zA-Z0-9_]*)"')

# The full registration call, so the label names can be read out of it too.
# Counter(name, help, labels...) and Gauge(name, help, labels...) put the labels
# straight after the help string; Histogram(name, help, buckets, labels...) has
# a bucket argument in between, which is skipped by taking only the quoted
# identifiers that follow the help string and look like label names.
REGISTRATION = re.compile(
    r'\.(?P<kind>Counter|Gauge|Histogram)\(\s*"(?P<name>[a-zA-Z_][a-zA-Z0-9_]*)"\s*,'
    r'(?P<rest>(?:[^()]|\([^()]*\))*?)\)',
    re.S,
)
LABEL_ARG = re.compile(r'"([a-z_][a-z0-9_]*)"')
# r.Gauge(metricBrokerConnected, ...) — a constant reference.
VIA_CONST = re.compile(r'\.(?:Counter|Gauge|Histogram)\(\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*,')
# const metricBrokerConnected = "usslp_mqtt_broker_connected_clients"
CONST_DECL = re.compile(r'^\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*=\s*"([a-z_][a-z0-9_]*)"\s*$', re.M)
# r.Gauge(ns+"_keys", ...) — a namespaced family.
NAMESPACED = re.compile(r'\.(?:Counter|Gauge|Histogram)\(\s*\w+\s*\+\s*"(_[a-z0-9_]+)"')
# MetricNamespace: "label_kvstore" — the prefixes those families are built with.
NS_VALUE = re.compile(r'MetricNamespace:\s*"([a-z0-9_]+)"')


GO_LABELS: dict[str, set[str]] = {}


def go_registered() -> tuple[set[str], set[str]]:
    """Return (exact names, namespaced-family suffix names)."""
    exact: set[str] = set()
    suffixes: set[str] = set()
    consts: dict[str, str] = {}
    const_refs: set[str] = set()
    namespaces: set[str] = set()

    for path in REPO.rglob("*.go"):
        if "/deploy/" in str(path):
            continue
        text = path.read_text(encoding="utf-8", errors="replace")
        exact.update(DIRECT.findall(text))
        for m in REGISTRATION.finditer(text):
            rest = m.group("rest")
            # Drop the help string (the first quoted argument after the name).
            quoted = LABEL_ARG.findall(rest)
            # The help text is a sentence, so it is the one containing a space.
            labels = [q for q in quoted if " " not in q]
            # A help string with no space (rare but possible) would be
            # misread as a label; requiring the identifier shape above plus the
            # absence of a space is as far as a regex can honestly go.
            GO_LABELS.setdefault(m.group("name"), set()).update(labels)
        const_refs.update(VIA_CONST.findall(text))
        suffixes.update(NAMESPACED.findall(text))
        namespaces.update(NS_VALUE.findall(text))
        for name, value in CONST_DECL.findall(text):
            consts[name] = value

    for ref in const_refs:
        if ref in consts:
            exact.add(consts[ref])

    # Expand the namespaced families into concrete names.
    for ns in namespaces:
        for suffix in suffixes:
            exact.add(ns + suffix)

    return exact, {s for s in suffixes}


# ---------------------------------------------------------------------------
# 2. What the rules and dashboards reference
# ---------------------------------------------------------------------------

# A PromQL metric selector: an identifier at the start of a selector, not
# preceded by a dot (which would make it a label) and not followed by "(" (which
# would make it a function).
PROMQL_NAME = re.compile(r'(?<![\w.:])([a-z_][a-z0-9_]*)(?=\s*[\{\[\)\s,+/*<>=!-]|$)', re.M)

# Label matchers and grouping clauses contain identifiers that are label names,
# not metric names. Both are stripped before the metric extractor runs;
# otherwise every `by (store)` reports `store` as an unregistered metric, which
# is exactly the kind of noise that gets a verification script switched off.
LABEL_MATCHER = re.compile(r'\{[^{}]*\}')
GROUPING = re.compile(r'\b(?:by|without|on|ignoring|group_left|group_right)\s*\([^()]*\)')


def strip_labels(expr: str) -> str:
    previous = None
    while previous != expr:
        previous = expr
        expr = LABEL_MATCHER.sub("{}", expr)
        expr = GROUPING.sub(" ", expr)
    return expr

PROMQL_FUNCS = {
    "rate", "irate", "increase", "sum", "avg", "min", "max", "count", "count_values",
    "stddev", "stdvar", "topk", "bottomk", "quantile", "histogram_quantile", "by", "on",
    "without", "ignoring", "group_left", "group_right", "and", "or", "unless", "offset",
    "abs", "absent", "absent_over_time", "ceil", "floor", "round", "clamp", "clamp_max",
    "clamp_min", "delta", "deriv", "exp", "ln", "log2", "log10", "sqrt", "predict_linear",
    "resets", "changes", "idelta", "holt_winters", "label_replace", "label_join",
    "time", "timestamp", "vector", "scalar", "sort", "sort_desc", "avg_over_time",
    "min_over_time", "max_over_time", "sum_over_time", "count_over_time",
    "quantile_over_time", "stddev_over_time", "stdvar_over_time", "last_over_time",
    "present_over_time", "group", "bool", "atan2", "year", "month", "day_of_month",
    "day_of_week", "days_in_month", "hour", "minute", "start", "end", "e",
}

# Names that are legitimately not registered in the Go tree.
ALLOWED = {
    # Prometheus' own per-target series.
    "up": "Prometheus target health, not a USSLP metric",
    "scrape_duration_seconds": "Prometheus scrape metadata",
    "scrape_samples_scraped": "Prometheus scrape metadata",
    # EMQX's own exposition, scraped by the `emqx` job.
    "emqx_connections_count": "EMQX 5.x exposition",
    "emqx_sessions_count": "EMQX 5.x exposition",
    "emqx_messages_dropped": "EMQX 5.x exposition",
    # Kafka Connect's JMX exposition.
    "kafka_connect_records_lag_max": "Kafka Connect JMX exporter",
    "kafka_consumer_fetch_manager_records_lag_max": "Kafka Connect JMX exporter",
    # Kubernetes state, from kube-state-metrics.
    "kube_deployment_status_replicas_available": "kube-state-metrics",
    "kube_horizontalpodautoscaler_status_current_replicas": "kube-state-metrics",
    "kube_horizontalpodautoscaler_spec_max_replicas": "kube-state-metrics",
    "kube_poddisruptionbudget_status_current_healthy": "kube-state-metrics",
    "kube_poddisruptionbudget_status_desired_healthy": "kube-state-metrics",
}

# Histogram families expose _bucket/_sum/_count; counters may be queried by
# their base name. Strip these before checking.
SUFFIXES = ("_bucket", "_sum", "_count")


def strip_suffix(name: str) -> str:
    for s in SUFFIXES:
        if name.endswith(s):
            return name[: -len(s)]
    return name


# obs.NewRuntime attaches these as const labels on every series it exports
# (platform/pkg/obs/runtime.go), and Prometheus/the ServiceMonitor add the rest.
AMBIENT_LABELS = {
    "service", "version", "region", "instance",   # obs const labels
    "job", "namespace", "pod", "node", "container", "usslp_service",
    "le", "quantile", "__name__", "environment", "optional",
    "deployment", "horizontalpodautoscaler", "poddisruptionbudget",
}

# metric -> set of label names used in a selector somewhere.
USED_LABELS: dict[str, set[str]] = {}

SELECTOR = re.compile(r"([a-z_][a-z0-9_]*)\{([^{}]*)\}")
LABEL_IN_SELECTOR = re.compile(r"([a-z_][a-z0-9_]*)\s*(?:=~|!~|=|!=)")


def note_selector_labels(expr: str) -> None:
    for m in SELECTOR.finditer(expr):
        metric, body = m.group(1), m.group(2)
        for label in LABEL_IN_SELECTOR.findall(body):
            USED_LABELS.setdefault(metric, set()).add(label)


def check_labels() -> dict[str, set[str]]:
    """metric -> labels used in a selector that the Go registration does not declare."""
    bad: dict[str, set[str]] = {}
    for metric, labels in sorted(USED_LABELS.items()):
        base = strip_suffix(metric)
        declared = GO_LABELS.get(base) or GO_LABELS.get(metric)
        if declared is None:
            # Not a Go-registered metric (a recording rule output, `up`, an
            # EMQX series); nothing to check against.
            continue
        unknown = {l for l in labels if l not in declared and l not in AMBIENT_LABELS}
        if unknown:
            bad[metric] = unknown
    return bad


def referenced() -> dict[str, set[str]]:
    """metric name -> set of files referencing it."""
    import yaml

    refs: dict[str, set[str]] = {}

    def note(name: str, where: str) -> None:
        refs.setdefault(name, set()).add(where)

    rules_dir = REPO / "deploy/observability/prometheus/rules"
    recorded: set[str] = set()

    for path in sorted(rules_dir.glob("*.yaml")):
        doc = yaml.safe_load(path.read_text())
        for group in doc.get("groups", []):
            for rule in group.get("rules", []):
                if "record" in rule:
                    recorded.add(rule["record"])
        for group in doc.get("groups", []):
            for rule in group.get("rules", []):
                note_selector_labels(rule["expr"])
                for name in PROMQL_NAME.findall(strip_labels(rule["expr"])):
                    note(name, path.name)

    dash_dir = REPO / "deploy/observability/grafana/dashboards"
    for path in sorted(dash_dir.glob("*.json")):
        doc = json.loads(path.read_text())
        for expr in find_exprs(doc):
            note_selector_labels(expr)
            for name in PROMQL_NAME.findall(strip_labels(expr)):
                note(name, path.name)

    # A recording rule's own output is a legitimate reference elsewhere.
    for name in recorded:
        refs.pop(name, None)
    return refs


def find_exprs(node) -> list[str]:
    out: list[str] = []
    if isinstance(node, dict):
        for k, v in node.items():
            if k == "expr" and isinstance(v, str):
                out.append(v)
            else:
                out.extend(find_exprs(v))
    elif isinstance(node, list):
        for v in node:
            out.extend(find_exprs(v))
    return out


def main() -> int:
    exact, _ = go_registered()

    if "--list" in sys.argv:
        for name in sorted(exact):
            print(name)
        return 0

    refs = referenced()
    unknown: dict[str, set[str]] = {}
    for name, where in sorted(refs.items()):
        if name in PROMQL_FUNCS or name in ALLOWED:
            continue
        if name.startswith("usslp:") or ":" in name:
            continue
        base = strip_suffix(name)
        if base in exact or name in exact:
            continue
        # Recording-rule outputs consumed by the HPAs are plain names.
        if name in {"usslp_requests_per_second", "ota_downloads_in_flight_per_pod"}:
            continue
        unknown[name] = where

    bad_labels = check_labels()

    print(f"registered in Go:  {len(exact)}")
    print(f"referenced:        {len(refs)}")
    print(f"label sets known:  {len(GO_LABELS)}")

    if bad_labels:
        print("\nSELECTORS USING LABELS THE METRIC DOES NOT DECLARE:")
        for metric, labels in sorted(bad_labels.items()):
            declared = sorted(GO_LABELS.get(strip_suffix(metric), set()))
            print(f"  {metric}{{{', '.join(sorted(labels))}}}")
            print(f"      declares: {', '.join(declared) or '(none)'}")
        print(
            "\n  A selector on a label the metric does not have matches every series "
            "(an absent label compares equal to \"\"), so an alert written this way "
            "either never fires or always fires."
        )

    if unknown:
        print("\nNOT REGISTERED ANYWHERE IN THE GO TREE:")
        for name, where in sorted(unknown.items()):
            print(f"  {name}\n      referenced by: {', '.join(sorted(where))}")
        return 1
    if bad_labels:
        return 1
    print("\nevery referenced metric is registered in the Go tree or explicitly allow-listed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
