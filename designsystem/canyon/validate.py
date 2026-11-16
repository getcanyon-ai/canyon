#!/usr/bin/env python3
"""Validate Canyon's design registry. Exit 0 clean, 1 with findings on stderr.

Checks: JSON parses with no duplicate keys · registry.json and r/ agree in both
directions (existence, title, description, no shadowed names) · every DTCG leaf
has $value, every {alias} resolves, no cycles, every color leaf is a plain hex
value, no resolved value carries CSS-breaking characters, the dashboard's light
and dark colour sets name the same leaves and every text pair in each clears
WCAG AA's 4.5:1 · theme items are data-only and
every dark/light cssVar is a hex value that traces to a token; theme-mode values
carry no CSS-breaking characters. Theme items trace against
themes/<name>/tokens.json when it exists, else the root tokens.json (Canyon's
own). dashboard.tokens.json is the dashboard's own set — gated here, published
nowhere. Run: python3 designsystem/canyon/validate.py
"""
import json
import pathlib
import re
import sys

BASE = pathlib.Path(__file__).parent
HEX = re.compile(r"^#[0-9a-fA-F]{6}([0-9a-fA-F]{2})?$")
CSS_BREAKERS = re.compile(r"[;{}]|url\(")
errors = []


def no_dupes(pairs):
    seen = {}
    for k, v in pairs:
        if k in seen:
            raise ValueError(f"duplicate key: {k}")
        seen[k] = v
    return seen


def load(path):
    try:
        return json.loads(path.read_text(), object_pairs_hook=no_dupes)
    except ValueError as e:
        errors.append(f"{path.relative_to(BASE)}: {e}")
        return None


registry = load(BASE / "registry.json")
tokens = load(BASE / "tokens.json")
items = {p.stem: load(p) for p in sorted((BASE / "r").glob("*.json"))}

if registry:
    names = [i["name"] for i in registry["items"]]
    for name in {n for n in names if names.count(n) > 1}:
        errors.append(f"registry.json lists '{name}' more than once")
    listed = {i["name"]: i for i in registry["items"]}
    for name, entry in listed.items():
        if name not in items:
            errors.append(f"registry.json lists '{name}' but r/{name}.json is missing")
        elif items[name]:
            for field in ("title", "description", "type"):
                if entry.get(field) != items[name].get(field):
                    errors.append(f"'{name}': {field} differs between registry.json and r/{name}.json")
    for name in items:
        if name not in listed:
            errors.append(f"r/{name}.json exists but registry.json does not list it")

def resolve_tokens(tokens_obj, label):
    resolved = {}

    def walk(node, path):
        if not isinstance(node, dict):
            return
        children = {k: v for k, v in node.items() if not k.startswith("$")}
        if not children and path:
            if "$value" not in node:
                errors.append(f"{label}: no $value at {path}")
            else:
                resolved[path] = node["$value"]
            return
        for k, v in children.items():
            walk(v, f"{path}.{k}" if path else k)

    walk(tokens_obj, "")
    for path in resolved:
        seen = set()
        while isinstance(resolved[path], str) and resolved[path].startswith("{"):
            target = resolved[path].strip("{}")
            if target in seen:
                errors.append(f"{label}: alias cycle at {path}")
                break
            seen.add(target)
            if target not in resolved:
                errors.append(f"{label}: {path} references unknown token {{{target}}}")
                break
            resolved[path] = resolved[target]
    return resolved


DASHBOARD = "dashboard.tokens.json"
token_files = {"tokens.json": tokens}
if (BASE / DASHBOARD).exists():
    token_files[DASHBOARD] = load(BASE / DASHBOARD)
for p in sorted(BASE.glob("themes/*/tokens.json")):
    if not p.resolve().is_relative_to(BASE.resolve()):
        errors.append(f"{p} escapes the registry directory (symlink?)")
        continue
    token_files[f"themes/{p.parent.name}/tokens.json"] = load(p)

resolved_by_file = {
    label: resolve_tokens(obj, label) for label, obj in token_files.items() if obj
}

