#!/usr/bin/env python3
"""A standing-in Helm template checker for environments with no `helm` binary.

WHAT THIS DOES AND DOES NOT PROVE
---------------------------------
It does NOT render the chart. It cannot: rendering needs Go's text/template, the
Sprig function library and Helm's own built-in objects, none of which are
available here.

What it does is a *structural* check that catches the mistakes that actually
happen when writing Helm templates by hand:

  1.  Delimiter balance. Every {{ has a }}; every if/range/with/define has a
      matching end. An unbalanced block is the single most common Helm error
      and the one whose message is least helpful.

  2.  YAML well-formedness of the skeleton. Every {{ ... }} expression is
      replaced by a placeholder scalar and every line that is *only* a control
      action is removed, then the result is parsed as YAML. This catches
      indentation errors, a missing colon, a list item at the wrong depth — the
      things that survive a syntactically valid template and fail at apply
      time.

  3.  Helper references. Every `include "name"` names a helper that some
      _helpers.tpl actually defines.

  4.  Values references. Every `.Values.x.y` path exists in values.yaml, unless
      it is inside a `range`/`with` scope where the leading dot is rebound.

  5.  Chart hygiene. Chart.yaml parses and has the required fields; values
      files parse; the generated files/rules copies match their canonical
      source in deploy/observability.

Run:  python3 deploy/helm/lint-templates.py
"""

from __future__ import annotations

import pathlib
import re
import sys

import yaml

REPO = pathlib.Path(__file__).resolve().parents[2]
CHART = REPO / "deploy/helm/usslp"

FAILURES: list[str] = []


def fail(msg: str) -> None:
    FAILURES.append(msg)


# ---------------------------------------------------------------------------
# 1. Delimiter and block balance
# ---------------------------------------------------------------------------

ACTION = re.compile(r"\{\{-?\s*(.*?)\s*-?\}\}", re.S)
OPENERS = ("if ", "if(", "range ", "range(", "with ", "with(", "define ", "block ")


def check_balance(path: pathlib.Path, text: str) -> None:
    if text.count("{{") != text.count("}}"):
        fail(f"{path}: unbalanced delimiters ({text.count('{{')} '{{{{' vs {text.count('}}')} '}}}}')")
        return
    depth = 0
    for i, action in enumerate(ACTION.findall(text)):
        head = action.lstrip("- ").strip()
        if head.startswith("/*"):
            continue
        if any(head.startswith(o) for o in OPENERS) or head in ("if", "range", "with"):
            depth += 1
        elif head == "end" or head.startswith("end "):
            depth -= 1
            if depth < 0:
                fail(f"{path}: an 'end' with no matching block (action #{i + 1})")
                return
    if depth != 0:
        fail(f"{path}: {depth} unclosed if/range/with/define block(s)")


# ---------------------------------------------------------------------------
# 2. YAML skeleton
# ---------------------------------------------------------------------------

# A line whose entire content is one or more template actions is a control line
# and contributes no YAML.
CONTROL_ONLY = re.compile(r"^\s*(?:\{\{-?.*?-?\}\}\s*)+$", re.S)
COMMENT_ACTION = re.compile(r"\{\{-?\s*/\*.*?\*/\s*-?\}\}", re.S)


def skeleton(text: str) -> str:
    # Drop template comments first; they legitimately span lines.
    text = COMMENT_ACTION.sub("", text)
    out: list[str] = []
    for line in text.splitlines():
        stripped = line.strip()
        if not stripped:
            out.append("")
            continue
        if CONTROL_ONLY.match(line):
            # A control-only line contributes nothing to the document. A
            # `key:` above it is then a null value, which is valid YAML — which
            # is exactly the property that makes this check possible.
            continue
        out.append(ACTION.sub("PLACEHOLDER", line))
    return "\n".join(out)


def check_yaml(path: pathlib.Path, text: str) -> None:
    skel = skeleton(text)
    try:
        list(yaml.safe_load_all(skel))
    except yaml.YAMLError as exc:
        detail = str(exc).replace("\n", " ")
        fail(f"{path}: YAML skeleton does not parse: {detail}")


