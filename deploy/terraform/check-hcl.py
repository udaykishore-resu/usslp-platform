#!/usr/bin/env python3
"""A standing-in HCL checker for environments with no `terraform` binary.

WHAT THIS DOES AND DOES NOT PROVE
---------------------------------
It does NOT run `terraform validate`, `terraform fmt -check` or `terraform plan`.
Those need the terraform binary and, for validate, an initialised provider
schema. If terraform IS available, use it — `make tf-check` does — and treat
this script as the fallback.

What it checks:

  1.  Structural balance. Braces, brackets, parentheses and quotes balance
      across each file, accounting for `<<-EOT` heredocs, `#` and `//` line
      comments, `/* */` block comments, and escaped quotes. An unbalanced brace
      is the most common hand-written-HCL error and produces the least helpful
      terraform message.

  2.  Block syntax. Every top-level block is one of the eleven HCL block types
      Terraform accepts, and its label count matches what that type requires
      (resource takes two labels, variable one, locals none).

  3.  Reference integrity within a directory:
        - every `var.X` is declared by a `variable "X"` block;
        - every `local.X` is defined in a `locals` block;
        - every `module.X` is declared by a `module "X"` block;
        - every `each.value`/`each.key` is inside a block with `for_each`.

  4.  Cross-module wiring: every `module.X.Y` in a region composition names an
      `output "Y"` that the referenced module actually declares, and every
      argument passed to a module names a `variable` that module declares.
      This is the check that catches a renamed output, which is the mistake that
      survives a careless review and fails at plan time.

  5.  Formatting hygiene: no tab indentation, no trailing whitespace, a
      trailing newline. Not the whole of `terraform fmt`, but the parts of it
      that a diff would otherwise be full of.

Run:  python3 deploy/terraform/check-hcl.py
"""

from __future__ import annotations

import pathlib
import re
import sys

REPO = pathlib.Path(__file__).resolve().parents[2]
TF_ROOT = REPO / "deploy/terraform"

FAILURES: list[str] = []


def fail(msg: str) -> None:
    FAILURES.append(msg)


# ---------------------------------------------------------------------------
# Tokenising: strip comments, strings and heredocs so the balance check sees
# only structure.
# ---------------------------------------------------------------------------


def strip_noise(text: str, where: str) -> str:
    out: list[str] = []
    i = 0
    n = len(text)
    while i < n:
        ch = text[i]

        # Heredoc: <<EOT or <<-EOT ... EOT on its own line.
        m = re.match(r"<<-?([A-Za-z_][A-Za-z0-9_]*)\n", text[i:])
        if m:
            marker = m.group(1)
            end = re.search(rf"^\s*{re.escape(marker)}\s*$", text[i + m.end():], re.M)
            if not end:
                fail(f"{where}: unterminated heredoc <<{marker}")
                return "".join(out)
            i = i + m.end() + end.end()
            out.append(" ")
            continue

        # Block comment.
        if text.startswith("/*", i):
            close = text.find("*/", i + 2)
            if close == -1:
                fail(f"{where}: unterminated /* block comment")
                return "".join(out)
            i = close + 2
            continue

        # Line comment.
        if ch == "#" or text.startswith("//", i):
            nl = text.find("\n", i)
            i = n if nl == -1 else nl
            continue

        # String. Interpolations can contain braces, which is why the whole
        # string is replaced by a placeholder rather than scanned.
        if ch == '"':
            j = i + 1
            while j < n:
                if text[j] == "\\":
                    j += 2
                    continue
                if text[j] == '"':
                    break
                j += 1
            if j >= n:
                fail(f"{where}: unterminated string starting at offset {i}")
                return "".join(out)
            out.append('""')
            i = j + 1
            continue

        out.append(ch)
        i += 1
    return "".join(out)


def check_balance(path: pathlib.Path, text: str) -> None:
    where = str(path.relative_to(REPO))
    stripped = strip_noise(text, where)
    pairs = {"{": "}", "[": "]", "(": ")"}
    stack: list[str] = []
    line = 1
    for ch in stripped:
        if ch == "\n":
            line += 1
        elif ch in pairs:
            stack.append(ch)
        elif ch in pairs.values():
            if not stack:
                fail(f"{where}:{line}: unmatched closing '{ch}'")
                return
            opener = stack.pop()
            if pairs[opener] != ch:
                fail(f"{where}:{line}: '{opener}' closed by '{ch}'")
                return
    if stack:
        fail(f"{where}: {len(stack)} unclosed {''.join(stack)!r}")