for label, resolved in resolved_by_file.items():
    for path, v in resolved.items():
        if path.startswith("color.") and not (isinstance(v, str) and HEX.match(v)):
            errors.append(f"{label}: {path} = {v!r} is not a plain hex color")
        for part in v if isinstance(v, list) else [v]:
            if isinstance(part, str) and CSS_BREAKERS.search(part):
                errors.append(f"{label}: {path} = {part!r} contains CSS-breaking characters")

dash = resolved_by_file.get(DASHBOARD, {})
sides = {m: {p[len(f"color.{m}."):] for p in dash if p.startswith(f"color.{m}.")} for m in ("light", "dark")}
for leaf in sorted(sides["light"] ^ sides["dark"]):
    errors.append(f"{DASHBOARD}: color.{leaf} has no {'dark' if leaf in sides['light'] else 'light'} value")

def luminance(hex_value):
    rgb = [int(hex_value[i:i + 2], 16) / 255 for i in (1, 3, 5)]
    r, g, b = [c / 12.92 if c <= 0.04045 else ((c + 0.055) / 1.055) ** 2.4 for c in rgb]
    return 0.2126 * r + 0.7152 * g + 0.0722 * b

def contrast(a, b):
    hi, lo = sorted((luminance(a), luminance(b)), reverse=True)
    return (hi + 0.05) / (lo + 0.05)

# WCAG AA for body text, 4.5:1, in each set: every colour words are drawn in on the three grounds, each
# status and the accent on its own -dim fill, and the label on an accent fill. The accent is also the
# focus ring, which the same 4.5:1 on the grounds clears for UI's 3:1.
INKS = ("text", "text-dim", "text-faint", "accent", "link", "critical", "amber", "ok", "neutral", "beta")
PAIRS = [(ink, ground) for ink in INKS for ground in ("bg", "surface", "surface-2")] + \
        [(ink, f"{ink}-dim") for ink in ("accent", "critical", "amber", "ok", "neutral", "beta")] + [("accent-ink", "accent")]
for m in ("light", "dark") if dash else ():
    color = {leaf: dash[f"color.{m}.{leaf}"] for leaf in sides[m]}
    for ink, ground in PAIRS:
        if ink not in color or ground not in color:
            errors.append(f"{DASHBOARD}: color.{m} has no {ink if ink not in color else ground} to check contrast with")
        elif HEX.match(color[ink]) and HEX.match(color[ground]) and (ratio := contrast(color[ink][:7], color[ground][:7])) < 4.5:
            errors.append(f"{DASHBOARD}: color.{m}.{ink} on {ground} is {ratio:.2f}:1, under WCAG AA's 4.5:1")

for name, item in items.items():
    if not item:
        continue
    is_theme = item.get("type") == "registry:theme"
    if is_theme:
        for banned in ("files", "dependencies", "registryDependencies"):
            if banned in item:
                errors.append(f"{name}: theme items are data-only, found '{banned}'")
    css_vars = item.get("cssVars")
    if not css_vars:
        if is_theme:
            errors.append(f"{name}: theme item has no cssVars")
        continue
    if is_theme and name != "theme-canyon":
        label = f"themes/{name.removeprefix('theme-')}/tokens.json"
        if label not in resolved_by_file:
            errors.append(f"{name}: no {label} to trace against")
            continue
    else:
        label = "tokens.json"
    literals = {
        v.lower() for v in resolved_by_file.get(label, {}).values() if isinstance(v, str)
    }
    for mode in ("dark", "light"):
        for key, value in css_vars.get(mode, {}).items():
            if not HEX.match(value):
                errors.append(f"{name} {mode}.{key} = {value!r} is not a plain hex color")
            elif value.lower() not in literals:
                errors.append(f"{name} {mode}.{key} = {value} traces to no token in {label}")
    for key, value in css_vars.get("theme", {}).items():
        if CSS_BREAKERS.search(value):
            errors.append(f"{name} theme.{key} = {value!r} contains CSS-breaking characters")

for label in token_files:
    if label not in ("tokens.json", DASHBOARD):
        item_name = f"theme-{pathlib.Path(label).parent.name}"
        if item_name not in items:
            errors.append(f"{label} exists but r/{item_name}.json does not")

if errors:
    print("\n".join(errors), file=sys.stderr)
    sys.exit(1)
print("registry valid")