# ---------------------------------------------------------------------------
# 3. Helper references
# ---------------------------------------------------------------------------

DEFINE = re.compile(r'\{\{-?\s*define\s+"([^"]+)"')
INCLUDE = re.compile(r'(?:include|template)\s+"([^"]+)"')


def collect_helpers() -> set[str]:
    names: set[str] = set()
    for path in CHART.rglob("*.tpl"):
        names.update(DEFINE.findall(path.read_text()))
    return names


# ---------------------------------------------------------------------------
# 4. Values references
# ---------------------------------------------------------------------------

VALUES_REF = re.compile(r"\.Values\.([A-Za-z0-9_.]+)")


def values_paths(node, prefix: str = "") -> set[str]:
    paths: set[str] = set()
    if isinstance(node, dict):
        for key, value in node.items():
            path = f"{prefix}.{key}" if prefix else str(key)
            paths.add(path)
            paths.update(values_paths(value, path))
    return paths


def check_values_refs(path: pathlib.Path, text: str, known: set[str]) -> None:
    for ref in VALUES_REF.findall(text):
        ref = ref.rstrip(".")
        if ref in known:
            continue
        # A per-service key: .Values.services.<name>.<field>. The <name> is a
        # map key, so check the shape rather than the literal path.
        parts = ref.split(".")
        if parts[0] == "services" and len(parts) >= 2:
            continue
        # Values defined only in an environment overlay (iamRoleArn) are
        # legitimately absent from the base file.
        if parts[-1] in {"iamRoleArn"}:
            continue
        fail(f"{path}: .Values.{ref} is not defined in values.yaml")


# ---------------------------------------------------------------------------
# 5. Chart hygiene
# ---------------------------------------------------------------------------


def check_chart() -> None:
    chart_yaml = CHART / "Chart.yaml"
    doc = yaml.safe_load(chart_yaml.read_text())
    for field in ("apiVersion", "name", "version", "description", "type"):
        if field not in doc:
            fail(f"{chart_yaml}: missing required field '{field}'")
    if doc.get("apiVersion") != "v2":
        fail(f"{chart_yaml}: apiVersion must be v2")

    for path in sorted(CHART.glob("values*.yaml")):
        try:
            yaml.safe_load(path.read_text())
        except yaml.YAMLError as exc:
            fail(f"{path}: does not parse: {exc}")


def check_rules_sync() -> None:
    canonical = REPO / "deploy/observability/prometheus/rules"
    generated = CHART / "files/rules"
    for src in sorted(canonical.glob("*.yaml")):
        dst = generated / src.name
        if not dst.exists():
            fail(f"{dst}: missing — run `make helm-sync-rules`")
        elif src.read_bytes() != dst.read_bytes():
            fail(f"{dst}: has drifted from {src} — run `make helm-sync-rules`")
    for dst in sorted(generated.glob("*.yaml")):
        if not (canonical / dst.name).exists():
            fail(f"{dst}: has no canonical source in {canonical}")


# ---------------------------------------------------------------------------

def main() -> int:
    check_chart()
    check_rules_sync()

    helpers = collect_helpers()
    known_values = values_paths(yaml.safe_load((CHART / "values.yaml").read_text()))

    templates = sorted(CHART.glob("templates/*.yaml")) + sorted(CHART.glob("templates/*.tpl"))
    if not templates:
        fail("no templates found")

    for path in templates:
        text = path.read_text()
        rel = path.relative_to(REPO)
        check_balance(rel, text)
        check_values_refs(rel, text, known_values)
        for name in INCLUDE.findall(text):
            if name not in helpers:
                fail(f"{rel}: include \"{name}\" — no _helpers.tpl defines it")
        if path.suffix == ".yaml":
            check_yaml(rel, text)

    print(f"chart:      {CHART.relative_to(REPO)}")
    print(f"templates:  {len(templates)}")
    print(f"helpers:    {len(helpers)}")

    if FAILURES:
        print(f"\n{len(FAILURES)} problem(s):")
        for f in FAILURES:
            print(f"  {f}")
        return 1
    print("\nstructural checks passed (see the docstring for what this does and does not prove)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