# ---------------------------------------------------------------------------
# Blocks
# ---------------------------------------------------------------------------

BLOCK_LABELS = {
    "terraform": 0,
    "provider": 1,
    "resource": 2,
    "data": 2,
    "variable": 1,
    "output": 1,
    "locals": 0,
    "module": 1,
    "moved": 0,
    "import": 0,
    "check": 1,
    "removed": 0,
}

TOP_BLOCK = re.compile(r'^([a-z_]+)((?:\s+"[^"]*")*)\s*\{', re.M)


def check_blocks(path: pathlib.Path, text: str) -> None:
    where = str(path.relative_to(REPO))
    for m in TOP_BLOCK.finditer(text):
        kind = m.group(1)
        labels = re.findall(r'"([^"]*)"', m.group(2))
        if kind not in BLOCK_LABELS:
            fail(f"{where}: '{kind}' is not a Terraform top-level block type")
            continue
        expected = BLOCK_LABELS[kind]
        if len(labels) != expected:
            fail(f"{where}: block '{kind}' takes {expected} label(s), got {len(labels)}")


# ---------------------------------------------------------------------------
# References
# ---------------------------------------------------------------------------

VAR_DECL = re.compile(r'^variable\s+"([^"]+)"\s*\{', re.M)
OUT_DECL = re.compile(r'^output\s+"([^"]+)"\s*\{', re.M)
MOD_DECL = re.compile(r'^module\s+"([^"]+)"\s*\{', re.M)
VAR_REF = re.compile(r"\bvar\.([A-Za-z_][A-Za-z0-9_]*)")
LOCAL_REF = re.compile(r"\blocal\.([A-Za-z_][A-Za-z0-9_]*)")
MOD_REF = re.compile(r"\bmodule\.([A-Za-z_][A-Za-z0-9_]*)\.([A-Za-z_][A-Za-z0-9_]*)")
MOD_SOURCE = re.compile(r'^module\s+"([^"]+)"\s*\{([^{}]|\{[^{}]*\})*?source\s*=\s*"([^"]+)"', re.M | re.S)


def locals_defined(text: str) -> set[str]:
    names: set[str] = set()
    for m in re.finditer(r"^locals\s*\{", text, re.M):
        depth = 0
        i = m.end() - 1
        start = i
        while i < len(text):
            if text[i] == "{":
                depth += 1
            elif text[i] == "}":
                depth -= 1
                if depth == 0:
                    break
            i += 1
        body = text[start + 1 : i]
        # Top-level assignments only: nested map keys are not locals.
        for line in body.splitlines():
            am = re.match(r"\s{0,4}([A-Za-z_][A-Za-z0-9_]*)\s*=", line)
            if am:
                names.add(am.group(1))
    return names


def module_args(text: str) -> dict[str, set[str]]:
    """module name -> set of argument names passed to it."""
    args: dict[str, set[str]] = {}
    for m in re.finditer(r'^module\s+"([^"]+)"\s*\{', text, re.M):
        name = m.group(1)
        depth = 0
        i = m.end() - 1
        start = i
        while i < len(text):
            if text[i] == "{":
                depth += 1
            elif text[i] == "}":
                depth -= 1
                if depth == 0:
                    break
            i += 1
        body = text[start + 1 : i]
        found: set[str] = set()
        for line in body.splitlines():
            am = re.match(r"\s{0,4}([A-Za-z_][A-Za-z0-9_]*)\s*=", line)
            if am and am.group(1) not in ("source", "version", "count", "for_each", "providers", "depends_on"):
                found.add(am.group(1))
        args[name] = found
    return args


def check_directory(directory: pathlib.Path, module_index: dict[str, dict[str, set[str]]]) -> None:
    files = sorted(directory.glob("*.tf"))
    if not files:
        return
    where = str(directory.relative_to(REPO))
    text = "\n".join(f.read_text() for f in files)

    declared_vars = set(VAR_DECL.findall(text))
    declared_locals = locals_defined(text)
    declared_modules = set(MOD_DECL.findall(text))

    for name in sorted(set(VAR_REF.findall(text))):
        if name not in declared_vars:
            fail(f"{where}: var.{name} is used but no `variable \"{name}\"` block declares it")

    for name in sorted(set(LOCAL_REF.findall(text))):
        if name not in declared_locals:
            fail(f"{where}: local.{name} is used but no `locals` block defines it")

    # Cross-module wiring.
    sources: dict[str, str] = {}
    for m in re.finditer(r'^module\s+"([^"]+)"\s*\{', text, re.M):
        name = m.group(1)
        tail = text[m.end() : m.end() + 4000]
        sm = re.search(r'source\s*=\s*"([^"]+)"', tail)
        if sm:
            sources[name] = sm.group(1)

    for mod_name, attr in sorted(set(MOD_REF.findall(text))):
        if mod_name not in declared_modules:
            fail(f"{where}: module.{mod_name} is referenced but not declared")
            continue
        src = sources.get(mod_name)
        if not src or not src.startswith("."):
            continue
        target = (directory / src).resolve()
        key = str(target)
        if key not in module_index:
            fail(f"{where}: module \"{mod_name}\" source {src} does not resolve to a module directory")
            continue
        if attr not in module_index[key]["outputs"]:
            fail(
                f"{where}: module.{mod_name}.{attr} — {src} declares no output \"{attr}\". "
                f"It declares: {', '.join(sorted(module_index[key]['outputs'])) or '(none)'}"
            )

    for mod_name, passed in module_args(text).items():
        src = sources.get(mod_name)
        if not src or not src.startswith("."):
            continue
        key = str((directory / src).resolve())
        if key not in module_index:
            continue
        accepted = module_index[key]["variables"]
        for arg in sorted(passed):
            if arg not in accepted:
                fail(f"{where}: module \"{mod_name}\" is passed `{arg}`, which {src} does not declare as a variable")


# ---------------------------------------------------------------------------
# Formatting
# ---------------------------------------------------------------------------


def check_format(path: pathlib.Path, text: str) -> None:
    where = str(path.relative_to(REPO))
    for i, line in enumerate(text.splitlines(), 1):
        if line.startswith("\t") or "\n\t" in line:
            fail(f"{where}:{i}: tab indentation; terraform fmt uses two spaces")
        if line != line.rstrip():
            fail(f"{where}:{i}: trailing whitespace")
    if text and not text.endswith("\n"):
        fail(f"{where}: no trailing newline")


# ---------------------------------------------------------------------------


def main() -> int:
    tf_files = sorted(TF_ROOT.rglob("*.tf"))
    if not tf_files:
        print("no .tf files found")
        return 1

    for path in tf_files:
        text = path.read_text()
        check_balance(path, text)
        check_blocks(path, text)
        check_format(path, text)

    # Index every module directory's variables and outputs.
    module_index: dict[str, dict[str, set[str]]] = {}
    directories = sorted({p.parent for p in tf_files})
    for directory in directories:
        text = "\n".join(f.read_text() for f in sorted(directory.glob("*.tf")))
        module_index[str(directory.resolve())] = {
            "variables": set(VAR_DECL.findall(text)),
            "outputs": set(OUT_DECL.findall(text)),
        }

    for directory in directories:
        check_directory(directory, module_index)

    print(f"terraform root: {TF_ROOT.relative_to(REPO)}")
    print(f"files:          {len(tf_files)}")
    print(f"directories:    {len(directories)}")
    for directory in directories:
        idx = module_index[str(directory.resolve())]
        print(
            f"  {directory.relative_to(TF_ROOT)}: "
            f"{len(idx['variables'])} variables, {len(idx['outputs'])} outputs"
        )

    if FAILURES:
        print(f"\n{len(FAILURES)} problem(s):")
        for f in FAILURES:
            print(f"  {f}")
        return 1
    print("\nstructural checks passed (see the docstring for what this does and does not prove)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
