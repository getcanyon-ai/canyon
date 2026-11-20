#!/usr/bin/env python3
"""The contracts' own gate. Exit 0 clean, 1 with findings on stderr.

One function per seam; each validates the golden fixture under contracts/testdata/<seam>/
against contracts/schema/<seam>.schema.json and then checks the invariants the contract
text states but a schema cannot express. Run: python3 contracts/validate.py
Needs: jsonschema (pip install jsonschema==4.23.0).
"""
import base64
import copy
import datetime
import hashlib
import hmac
import ipaddress
import json
import pathlib
import re
import sys
import unicodedata
import urllib.parse

from jsonschema import Draft7Validator, validators
from referencing import Registry
from referencing.jsonschema import DRAFT7

ROOT = pathlib.Path(__file__).resolve().parent.parent
CONTRACTS = ROOT / "contracts"
errors = []


def load(path):
    try:
        return json.loads(path.read_text())
    except ValueError as e:
        errors.append(f"{path.relative_to(ROOT)}: {e}")
        return None


# Every contract schema under its own $id, so one schema's $ref to another's (app-page's and fleet's
# envs.<env> to release.schema.json's env_pending) resolves from this directory, never the network.
SCHEMAS = Registry().with_resources((s["$id"], DRAFT7.create_resource(s))
                                    for s in (json.loads(p.read_text()) for p in sorted((CONTRACTS / "schema").glob("*.schema.json"))))


def schema_check(label, schema_path, doc, name=None):
    """`name` renames an error's location — a seam whose rows have ids says which row, not which index."""
    schema = load(schema_path)
    if schema is None or doc is None:
        return False
    Draft7Validator.check_schema(schema)
    bad = sorted(Draft7Validator(schema, registry=SCHEMAS).iter_errors(doc), key=lambda e: list(e.path))
    for e in bad:
        where = (name and name(list(e.path))) or "/".join(str(p) for p in e.path) or "<root>"
        errors.append(f"{label}: {where}: {e.message}")
    return not bad


def definition_check(label, schema_path, definition, doc):
    """schema_check against one definition of a schema, reached by $ref through the registry."""
    schema = load(schema_path)
    if schema is None or doc is None:
        return False
    bad = sorted(Draft7Validator({"$ref": f"{schema['$id']}#/definitions/{definition}"}, registry=SCHEMAS).iter_errors(doc), key=lambda e: list(e.path))
    for e in bad:
        errors.append(f"{label}: {'/'.join(str(p) for p in e.path) or '<root>'}: {e.message}")
    return not bad


def accepts(schema_path, doc):
    """Whether a schema takes a probe; the probe's errors are its own, never the fixture's."""
    mark = len(errors)
    schema_check("selftest", schema_path, doc)
    ok = len(errors) == mark
    del errors[mark:]
    return ok


def takes(schema_path, definition, doc):
    """accepts, against one definition of a schema."""
    mark = len(errors)
    definition_check("selftest", schema_path, definition, doc)
    ok = len(errors) == mark
    del errors[mark:]
    return ok


# ---------------------------------------------------------------- seam 1 — the finding
SARIF_SCHEMA = "vendor/sarif-2.1.0.schema.json"   # provenance + refresh: contracts/vendor/README.md
SARIF_SCHEMA_SHA256 = "981917f597e8c22da30a5c79b55c9dc9a7b9092b359fa12c083aca69fcfbdcc7"
LEVEL = {"critical": "error", "high": "error", "medium": "warning", "low": "note"}
# snapshot.md: a finding on a Dockerfile never blocks — checkov's dockerfile checks, Canyon building from no Dockerfile.
DOCKERFILE_CHECKS = ("iac.misconfig.checkov.CKV_DOCKER_", "iac.misconfig.checkov.CKV2_DOCKER_")
NOT_READ_DOCKERFILE = ". Canyon builds this app with buildpacks and never builds from this Dockerfile, so this does not block; it matters only where something else builds from it."  # finding.md
LIVE_KEY = re.compile(r"[rs]k_live_[A-Za-z0-9]{20,}")
LINE_BREAK = re.compile("[\\r\\n\\u2028\\u2029]")  # finding.md: a do_not entry is one line; escapes, so an editor cannot normalise them away
MAX_SNIPPET_LINES = 3  # finding.md: a snippet is the span's lines, ≤3 as serialized
# tool.driver.name → the snapshot producer whose rules that run may carry (finding.md, profile rules).
# finding.md: the only notifications a reviewer's run carries, each exactly so.
AGENT_NOTE = re.compile(r"(?P<who>ai review|custom rules) · (skipped \(no model configured on this Canyon host\)|replayed from a recording|dropped [1-9][0-9]* invalid result\(s\)|a model's second opinion: another review, even of a small change, may find different things; a blocking finding stays while its code is unchanged|1 blocking finding from an earlier review: the code it's on hasn't changed|(?:[2-9]|[1-9][0-9]+) blocking findings from an earlier review: the code they're on hasn't changed)")
DRIVER_PRODUCER = {"canyon/scanners": "scanner", "canyon/server-state": "server-state", "canyon/promote-gate": "promote-gate"}
# finding.md, In: external SARIF: an imported tool's run, its rule sarif.<tool>, and its notifications, each exactly so.
SARIF_DRIVER = re.compile(r"sarif/((?=.{1,40}$)[a-z0-9]+(?:-[a-z0-9]+)*)")
IMPORT_NOTE = r" · (report attached by .+|dropped [1-9][0-9]* result\(s\) at attach|no report for this package|could not check: .+|not checked: .+)"
# finding.md, From the tool into the profile: the governance prose every imported result carries, exactly so.
IMPORT_END_STATE = "{tool} no longer reports {rule} for this code when it runs again, and the app still does what it did."
IMPORT_DO_NOT = ["Silence it in {tool}'s settings or with an ignore comment — Canyon does not read those.",
                 "Remove or rename the flagged code just so the tool stops matching it."]


def policy_table(text=None):
    """snapshot.md, Tool results mapped to a policy: {(policy, tool): (selects, severity or None, fix or None)}, in
    table order. Every data row must parse, and no (policy, tool) twice: a row that silently fails to
    parse would be a mapping nothing checks."""
    section = (text or (CONTRACTS / "snapshot.md").read_text()).split("## Tool results mapped to a policy")[1].split("\n## ")[0]
    data = [line for line in section.splitlines() if line.startswith("| `")]
    rows = {}
    for line in data:
        m = re.fullmatch(r"\| `([a-z]+\.[a-z][a-z-]*)` \| `([a-z0-9-]+)` \| ([^|]+) \| (?:`([a-z]+)`|the tool's) \| (?:`([^`]+)`|the tool's) \|", line)
        if m is None or (m.group(1), m.group(2)) in rows:
            errors.append(f"snapshot: policy table: a row that does not parse, or a (policy, tool) twice: {line[:80]!r}")
            continue
        rows[(m.group(1), m.group(2))] = (m.group(3), m.group(4), m.group(5))
    return rows


def secret_name(rule):
    """snapshot.md's <SECRET_NAME>: the tool's ruleId upper-cased, each run outside A-Z0-9 one _, the ends'
    _ dropped, SECRET_ in front when it does not start with a letter."""
    name = re.sub("[^A-Z0-9]+", "_", rule.upper()).strip("_")
    return name if re.match("[A-Z]", name) else "SECRET_" + name


def clip(text, n=120):
    """finding.md: a title over n code points is its first n-1 cut back to their last space (all of them
    when there is none), trailing space dropped, then an ellipsis."""
    if len(text) <= n:
        return text
    head = text[:n - 1]
    if not text[n - 1].isspace() and re.search(r"\s", head[1:]):
        head = head[:max(i for i, ch in enumerate(head) if ch.isspace())]
    return head.rstrip() + "…"


def mapped(rid, tool):
    """The (policy, the tool's own ruleId) of a result of tool's run the policy table maps, else None."""
    for policy, row_tool in POLICY_TABLE:
        if row_tool == tool and rid.startswith(f"{policy}.{tool}."):
            return policy, rid[len(policy) + len(tool) + 2:]
    return None


def sarif_schema():
    """The SARIF 2.1.0 schema, vendored. The contracts' gate reads no network: a check that
    needs schemastore.org reachable is a check that stops running on their bad day."""
    raw = (CONTRACTS / SARIF_SCHEMA).read_bytes()
    digest = hashlib.sha256(raw).hexdigest()
    if digest != SARIF_SCHEMA_SHA256:
        errors.append(f"finding: contracts/{SARIF_SCHEMA} is not the pinned schema (sha256 {digest}); refresh with `make vendor-sarif` and re-pin deliberately")
        return None
    return json.loads(raw)


def framed(*fields):
    """finding.md's canyon/v1 digest over its framed fields: each prefixed with its UTF-8 **byte**
    length, never its code-point count, so Go's len(string) and this agree on a non-ASCII snippet
    such as the golden's mask. Framing is what makes the serialisation unambiguous: a uri may carry
    a newline and a snippet is legitimately multi-line, so a plain "\\n" join lets two different
    findings hash alike, and approvals key on the fingerprint."""
    out = b""
    for f in fields:
        raw = f.encode("utf-8")
        out += str(len(raw)).encode("ascii") + b":" + raw
    return "f_" + hashlib.sha256(out).hexdigest()


def fingerprints(log, digests=None):
    """The canyon/v1 fingerprint of every result by finding.md's recipe, keyed by id() of the
    result; None where a string is not valid Unicode (the run fails closed). `digests` (id() of an
    imported result → its region's sha256) frames the extra field a sarif/<tool> result carries."""
    seen, out = {}, {}
    for run in log["runs"]:
        for res in run["results"]:  # the occurrence counts in log order
            pl = res["locations"][0]["physicalLocation"]
            reg = pl["region"]
            key = (res["ruleId"], pl["artifactLocation"]["uri"], reg["snippet"]["text"], str(reg["startColumn"]))
            n = seen.get(key, 0)
            seen[key] = n + 1
            try:
                out[id(res)] = framed(*key, str(n), *([digests[id(res)]] if digests else []))
            except UnicodeEncodeError:
                out[id(res)] = None
    return out


def recipe_vector(log):
    """canyonv2-fb7, which the golden never exercises: a sibling on another column never renumbers a
    finding, and the same line twice at one column is told apart by its occurrence in log order."""
    first = log["runs"][1]["results"][1]
    moved, other = copy.deepcopy(first), copy.deepcopy(first)
    moved["locations"][0]["physicalLocation"]["region"]["startLine"] += 10
    other["locations"][0]["physicalLocation"]["region"]["startColumn"] += 1
    other["locations"][0]["physicalLocation"]["region"]["endColumn"] += 1
    alone = fingerprints({"runs": [{"results": [first]}]})[id(first)]
    got = fingerprints({"runs": [{"results": [other, first, moved]}]})
    pl = first["locations"][0]["physicalLocation"]
    key = (first["ruleId"], pl["artifactLocation"]["uri"], pl["region"]["snippet"]["text"], str(pl["region"]["startColumn"]))
    if got[id(first)] != alone or got[id(other)] == alone or got[id(moved)] != framed(*key, "1"):
        errors.append("finding: the canyon/v1 recipe is not rule, uri, snippet, start column and the occurrence among those")


def split_snippet(text):
    """A snippet's lines under any line ending; a trailing newline ends the last line rather than
    starting another. Mirrors snippetLines() in gate/finding/validate.go — the two must agree or a
    snippet ending in a newline is counted differently on each side of the seam."""
    for sep in ("\r\n", "\r", "\u2028", "\u2029"):
        text = text.replace(sep, "\n")
    return text.removesuffix("\n").split("\n")


def snippet_lines(text):
    return len(split_snippet(text))


def caret_outside(res):
    """finding.md: the caret a renderer draws — canyon.shown when present, else the region's own
    columns — lies on the snippet as serialized: start on its first line, end on its last."""
    reg = res["locations"][0]["physicalLocation"]["region"]
    at = res["properties"]["canyon"].get("shown", reg)
    lines = split_snippet(reg["snippet"]["text"])
    start, end = at["startColumn"], at["endColumn"]
    return not (1 <= start <= len(lines[0]) + 1 and 1 <= end <= len(lines[-1]) + 1 and (len(lines) > 1 or start <= end))


def by_id(label, kind, items):
    """Index rows by id; last-wins on a repeat hides which copy answers a lookup (gate/finding/validate.go's
    rulesByID/agentsByID refuse it Go-side), so this refuses too: an error naming the id, and None."""
    out = {}
    for it in items:
        if it["id"] in out:
            errors.append(f"{label}: the snapshot carries {kind} '{it['id']}' twice")
            return None
        out[it["id"]] = it
    return out


# The gate's own rule (finding.md): no policy, in no snapshot, the scanners' and always blocking.
# gate/finding's rulesByID adds it to every snapshot's rules the same way.
GATE_UNREADABLE = {"id": "gate.unreadable", "name": "Canyon checks every file", "state": "blocking", "producer": "scanner"}
# finding.md: a Canyon-run tool's digest over the tree, its one form.
UNREADABLE_SNIPPET = re.compile(r"\((trivy|opengrep|eslint|grype|checkov) could not check this tree; tree sha256:[0-9a-f]{64}\)")


def check_log(label, log, snap, schema):
    """One SARIF log against the SARIF 2.1.0 schema, the profile schema, and the seam-1 invariants."""
    for e in validators.validator_for(schema)(schema).iter_errors(log):
        errors.append(f"{label}: SARIF 2.1.0: {'/'.join(str(p) for p in e.path) or '<root>'}: {e.message}")
    if not schema_check(f"{label}: profile", CONTRACTS / "schema" / "finding.schema.json", log):
        return
    if LIVE_KEY.search(json.dumps(log)):
        errors.append(f"{label}: a live-key literal appears in the serialized log; redaction happens at construction")
    rules = by_id(label, "rule", snap["rules"] + [GATE_UNREADABLE])
    agent_rows = by_id(label, "agent", snap["agents"])
    if rules is None or agent_rows is None:
        return
    agents = {aid: set(a["namespace"]) for aid, a in agent_rows.items()}
    recipe = fingerprints(log)
    seen = {}
    for run in log["runs"]:
        driver = run["tool"]["driver"]
        declared = {r["id"] for r in driver["rules"]}
        imported = SARIF_DRIVER.fullmatch(driver["name"])
        tool = imported and imported.group(1)
        if driver["name"] not in DRIVER_PRODUCER:
            note = re.compile(re.escape(tool) + IMPORT_NOTE) if imported else AGENT_NOTE
            who = "custom rules" if driver["name"] == "custom-rules" else "ai review"  # finding.md, Custom rules
            for inv in run.get("invocations", []):
                for n in inv["toolExecutionNotifications"]:
                    m = note.fullmatch(n["message"]["text"])
                    if not m or not imported and m["who"] != who:
                        errors.append(f"{label}: {driver['name']}: a notification is not one finding.md names: {n['message']['text']!r}")
                    elif not imported and " · dropped " in n["message"]["text"] and n.get("level") != "note":  # finding.md: what the model got wrong is a note
                        errors.append(f"{label}: {driver['name']}: the dropped count is not a note")
        if not imported and driver["name"] not in DRIVER_PRODUCER:  # finding.md: an agent says one finding once per rule and region
            spots = [(r["ruleId"], pl["artifactLocation"]["uri"], pl["region"]["startLine"], pl["region"]["startColumn"], pl["region"]["endLine"], pl["region"]["endColumn"])
                     for r in run["results"] for pl in [r["locations"][0]["physicalLocation"]]]
            if len(spots) != len(set(spots)):
                errors.append(f"{label}: {driver['name']}: two results share a ruleId and region")
        if imported:  # sorted at attach, so the tool's output order cannot move a fingerprint
            order = [(pl["artifactLocation"]["uri"], pl["region"]["startLine"], pl["region"]["startColumn"], r["ruleId"])
                     for r in run["results"] for pl in [r["locations"][0]["physicalLocation"]]]
            if order != sorted(order):
                errors.append(f"{label}: {driver['name']}: results are not in (uri, startLine, startColumn, ruleId) order")
        for res in run["results"]:
            rid, c = res["ruleId"], res["properties"]["canyon"]
            tag = f"{label}: {rid}"
            if rid not in declared:
                errors.append(f"{tag}: not declared in tool.driver.rules of {driver['name']}")
            row = imported and mapped(rid, tool)  # a result the policy table maps reads its policy (snapshot.md)
            rule = rules.get(row[0] if row else f"sarif.{tool}" if imported else rid)  # an imported finding reads its tool's rule
            if rule is None:
                errors.append(f"{tag}: not in the policy snapshot")
                continue
            # Namespace-confined per producer (seam 5): scanners emit scanner rules, and one outside is a bug that
            # fails the run; an agent emits its namespaces, and the reviewer drops the rest before its log
            # exists, so one here was never dropped.
            ns = rid.split(".", 1)[0]
            producer = DRIVER_PRODUCER.get(driver["name"])
            if row:
                ok = True  # confined by the table's (policy, tool) rows, not by the policy's producer
            elif imported:
                ok = rule["producer"] == "sarif" and rid.startswith(f"sarif.{tool}.")
            elif producer:
                ok = rule["producer"] == producer
            else:  # an agent's run carries its snapshot id and may emit only the namespaces granted there
                ok = ns in agents.get(driver["name"], set())
            if not ok:
                errors.append(f"{tag}: outside producer {driver['name']}'s namespace")
            blocks = rule["state"] == "blocking" and c["severity"] in ("critical", "high") and not rid.startswith(DOCKERFILE_CHECKS)
            if c["blocks"] != blocks:
                errors.append(f"{tag}: blocks={c['blocks']} but policy is {rule['state']} and severity {c['severity']} ⇒ {blocks}")
            if res["level"] != LEVEL[c["severity"]]:
                errors.append(f"{tag}: level {res['level']} is not the projection of severity {c['severity']} ({LEVEL[c['severity']]})")
            if c["title"] != res["message"]["text"]:
                errors.append(f"{tag}: title differs from message.text")
            if imported and c["title"] != clip(c["problem"].split(". ", 1)[0]):
                errors.append(f"{tag}: an imported title is not the problem before its first '. ', cut to 120 code points")
            if rid.startswith(DOCKERFILE_CHECKS) and not c["problem"].endswith(NOT_READ_DOCKERFILE):
                errors.append(f"{tag}: a Dockerfile's problem does not end in Canyon's sentence")
            every = next(((p, selects) for (p, t), (selects, _, _) in POLICY_TABLE.items() if t == tool and selects.startswith("every result")), None)
            # "every result but a `<rule>` one in a documentation file": that rule may stay unmapped (the path's
            # classification is Linguist's, held by the gate's attach tests)
            spared = every and re.match(r"every result but a `([a-z0-9-]+)` one in a documentation file of prose", every[1])
            if imported and not row and every and not (spared and rid == f"sarif.{tool}.{spared.group(1)}"):
                errors.append(f"{tag}: the policy table maps every {tool} result under {every[0]}")
            if row:
                _, severity, fix = POLICY_TABLE[(row[0], tool)]
                secret = secret_name(row[1])
                if severity and c["severity"] != severity:
                    errors.append(f"{tag}: severity {c['severity']} is not the policy table's {severity}")
                if fix is not None and c["fix"] != fix.replace("<SECRET_NAME>", secret):  # None: the tool's fix
                    errors.append(f"{tag}: a mapped result's fix is not the policy table's")
                if (ns == "secrets") != ("secret_name" in c) or ns == "secrets" and c["secret_name"] != secret:
                    errors.append(f"{tag}: secret_name is not the policy table's <SECRET_NAME> of the tool's ruleId")
            if imported:
                own = row[1] if row else rid[len(f"sarif.{tool}."):]
                if (c.get("required_end_state") != IMPORT_END_STATE.format(tool=tool, rule=own) or
                        c.get("do_not") != [s.format(tool=tool) for s in IMPORT_DO_NOT]):
                    errors.append(f"{tag}: an imported result's required_end_state and do_not are not the fixed ones finding.md states")
            if c["policy_name"] != rule["name"]:
                errors.append(f"{tag}: policy_name {c['policy_name']!r} differs from the snapshot's {rule['name']!r}")
            if ns == "custom" and c["severity"] != "high":
                errors.append(f"{tag}: a custom rule's finding is high whatever the model answered (finding.md, Custom rules)")
            if "secret_name" in c and ns != "secrets":
                errors.append(f"{tag}: secret_name only belongs on secrets.* findings")
            if "carried_from" in c and (imported or driver["name"] in DRIVER_PRODUCER):  # finding.md: only a review's result is carried
                errors.append(f"{tag}: carried_from is on a result of no agent's run")
            if c["blocks"]:  # the governance prose is required once the finding blocks; permitted, never forbidden, otherwise
                for key in ("required_end_state", "do_not"):
                    if key not in c:
                        errors.append(f"{tag}: {key} is required on a finding that blocks")
            if "do_not" in c and (not c["do_not"] or any(not s or LINE_BREAK.search(s) for s in c["do_not"])):
                errors.append(f"{tag}: do_not must be a non-empty list of non-empty single-line entries")
            fp =res["partialFingerprints"]["canyon/v1"]
            if fp in seen:
                errors.append(f"{tag}: fingerprint {fp} already used by {seen[fp]}")
            seen[fp] = rid
            # an imported result's recipe also frames its region's digest, which needs the package file
            if not imported and (recipe[id(res)] is None or fp != recipe[id(res)]):
                errors.append(f"{tag}: canyon/v1 is not the recipe's (finding.md: the gate derives it)")
            # finding.md: a snippet is ≤3 lines AS SERIALIZED. A log off the wire is only ever in
            # this form, so this is the whole enforcement for one — gate/finding's own span check
            # runs before redaction and never sees this log.
            if caret_outside(res):
                errors.append(f"{tag}: the caret is outside the snippet it is drawn on (a masked or windowed snippet carries canyon.shown)")
            for n, loc in enumerate(res["locations"] + res.get("relatedLocations", [])):
                text = loc["physicalLocation"]["region"].get("snippet", {}).get("text")
                if text is not None and snippet_lines(text) > MAX_SNIPPET_LINES:
                    errors.append(f"{tag}: location {n}: a snippet is over {MAX_SNIPPET_LINES} lines")
                if imported and rid.startswith("secrets.") and not (text is not None and "••••" in text and snippet_lines(text) == 1):
                    errors.append(f"{tag}: location {n}: an imported region under secrets.* is not hidden whole")
                if rid == GATE_UNREADABLE["id"] and not (text is not None and UNREADABLE_SNIPPET.fullmatch(text)):
                    errors.append(f"{tag}: location {n}: a gate.unreadable snippet is not a tool's tree digest")


def selftest(log, snap, schema):
    """The one rule no golden can exercise gets its counterexample here. Every fixture in
    testdata/ is a MUST-PASS, so a check that only ever fires on a bad log is invisible to this
    script: delete it and everything still says "contracts valid". A four-line snippet is not a
    shape any golden may carry, so the must-fail case is built in memory instead."""
    for text, want in (("a", 1), ("a\n", 1), ("a\r\nb", 2), ("a\rb\rc", 3), ("a\nb\nc\nd", 4),
                       ("a\u2028b", 2), ("a\u2029b\u2028c\u2028d", 4), ("a\u2028", 1)):
        if snippet_lines(text) != want:
            errors.append(f"selftest: snippet_lines({text!r}) counts {snippet_lines(text)}, not {want}; "
                          "gate/finding's lines() is what it has to agree with")
    hexes = "a" * 64  # a gate.unreadable snippet: a Canyon-run tool's over the tree, and nothing else
    for text, ok in ((f"(trivy could not check this tree; tree sha256:{hexes})", True), (f"(eslint could not check this tree; tree sha256:{hexes})", True),
                     (f"(this line could not be read; file sha256:{hexes})", False), (f"(trivy could not check this tree; file sha256:{hexes})", False),
                     (f"(semgrep could not check this tree; tree sha256:{hexes})", False)):
        if bool(UNREADABLE_SNIPPET.fullmatch(text)) != ok:
            errors.append(f"selftest: the gate.unreadable snippet {text!r} is {'refused' if ok else 'accepted'}; finding.md names a tool's form alone")
    probe = copy.deepcopy(log)
    probe["runs"][1]["results"][0]["locations"][0]["physicalLocation"]["region"]["snippet"]["text"] += "\nb\nc\nd"
    mark = len(errors)
    check_log("selftest", probe, snap, schema)
    refused = any("a snippet is over" in e for e in errors[mark:])
    del errors[mark:]  # the probe's own errors are not the log's; drop them before reporting on it
    if not refused:
        errors.append("selftest: a four-line snippet was not refused; the cap is enforced by nothing")
    probe = copy.deepcopy(log)
    region = probe["runs"][1]["results"][0]["locations"][0]["physicalLocation"]["region"]  # a caret drawn past its snippet's line
    region["startColumn"] = len(split_snippet(region["snippet"]["text"])[0]) + 3
    region["endColumn"] = region["startColumn"] + 1
    mark = len(errors)
    check_log("selftest", probe, snap, schema)
    refused = any("the caret is outside" in e for e in errors[mark:])
    del errors[mark:]
    if not refused:
        errors.append("selftest: a caret past its snippet was not refused; the caret check is enforced by nothing")
    for name, mutate, want in (
            ("a reviewer result outside its namespace", lambda p: p["runs"][1]["results"][0].update(ruleId="sql.string-concat"), "outside producer"),
            ("a drop notice that counts nothing", lambda p: p["runs"][1]["invocations"][0]["toolExecutionNotifications"].append(
                {"level": "note", "message": {"text": "ai review · dropped 0 invalid result(s)"}}), "notification is not one"),
            ("a reviewer's finding said twice", lambda p: p["runs"][1]["results"].append(copy.deepcopy(p["runs"][1]["results"][0])), "share a ruleId and region"),
            ("a dropped count at warning", lambda p: p["runs"][1]["invocations"][0]["toolExecutionNotifications"].append(
                {"level": "warning", "message": {"text": "ai review · dropped 1 invalid result(s)"}}), "dropped count is not a note")):
        probe = copy.deepcopy(log)
        probe["runs"][1]["tool"]["driver"]["rules"] = probe["runs"][0]["tool"]["driver"]["rules"]
        probe["runs"][1]["invocations"] = [{"executionSuccessful": True, "toolExecutionNotifications": []}]
        mutate(probe)
        mark = len(errors)
        check_log("selftest", probe, snap, schema)
        refused = any(want in e for e in errors[mark:])
        del errors[mark:]
        if not refused:
            errors.append(f"selftest: {name} was not refused; the rule is enforced by nothing")
    for name, mutate in (("required_end_state missing", lambda c: c.pop("required_end_state")),
                         ("do_not empty", lambda c: c.__setitem__("do_not", []))):
        probe = copy.deepcopy(log)
        mutate(next(r for run in probe["runs"] for r in run["results"] if r["properties"]["canyon"]["blocks"])["properties"]["canyon"])
        mark = len(errors)
        check_log("selftest", probe, snap, schema)
        refused = any("required_end_state is required" in e or "do_not" in e for e in errors[mark:])  # the schema's minItems or check_log's own line
        del errors[mark:]
        if not refused:
            errors.append(f"selftest: a blocking finding with {name} was not refused; the rule is enforced by nothing")


def seam1(snap):
    log = load(CONTRACTS / "testdata" / "finding" / "claims-portal.sarif.json")
    schema = sarif_schema()
    if log is None or schema is None:
        return None
    check_log("finding", log, snap, schema)
    # A reviewer with no model: its run is there, no results, and D10's string as its one notification.
    skipped = load(CONTRACTS / "testdata" / "finding" / "skipped-review.sarif.json")
    if skipped is not None:
        check_log("finding: skipped review", skipped, snap, schema)
        run = skipped["runs"][0]
        notes = [n["message"]["text"] for inv in run.get("invocations", []) for n in inv["toolExecutionNotifications"]]
        if run["results"] or notes != ["ai review · skipped (no model configured on this Canyon host)"]:
            errors.append(f"finding: skipped review: no results and D10's one notification, not {notes}")
    # A carried finding: an agent's result with carried_from, and the run's note counting it (finding.md, A blocking
    # finding an earlier review made stays while its code stands).
    carried = load(CONTRACTS / "testdata" / "finding" / "carried-review.sarif.json")
    if carried is not None:
        check_log("finding: carried review", carried, snap, schema)
        run = carried["runs"][0]
        notes = [n["message"]["text"] for inv in run.get("invocations", []) for n in inv["toolExecutionNotifications"]]
        held = [r for r in run["results"] if "carried_from" in r["properties"]["canyon"]]
        if len(held) != 1 or notes[-1:] != ["ai review · 1 blocking finding from an earlier review: the code it's on hasn't changed"]:
            errors.append(f"finding: carried review: one carried result and the note counting it last, not {len(held)} and {notes}")
    # The reviewer's notifications finding.md names are exactly the ones the gate declares.
    documented = set(re.findall(r"`(ai review · [^`]+)`", (CONTRACTS / "finding.md").read_text().replace("\n  ", " ")))
    declared = {t.replace("%d", "N").replace("%s", "<path>") for t in re.findall(r'^const \w+ = "(ai review · [^"]+)"$', (ROOT / "gate" / "review.go").read_text(), re.M)}  # a Go count verb is finding.md's N, a string verb its <path>
    if documented != declared:
        errors.append(f"finding: reviewer notifications: finding.md names {sorted(documented)}, gate/review.go declares {sorted(declared)}")
    imported_tool(snap, schema)
    mapped_tool(snap, schema)
    selftest(log, snap, schema)
    recipe_vector(log)
    return log, schema


def imported_tool(snap, schema):
    """An imported semgrep report as a run carries it, against the snapshot after an admin added
    semgrep Advisory (finding.md, In: external SARIF), and its results as the producer.sarif row
    stores them (events.md: no partialFingerprints); then logs it must refuse."""
    log = load(CONTRACTS / "testdata" / "finding" / "imported-semgrep.sarif.json")
    added = copy.deepcopy(snap)
    added["rules"].append({"id": "sarif.semgrep", "name": "Findings from semgrep", "state": "advisory", "producer": "sarif"})
    if log is None or not schema_check("finding: imported: snapshot", CONTRACTS / "schema" / "snapshot.schema.json", added):
        return
    check_log("finding: imported", log, added, schema)
    fixture_app(log, hidden=True)
    stored = [{k: v for k, v in r.items() if k != "partialFingerprints"} for r in log["runs"][0]["results"]]
    row = {"id": 1, "ts": "2026-09-24T00:00:00Z", "scope": "run", "run": "gr_0001", "project": "claims-portal", "kind": "producer.sarif",
           "data": {"producer": "sarif/semgrep", "tool": "semgrep", "category": "", "version": "1.86.0", "digest": "sha256:" + "0" * 64,
                    "attached_by": "ci-bot", "dropped": 1, "first_dropped": "result 3: kind is pass", "results": stored}}
    schema_check("finding: imported: stored row", CONTRACTS / "schema" / "timeline-row.schema.json", row)
    for name, key, value in (("a stored result carrying partialFingerprints", "results", log["runs"][0]["results"]),
                             ("a first_dropped not shaped 'result <i>: <why>'", "first_dropped", "3: kind is pass")):
        probe = copy.deepcopy(row)
        probe["data"][key] = value
        mark = len(errors)
        schema_check("selftest", CONTRACTS / "schema" / "timeline-row.schema.json", probe)
        if len(errors) == mark:
            errors.append(f"selftest: an imported row with {name} was not refused")
        del errors[mark:]
    # finding.md, Canyon-run tools: a tool with run on that this host has no image for blocks; its run never shrugs it off in a note.
    probe = json.loads(json.dumps(log).replace("semgrep", "trivy"))
    probe["runs"][0]["invocations"][0]["toolExecutionNotifications"][0].update(level="warning", message={"text": "not checked on this host: trivy"})
    mark = len(errors)
    check_log("selftest", probe, {**added, "rules": added["rules"] + [{"id": "sarif.trivy", "name": "Findings from trivy", "state": "advisory", "producer": "sarif"}]}, schema)
    passed = len(errors) == mark
    del errors[mark:]
    if passed:
        errors.append("selftest: a trivy run saying 'not checked on this host: trivy' was accepted")
    region = lambda p: p["runs"][0]["results"][0]["locations"][0]["physicalLocation"]["region"]
    for name, mutate, want in (
            ("another tool's result", lambda p: p["runs"][0]["results"][0].__setitem__("ruleId", "sarif.gitleaks.generic-api-key"), "outside producer"),
            ("a title that is not the first sentence", lambda p: p["runs"][0]["results"][0]["properties"]["canyon"].__setitem__("problem", "Other. Text"), "imported title"),
            ("no required_end_state", lambda p: p["runs"][0]["results"][0]["properties"]["canyon"].pop("required_end_state", None), "the fixed ones"),
            ("a do_not of its own", lambda p: p["runs"][0]["results"][0]["properties"]["canyon"].__setitem__("do_not", ["add a nosemgrep comment"]), "the fixed ones"),
            ("results out of order", lambda p: p["runs"][0]["results"].insert(0, {**p["runs"][0]["results"][0], "locations": [
                {"physicalLocation": {"artifactLocation": {"uri": "z.ts"}, "region": region(p)}}]}), "not in (uri")):
        probe = copy.deepcopy(log)
        mutate(probe)
        mark = len(errors)
        check_log("selftest", probe, added, schema)
        refused = any(want in e for e in errors[mark:])
        del errors[mark:]
        if not refused:
            errors.append(f"selftest: an imported log with {name} was not refused; the rule is enforced by nothing")


def mapped_tool(snap, schema):
    """gitleaks' result as the policy table maps it under secrets.client-bundle (snapshot.md; finding.md,
    Mapped fixture), against the snapshot, which carries gitleaks; then mapped logs it must refuse."""
    log = load(CONTRACTS / "testdata" / "finding" / "mapped-gitleaks.sarif.json")
    added = copy.deepcopy(snap)  # sarif.gitleaks is canonical (snapshot.md): no admin adds it
    if not POLICY_TABLE or any(policy not in {r["id"] for r in snap["rules"]} for policy, _ in POLICY_TABLE):
        errors.append(f"finding: mapped: snapshot.md's policy table reads as {sorted(POLICY_TABLE)}: empty, or naming a rule the snapshot lacks")
    if log is None:
        return
    check_log("finding: mapped", log, added, schema)
    fixture_app(log, hidden=True)
    canyon = lambda p: p["runs"][0]["results"][0]["properties"]["canyon"]
    result = lambda p: p["runs"][0]["results"][0]

    def retitle(p, text):
        result(p)["message"]["text"] = canyon(p)["title"] = text

    def opengrep(p):  # the same result as opengrep's under sql.string-concat, which takes no secret_name
        q = json.loads(json.dumps(p).replace("secrets.client-bundle.gitleaks.stripe-access-token", "sql.string-concat.opengrep.sqli")
                       .replace("gitleaks", "opengrep").replace("stripe-access-token", "sqli"))
        p.clear()
        p.update(q)
        canyon(p).pop("secret_name")
        canyon(p)["policy_name"] = p["runs"][0]["tool"]["driver"]["rules"][0]["name"] = "No hand-built SQL from user input"
        canyon(p)["fix"] = "Change this code so opengrep no longer reports sqli here; the line above says what it found."
    for name, mutate, want, snap_rules in (
            ("a title cut without its ellipsis", lambda p: retitle(p, canyon(p)["problem"][:80]), "imported title", []),
            ("a region under secrets.* that is shown", lambda p: result(p)["locations"][0]["physicalLocation"]["region"]["snippet"].__setitem__("text", "const stripe = new Stripe(\"sk_live\");"), "not hidden whole", []),
            ("a severity that is not the table's", lambda p: canyon(p).update(severity="high"), "policy table's critical", []),
            ("a fix of its own", lambda p: canyon(p).update(fix="Delete the key."), "policy table's", []),
            ("another secret_name", lambda p: canyon(p).update(secret_name="STRIPE_SECRET_KEY"), "secret_name", []),
            ("a policy the table does not map gitleaks to", lambda p: result(p).update(ruleId="sql.string-concat.gitleaks.stripe-access-token"), "outside producer", []),
            ("a gitleaks result left unmapped", lambda p: [r.update(id="sarif.gitleaks.stripe-access-token") for r in p["runs"][0]["tool"]["driver"]["rules"]]
             + [result(p).update(ruleId="sarif.gitleaks.stripe-access-token")], "maps every gitleaks result", []),
            ("an opengrep result's fix not the table's", opengrep, "policy table's", [])):
        probe = copy.deepcopy(log)
        mutate(probe)
        mark = len(errors)
        check_log("selftest", probe, {**added, "rules": added["rules"] + snap_rules}, schema)
        refused = any(want in e for e in errors[mark:])
        del errors[mark:]
        if not refused:
            errors.append(f"selftest: a mapped log with {name} was not refused; the rule is enforced by nothing")
    for name, row in (("a row that does not parse", "| `deps.known-vuln` | `trivy` | every result | `high` | no fix |"),
                      ("a (policy, tool) twice", "| `secrets.client-bundle` | `gitleaks` | every result | `critical` | `x` |")):
        mark = len(errors)
        policy_table((CONTRACTS / "snapshot.md").read_text().replace("|---|---|---|---|---|\n", "|---|---|---|---|---|\n" + row + "\n"))
        refused = len(errors) > mark
        del errors[mark:]
        if not refused:
            errors.append(f"selftest: a policy table with {name} was read; it would map nothing anyone checks")
    # The same result as checkov's dockerfile check under iac.misconfig held blocking: it never blocks (snapshot.md).
    probe = json.loads(json.dumps(log).replace("secrets.client-bundle.gitleaks.stripe-access-token", "iac.misconfig.checkov.CKV_DOCKER_2")
                       .replace("gitleaks", "checkov").replace("stripe-access-token", "CKV_DOCKER_2"))
    canyon(probe).pop("secret_name")
    canyon(probe)["policy_name"] = probe["runs"][0]["tool"]["driver"]["rules"][0]["name"] = next(r["name"] for r in added["rules"] if r["id"] == "iac.misconfig")
    canyon(probe)["fix"] = "Change this code so checkov no longer reports CKV_DOCKER_2 here; the line above says what it found."
    canyon(probe)["problem"] += NOT_READ_DOCKERFILE
    canyon(probe)["blocks"] = False
    result(probe)["locations"][0]["physicalLocation"]["artifactLocation"]["uri"] = "Dockerfile"
    held = {**added, "rules": [{**r, "state": "blocking"} if r["id"] == "iac.misconfig" else r for r in added["rules"]]}
    mark = len(errors)
    check_log("selftest", probe, held, schema)
    if len(errors) > mark:
        errors[mark:] = [f"selftest: a Dockerfile's finding under iac.misconfig held blocking, blocks false, was refused: {e}" for e in errors[mark:]]
    canyon(probe)["problem"] = canyon(probe)["problem"].removesuffix(NOT_READ_DOCKERFILE)
    mark = len(errors)
    check_log("selftest", probe, held, schema)
    refused = any("Canyon's sentence" in e for e in errors[mark:])
    del errors[mark:]
    if not refused:
        errors.append("selftest: a Dockerfile's finding without Canyon's sentence was not refused; the rule is enforced by nothing")


# ---------------------------------------------------------------- the fixture app the golden log describes
FIXTURE_APP = ROOT / "gate" / "testdata" / "apps" / "claims-portal"
# TEST-ONLY project key (finding.md): the imported fixture's canyon/v1 frames HMAC-SHA-256 under 32 zero
# bytes. A real project's key is 32 random bytes sealed in the vault, never exported.
TEST_PROJECT_KEY = bytes(32)


def fixture_app(log, hidden=False):
    """Every golden-log location exists in gate/testdata/apps/claims-portal and reads as its snippet says.
    `hidden`: an imported log (finding.md, In: external SARIF) — lines placed by SARIF's default
    newlineSequences, and canyon/v1 framing the region's digest; its snippet the region hidden whole
    under secrets.*, else the line as a Canyon finding's is."""
    digests = {}
    for run in log["runs"]:
        for res in run["results"]:
            for loc in res["locations"] + res.get("relatedLocations", []):
                pl = loc["physicalLocation"]
                uri, reg = pl["artifactLocation"]["uri"], pl["region"]
                where = f"fixture: {uri}:{reg['startLine']}"
                path = FIXTURE_APP / uri
                if not path.exists():
                    errors.append(f"{where}: file missing from the claims-portal app")
                    continue
                if hidden:
                    parts = re.split("(\r\n|\n)", path.read_bytes().decode("utf-8"))
                    lines, ends = parts[0::2], parts[1::2] + [""]
                else:
                    lines = path.read_text().splitlines()
                if reg["endLine"] > len(lines):
                    errors.append(f"{where}: past the end of the file ({len(lines)} lines)")
                    continue
                s, e, sc, ec = reg["startLine"] - 1, reg["endLine"] - 1, reg["startColumn"], reg["endColumn"]
                line, last, text = lines[s], lines[e], reg["snippet"]["text"]
                if hidden:
                    body = [lines[i] + ends[i] for i in range(s, e + 1)]  # the region's bytes, terminators as stored
                    cut = "".join(body)[sc - 1:len("".join(body[:-1])) + ec - 1]
                    digests[id(res)] = hmac.new(TEST_PROJECT_KEY, cut.encode("utf-8"), hashlib.sha256).hexdigest()
                if hidden and res["ruleId"].startswith("secrets."):
                    ok = text == line[:sc - 1] + "••••" + last[ec - 1:]
                elif "•" in text:  # redacted snippet: the visible head and tail must match
                    head, tail = text.split("•", 1)[0], text.rsplit("•", 1)[1]
                    ok = line.startswith(head) and line.endswith(tail)
                else:
                    ok = line == text
                if not ok:
                    errors.append(f"{where}: reads {line.strip()!r}, not the snippet")
                if not (1 <= sc <= len(line) + 1 and 1 <= ec <= len(last) + 1 and (s, sc) < (e, ec)):
                    errors.append(f"{where}: region {sc}-{ec} is outside the line")
                first = split_snippet(text)[0]  # the span starts on the snippet's first line
                moved = first[sc - 1:ec - 1] != line[sc - 1:ec - 1] if s == e else first[sc - 1:] != line[sc - 1:]
                if loc is res["locations"][0] and "shown" not in res["properties"]["canyon"] and moved:
                    errors.append(f"{where}: the snippet moved the span but the finding carries no canyon.shown")
    if hidden:
        recipe = fingerprints(log, digests)
        for run in log["runs"]:
            for res in run["results"]:
                if id(res) in digests and res["partialFingerprints"]["canyon/v1"] != recipe[id(res)]:
                    errors.append(f"fixture: {res['ruleId']}: canyon/v1 is not the recipe's over the fixture app's region")


# ---------------------------------------------------------------- seam 2 — the run timeline
def catalogue():
    """The row kinds contracts/events.md lists, per scope table: {scope: {kind}}."""
    text = (CONTRACTS / "events.md").read_text()
    tables = {"run": text.split("## Run scope")[1].split("## Org scope")[0], "org": text.split("## Org scope")[1]}
    return {scope: set(re.findall(r"^\| *`([a-z][a-z.]*)` *\|", part, re.M)) for scope, part in tables.items()}


def failure_reasons():
    """The failure_reason values contracts/events.md lists on the run.state row, in its order."""
    row = next(line for line in (CONTRACTS / "events.md").read_text().splitlines() if line.startswith("| `run.state`"))
    return re.findall(r"`([a-z_]+)` \(", row.split("`failure_reason` ∈")[1].split(" · ")[0])


def enums(node, key):
    """Every enum a schema gives a property named key, wherever it sits."""
    if isinstance(node, dict):
        if isinstance(node.get(key), dict) and "enum" in node[key]:
            yield node[key]["enum"]
        for child in node.values():
            yield from enums(child, key)
    elif isinstance(node, list):
        for child in node:
            yield from enums(child, key)


def seam2(snap, golden, sarif, fleet_tiles):
    sys.path.insert(0, str(CONTRACTS / "testdata" / "timeline"))
    import generate
    rows = generate.rows()
    schema = load(CONTRACTS / "schema" / "timeline-row.schema.json")
    Draft7Validator.check_schema(schema)
    v = Draft7Validator(schema)
    for i, row in enumerate(rows):
        for e in sorted(v.iter_errors(row), key=lambda e: list(e.path)):
            errors.append(f"timeline: row {row.get('id', i + 1)}: {'/'.join(str(p) for p in e.path) or '<root>'}: {e.message}")
    known = catalogue()
    # A failed run's reason is one events.md lists, and every schema that carries one lists exactly those.
    listed = failure_reasons()
    for path in sorted((CONTRACTS / "schema").glob("*.schema.json")):
        for enum in enums(load(path), "failure_reason"):
            if enum != listed:
                errors.append(f"timeline: {path.name} gives failure_reason {enum}; events.md lists {listed}")
    # A step Canyon failed in is one routes.md's *A failure reason in words* names, and every schema that
    # carries failed_in lists exactly those (#1645).
    words = (CONTRACTS / "routes.md").read_text().split("**A failure reason in words**")[1].split("A reason this list doesn't name")[0]
    steps = re.findall(r"`([a-z]+)`\s+`[a-z]", words.split("the step's words:")[1].split(". Those words")[0])
    for path in sorted((CONTRACTS / "schema").glob("*.schema.json")):
        for enum in enums(load(path), "failed_in"):
            if enum != steps:
                errors.append(f"timeline: {path.name} gives failed_in {enum}; routes.md names {steps}")
    for scope in ("run", "org"):
        feed = [r for r in rows if r["scope"] == scope]
        # 1. Within a scope, id is the cursor: strictly increasing; timestamps never step back.
        ids = [r["id"] for r in feed]
        if ids != sorted(set(ids)):
            errors.append(f"timeline: {scope}-scope ids are not strictly increasing")
        if [r["ts"] for r in feed] != sorted(r["ts"] for r in feed):
            errors.append(f"timeline: {scope}-scope timestamps step backwards")
        # 2. Every kind comes from this scope's catalogue table, and the recording exercises every kind in it.
        kinds = {r["kind"] for r in feed}
        for k in sorted(kinds - known[scope]):
            errors.append(f"timeline: kind '{k}' is not in the events.md {scope}-scope catalogue")
        for k in sorted(known[scope] - kinds):
            errors.append(f"timeline: {scope}-scope catalogue kind '{k}' never appears in the recording")
    # 3. Each run's states run pending → running → one terminal state.
    for run in {r["run"] for r in rows if r["scope"] == "run"}:
        states = [r["data"]["state"] for r in rows if r.get("run") == run and r["kind"] == "run.state"]
        if states[:2] != ["pending", "running"] or len(states) != 3 or states[2] not in ("clean", "blocked", "failed"):
            errors.append(f"timeline: {run} states {states}; expected pending, running, then one terminal state")
    # 4. Embedded SARIF is real seam-1 content; verdict counts match its findings; the hero verdict is the golden log.
    first_verdict = True
    for row in rows:
        if row["kind"] == "producer.sarif":
            check_log(f"timeline: row {row['id']} producer.sarif", row["data"]["sarif"], snap, sarif)
            if any(run["tool"]["driver"]["name"] != row["data"]["producer"] for run in row["data"]["sarif"]["runs"]):
                errors.append(f"timeline: row {row['id']}: a run in the log is not from producer {row['data']['producer']}")
        elif row["kind"] == "verdict":
            log = row["data"]["findings"]
            check_log(f"timeline: row {row['id']} verdict", log, snap, sarif)
            flags = [res["properties"]["canyon"]["blocks"] for run in log["runs"] for res in run["results"]]
            if (row["data"]["blocking"], row["data"]["warnings"]) != (flags.count(True), flags.count(False)):
                errors.append(f"timeline: row {row['id']}: blocking/warnings {row['data']['blocking']}/{row['data']['warnings']} differ from the findings ({flags.count(True)}/{flags.count(False)})")
            if first_verdict and log != golden:
                errors.append(f"timeline: row {row['id']}: the first verdict's findings are not the golden claims-portal log")
            first_verdict = False
    # 5. The story (D4): a blocked verdict, the fix, a clean verdict, then the deploy of the clean run.
    verdicts = [r for r in rows if r["kind"] == "verdict"]
    if [v["data"]["state"] for v in verdicts] != ["blocked", "clean"]:
        errors.append(f"timeline: verdicts run {[v['data']['state'] for v in verdicts]}; the story is blocked, then clean")
    for r in rows:
        if r["kind"] == "deploy.completed" and (not verdicts or r["run"] != verdicts[-1]["run"]):
            errors.append(f"timeline: row {r['id']}: deploy.completed belongs to {r['run']}, not the clean run")
    # 6. An approved deploy's verdict (events.md): `approved` rides only on a blocked verdict with
    #    blocking findings, each approval by a person, its expiry a whole-second UTC instant. The
    #    recording carries no such row, so the clause is exercised on variants of the hero verdict:
    #    the well-formed one must pass and each broken one must be refused.
    hero = next((r for r in rows if r["kind"] == "verdict" and r["data"]["state"] == "blocked"), None)
    if hero is None:
        errors.append("timeline: no blocked verdict to build the approved variants from")
    else:
        import copy
        blocking_findings = [res for run in hero["data"]["findings"]["runs"] for res in run["results"] if res["properties"]["canyon"]["blocks"]]
        approval = {"rule": blocking_findings[0]["ruleId"], "fingerprint": blocking_findings[0]["partialFingerprints"]["canyon/v1"],
                    "reason": "rotation is ticketed", "expires_at": "2026-12-31T00:00:00Z",
                    "approved_by": {"kind": "person", "subject": "dana@meridian.example", "display": "Dana Whitfield"}}
        approved = copy.deepcopy(hero)
        approved["data"]["approved"] = [approval]
        if list(v.iter_errors(approved)):
            errors.append("timeline: a blocked verdict listing a person's unexpired approval must validate")
        def refused(label, mutate):
            row = copy.deepcopy(approved)
            mutate(row)
            if not list(v.iter_errors(row)):
                errors.append(f"timeline: the schema accepts an approved verdict {label}; events.md forbids it")
        refused("whose state is clean", lambda r: r["data"].__setitem__("state", "clean"))
        refused("with no blocking finding", lambda r: r["data"].__setitem__("blocking", 0))
        refused("approved by an agent", lambda r: r["data"]["approved"][0]["approved_by"].__setitem__("kind", "agent"))
        refused("whose expiry is not a whole-second UTC instant", lambda r: r["data"]["approved"][0].__setitem__("expires_at", "2026-10-02T00:00:00.5+02:00"))
        refused("with an empty approval list", lambda r: r["data"].__setitem__("approved", []))
        refused("approved by Canyon", lambda r: r["data"]["approved"][0].__setitem__("approved_by", {"kind": "canyon", "subject": "ai", "display": "Canyon's AI"}))  # only a person approves (routes.md)
    # 7. One tile vocabulary: a tiles.updated row carries the counters the fleet read serves, no others.
    if not fleet_tiles:
        errors.append("timeline: the fleet page fixture did not validate, so tiles.updated was never checked against its tiles")
    for r in rows:
        if r["kind"] == "tiles.updated" and fleet_tiles and set(r["data"]["tiles"]) != fleet_tiles:
            errors.append(f"timeline: row {r['id']}: tiles.updated carries {sorted(r['data']['tiles'])}, not the fleet's {sorted(fleet_tiles)} (routes.md row 75)")


# ---------------------------------------------------------------- seam 2 — the deploy's access lines
# build.log rows (events.md) that are D5 strings (design/EXPERIENCE.md): one registry, byte for byte.
ACCESS_LINES = ("Private: only its owners and admins can open it. Share it: canyon share <email> · canyon share everyone",
                "Shared with everyone at <org>.",
                "No description yet — add description = \"…\" to canyon.toml so colleagues see what it does.")


def access_lines():
    """A deploy tells its builder who can open the app (events.md, build.log). The CLI, the ai format and
    the dashboard print these words and never paraphrase them, so each document carries them exactly —
    cli.md only the private line, the one its transcripts show."""
    for doc, lines in ((CONTRACTS / "events.md", ACCESS_LINES), (ROOT / "design" / "EXPERIENCE.md", ACCESS_LINES),
                       (ROOT / "design" / "specs" / "cli.md", ACCESS_LINES[:1])):
        text = doc.read_text()
        for line in lines:
            if line not in text:
                errors.append(f"{doc.relative_to(ROOT)}: does not carry the deploy's access line {line!r} byte for byte")


# ---------------------------------------------------------------- seam 3 — the fleet read
# routes.md row 75's risk order: the bucket each stage sorts into, worst first. Ten stages,
# eight buckets — `created` ties with `awaiting_review`, `building` with `reviewing`.
STAGE_BUCKET = {"blocked": 0, "failed": 1, "approved": 2, "awaiting_review": 3, "created": 3,
                "reviewing": 4, "building": 4, "passed": 5, "preview": 6, "production": 7}
# The two stages `since` reads off an environment rather than a run or a package (routes.md).
DEPLOYED_STAGE = {"preview": "preview", "production": "prod"}


# routes.md, *What needs a person*: each kind's rank, by its environment where the two differ, and the kinds' order.
NEED_KINDS = ["held", "down", "refused", "refused_code", "secret", "approval", "blocked", "unchecked", "blocked_prod", "will_refuse",
              "patch", "access", "unowned", "never_reviewed", "recreate", "notify_failing", "licence", "patching_unlicensed"]
NEED_RANK = {"held": 1, "down": 2, "refused": 3, "refused_code": 3, "secret": 4, "approval": 5, "blocked": 6, "unchecked": 6, "blocked_prod": 6,
             "patch": 8, "access": 9, "unowned": 10, "never_reviewed": 10, "recreate": 11, "notify_failing": 11, "licence": 11, "patching_unlicensed": 11}
NO_NEED = 12  # a row read whole carrying none, after every row carrying one


def need_rank(n):
    """A need's rank (routes.md, *What needs a person*): preview's down, refused and refused_code 7, will_refuse 6 while it blocks."""
    if n["kind"] == "will_refuse":
        return 6 if n["blocks"] else 7
    if n["env"] == "preview" and n["kind"] in ("down", "refused", "refused_code"):
        return 7
    return NEED_RANK[n["kind"]]


def need_order(n):
    """The list's order within an app: rank, then since (oldest first, null last), then kind, then what it names."""
    subject = n.get("destination") or (n.get("asked_by") or {}).get("display") or ""
    return need_rank(n), n["since"] is None, n["since"] or "", NEED_KINDS.index(n["kind"]), subject


def fleet_order(app):
    """routes.md row 75: a row read whole carrying needs first, by its first need's rank, then stage bucket, then
    staleness (oldest change first), then id."""
    needs = app.get("needs") or []
    return need_rank(needs[0]) if needs else NO_NEED, STAGE_BUCKET[app["latest"]["stage"]], app["latest"]["since"], app["id"]


def stats_problems(label, doc):
    """The fleet's stats and total (routes.md, the fleet read) against a whole page: the stats count the rows
    the caller reads whole, as the tiles do; total is every row the page holds once next is null."""
    team = [a for a in doc["apps"] if "gate_state" in a]
    deployed = lambda a, env: "url" in (a["envs"].get(env) or {})
    s, out = doc["stats"], []
    for key, n in {"apps": len(team), "production": sum(deployed(a, "prod") for a in team), "preview": sum(deployed(a, "preview") for a in team),
                   "passed": sum(a["gate_state"] == "clean" for a in team)}.items():
        if s[key] != n:
            out.append(f"{label}: stats.{key} is {s[key]}, but {n} of the rows read whole qualify")
    if not 0 <= s["owners"] <= sum(len(a["owners"]) for a in team):
        out.append(f"{label}: stats.owners is {s['owners']}; the distinct owners of the rows read whole, at most {sum(len(a['owners']) for a in team)}")
    if s["production_7d"] > s["deploys_7d"]:
        out.append(f"{label}: stats.production_7d {s['production_7d']} is more than deploys_7d {s['deploys_7d']}, which counts it")
    if doc["next"] is None and doc["total"] != len(doc["apps"]):
        out.append(f"{label}: total is {doc['total']} on a last page holding all {len(doc['apps'])} rows")
    if doc["page"] != 1:
        out.append(f"{label}: page is {doc['page']} on a read with neither page nor cursor; that is page 1")
    return out


def seam3():
    """The fleet page's one read. The golden is a single complete page (`next` null) — which is
    what lets tiles, counts that no filter narrows, be counted back out of its rows.
    Three of the four can be: `secrets_exposed` reads each project's latest verdict, which a fleet
    row does not carry, so it is the Go route's test (M3 PR 3, red: the tile off by one) and this
    check leaves it alone, as it leaves `refused`, which reads the app's egress requests (#2723). Returns the tile vocabulary the golden serves, which seam 2's
    `tiles.updated` rows must match.

    The fixture is **one page of a fleet that does not exist**, not the D4 demo seed: it carries the
    shapes a reader has to handle, so it holds apps the seed does not. Nothing should seed from it."""
    doc = load(CONTRACTS / "testdata" / "fleet" / "page.json")

    def row_id(path):
        """Name an error by its app, not by its index: a reader goes to claims-portal, not to apps/1."""
        if len(path) >= 2 and path[0] == "apps":
            rest = "/".join(str(p) for p in path[2:])
            return f"{doc['apps'][path[1]].get('id', path[1])}: {rest or '<row>'}"
        return None

    schema = CONTRACTS / "schema" / "fleet.schema.json"
    if not schema_check("fleet", schema, doc, row_id):
        return None
    # 0. The risk order covers the stage vocabulary exactly — a tenth stage is a named red, not a KeyError.
    declared = set(load(schema)["definitions"]["stage"]["enum"])
    if declared != set(STAGE_BUCKET):
        errors.append(f"fleet: the risk order ranks {sorted(STAGE_BUCKET)} but the stage enum is {sorted(declared)}; "
                      "routes.md's stage table is the one list both follow")
        return None
    apps = doc["apps"]
    if doc["next"] is not None:
        errors.append("fleet: the golden must be one complete page (next: null); the tile counts below hold only then")
        return None
    if doc["viewer"]["role"] == "admin" and not all(a["claimable"] for a in apps):
        errors.append("fleet: page.json is an admin's, and an admin's claim is always accepted: every row is claimable")
    # 1. Five of the seven tiles are facts of the rows (secrets_exposed reads the verdict, refused the egress requests, not the row).
    #    never_reviewed is a project fact: no run has ever looked at it. A newer package awaiting review
    #    on a project that has had runs is not the same thing and does not tick the tile.
    counted = {"blocked": sum(1 for a in apps if a["latest"]["stage"] == "blocked"),
               "approved": sum(1 for a in apps if a["latest"]["stage"] == "approved"),
               "failed": sum(1 for a in apps if a["latest"]["stage"] == "failed"),
               "never_reviewed": sum(1 for a in apps if a["gate_state"] == "never_reviewed"),
               "unowned": sum(1 for a in apps if not a["owners"])}
    for tile, n in counted.items():
        if doc["tiles"][tile] != n:
            errors.append(f"fleet: tiles.{tile} is {doc['tiles'][tile]}, but {n} of the {len(apps)} rows qualify")
    errors.extend(stats_problems("fleet", doc))
    # 2. The server sorts; a client that re-sorts a page sorts one page.
    for before, after in zip(apps, apps[1:]):
        if fleet_order(before) > fleet_order(after):
            errors.append(f"fleet: {after['id']} ({after['latest']['stage']}, {after['latest']['since']}) sorts above "
                          f"{before['id']} ({before['latest']['stage']}, {before['latest']['since']}): "
                          "apps[] runs the first need's rank, then stage, then oldest change, then id")
    # 3. A project with no package has nothing to run. (`awaiting_review` may: a newer package
    #    awaits review while the environments keep what they were given — routes.md row 47.)
    for a in apps:
        if a["latest"]["stage"] == "created" and a["envs"]:
            errors.append(f"fleet: {a['id']} has no package but runs {sorted(a['envs'])}; a project reaches an environment through a package")
    # 4. On the two stages an environment defines, the row agrees with the environment it names.
    for a in apps:
        env = DEPLOYED_STAGE.get(a["latest"]["stage"])
        at = (a["envs"].get(env) or {}).get("deployed_at") if env else None
        if env and at != a["latest"]["since"]:
            errors.append(f"fleet: {a['id']} is {a['latest']['stage']} since {a['latest']['since']} but its "
                          f"{env} deployed at {at}; on those two stages `since` is that environment's deployed_at")
    # 5. One version, one name (routes.md, *Naming a version*): an environment running the row's
    #    latest package names it the way `latest` does — the row never disagrees with itself.
    for a in apps:
        for env, e in a["envs"].items():
            if e["package"] == a["latest"]["package"] and e.get("label") != a["latest"].get("label"):
                errors.append(f"fleet: {a['id']} runs {e['package']} in {env} as {e.get('label')!r} but calls it "
                              f"{a['latest'].get('label')!r} in latest; a version is named the same way on every read")
    # 6. A page names each project once — a row repeated across the page is a row the reader acts on twice.
    ids = [a["id"] for a in apps]
    for i in sorted(set(i for i in ids if ids.count(i) > 1)):
        errors.append(f"fleet: {i} appears {ids.count(i)} times in one page; apps[] names a project once")
    return set(doc["tiles"]) - {"needs_you"}  # needs_you counts needs, beside the seven: no tiles.updated row carries it


# ---------------------------------------------------------------- seam 3 — what needs a person
def needs_problems(label, needs, row):
    """One app's needs against the facts its read carries (routes.md, *What needs a person*): in the list's order,
    and every need the row's own fields say there is, so a read never carries a fact without its need."""
    out = []
    for n in needs:
        if "rank" in n and n["rank"] != need_rank(n):
            out.append(f"{label}: a {n['kind']} need of {n['env']} carries rank {n['rank']}, but its kind's row ranks it {need_rank(n)}")
    for before, after in zip(needs, needs[1:]):
        if need_order(before) > need_order(after):
            out.append(f"{label}: {after['kind']} ({after['env']}) is listed after {before['kind']} ({before['env']}): "
                       "the list runs rank, then since, then kind, then what it names")
    kinds = {(n["kind"], n["env"]) for n in needs}
    held = row.get("hold") is not None
    if held and needs[:1] and needs[0]["kind"] != "held":
        out.append(f"{label}: production is held but its first need is {needs[0]['kind']}; held outranks every other")
    if held != (("held", "prod") in kinds):
        out.append(f"{label}: hold {'set' if held else 'unset'} but the held need {'missing' if held else 'listed'}")
    for env, e in (row.get("envs") or {}).items():
        down = e.get("state") in ("stopped", "restarting") and not (env == "prod" and held)
        if down != (("down", env) in kinds):
            out.append(f"{label}: {env} reads {e.get('state')} but its down need is {'missing' if down else 'listed'}")
    for n in needs:
        if n["kind"] == "down" and (row.get("envs") or {}).get(n["env"], {}).get("state") != n["state"]:
            out.append(f"{label}: a down need of {n['env']} says {n['state']}, its environment {(row.get('envs') or {}).get(n['env'], {}).get('state')}")
    latest = row.get("latest")
    if latest is not None:
        # a version its secrets alone block is the secret need's: no blocked need beside it (amended 2026-11-09)
        blocked, listed = latest["stage"] == "blocked", bool(kinds & {("blocked", None), ("unchecked", None)})
        if (blocked and not (listed or ("secret", None) in kinds)) or (listed and not blocked):
            out.append(f"{label}: stage {latest['stage']} but its blocked need is {'missing' if blocked else 'listed'}")
    if "gate_state" in row:
        if (row["gate_state"] == "never_reviewed") != (("never_reviewed", None) in kinds):
            out.append(f"{label}: gate_state {row['gate_state']} and the never_reviewed need disagree")
        if (not row["owners"]) != (("unowned", None) in kinds):
            out.append(f"{label}: {len(row['owners'])} owners and the unowned need disagree")
        for n in needs:
            if n["kind"] == "unowned" and n["you"] != row["claimable"]:
                out.append(f"{label}: the unowned need says you={n['you']} but the row's claimable is {row['claimable']}")
        access = sum(n["kind"] == "access" for n in needs)
        if access != (row["access"].get("requests") or 0):
            out.append(f"{label}: {access} access needs but {row['access'].get('requests')} requests: one need a request, to whoever reads them")
    return out


def seam3_needs():
    """What needs a person (routes.md; #2724): the fleet's rows and the status read carry one ordered list, a need
    for every fact a person must act on and no other, counted in tiles.needs_you; a reduced row carries none but a
    responder's held need."""
    fleet_schema, ap_path = CONTRACTS / "schema" / "fleet.schema.json", CONTRACTS / "schema" / "app-page.schema.json"
    declared = load(fleet_schema)["definitions"]["need"]["properties"]["kind"]["enum"]
    if declared != NEED_KINDS:
        errors.append(f"needs: the schema's kinds {declared} are not routes.md's, in its order: {NEED_KINDS}")
        return
    for name in ("page", "member", "responder"):
        doc = load(CONTRACTS / "testdata" / "fleet" / f"{name}.json")
        if doc is None or not accepts(fleet_schema, doc):
            continue  # seam3 names the schema's errors
        carried = sum(len(a.get("needs", [])) for a in doc["apps"]) + len(doc["org_needs"])
        if doc["next"] is None and doc["tiles"]["needs_you"] != carried:
            errors.append(f"fleet: {name}.json: tiles.needs_you is {doc['tiles']['needs_you']} on a complete page whose rows carry {carried} needs")
        for a in doc["apps"]:
            if "gate_state" in a:
                errors.extend(needs_problems(f"fleet: {name}.json: {a['id']}", a["needs"], a))
            elif a.get("needs") and doc["viewer"]["role"] != "responder":
                errors.append(f"fleet: {name}.json: {a['id']}: a reduced row carrying needs; only a responder's held row does")
        if name == "page":
            for tile, kind in (("refused", "refused"), ("secrets_exposed", "secret")):
                n = sum(any(x["kind"] == kind for x in a.get("needs", [])) for a in doc["apps"])
                if doc["tiles"][tile] != n:
                    errors.append(f"fleet: page.json: tiles.{tile} is {doc['tiles'][tile]}, but {n} rows carry a {kind} need")
    for name, key in (("claims-portal.json", "status"), ("claims-portal.json", "status_approved"), ("claims-portal-held-status.json", None),
                      ("claims-portal-prod-stopped-status.json", None), ("claims-portal-rollback-status.json", None),
                      ("broker-portal-status.json", None), ("claims-archive-handoff-status.json", None)):
        doc = load(CONTRACTS / "testdata" / "app-page" / name)
        st = doc[key] if key else doc
        errors.extend(needs_problems(f"app-page: {name}{' ' + key if key else ''}", st["needs"], st))
    page = load(CONTRACTS / "testdata" / "fleet" / "page.json")
    member = load(CONTRACTS / "testdata" / "fleet" / "member.json")
    responder = load(CONTRACTS / "testdata" / "fleet" / "responder.json")
    row = next(a for a in page["apps"] if a["id"] == "premium-ledger")
    refused = {"kind": "refused", "env": "prod", "rank": 3, "since": "2026-08-28T10:04:01Z", "you": True, "ask": [], "destination": "api.stripe.com",
               "refusals": 14, "last_refused_at": "2026-09-02T11:58:00Z", "other_calls": 212, "held": False}
    held = next(a for a in responder["apps"] if a.get("needs"))
    nobody = next(a for a in member["apps"] if a["access"]["level"] == "none")
    viewer = next(a for a in member["apps"] if a["access"]["level"] == "viewer")
    with_need = lambda n: {**page, "apps": [{**row, "needs": [n]}]}
    failing = {"kind": "notify_failing", "env": None, "rank": 11, "since": "2026-11-09T15:00:00Z", "you": True, "ask": [], "channel": "Risk team", "channel_id": "risk-team"}
    lapsed = {"kind": "licence", "env": None, "rank": 11, "since": "2027-11-14T00:00:00Z", "you": True, "ask": [], "licence": "lapsed"}
    grace = {**lapsed, "licence": "grace", "grace_until": "2027-12-14T00:00:00Z"}
    for name, probe, ok in (("the fixture's refused need", with_need(refused), True),
                            ("a kind routes.md does not name", with_need({**refused, "kind": "stuck"}), False),
                            ("a refused need naming no destination", with_need({k: v for k, v in refused.items() if k != "destination"}), False),
                            ("a refused need of no environment", with_need({**refused, "env": None}), False),
                            ("a refused need naming admins to ask", with_need({**refused, "ask": [{"display": "Elena Vasquez", "as": "admin"}]}), False),
                            ("a need naming an owner to ask", with_need({"rank": 6, "kind": "blocked", "env": None, "since": None, "you": True, "ask": [{"display": "Dana Whitfield", "as": "owner"}], "version": "v5.0", "count": 1}), False),
                            ("a blocked need counting none", with_need({"rank": 6, "kind": "blocked", "env": None, "since": None, "you": True, "ask": [], "version": "v5.0", "count": 0}), False),
                            ("a down need of the app", with_need({"rank": 6, "kind": "down", "env": None, "since": None, "you": True, "ask": [], "state": "stopped"}), False),
                            ("a held need of preview", with_need({"rank": 6, "kind": "held", "env": "preview", "since": None, "you": True, "ask": []}), False),
                            ("a need with no you", with_need({k: v for k, v in refused.items() if k != "you"}), False),
                            ("a need with no rank", with_need({k: v for k, v in refused.items() if k != "rank"}), False),
                            ("a need ranked past the table", with_need({**refused, "rank": 12}), False),
                            ("a need carrying a field no kind has", with_need({**refused, "secret": "sk_live_x"}), False),
                            ("will_refuse naming what it denies", with_need({"rank": 6, "kind": "will_refuse", "env": "prod", "since": None, "you": True, "ask": [], "version": "v5.0", "blocks": True,
                                                                             "denied": [{"destination": "1.1.1.1", "port": 443, "allowable": False}, {"destination": "api.stripe.com", "port": 443, "allowable": True}]}), True),
                            ("will_refuse denying nothing", with_need({"rank": 6, "kind": "will_refuse", "env": "prod", "since": None, "you": True, "ask": [], "version": "v5.0", "blocks": True, "denied": []}), False),
                            ("an approval with a code not shown XXXX-XXXX", with_need({"rank": 6, "kind": "approval", "env": "prod", "since": None, "you": True, "ask": [], "version": "v5.0",
                                                                                       "asked_by": {"kind": "agent", "subject": "agent:x", "display": "x"}, "code": "ABCD1234", "expires_at": "2026-09-03T10:00:00Z"}), False),
                            ("a row read whole with no needs", {**page, "apps": [{k: v for k, v in row.items() if k != "needs"}]}, False),
                            ("a member's reduced row carrying needs", {**member, "apps": [{**nobody, "needs": [refused]}]}, False),
                            ("a responder's reduced row carrying a refused need", {**responder, "apps": [{**held, "needs": [refused]}]}, False),
                            ("a responder's reduced row carrying its held need", {**responder, "apps": [held]}, True),
                            ("tiles with no needs_you", {**page, "tiles": {k: v for k, v in page["tiles"].items() if k != "needs_you"}}, False),
                            ("a fleet read that says nothing of what it couldn't check", {k: v for k, v in page.items() if k != "unchecked"}, False),
                            ("a fleet read with no org_needs", {k: v for k, v in page.items() if k != "org_needs"}, False),
                            ("an admin's failing channel", {**page, "org_needs": [failing]}, True),
                            ("a member reading a failing channel", {**member, "org_needs": [failing]}, False),
                            ("an org need of an app's kind", {**page, "org_needs": [refused]}, False),
                            ("an app row carrying a failing channel", with_need(failing), False),
                            ("a failing channel of an environment", {**page, "org_needs": [{**failing, "env": "prod"}]}, False),
                            ("an admin's lapsed licence", {**page, "org_needs": [lapsed]}, True),
                            ("an admin's licence in grace", {**page, "org_needs": [grace, failing]}, True),
                            ("an admin's invalid licence", {**page, "org_needs": [{**lapsed, "licence": "invalid", "since": None}]}, True),
                            ("a licence in grace saying no end", {**page, "org_needs": [{k: v for k, v in grace.items() if k != "grace_until"}]}, False),
                            ("a lapsed licence with a grace end", {**page, "org_needs": [{**lapsed, "grace_until": "2027-12-14T00:00:00Z"}]}, False),
                            ("a licence need saying no state", {**page, "org_needs": [{k: v for k, v in lapsed.items() if k != "licence"}]}, False),
                            ("a licence need in force", {**page, "org_needs": [{**lapsed, "licence": "valid"}]}, False),
                            ("a member reading a lapsed licence", {**member, "org_needs": [lapsed]}, False),
                            ("an app row carrying a lapsed licence", with_need(lapsed), False),
                            # envs.<env>.patch (routes.md; #2883): on a row read whole, as the project read carries it; never on a reduced row.
                            ("a row's production carrying its patch state", {**page, "apps": [{**row, "envs": {**row["envs"], "prod": {**row["envs"]["prod"], "patch": {"state": "asked", "since": "2026-09-02T10:00:00Z", "identity": "sha256:" + "ab" * 32, "run": "gr_9a1c", "approval": "K7QF-2M9X"}}}}]}, True),
                            ("a row's patch state the contract doesn't name", {**page, "apps": [{**row, "envs": {**row["envs"], "prod": {**row["envs"]["prod"], "patch": {"state": "maybe", "since": "2026-09-02T10:00:00Z"}}}}]}, False),
                            ("a row's patch state saying no since", {**page, "apps": [{**row, "envs": {**row["envs"], "prod": {**row["envs"]["prod"], "patch": {"state": "patched"}}}}]}, False),
                            ("a viewer's reduced row carrying a patch state", {**member, "apps": [{**viewer, "envs": {"preview": {**viewer["envs"]["preview"], "patch": {"state": "patched", "since": "2026-09-02T10:00:00Z"}}}}]}, False),
                            ("an admin's patching no licence unlocks", {**page, "org_needs": [{"kind": "patching_unlicensed", "env": None, "rank": 11, "since": None, "you": True, "ask": []}]}, True),
                            ("patching no licence unlocks, of an environment", {**page, "org_needs": [{"kind": "patching_unlicensed", "env": "prod", "rank": 11, "since": None, "you": True, "ask": []}]}, False),
                            ("a member reading patching no licence unlocks", {**member, "org_needs": [{"kind": "patching_unlicensed", "env": None, "rank": 11, "since": None, "you": True, "ask": []}]}, False),
                            ("an app row carrying patching no licence unlocks", with_need({"kind": "patching_unlicensed", "env": None, "rank": 11, "since": None, "you": True, "ask": []}), False),
                            ("a go-live rule that couldn't check, unchecked in production", with_need({"rank": 6, "kind": "unchecked", "env": "prod", "since": None, "you": True, "ask": [], "version": "v5.0", "count": 1,
                                                                                                         "checks": [{"tool": "prod.egress-allowed", "reason": "Canyon can't read preview's connections right now.", "fix": "review"}]}), True),
                            ("unchecked in preview", with_need({"rank": 6, "kind": "unchecked", "env": "preview", "since": None, "you": True, "ask": [], "version": "v5.0", "count": 1,
                                                                "checks": [{"tool": "trivy", "reason": "x", "fix": "review"}]}), False)):
        if accepts(fleet_schema, probe) != ok:
            errors.append(f"selftest: {name} was {'refused' if ok else 'accepted'}")
    ap = load(CONTRACTS / "testdata" / "app-page" / "claims-portal.json")
    for name, probe, ok in (("a status read with no needs", {k: v for k, v in ap["status"].items() if k != "needs"}, False),
                            ("a responder's status read carrying a blocked need", {**ap["status_responder"], "needs": [
                                {"kind": "blocked", "env": None, "rank": 6, "since": None, "you": True, "ask": [], "version": "v1.4", "count": 1}]}, False)):
        if takes(ap_path, "status" if "blocked" not in name else "status_responder", probe) != ok:
            errors.append(f"selftest: {name} was {'refused' if ok else 'accepted'}")
    for name, needs, row, ok in (("a stopped production with no down need", [], {"envs": {"prod": {"state": "stopped"}}}, False),
                                 ("a held production listing down", [{"kind": "held", "env": "prod", "since": None}, {"kind": "down", "env": "prod", "since": None, "state": "stopped"}],
                                  {"hold": {}, "envs": {"prod": {"state": "stopped"}}}, False),
                                 ("needs out of rank", [{"kind": "blocked", "env": None, "since": None}, {"kind": "secret", "env": None, "since": None}], {}, False),
                                 ("needs in rank", [{"kind": "secret", "env": None, "since": None}], {}, True),
                                 ("a refused need of production ranked as preview's", [{"kind": "refused", "env": "prod", "rank": 7, "since": None}], {}, False)):
        if (not needs_problems("selftest", needs, row)) != ok:
            errors.append(f"selftest: {name} was {'refused' if ok else 'accepted'} by the needs check")
    if "### What needs a person" not in (CONTRACTS / "routes.md").read_text():
        errors.append("routes: no *What needs a person* section, which the fleet read and the status read name")


# ---------------------------------------------------------------- seam 3 — the responder and the hold
def seam3_hold():
    """The responder and the hold (routes.md, *The hold*; design/OPERATIONS.md §3; design/PERMISSIONS.md,
    Security responders): a responder's fleet page, where a row at none carries production's address and
    state and its hold and nothing of preview, and a held app's status read, whose receipt is not
    evaluated. A hold shows to contributors and above, responders and admins; never on a member's reduced row."""
    fleet_schema, ap_path = CONTRACTS / "schema" / "fleet.schema.json", CONTRACTS / "schema" / "app-page.schema.json"
    page = load(CONTRACTS / "testdata" / "fleet" / "responder.json")
    member = load(CONTRACTS / "testdata" / "fleet" / "member.json")
    st = load(CONTRACTS / "testdata" / "app-page" / "claims-portal-held-status.json")
    if not schema_check("fleet: responder", fleet_schema, page) or member is None:
        return
    if page["viewer"]["role"] != "responder":
        errors.append(f"fleet: responder.json is read by a {page['viewer']['role']}; it is a responder's page")
        return
    held = next((a for a in page["apps"] if "hold" in a), None)
    live = next((a for a in page["apps"] if "envs" in a and "hold" not in a), None)
    if held is None or live is None or any(a["access"]["level"] != "none" for a in page["apps"]):
        errors.append("fleet: responder.json must carry, at none, a held row and a row running in production, or it proves neither")
        return
    errors.extend(stats_problems("fleet: responder.json", page))
    nobody = next(a for a in member["apps"] if a["access"]["level"] == "none")
    for name, probe, ok in (("a responder's row at none carrying preview", {**page, "apps": [{**live, "envs": {**live["envs"], "preview": live["envs"]["prod"]}}]}, False),
                            ("a responder's row at none carrying what production runs", {**page, "apps": [{**live, "envs": {"prod": {**live["envs"]["prod"], "package": "pkg_8b21"}}}]}, False),
                            ("a responder's row carrying stack", {**page, "apps": [{**held, "stack": "Next.js 15"}]}, False),
                            ("a hold with no reason", {**page, "apps": [{**held, "hold": {k: v for k, v in held["hold"].items() if k != "reason"}}]}, False),
                            ("a hold reason of 1025 characters", {**page, "apps": [{**held, "hold": {**held["hold"], "reason": "x" * 1025}}]}, False),
                            ("a hold that ran nothing", {**page, "apps": [{**held, "hold": {**held["hold"], "version": None}}]}, True),
                            ("a member's row at none carrying production", {**member, "apps": [{**nobody, "envs": live["envs"]}]}, False),
                            ("a member's row at none carrying a hold", {**member, "apps": [{**nobody, "hold": held["hold"]}]}, False),
                            ("a team row carrying a hold", {**member, "apps": [{**member["apps"][0], "hold": held["hold"]}]}, True),
                            ("a role outside the three", {**page, "viewer": {**page["viewer"], "role": "security"}}, False)):
        if accepts(fleet_schema, probe) != ok:
            errors.append(f"selftest: {name} was {'refused' if ok else 'accepted'}")
    if not definition_check("app-page: claims-portal held status", ap_path, "status", st):
        return
    receipt_check("claims-portal held status receipt", st["receipt"], {"rules": []})
    refusal = load(CONTRACTS / "testdata" / "app-page" / "claims-portal-held-refusal.json")
    if definition_check("app-page: claims-portal held refusal", ap_path, "refusal_held", refusal) and refusal["hold"] != st["receipt"]["held"]:
        errors.append("app-page: the 409 held names another hold than the receipt; both read the one hold row")
    if "prod" in st["envs"]:
        errors.append("app-page: a held production with a row on the status read; the stop takes production off envs")
    if {k: v for k, v in st["hold"].items() if k != "version"} != st["receipt"]["held"]:
        errors.append("app-page: the status read's hold and its receipt's held disagree; one hold, read once")
    ap = load(CONTRACTS / "testdata" / "app-page" / "claims-portal.json")
    for name, receipt in (("a held receipt evaluating rules", {**st["receipt"], "rules": [{"id": "prod.clean-gate", "name": "Production needs a passing review", "blocks": True, "passed": True, "why": "v1.3 passed review"}]}),
                          ("a held receipt waiting on preview", {**st["receipt"], "waiting": "Deploy a new version to preview to promote it."}),
                          ("a held receipt blocking by another rule", {**st["receipt"], "blocked_by": ["prod.held", "prod.clean-gate"]})):
        if accepts(ap_path, {**ap, "status": {**st, "receipt": receipt}}):
            errors.append(f"selftest: {name} was accepted")
    for name, probe, ok in (("a refusal naming no admins", {k: v for k, v in refusal.items() if k != "admins"}, False),
                            ("a refusal's hold naming nobody", {**refusal, "hold": {k: v for k, v in refusal["hold"].items() if k != "by"}}, False),
                            ("a refusal's hold at no instant", {**refusal, "hold": {**refusal["hold"], "at": "2026-10-11 09:40"}}, False),
                            ("a refusal's hold naming the version", {**refusal, "hold": {**refusal["hold"], "version": "v1.3"}}, False),
                            ("the fixture's refusal", refusal, True)):
        if takes(ap_path, "refusal_held", probe) != ok:
            errors.append(f"selftest: {name} was {'refused' if ok else 'accepted'}")
    # The role is one of three wherever a row or a read names it: an audit row, the preferences' viewer.
    access_path, access = CONTRACTS / "schema" / "access.schema.json", load(CONTRACTS / "testdata" / "access" / "claims-portal.json")
    gr_path, gr = CONTRACTS / "schema" / "guardrails.schema.json", load(CONTRACTS / "testdata" / "guardrails" / "preferences.json")
    for role, ok in (("responder", True), ("security", False)):
        row = {**access["audit"][0], "role": role}
        if accepts(access_path, {**access, "audit": [row] + access["audit"][1:]}) != ok:
            errors.append(f"selftest: an audit row by a {role} was {'refused' if ok else 'accepted'}")
        member = gr["preferences_member"]
        if accepts(gr_path, {**gr, "preferences_member": {**member, "viewer": {**member["viewer"], "role": role}}}) != ok:
            errors.append(f"selftest: the preferences read by a {role} was {'refused' if ok else 'accepted'}")
    # routes.md names every refusal the hold routes answer, and the hold's two audit actions are governance.
    text = (CONTRACTS / "routes.md").read_text()
    for code in ("responder_required", "person_required", "dashboard_required", '"held"'):
        if code not in text.partition("### The hold")[2].partition("\n### ")[0]:
            errors.append(f"routes: *The hold* does not name {code}")
    for action in ("stop app", "lift hold"):
        if audit_categories().get(action, "governance") != "governance":
            errors.append(f"routes: {action} is not governance; a stop and a lift are the production change record")


# ---------------------------------------------------------------- seam 3 — app teams
def seam3_access():
    """App teams and the Access group (routes.md; design/PERMISSIONS.md wins on the model): a fleet page
    as a member reads it, and one app's access read, a share, the rows the group writes and its
    refusals. The member's page is its own sample beside fleet/page.json, which is an admin's."""
    fleet_schema, access_schema = CONTRACTS / "schema" / "fleet.schema.json", CONTRACTS / "schema" / "access.schema.json"
    page = load(CONTRACTS / "testdata" / "fleet" / "member.json")
    doc = load(CONTRACTS / "testdata" / "access" / "claims-portal.json")
    if not schema_check("fleet: member", fleet_schema, page) or not schema_check("access", access_schema, doc):
        return
    errors.extend(miscategorised("access", doc["audit"]))
    # 1. A member's page: the team rows (contributor or above) in the risk order, then the reduced rows
    #    by id alone; tiles count the team rows only; requests ride only on a row the member owns.
    apps = page["apps"]
    team = [a for a in apps if "gate_state" in a]
    reduced = sorted((a for a in apps if "gate_state" not in a), key=lambda a: a["id"])
    if page["viewer"]["role"] != "member" or not team or not reduced:
        errors.append("fleet: member.json must be a member's page with a team row and a reduced row, or it proves neither shape")
        return
    if [a["id"] for a in apps] != [a["id"] for a in sorted(team, key=fleet_order) + reduced]:
        errors.append(f"fleet: member.json orders {[a['id'] for a in apps]}; team rows in the risk order, then the reduced rows by id")
    for a in team:
        if a["access"]["level"] not in ("owner", "contributor"):
            errors.append(f"fleet: {a['id']} is a whole row read by a member at {a['access']['level']}; below contributor the row is reduced")
    for a in apps:
        if "requests" in a["access"] and a["access"]["level"] != "owner":
            errors.append(f"fleet: {a['id']} answers a {a['access']['level']} its access requests; they are its owners' and admins'")
        if a["access"]["level"] == "owner" and not a["claimable"]:
            errors.append(f"fleet: {a['id']} answers its owner claimable false; an owner's claim is always accepted")
        if a["owners"] and a["access"]["level"] != "owner" and a["claimable"]:
            errors.append(f"fleet: {a['id']} has an owner and answers a member at {a['access']['level']} claimable; it is its owners' and admins' to claim")
    counted = {"blocked": sum(1 for a in team if a["latest"]["stage"] == "blocked"),
               "approved": sum(1 for a in team if a["latest"]["stage"] == "approved"),
               "failed": sum(1 for a in team if a["latest"]["stage"] == "failed"),
               "never_reviewed": sum(1 for a in team if a["gate_state"] == "never_reviewed"),
               "unowned": sum(1 for a in team if not a["owners"])}
    for tile, n in counted.items():
        if page["tiles"][tile] != n:
            errors.append(f"fleet: member.json's tiles.{tile} is {page['tiles'][tile]}, but {n} of the member's team rows qualify")
    errors.extend(stats_problems("fleet: member.json", page))
    # 2. The shapes a schema must refuse: a reduced row says nothing beyond its seven fields but a
    #    viewer's live addresses, each a url and a state alone, is read below contributor only, and a
    #    level is one of four.
    row, whole = reduced[0], team[0]
    seen = next((a for a in reduced if a["access"]["level"] == "viewer" and a.get("envs")), None)
    nobody = next((a for a in reduced if a["access"]["level"] == "none"), None)
    if seen is None or nobody is None:
        errors.append("fleet: member.json must carry a viewer's reduced row with an address and one at none, or it proves neither")
        return
    env = next(iter(seen["envs"].values()))
    for name, probe in (("a reduced row at none carrying envs", {**nobody, "envs": {}}),
                        ("a reduced row at none carrying an address", {**nobody, "envs": seen["envs"]}),
                        ("a viewer's reduced row without its envs", {k: v for k, v in seen.items() if k != "envs"}),
                        ("a viewer's env carrying its package", {**seen, "envs": {"preview": {**env, "package": "pkg_7a1c"}}}),
                        ("a viewer's env carrying its digest", {**seen, "envs": {"preview": {**env, "image_digest": "sha256:cfd86d789f71"}}}),
                        ("a viewer's env saying who runs it", {**seen, "envs": {"preview": {**env, "run_by": "canyon"}}}),
                        ("a viewer's env waiting on its first release", {**seen, "envs": {"preview": {"run_by": "org", "pending": {"release": "rel_3d9a", "event": "deploy", "sent_at": "2026-09-02T10:00:00Z", "state": "accepted"}}}}),
                        ("a viewer's env with no address", {**seen, "envs": {"preview": {"state": "running"}}})):
        if accepts(fleet_schema, {**page, "apps": [probe]}):
            errors.append(f"selftest: {name} was accepted")
    for name, probe in (("a reduced row carrying stack", {**row, "stack": "Next.js 15"}),
                        ("a reduced row carrying last_run", {**row, "last_run": None}),
                        ("a reduced row without its summary", {k: v for k, v in row.items() if k != "summary"}),
                        ("a reduced row read by an owner", {**row, "access": {**row["access"], "level": "owner"}}),
                        ("a reduced row answering access requests", {**row, "access": {**row["access"], "requests": 1}}),
                        ("a reduced row saying who the app is shared with", {**row, "access": {**row["access"], "everyone": False, "people": 0, "groups": 0}}),
                        ("a whole row without its summary", {k: v for k, v in whole.items() if k != "summary"}),
                        ("a whole row without its access", {k: v for k, v in whole.items() if k != "access"}),
                        ("a whole row not saying who the app is shared with", {**whole, "access": {"level": whole["access"]["level"]}}),
                        ("a level outside the four", {**whole, "access": {**whole["access"], "level": "admin"}})):
        if accepts(fleet_schema, {**page, "apps": [probe]}):
            errors.append(f"selftest: {name} was accepted")
    levels = load(access_schema)["definitions"]["level"]["enum"]
    if load(fleet_schema)["definitions"]["access"]["properties"]["level"]["enum"] != levels + ["none"]:
        errors.append(f"fleet: a row's access.level is not the grant levels {levels} and none; one vocabulary")
    for name, grant, change in (("an everyone grant at contributor", {"kind": "everyone", "grantee": "everyone"}, {"level": "contributor"}),
                                ("a grant naming an address in capitals", {"kind": "email", "grantee": "Sam@meridian.example"}, {})):
        probe = copy.deepcopy(doc)
        probe["share"]["grant"] = grant
        probe["share"]["request"].update(change)
        if accepts(access_schema, probe):
            errors.append(f"selftest: {name} was accepted")
    probe = copy.deepcopy(doc)
    probe["audit"][0]["detail"].update(kind="everyone", grantee="everyone")
    probe["audit"][0]["detail"]["level"]["after"] = "owner"
    if accepts(access_schema, probe):
        errors.append("selftest: a share app row granting everyone owner was accepted")
    # 3. A share writes the grant whole, deletes that address's request, and its row says what changed.
    share, before = doc["share"], doc["read"]
    key = (share["grant"]["kind"], share["grant"]["grantee"])
    kept = [(e["kind"], e["grantee"], e["level"], e["roles"]) for e in before["entries"] if (e["kind"], e["grantee"]) != key]
    want = kept + [key + (share["request"]["level"], share["request"].get("roles", []))]
    if [(e["kind"], e["grantee"], e["level"], e["roles"]) for e in share["response"]["entries"]] != want:
        errors.append(f"access: the share answers entries that are not the read's with {key} written whole")
    if share["response"]["requests"] != [r for r in before["requests"] if (share["grant"]["kind"], r["email"]) != ("email", key[1])]:
        errors.append(f"access: sharing with {key[1]} leaves its request pending; an email grant deletes it in the same transaction")
    declared = {r["name"] for r in before["roles"]}
    for e in share["response"]["entries"] + [share["response"]["everyone"] or {"roles": []}]:
        if set(e["roles"]) - declared:
            errors.append(f"access: a grant holds {sorted(set(e['roles']) - declared)}, which no package declares; the PUT answers unknown_role")
    row = next((e for e in doc["audit"] if e["action"] == "share app"), None)
    if row and ((row["detail"]["kind"], row["detail"]["grantee"]) != key or row["detail"]["level"]["after"] != share["request"]["level"]
                or row["detail"]["roles"]["after"] != share["request"].get("roles", [])):
        errors.append(f"access: the share app row {row['detail']} is not the share it records")
    # 4. team_required names the owners the read's grants make, people and groups, and asks the first of
    #    them; owner_required asks the same one.
    owners = [e["display"] for e in before["entries"] if e["kind"] == "email" and e["level"] == "owner"]
    groups = sorted(e["grantee"] for e in before["entries"] if e["kind"] == "group" and e["level"] == "owner")
    who = owners[0] if owners else f"a member of the {groups[0]} group" if groups else "an admin"
    tr, orq = doc["team_required"], doc["owner_required"]
    if [o["display"] for o in tr["owners"]] != owners or tr["owner_groups"] != groups or tr["message"] != f"Ask {who} to add you to {doc['project']}":
        errors.append(f"access: team_required answers {tr['message']!r} with {tr['owners']} and {tr['owner_groups']}; the owners are {owners} and {groups}")
    if orq["message"] != f"only the owners of {doc['project']} and admins can do this: ask {who}":
        errors.append(f"access: owner_required answers {orq['message']!r}; it asks {who}")
    # 5. routes.md names every refusal App teams and the Access group answer, and an open claim never
    #    hands over an app that already holds secrets or runs something (ownership carries both).
    text = (CONTRACTS / "routes.md").read_text()
    claim = route_row("PUT /api/v1/projects/:id/owner")
    if "no secret row" not in claim or "nothing is deployed" not in claim:
        errors.append("routes: the claim row opens an ownerless app without requiring that it hold no secret row and run nothing")
    for code in ("team_required", "owner_required", "invalid_grantee", "invalid_level", "unknown_role", "groups_unsupported",
                 "already_on_team", "last_owner", "invalid_access", "invalid_roles", "invalid_description", "invalid_to"):
        if f'error:"{code}"' not in text and f'"{code}"' not in text:
            errors.append(f"routes: no refusal names {code}")


# ---------------------------------------------------------------- seam 3 — the app page's reads
# The three paged lists routes.md gives the app page: the fixture key, the row key, and the sort
# key a page is newest-first by. `id` is the row's name in an error and the tie-break.
PAGED = {"packages": ("packages", "created_at"), "gate_runs": ("runs", "started_at"), "audit": ("events", "ts")}
# The four go-live rules (snapshot.md, Two gates): placed before production only, and on every receipt.
PROMOTE_RULES = {"prod.clean-gate", "prod.human-approval", "stack.blessed", "owner.required"}
# The fifth and sixth, design/PATCHING.md §3.3's and design/EGRESS.md §3.4's: placed before production only
# too, and on a receipt while each is on.
PLATFORM_RULE = "platform.known-vuln"
EGRESS_RULE = "prod.egress-allowed"
WHILE_ON = (PLATFORM_RULE, EGRESS_RULE)
# routes.md, The promote receipt: the rules that can be unset, each on the one line it says it with.
NEUTRAL = {"stack.blessed": "no approved stacks set", EGRESS_RULE: "not checked: preview runs on your own platform",
           "owner.required": "no owner needed: sign-in is off on this Canyon"}
GO_LIVE = PROMOTE_RULES | set(WHILE_ON)


def production(rule):
    """A rule's state before production (snapshot.md, Two gates): a go-live rule's state, else its
    production, Off when absent."""
    return rule["state"] if rule["id"] in GO_LIVE else rule.get("production", "off")


def paged_rows(label, pages, rows_key, ts_key):
    """One list walked page by page with a small limit: every page newest first, the boundary
    steps strictly older, no row twice, `next` null on the last page and only there; a list that
    answers `total` and `page` (the audit read) answers every row of the walk as its total on each page,
    and page 1 on the first read, by neither page nor cursor, then null on each read by cursor. Returns the
    rows of every page together — the fixture is the whole project, which is what lets the checks
    below cross-reference it."""
    seen, out, last = {}, [], None
    for n, page in enumerate(pages):
        rows = page[rows_key]
        keys = [(r[ts_key], r["id"]) for r in rows]
        if keys != sorted(keys, reverse=True):
            errors.append(f"app-page: {label} page {n + 1} is not newest first by {ts_key} then id: {[r['id'] for r in rows]}")
        if last and keys and keys[0] >= last:
            errors.append(f"app-page: {label} page {n + 1} starts at {rows[0]['id']} ({keys[0][0]}), not older than page {n}'s last row {last[1]} ({last[0]})")
        for r in rows:
            if r["id"] in seen:
                errors.append(f"app-page: {label} row {r['id']} appears on page {seen[r['id']]} and again on page {n + 1}; a page boundary repeats nothing in a fixture that does not move")
            seen[r["id"]] = n + 1
        if (page["next"] is None) != (n == len(pages) - 1):
            errors.append(f"app-page: {label} page {n + 1} has next {page['next']!r}; null on the last page and only there")
        if "page" in page and page["page"] != (1 if n == 0 else None):
            errors.append(f"app-page: {label} page {n + 1} answers page {page['page']!r}; 1 on the first read, null on a read by cursor")
        out += rows
        last = keys[-1] if keys else last
    for n, page in enumerate(pages):
        if "total" in page and page["total"] != len(out):
            errors.append(f"app-page: {label} page {n + 1} answers total {page['total']}; the walk holds {len(out)} rows")
    return out


def receipt_check(label, receipt, snap):
    """The four go-live rules in the snapshot's order, then every rule placed before production in its order,
    with its names and before-production states; blocked_by exactly the
    blocking rules that did not pass. Returns blocked_by as the rules imply it. A receipt with nothing
    in preview is not evaluated: `waiting`, and no rules (routes.md, amended for canyonv2-5r11.20). Nor is one
    while production is held: `held`, and blocked_by `prod.held` alone (routes.md, *The hold*)."""
    if "held" in receipt:
        if receipt["rules"] or receipt["blocked_by"] != ["prod.held"] or "waiting" in receipt:
            errors.append(f"app-page: {label} is held and still evaluates {[r['id'] for r in receipt['rules']]}, or blocks by {receipt['blocked_by']}; a held receipt blocks by prod.held alone")
        return ["prod.held"]
    if "waiting" in receipt:
        if receipt["rules"] or receipt["blocked_by"]:
            errors.append(f"app-page: {label} is waiting on a preview and still evaluates {[r['id'] for r in receipt['rules']]}")
        return []
    want_ids = ([r["id"] for r in snap["rules"] if r["id"] in PROMOTE_RULES] + [r["id"] for r in snap["rules"] if r["id"] in WHILE_ON and r["state"] != "off"]
                + [r["id"] for r in snap["rules"] if "production" in r])
    ids = [r["id"] for r in receipt["rules"]]
    if ids != want_ids:
        errors.append(f"app-page: {label} rules are {ids}; the go-live rules then the rules placed before production, each in the snapshot's order, are {want_ids}")
    snap_rules = by_id(f"app-page: {label}", "rule", snap["rules"])
    if snap_rules is None:
        return []
    for r in receipt["rules"]:
        s = snap_rules.get(r["id"])
        if s is None:
            continue
        if r["name"] != s["name"]:
            errors.append(f"app-page: {label} rule {r['id']} is named {r['name']!r}; the snapshot says {s['name']!r}")
        if r["blocks"] != (production(s) == "blocking"):
            errors.append(f"app-page: {label} rule {r['id']} has blocks={r['blocks']} but its state before production is {production(s)}")
        if r["id"] == "prod.human-approval" and r["why"] != "a person promotes, or an agent they ask, with their CLI sign-in":
            errors.append(f"app-page: {label}'s prod.human-approval says {r['why']!r}; it states the rule in force — the fixtures' org leaves production approval off — never who is reading")
    for r in receipt["rules"]:  # only a rule with nothing set to check against is neutral, on its one line; anything else unset hides a blocker
        if r.get("unset") and NEUTRAL.get(r["id"]) != r["why"]:
            errors.append(f"app-page: {label} marks {r['id']} unset, saying {r['why']!r}; only {NEUTRAL} can be")
    want = [r["id"] for r in receipt["rules"] if r["blocks"] and not r["passed"] and not r.get("unset")]
    if receipt["blocked_by"] != want:
        errors.append(f"app-page: {label} blocked_by is {receipt['blocked_by']}; the blocking rules that did not pass are {want}")
    return want


def version_name(row):
    """routes.md, *Naming a version*: a version is its label when it has one, else its id without
    the `pkg_` prefix. `row` is anything that names a package — a run row, an environment."""
    return row.get("label") or row["package"][len("pkg_"):]


# routes.md, *A failure reason in words*: what a person reads for a failed run's reason.
FAILURE_WORDS = {
    "interrupted": "it was cancelled, or another run of this app replaced it",
    "error": "Canyon itself failed",
    "model": "the reviewer's model failed",
    "build": "the package did not build",
    "start": "the app did not stay up",
}


def counts(run, preview, deployed):
    """routes.md, *Patch runs*: whether a run is the receipt's and the status read's to read — any run but a patch
    run, and a patch run only while preview runs the image it deployed. deployed maps a patch run to that image."""
    return run.get("kind") != "patch" or (preview is not None and deployed.get(run["id"]) == preview.get("image_digest"))


def clean_gate(run, runs, preview, unapproved=None, deployed=None):
    """prod.clean-gate in routes.md's order, the first failing check answering. unapproved is the run's
    blocking findings no unexpired approval covers (all of them when None); runs are the gate-runs
    pages, newest first; deployed the image each patch run deployed (counts)."""
    if run is None:
        return False, "no run has looked at this app"
    rid, state, v = run["id"], run["state"], version_name(run)  # a reason names versions, never runs
    if state in ("pending", "running"):
        return False, f"{v}'s review is {state}"
    if state == "failed":
        return False, f"{v}'s run failed: {FAILURE_WORDS.get(run['failure_reason'], run['failure_reason'])}"
    n = run["blocking"] if unapproved is None else unapproved
    if n:
        return False, f"{v}'s review found {n} blocking finding{'' if n == 1 else 's'}, not approved"
    if run["package"] != preview["package"]:
        return False, f"the review named is {v}'s; preview runs {version_name(preview)}"
    newest = next(r for r in runs if r["package"] == run["package"] and counts(r, preview, deployed or {}))
    if newest["id"] != rid:
        return False, f"{v} has a newer review than the one named"
    if state == "blocked":
        return True, f"{v} passed review: every blocking finding approved"
    return True, f"{v} passed review: no blocking finding"


def rule_row(rule, run, findings, runs, preview):
    """The receipt row of a rule placed before production (routes.md, The promote receipt), read before any
    approval: on the run prod.clean-gate passes on alone — ended, of preview's package, its newest — else
    clean-gate's reason for the run; then passed when no finding of it blocks before production, why naming
    the count. findings are the run's (ruleId, severity); runs the gate-runs pages, newest first."""
    passed, why = clean_gate(run, runs, preview, unapproved=0)
    if not passed:
        return passed, why
    v = version_name(run)
    n = len([1 for rid, sev in findings if policy_of(rid) == rule["id"] and production(rule) == "blocking" and sev in ("critical", "high") and not rid.startswith(DOCKERFILE_CHECKS)])
    if n:
        return False, f"{v}'s review found {n} blocking finding{'' if n == 1 else 's'}, not approved"
    return True, f"{v} passed review: no blocking finding"


def policy_of(rid):
    """The rule a finding is judged under: a mapped result's policy, an imported one's sarif.<tool>, else its own id."""
    head = rid.split(".")
    if rid.startswith("sarif."):
        return ".".join(head[:2])
    return ".".join(head[:2]) if len(head) > 2 else rid


def promote_run_of(status, gate_runs, deployed=None):
    """routes.md, the status row: the newest run of the package preview runs that counts (a patch run only while
    preview runs the image it deployed), else last_run. gate_runs are the gate-runs pages, newest first."""
    preview = status["envs"].get("preview", {})
    if not preview.get("package"):
        return status["last_run"]
    return next((r["id"] for r in gate_runs if r["package"] == preview["package"] and counts(r, preview, deployed or {})), None)


def audit_categories():
    """routes.md's *Audit categories* table: each action named there and its category."""
    table = (CONTRACTS / "routes.md").read_text().partition("### Audit categories")[2]
    return dict(re.findall(r"^\| `([^`]+)` \| (governance|access|outcome) \|$", table, re.M))


def miscategorised(label, events):
    """Each event's category is its action's in routes.md's table, governance for an action the table does not name."""
    table = audit_categories()
    return [f"{label}: event {e['id']} ({e['action']}) is {e['category']}; routes.md's *Audit categories* says {table.get(e['action'], 'governance')}"
            for e in events if e["category"] != table.get(e["action"], "governance")]


def unverdicted(runs, audit):
    """A run that reached a verdict (it ended clean or blocked, or failed building, detecting or starting after
    one) has one Canyon row saying so, with the run row's counts (routes.md, the audit read): what breaks that."""
    out = []
    for r in runs:
        said = [e for e in audit if e["action"] == "verdict" and e.get("run") == r["id"]]
        if r["state"] not in ("clean", "blocked") and r.get("failure_reason") not in ("build", "detect", "start"):
            continue
        if len(said) != 1:
            out.append(f"app-page: run {r['id']} has {len(said)} verdict rows on the trail; a verdict writes one")
        elif (said[0]["detail"]["package"], said[0]["detail"]["blocking"], said[0]["detail"]["warnings"]) != (r["package"], r["blocking"], r["warnings"]) \
                or (r["state"] != "failed" and said[0]["detail"]["state"] != r["state"]):
            out.append(f"app-page: run {r['id']}'s verdict row {said[0]['detail']} disagrees with the run row")
    return out


def dependency_of(finding):
    """finding.md, *A dependency's name and version*: trivy's own lines in the problem — the last
    `Package: ` before the first `\nInstalled Version: ` with no line break between them, then the
    last `Vulnerability `, `Fixed Version: ` and `Link: ` lines after it — without `fixed_in`, which
    is its group's (dependency_groups); None for any other finding."""
    if finding.get("tool") != "sarif/trivy":
        return None
    head, sep, rest = finding["properties"]["canyon"]["problem"].partition("\nInstalled Version: ")
    i = head.rfind("Package: ")
    if not sep or i < 0 or "\n" in head[i:]:
        return None
    lines = rest.split("\n")
    name, version = head[i + len("Package: "):], lines[0]
    if not name or not version:
        return None
    out = {"name": name, "version": version}
    said = {}
    for line in lines[1:]:
        for key, prefix in (("vulnerability", "Vulnerability "), ("fixed", "Fixed Version: "), ("link", "Link: ")):
            if line.startswith(prefix):
                said[key] = line[len(prefix):]
    if said.get("vulnerability"):
        out["vulnerability"] = said["vulnerability"]
    if said.get("fixed"):
        out["fixed"] = said["fixed"]
    link = said.get("link", "")
    if "vulnerability" in out and link.startswith(f"[{out['vulnerability']}](") and link.endswith(")"):
        url = link[len(out["vulnerability"]) + 3:-1]
        if re.fullmatch(r"https://[!-(*-?A-~]+", url):
            out["link"] = url
    return out


def plain_version(s):
    """A plain X.Y.Z as a tuple, else None (cli.md §5.3: a distribution's revision is not one)."""
    m = re.fullmatch(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", s)
    return tuple(int(n) for n in m.groups()) if m else None


def fixed_in(installed, lists):
    """cli.md §5.3: the lowest version a list names, at or above installed, that fixes every list —
    a list fixes v when it names one at or below v in v's MAJOR.MINOR, or v is at or above its highest."""
    at = plain_version(installed)
    if at is None or len(lists) > 200 or any(len(one) > 32 for one in lists):
        return None
    parsed = [[plain_version(v.removeprefix(">=")) for v in one] for one in lists]
    if any(None in one for one in parsed):
        return None
    fixes = lambda one, v: v >= max(one) or any(f <= v and f[:2] == v[:2] for f in one)  # noqa: E731
    for c in sorted({v for one in parsed for v in one if v >= at}):
        if all(fixes(one, c) for one in parsed):
            return ".".join(str(n) for n in c)
    return None


def dependency_groups(findings):
    """Each finding's dependency with its group's fixed_in: the findings of one tool, dependency and blocks."""
    out = [dependency_of(f) for f in findings]
    for f, d in zip(findings, out):
        if d is None:
            continue
        lists = [o["fixed"].split(", ") for g, o in zip(findings, out) if o is not None and o.get("fixed")
                 and (g["tool"], o["name"], o["version"], g["properties"]["canyon"]["blocks"]) == (f["tool"], d["name"], d["version"], f["properties"]["canyon"]["blocks"])]
        v = fixed_in(d["version"], lists) if lists else None
        if v is not None:
            d["fixed_in"] = v
    return out


def seam3_dependency_approval():
    """One dependency's findings approved together (routes.md, POST /gate-runs/:run/findings/approve):
    the run read names each finding's dependency as its tool wrote it, the body names findings of one
    tool and one dependency, the answer approves each as the single approve does, in the body's order,
    and one audit row names the dependency and lists the findings."""
    schema = CONTRACTS / "schema" / "app-page.schema.json"
    doc = load(CONTRACTS / "testdata" / "app-page" / "birthday-list-dependency-approval.json")
    if doc is None:
        return
    shapes = (("run", "run_read"), ("approve_findings", "approve_findings_request"), ("approved_findings", "approved_findings"), ("audit", "event"))
    if not all([definition_check(f"app-page: dependency approval {k}", schema, d, doc[k]) for k, d in shapes]):
        return
    run, body, answer, row = doc["run"]["run"], doc["approve_findings"], doc["approved_findings"], doc["audit"]
    for f, want in zip(run["findings"], dependency_groups(run["findings"])):
        if f.get("dependency") != want:
            errors.append(f"app-page: run read finding {f['ruleId']} names dependency {f.get('dependency')}; its tool's lines say {want}")
    # A link is carried only as finding.md words it: the same id, https, no space and no `)` in the url.
    probe = copy.deepcopy(run["findings"][0])
    for link in ("[CVE-2018-16487](javascript:alert(1))", "[CVE-0000-0001](https://example.com/x)", "[CVE-2018-16487](https://a b)", "CVE-2018-16487 https://example.com/x",
                 "[CVE-2018-16487](https://avd.aquasec.com@evil.example/)"):
        probe["properties"]["canyon"]["problem"] = "Lodash: x. Package: lodash\nInstalled Version: 4.17.4\nVulnerability CVE-2018-16487\nLink: " + link
        if "link" in dependency_of(probe):
            errors.append(f"app-page: dependency_of carries the link {link!r}; finding.md carries none")
    if fixed_in("4.17.4", [["5.0.1", "4.17.21"], ["4.17.12"]]) != "4.17.21" or fixed_in("5.1.4", [["5.1.5", "5.0.11"], ["6.0.0"]]) != "6.0.0" or fixed_in("1.0.0", [["1.0.1-1+deb12u1"]]) is not None \
            or fixed_in("1.0.0", [["1.0.1"]] * 201) is not None or fixed_in("1.0.0", [[f"1.0.{n}" for n in range(1, 34)]]) is not None:
        errors.append("app-page: fixed_in is not cli.md §5.3's rule")
    by_fp = {f["partialFingerprints"]["canyon/v1"]: f for f in run["findings"]}
    named = [by_fp.get(fp) for fp in body["fingerprints"]]
    if None in named:
        errors.append("app-page: the dependency approval names a fingerprint the run's verdict does not carry")
        return
    if len({(f["tool"], (f.get("dependency") or {}).get("name"), (f.get("dependency") or {}).get("version")) for f in named}) != 1 or named[0].get("dependency") is None:
        errors.append("app-page: the dependency approval's findings are not one tool's and one dependency's")
        return
    want = [{"rule": f["ruleId"], "fingerprint": f["partialFingerprints"]["canyon/v1"], "reason": body["reason"], "expires_at": body["expires_at"]} for f in named]
    if [{k: a[k] for k in ("rule", "fingerprint", "reason", "expires_at")} for a in answer["approvals"]] != want:
        errors.append("app-page: the answer's approvals are not one per finding the body names, in its order, each with the body's reason and expiry")
    if any(a["approved_by"]["kind"] != "person" for a in answer["approvals"]) or len({json.dumps(a["approved_by"], sort_keys=True) for a in answer["approvals"]}) != 1:
        errors.append("app-page: a dependency approval is one person's (routes.md: only a person approves)")
    if row["action"] != "POST /api/v1/gate-runs/{run}/findings/approve" or row.get("run") != run["id"] or row["actor"] != answer["approvals"][0]["approved_by"]:
        errors.append("app-page: the dependency approval's audit row is not the route's, on its run, by its approver")
    if row["detail"] != {"tool": named[0]["tool"], "dependency": {k: named[0]["dependency"][k] for k in ("name", "version")}, "findings": [{k: w[k] for k in ("rule", "fingerprint")} for w in want],
                         "reason": body["reason"], "expires_at": body["expires_at"]}:
        errors.append(f"app-page: the dependency approval's audit detail {row['detail']} does not name the dependency and list the findings")
    errors.extend(miscategorised("app-page: dependency approval", [row]))
    request = {"$ref": f"{load(schema)['$id']}#/definitions/approve_findings_request"}
    for label, fps in (("names one finding twice", body["fingerprints"] + body["fingerprints"][:1]), ("names none", []), ("names 201", [f"f_{n:064x}" for n in range(201)])):
        if not list(Draft7Validator(request, registry=SCHEMAS).iter_errors({**body, "fingerprints": fps})):
            errors.append(f"app-page: the schema takes a dependency approval that {label}; routes.md takes 1 to 200 distinct fingerprints")


def seam3_app(snap, golden):
    """The app page's reads: the status read at rest, one project walked page by page (routes.md,
    the packages, gate-runs and audit rows), its promote refusal with the receipt, one approve
    body and the approval it answers, keyed on a finding of the golden log (seam 1), and a promotion
    that ships the image preview runs."""
    # 0. An app waiting on its first release (HAND-OFF §7: the app page says "waiting on your platform
    #    since 10:00"): broker-portal's status read, the moment the fleet's member page shows it — the
    #    same environments, and a receipt whose prod.clean-gate is routes.md's for that row's run.
    waiting = load(CONTRACTS / "testdata" / "app-page" / "broker-portal-status.json")
    member = load(CONTRACTS / "testdata" / "fleet" / "member.json")
    if definition_check("app-page: broker-portal status", CONTRACTS / "schema" / "app-page.schema.json", "status", waiting) and member is not None:
        row = next((a for a in member["apps"] if a["id"] == waiting["project"]), None)
        if row is None or row["envs"] != waiting["envs"] or row["last_run"] != waiting["last_run"]:
            errors.append("app-page: broker-portal's status read and its member fleet row disagree on its environments or its run")
        elif waiting["promote_run"] != promote_run_of(waiting, []):
            errors.append(f"app-page: broker-portal's promote_run is {waiting['promote_run']}; routes.md's status row gives {promote_run_of(waiting, [])}")
        elif waiting["envs"].get("preview", {}).get("url") is None and "waiting" not in waiting["receipt"]:
            errors.append("app-page: nothing runs in broker-portal's preview, so its receipt waits and evaluates nothing")
        else:
            receipt_check("broker-portal status receipt", waiting["receipt"], snap)
    # 0b. A production the org runs, checked in (#2452): claims-archive's status read, the moment the member's
    #     fleet shows it — the same environments, seen_at among them — and seen_at said beside no state, at
    #     every reduction: a viewer's and a responder's production carry it, a member's at none nothing.
    handed = load(CONTRACTS / "testdata" / "app-page" / "claims-archive-handoff-status.json")
    ap_path, fl_path = CONTRACTS / "schema" / "app-page.schema.json", CONTRACTS / "schema" / "fleet.schema.json"
    if definition_check("app-page: claims-archive status", ap_path, "status", handed) and member is not None:
        row = next((a for a in member["apps"] if a["id"] == handed["project"]), None)
        prod = handed["envs"].get("prod", {})
        if row is None or row["envs"] != handed["envs"] or row["last_run"] != handed["last_run"]:
            errors.append("app-page: claims-archive's status read and its member fleet row disagree on its environments or its run")
        elif prod.get("run_by") != "org" or "seen_at" not in prod or "state" in prod:
            errors.append("app-page: claims-archive's production is the org's and checked in: run_by org, a seen_at, no state")
        if "url" in prod and "seen_at" in prod:  # one without either is reported above, and builds no probe
            seen = {"url": prod["url"], "seen_at": prod["seen_at"]}
            viewer = next(a for a in member["apps"] if a["access"]["level"] == "viewer" and a.get("envs"))
            responder = load(CONTRACTS / "testdata" / "app-page" / "claims-portal.json")["status_responder"]
            for name, probe, ok in (("a viewer's production checked in", ("fleet", {**member, "apps": [{**viewer, "envs": {"prod": seen}}]}), True),
                                    ("a viewer's production checked in and running", ("fleet", {**member, "apps": [{**viewer, "envs": {"prod": {**seen, "state": "running"}}}]}), False),
                                    ("a viewer's check-in that is not a timestamp", ("fleet", {**member, "apps": [{**viewer, "envs": {"prod": {**seen, "seen_at": "recently"}}}]}), False),
                                    ("a responder's status read with production checked in", ("status_responder", {**responder, "envs": {"prod": seen}}), True),
                                    ("a responder's status read with preview checked in", ("status_responder", {**responder, "envs": {"preview": seen}}), False)):
                which, doc_ = probe
                got = accepts(fl_path, doc_) if which == "fleet" else takes(ap_path, "status_responder", doc_)
                if got != ok:
                    errors.append(f"selftest: {name} was {'refused' if ok else 'accepted'}")
    doc = load(CONTRACTS / "testdata" / "app-page" / "claims-portal.json")
    if not schema_check("app-page", CONTRACTS / "schema" / "app-page.schema.json", doc):
        return
    rows = {k: paged_rows(k, doc[k], *PAGED[k]) for k in PAGED}
    # 1. The fixture is one project: every run reviews a listed package, a check's run one never listed;
    #    every event is this project's and names a listed run when it names one; every package and every run is on the trail.
    packages = {p["id"] for p in rows["packages"]}
    runs = {r["id"] for r in rows["gate_runs"]}
    for r in rows["gate_runs"]:
        if (r["package"] in packages) == r.get("check", False):
            errors.append(f"app-page: run {r['id']} reviews {r['package']}, which the packages pages {'list: a check is never listed' if r.get('check') else 'do not list'}")
    #    The read of one package by id (routes.md, GET /projects/:id/packages/:pkg) answers one the pages never
    #    list: a check's, which a check's run reviews, named by the display of the trail's newest upload of it.
    one = doc["package_read"]["package"]
    if one["id"] in packages or one["source"]["kind"] != "check" or one["id"] not in {r["package"] for r in rows["gate_runs"] if r.get("check")}:
        errors.append(f"app-page: package_read reads {one['id']} ({one['source']['kind']}); the fixture reads a check's package, which the pages never list")
    made = [e for e in rows["audit"] if e["action"] == "POST /api/v1/projects/{id}/packages" and e["detail"].get("package") == one["id"]]
    if not made or one["source"]["actor"] != made[0]["actor"]["display"] or one["created_at"] != made[0]["ts"]:
        errors.append(f"app-page: package_read's {one['id']} is not the trail's newest upload of it, by its display and at its instant")
    named_runs, named_packages = set(), set()
    for e in rows["audit"]:
        if e.get("project") != doc["project"]:
            errors.append(f"app-page: event {e['id']} is {e.get('project')!r}'s; the pages were read with project={doc['project']}")
        if "run" in e and e["run"] not in runs:
            errors.append(f"app-page: event {e['id']} names {e['run']}, which the gate-runs pages do not list")
        named_runs.add(e.get("run"))
        named_packages.add(e["detail"].get("package"))
    for missing in sorted(runs - named_runs):
        errors.append(f"app-page: run {missing} started without an audit row; a run start is a state change")
    for missing in sorted(packages - named_packages):
        errors.append(f"app-page: package {missing} landed without an audit row; an upload is a state change")
    errors.extend(unverdicted(rows["gate_runs"], rows["audit"]))
    errors.extend(miscategorised("app-page", rows["audit"]))
    #    A session is named by its digest, never its secret (routes.md, the audit read): the 64 hex of a session's
    #    cookie on a row is refused, and so is a channel the table does not name.
    first = doc["audit"][0]["events"][0]
    for name, probe in (("a session's secret on a row", {**first, "session": "a" * 64}), ("a channel that is not one of the five", {**first, "via": "email"})):
        if accepts(CONTRACTS / "schema" / "app-page.schema.json", {**doc, "audit": [{**doc["audit"][0], "events": [probe]}] + doc["audit"][1:]}):
            errors.append(f"selftest: {name} was accepted")
    #    The fixture's pages are the server's own golden (cmd/canyon-server walks them), so a failed detect,
    #    which a server without the detect migration cannot hold, is a probe: one with no verdict row is caught.
    probe = {"id": "gr_probe", "package": "pkg_5c02", "state": "failed", "failure_reason": "detect", "blocking": 0, "warnings": 0}
    if not unverdicted([probe], rows["audit"]):
        errors.append("selftest: a failed detect with no verdict row on the trail passed the audit check")
    # 2. The status read at rest: its run and environments are the project's; the receipt is checked as the refusal's is.
    st = doc["status"]
    if st["project"] != doc["project"]:
        errors.append(f"app-page: status is {st['project']}'s, the fixture is {doc['project']}'s")
    if st["last_run"] != rows["gate_runs"][0]["id"]:
        errors.append(f"app-page: status last_run {st['last_run']} is not the newest run on the gate-runs pages")
    # promote_run is the newest run of the package preview runs, so a later run of another package never
    # stands in for it; the golden's newest run reviewed v1.4, which never reached preview.
    for label, read in (("status", st), ("status_approved", doc["status_approved"])):
        if read["promote_run"] != promote_run_of(read, rows["gate_runs"]):
            errors.append(f"app-page: {label} promote_run is {read['promote_run']}; the newest run of what preview runs is {promote_run_of(read, rows['gate_runs'])}")
    if promote_run_of(st, rows["gate_runs"]) == st["last_run"]:
        errors.append("selftest: the golden's newest run stands in for preview's, so promote_run checks nothing")
    for env, e in st["envs"].items():
        if e["package"] not in packages or e["gate_run"] not in runs:
            errors.append(f"app-page: status envs.{env} runs {e['package']} from {e['gate_run']}; both must be on the pages")
    receipt_check("status receipt", st["receipt"], snap)
    by_id = {r["id"]: r for r in rows["gate_runs"]}
    # The status and refusal are read before the approval of the hero run (last_run); status_approved
    # after it, the same, because the receipt at rest answers for promote_run, which that approval does
    # not touch. The refusal answers for the hero run, which a promotion of the newest run names.
    reads = (("status receipt", st, st["promote_run"]), ("refusal receipt", {"receipt": doc["refusal"]["receipt"]}, st["last_run"]),
             ("status_approved receipt", doc["status_approved"], doc["status_approved"]["promote_run"]))
    for label, read, named in reads:
        gate = clean_gate(by_id.get(named), rows["gate_runs"], st["envs"].get("preview"))
        rule = next((r for r in read["receipt"]["rules"] if r["id"] == "prod.clean-gate"), None)
        if rule is not None and (rule["passed"], rule["why"]) != gate:
            errors.append(f"app-page: {label} prod.clean-gate is {(rule['passed'], rule['why'])}; routes.md's checks, in order, give {gate}")
    # 2b. A rule placed before production (design/EXPERIENCE.md D14): the refusal's receipt read against
    #     claims-portal's effective snapshot: the hero run is not of the package preview runs, so its placed
    #     row reads clean-gate's reason, never the hero run's count.
    placed, effective_snap = doc["placed_receipt"], load(CONTRACTS / "testdata" / "snapshot" / "claims-portal.json")
    receipt_check("placed receipt", placed, effective_snap)
    hero = by_id[st["last_run"]]
    logged = [(res["ruleId"], res["properties"]["canyon"]["severity"]) for run in golden["runs"] for res in run["results"]]
    for r in placed["rules"]:
        s = next((x for x in effective_snap["rules"] if x["id"] == r["id"]), None)
        want = clean_gate(hero, rows["gate_runs"], st["envs"].get("preview")) if r["id"] == "prod.clean-gate" else \
            rule_row(s, hero, logged, rows["gate_runs"], st["envs"].get("preview")) if s is not None and "production" in s else (r["passed"], r["why"])
        if (r["passed"], r["why"]) != want:
            errors.append(f"app-page: placed receipt {r['id']} is {(r['passed'], r['why'])}; routes.md's receipt gives {want}")
    receipt_check("status_approved receipt", doc["status_approved"]["receipt"], snap)
    # 3. The refusal: the same receipt, blocking something, and the error code following from it.
    # A forged unset on a blocking rule must not slip it out of blocked_by (codex on #588).
    # The schema's empty rules are a waiting receipt's alone: an evaluated one names the four rules (codex on #594).
    empty, before = {**copy.deepcopy(doc["refusal"]["receipt"]), "rules": [], "blocked_by": []}, len(errors)
    definition_check("emptied refusal receipt", CONTRACTS / "schema" / "app-page.schema.json", "receipt", empty)
    caught = len(errors) > before
    del errors[before:]
    if not caught:
        errors.append("app-page: the schema accepts an evaluated receipt with no rules; only a waiting receipt is empty")
    forged, before = copy.deepcopy(doc["refusal"]["receipt"]), len(errors)
    forged["rules"][0].update(unset=True)
    forged["blocked_by"] = [i for i in forged["blocked_by"] if i != forged["rules"][0]["id"]]
    receipt_check("forged receipt", forged, snap)
    caught = len(errors) > before
    del errors[before:]
    if not caught:
        errors.append("app-page: the checker accepts prod.clean-gate marked unset and out of blocked_by")
    # prod.egress-allowed's neutral row (no golden receipt has the rule on): out of blocked_by at blocking on its one line,
    # and any other line marked unset is a blocker hidden.
    on = copy.deepcopy(snap)
    next(r for r in on["rules"] if r["id"] == EGRESS_RULE)["state"] = "blocking"
    neutral, before = copy.deepcopy(st["receipt"]), len(errors)
    row = {"id": EGRESS_RULE, "name": next(r["name"] for r in on["rules"] if r["id"] == EGRESS_RULE), "blocks": True, "passed": False, "why": NEUTRAL[EGRESS_RULE], "unset": True}
    at = max(i for i, r in enumerate(neutral["rules"]) if r["id"] in PROMOTE_RULES or r["id"] == PLATFORM_RULE) + 1
    neutral["rules"].insert(at, row)
    receipt_check("neutral receipt", neutral, on)
    if len(errors) > before:
        errors.append("app-page: the checker refuses prod.egress-allowed's neutral row")
    row["why"] = "production does not allow api.openai.com"
    before = len(errors)
    receipt_check("forged neutral receipt", neutral, on)
    caught = len(errors) > before
    del errors[before:]
    if not caught:
        errors.append("app-page: the checker accepts prod.egress-allowed marked unset on a line that names a destination")
    # owner.required's neutral row, on a host with sign-in off (no golden receipt is from one): accepted on its one line,
    # and any other line marked unset is a missing owner hidden.
    signed_off, before = copy.deepcopy(st["receipt"]), len(errors)
    owner = next(r for r in signed_off["rules"] if r["id"] == "owner.required")
    owner.update(passed=False, why="no owner needed: sign-in is off on this Canyon", unset=True)
    receipt_check("sign-in-off receipt", signed_off, snap)
    if len(errors) > before:
        errors.append("app-page: the checker refuses owner.required's neutral row on a host with sign-in off")
    owner["why"] = "no owner has claimed this app"
    before = len(errors)
    receipt_check("forged sign-in-off receipt", signed_off, snap)
    caught = len(errors) > before
    del errors[before:]
    if not caught:
        errors.append("app-page: the checker accepts owner.required marked unset on the line of an app nobody claimed")
    want = receipt_check("refusal receipt", doc["refusal"]["receipt"], snap)
    if not want:
        errors.append("app-page: the refusal's receipt blocks nothing; an empty blocked_by is the 200, not the 409")
    code = "gate_not_clean" if "prod.clean-gate" in want else "approval_required"
    if doc["refusal"]["error"] != code:
        errors.append(f"app-page: the refusal says {doc['refusal']['error']} but blocked_by {want} makes it {code}")
    # 4. A promotion ships what preview runs: the four copied fields are the preview row's, the url its own.
    pm, preview = doc["promotion"], doc["status"]["envs"]["preview"]
    if pm["url"] != pm["promotion"]["url"]:
        errors.append("app-page: promotion.url must repeat promotion.promotion.url")
    for k in ("package", "label", "image_digest", "gate_run", "deployed_by"):
        if pm["promotion"].get(k) != preview.get(k):
            errors.append(f"app-page: a promotion ships what preview runs: its {k} is the preview row's")
    if pm["promotion"]["url"] == preview["url"]:
        errors.append("app-page: a promotion answers the prod surface, not the preview url")
    passed, why = clean_gate(by_id.get(pm["promotion"]["gate_run"]), rows["gate_runs"], preview)
    if not passed:
        errors.append(f"app-page: the promotion ships {pm['promotion']['gate_run']}, whose clean gate does not pass: {why}")
    # 5. The approval answers the body it was given, for a finding the golden log carries under that rule.
    ap = doc["approved"]["approval"]
    for k in ("reason", "expires_at"):
        if ap[k] != doc["approve"][k]:
            errors.append(f"app-page: the approval's {k} {ap[k]!r} is not the body's {doc['approve'][k]!r}")
    findings = {(res["ruleId"], res["partialFingerprints"]["canyon/v1"]) for run in golden["runs"] for res in run["results"]}
    if (ap["rule"], ap["fingerprint"]) not in findings:
        errors.append(f"app-page: the approval keys {ap['rule']} {ap['fingerprint'][:14]}…, which is not a finding of the golden log")
    # 5b. The hero run read after it: the golden log's results in log order, the approved one carrying
    #     that approval less its key (the finding is the key), every other one carrying nothing more.
    rr = doc["run_approved"]["run"]
    if rr["id"] != st["last_run"]:
        errors.append(f"app-page: run_approved reads {rr['id']}; the approval was made on {st['last_run']}")
    results = [{**res, "tool": run["tool"]["driver"]["name"]} for run in golden["runs"] for res in run["results"]]  # each names the tool it came from
    for n, (got, res) in enumerate(zip(rr["findings"], results)):
        key = (res["ruleId"], res["partialFingerprints"]["canyon/v1"])
        want = {**res, "approval": {k: ap[k] for k in ("reason", "expires_at", "approved_by")}} if key == (ap["rule"], ap["fingerprint"]) else res
        # entry numbers the finding's entry (finding.md, One problem, one entry): gate/finding.Entries is its one
        # rule, checked by the server's tests; here it names a finding at or before it that is its own entry's first.
        entry = got.get("entry")
        if not isinstance(entry, int) or not 0 <= entry <= n or rr["findings"][entry].get("entry") != entry:
            errors.append(f"app-page: run_approved finding {n + 1} ({key[0]}) carries no entry its findings agree on")
        if {k: v for k, v in got.items() if k != "entry"} != want:
            errors.append(f"app-page: run_approved finding {n + 1} ({key[0]}) is not the golden log's result with its approval, if any")
    if len(rr["findings"]) != len(results):
        errors.append(f"app-page: run_approved has {len(rr['findings'])} findings; the golden log has {len(results)}")
    # 5b'. Who approves a blocking finding is the server's answer, never the screen's (#2204, #2206): approvable and
    #      blocks_production are true or absent, and the approve rows name the refusals of a blocking finding.
    ap_path = CONTRACTS / "schema" / "app-page.schema.json"
    first = rr["findings"][0]
    without = {k: v for k, v in rr.items() if k != "approvable"}
    if not takes(ap_path, "run_read", {"run": {**without, "link_github": True}}):
        errors.append("selftest: a run read asking its reader to link GitHub was refused")
    for name, probe in (("a run read approvable false", {**rr, "approvable": False}),
                        ("a run read link_github false", {**without, "link_github": False}),
                        ("a run read both approvable and link_github", {**rr, "link_github": True}),
                        ("a finding blocking production false", {**rr, "findings": [{**first, "blocks_production": False}] + rr["findings"][1:]})):
        if takes(ap_path, "run_read", {"run": probe}):
            errors.append(f"selftest: {name} was accepted")
    if not takes(ap_path, "run_read", {"run": {**rr, "findings": [{**first, "blocks_production": True}] + rr["findings"][1:]}}):
        errors.append("selftest: a finding blocking production was refused")
    # run_placed is run_approved under a rule placed blocking before production: one finding, advisory before
    # preview, carries blocks_production, and nothing else moves (#2206).
    placed = doc["run_placed"]["run"]
    marked = [f for f in placed["findings"] if f.get("blocks_production")]
    if len(marked) != 1 or marked[0]["properties"]["canyon"]["blocks"] or [{k: v for k, v in f.items() if k != "blocks_production"} for f in placed["findings"]] != rr["findings"]:
        errors.append("app-page: run_placed is not run_approved with one advisory finding marked blocks_production")
    # A responder with no grant reads the status trimmed to what approving needs (#2204): no earlier versions, and
    # production as a viewer reads it, never what it answered or who deployed it.
    sr = doc["status_responder"]
    if sr["receipt"] != doc["status"]["receipt"] or sr["last_run"] != doc["status"]["last_run"]:
        errors.append("app-page: status_responder is not the status read's receipt and runs")
    for name, probe in (("a responder's status with earlier versions", {**sr, "earlier": []}),
                        ("a responder's status with preview", {**sr, "envs": {"preview": {"url": "http://claims-portal.preview.localhost:7777"}}}),
                        ("a responder's status saying what production answered", {**sr, "envs": {"prod": {"url": "http://claims-portal.localhost:7777", "said": "ok"}}}),
                        ("a responder's status naming who deployed", {**sr, "envs": {"prod": {"url": "http://claims-portal.localhost:7777", "deployed_by": {"kind": "person", "subject": "x", "display": "x"}}}})):
        if takes(ap_path, "status_responder", probe):
            errors.append(f"selftest: {name} was accepted")
    for route in ("POST /api/v1/gate-runs/:run/findings/:fingerprint/approve", "POST /api/v1/gate-runs/:run/findings/approve"):
        for code in ("approver_required", "author_cannot_approve", "dashboard_required", "author"):
            if code not in (route_row(route) or ""):
                errors.append(f"routes: {route} does not name {code}")
    # 5c. A failed detect's read names what the host builds: the executor's builds for it, verbatim
    #     (routes.md, the run read; executor.md, /build).
    said, read = load(CONTRACTS / "testdata" / "executor" / "claims-portal.json")["build_undetected"], doc["run_undetected"]["run"]
    if read["builds"] != said["builds"]:
        errors.append(f"app-page: run_undetected names builds {read['builds']}; the executor answered {said['builds']}")
    if load(CONTRACTS / "schema" / "app-page.schema.json")["definitions"]["run_read"]["properties"]["run"]["properties"]["builds"] != \
            load(CONTRACTS / "schema" / "executor.schema.json")["definitions"]["build_undetected"]["properties"]["builds"]:
        errors.append("app-page: the run read's builds are not the shape of the executor's, which it names verbatim")
    if read.get("errored") != said.get("errored"):
        errors.append(f"app-page: run_undetected's errored is {read.get('errored')}; the executor answered {said.get('errored')}")
    # 6. One version, one name (routes.md, *Naming a version*): every read that names a package
    #    carries that package's label — or carries none, when the package has none.
    labels = {p["id"]: p.get("label") for p in rows["packages"]}
    named = [(f"run {r['id']}", r) for r in rows["gate_runs"]]
    named += [(f"{read} envs.{env}", e) for read in ("status", "status_approved") for env, e in doc[read]["envs"].items()]
    named.append(("the promotion", doc["promotion"]["promotion"]))
    for where, row in named:
        if row["package"] in labels and row.get("label") != labels[row["package"]]:
            errors.append(f"app-page: {where} names {row['package']} {row.get('label')!r}, the packages pages label it "
                          f"{labels[row['package']]!r}; a version is named the same way on every read")


# ------------------------------------------- seam 3 — org preferences and agentic setup
# The preferences key each setup tool writes (routes.md, POST /api/v1/guardrails/setup).
SETUP_TOOLS = {"set_stacks": "stacks", "set_design_system": "design_system"}  # the writing tools; the schema enumerates the same two, extend both together
SETUP_BODY_BYTES = 32 * 1024
# The organisation's settings on the preferences row (design/ONBOARDING.md §5) and the three
# refusals PUT answers for them; routes.md's row must name every one.
ORG_KEYS = ("org_name", "allowed_domains", "allowed_emails", "admin_emails", "admin_claim", "admin_value", "groups_claim", "onboarded_at")
PREFERENCES_REFUSALS = ("no_members", "no_admins", "set_by_operator")


def route_row(route):
    """The routes.md table row whose route cell names `route`: one line, which is how the file keeps a row."""
    for line in (CONTRACTS / "routes.md").read_text().splitlines():
        if line.startswith("|") and route in line.split("|")[1]:
            return line
    errors.append(f"routes: no row names {route}")
    return ""


def setup_state_prose():
    """The paragraph of routes.md that states the setup state: the one prose paragraph, outside
    any table, that names `setup_required`."""
    found = [p for p in (CONTRACTS / "routes.md").read_text().split("\n\n") if "setup_required" in p and not p.lstrip().startswith("|")]
    if len(found) != 1:
        errors.append(f"routes: {len(found)} prose paragraphs name setup_required; the setup state is stated once")
        return ""
    return found[0]


def org_settings(label, prefs):
    """design/ONBOARDING.md §5's two rules on a preferences read, the ones PUT refuses: the membership
    lists are not both empty, and the admin claim and value are both set or neither."""
    if not prefs["allowed_domains"] and not prefs["allowed_emails"]:
        errors.append(f"{label}: allowed_domains and allowed_emails are both empty — an open door or a brick; PUT refuses it no_members")
    if ("admin_claim" in prefs) != ("admin_value" in prefs):
        errors.append(f"{label}: admin_claim and admin_value are set together or not at all")
    if not prefs["admin_emails"] and not prefs.get("bootstrap_admins") and "admin_claim" not in prefs:
        errors.append(f"{label}: admin_emails and bootstrap_admins are both empty with no admin_claim; PUT refuses it no_admins")


def one_row_per_language(label, stacks):
    """routes.md: `stacks` is one row per language. Two rows for one language leave
    `stack.blessed` with two framework lists and no rule for choosing between them."""
    langs = [s["language"] for s in stacks]
    for lang in sorted({x for x in langs if langs.count(x) > 1}):
        errors.append(f"{label}: stacks carries {langs.count(lang)} rows for '{lang}'; one row per language")


def seam3_guardrails(snap):
    """The guardrails screen's reads and its two setup turns (routes.md). The five things the
    schema cannot say: one row per language; the chat's writes are what the form reads back;
    a proposal names a rule the snapshot carries and applies nothing (an `apply` flag the
    schema already refuses); the resolved default is one of the registry rows; and the skill
    manifest repeats the preferences read and the snapshot, deriving every field it hands out.
    Then the organisation's settings (design/ONBOARDING.md §5): the two rules PUT refuses, and
    the words routes.md must say — the three refusals, and that /ready answers 200 in setup."""
    doc = load(CONTRACTS / "testdata" / "guardrails" / "preferences.json")
    if not schema_check("guardrails", CONTRACTS / "schema" / "guardrails.schema.json", doc):
        return
    prefs = doc["preferences"]
    # 1. One row per language on the read (the snapshot golden's stacks stay empty: the org row is the truth).
    one_row_per_language("guardrails", prefs["stacks"])
    # 2. What the chat wrote is what the form reads: each tool's last `after` is what GET answers.
    last = {w["tool"]: w["after"] for w in doc["setup"]["response"]["writes"]}
    for tool, after in last.items():
        key = SETUP_TOOLS.get(tool)
        if key is None:
            errors.append(f"guardrails: setup writes {tool}, which sets nothing GET /preferences answers")
        elif after != prefs[key]:
            errors.append(f"guardrails: the last {tool} left {after!r} but GET /preferences answers {prefs[key]!r}; the form is the record")
    # 3. A proposal names a rule the snapshot carries, one of the three states, and writes nothing:
    #    the model has no apply tool, so applying stays the admin's click, attributed to them.
    turn = doc["proposal"]["response"]
    p = turn["proposal"]
    if p["rule"] not in {r["id"] for r in snap["rules"]}:
        errors.append(f"guardrails: the proposal names '{p['rule']}', which the snapshot does not carry")
    states = load(CONTRACTS / "schema" / "snapshot.schema.json")["properties"]["rules"]["items"]["properties"]["state"]["enum"]
    proposal_states = load(CONTRACTS / "schema" / "guardrails.schema.json")["definitions"]["proposal"]["properties"]["state"]["enum"]
    if sorted(states) != sorted(proposal_states):
        errors.append(f"guardrails: a proposal may name {sorted(proposal_states)} but the snapshot's states are {sorted(states)}; one vocabulary")
    manifest_states = load(CONTRACTS / "schema" / "guardrails.schema.json")["properties"]["manifest"]["properties"]["policies"]["items"]["properties"]["state"]["enum"]
    if sorted(states) != sorted(manifest_states):
        errors.append(f"guardrails: the manifest may name {sorted(manifest_states)} but the snapshot's states are {sorted(states)}; one vocabulary")
    if turn["writes"]:
        errors.append(f"guardrails: the proposal turn wrote {[w['tool'] for w in turn['writes']]}; a proposal applies nothing")
    # 4. A turn's request is at most 32 KB of body (routes.md): a byte bound a schema of code points
    #    cannot state, so the golden's request is measured.
    for label in ("setup", "proposal"):
        size = len(json.dumps(doc[label]["request"], separators=(",", ":")).encode())
        if size > SETUP_BODY_BYTES:
            errors.append(f"guardrails: the {label} turn's request is {size} bytes; the route takes at most {SETUP_BODY_BYTES}")
    # 5. One row per namespace in the registry (routes.md's 409), and the resolved default IS its row:
    #    every field the resolved shape carries equals the registry row's, not just the namespace.
    rows = doc["design_systems"]["design_systems"]
    seen = {}
    for r in rows:
        if r["namespace"] in seen:
            errors.append(f"guardrails: design-systems rows {seen[r['namespace']]} and {r['id']} both carry {r['namespace']}; one row per namespace")
        seen[r["namespace"]] = r["id"]
    ds = prefs["design_system"]
    if ds is not None:
        row = next((r for r in rows if r["namespace"] == ds["namespace"]), None)
        if row is None:
            errors.append(f"guardrails: preferences resolve {ds['namespace']}, which no design-systems row carries")
        elif any(ds.get(k) != row.get(k) for k in ds):
            errors.append(f"guardrails: preferences resolve {ds['namespace']} as {ds}, but its row reads {({k: row.get(k) for k in ds})}")
    # 6. The manifest an agent is handed is the preferences read and the snapshot, said again: every
    #    field is derived, so it carries no prose of its own and drift on either side names itself here.
    man = doc["manifest"]
    if man["stacks"] != prefs["stacks"]:
        errors.append(f"guardrails: the manifest hands an agent {man['stacks']!r} but GET /preferences answers {prefs['stacks']!r}; one approved list")
    resolved = None if ds is None else {k: v for k, v in ds.items() if k != "id"}
    if man["design_system"] != resolved:
        errors.append(f"guardrails: the manifest's design_system is {man['design_system']!r}, not the resolved default without its id, {resolved!r}")
    # Where a rule blocks in either gate, and only there, a Canyon rule carries its line on how to pass it
    # and a custom rule its text (#2622): an agent reads how to meet a rule that can stop its deploy, and
    # the names of the rest. The line is Canyon's own (gate/policy's howToPass), so the derivation takes
    # the golden's text and holds where it stands; the Go route test holds the text to the table.
    blocks = lambda r: "blocking" in (r["state"], production(r))
    policy_of = lambda r: {**{k: r[k] for k in ("id", "name", "state", "production", "instructions") if k in r and (k != "instructions" or blocks(r))},
                           **({"says": r["check"]["says"]} if blocks(r) and r.get("check", {}).get("by") == "code" else {})}  # beside a code-checked rule's text, what it checks (#2639)
    how_of = {p["id"]: p["how"] for p in man["policies"] if "how" in p}
    with_how = lambda r, p: {**p, "how": how_of.get(r["id"], "")} if blocks(r) and not r["id"].startswith("custom.") else p
    policies_of = lambda s: [with_how(r, policy_of(r)) for r in s["rules"] if "off" != r["state"] or "off" != production(r)]  # a rule Off in both gates is never handed to an agent
    rules = policies_of(snap)
    # The fixture carries no off rule and no custom rule (the canonical rules are byte-exact, EXPERIENCE D2), so
    # both filters above are no-ops on it; a variant with each proves the derivation drops an off rule and a
    # warn-only custom rule's text, and keeps a blocking one's, and deleting either filter turns this red.
    variant = copy.deepcopy(snap)
    variant["rules"] += [{"id": "x.off", "name": "Off", "state": "off", "producer": "scanner"}, {"id": "x.late", "name": "Late", "state": "off", "production": "advisory", "producer": "scanner"},
                         {"id": "custom.audit", "name": "Audit", "state": "off", "production": "blocking", "producer": "llm-review", "instructions": "Log every write."},
                         {"id": "custom.warn", "name": "Warn", "state": "advisory", "producer": "llm-review", "instructions": "Prefer UUIDs."},
                         {"id": "custom.gone", "name": "Gone", "state": "off", "producer": "llm-review", "instructions": "Never seen."},
                         {"id": "custom.readme", "name": "Readme", "state": "blocking", "producer": "scanner", "instructions": "Every app has a README.md.",
                          "check": {"by": "code", "expr": "'README.md' in files", "says": "The app has a README.md at its root.", "facts_version": 2}},
                         {"id": "custom.license", "name": "License", "state": "advisory", "producer": "scanner", "instructions": "Every app has a LICENSE.",
                          "check": {"by": "code", "expr": "'LICENSE' in files", "says": "The app has a LICENSE at its root.", "facts_version": 2}}]
    handed = {p["id"]: p for p in policies_of(variant) if not any(p["id"] == r["id"] for r in snap["rules"])}
    if handed.get("custom.readme", {}).get("says") != "The app has a README.md at its root." or "says" in handed.get("custom.license", {}) or "says" in handed.get("custom.audit", {}):
        errors.append("guardrails: the manifest derivation hands what a code check checks where the rule only warns or is checked by AI, or withholds it where it blocks")
    handed = {k: v for k, v in handed.items() if k not in ("custom.readme", "custom.license")}
    if list(handed) != ["x.late", "custom.audit", "custom.warn"]:
        errors.append("guardrails: the manifest derivation hands an agent a rule Off in both gates, or withholds one on before production")
    if handed.get("custom.audit", {}).get("instructions") != "Log every write." or "instructions" in handed.get("custom.warn", {}):
        errors.append("guardrails: the manifest derivation hands a custom rule's text where it only warns, or withholds it where it blocks")
    if any("how" in p for p in handed.values()):
        errors.append("guardrails: the manifest derivation hands a line on how to pass a custom rule, or a rule that only warns")
    # The schema refuses the text where the derivation would never hand it: on a rule that is not custom, or on one that only warns.
    for what, bad in (("the text of a Canyon rule", {"id": "secrets.client-bundle", "name": "No secrets in the code", "state": "blocking", "instructions": "x"}),
                      ("the text of a custom rule that only warns", {"id": "custom.warn", "name": "Warn", "state": "advisory", "production": "advisory", "instructions": "x"}),
                      ("how to pass a custom rule", {"id": "custom.audit", "name": "Audit", "state": "blocking", "how": "x"}),
                      ("what a code check checks without the rule's text", {"id": "custom.readme", "name": "Readme", "state": "blocking", "says": "x"}),
                      ("how to pass a rule that only warns", {"id": "deps.known-vuln", "name": "No known-vulnerable dependencies", "state": "advisory", "how": "x"})):
        broken = copy.deepcopy(doc)
        broken["manifest"]["policies"].append(bad)
        if not list(Draft7Validator(load(CONTRACTS / "schema" / "guardrails.schema.json"), registry=SCHEMAS).iter_errors(broken)):
            errors.append(f"guardrails: the schema takes a manifest that hands {what}")
    if man["policies"] != rules:
        at = next((i for i, (a, b) in enumerate(zip(man["policies"], rules)) if a != b), min(len(man["policies"]), len(rules)))
        errors.append(f"guardrails: the manifest's policy {at} is {man['policies'][at:at+1]!r} where the snapshot's rule {at} is {rules[at:at+1]!r}; every rule the snapshot carries that is on, as {{id, name, state, production?}}, in the snapshot's order")
    if man["component_types"] != snap["component_menu"]:
        errors.append(f"guardrails: the manifest offers component types {man['component_types']!r} but the snapshot's menu is {snap['component_menu']!r}")
    # 7. GET /api/v1/component-types answers one row per menu entry, in the snapshot's order — the
    #    same menu the manifest offers as bare names (check 6), but this shape carries prose: a
    #    name compares to a name, never a row to a row.
    ct_rows = doc["component_types"]["component_types"]
    ct_types = [r["type"] for r in ct_rows]
    if ct_types != snap["component_menu"]:
        errors.append(f"guardrails: component-types answers {ct_types!r} but the snapshot's menu is {snap['component_menu']!r}; one row per menu entry, in its order")
    for r in ct_rows:
        for g in r["gives"]:
            if g == "PORT":
                errors.append(f"guardrails: component type '{r['type']}' gives PORT, which the executor sets and no declaration may claim (executor.md)")
    # 8. The organisation's settings (design/ONBOARDING.md §5): the read and the PUT's answer keep the
    #    membership non-empty and the admin pair whole. The golden must pass, so each rule is also
    #    shown to fire on a variant that breaks it — a rule enforced by nothing reads as valid.
    org_settings("guardrails: preferences", prefs)
    org_settings("guardrails: preferences_put response", doc["preferences_put"]["response"])
    for name, mutate in (("both membership lists empty", lambda p: p.update(allowed_domains=[], allowed_emails=[])),
                         ("no admin address and no admin claim", lambda p: p.update(admin_emails=[])),
                         ("an admin claim without its value", lambda p: p.__setitem__("admin_claim", "groups"))):
        probe = copy.deepcopy(prefs)
        mutate(probe)
        mark = len(errors)
        org_settings("probe", probe)
        refused = len(errors) > mark
        del errors[mark:]
        if not refused:
            errors.append(f"guardrails: a read with {name} was not refused; the rule is enforced by nothing")
    # 8b. A member is not answered the admin claim (routes.md row 219, canyonv2-7mtz). The schema
    #     describes one object either way — which projection a reader gets is a rule about the role,
    #     not about the shape — so it goes here. The rule needs two shapes to compare, and they are
    #     their own samples: `preferences` and `preferences_put` are pinned byte for byte by
    #     cmd/canyon-server's TestThePreferencesAreTheGoldenShape, so widening either ahead of the
    #     server reds main. `overridden` is the same list for both roles, because a variable's name
    #     is not its value and a member who cannot tell a redacted field from an unset one reads an
    #     operator-pinned claim as no claim at all.
    admin, member = doc["preferences_admin"], doc["preferences_member"]
    for key in ("viewer", "admin_claim", "admin_value"):
        if key in prefs:
            errors.append(f"guardrails: the `preferences` sample carries {key}, which the server does not answer yet; "
                          "TestThePreferencesAreTheGoldenShape pins it byte for byte, so the widened read belongs in preferences_admin")
    if admin["viewer"]["role"] != "admin" or "admin_claim" not in admin:
        errors.append("guardrails: preferences_admin is not an admin read carrying the admin claim, so the member read proves no redaction")
    if member["viewer"]["role"] != "member":
        errors.append(f"guardrails: preferences_member is read by a {member['viewer']['role']}; it is the member's projection of the row")
    for key in ("admin_claim", "admin_value", "responder_emails", "responder_claim", "responder_value"):
        if key in member:
            errors.append(f"guardrails: the member read answers {key}, so a member is answered an admin's key; it is redacted for a member (routes.md row 219)")
    if "responder_emails" not in admin or ("responder_claim" in admin) != ("responder_value" in admin):
        errors.append("guardrails: preferences_admin answers no responder_emails, or half the responder claim; an admin reads the list and the pair whole")
    if member["overridden"] != admin["overridden"]:
        errors.append(f"guardrails: the member read's overridden is {member['overridden']!r} where the admin's is {admin['overridden']!r}; the keys the environment sets are named to both")
    org_settings("guardrails: preferences_admin", admin)
    org_settings("guardrails: preferences_member", member)
    # 9. routes.md's row names what the schema lets the read say. `overridden` lists the keys the
    #    environment sets, which a PUT cannot write: without the set_by_operator refusal the read
    #    documents an override the write does not honour — a state the route cannot produce. Every
    #    refusal is named in the row, and every overridable key is one the PUT accepts, so the
    #    refusal has a key to name.
    row = route_row("PUT /api/v1/preferences")
    schema = load(CONTRACTS / "schema" / "guardrails.schema.json")
    read_keys = schema["properties"]["preferences"]["properties"]
    put_keys = schema["definitions"]["preferencesPut"]["properties"]
    for key in ORG_KEYS:
        if key not in read_keys or key not in put_keys:
            errors.append(f"guardrails: the schema does not carry {key} on both the preferences read and the PUT (design/ONBOARDING.md §5)")
    if "overridden" in read_keys and "set_by_operator" not in row:
        errors.append("guardrails: GET /preferences answers `overridden`, the keys the environment sets, but routes.md's row names no 409 set_by_operator: "
                      "a PUT to such a key would answer 200 and change nothing — a state the route documents and cannot produce")
    for code in PREFERENCES_REFUSALS:
        if code not in row:
            errors.append(f"guardrails: routes.md's preferences row does not name the {code} refusal (design/ONBOARDING.md §5)")
    for key in schema["definitions"]["overridable"]["enum"]:
        if key not in put_keys:
            errors.append(f"guardrails: {key} can be overridden but the PUT does not accept it, so set_by_operator has nothing to refuse")
        if f"`{key}`" not in row:
            errors.append(f"guardrails: routes.md's preferences row does not name {key}")
    # 10. The setup state is stated once, and /ready answers 200 through it: a 503 pulls the host from a
    #     load balancer's rotation and puts the onboarding screen out of reach (design/ONBOARDING.md §2).
    #     Finishing setup is on the audit trail under its own action.
    prose = setup_state_prose()
    if prose and not re.search(r"`/ready`[^.]*`200`", prose):
        errors.append("routes: the setup-state paragraph does not say `/ready` answers `200` while onboarded_at is null; "
                      "a 503 pulls the host from a load balancer's rotation and puts the onboarding screen out of reach")
    if "`complete setup`" not in route_row("GET /api/v1/audit"):
        errors.append("routes: the audit read's action prose does not name `complete setup`, the row the PUT that sets onboarded_at writes")


# ---------------------------------------------------------------- seam 3 — the org's policies
def seam3_policies(snap):
    """The Policies screen's four answers (routes.md, the policies row). Two things the schema
    cannot say: the list is every rule the snapshot carries, in its order, under its name; and the
    preview and the apply name a rule the snapshot carries, with one vocabulary of states."""
    doc = load(CONTRACTS / "testdata" / "policies" / "meridian.json")
    schema = CONTRACTS / "schema" / "policies.schema.json"
    if not schema_check("policies", schema, doc):
        return
    listed = [(r["id"], r["name"]) for r in doc["policies"]["policies"]]
    carried = [(r["id"], r["name"]) for r in snap["rules"]]
    if listed != carried:
        errors.append(f"policies: GET lists {listed}, not the snapshot's rules in its order {carried}")
    for label in ("preview", "apply"):
        if doc[label]["rule"] not in {r["id"] for r in snap["rules"]}:
            errors.append(f"policies: the {label} names '{doc[label]['rule']}', which the snapshot does not carry")
    states = load(CONTRACTS / "schema" / "snapshot.schema.json")["properties"]["rules"]["items"]["properties"]["state"]["enum"]
    if sorted(states) != sorted(load(schema)["definitions"]["state"]["enum"]):
        errors.append(f"policies: the schema's states are not the snapshot's {sorted(states)}; one vocabulary")
    # Canyon-run tools (routes.md): run and settings on the four tools' sarif rules only, settings each
    # tool's allow-list, answered as the edit leaves them; and one settings shape across the seams.
    sa = doc["scanner_apply"]
    if {k: sa["response"]["policy"].get(k) for k in ("run", "settings")} != {k: sa["request"].get(k) for k in ("run", "settings")}:
        errors.append("policies: the scanner apply does not answer the run and settings it set")
    rules = {"repo": "https://github.com/meridian/opengrep-rules", "commit": "3f9a" * 10}
    for name, rule, request, ok in (
            ("opengrep's rules repo pinned to a commit", "sarif.opengrep", {"run": True, "settings": {"rules": rules, "skip_paths": ["vendor"]}}, True),
            ("checkov's frameworks and skipped checks", "sarif.checkov", {"settings": {"frameworks": ["terraform"], "skip_checks": ["CKV_AWS_20"]}}, True),
            ("run on a tool Canyon does not run", "sarif.semgrep", {"run": True}, False),
            ("run on a Canyon rule", "secrets.client-bundle", {"run": True}, False),
            ("a setting outside the tool's allow-list", "sarif.trivy", {"settings": {"severity": "HIGH"}}, False),
            ("a trivy scanner outside vuln, misconfig and secret", "sarif.trivy", {"settings": {"scanners": ["license"]}}, False),
            ("a rules repo over plain http", "sarif.opengrep", {"settings": {"rules": {**rules, "repo": "http://github.com/meridian/opengrep-rules"}}}, False),
            ("a rules repo at a branch, not a commit", "sarif.opengrep", {"settings": {"rules": {**rules, "commit": "main"}}}, False),
            ("rules on a tool other than opengrep", "sarif.grype", {"settings": {"rules": rules}}, False),
            ("a skip path outside the package", "sarif.grype", {"settings": {"skip_paths": ["../etc"]}}, False),
            ("a skip path that reads as an option", "sarif.grype", {"settings": {"skip_paths": ["--config=x"]}}, False),
            # platform.known-vuln takes settings {source} and no run (routes.md, the policies row; #2077)
            ("the platform rule's source", "platform.known-vuln", {"settings": {"source": "wiz"}}, True),
            ("run on the platform rule", "platform.known-vuln", {"run": True}, False),
            ("another key on the platform rule", "platform.known-vuln", {"settings": {"skip_paths": ["vendor"]}}, False)):
        if accepts(schema, {**doc, "scanner_apply": {**sa, "rule": rule, "request": request}}) != ok:
            errors.append(f"selftest: a policies apply with {name} was {'refused' if ok else 'accepted'}")
    if accepts(schema, {**doc, "policies": {"policies": doc["policies"]["policies"] + [{"id": "sarif.semgrep", "name": "Findings from semgrep", "state": "advisory", "run": True}]}}):
        errors.append("selftest: a policy row with run on a tool Canyon does not run was accepted")
    shape = lambda f: load(CONTRACTS / "schema" / f).get("definitions", {}).get("toolSettings", {})
    served = copy.deepcopy(shape("policies.schema.json"))
    served.get("opengrep", {}).get("properties", {}).pop("rules", None)  # the server fetches the rules repo; the executor gets a tar
    if shape("snapshot.schema.json") != shape("policies.schema.json") or shape("executor.schema.json") != served:
        errors.append("policies: a tool's settings are one shape in the policies, snapshot and executor schemas (opengrep's rules repo aside)")


def pipeline_problems(doc, snap):
    """routes.md, the policies row and the additions row (design/EXPERIENCE.md D14): what the schema cannot say."""
    rules, out = {r["id"]: r for r in snap["rules"]}, []
    pa = doc["production_apply"]
    org, got, d = rules[pa["rule"]], pa["response"]["policy"], pa["audit"]["detail"]
    if (got["id"] != pa["rule"] or got["state"] != org["state"] or production(got) != pa["request"]["production"] or d["rule"] != pa["rule"]
            or (d["before"], d["after"]) != (org["state"], org["state"]) or d.get("production") != {"before": production(org), "after": pa["request"]["production"]}):
        out.append("production alone sets before production and leaves before preview, and its policy.set row says so")
    ps = doc["production_scheduled"]
    org, got = rules[ps["rule"]], ps["response"]["policy"]
    if (got["state"] != org["state"] or production(got) != production(org) or "scheduled" in got or got.get("grace_days") != ps["request"]["grace_days"]
            or got.get("scheduled_production", {}).get("state") != ps["request"]["production"]):
        out.append("a graced production waits under scheduled_production alone, before production as it was and before preview untouched")
    pn = doc["production_no_model"]
    if rules[pn["rule"]]["producer"] != "llm-review" or pn["rule"].startswith("custom.") or pn["response"]["message"] not in route_row("PUT /api/v1/policies/:rule"):
        out.append("production_no_model names a rule no agent reviews, or not in routes.md's words")
    if doc["production_not_offered"]["response"]["message"] not in route_row("PUT /api/v1/policies/:rule"):
        out.append("production_not_offered is not in routes.md's words")
    ap = doc["app_policy_put"]
    org, row = rules[ap["rule"]], ap["response"]["policy"]
    gates = {"state": org["state"], "production": production(org)}
    if any(RANK[v] <= RANK[gates[k]] for k, v in ap["request"].items()):
        out.append("the addition is not stricter than the org's in every gate it names")
    if {k: row.get(k) for k in ("id", "name", "state", "production")} != {k: org.get(k) for k in ("id", "name", "state", "production")}:
        out.append("the addition answers a policy row that is not the org's rule")
    if {"project": ap["project"], **ap["request"]} not in [{k: v for k, v in a.items() if k not in ("set_by", "set_at")} for a in row.get("additions", [])]:
        out.append("the addition answers a policy row whose additions do not carry it")
    if ap["audit"]["project"] != ap["project"] or ap["audit"]["detail"] != {"rule": ap["rule"], **{k: {"before": gates[k], "after": v} for k, v in ap["request"].items()}}:
        out.append("the app_policy.set row does not name the app and each gate it changed, before and after")
    if any(w["project"] != ap["project"] for w in ap["response"]["would_block"]):
        out.append("an addition's would_block names another app")
    ns = doc["not_stricter"]
    org = rules[ns["rule"]]
    if all(RANK[v] > RANK[{"state": org["state"], "production": production(org)}[k]] for k, v in ns["request"].items()):
        out.append("not_stricter refuses an addition that is stricter in every gate it names")
    if ns["response"]["message"] not in route_row("PUT /api/v1/projects/:id/policies/:rule"):
        out.append("not_stricter is not in routes.md's words")
    return out


def seam3_pipeline(snap):
    doc = load(CONTRACTS / "testdata" / "policies" / "meridian.json")
    if not accepts(CONTRACTS / "schema" / "policies.schema.json", doc):
        return  # seam3_policies names the schema's errors
    for p in pipeline_problems(doc, snap):
        errors.append(f"policies: {p}")
    for name, mutate in (("production also moving before preview", lambda d: d["production_apply"]["response"]["policy"].update(state="blocking")),
                         ("a policy.set row without the production change", lambda d: d["production_apply"]["audit"]["detail"].pop("production", None)),
                         ("a pending production edit under the gate before preview's scheduled", lambda d: d["production_scheduled"]["response"]["policy"].update(scheduled=d["production_scheduled"]["response"]["policy"].pop("scheduled_production", {}))),
                         ("a pending production edit already in force", lambda d: d["production_scheduled"]["response"]["policy"].update(production="blocking")),
                         ("a production no_model on a scanner's rule", lambda d: d["production_no_model"].update(rule="deps.known-vuln")),
                         ("a production no_model in other words", lambda d: d["production_no_model"]["response"].update(message="No model.")),
                         ("an addition equal to the org's", lambda d: d["app_policy_put"]["request"].update(production="advisory")),
                         ("an addition missing from the row it answers", lambda d: d["app_policy_put"]["response"]["policy"]["additions"][0].update(project="quote-engine")),
                         ("an app_policy.set row naming the org's state after", lambda d: d["app_policy_put"]["audit"]["detail"]["production"].update(after="advisory")),
                         ("a not_stricter on a stricter addition", lambda d: d["not_stricter"]["request"].update(state="blocking", production="blocking") or d["not_stricter"].update(rule="deps.known-vuln"))):
        probe = copy.deepcopy(doc)
        mutate(probe)
        if not pipeline_problems(probe, snap):
            errors.append(f"selftest: the pipeline samples with {name} were not refused")
    schema = CONTRACTS / "schema" / "policies.schema.json"
    for name, mutate in (("production on a go-live row", lambda d: d["policies"]["policies"][2].update(production="advisory")),
                         ("a row placing a rule before production as off", lambda d: d["production_apply"]["response"]["policy"].update(production="off")),
                         ("production_not_offered on a rule that takes production", lambda d: d["production_not_offered"].update(rule="deps.known-vuln")),
                         ("a pending production edit on a go-live row", lambda d: next(r for r in d["policies"]["policies"] if r["id"] == "owner.required").update(scheduled_production={"state": "advisory", "effective_at": "2026-10-16T09:00:00Z"})),
                         ("a pending edit naming both gates in one", lambda d: d["production_scheduled"]["response"]["policy"].setdefault("scheduled_production", {}).update(production="blocking")),
                         ("grace_days on an addition", lambda d: d["app_policy_put"]["request"].update(grace_days=3)),
                         ("an addition setting run", lambda d: d["app_policy_put"]["request"].update(run=True))):
        probe = copy.deepcopy(doc)
        mutate(probe)
        if accepts(schema, probe):
            errors.append(f"selftest: the policies samples with {name} were accepted")
    held = copy.deepcopy(doc)  # a project whose promote_run did not have the rule on is held with nothing read (routes.md)
    held["production_apply"]["response"]["would_block"].append({"project": "renewal-reminder", "findings": 0})
    if not accepts(schema, held):
        errors.append("selftest: a would_block row of 0 findings, a project held for a rule its promote_run did not have on, was refused")


# ------------------------------------------------ seam 3 — the secrets a project's apps read
# What a reader must never be handed, because no route reads a value back (routes.md row 175):
# the value itself, or anything computed from one. A schema that names any of these has grown a
# key the route cannot fill.
VALUE_WORDS = ("value", "secret_value", "mask", "masked", "length", "digest", "preview_of", "hint")


def seam3_integrations():
    """contracts/routes.md, the channels row and *Integrations* (#2725): the golden fits its schema, a channel names no
    URL, a summary's apps are at most five, the rest counted, and a message carries names only."""
    doc = load(CONTRACTS / "testdata" / "integrations" / "meridian.json")
    schema = CONTRACTS / "schema" / "integrations.schema.json"
    if not schema_check("integrations", schema, doc):
        return
    for key in load(schema)["definitions"]["channel"]["properties"]:
        if "url" in key or "webhook" in key or "secret" in key:
            errors.append(f"integrations: a channel names {key!r}; no read answers a webhook URL, a bearer credential")
    for c in doc["list"]["channels"]:
        if "/" in c["host"] or c["host"].count(".") < 1:
            errors.append(f"integrations: {c['id']}'s host {c['host']!r} is not a host name")
    if doc["list"]["channels"] != sorted(doc["list"]["channels"], key=lambda c: c["id"]):
        errors.append("integrations: the list is not in id order")
    posted = set(load(schema)["definitions"]["message"]["properties"]["need"]["enum"])
    if not posted <= set(NEED_KINDS):
        errors.append(f"integrations: a message posts {sorted(posted - set(NEED_KINDS))}, no kind of *What needs a person*")
    for name, probe, ok in (("a message carrying an approval's code", {**doc, "messages": [{**doc["messages"][0], "fields": {"code": "ABCD-2345"}}]}, False),
                            ("a message carrying a stopped app's last line", {**doc, "messages": [{**doc["messages"][2], "fields": {"state": "restarting", "said": "panic: x"}}]}, False),
                            ("a preview need posted", {**doc, "messages": [{**doc["messages"][0], "env": "preview"}]}, False),
                            ("a clearing saying since when", {**doc, "messages": [{**doc["messages"][3], "since": "2026-11-09T14:02:00Z"}]}, False),
                            ("an app need with no app", {**doc, "messages": [{k: v for k, v in doc["messages"][0].items() if k != "app"}]}, False)):
        if accepts(schema, probe) != ok:
            errors.append(f"selftest: {name} was {'refused' if ok else 'accepted'}")


def seam3_secrets():
    """The secrets list, routes.md row 176. Four things the schema cannot say: the rows are in the
    server's order and that order is deliberately not name order, so a client that re-sorts moves a
    row; (env, name) is the table's key, so a list never names one twice; `origin` is `user` in v1,
    since `gate` and `provisioned` have no producer yet; and a name is an environment variable name
    the executor's env accepts, because that is what a secret becomes when the app runs."""
    doc = load(CONTRACTS / "testdata" / "secrets" / "claims-portal.json")

    def row_name(path):
        """Name an error by its secret, not by its index: a reader goes to DATABASE_URL, not to secrets/2."""
        if len(path) >= 2 and path[0] == "secrets":
            rest = "/".join(str(p) for p in path[2:])
            return f"{doc['secrets'][path[1]].get('name', path[1])}: {rest or '<row>'}"
        return None

    schema = CONTRACTS / "schema" / "secrets.schema.json"
    if not schema_check("secrets", schema, doc, row_name):
        return
    rows = doc["secrets"]
    # 1. The server's order, env then name, and the golden is not in name order: a client that sorted
    #    the answer itself would read the same rows here and print them somewhere else.
    order = [(r["env"], r["name"]) for r in rows]
    if order != sorted(order):
        errors.append(f"secrets: the golden is {order}, not the server's env-then-name order {sorted(order)}")
    names = [r["name"] for r in rows]
    if names == sorted(names):
        errors.append(f"secrets: the golden's names {names} are in name order, so a client that re-sorted them would pass; give one environment a name that sorts before another's")
    # 2. (env, name) is the row's key: a list that names one twice is not one the table can answer.
    if len(set(order)) != len(order):
        errors.append(f"secrets: {order} names one (env, name) twice; that pair is the row's key")
    # 3. `origin` is `user` until a producer writes one (routes.md, the secrets list).
    for r in rows:
        if r["origin"] != "user":
            errors.append(f"secrets: {r['name']} has origin {r['origin']}, which has no producer in v1")
    # 4. A name is an env var name the executor accepts: a secret is read as one when the app runs.
    env_name = re.compile(load(CONTRACTS / "schema" / "executor.schema.json")["definitions"]["run_request"]["properties"]["env"]["propertyNames"]["pattern"])
    for r in rows:
        if not env_name.match(r["name"]):
            errors.append(f"secrets: {r['name']} is not a name the executor's env accepts ({env_name.pattern})")
    # 5. No value, and nothing derived from one, anywhere in the shape this route may answer.
    for key in load(schema)["definitions"]["secret"]["properties"]:
        if any(word in key for word in VALUE_WORDS):
            errors.append(f"secrets: the schema names {key!r} on a row; no route reads a value back, so there is none to show")


# design/EGRESS.md §2.4: what a record never carries, by the names Envoy's access-log entry gives it.
EGRESS_NEVER = ("path", "query", "header", "body", "address", "user_agent", "referer", "forwarded_for", "request_id")
EGRESS_KEY = ("app", "env", "destination", "port")
EGRESS_DESTINATIONS = 500


def seam3_egress():
    """An app's calls out, design/EGRESS.md §2.4: one row per destination an app env calls. What the
    schema cannot say: a key is one row; a row is never last seen before it was first seen; the flags
    are sorted; invalid is on 443; an app env holds at most 500 rows, invalid not among them; and the
    schema itself names no field of the entry the receiver drops."""
    path = CONTRACTS / "schema" / "egress.schema.json"
    doc = load(CONTRACTS / "testdata" / "egress" / "claims-portal.json")
    if not schema_check("egress", path, doc):
        return
    row = doc["destinations"][0]
    for name, probe in (("a row carrying the request's path", {**row, "path": "/v1/charges?token=sk_live_" + "x" * 24}),
                        ("a row carrying the resolved address", {**row, "upstream_remote_address": "3.18.12.63:443"}),
                        ("a row carrying a header value", {**row, "request_headers": {"authorization": "Bearer x"}}),
                        ("a row carrying a minute's count", {**row, "at": "2026-10-11T10:02:00Z", "count": 3}),
                        ("a row of no app", {**row, "app": None}),
                        ("a row of an env no app has", {**row, "env": "staging"}),
                        ("a destination past the cap, as the minute rows had it", {**row, "destination": "other"}),
                        ("a destination whose label is 64 characters", {**row, "destination": "a" * 64 + ".example.com"}),
                        ("a destination with a trailing dot", {**row, "destination": "api.stripe.com."}),
                        ("a protocol Envoy does not speak to a destination", {**row, "last_protocol": "grpc"}),
                        ("a last seen with a fraction of a second", {**row, "last_seen_at": "2026-10-11T10:41:12.5Z"}),
                        ("a row with no credential refusal said: absent is never none (#2692)", {k: v for k, v in row.items() if k != "last_credential_refusal"}),
                        ("a credential refusal for not_allowed, which is no credential's", {**row, "last_credential_refusal": {"reason": "not_allowed", "at": row["last_seen_at"]}}),
                        ("a credential refusal with no time", {**row, "last_credential_refusal": {"reason": "inject_unavailable"}}),
                        ("a credential refusal carrying its body", {**row, "last_credential_refusal": {"reason": "inject_unavailable", "at": row["last_seen_at"], "line": "Canyon could not add"}})):
        if accepts(path, {**doc, "destinations": [probe]}):
            errors.append(f"selftest: egress: {name} was accepted")
    if not accepts(path, {**doc, "destinations": [row]}):
        errors.append("selftest: egress: a row the fixture carries was refused, so the probes above prove nothing")
    gap, cap, setting = doc["gaps"][2], doc["gaps"][1], doc["setting_admin"]
    for name, key, value in (("an unattributed gap that counts none", "gaps", [{**gap, "calls": None}]),
                             ("a gap of a reason no receiver writes", "gaps", [{**gap, "reason": "cap"}]),
                             ("a receiver_down gap counting calls no one counted", "gaps", [{**gap, "reason": "receiver_down"}]),
                             ("an unattributed gap naming an app", "gaps", [{**gap, "app": "claims-portal", "env": "prod"}]),
                             ("a report that says nothing of the calls", "dropped_report", {}),
                             ("a report of no calls: an unread count is never none", "dropped_report", {"calls": 0}),
                             ("a report of fewer calls than before: a decrease is never a negative gap", "dropped_report", {"calls": -3}),
                             ("a report past what any Envoy counts between two reads", "dropped_report", {"calls": 2 ** 40 + 1}),
                             ("a report naming an app: a lost call names none", "dropped_report", {"calls": 1, "app": "claims-portal"}),
                             ("an over_cap gap naming no app", "gaps", [{**cap, "app": None, "env": None}]),
                             ("an over_cap gap counting none", "gaps", [{**cap, "calls": None}]),
                             ("a member's read naming the proxy", "setting_member", {**doc["setting_member"], "proxy": setting["proxy"]}),
                             ("a via the setting does not have", "setting_put", {"via": "istio"}),
                             ("an admin's read naming no replicas", "setting_admin", {k: v for k, v in setting.items() if k != "replicas"}),
                             ("a replica holding no version", "setting_admin", {**setting, "replicas": [{**setting["replicas"][0], "have": "42"}]}),
                             ("a replica that never asked", "setting_admin", {**setting, "replicas": [{**setting["replicas"][0], "seen_at": None}]}),
                             ("a member's read naming the replicas", "setting_member", {**doc["setting_member"], "replicas": setting["replicas"]}),
                             ("a read naming a destination's path", "project_read", {**doc["project_read"], "envs": {**doc["project_read"]["envs"], "prod": {
                                 "via": "canyon", "destinations": [{**doc["project_read"]["envs"]["prod"]["destinations"][0], "path": "/v1/charges"}]}}})):
        if accepts(path, {**doc, key: value}):
            errors.append(f"selftest: egress: {name} was accepted")
    defs = load(path)["definitions"]
    for word in EGRESS_NEVER:
        for name in list(defs["row"]["properties"]) + list(defs["destination_read"]["properties"]):
            if word in name:
                errors.append(f"egress: the schema names {name!r} on a row; a record never carries a {word} (design/EGRESS.md §2.4)")
    seen = [tuple(r[k] for k in EGRESS_KEY) for r in doc["destinations"]]
    if len(set(seen)) != len(seen):
        errors.append("egress: destinations names one key twice; (app, env, destination, port) is one row")
    held = {}
    for r in doc["destinations"]:
        if r["last_seen_at"] < r["first_seen_at"]:
            errors.append(f"egress: {r['app']} {r['env']} {r['destination']} was last seen at {r['last_seen_at']}, before it was first seen at {r['first_seen_at']}")
        if r["last_response_flags"] != sorted(r["last_response_flags"]):
            errors.append(f"egress: {r['destination']}'s flags {r['last_response_flags']} are not sorted")
        refusal = r["last_credential_refusal"]
        if refusal is not None and not r["first_seen_at"] <= refusal["at"] <= r["last_seen_at"]:
            errors.append(f"egress: {r['destination']}'s credential refusal at {refusal['at']} is outside its row's {r['first_seen_at']} to {r['last_seen_at']}")
        if r["destination"] == "invalid" and r["port"] != 443:
            errors.append(f"egress: invalid is on 443, its one port, never {r['port']}")
        if r["destination"] != "invalid":
            held[(r["app"], r["env"])] = held.get((r["app"], r["env"]), 0) + 1
    for (app, env), n in held.items():
        if n > EGRESS_DESTINATIONS:
            errors.append(f"egress: {app} {env} holds {n} destinations; past {EGRESS_DESTINATIONS} a new one's calls are an over_cap gap")
    egress_reads(doc)
    egress_restrict(doc)


def egress_reads(doc):
    """The app's read and the host's setting, against the rows they read: claims-portal's destinations are
    its rows last seen since the read's since, per env, without the app and env, the latest seen first, and
    no other app's; its gaps are every gap ending in the window that names no app, and its own over_cap
    gaps — never another app's; an admin's read says the via a member's does."""
    read = doc["project_read"]
    app = "claims-portal"
    for env in ("preview", "prod"):
        want = [{k: v for k, v in r.items() if k not in ("app", "env")} for r in doc["destinations"]
                if r["app"] == app and r["env"] == env and r["last_seen_at"] >= read["since"]]
        want = sorted(sorted(want, key=lambda d: (d["destination"], d["port"])), key=lambda d: d["last_seen_at"], reverse=True)
        if read["envs"][env]["destinations"] != want:
            errors.append(f"egress: {app}'s {env} read is {read['envs'][env]['destinations']}; its rows since {read['since']} are {want}")
    if read["gaps"] != [g for g in doc["gaps"] if g["to"] >= read["since"] and g["app"] in (None, app)]:
        errors.append("egress: an app's read carries every gap ending in its window that names no app, and its own over_cap gaps alone")
    if not accepts(CONTRACTS / "schema" / "egress.schema.json", {**doc, "gaps": [{**doc["gaps"][2], "reason": "dropped", "calls": None}]}):
        errors.append("selftest: egress: a dropped gap no one counted — a message too large to read — was refused")
    if not accepts(CONTRACTS / "schema" / "egress.schema.json", {**doc, "gaps": [{**doc["gaps"][2], "reason": "envoy_dropped", "calls": None}]}):
        errors.append("selftest: egress: an envoy_dropped gap no one counted — Envoy restarted, a stream failed to open, a read failed — was refused")
    if not accepts(CONTRACTS / "schema" / "egress.schema.json", {**doc, "dropped_report": {"calls": None}}):
        errors.append("selftest: egress: a report that no one counted was refused")
    if doc["setting_admin"]["via"] != doc["setting_member"]["via"]:
        errors.append("egress: an admin and a member read one via")
    replicas = doc["setting_admin"]["replicas"]
    if [r["replica"] for r in replicas] != sorted({r["replica"] for r in replicas}):
        errors.append("egress: the admin's replicas are by name, sorted, each once")
    for r in replicas:  # design/EGRESS.md §3.2: current_at is when it asked holding the current version, or was served it
        if r["current_at"] is not None and (r["have"] is None or r["current_at"] > r["seen_at"]):
            errors.append(f"egress: replica {r['replica']} was current at {r['current_at']}, after it last asked or holding no version")
    proxy = load(CONTRACTS / "schema" / "executor.schema.json")["definitions"]["egress"]["properties"]["proxy"]["pattern"]
    if load(CONTRACTS / "schema" / "egress.schema.json")["definitions"]["setting_admin"]["properties"]["proxy"]["oneOf"][0]["pattern"] != proxy:
        errors.append("egress: the setting's proxy is the one a /run names, executor.schema.json's egress.proxy")


# design/EGRESS.md §3.4: pending egress requests an app env holds at most.
EGRESS_PENDING = 50


def egress_allows(entry, name):
    """design/EGRESS.md §3.3: an entry allows its own name, and *. and a name every name any depth below it,
    never the name itself."""
    return name.endswith(entry[1:]) if entry.startswith("*.") else name == entry


def egress_restrict(doc):
    """Layer 2, design/EGRESS.md §3.2–3.4. What the schema cannot say: an env's mode is its own, else the
    org's; every allowlist is sorted and holds no platform destination; a request is never of a destination
    its env allows, pending ones come first, each the latest refused first, at most 50 pending, and one
    refused in the read's window has its row, or an over_cap gap of its env then; the policy says what the reads say; the admin's fleet read is
    every row and gap in its window, in order; each new route row names its refusals, byte for byte; and
    Envoy's bodies are §8.4's, byte for byte, in The check's own rows. The probes: every IP-literal
    encoding §3.3 names, and every other shape it refuses, is no entry. Whether punycode decodes is Go's
    IDNA's to say: contracts/egress_test.go runs every name here through internal/destination."""
    path = CONTRACTS / "schema" / "egress.schema.json"
    read, policy = doc["project_read"], doc["policy"]
    for name, entry in (("a dotted IPv4 literal", "10.0.0.1"), ("an octal IPv4 literal", "0300.0250.1.1"),
                        ("a hex IPv4 literal", "0xc0.0xa8.0x1.0x1"), ("an integer IPv4 literal", "3232235777"),
                        ("a name ending in a hex number", "api.0x7f"), ("a wildcard over an IPv4 literal", "*.10.0.0.1"),
                        ("a bracketed IPv6 literal", "[::1]"), ("a v4-mapped IPv6 literal", "::ffff:10.0.0.1"),
                        ("a single label", "localhost"), ("a wildcard over a single label", "*.com"), ("a bare wildcard", "*"),
                        ("a wildcard inside a name", "api.*.stripe.com"), ("an upper-case name", "API.stripe.com"),
                        ("a trailing dot", "api.stripe.com."), ("a U-label", "bücher.de"), ("userinfo", "user@api.stripe.com"),
                        ("a port", "api.stripe.com:443"), ("a label of 64 characters", "a" * 64 + ".example.com"),
                        ("a name ending in a line break", "api.openai.com\n"), ("a wildcard ending in a line break", "*.stripe.com\n")):
        if accepts(path, {**doc, "policy": {**policy, "envs": [{**policy["envs"][0], "allowed": [entry]}]}}):
            errors.append(f"selftest: egress: {name} ({entry!r}) was accepted on an allowlist")
    for entry in ("xn--bcher-kva.de", "*.stripe.com", "1password.com", "123.example.com"):
        if not accepts(path, {**doc, "policy": {**policy, "envs": [{**policy["envs"][0], "allowed": [entry]}]}}):
            errors.append(f"selftest: egress: {entry!r}, a name §3.3 keeps, was refused on an allowlist")
    for name, key, value in (("a mode no app env has", "mode_put", {"mode": "off"}),
                             ("a request filed by no refusal", "project_read", {**read, "envs": {**read["envs"], "prod": {
                                 **read["envs"]["prod"], "requests": [{**read["envs"]["prod"]["requests"][0], "refusals": 0}]}}}),
                             ("a request in a state Allow leaves none in", "project_read", {**read, "envs": {**read["envs"], "prod": {
                                 **read["envs"]["prod"], "requests": [{**read["envs"]["prod"]["requests"][0], "state": "allowed"}]}}}),
                             ("an Allow an agent made: the route refuses every actor but a person", "project_read", {**read, "envs": {**read["envs"], "prod": {
                                 **read["envs"]["prod"], "allows": [{**read["envs"]["prod"]["allows"][0], "by": {**read["envs"]["prod"]["allows"][0]["by"], "kind": "agent"}}]}}}),
                             ("an Allow that says nothing of the refusals it closed", "project_read", {**read, "envs": {**read["envs"], "prod": {
                                 **read["envs"]["prod"], "allows": [{k: v for k, v in read["envs"]["prod"]["allows"][0].items() if k != "last_refused_at"}]}}}),
                             ("an env read with no allows", "project_read", {**read, "envs": {**read["envs"], "prod": {
                                 k: v for k, v in read["envs"]["prod"].items() if k != "allows"}}}),
                             ("a platform destination ending in a line break", "policy", {**policy, "platform": ["login.example-idp.com\n"]}),
                             ("a policy carrying a credential", "policy", {**policy, "routes": [{"destination": "api.stripe.com", "value": "sk_live_" + "x" * 24}]}),
                             ("a policy whose version is no epoch", "policy", {**policy, "epoch": "1"}),
                             ("a policy with no format", "policy", {k: v for k, v in policy.items() if k != "format"}),
                             ("an IPv4 address among an env's known names", "policy", {**policy, "envs": [{**policy["envs"][0], "known": ["10.0.0.1"]}]}),
                             ("a caller by a name, not an address", "policy", {**policy, "callers": [{**policy["callers"][0], "address": "claims-portal-prod"}]}),
                             ("a setting change naming nothing", "setting_put", {})):
        if accepts(path, {**doc, key: value}):
            errors.append(f"selftest: egress: {name} was accepted")
    org = doc["setting_member"].get("mode")
    if doc["setting_admin"].get("mode") != org:
        errors.append("egress: an admin and a member read one org mode")
    platform = set(read["platform"])
    for env, e in read["envs"].items():
        if e["mode"] != (e["app_mode"] or org):
            errors.append(f"egress: {env}'s mode is {e['mode']}; its own is {e['app_mode']}, the org's {org}")
        # #2722: who allowed each entry, one record an entry on the allowlist, sorted as it is; the refusals it closed came before it
        allows = [a["entry"] for a in e["allows"]]
        if allows != sorted(set(allows)) or not set(allows) <= set(e["allowed"]):
            errors.append(f"egress: {env}'s allows name {allows}; each is one entry of its allowlist {e['allowed']}, once, sorted")
        for a in e["allows"]:
            if a["last_refused_at"] is not None and a["last_refused_at"] > a["at"]:
                errors.append(f"egress: {env}'s Allow of {a['entry']} at {a['at']} closed a request last refused at {a['last_refused_at']}, after it")
        reqs = e["requests"]
        if reqs != sorted(sorted(reqs, key=lambda r: r["last_refused_at"], reverse=True), key=lambda r: r["state"] != "pending"):
            errors.append(f"egress: {env}'s requests are pending first, then dismissed, each the latest refused first")
        if sum(r["state"] == "pending" for r in reqs) > EGRESS_PENDING:
            errors.append(f"egress: {env} holds more than {EGRESS_PENDING} pending requests")
        for r in reqs:
            if r["last_refused_at"] < r["first_refused_at"]:
                errors.append(f"egress: {r['destination']} was last refused before it was first refused")
            if any(egress_allows(a, r["destination"]) for a in e["allowed"]):
                errors.append(f"egress: {env} allows {r['destination']}, so allowing it closed its request")
            # a refusal past the 500-row cap has no row: it is counted on the env's over_cap gap (§2.4)
            over_cap = any(g["reason"] == "over_cap" and g["app"] == "claims-portal" and g["env"] == env and g["from"] <= r["last_refused_at"] <= g["to"] for g in read["gaps"])
            if r["last_refused_at"] >= read["since"] and not over_cap and not any(d["destination"] == r["destination"] and d["last_seen_at"] >= r["last_refused_at"] for d in e["destinations"]):
                errors.append(f"egress: {env}'s request of {r['destination']} was refused at {r['last_refused_at']}, in the read's window, and {env} has no row of it seen since, nor an over_cap gap then")
        mine = [{"mode": p["mode"], "allowed": p["allowed"]} for p in policy["envs"] if p["app"] == "claims-portal" and p["env"] == env]
        if e["via"] is not None and mine != [{"mode": e["mode"], "allowed": e["allowed"]}]:
            errors.append(f"egress: the policy's claims-portal {env} is {mine}; the read says {e['mode']} {e['allowed']}")
    for where, allowed in [(f"the read's {env}", e["allowed"]) for env, e in read["envs"].items()] + [(f"the policy's {p['app']} {p['env']}", p["allowed"]) for p in policy["envs"]]:
        if allowed != sorted(allowed):
            errors.append(f"egress: {where} allowlist {allowed} is not sorted")
        if platform & set(allowed):
            errors.append(f"egress: {where} allowlist holds the platform destinations {sorted(platform & set(allowed))}; a platform destination is on no team's list")
    if read["platform"] != sorted(read["platform"]) or read["platform"] != policy["platform"]:
        errors.append("egress: the read's platform destinations are the policy's, sorted")
    # known[] (#2116): names a call of the env went through to in the window, sorted. A row last allowed was admitted then;
    # one last refused may have been admitted before, or never, so it bounds known[] from above alone.
    for p in policy["envs"]:
        rows = [r for r in doc["destinations"] if (r["app"], r["env"]) == (p["app"], p["env"]) and r["last_seen_at"] >= doc["fleet_read"]["since"]
                and r["destination"] != "invalid" and not re.fullmatch(r"[0-9]+(\.[0-9]+){3}", r["destination"])]
        admitted = {r["destination"] for r in rows if r["last_outcome"] == "allowed"}
        if p["known"] != sorted(set(p["known"])) or not admitted <= set(p["known"]) <= {r["destination"] for r in rows}:
            errors.append(f"egress: the policy's {p['app']} {p['env']} known[] is {p['known']}; its records admitted {sorted(admitted)}")
    held = [(c["app"], c["env"]) for c in policy["callers"]]
    envs = [(p["app"], p["env"]) for p in policy["envs"]]
    if len({c["address"] for c in policy["callers"]}) != len(held) or len(set(envs)) != len(envs) or not set(held) <= set(envs):
        errors.append("egress: the policy names each caller's address once and each app env once, and no caller of an env it does not run")
    fleet = doc["fleet_read"]
    want = sorted(sorted([r for r in doc["destinations"] if r["last_seen_at"] >= fleet["since"]], key=lambda r: tuple(r[k] for k in EGRESS_KEY)),
                  key=lambda r: r["last_seen_at"], reverse=True)
    if fleet["destinations"] != want:
        errors.append("egress: the fleet read is every row last seen since its since, the latest first, then by key")
    if fleet["gaps"] != sorted([g for g in doc["gaps"] if g["to"] >= fleet["since"]], key=lambda g: (g["from"], g["to"])):
        errors.append("egress: the fleet read's gaps are every gap ending since its since, the oldest first")
    egress_routes(doc)
    egress_ca(doc)
    egress_rows()


def egress_routes(doc):
    """Layer 3, design/EGRESS.md §4.1. What the schema cannot say: an env's routes are sorted, each destination
    once; a route's destination is one the check lets the env reach; and the policy names the read's routes. The
    probes: every way a value could ride out of the vault, and every header a route may never set; and neither
    definition may ever name a value, so a later field cannot quietly let one through."""
    path = CONTRACTS / "schema" / "egress.schema.json"
    read, policy, put, answer = doc["project_read"], doc["policy"], doc["route_put"], doc["route_value"]
    prod = read["envs"]["prod"]
    route = prod["routes"][0]
    in_policy = next(p for p in policy["envs"] if (p["app"], p["env"]) == ("claims-portal", "prod"))
    fake = "sk_live_" + "x" * 24

    def on_read(row):
        return {**read, "envs": {**read["envs"], "prod": {**prod, "routes": [row]}}}

    def on_policy(row):
        return {**policy, "envs": [{**in_policy, "routes": [row]}]}

    if not accepts(path, {**doc, "project_read": on_read(route)}) or not accepts(path, {**doc, "policy": on_policy(in_policy["routes"][0])}):
        errors.append("selftest: egress: a route the fixture carries was refused, so the probes below prove nothing")
    for name, key, value in (("a route read carrying a value", "project_read", on_read({**route, "value": fake})),
                             ("a route read carrying its value under another name", "project_read", on_read({**route, "secret": fake})),
                             ("a route read with no from_secret", "project_read", on_read({k: v for k, v in route.items() if k != "from_secret"})),
                             ("a route on a wildcard, which no one certificate names", "project_read", on_read({**route, "destination": "*.stripe.com"})),
                             ("a scheme no route has", "project_read", on_read({**route, "scheme": "digest"})),
                             ("a route read that says nothing of waiting", "project_read", on_read({k: v for k, v in route.items() if k != "waiting"})),
                             ("a route waiting on a secret it does not name", "project_read", on_read({**route, "waiting": {"reason": "secret", "secret": None}})),
                             ("a route waiting on a redeploy and naming a secret", "project_read", on_read({**route, "waiting": {"reason": "redeploy", "secret": "SSL_CERT_DIR"}})),
                             ("a wait the contract has no word for", "project_read", on_read({**route, "waiting": {"reason": "deploy", "secret": None}})),
                             ("a recorded root that is no fingerprint", "trust_state", {**doc["trust_state"], "signing_root": "sha256:abc"}),
                             ("a route waiting on a secret that sets no trust", "project_read", on_read({**route, "waiting": {"reason": "secret", "secret": "STRIPE_KEY"}})),
                             ("a policy route carrying a value", "policy", on_policy({**in_policy["routes"][0], "value": fake})),
                             ("a policy route with no rev", "policy", on_policy({k: v for k, v in in_policy["routes"][0].items() if k != "rev"})),
                             ("a policy route whose rev no write made", "policy", on_policy({**in_policy["routes"][0], "rev": 0})),
                             ("a route set without its value", "route_put", {k: v for k, v in put.items() if k != "value"}),
                             ("an empty value", "route_put", {**put, "value": ""}),
                             ("a value ending in a line break, which would split the header", "route_put", {**put, "value": "x\n"}),
                             ("a value with a line break inside", "route_put", {**put, "value": "x\r\nHost: evil.example"}),
                             ("a value with a NUL", "route_put", {**put, "value": "x\x00y"}),
                             ("a value past 8,192 characters", "route_put", {**put, "value": "x" * 8193}),
                             ("a header that is no field name", "route_put", {**put, "header": "X Token"}),
                             ("a placeholder that is no variable name", "route_put", {**put, "placeholder_env": "1KEY"}),
                             ("a placeholder ending in a line break", "route_put", {**put, "placeholder_env": "KEY\n"}),
                             ("a from-secret carrying a value", "route_from_secret", {**doc["route_from_secret"], "value": fake}),
                             ("a from-secret naming no secret", "route_from_secret", {k: v for k, v in doc["route_from_secret"].items() if k != "secret"}),
                             ("a fetch answering no rev", "route_value", {"value": answer["value"]}),
                             ("a fetch answering more than the value", "route_value", {**answer, "header": "authorization"})):
        if accepts(path, {**doc, key: value}):
            errors.append(f"selftest: egress: {name} was accepted")
    for header in EGRESS_HEADERS_REFUSED:
        if accepts(path, {**doc, "project_read": on_read({**route, "header": header})}):
            errors.append(f"selftest: egress: a route setting {header} was accepted")
    stored = load(path)["definitions"]["route_header"]["not"]["anyOf"]
    words = stored[2]["enum"] + [prefix + "anything" for prefix in re.findall(r"[a-z-]+-", stored[1]["pattern"])]
    for header in sorted({w for word in words for w in (word, word.upper(), "-".join(p.capitalize() for p in word.split("-")))}):
        for key in ("route_put", "route_from_secret"):
            if accepts(path, {**doc, key: {**doc[key], "header": header}}):
                errors.append(f"selftest: egress: a {key} setting {header} was accepted; stored lower-case it is no route_header")
    for name in EGRESS_PROXY_VARS + tuple(line.split("=")[0] for line in EGRESS_TRUST) + EGRESS_SECRET_REFUSED:
        for key, value in (("route_put", {**put, "placeholder_env": name}), ("route_from_secret", {**doc["route_from_secret"], "placeholder_env": name}),
                           ("project_read", on_read({**route, "placeholder_env": name}))):
            if accepts(path, {**doc, key: value}):
                errors.append(f"selftest: egress: a {key} naming the placeholder {name} was accepted")
    executor = (CONTRACTS / "executor.md").read_text()
    for name in EGRESS_PROXY_VARS:
        if not re.search(rf"`{name}[`=]", executor):
            errors.append(f"egress: executor.md's Egress does not set {name}, so it is no name to keep from a placeholder")
    for key, header, placeholder in (("route_put", "X-Api-Key", "STRIPE_SECRET_KEY"), ("route_from_secret", "Authorization", "OPENAI_API_KEY")):
        if not accepts(path, {**doc, key: {**doc[key], "header": header, "placeholder_env": placeholder}}):
            errors.append(f"selftest: egress: a {key} setting {header} with the placeholder {placeholder}, both ordinary, was refused")
    revs = [r["rev"] for p in policy["envs"] for r in p.get("routes", [])]
    if len(set(revs)) != len(revs) or answer["rev"] not in revs:
        errors.append(f"egress: the policy's revs are {revs}; one sequence over every route write draws them, so none repeats, and a fetch answers one of them")
    if not accepts(path, {**doc, "project_read": on_read({**route, "header": "x-api-key"})}):
        errors.append("selftest: egress: a route setting x-api-key, an ordinary header, was refused")
    defs = load(path)["definitions"]
    for name in ("route_read", "policy_route"):
        if "value" in defs[name]["properties"]:
            errors.append(f"egress: the schema's {name} names a value; no read and no policy ever carries one")
    platform = set(read["platform"])
    state = doc["trust_state"]
    host = state["via"] == "canyon" and state["run_by"] == "canyon"
    for env, e in read["envs"].items():
        names = [r["destination"] for r in e["routes"]]
        if names != sorted(set(names)):
            errors.append(f"egress: {env}'s routes are {names}; they are sorted by destination, each once")
        for name in names:
            if e["mode"] != "observe" and name not in platform and not any(egress_allows(a, name) for a in e["allowed"]):
                errors.append(f"egress: {env} holds a route to {name}, which its {e['mode']} does not allow")
        mine = next(p.get("routes", []) for p in policy["envs"] if (p["app"], p["env"]) == ("claims-portal", env))
        record = next((s for s in state["envs"] if (s["app"], s["env"]) == ("claims-portal", env)), {"egress_roots": [], "trust_secret": None})
        live = host and state["signing_root"] in record["egress_roots"]
        want = None if live else {"reason": "host", "secret": None} if not host else \
            {"reason": "secret", "secret": record["trust_secret"]} if record["trust_secret"] else {"reason": "redeploy", "secret": None}
        if any(r["waiting"] != want for r in e["routes"]):
            errors.append(f"egress: {env}'s routes wait for {want}, from its recorded roots, the signing root and the host; the read says otherwise")
        if [(r["destination"], r["header"], r["scheme"]) for r in mine] != [(r["destination"], r["header"], r["scheme"]) for r in e["routes"] if live]:
            errors.append(f"egress: the policy names claims-portal {env}'s routes while the signing root is not among its trusted roots, or leaves them out while it is")
    rollout = state["rollout"]
    if set(rollout["pre_run"]) != set(rollout["before"]) & set(rollout["carried"]) or set(rollout["settled"]) != set(rollout["carried"]):
        errors.append("egress: before a /run the trusted roots are the old set intersected with the carried one, and the carried set alone once the old instance is gone")


def egress_ca(doc):
    """The interception roots, the leaf egress-control serves and a rotation (design/EGRESS.md §4.5; #2704). What the
    schema cannot say: the admin's key dates are the newest root first, the newest ending at its certificate's end and
    each older one at most 60 days after the newest began; Canyon rotates by itself 60 days before its own newest
    root's end and never an admin's sub-CA; and one signing root, the same in the admin's read, the policy, the trust
    the read's waits come from and the leaf. The probes: every way a key could ride out where it must not, and more
    roots than one rotation at a time leaves. An admin's sub-CA's body is a fixture of its own, as gitleaks' private-key
    rule spans from one key to the next in a file, and the leaf's carries one already."""
    path = CONTRACTS / "schema" / "egress.schema.json"
    setting, policy, leaf = doc["setting_admin"], doc["policy"], doc["leaf"]
    rotate = load(CONTRACTS / "testdata" / "egress" / "byo-rotation.json")
    ca = setting["ca"]
    root = ca["roots"][0]

    def on_ca(**kw):
        return {**setting, "ca": {**ca, **kw}}

    cert = rotate["cert"].split("-----END CERTIFICATE-----\n")[0] + "-----END CERTIFICATE-----\n"
    for name, body in (("a rotation to a root Canyon mints, {}", {}), ("an admin's sub-CA, byo-rotation.json", rotate)):
        if not accepts(path, {**doc, "ca_rotate": body}):
            errors.append(f"selftest: egress: {name} was refused, so the probes below prove nothing")
    for name, key, value in (("an admin's read naming no key dates", "setting_admin", {k: v for k, v in setting.items() if k != "ca"}),
                             ("a member's read naming the key dates", "setting_member", {**doc["setting_member"], "ca": ca}),
                             ("a root naming its key", "setting_admin", on_ca(roots=[{**root, "key": leaf["key"]}])),
                             ("a root with no end", "setting_admin", on_ca(roots=[{k: v for k, v in root.items() if k != "ends_at"}])),
                             ("a root that says nothing of whose it is", "setting_admin", on_ca(roots=[{k: v for k, v in root.items() if k != "byo"}])),
                             ("three roots at once: one rotation at a time", "setting_admin", on_ca(roots=[root] * 3)),
                             ("a signing root that is no fingerprint", "setting_admin", on_ca(signing="sha256:" + root["fingerprint"])),
                             ("key dates with no rotation date", "setting_admin", {**setting, "ca": {k: v for k, v in ca.items() if k != "rotates_at"}}),
                             ("a policy naming a signing root that is no fingerprint", "policy", {**policy, "signing_root": "sha256:abc"}),
                             ("a leaf answer without its key", "leaf", {k: v for k, v in leaf.items() if k != "key"}),
                             ("a leaf answer naming no root", "leaf", {k: v for k, v in leaf.items() if k != "root"}),
                             ("a leaf answer carrying more than the leaf", "leaf", {**leaf, "value": "canyon-fixture-not-a-credential"}),
                             ("a leaf key that is no PKCS #8", "leaf", {**leaf, "key": leaf["key"].replace("PRIVATE KEY", "EC PRIVATE KEY")}),
                             ("a leaf of two certificates", "leaf", {**leaf, "cert": leaf["cert"] * 2}),
                             ("a chain carrying a key", "leaf", {**leaf, "chain": leaf["chain"] + leaf["key"]}),
                             ("a leaf with no chain", "leaf", {**leaf, "chain": ""}),
                             ("a leaf with no end", "leaf", {k: v for k, v in leaf.items() if k != "not_after"}),
                             ("a rotation naming a sub-CA and no key", "ca_rotate", {"cert": rotate["cert"]}),
                             ("a rotation naming a key and no sub-CA", "ca_rotate", {"key": rotate["key"]}),
                             ("a rotation naming a root by fingerprint", "ca_rotate", {"fingerprint": root["fingerprint"]}),
                             ("a sub-CA of five certificates", "ca_rotate", {**rotate, "cert": cert * 5}),
                             ("a sub-CA's key that is no PKCS #8", "ca_rotate", {**rotate, "key": rotate["key"].replace("PRIVATE KEY", "RSA PRIVATE KEY")}),
                             ("a sub-CA with something beside its certificates", "ca_rotate", {**rotate, "cert": "issued by the PKI team\n" + rotate["cert"]})):
        if accepts(path, {**doc, key: value}):
            errors.append(f"selftest: egress: {name} was accepted")
    defs = load(path)["definitions"]
    for name in ("ca_read", "setting_admin"):
        if "key" in json.dumps(defs[name]["properties"]).replace('"keys"', ""):
            errors.append(f"egress: the schema's {name} names a key; no read ever carries one")

    def at(s):
        return datetime.datetime.fromisoformat(s.replace("Z", "+00:00"))

    roots = ca["roots"]
    if [r["created_at"] for r in roots] != sorted((r["created_at"] for r in roots), reverse=True) or len({r["fingerprint"] for r in roots}) != len(roots):
        errors.append("egress: the admin's roots are each root once, the newest first")
    if roots and roots[0]["ends_at"] != roots[0]["not_after"]:
        errors.append("egress: the newest root ends at its certificate's end: no rotation replaced it")
    for r in roots[1:]:
        if at(r["ends_at"]) > min(at(r["not_after"]), at(roots[0]["created_at"]) + datetime.timedelta(days=60)):
            errors.append(f"egress: root {r['fingerprint'][:12]} ends {r['ends_at']}, past 60 days after the rotation that replaced it or its own end")
    want = None if not roots or roots[0]["byo"] else (at(roots[0]["ends_at"]) - datetime.timedelta(days=60)).strftime("%Y-%m-%dT%H:%M:%SZ")
    if ca["rotates_at"] != want:
        errors.append(f"egress: Canyon rotates by itself at {ca['rotates_at']}; 60 days before its own newest root's end is {want}, and never an admin's sub-CA")
    if (ca["signing"] is None) != (not roots) or ca["signing"] is not None and ca["signing"] not in {r["fingerprint"] for r in roots}:
        errors.append("egress: the signing root is one of the live roots, and none signs only while none is live")
    signing = {"the admin's read": ca["signing"], "the policy": policy.get("signing_root"), "the trust": doc["trust_state"]["signing_root"], "the leaf": leaf["root"]}
    if len(set(signing.values())) != 1:
        errors.append(f"egress: one root signs the current leaves, but {signing}")


# The refusals each new egress row names, byte for byte (contracts/routes.md, Egress control): deleting one
# from its row, or paraphrasing a body, fails here.
EGRESS_PERSON = '`403 {error:"person_required", message:"a person changes how apps call out"}`'
EGRESS_DASHBOARD = '`403 {error:"dashboard_required", message:"change it in Canyon\'s dashboard"}`'
EGRESS_EXTERNAL_LINE = "this Canyon hands its apps to your own platform, so their calls don't go through Canyon"
EGRESS_EXTERNAL = f'`409 {{error:"egress_external", message:"{EGRESS_EXTERNAL_LINE}"}}`'
EGRESS_TRUST_LINE = "delete the secret <name> first: it would replace the certificates Canyon needs to add this credential"
EGRESS_WAIT_LINES = ("the version running in <env> doesn't trust Canyon's certificate until its next deploy or promotion, so the secret stays where it is",
                     "ask an admin to turn on Through Canyon first: apps call out directly here, so Canyon can't add this credential yet")
EGRESS_EXIST_LINE = "<apps> have credentials that work only through Canyon. Their teams need to set those keys as secrets again and deploy, then remove the credentials."
EGRESS_EXIST_ONE = "<app> has credentials that work only through Canyon. Its team needs to set those keys as secrets again and deploy, then remove the credentials."
EGRESS_EXIST = f'`409 {{error:"credentials_exist", apps, message:"{EGRESS_EXIST_LINE}"}}` (`message:"{EGRESS_EXIST_ONE}"` with one app, **amended 2026-11-15**)'
EGRESS_UNCONSTRAINED_LINE = "Canyon takes only a sub-CA that is name-constrained to the destinations it adds credentials for, and that allows no IP addresses."
EGRESS_ROUTE_TRUST = '**`409 {error:"route_trust", message:"remove the app\'s credentials in <env> first: Canyon needs to set <name> itself for them"}`**'
EGRESS_ROWS = {
    "`GET /api/v1/egress/destinations`": ['`403 {error:"admin_required"}`'],
    "`PUT /api/v1/projects/:id/egress/:env/mode {mode}`": [
        "`403 team_required`", "`403 owner_required`", '`400 {error:"invalid_mode"}`', EGRESS_PERSON, EGRESS_DASHBOARD,
        '`409 {error:"looser_than_org", message:"only an admin can set an app\'s mode looser than the org\'s"}`',
        '`409 {error:"floor_not_enforced", message:"this app\'s calls do not all leave through Canyon\'s egress gateway yet"}`',
        EGRESS_EXTERNAL, "`409 held`"],
    "`PUT /api/v1/projects/:id/egress/:env/destinations/:destination` · `DELETE`": [
        "`403 team_required`", EGRESS_PERSON, EGRESS_DASHBOARD, '`400 {error:"invalid_destination"}`',
        '`409 {error:"platform_destination", message:"Canyon allows <destination> for every app already"}`', "`409 held`"],
    "`POST /api/v1/projects/:id/egress/:env/requests/:destination/dismiss`": [
        "`403 team_required`", EGRESS_PERSON, '`404 {error:"not_found"}`'],
    "`GET /api/v1/egress` · `PUT /api/v1/egress {via, mode, caps, credential_routes}`": [
        '`403 {error:"admin_required"}`', EGRESS_PERSON, EGRESS_DASHBOARD, '`400 {error:"invalid_mode"}`',
        '`409 {error:"egress_external", message:"the org runs its apps, so their calls do not leave through Canyon"}`',
        '`409 {error:"floor_not_enforced", message:"apps\' calls do not leave through Canyon\'s egress gateway while it is off"}`', EGRESS_EXIST],
    "`PUT /api/v1/projects/:id/egress/:env/routes/:destination {header, scheme, value, placeholder_env?}` · `DELETE`": [
        "`403 team_required`", "`403 owner_required`", EGRESS_PERSON, EGRESS_DASHBOARD, '`400 {error:"invalid_destination"}`',
        '`400 {error:"invalid_route", field}`', EGRESS_EXTERNAL,
        '`409 {error:"destination_not_allowed", message:"allow <destination> before you add a credential for it"}`', "`409 held`",
        f'`409 {{error:"trust_secret", message:"{EGRESS_TRUST_LINE}"}}`'],
    "`POST /api/v1/projects/:id/egress/:env/routes/:destination/from-secret {secret, header, scheme, placeholder_env?}`": [
        "`403 team_required`", "`403 owner_required`", EGRESS_PERSON, EGRESS_DASHBOARD, '`400 {error:"invalid_destination"}`',
        '`400 {error:"invalid_route", field}`', '`404 {error:"not_found"}`', '`409 {error:"provisioned"}`', "`409 egress_external`",
        "`409 destination_not_allowed`", "`409 held`", "`409 trust_secret`", '`409 {error:"would_wait", reason, message}`'],
    # in this order, the contract's (#2704): level, actor, session, body, then state
    "`POST /api/v1/egress/ca/rotate {}` · `{cert, key}`": [
        '`403 {error:"admin_required"}`', EGRESS_PERSON, EGRESS_DASHBOARD, '`400 {error:"invalid_ca", reason}`',
        f'`409 {{error:"ca_unconstrained", message:"{EGRESS_UNCONSTRAINED_LINE}"}}`', '`409 {error:"rotating", ends_at}`',
        '`409 {error:"ca_excludes", destinations}`'],
}
# Layer 3's refusals (contracts/routes.md, The check): each body a D5 string, each status other than 403 on its row.
EGRESS_INJECT = {"inject_requires_tls": None, "inject_unavailable": "502", "misdirected": "421", "upstream_failed": "502"}
# What Envoy and egress-control say to each other: each stated in contracts/routes.md, Interception, and named in design/EGRESS.md.
EGRESS_INTERCEPT = ("**`x-canyon-intercept`**", "**`x-canyon-caller`**", "**`canyon.caller`**", "**`context_extensions`**",
                    "namespace `envoy.filters.http.ext_authz`",
                    # what binds an inner request to the tunnel ext_authz #1 approved (#2140)
                    "**`canyon.destination`**", "**`x-canyon-destination`**")
# What the internal value fetch must say: deleting any one from its row fails here.
EGRESS_FETCH = ("`route_value`", "else `401`, no body", "`Cache-Control: no-store`", '`404 {error:"no_route"}`', '`400 {error:"invalid_request"}`',
                "no log line, error or audit row carries the value")
# What the internal leaf fetch must say (#2704): deleting any one from its row fails here.
EGRESS_LEAF = ("`leaf`", "else `401`, no body", "`Cache-Control: no-store`", '`400 {error:"invalid_request"}`', '`404 {error:"no_route"}`',
               '`503 {error:"no_signing_root"}`', "no log line, error or audit row carries a key", "its key never written anywhere",
               "excluding every IPv4 and IPv6 address", "one SAN, the destination", "to 24 hours after", "to 7 days after", "a path length of 0")
EGRESS_PLACEHOLDER = "canyon-adds-the-real-key"
# What the product pass pinned (#2690), each where it says it, whitespace aside: deleting one fails here.
EGRESS_PINS = {
    "the refusal bullet": ("is Envoy's own, with no `Canyon-Egress` header",),
    "Interception": ("`retry_on` `connect-failure` and `reset-before-request` alone",
                     # without its filter-state object a header is empty, and the empty value still overwrites (#2140's review)
                     "empty without one (`keep_empty_value: true`, so an empty value still overwrites",
                     "gives the `upstream_failed` body only to a `UF` or `DF` reply",
                     "a plain `CONNECT` that fails never reads a line about credentials",
                     # how Envoy tells a destination with a route (#2141)
                     "a request that is no `CONNECT` whose check's OK carried `destination`, its `metadata_filter`",
                     "there to a `UH` reply too, as its dynamic forward proxy's sub-clusters",
                     "ext_authz #1 adds it, alone, to the OK of the absolute-form",
                     # the inner access log (#2740)
                     "receiver under the log name `intercept`, with the receiver's token and `canyon.caller` alone of the tunnel's state",
                     "both compared canonically — lower-case, the one trailing dot dropped by the chain's connection manager before the check"),
    "the from-secret row": ("`placeholder_env` omitted is the secret's own name", "the app keeps the real key until its next deploy",
                            "unless the route would be live at once"),
    "executor.md's egress.ca": ("on every `/run` that carries `egress`, so a route works as soon as the policy names it",
                                "When `env` names any of the three, the executor sets none of the three, and the app's own values stand",
                                "The executor never refuses a `/run` over these",
                                "records them on the env's deploy row, `egress_roots`",
                                "the server records that run as carrying no root"),
    # The trust invariant and every ordering the review walked: deleting one fails here.
    "Interception, Trust": ("Canyon intercepts an env's routes only while the host's `credential_routes` is `true`", "and the root signing the current leaves",
                            "the old set intersected with the carried one, in the transaction that names the env, so it moves `seq`",
                            "the carried set alone only once the executor answers that the new version is up and the previous instance is gone — on Docker its `200`, on Kubernetes a `200` whose `previous_gone` is `true`",
                            "A failed, interrupted or stuck `/run`, or a retry that records nothing, leaves the intersection",
                            "nothing recorded, or a row that cannot be read, is the empty set: fail closed",
                            "a secret, then a route: the route's write is `409 trust_secret`",
                            "a route, then a secret: the secrets `PUT` is `409 route_trust`",
                            "two such writes at once, a route's and a secret's: each takes the app env's advisory lock before its check",
                            "a secret deleted, then a route added while the running version still carries its own CA",
                            "a version that predates #2143: its row records no roots",
                            "a promotion, a rollback, a platform patch, a network recreation: each is a `/run`",
                            "a rollout's overlap, old and new instances serving at once: the intersection holds until the old are gone",
                            "a failed Docker start, a Kubernetes rollout that never completes, or one whose previous pods outlive the bound, `previous_gone` `false`: the intersection stays",
                            "a retry that starts nothing: it records nothing, so the intersection stays",
                            "a root's rotation, or a switch to a BYO sub-CA: leaves stay signed by the old root until no env holding a route lacks the new one",
                            "via to `off`, or `run_by` to `org`: refused while any route exists, `409 credentials_exist`",
                            "a from-secret whose route would wait: `409 would_wait`",
                            # who holds the old root back, and seq at a root's end (#2704)
                            "an env holding a route counts only while Canyon runs it, its deploy row's `run_by` `canyon`",
                            "one with no deploy row, or one the org runs, has no instance Canyon intercepts, so it holds no root back",
                            "a root passing its end changes the signing root with no row written: the policy's next answer moves `seq` first"),
    # what a sub-CA must be (#2717's review): an empty subtree matches every name, an open address space every address
    "the rotate row": ("permit at least one DNS subtree, none of them empty, a leading dot aside",
                       "permitted IP ranges, none of them a `/0`, or excluded ranges covering all of IPv4 (`0.0.0.0/0`) and IPv6 (`::/0`)",
                       "`verify`, a leaf for a name it permits",
                       "whether or not a route names a destination"),
    "the policy row": ("names its routes only while the signing root is among the env's trusted roots, else `[]`",
                       "an answer whose read finds another moves `seq` first"),
    "the PUT row": ("checked and written under the app env's advisory lock",),
    # the inner access log (#2740): who an inner entry is, its outcome, and the token egress-control holds
    "the receiver row": ("of an `intercept` entry is its `canyon.caller` alone, `<app>/<env>`",
                         "an `intercept` entry's `allowed` only when ext_authz #2's `outcome` says so, else `refused`",
                         "without a valid one it serves no interception listener, so a call to a destination with a route finds none and is `503`",
                         "never in a log line, an error, a metric or an SDS answer",
                         # #2718: the row's calls are every call
                         "`calls` added, every call whatever its outcome and reason, which an egress request's `refusals` is not"),
    # #2691: the caps, an admin's, never one that means none
    "the setting row": ("each a whole number in its range — `destinations` 50 to 5000, `new_names` 1 to 600, `pending` 1 to 500 — nothing moved: never a value that means no cap",
                        "a member's read names none"),
    # #2718: what a request's refusals count, beside the row's calls
    "the requests bullet": ("`refusals` counts the `not_allowed` calls alone, the ones an allowlist entry would let through",
                            "is a call of the row and no refusal of the request"),
}
# What the executor sets with egress (contracts/executor.md, Egress), which no placeholder may name; egress.ca's are EGRESS_TRUST.
EGRESS_PROXY_VARS = ("HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy", "NO_PROXY", "no_proxy", "NODE_USE_ENV_PROXY", "JDK_JAVA_OPTIONS")
# What the secrets row refuses on every host, which no placeholder may name either.
EGRESS_SECRET_REFUSED = ("PORT", "PATH", "LD_PRELOAD", "LD_LIBRARY_PATH", "CANYON_IDENTITY_ISSUER", "CNB_APP_DIR")
# What a route's header is never, in its stored lower-case form (contracts/routes.md, the routes row).
EGRESS_HEADERS_REFUSED = ("host", "content-length", "transfer-encoding", "connection", "upgrade", "proxy-authorization", "x-canyon-intercept",
                          "x-canyon-caller", "x-envoy-original-path", "canyon-egress")


def egress_rows():
    """Each new egress row of routes.md names its refusals byte for byte, and The check's own rows carry
    design/EGRESS.md §8.4's bodies byte for byte — a copy anywhere else in routes.md satisfies nothing."""
    routes = (CONTRACTS / "routes.md").read_text()
    _, section = routes.split("\n## Egress control\n", 1)
    section, _ = section.split("\n## Access\n", 1)
    rows = {line.split(" | ", 1)[0][2:]: line for line in section.splitlines() if line.startswith("| `")}
    for route, refusals in EGRESS_ROWS.items():
        for refusal in refusals:
            if refusal not in rows.get(route, ""):
                errors.append(f"egress: routes.md's row {route} does not name {refusal}")
    _, check = section.split("\n### The check\n", 1)
    bodies = dict(re.findall(r"^\| `([a-z_]+)` \| `([^`]+)`", check, re.M))
    _, proposed = (ROOT / "design" / "EGRESS.md").read_text().split("### 8.4 Proposed strings")
    refused, unanswered, unknown = re.findall(r"^- `([^`]+)`", proposed.split("\n## ")[0], re.M)[:3]
    for reason, line in (("not_allowed", refused), ("unknown_caller", unknown)):
        if bodies.get(reason) != line:
            errors.append(f"egress: The check's {reason} body is {bodies.get(reason)!r}; design/EGRESS.md §8.4 says {line!r}")
    if f"no answer is\n`503` with the body `{unanswered}`" not in check:
        errors.append(f"egress: The check's 503 body is not design/EGRESS.md §8.4's {unanswered!r}")
    experience = (ROOT / "design" / "EXPERIENCE.md").read_text()
    for reason, status in EGRESS_INJECT.items():
        body = bodies.get(reason)
        if body is None or f"`{body}`" not in experience:
            errors.append(f"egress: The check's {reason} body is {body!r}, which is no D5 string of design/EXPERIENCE.md")
        if status and f"| `{reason}` | `{body}` — status `{status}`" not in check:
            errors.append(f"egress: The check's {reason} row does not name its status {status}")
    _, intercept = check.split("\n### Interception\n", 1)
    design = (ROOT / "design" / "EGRESS.md").read_text()
    for name in EGRESS_INTERCEPT:
        if name not in intercept or name.split("`")[1] not in design:
            errors.append(f"egress: Interception and design/EGRESS.md do not both name {name}")
    fetch = next((line for line in section.splitlines() if line.startswith("| internal: `GET /egress/route-value?")), "")
    for words in EGRESS_FETCH:
        if words not in fetch:
            errors.append(f"egress: routes.md's value fetch does not say {words}")
    fetch = next((line for line in section.splitlines() if line.startswith("| internal: `GET /egress/leaf?")), "")
    for words in EGRESS_LEAF:
        if words not in fetch:
            errors.append(f"egress: routes.md's leaf fetch does not say {words}")
    timeout = re.search(r"Envoy asks with `failure_mode_allow: false` and a ([0-9.]+) s timeout", check)
    bound = re.search(r"none past ([0-9.]+) s from when its check began, a refetch of the caller\s+included", intercept)
    if not timeout or not bound or float(bound.group(1)) >= float(timeout.group(1)):
        errors.append("egress: a value's ask, the caller's refetch included, ends inside Envoy's timeout, so the app reads inject_unavailable, never the 503")
    if "only on `UF` or `DF` and only for a destination with a route" not in next((line for line in check.splitlines() if line.startswith("| `upstream_failed` |")), ""):
        errors.append("egress: upstream_failed is Envoy's reply only on UF or DF for a destination with a route; past them \"did not send\" may be untrue")
    def flat(text):
        return " ".join(text.split())
    refusal = next(b for b in check.split("\n- ") if b.startswith("**A refusal**"))
    requests = next((b for b in check.split("\n- ") if b.startswith("**Egress requests**")), "")
    from_secret = next((line for line in section.splitlines() if line.startswith("| `POST /api/v1/projects/:id/egress/:env/routes/:destination/from-secret")), "")
    executor = (CONTRACTS / "executor.md").read_text()
    ca = executor[executor.index("**`egress.ca`**"):]
    ca = ca[:ca.index("\n\n")]
    policy_row = next((line for line in section.splitlines() if line.startswith("| internal: `GET /egress/policy?")), "")
    receiver_row = next((line for line in section.splitlines() if line.startswith("| internal: Envoy's access-log receiver, gRPC")), "")
    for where, text in (("the refusal bullet", refusal), ("Interception", intercept), ("the from-secret row", from_secret), ("executor.md's egress.ca", ca),
                        ("Interception, Trust", intercept), ("the policy row", policy_row), ("the receiver row", receiver_row),
                        ("the requests bullet", requests), ("the setting row", rows.get("`GET /api/v1/egress` · `PUT /api/v1/egress {via, mode, caps, credential_routes}`", "")),
                        ("the rotate row", rows.get("`POST /api/v1/egress/ca/rotate {}` · `{cert, key}`", "")),
                        ("the PUT row", rows.get("`PUT /api/v1/projects/:id/egress/:env/routes/:destination {header, scheme, value, placeholder_env?}` · `DELETE`", ""))):
        for pin in EGRESS_PINS[where]:
            if pin not in flat(text):
                errors.append(f"egress: {where} does not say {pin!r}")
    rotate = rows.get("`POST /api/v1/egress/ca/rotate {}` · `{cert, key}`", "")
    order = [rotate.find(r) for r in EGRESS_ROWS["`POST /api/v1/egress/ca/rotate {}` · `{cert, key}`"]]
    if -1 in order or order != sorted(order):
        errors.append("egress: the rotation row answers its refusals in the contract's order: level, actor, session, body, then state")
    if f"| `{EGRESS_UNCONSTRAINED_LINE}` |" not in experience:
        errors.append(f"egress: {EGRESS_UNCONSTRAINED_LINE!r} is no D5 string of design/EXPERIENCE.md")
    for route, row in rows.items():
        words = ("`409 held`", "destination_not_allowed", "trust_secret") + (("would_wait",) if "from-secret" in route else ())
        at = [row.find(word) for word in words]
        if "routes/:destination" in route and (-1 in at or at != sorted(at)):
            errors.append(f"egress: {route} answers {', '.join(words)}, in that order")
    secrets_put = next((line for line in routes.splitlines() if line.startswith("| `PUT /api/v1/projects/:id/secrets/:name` |")), "")
    if EGRESS_ROUTE_TRUST not in secrets_put or "checked and written under the app env's advisory lock" not in secrets_put:
        errors.append(f"egress: the secrets PUT does not name {EGRESS_ROUTE_TRUST} under the app env's advisory lock, so a secret set after a route could end its trust unseen")
    for line in EGRESS_WAIT_LINES:
        if f"`{line}`" not in from_secret:
            errors.append(f"egress: the from-secret row does not answer would_wait with D5's {line!r}")
    preferences = next((line for line in routes.splitlines() if line.startswith("| `GET /api/v1/preferences` · `PUT /api/v1/preferences`")), "")
    if EGRESS_EXIST not in preferences:
        errors.append(f"egress: a PUT of run_by org does not answer {EGRESS_EXIST} while routes exist, so a handed-off app could keep a credential nothing adds")
    for line in (EGRESS_EXTERNAL_LINE, EGRESS_TRUST_LINE, EGRESS_ROUTE_TRUST.split('message:"')[1].split('"}')[0]) + EGRESS_WAIT_LINES:
        if f"| `{line}` |" not in experience and not (line in EGRESS_WAIT_LINES and f"`{line}`" in experience):
            errors.append(f"egress: {line!r} is no D5 string of design/EXPERIENCE.md")
    if f"| `{EGRESS_EXIST_ONE}` / `{EGRESS_EXIST_LINE}` |" not in experience:
        errors.append("egress: credentials_exist's one-app and many-app lines are no D5 string of design/EXPERIENCE.md")
    if f"`{EGRESS_PLACEHOLDER}`" not in section or f"| `{EGRESS_PLACEHOLDER}` |" not in experience:
        errors.append(f"egress: the placeholder {EGRESS_PLACEHOLDER} is the routes row's and a D5 string, byte for byte")


def app_address(app, env, domain, form):
    """routes.md, the front-door row: an app's address on a Kubernetes host, by the form."""
    return f"https://{app}.{env}.{domain}" if form == "app.env.domain" else f"https://{app}-{env}.{domain}"


def seam3_front_door():
    """GET and PUT /api/v1/front-door, routes.md (Platform). What the schema cannot say: the answer is
    the body saved, its secret aside and its domain defaulted; a refusal is the probe's own answer, its
    message the probe's fix naming the wildcards the form needs; and a deploy's address follows the form."""
    doc = load(CONTRACTS / "testdata" / "front-door" / "meridian.json")
    schema = CONTRACTS / "schema" / "front-door.schema.json"
    if not schema_check("front-door", schema, doc):
        return
    put, get, probe, refusal = doc["put"], doc["get"], doc["probe_result"], doc["refusal"]
    door = get["front_door"]
    # 1. The answer is the body, the secret never in it and the domain the default when the body names none.
    saved = {**put, "domain": put.get("domain", get["default_domain"]), "set_by": door["set_by"], "set_at": door["set_at"], "probed": True, "enforcing": True}
    if "signin" in put:
        saved["signin"] = {k: v for k, v in put["signin"].items() if k != "client_secret"}
    if door != saved:
        errors.append(f"front-door: the GET after a PUT answers the body saved, secret aside and domain defaulted: {saved}, got {door}")
    # 2. A refusal is the probe's answer: its fix verbatim, only when the probe did not pass, naming the form's wildcards.
    if (refusal["network_policy"], refusal["certificate"], refusal["message"]) != (probe["network_policy"], probe["certificate"], probe["fix"]) \
            or (probe["network_policy"], probe["certificate"]) == ("enforced", "ok"):
        errors.append("front-door: probe_failed carries the probe's own answer and its fix verbatim, and only when it did not pass")
    names = f"*.preview.{door['domain']} and *.prod.{door['domain']}" if door["form"] == "app.env.domain" else f"*.{door['domain']}"
    if probe["certificate"] == "missing" and f" {names} " not in refusal["message"]:
        errors.append(f"front-door: a certificate refusal names what the form needs, {names}")
    # 2b. grants_note (routes.md, the front-door row; #2299): each non-empty value is a D5 string of EXPERIENCE.md, byte for
    #     byte, and the golden's doors answer it by the rule — "" with sign-in off and on Canyon's issuer, the alb line for
    #     every alb, the other-issuer line when the door's issuer differs, compared exactly, as the sidecar compares a token's.
    blank, other, alb = load(schema)["definitions"]["get"]["properties"]["grants_note"]["enum"]
    experience = (ROOT / "design" / "EXPERIENCE.md").read_text()
    for line in (other, alb):
        if f"| `{line}` |" not in experience:
            errors.append(f"front-door: grants_note {line!r} is not a D5 string of design/EXPERIENCE.md")
    notes = load(CONTRACTS / "testdata" / "front-door" / "grants-notes.json")
    for i, case in enumerate(notes["cases"]):
        definition_check(f"front-door: grants-notes case {i}", schema, "put", case["door"])
        d, issuer = case["door"], notes["signin_issuer"]
        want = blank if not issuer else alb if d.get("gateway") == "alb" else other if (d["signin"]["issuer"] if d["choice"] == 2 else d["issuer"]) != issuer else blank
        if case["grants_note"] != want:
            errors.append(f"front-door: grants-notes case {i} answers {case['grants_note']!r}, the rule {want!r}")
    if {c["grants_note"] for c in notes["cases"]} != {blank, other, alb}:
        errors.append("front-door: grants-notes has a door for each of grants_note's values")
    # 2c. egress_note (routes.md, the front-door row; #2316): its one line is a D5 string of EXPERIENCE.md, byte for byte.
    for line in load(schema)["definitions"]["get"]["properties"]["egress_note"]["enum"][1:]:
        if f"| `{line}` |" not in experience:
            errors.append(f"front-door: egress_note {line!r} is not a D5 string of design/EXPERIENCE.md")
    # 2d. probe_note (routes.md, the front-door row; #2438, #2507): D5's not-enforcing line, then why, while the door is
    #     not enforcing; else D5's not-checked line, then why, while it is not probed; "" while it is both. The golden's
    #     unprobed door is a choice 1 its probe found not enforcing, and its why that probe's fix.
    unprobed = doc["get_unprobed"]
    line = "Not checked on this cluster: your platform can run apps behind this front door, but Canyon won't until a save passes the check."
    unenforced = "Not checked on this cluster: Canyon builds nothing here until a save finds NetworkPolicy enforced, so no deploy goes through, whoever runs the apps."
    for d5 in (line, unenforced):
        if f"| `{d5} <why>` |" not in experience:
            errors.append(f"front-door: probe_note's line {d5!r} is not a D5 string of design/EXPERIENCE.md")
    if get["front_door"]["probed"] is not True or get["front_door"]["enforcing"] is not True or get["probe_note"] != "":
        errors.append("front-door: a door that passed the probe answers probed and enforcing true and no probe_note")
    if unprobed["front_door"]["probed"] is not False or unprobed["front_door"]["enforcing"] is not False \
            or not unprobed["probe_note"].startswith(unenforced + " ") or unprobed["front_door"]["choice"] != 1:
        errors.append("front-door: an unprobed door not enforcing is a choice 1 whose probe_note is D5's not-enforcing line and why")
    # 3. A deploy's address follows the form.
    a = doc["app_url"]
    if a["url"] != app_address(a["app"], a["env"], door["domain"], door["form"]):
        errors.append(f"front-door: {a['app']} in {a['env']} is at {app_address(a['app'], a['env'], door['domain'], door['form'])} under this front door, got {a['url']}")
    # 4. One shape with /run's front_door (executor.md, On Kubernetes), the secret aside: each field the PUT
    #    takes is the one /run takes, the choices' rules are /run's, and the stored door with a secret back
    #    is one a Kubernetes /run carries.
    ex_path, fd = CONTRACTS / "schema" / "executor.schema.json", load(schema)
    ex = load(ex_path)
    run_door = ex["definitions"]["front_door"]
    resolve = lambda spec, defs: defs[spec["$ref"].split("/")[-1]] if "$ref" in spec else spec
    for key, spec in fd["definitions"]["put"]["properties"].items():
        if key != "signin" and resolve(spec, fd["definitions"]) != resolve(run_door["properties"].get(key, {}), ex["definitions"]):
            errors.append(f"front-door: the PUT's {key} is not /run's front_door {key} (executor.schema.json)")
    if fd["definitions"]["choice_rules"]["oneOf"] != run_door["oneOf"]:
        errors.append("front-door: the choices' rules are not /run's front_door rules (executor.schema.json)")
    stored = {k: v for k, v in door.items() if k not in ("set_by", "set_at", "probed", "enforcing")}
    if "signin" in stored:
        stored["signin"] = {**stored["signin"], "client_secret": "redacted"}
    exdoc = load(CONTRACTS / "testdata" / "executor" / "claims-portal.json")
    if not accepts(ex_path, {**exdoc, "kube_run_request": {**exdoc["kube_run_request"], "front_door": stored}}):
        errors.append("front-door: the stored front door, its secret put back, is not one a Kubernetes /run carries")
    # 5. The schema refuses what the row refuses.
    alb = {"choice": 1, "gateway": "alb", "alb_arn": "arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/canyon/0123456789abcdef", "form": "app-env.domain"}
    for name, key, value, ok in (
            ("choice 1 behind an ALB, the app-env form", "put", alb, True),
            ("an OIDC gateway naming its session cookie", "put", {"choice": 1, "gateway": "oidc", "issuer": "https://login.meridian.example", "audience": "canyon-gateway", "jwks_url": "https://login.meridian.example/keys", "cookie": "_pomerium", "form": "app.env.domain"}, True),
            ("an OIDC gateway without its session cookie", "put", {"choice": 1, "gateway": "oidc", "issuer": "https://login.meridian.example", "audience": "canyon-gateway", "jwks_url": "https://login.meridian.example/keys", "form": "app.env.domain"}, False),
            ("a form outside the two", "put", {**put, "form": "app.domain"}, False),
            ("a domain whose first label is prod", "put", {**put, "domain": "prod.meridian.example"}, False),
            ("an ALB gateway with a sign-in client", "put", {**alb, "signin": put["signin"]}, False),
            ("the client's secret in the answer", "get", {**get, "front_door": {**door, "signin": {**door["signin"], "client_secret": "redacted"}}}, False),
            ("a front door on a Docker host", "get", {**get, "runtime": "docker"}, False),
            ("a stored door without probed", "get", {**get, "front_door": {k: v for k, v in door.items() if k != "probed"}}, False),
            ("a stored door without enforcing", "get", {**get, "front_door": {k: v for k, v in door.items() if k != "enforcing"}}, False),
            ("choice 2 a NetworkPolicy refusal left unprobed", "get", {**get, "front_door": {**door, "probed": False, "enforcing": False}}, True),
            ("a probe_note that is not D5's line", "get_unprobed", {**doc["get_unprobed"], "probe_note": "Unchecked."}, False),
            ("D5's line alone, a door stored before the field", "get_unprobed", {**doc["get_unprobed"], "probe_note": line}, True),
            ("D5's not-enforcing line alone", "get_unprobed", {**doc["get_unprobed"], "probe_note": unenforced}, True)):
        if accepts(schema, {**doc, key: value}) != ok:
            errors.append(f"selftest: a front-door {key} with {name} was {'refused' if ok else 'accepted'}")


# ---------------------------------------------------------------- seam 4 — the executor calls
def unmasked(env, output):
    """The secret values executor.md's /run masks that the output still carries, by name: each env value,
    and the password of one that is a URL with userinfo, decoded."""
    found = []
    for name, value in env.items():
        if value and value in output:
            found.append(name)
        password = urllib.parse.urlsplit(value).password if "://" in value else None
        if password and password in output:
            found.append(f"{name} password")
    return found


def mask(env, text):
    """executor.md's /run rule, which the server applies to POST /running's `said` (routes.md, envs.<env>.stopped):
    each non-empty value, and each line of a multi-line one, as <NAME>, and a URL value's userinfo password as
    <NAME password>, the longest first, a tie by name."""
    values = []
    for name, value in env.items():
        values += [(t, name) for t in {value, *value.split("\n"), *(p.rstrip("\r") for p in value.split("\n"))} if t]
        password = urllib.parse.urlsplit(value).password if "://" in value else None
        if password:
            values.append((urllib.parse.unquote(password), f"{name} password"))
    for t, name in sorted(values, key=lambda v: (-len(v[0]), v[1])):
        text = text.replace(t, f"<{name}>")
    return text


def hides_text(text):
    """The characters a description refuses and the server removes from a stopped line: control, format,
    line and paragraph separators (Unicode Cc, Cf, Zl, Zp) — an escape, a bidi override."""
    return [c for c in text if unicodedata.category(c) in ("Cc", "Cf", "Zl", "Zp")]


def seam4_rollback():
    """A rollback review's status read (routes.md, the status row's ?run= and earlier[]; The promote receipt;
    design/OPERATIONS.md §1): production's earlier versions are per (package, image), newest first, never
    what production runs now, at most five, each saying whether its image was kept; the receipt ships an
    earlier version's kept image, and no other receipt carries ships."""
    ap_path = CONTRACTS / "schema" / "app-page.schema.json"
    st = load(CONTRACTS / "testdata" / "app-page" / "claims-portal-rollback-status.json")
    ap = load(CONTRACTS / "testdata" / "app-page" / "claims-portal.json")
    if not definition_check("app-page: claims-portal rollback status", ap_path, "status", st) or ap is None:
        return
    prod, earlier = st["envs"]["prod"], st["earlier"]
    if not earlier:
        errors.append("app-page: the rollback status carries no earlier[] to put back")
        return
    pairs = [(e["package"], e["image_digest"]) for e in earlier]
    if len(set(pairs)) != len(pairs):
        errors.append(f"app-page: earlier[] names a (package, image) twice: {pairs}; each is its newest row once")
    if (prod["package"], prod["image_digest"]) in pairs:
        errors.append("app-page: earlier[] lists what production runs now; it is left out")
    if [e["deployed_at"] for e in earlier] != sorted((e["deployed_at"] for e in earlier), reverse=True):
        errors.append("app-page: earlier[] is not newest first")
    ships, run = st["receipt"].get("ships"), st["promote_run"]
    if ships is None or not any(e["version"] == ships["version"] and e["image_digest"] == ships["image_digest"] and e["kept"] for e in earlier):
        errors.append(f"app-page: the rollback receipt ships {ships}; it is an earlier version's kept image")
    if run is None or st["last_run"] != run:
        errors.append("app-page: a status read ?run= reads the receipt for that run, the promote_run it names")
    row = earlier[0]
    undeployed = {**st, "envs": {k: v for k, v in st["envs"].items() if k != "prod"}}
    for name, probe, ok in (
            ("the rollback status", st, True),
            ("earlier versions with nothing in production", undeployed, True),
            ("an earlier version that says nothing of its image kept", {**st, "earlier": [{k: v for k, v in row.items() if k != "kept"}]}, False),
            ("six earlier versions", {**st, "earlier": [row] * 6}, False),
            ("an image marked not patched", {**st, "earlier": [{**row, "patched": False}]}, False),
            ("a status read without earlier[]", {k: v for k, v in st.items() if k != "earlier"}, False),
            ("production's env carrying earlier[]", {**st, "envs": {**st["envs"], "prod": {**prod, "earlier": earlier}}}, False),
            ("a receipt waiting on preview that ships", {**st, "receipt": {**st["receipt"], "rules": [], "blocked_by": [], "waiting": "Deploy a new version to preview to promote it."}}, False),
            ("a receipt shipping a tag", {**st, "receipt": {**st["receipt"], "ships": {"version": "v1.3", "image_digest": "latest"}}}, False)):
        if accepts(ap_path, {**ap, "status": probe}) != ok:
            errors.append(f"selftest: {name} was {'refused' if ok else 'accepted'}")

def seam4_stopped():
    """Why a stopped environment stopped (routes.md, the status row; executor.md, POST /running): the status
    read's envs.<env>.stopped is the executor's raw last line masked by /run's rule with the values the app
    was handed — the /run fixture's stand in for prod's — cut to 200 characters; on a stopped environment of
    the status read alone, never on the fleet."""
    ex = load(CONTRACTS / "testdata" / "executor" / "claims-portal.json")
    st = load(CONTRACTS / "testdata" / "app-page" / "claims-portal-prod-stopped-status.json")
    ap_path, ap = CONTRACTS / "schema" / "app-page.schema.json", load(CONTRACTS / "testdata" / "app-page" / "claims-portal.json")
    if not definition_check("app-page: claims-portal stopped status", ap_path, "status", st) or ex is None or ap is None:
        return
    raw = {(s["app"], s["target"]): s for s in ex["running_batch_result"]["stopped"]}
    for env, e in st["envs"].items():
        said = raw.get((st["project"], env))
        if "stopped" not in e:
            continue
        if said is None or e["stopped"] != {"exit_code": said["exit_code"], "said": mask(ex["run_request"]["env"], said["said"])[:200]}:
            errors.append(f"app-page: envs.{env}.stopped is {e['stopped']}; it is the executor's exit code and line, masked by /run's rule and cut to 200")
        if hides_text(e["stopped"]["said"]):
            errors.append(f"app-page: envs.{env}.stopped carries {hides_text(e['stopped']['said'])}, which the server removes")
        for name in unmasked(ex["run_request"]["env"], e["stopped"]["said"]):
            errors.append(f"app-page: envs.{env}.stopped carries the value of {name}; it carries <{name}>")
    prod = st["envs"]["prod"]
    fl_path, fl = CONTRACTS / "schema" / "fleet.schema.json", load(CONTRACTS / "testdata" / "fleet" / "page.json")
    row = next(a for a in fl["apps"] if "prod" in a["envs"] and "url" in a["envs"]["prod"])
    for name, path, probe, ok in (
            ("a stopped environment saying why", ap_path, {**ap, "status": st}, True),
            ("a running environment saying why it stopped", ap_path, {**ap, "status": {**st, "envs": {**st["envs"], "prod": {**prod, "state": "running"}}}}, False),
            ("a line of 201 characters", ap_path, {**ap, "status": {**st, "envs": {**st["envs"], "prod": {**prod, "stopped": {"exit_code": 1, "said": "x" * 201}}}}}, False),
            ("a line carrying a newline", ap_path, {**ap, "status": {**st, "envs": {**st["envs"], "prod": {**prod, "stopped": {"exit_code": 1, "said": "a\nb"}}}}}, False),
            ("the fleet row saying why", fl_path, {**fl, "apps": [{**a, "envs": {**a["envs"], "prod": {**a["envs"]["prod"], "state": "stopped", "stopped": prod["stopped"]}}} if a is row else a for a in fl["apps"]]}, False),
            ("an executor line over 4 KiB", CONTRACTS / "schema" / "executor.schema.json", {**ex, "running_batch_result": {**ex["running_batch_result"], "stopped": [{"app": "claims-portal", "target": "prod", "exit_code": 1, "said": "x" * 4097}]}}, False),
            ("an environment saying what it answered", ap_path, {**ap, "status": {**st, "envs": {**st["envs"], "prod": {k: v for k, v in {**prod, "state": "running", "answered": {"status": 400, "said": "<h1>Bad Request (400)</h1>"}}.items() if k != "stopped"}}}}, True),
            ("an environment nothing answered", ap_path, {**ap, "status": {**st, "envs": {**st["envs"], "prod": {k: v for k, v in {**prod, "state": "running", "answered": {"error": "no answer in 5 s"}}.items() if k != "stopped"}}}}, True),
            ("a status no HTTP answer has", ap_path, {**ap, "status": {**st, "envs": {**st["envs"], "prod": {k: v for k, v in {**prod, "state": "running", "answered": {"status": 600, "said": ""}}.items() if k != "stopped"}}}}, False),
            ("the fleet row saying what it answered", fl_path, {**fl, "apps": [{**a, "envs": {**a["envs"], "prod": {**a["envs"]["prod"], "answered": {"status": 400, "said": ""}}}} if a is row else a for a in fl["apps"]]}, False),
            ("a restarting environment saying how often", ap_path, {**ap, "status": {**st, "envs": {**st["envs"], "prod": {k: v for k, v in {**prod, "state": "restarting", "restarting": {"restarts": 7}}.items() if k != "stopped"}}}}, True),
            ("a running environment saying it restarts", ap_path, {**ap, "status": {**st, "envs": {**st["envs"], "prod": {k: v for k, v in {**prod, "state": "running", "restarting": {"restarts": 1}}.items() if k != "stopped"}}}}, False),
            ("a restart count of 0", ap_path, {**ap, "status": {**st, "envs": {**st["envs"], "prod": {k: v for k, v in {**prod, "state": "restarting", "restarting": {"restarts": 0}}.items() if k != "stopped"}}}}, False),
            ("the fleet row saying how often it restarts", fl_path, {**fl, "apps": [{**a, "envs": {**a["envs"], "prod": {**a["envs"]["prod"], "state": "restarting", "restarting": {"restarts": 1}}}} if a is row else a for a in fl["apps"]]}, False),
            ("an executor restart count of 0", CONTRACTS / "schema" / "executor.schema.json", {**ex, "running_batch_result": {**ex["running_batch_result"], "restarting": [{"app": "case-dies-later", "target": "preview", "restarts": 0}]}}, False)):
        if accepts(path, probe) != ok:
            errors.append(f"selftest: {name} was {'refused' if ok else 'accepted'}")
    shape = lambda d: {k: v for k, v in d.items() if k != "description"}  # each schema compiles on its own, so each carries the shape
    for copy in ("timeline-row", "executor"):
        if shape(load(CONTRACTS / "schema" / f"{copy}.schema.json")["definitions"]["answered"]) != shape(load(ap_path)["definitions"]["env"]["properties"]["answered"]):
            errors.append(f"contracts: {copy}'s answered is app-page's env.answered, the same shape")
    rows = load(CONTRACTS / "testdata" / "timeline" / "claims-portal-run.json")
    done = next(r for r in (rows["rows"] if isinstance(rows, dict) else rows) if r["kind"] == "deploy.completed")
    for name, answered, ok in (("a deploy.completed row saying what the app answered", {"status": 400, "said": "Bad Request (400)"}, True),
                               ("a deploy.completed row with a status no HTTP answer has", {"status": 99, "said": ""}, False)):
        if accepts(CONTRACTS / "schema" / "timeline-row.schema.json", {**done, "data": {**done["data"], "answered": answered}}) != ok:
            errors.append(f"selftest: {name} was {'refused' if ok else 'accepted'}")
    if mask({"DATABASE_URL": "postgres://a:pw%2Fx@db/a", "K": "pw"}, "pw/x pw") != "<DATABASE_URL password> <K>":
        errors.append("selftest: the mask names a URL's decoded password before a shorter value")
    if mask({"CERT": "line-one-91c2\r\nline-two"}, "read line-one-91c2 and line-two") != "read <CERT> and <CERT>":
        errors.append("selftest: the mask names a line of a CRLF value without its carriage return")
    if hides_text("ok \u202e\x1b[31m") != ["\u202e", "\x1b"]:
        errors.append("selftest: a bidi override and an escape are not read as hiding text")


# design/EGRESS.md §4.4: what the executor sets beside egress.ca, byte for byte in executor.md.
EGRESS_TRUST = ("SSL_CERT_DIR=/etc/ssl/certs:/etc/canyon-egress", "NODE_EXTRA_CA_CERTS=/etc/canyon-egress/bundle.pem",
                "REQUESTS_CA_BUNDLE=/etc/canyon-egress/system-and-canyon.pem")


def seam4_egress_ca():
    """/run's egress.ca, contracts/executor.md. What the schema cannot say: a root's name is its own, once; and
    the trust variables are executor.md's byte for byte, the first design/EGRESS.md §4.4's too. The probes: what
    no root is. The fixture holds it apart from run_request, as neither executor reads it before #2143."""
    path = CONTRACTS / "schema" / "executor.schema.json"
    doc = load(CONTRACTS / "testdata" / "executor" / "claims-portal.json")
    egress, roots = doc["egress_with_ca"], doc["egress_with_ca"]["ca"]
    root = roots[0]
    if not accepts(path, doc):
        errors.append("selftest: executor: the fixture was refused, so the probes below prove nothing")
    for name, ca in (("no roots", []),
                     ("nine roots", [{**root, "name": f"c959b7c8.{i % 10}"} for i in range(9)]),
                     ("a root under a name Paketo's loader never reads", [{**root, "name": "canyon.pem"}]),
                     ("a root whose name ends in a line break", [{**root, "name": root["name"] + "\n"}]),
                     ("a root with no name", [{"pem": root["pem"]}]),
                     ("a private key for a root", [{**root, "pem": root["pem"].replace("CERTIFICATE", "PRIVATE KEY")}]),
                     ("two certificates in one root", [{**root, "pem": root["pem"] * 2}]),
                     ("a root past 8 KiB", [{**root, "pem": root["pem"].replace("\n", "\n" + "QUJD\n" * 2000, 1)}])):
        if accepts(path, {**doc, "egress_with_ca": {**egress, "ca": ca}}):
            errors.append(f"selftest: executor: egress.ca with {name} was accepted")
    names = [r["name"] for r in roots]
    if len(set(names)) != len(names):
        errors.append(f"executor: egress.ca names a root twice: {names}")
    executor = (CONTRACTS / "executor.md").read_text()
    for line in EGRESS_TRUST:
        if f"`{line}`" not in executor:
            errors.append(f"executor: executor.md's egress.ca does not set {line}")
    # The narrowing waits on the executor's word that no earlier instance serves (executor.md, POST /run and On Kubernetes).
    flat = " ".join(executor.split())
    for pin in ("A `/run` answers `200` only once the previous container is removed",
                "once no pod of an earlier template remains, terminating ones included",
                "The server moves an env's trusted roots to the carried set only on `previous_gone` `true`",
                "or an answer without it, leaves the intersection"):
        if pin not in flat:
            errors.append(f"executor: executor.md does not say {pin!r}")
    if doc["kube_run_result"].get("previous_gone") is not True or "previous_gone" in doc["run_result"]:
        errors.append("executor: a Kubernetes /run answers previous_gone, a Docker /run none, as its 200 comes after the container is removed")
    if accepts(path, {**doc, "kube_run_result": {**doc["kube_run_result"], "previous_gone": "yes"}}):
        errors.append("selftest: executor: a previous_gone that is no boolean was accepted")
    if f"`{EGRESS_TRUST[0]}`" not in (ROOT / "design" / "EGRESS.md").read_text():
        errors.append(f"executor: design/EGRESS.md §4.4 does not set {EGRESS_TRUST[0]}")
    if "in `system-and-canyon.pem` the system roots of the executor's own image" not in " ".join(executor.split()):
        errors.append("executor: REQUESTS_CA_BUNDLE's file holds the system roots as well as Canyon's, as requests reads it in place of its own")


def seam4():
    """The ten executor shapes, then the invariants executor.md states that a fixture can carry:
    /run re-uses the built image by digest (invariant 4), the env never carries PORT, the
    container is <app>-<target>, it joins its app's own network (invariant 11) with a token minted
    for that app, what it keeps is digests and never a tag, and /stop, /forget and PUT /access name that same app. Invariant 1 (only where: build values reach a build) is a
    control-plane test — a fixture has no where."""
    doc = load(CONTRACTS / "testdata" / "executor" / "claims-portal.json")
    if not schema_check("executor", CONTRACTS / "schema" / "executor.schema.json", doc):
        return
    run, res = doc["run_request"], doc["run_result"]
    if run["image"] != doc["build_result"]["image"]:
        errors.append("executor: /run must re-use the image /build returned, by digest")
    if "PORT" in run["env"]:
        errors.append("executor: PORT is the executor's, never in the env")
    if "answered" in res:
        errors.append("executor: a Docker /run answers no answered; the server asks the app itself")
    if res["container"] != f"{run['app']}-{run['target']}" or res["addr"] != res["container"] + ":3000":
        errors.append(f"executor: container must be <app>-<target> and addr <container>:3000, got {res}")
    # What the app answered (executor.md, #1740): a Kubernetes /run's answer may carry it, in the status read's shape.
    ex_path, kres = CONTRACTS / "schema" / "executor.schema.json", doc["kube_run_result"]
    for name, answered, ok in (("a /run saying what the app answered", {"status": 400, "said": "<h1>Bad Request (400)</h1>"}, True),
                               ("a /run saying nothing answered", {"error": "no answer in 5 s"}, True),
                               ("a /run answered with a status no HTTP answer has", {"status": 600, "said": ""}, False),
                               ("a /run answered with both a status and an error", {"status": 200, "said": "", "error": "refused"}, False),
                               ("a /run whose error is a library's message", {"error": "dial tcp 10.0.3.7:3000: connect: connection refused"}, False)):
        if accepts(ex_path, {**doc, "kube_run_result": {**kres, "answered": answered}}) != ok:
            errors.append(f"selftest: {name} was {'refused' if ok else 'accepted'}")
    # An app that did not stay up (executor.md, /run): its last 20 lines, at most 8 KiB, every secret value — the request's env,
    # and a URL value's userinfo password on its own — replaced by its name.
    output = doc["run_exited"]["output"]
    if len(output.splitlines()) > 20 or len(output.encode()) > 8192:
        errors.append("executor: /run's output is the app's last 20 lines, at most 8 KiB")
    for name in unmasked(run["env"], output):
        errors.append(f"executor: /run's output carries the value of {name}; it carries <{name}>")
    password = "pw-" + "7f3a"  # a probe: a DSN's password, logged alone by an app that failed to connect
    if not unmasked({"DATABASE_URL": "postgres://app:" + password + "@db:5432/app"}, f"password authentication failed ({password})\n"):
        errors.append("selftest: an output carrying a DATABASE_URL's password alone was accepted")
    stop = doc["stop_request"]
    if f"{stop['app']}-{stop['target']}" != res["container"]:
        errors.append(f"executor: /stop names the container /run started, got {stop}")
    if doc["forget_request"]["app"] != run["app"]:
        errors.append(f"executor: /forget names the app /run started, got {doc['forget_request']}")
    if doc["running_result"]["image"] != run["image"]:
        errors.append("executor: /running answers the image /run started, by digest")
    # POST /running (executor.md): those asked whose container runs, each once, in the order asked — never one it was not asked.
    asked = [(a["app"], a["target"]) for a in doc["running_batch_request"]["containers"]]
    answered = [(a["app"], a["target"]) for a in doc["running_batch_result"]["running"]]
    if answered != [a for a in asked if a in answered] or len(set(answered)) != len(answered):
        errors.append(f"executor: POST /running answers {answered}; it names those asked that run, each once, in the order asked {asked}")
    if (run["app"], run["target"]) not in answered:
        errors.append("executor: POST /running names the container /run started, which runs")
    # Why it stopped (executor.md, POST /running): those asked that do not run, each once, in the order asked.
    stopped = [(a["app"], a["target"]) for a in doc["running_batch_result"]["stopped"]]
    if stopped and not doc["running_batch_request"].get("why"):
        errors.append("executor: POST /running says why though the request did not ask why; only the status read asks it")
    if stopped != [a for a in asked if a in stopped and a not in answered] or len(set(stopped)) != len(stopped):
        errors.append(f"executor: POST /running says why of {stopped}; it says why of those asked that do not run, each once, in the order asked {asked}")
    if not stopped:
        errors.append("selftest: the executor fixture says why of no stopped container, so the masking check below checks nothing")
    # Restarting (executor.md, POST /running): those asked started again since /run, each once, in the order asked, never
    # running and never stopped.
    restarting = doc["running_batch_result"].get("restarting", [])
    again = [(a["app"], a["target"]) for a in restarting]
    if again != [a for a in asked if a in again] or len(set(again)) != len(again) or set(again) & (set(answered) | set(stopped)):
        errors.append(f"executor: POST /running answers {again} restarting; each asked once, in the order asked, never running or stopped too")
    if not restarting:
        errors.append("selftest: the executor fixture answers no restarting container, so the checks above check nothing")
    # The app's own network and identity (executor.md, /run): canyon-app-<app>, the token's audience the app.
    if run["network"] != f"canyon-app-{run['app']}":
        errors.append(f"executor: /run joins {run['network']}; an app's containers join canyon-app-<app>, never another app's")
    if run["identity"]["audience"] != run["app"]:
        errors.append(f"executor: /run's identity audience is {run['identity']['audience']}; the token is minted for the app it reaches")
    # What the tag cleanup spares (executor.md, /run; design/OPERATIONS.md §1.5): the app's images, each by its digest alone,
    # exactly — a tag moves, so a kept tag would spare whatever it names at cleanup, not the image the server meant.
    kube = doc["kube_run_request"]
    if not run.get("keep") or not kube.get("keep"):
        errors.append("selftest: an executor fixture's /run keeps nothing, so the keep checks below check nothing")
    digest = lambda n: "sha256:" + f"{n:x}" * 64
    for name, keep, ok in (("sixteen digests", [digest(n) for n in range(16)], True),
                           ("one digest twice, the same as once", [digest(1)] * 2, True),
                           ("its own image's digest", [run["image"].split("@")[1]], True),
                           ("nothing, the same as no keep", [], True),
                           ("a tag", ["v1.3"], False),
                           ("an image reference, not a digest", [doc["build_result"]["image"]], False),
                           ("a digest and a trailing newline", [digest(1) + "\n"], False),
                           ("a digest cut short", [digest(1)[:-1]], False),
                           ("an uppercase digest", [digest(10).upper().replace("SHA256", "sha256")], False),
                           ("a sha512 digest", ["sha512:" + "a" * 128], False),
                           ("a number", [1], False),
                           ("null", None, False),
                           ("a bare string, not a list", digest(1), False),
                           ("seventeen digests", [digest(n) for n in range(17)], False)):
        for key, req in (("run_request", run), ("kube_run_request", kube)):
            if accepts(CONTRACTS / "schema" / "executor.schema.json", {**doc, key: {**req, "keep": keep}}) != ok:
                errors.append(f"selftest: an executor {key} keeping {name} was {'refused' if ok else 'accepted'}")
    # PUT /access (Kubernetes) rewrites the file of a container /run started; its roles are the ones it declares.
    put, access = doc["access_request"], doc["access_request"]["access"]
    if (put["app"], put["target"]) != (run["app"], run["target"]):
        errors.append(f"executor: PUT /access names {put['app']}-{put['target']}, not the container /run started")
    for e in access["entries"]:
        if set(e["roles"]) - set(access["roles"]):
            errors.append(f"executor: access entry {e['kind']}:{e['grantee']} holds {sorted(set(e['roles']) - set(access['roles']))}, which the file does not declare")
    env = lambda **extra: {**run, "env": {**run["env"], **extra}}
    entry = lambda **fields: {**put, "access": {**access, "entries": [{"kind": "email", "grantee": "sam@meridian.example", "subject": "", "level": "viewer", "roles": [], **fields}]}}
    for name, key, value in (("the shared canyon-apps network", "run_request", {**run, "network": "canyon-apps"}),
                             ("no identity", "run_request", {k: v for k, v in run.items() if k != "identity"}),
                             ("CANYON_IDENTITY_ISSUER in its env", "run_request", env(CANYON_IDENTITY_ISSUER="https://evil.example")),
                             ("signin outside the front door", "run_request", {**run, "signin": {"issuer": "https://login.meridian.example"}}),
                             ("an everyone grant above read-only", "access_request", entry(kind="everyone", grantee="", level="contributor")),
                             ("a level outside the three", "access_request", entry(level="admin"))):
        if accepts(CONTRACTS / "schema" / "executor.schema.json", {**doc, key: value}):
            errors.append(f"selftest: an executor {key} with {name} was accepted")
    for name, value in (("DOCKERFILE_PATH", "Dockerfile"), ("MY_DOCKER_TOKEN", "t"), ("PACKAGE_NAME", "x")):  # a name that only holds DOCKER or PACK is the app's
        if not accepts(CONTRACTS / "schema" / "executor.schema.json", {**doc, "run_request": env(**{name: value})}):
            errors.append(f"selftest: an executor run_request with {name} in its env was refused")
    # A failed detect's builds (executor.md, /build): each buildpack's name, else its id — printable ASCII from a letter
    # or digit, at most 128 characters, never a control or bidi character — each once, at most 64.
    undetected = doc["build_undetected"]
    for name, builds, ok in (("a name with spaces and dots", ["Paketo Buildpack for Node.js"], True),
                             ("an id, a name's fallback", ["paketo-buildpacks/nodejs"], True),
                             ("a control character", ["Paketo Buildpack for Go\u001b[2J"], False),
                             ("a bidi override", ["Paketo Buildpack for Ruby\u202e"], False),
                             ("a 129-character name", ["P" * 129], False),
                             ("a name opening on a space", [" Paketo Buildpack for Go"], False),
                             ("65 names", [f"Buildpack {n}" for n in range(65)], False),
                             ("one name twice", ["Paketo Buildpack for Go"] * 2, False)):
        if accepts(CONTRACTS / "schema" / "executor.schema.json", {**doc, "build_undetected": {**undetected, "builds": builds}}) != ok:
            errors.append(f"selftest: a failed detect's builds with {name} were {'refused' if ok else 'accepted'}")
    # POST /scan: the answer names the tool asked, and its SARIF is one run of SARIF 2.1.0 — a report finding.md attaches.
    scan, found = doc["scan_request"], doc["scan_result"]
    if found["tool"] != scan["tool"]:
        errors.append(f"executor: /scan answers {found['tool']}, not the tool asked, {scan['tool']}")
    sarif = sarif_schema()
    for e in (validators.validator_for(sarif)(sarif).iter_errors(found["sarif"]) if sarif else []):
        errors.append(f"executor: /scan: SARIF 2.1.0: {e.message}")
    if len(found["sarif"]["runs"]) != 1:
        errors.append("executor: /scan: the SARIF is one run, as an attached report is")
    schema = CONTRACTS / "schema" / "executor.schema.json"
    unfinished = {"tool": "checkov", "image": "bridgecrew/checkov@sha256:" + "0" * 64, "not_finished": "timeout"}
    for name, key, value, ok in (
            ("checkov that did not finish", "scan_result", unfinished, True),
            ("opengrep that could not load its rules", "scan_result", {**unfinished, "tool": "opengrep", "not_finished": "rules_error"}, True),
            ("grype's SARIF", "scan_result", {**found, "tool": "grype"}, True),
            ("no image for the tool", "scan_result", {"tool": "grype", "not_finished": "no_image"}, True),
            ("opengrep skipping a path", "scan_request", {**scan, "tool": "opengrep", "settings": {"skip_paths": ["vendor"]}}, True),
            ("a setting outside the tool's allow-list", "scan_request", {**scan, "settings": {"severity": "HIGH"}}, False),
            ("another tool's setting", "scan_request", {**scan, "settings": {"frameworks": ["terraform"]}}, False),
            ("opengrep's rules repo, which the server fetches", "scan_request", {**scan, "tool": "opengrep", "settings": {"rules": {"repo": "https://github.com/meridian/r", "commit": "0" * 40}}}, False),
            ("a tool Canyon does not run", "scan_request", {**scan, "tool": "gitleaks", "settings": {}}, False),
            ("both sarif and not_finished", "scan_result", {**found, "not_finished": "timeout"}, False),
            ("a reason executor.md does not name", "scan_result", {**unfinished, "not_finished": "crashed"}, False),
            ("an sbom, which v1 does not answer", "scan_result", {**found, "tool": "grype", "sbom": {"bomFormat": "CycloneDX"}}, False),
            ("an image beside no_image", "scan_result", {**unfinished, "not_finished": "no_image"}, False),
            ("a timeout without the image that ran", "scan_result", {"tool": "checkov", "not_finished": "timeout"}, False),
            ("a skip path that reads as an option", "scan_request", {**scan, "settings": {"skip_paths": ["-rf"]}}, False),
            ("a vulnerability database date on checkov", "scan_result", {**found, "tool": "checkov"}, False),
            ("trivy without its database date", "scan_result", {k: v for k, v in found.items() if k != "db_built_at"}, False),
            ("an image by tag", "scan_result", {**found, "image": "ghcr.io/aquasecurity/trivy:0.56.2"}, False),
            ("opengrep's files not read whole", "scan_result", {k: v for k, v in found.items() if k != "db_built_at"} | {"tool": "opengrep", "unparsed": ["src/a.tsx"]}, True),
            ("an empty unparsed", "scan_result", {k: v for k, v in found.items() if k != "db_built_at"} | {"tool": "opengrep", "unparsed": []}, False),
            ("unparsed on trivy", "scan_result", {**found, "unparsed": ["src/a.tsx"]}, False),
            ("opengrep's rules it could not load", "scan_result", {k: v for k, v in found.items() if k != "db_built_at"} | {"tool": "opengrep", "unloaded": ["org-bad-regex"]}, True),
            ("unloaded on trivy", "scan_result", {**found, "unloaded": ["org-bad-regex"]}, False),
            ("unparsed beside not_finished", "scan_result", {**unfinished, "tool": "opengrep", "unparsed": ["src/a.tsx"]}, False)):
        if accepts(schema, {**doc, key: value}) != ok:
            errors.append(f"selftest: an executor /scan with {name} was {'refused' if ok else 'accepted'}")
    seam4_image(doc, schema)
    seam4_kube(doc, schema)


def image_layer_errors(img, rematch):
    """What the schema cannot say of an image answer and a re-match of its SBOM (executor.md, /scan): trivy's SBOM
    names the image's diff IDs sorted, and diff_ids gives their order, bottom first — the run image's top layer
    below every buildpack's layer and the app's, as its lifecycle label names them; and each vulnerability the
    re-match names is joined to its component by PkgIdentifier.BOMRef, never by name and version, and carries
    that component's LayerDiffID, one of the image's diff IDs."""
    found = []
    props = lambda c: [(p["name"], p["value"]) for p in c.get("properties", [])]
    diff_ids = img["diff_ids"]
    if sorted(diff_ids) != [v for n, v in props(img["sbom"]["metadata"]["component"]) if n == "aquasecurity:trivy:DiffID"]:
        found.append("executor: /scan's image answer's diff_ids are not the layers its SBOM names, which trivy sorts")
    owners = json.loads(img["lifecycle_metadata"])
    top = owners["runImage"]["topLayer"]
    above = [l["sha"] for bp in owners["buildpacks"] for l in bp["layers"].values()] + [l["sha"] for l in owners["app"]]
    if top not in diff_ids or any(sha not in diff_ids or diff_ids.index(sha) <= diff_ids.index(top) for sha in above):
        found.append("executor: /scan's image answer's diff_ids are not in the image's order: the run image's top layer below every buildpack's and the app's")
    layer_of = {c["bom-ref"]: v for c in img["sbom"]["components"] for n, v in props(c) if n == "aquasecurity:trivy:LayerDiffID"}
    read = {p.get("Identifier", {}).get("BOMRef") for r in rematch["report"]["Results"] for p in r.get("Packages", [])}
    for v in (v for r in rematch["report"]["Results"] for v in r.get("Vulnerabilities", [])):
        if v.get("PkgIdentifier", {}).get("BOMRef") not in read:
            found.append(f"executor: /scan's re-match names a vulnerability of {v.get('PkgIdentifier', {}).get('BOMRef')}, which its packages (--list-all-pkgs) do not list as read")
        layer, ref = v.get("Layer", {}).get("DiffID"), v.get("PkgIdentifier", {}).get("BOMRef")
        if layer is None or ref not in layer_of or layer != layer_of[ref] or layer not in diff_ids:
            found.append(f"executor: /scan's re-match of {ref} carries layer {layer}; it keeps the LayerDiffID of the image's "
                         "component its PkgIdentifier.BOMRef names, one of the image's diff IDs")
    return found


def seam4_image(doc, schema):
    """executor.md, /scan's image and sbom targets (PATCHING.md §3.1, §3.2): the image scanned is the one /build
    answered, and image_layer_errors; then the shapes the two targets' answers may and may not take."""
    req, img, rematch = doc["scan_image_request"], doc["scan_image_result"], doc["scan_sbom_result"]
    if req["image"] != doc["build_result"]["image"]:
        errors.append("executor: /scan's image target reads the image /build answered, by digest")
    if doc["scan_run_image_request"]["image"] != doc["builder_result"]["run_image"]:
        errors.append("executor: /scan's run image is the run_image GET /builder answered, exactly (executor.md, The run image)")
    if not any(r.get("Vulnerabilities") for r in rematch["report"]["Results"]):
        errors.append("selftest: the executor fixture's re-match names no vulnerability, so the layer check checks nothing")
    errors.extend(image_layer_errors(img, rematch))
    vuln = rematch["report"]["Results"][0]["Vulnerabilities"][0]
    with_vuln = lambda v: {**rematch, "report": {**rematch["report"], "Results": [{**rematch["report"]["Results"][0], "Vulnerabilities": [v]}]}}
    for name, value in (("diff_ids reversed", ({**img, "diff_ids": img["diff_ids"][::-1]}, rematch)),
                        ("a vulnerability joined by name to another component", (img, with_vuln({**vuln, "PkgIdentifier": {**vuln["PkgIdentifier"], "BOMRef": "pkg:npm/cross-spawn@7.0.3"}}))),
                        ("a vulnerability in another layer", (img, with_vuln({**vuln, "Layer": {"DiffID": img["diff_ids"][1]}}))),
                        ("a vulnerability of a package trivy did not list as read", (img, {**rematch, "report": {**rematch["report"], "Results": [{**rematch["report"]["Results"][0], "Packages": []}]}}))):
        if not image_layer_errors(*value):
            errors.append(f"selftest: an executor image answer and re-match with {name} were accepted")
    tool = "ghcr.io/aquasecurity/trivy@sha256:" + "0" * 64
    for name, key, value, ok in (
            ("an image scan of grype", "scan_image_request", {**req, "tool": "grype"}, False),
            ("an image scan with settings", "scan_image_request", {**req, "settings": {}}, False),
            ("an image scan of a package", "scan_image_request", {**req, "package": "pkg_7d3e8f"}, False),
            ("an image scan naming no image", "scan_image_request", {k: v for k, v in req.items() if k != "image"}, False),
            ("an image scan of an image by tag", "scan_image_request", {**req, "image": "canyon/claims-portal:latest"}, False),
            ("an image named on a tree scan", "scan_request", {**doc["scan_request"], "image": req["image"]}, False),
            ("a run-image scan naming an app", "scan_run_image_request", {**doc["scan_run_image_request"], "app": "claims-portal"}, False),
            ("an app's image scan naming the run image", "scan_image_request", {**req, "image": doc["builder_result"]["run_image"]}, False),
            ("a run-image scan by tag", "scan_run_image_request", {**doc["scan_run_image_request"], "image": "canyon-run:noble"}, False),
            ("a Kubernetes run-image scan", "scan_run_image_request", {**doc["scan_run_image_request"], "image": "registry.example/canyon-builder-run-image@sha256:" + "5f" * 32}, True),
            ("a scan refused for another reason", "scan_refused", {**doc["scan_refused"], "error": "another_run_image"}, False),
            ("an sbom scan naming an app", "scan_sbom_request", {"tool": "trivy", "target": "sbom", "app": "claims-portal"}, False),
            ("an sbom scan of opengrep", "scan_sbom_request", {"tool": "opengrep", "target": "sbom"}, False),
            ("a packages scan with settings", "scan_packages_request", {**doc["scan_packages_request"], "settings": {}}, False),
            ("a packages scan naming no package", "scan_packages_request", {k: v for k, v in doc["scan_packages_request"].items() if k != "package"}, False),
            ("a packages scan naming an image", "scan_packages_request", {**doc["scan_packages_request"], "image": req["image"]}, False),
            ("a packages scan of grype", "scan_packages_request", {**doc["scan_packages_request"], "tool": "grype"}, False),
            ("packages with a database date", "scan_packages_result", {**doc["scan_packages_result"], "db_built_at": "2026-09-24T06:12:40Z"}, False),
            ("packages with a vulnerability", "scan_packages_result", {**doc["scan_packages_result"], "packages": {"SchemaVersion": 2, "Results": [{"Vulnerabilities": [{"VulnerabilityID": "CVE-2021-23337"}]}]}}, False),
            ("packages beside SARIF", "scan_packages_result", {**doc["scan_packages_result"], "sarif": doc["scan_result"]["sarif"]}, False),
            ("packages over the cap with no reason", "scan_packages_result", {"tool": "trivy", "image": tool, "not_finished": "over_cap"}, False),
            ("packages over the cap", "scan_packages_result", {"tool": "trivy", "image": tool, "not_finished": "over_cap", "reason": "report"}, True),
            ("packages of a tree with none, Results left out", "scan_packages_result", {**doc["scan_packages_result"], "packages": {"SchemaVersion": 2, "ArtifactType": "filesystem"}}, True),
            ("packages of another schema version", "scan_packages_result", {**doc["scan_packages_result"], "packages": {"SchemaVersion": 1, "Results": []}}, False),
            ("packages of grype", "scan_result", {"tool": "grype", "image": tool, "packages": doc["scan_packages_result"]["packages"]}, False),
            ("an image SBOM with a database date", "scan_image_result", {**img, "db_built_at": "2026-09-24T06:12:40Z"}, False),
            ("an image SBOM beside SARIF", "scan_image_result", {**img, "sarif": doc["scan_result"]["sarif"]}, False),
            ("lifecycle metadata without an SBOM", "scan_sbom_result", {**rematch, "lifecycle_metadata": "{}"}, False),
            ("diff IDs without an SBOM", "scan_sbom_result", {**rematch, "diff_ids": img["diff_ids"]}, False),
            ("an image SBOM without its diff IDs", "scan_image_result", {k: v for k, v in img.items() if k != "diff_ids"}, False),
            ("a Kubernetes image SBOM and the referrer publishing it", "scan_image_result", {**img, "image": tool, "referrer": "sha256:" + "c4" * 32}, True),
            ("a referrer beside no SBOM", "scan_image_result", {"tool": "trivy", "image": tool, "not_finished": "timeout", "referrer": "sha256:" + "c4" * 32}, False),
            ("a referrer named by tag", "scan_image_result", {**img, "referrer": "canyon/claims-portal:sbom"}, False),
            ("a referrer on a re-match", "scan_sbom_result", {**rematch, "referrer": "sha256:" + "c4" * 32}, False),
            ("a re-match without its database date", "scan_sbom_result", {k: v for k, v in rematch.items() if k != "db_built_at"}, False),
            ("a re-match from grype", "scan_sbom_result", {**rematch, "tool": "grype"}, False),
            ("no Java database", "scan_image_result", {"tool": "trivy", "image": tool, "not_finished": "db_missing", "reason": "java"}, True),
            ("a stale Java database", "scan_image_result", {"tool": "trivy", "image": tool, "not_finished": "db_stale", "reason": "java"}, True),
            ("a database reason that is not the Java database", "scan_image_result", {"tool": "trivy", "image": tool, "not_finished": "db_missing", "reason": "main"}, False),
            ("a reason on a timeout", "scan_image_result", {"tool": "trivy", "image": tool, "not_finished": "timeout", "reason": "java"}, False),
            ("an image over its cap, not saying why", "scan_image_result", {"tool": "trivy", "image": tool, "not_finished": "over_cap"}, False),
            ("a re-match over its cap, not saying why", "scan_sbom_result", {"tool": "trivy", "image": tool, "not_finished": "over_cap"}, False),
            ("a re-match naming what trivy lists but never scans", "scan_sbom_result", {**rematch, "unscanned": ["pkg:conda/numpy@1.26.4"]}, True),
            ("unscanned on an image answer", "scan_image_result", {**img, "unscanned": ["pkg:conda/numpy@1.26.4"]}, False),
            ("unscanned beside not_finished", "scan_sbom_result", {"tool": "trivy", "image": tool, "not_finished": "not_sarif", "reason": "unread", "unscanned": ["x"]}, False),
            ("an empty unscanned", "scan_sbom_result", {**rematch, "unscanned": []}, False),
            ("a re-match that trivy read in part", "scan_sbom_result", {"tool": "trivy", "image": tool, "not_finished": "not_sarif", "reason": "unread"}, True),
            ("an image answer read in part", "scan_image_result", {"tool": "trivy", "image": tool, "not_finished": "not_sarif", "reason": "unread"}, False),
            ("a not_sarif reason executor.md does not name", "scan_sbom_result", {"tool": "trivy", "image": tool, "not_finished": "not_sarif", "reason": "partly"}, False),
            ("no room, saying why", "scan_result", {"tool": "trivy", "image": tool, "not_finished": "no_room", "reason": "0/4 nodes are available: 4 Insufficient cpu."}, True),
            ("no room, without why", "scan_result", {"tool": "trivy", "image": tool, "not_finished": "no_room"}, False),
            ("a report inside an image answer", "scan_image_result", {**img, "report": rematch["report"]}, False),
            ("a report instead of an image's SBOM", "scan_image_result", {k: v for k, v in rematch.items()}, False),
            ("reason java on an sbom answer", "scan_sbom_result", {"tool": "trivy", "image": tool, "not_finished": "db_missing", "reason": "java"}, False),
            ("an sbom answer over its cap, past the report kept", "scan_sbom_result", {"tool": "trivy", "image": tool, "not_finished": "over_cap", "reason": "report"}, True),
            ("an sbom answer over an image's cap", "scan_sbom_result", {"tool": "trivy", "image": tool, "not_finished": "over_cap", "reason": "image"}, False),
            ("an image over its cap, saying so", "scan_image_result", {"tool": "trivy", "image": tool, "not_finished": "over_cap", "reason": "image"}, True),
            ("an over_cap reason executor.md does not name", "scan_image_result", {"tool": "trivy", "image": tool, "not_finished": "over_cap", "reason": "big"}, False),
            ("a report of another schema version", "scan_sbom_result", {**rematch, "report": {**rematch["report"], "SchemaVersion": 1}}, False),
            ("a vulnerability without its layer", "scan_sbom_result", with_vuln({k: v for k, v in vuln.items() if k != "Layer"}), False),
            ("a vulnerability whose layer names no diff ID", "scan_sbom_result", with_vuln({**vuln, "Layer": {}}), False),
            ("a vulnerability without its component", "scan_sbom_result", with_vuln({k: v for k, v in vuln.items() if k != "PkgIdentifier"}), False),
            ("a layer that is not a diff ID", "scan_sbom_result", with_vuln({**vuln, "Layer": {"DiffID": "1a"}}), False),
            ("a lifecycle label over 1 MiB", "scan_image_result", {**img, "lifecycle_metadata": "x" * ((1 << 20) + 1)}, False),
            ("129 diff IDs", "scan_image_result", {**img, "diff_ids": ["sha256:" + f"{n:064x}" for n in range(129)]}, False),
            ("a tree scan without its package", "scan_request", {k: v for k, v in doc["scan_request"].items() if k != "package"}, False),
            ("a tree scan without its settings", "scan_request", {k: v for k, v in doc["scan_request"].items() if k != "settings"}, False),
            ("a tree scan saying so", "scan_request", {**doc["scan_request"], "target": "tree"}, True),
            ("a tree scan saying so, naming an image", "scan_request", {**doc["scan_request"], "target": "tree", "image": req["image"]}, False),
            ("an image request saying tree", "scan_image_request", {**req, "target": "tree"}, False),
            ("an sbom request saying image", "scan_sbom_request", {"tool": "trivy", "target": "image"}, False)):
        if accepts(schema, {**doc, key: value}) != ok:
            errors.append(f"selftest: an executor /scan with {name} was {'refused' if ok else 'accepted'}")
    seam4_egress(doc, schema)


# executor.md, /run's Egress: a web container's fixed address in the lower /25 of its app's /24.
EGRESS_SLOTS = {10: "preview", 12: "preview", 11: "prod", 13: "prod"}


def seam4_egress(doc, schema):
    """executor.md, /run's Egress, POST /callers and POST /sidecars. What the schema cannot say: the database
    a run's env names is in its no_proxy, so the app's own database never reaches the proxy; /callers answers
    only addresses asked, each once, in the order asked; on Docker an address docker handed out (the upper
    /25) is never a caller, and a fixed address names its target; /sidecars names each environment once."""
    run = doc["run_request"]
    db = urllib.parse.urlsplit(run["env"]["DATABASE_URL"]).hostname
    if db not in run["egress"]["no_proxy"]:
        errors.append(f"executor: /run's egress no_proxy {run['egress']['no_proxy']} leaves out {db}, the database its env names")
    asked = doc["callers_request"]["addresses"]
    callers = doc["callers_result"]["callers"]
    named = [c["address"] for c in callers]
    if named != [a for a in asked if a in named] or len(set(named)) != len(named):
        errors.append(f"executor: POST /callers answers {named}; it names addresses asked, each once, in the order asked {asked}")
    for c in callers:
        host = int(c["address"].rsplit(".", 1)[1])
        if host >= 128:
            errors.append(f"executor: POST /callers names {c['address']}, an address docker handed out; only a fixed one names a caller")
        elif c["kind"] == "web" and EGRESS_SLOTS.get(host) != c["target"]:
            errors.append(f"executor: POST /callers names {c['address']} {c['target']}; .10 and .12 are preview's, .11 and .13 prod's")
    if len(named) == len(asked):
        errors.append("selftest: the executor fixture's /callers leaves out no address, so the unknown checks check nothing")
    rolled = [(r["app"], r["target"]) for r in doc["sidecars_result"]["rolled"]]
    if len(set(rolled)) != len(rolled):
        errors.append(f"executor: POST /sidecars answers {rolled}; it names each environment once")
    for name, key, value in (("a /run whose proxy is not Envoy's port", "run_request", {**run, "egress": {**run["egress"], "proxy": "http://canyon-egress:8080"}}),
                             ("a /run whose egress carries a CA before layer 3", "run_request", {**run, "egress": {**run["egress"], "ca": "pem"}}),
                             ("a /run whose run is not a run id", "run_request", {**run, "run": "pkg_7a1"}),
                             ("a /callers asking an IPv6 address", "callers_request", {"addresses": ["fd00::1"]}),
                             ("a /callers asking nothing", "callers_request", {"addresses": []}),
                             ("a /callers caller without its run", "callers_result", {"callers": [{k: v for k, v in callers[0].items() if k != "run"}]}),
                             ("a /callers caller without its since", "callers_result", {"callers": [{k: v for k, v in callers[0].items() if k != "since"}]}),
                             ("a /callers caller whose since is not UTC", "callers_result", {"callers": [{**callers[0], "since": "2026-10-11T11:14:02+02:00"}]})):
        if accepts(schema, {**doc, key: value}):
            errors.append(f"selftest: {name} was accepted")


PRIVATE = ["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16",  # and the rest Docker's floor refuses (deploy/egress/floor.sh)
           "0.0.0.0/8", "192.0.0.0/24", "198.18.0.0/15", "224.0.0.0/4", "240.0.0.0/4", "168.63.129.16/32"]


def private(cidr):
    """Whether a CIDR lies inside the ranges an app's public egress excludes: the apps database's are."""
    try:
        net = ipaddress.ip_network(cidr)
    except ValueError:
        return False
    return any(net.version == 4 and net.subnet_of(ipaddress.ip_network(p)) for p in PRIVATE[:4])


def kube_objects(run, objs, floored=False, signin=False):
    """Invariants 10 and 11 on what the Kubernetes /run writes (executor.md, On Kubernetes): the Service
    and NetworkPolicy <app>-<target>, 4180 — the front container's port — from the router's pods and the sidecar's
    4183 from the executor's (What the app answered), and nothing else in, and out
    only DNS, the apps database on 5433 in private ranges and public 80/443 — floored, with egress, Canyon's
    egress gateway on 3128 instead of any public address (design/EGRESS.md §2.2), and under
    CANYON_KUBE_SIGNIN_DIRECT (signin) TCP 443 to single IPv4 addresses (Sign-in past the gateway). Answers
    the errors, so a planted fault can be checked to fail."""
    out, name, port = [], f"{run['app']}-{run['target']}", 4180
    labels = {"canyon.app": run["app"], "canyon.target": run["target"]}
    svc, pol = objs["service"]["spec"], objs["network_policy"]["spec"]
    if {objs["service"]["metadata"]["name"], objs["network_policy"]["metadata"]["name"]} != {name}:
        out.append(f"executor: the Service and NetworkPolicy are {name}, the name the router reaches")
    if svc["selector"] != labels or pol["podSelector"] != {"matchLabels": labels}:
        out.append(f"executor: the Service and NetworkPolicy select {labels}, the app's pod and no other")
    if svc["ports"] != [{"port": port, "targetPort": port}]:
        out.append(f"executor: the Service is {port} to {port}, the sidecar's port on both sides (invariant 10)")
    front = [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "canyon"}}, "podSelector": {"matchLabels": {"canyon.front-door": "true"}}}]
    asker = [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "canyon"}}, "podSelector": {"matchLabels": {"app.kubernetes.io/name": "canyon-executor"}}}]
    if pol["ingress"] != [{"from": front, "ports": [{"protocol": "TCP", "port": port}]}, {"from": asker, "ports": [{"protocol": "TCP", "port": 4183}]}]:
        out.append(f"executor: the NetworkPolicy admits {port} from the router's pods, 4183 from the executor's, and nothing else (invariant 10)")
    dns = [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "kube-system"}}, "podSelector": {"matchLabels": {"k8s-app": "kube-dns"}}}]
    gateway = [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "canyon"}}, "podSelector": {"matchLabels": {"app.kubernetes.io/name": "canyon-egress"}}}]
    way_out = ([("TCP", 3128)], gateway) if floored else ([("TCP", 80), ("TCP", 443)], [{"ipBlock": {"cidr": "0.0.0.0/0", "except": PRIVATE}}])
    for rule in pol["egress"]:
        ports = sorted((p["protocol"], p["port"]) for p in rule["ports"])
        if any(set(p) - {"protocol", "port"} for p in rule["ports"]):
            out.append(f"executor: egress {ports} runs on to an endPort: each rule names its ports one by one (invariant 11)")
        if not ((ports == [("TCP", 53), ("UDP", 53)] and rule["to"] == dns)
                or (ports == [("TCP", 5433)] and all(list(t) == ["ipBlock"] and list(t["ipBlock"]) == ["cidr"] and private(t["ipBlock"]["cidr"]) for t in rule["to"]))
                or (ports, rule["to"]) == way_out
                or (signin and ports == [("TCP", 443)] and rule["to"] and all(list(t) == ["ipBlock"] and list(t["ipBlock"]) == ["cidr"] and single(t["ipBlock"]["cidr"]) for t in rule["to"]))):
            out.append(f"executor: egress {ports} is none of DNS, the apps database on 5433, {'the egress gateway on 3128' if floored else 'public 80/443'}{' and the sign-in addresses on 443' if signin else ''} (invariant 11)")
    return out


def single(cidr):
    """Whether a CIDR is one IPv4 address written as a /32, never a bare address, outside link-local, the node's
    instance metadata: the widest a sign-in address may be (executor.md, Sign-in past the gateway)."""
    if "/" not in cidr:
        return False
    try:
        net = ipaddress.ip_network(cidr)
    except ValueError:
        return False
    return net.version == 4 and net.prefixlen == 32 and not net.subnet_of(ipaddress.ip_network("169.254.0.0/16"))


def seam4_kube(doc, schema):
    """The Kubernetes /run and /probe (executor.md, On Kubernetes): the image under the executor's registry,
    the access file and the front door, the sidecar's key set, the objects /run writes."""
    run, kube, res = doc["run_request"], doc["kube_run_request"], doc["kube_run_result"]
    if {"access", "front_door", "database_ca"} & set(run) or not {"access", "front_door"} <= set(kube):
        errors.append("executor: a Kubernetes /run carries access and front_door, a Docker /run neither, nor database_ca")
    registry, _, rest = kube["image"].partition("/canyon/")
    if not registry or "/" in registry or not rest.startswith(kube["app"] + "@") or run["image"].split("@")[0] != "canyon/" + run["app"]:
        errors.append(f"executor: a Kubernetes image is <registry>/canyon/<app>@sha256:…, a Docker one canyon/<app>@sha256:…; got {kube['image']}")
    if kube["identity"] != {**kube["identity"], "audience": kube["app"], "jwks_url": "http://127.0.0.1:4182/.well-known/jwks.json"} or kube["network"] != f"canyon-app-{kube['app']}":
        errors.append("executor: a Kubernetes /run names its app's network and audience, and the sidecar's key set on 127.0.0.1:4182")
    name = f"{kube['app']}-{kube['target']}"
    if {k: v for k, v in res.items() if k != "previous_gone"} != {"container": name, "addr": f"{name}.canyon-apps.svc:4180"}:
        errors.append(f"executor: a Kubernetes /run answers {name} at {name}.canyon-apps.svc:4180, got {res}")
    if "PORT" in kube["env"]:
        errors.append("executor: PORT is the executor's, never in the env")
    if urllib.parse.urlsplit(kube["env"].get("DATABASE_URL", "postgres://h:5433")).port != 5433:
        errors.append("executor: an app on Kubernetes reaches its database on 5433 (the apps database)")
    for e in kube["access"]["entries"]:
        if set(e["roles"]) - set(kube["access"]["roles"]):
            errors.append(f"executor: access entry {e['kind']}:{e['grantee']} holds a role the file does not declare")
    errors.extend(kube_objects(kube, doc["kube_objects"]))
    floored = {**doc["kube_objects"], "network_policy": doc["kube_egress_policy"]}
    errors.extend(kube_objects(kube, floored, floored=True))
    if {**doc["kube_egress_policy"], "spec": {**doc["kube_egress_policy"]["spec"], "egress": []}} != {**doc["kube_objects"]["network_policy"], "spec": {**doc["kube_objects"]["network_policy"]["spec"], "egress": []}}:
        errors.append("executor: with egress the NetworkPolicy differs from the one without in its egress alone")
    errors.extend(signin_policy(kube, floored, doc["kube_signin_policy"]))
    if doc["kube_egress"]["proxy"] != "http://canyon-egress.canyon.svc:3128":
        errors.append("executor: on Kubernetes egress.proxy is http://canyon-egress.canyon.svc:3128, the gateway's Service")
    if doc["probe_request"] != {k: kube["front_door"][k] for k in ("domain", "form")}:
        errors.append("executor: /probe asks about the front door /run carries")
    door, objs = kube["front_door"], doc["kube_objects"]
    oidc = {"choice": 1, "gateway": "oidc", "issuer": "https://login.meridian.example", "audience": "canyon-gateway", "jwks_url": "https://login.meridian.example/keys", "cookie": "_pomerium", "domain": door["domain"], "form": door["form"]}
    alb = {"choice": 1, "gateway": "alb", "alb_arn": "arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/canyon/0123456789abcdef", "domain": door["domain"], "form": door["form"]}
    with_door = lambda **f: {**kube, "front_door": {**door, **f}}
    service = lambda port: {**objs, "service": {**objs["service"], "spec": {**objs["service"]["spec"], "ports": [{"port": 4180, "targetPort": port}]}}}
    ca = kube["database_ca"]
    with_ca = lambda text: {**kube, "database_ca": text}
    for name, key, value, ok in (
            ("choice 1 behind an OIDC gateway", "kube_run_request", {**kube, "front_door": oidc}, True),
            ("choice 1 behind an AWS ALB", "kube_run_request", {**kube, "front_door": alb}, True),
            ("the app-env form", "kube_run_request", with_door(form="app-env.domain"), True),
            ("a form outside the two", "kube_run_request", with_door(form="app.domain"), False),
            ("choice 3, which Kubernetes does not offer", "kube_run_request", with_door(choice=3), False),
            ("choice 1 without its gateway", "kube_run_request", {**kube, "front_door": {k: v for k, v in oidc.items() if k != "gateway"}}, False),
            ("an OIDC gateway without its audience", "kube_run_request", {**kube, "front_door": {k: v for k, v in oidc.items() if k != "audience"}}, False),
            ("an OIDC gateway without its session cookie", "kube_run_request", {**kube, "front_door": {k: v for k, v in oidc.items() if k != "cookie"}}, False),
            ("choice 2 naming a gateway's cookie", "kube_run_request", with_door(cookie="_pomerium"), False),
            ("an ALB gateway naming an issuer", "kube_run_request", {**kube, "front_door": {**alb, "issuer": "https://login.meridian.example"}}, False),
            ("a key set over http", "kube_run_request", {**kube, "front_door": {**oidc, "jwks_url": "http://login.meridian.example/keys"}}, False),
            ("choice 2 without its sign-in", "kube_run_request", {**kube, "front_door": {k: v for k, v in door.items() if k != "signin"}}, False),
            ("a domain whose first label is preview", "kube_run_request", with_door(domain="preview.meridian.example"), False),
            ("an access file without the front door", "kube_run_request", {k: v for k, v in kube.items() if k != "front_door"}, False),
            ("no database CA", "kube_run_request", {k: v for k, v in kube.items() if k != "database_ca"}, True),
            ("a database CA of two certificates", "kube_run_request", with_ca(ca + ca), True),
            ("a database CA that is not PEM", "kube_run_request", with_ca("apps-db.meridian.example CA"), False),
            ("an empty database CA", "kube_run_request", with_ca(""), False),
            ("text around the database CA's certificates", "kube_run_request", with_ca("# the RDS bundle\n" + ca), False),
            ("a public key as the database CA", "kube_run_request", with_ca("-----BEGIN PUBLIC KEY-----\nMFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE\n-----END PUBLIC KEY-----\n"), False),
            ("a database CA over 256 KiB", "kube_run_request", with_ca(ca * (262144 // len(ca) + 1)), False),
            ("a database CA on a /run without access or front door", "kube_run_request", {k: v for k, v in kube.items() if k not in ("access", "front_door")}, False),
            ("a Service targeting the app's port 3000", "kube_objects", service(3000), False),
            ("a Service targeting the sidecar behind oauth2-proxy", "kube_objects", service(4181), False),
            ("an ingress rule with no port", "kube_objects", {**objs, "network_policy": {**objs["network_policy"], "spec": {**objs["network_policy"]["spec"], "ingress": [{"from": objs["network_policy"]["spec"]["ingress"][0]["from"]}]}}}, False),
            ("a probe that failed without a fix", "probe_result", {"network_policy": "not_enforced", "certificate": "ok", "fix": ""}, False),
            ("a probe that passed with a fix", "probe_result", {**doc["probe_result"], "fix": "save again"}, False),
            ("a certificate answer outside the two", "probe_result", {**doc["probe_result"], "certificate": "expired"}, False)):
        if accepts(schema, {**doc, key: value}) != ok:
            errors.append(f"selftest: an executor {key} with {name} was {'refused' if ok else 'accepted'}")
    egress = lambda rule: {**objs, "network_policy": {**objs["network_policy"], "spec": {**objs["network_policy"]["spec"], "egress": [rule]}}}
    for name, planted in (("the apps database's port open to the internet", egress({"to": [{"ipBlock": {"cidr": "0.0.0.0/0"}}], "ports": [{"protocol": "TCP", "port": 5433}]})),
                          ("egress to the control plane's Postgres on 5432", egress({"to": [{"ipBlock": {"cidr": "10.20.48.0/20"}}], "ports": [{"protocol": "TCP", "port": 5432}]})),
                          ("public egress reaching instance metadata", egress({"to": [{"ipBlock": {"cidr": "0.0.0.0/0", "except": [p for p in PRIVATE if p != "169.254.0.0/16"]}}], "ports": [{"protocol": "TCP", "port": 443}]}))):
        if not kube_objects(kube, planted):
            errors.append(f"selftest: a Kubernetes /run writing {name} passed")
    public = doc["kube_objects"]["network_policy"]["spec"]["egress"][-1]
    egress_of = lambda o, rules: {**o, "network_policy": {**o["network_policy"], "spec": {**o["network_policy"]["spec"], "egress": rules}}}
    for name, planted, floor in (("public 80/443 beside the gateway with egress", floored["network_policy"]["spec"]["egress"] + [public], True),
                                 ("the gateway on a port other than 3128", floored["network_policy"]["spec"]["egress"][:-1] + [{**floored["network_policy"]["spec"]["egress"][-1], "ports": [{"protocol": "TCP", "port": 443}]}], True),
                                 ("the gateway without egress, in place of public 80/443", floored["network_policy"]["spec"]["egress"], False)):
        if not kube_objects(kube, egress_of(floored, planted), floored=floor):
            errors.append(f"selftest: a Kubernetes /run writing {name} passed")
    signin = doc["kube_signin_policy"]
    to = lambda *cidrs: [{"ipBlock": {"cidr": c}} for c in cidrs]
    with_rule = lambda rule: {**signin, "spec": {**signin["spec"], "egress": signin["spec"]["egress"][:-1] + [{**signin["spec"]["egress"][-1], **rule}]}}
    for name, planted in (("a sign-in range wider than one address", with_rule({"to": to("10.30.0.0/24")})),
                          ("a sign-in address beside a range", with_rule({"to": to("10.30.0.7/32", "10.30.0.0/16")})),
                          ("a sign-in address over IPv6", with_rule({"to": to("fd00::7/128")})),
                          ("a sign-in address on 80 as well as 443", with_rule({"ports": [{"protocol": "TCP", "port": 80}, {"protocol": "TCP", "port": 443}]})),
                          ("a sign-in address on 8443", with_rule({"ports": [{"protocol": "TCP", "port": 8443}]})),
                          ("a sign-in rule to no address, which admits every one", with_rule({"to": []})),
                          ("a sign-in address without its /32", with_rule({"to": to("10.30.0.7")})),
                          ("a sign-in address in link-local, the node's instance metadata", with_rule({"to": to("169.254.169.254/32")})),
                          ("a sign-in port that runs on to an endPort", with_rule({"ports": [{"protocol": "TCP", "port": 443, "endPort": 8443}]})),
                          ("no sign-in address at all", {**signin, "spec": {**signin["spec"], "egress": signin["spec"]["egress"][:-1]}})):
        if not signin_policy(kube, floored, planted):
            errors.append(f"selftest: a Kubernetes /run under CANYON_KUBE_SIGNIN_DIRECT writing {name} passed")


def signin_policy(kube, floored, policy):
    """The floored NetworkPolicy under CANYON_KUBE_SIGNIN_DIRECT (executor.md, Sign-in past the gateway): the
    one without the setting, kube_egress_policy, plus one last rule, TCP 443 to single IPv4 addresses."""
    out = kube_objects(kube, {**floored, "network_policy": policy}, floored=True, signin=True)
    egress, base = policy["spec"]["egress"], floored["network_policy"]["spec"]["egress"]
    if {**policy, "spec": {**policy["spec"], "egress": base}} != floored["network_policy"] or egress[:-1] != base or len(egress) != len(base) + 1:
        out.append("executor: under CANYON_KUBE_SIGNIN_DIRECT the floored NetworkPolicy is kube_egress_policy and one rule more, the sign-in addresses on 443")
    return out


# ---------------------------------------------------------------- seam 5 — the policy snapshot
# The namespace each producer may emit under. Derived from contracts/snapshot.md's rule list;
# a snapshot rule registered outside its producer's namespace is refused.
POLICY_TABLE = policy_table()
PRODUCER_NAMESPACES = {
    "scanner": {"secrets", "sql", "deps", "code", "iac", "design"},  # design: read from the package's manifests (finding.md)
    "promote-gate": {"prod", "platform"},  # platform: the image Canyon builds under each app (design/PATCHING.md §3.3)
    "server-state": {"stack", "owner", "prod"},  # prod: prod.egress-allowed alone, which reads what the server holds (snapshot.md)
    "llm-review": {"perf", "auth", "ai", "custom"},  # ai: the AI review where no scanner reads the language (#1667); custom: an org's custom rules (snapshot.md, Custom rules)
    "sarif": {"sarif"},   # one rule per imported tool, sarif.<tool> (snapshot.md)
}


# snapshot.md: "A new rule registers here and installs `off` — every rule but the nineteen above it". Fixed here,
# never read from D2: a rule added to D2, the fixture and the Go table together at another default agrees with
# all three, so only this list refuses it.
SHIPPED_ON = {"secrets.client-bundle", "sql.string-concat", "prod.clean-gate", "prod.human-approval", "auth.unauthenticated-route",
              "deps.known-vuln", "stack.blessed", "owner.required", "perf.unbounded-refetch", "design.org-system", "code.injection",
              "iac.misconfig", "ai.sql-string-concat", "ai.code-injection", "sarif.checkov", "sarif.eslint", "sarif.gitleaks",
              "sarif.opengrep", "sarif.trivy"}


def d2_rules():
    """The canonical rules as EXPERIENCE.md D2 states them: {id: (name, state)}."""
    text = (ROOT / "design" / "EXPERIENCE.md").read_text()
    section = text.split("## D2 ")[1].split("## D3 ")[0]
    rows = {}
    for line in section.splitlines():
        m = re.match(r"^\|\s*(?:\*\(meta\)\*\s*)?`([a-z.-]+)`\s*\|\s*([^|]+?)\s*\|\s*(Blocking|Advisory|Off)\b", line)
        if m:
            rows[m.group(1)] = (m.group(2), m.group(3).lower())
    return rows


def seam5():
    fixture = CONTRACTS / "testdata" / "snapshot" / "meridian.json"
    snap = load(fixture)
    if not schema_check("snapshot", CONTRACTS / "schema" / "snapshot.schema.json", snap):
        return None
    rules = {r["id"]: r for r in snap["rules"]}
    if len(rules) != len(snap["rules"]):
        errors.append("snapshot: duplicate rule id")

    # 1. The canonical rules, byte-exact against EXPERIENCE D2 — id, English name, demo state.
    canon = d2_rules()
    if len(canon) != 21:
        errors.append(f"snapshot: EXPERIENCE D2 parsed {len(canon)} rules, expected 21")
    for rid, (name, state) in canon.items():
        r = rules.get(rid)
        if r is None:
            errors.append(f"snapshot: D2 rule '{rid}' missing from the fixture")
            continue
        if r["name"] != name:
            errors.append(f"snapshot: '{rid}' name differs from D2: {r['name']!r} vs {name!r}")
        if r["state"] != state:
            errors.append(f"snapshot: '{rid}' state {r['state']} differs from D2 demo default {state}")

    # 2. Every rule but the nineteen installs Off — the data half of "registers and installs off".
    for rid, r in rules.items():
        if rid not in SHIPPED_ON and r["state"] != "off":
            errors.append(f"snapshot: '{rid}' installs as {r['state']}; every rule but the nineteen that shipped on installs off")

    # 3. Namespace-confined per producer.
    for rid, r in rules.items():
        ns = rid.split(".", 1)[0]
        if ns not in PRODUCER_NAMESPACES[r["producer"]]:
            errors.append(f"snapshot: '{rid}' is outside its producer's namespace ({r['producer']}: {sorted(PRODUCER_NAMESPACES[r['producer']])})")

    # 4. An agent may only claim namespaces that llm-review owns.
    for a in snap["agents"]:
        bad = set(a["namespace"]) - PRODUCER_NAMESPACES["llm-review"]
        if bad:
            errors.append(f"snapshot: agent '{a['id']}' claims namespace(s) {sorted(bad)} not owned by llm-review")

    # 5. A Canyon-run tool's rule carries run (only as true) and non-empty settings of its own allow-list.
    tool = lambda rid, **extra: {"id": rid, "name": "Findings from " + rid[6:], "state": "off", "producer": "sarif", **extra}
    for name, rule, ok in (("run and settings on trivy", tool("sarif.trivy", run=True, settings={"scanners": ["vuln"]}), True),
                           ("run spelled false", tool("sarif.trivy", run=False), False),
                           ("empty settings", tool("sarif.grype", settings={}), False),
                           ("another tool's settings", tool("sarif.trivy", settings={"frameworks": ["terraform"]}), False),
                           ("run on a tool Canyon does not run", tool("sarif.semgrep", run=True), False)):
        if accepts(CONTRACTS / "schema" / "snapshot.schema.json", {**snap, "rules": snap["rules"] + [rule]}) != ok:
            errors.append(f"selftest: a snapshot rule with {name} was {'refused' if ok else 'accepted'}")

    # 5b. platform.known-vuln's settings are its source alone, an imported tool, never trivy's own (snapshot.md; #2077).
    for name, rule, ok in (("an imported source", {**rules[PLATFORM_RULE], "settings": {"source": "wiz"}}, True),
                           ("trivy spelled as a source", {**rules[PLATFORM_RULE], "settings": {"source": "trivy"}}, False),
                           ("run on the platform rule", {**rules[PLATFORM_RULE], "run": True}, False),
                           ("a source on another rule", {**rules["deps.known-vuln"], "settings": {"source": "wiz"}}, False)):
        probe = {**snap, "rules": [rule if r["id"] == rule["id"] else r for r in snap["rules"]]}
        if accepts(CONTRACTS / "schema" / "snapshot.schema.json", probe) != ok:
            errors.append(f"selftest: a snapshot rule with {name} was {'refused' if ok else 'accepted'}")

    # 6. Two gates: production only on a rule placed before production, never Off there, never on a go-live rule.
    for name, rule, ok in (("deps.known-vuln blocking before production", {**rules["deps.known-vuln"], "production": "blocking"}, True),
                           ("a go-live rule placed before preview", {**rules["prod.clean-gate"], "production": "advisory"}, False),
                           ("the platform rule placed before preview", {**rules[PLATFORM_RULE], "production": "advisory"}, False),
                           ("the egress rule placed before preview", {**rules[EGRESS_RULE], "production": "advisory"}, False),
                           ("run on the egress rule", {**rules[EGRESS_RULE], "run": True}, False),
                           ("production spelled off", {**rules["deps.known-vuln"], "production": "off"}, False),
                           ("a fourth state before production", {**rules["deps.known-vuln"], "production": "on"}, False)):
        probe = {**snap, "rules": [rule if r["id"] == rule["id"] else r for r in snap["rules"]]}
        if accepts(CONTRACTS / "schema" / "snapshot.schema.json", probe) != ok:
            errors.append(f"selftest: a snapshot rule with {name} was {'refused' if ok else 'accepted'}")
    return snap


RANK = {"off": 0, "advisory": 1, "blocking": 2}


def effective(org, additions):
    """snapshot.md: the org's snapshot with an app's additions ({rule: {state?, production?}}) applied —
    the stricter state per gate, production only on a rule then placed before production."""
    out = copy.deepcopy(org)
    for r in out["rules"]:
        add = additions.get(r["id"], {})
        r["state"] = max(r["state"], add.get("state", "off"), key=RANK.get)
        prod = max(r.get("production", "off"), add.get("production", "off"), key=RANK.get)
        if r["id"] not in GO_LIVE and prod != "off":
            r["production"] = prod
    return out


def effective_snapshot(org):
    """snapshot.md, A project's runs use its effective snapshot: claims-portal's is Meridian's with the
    addition the policies golden applies (app_policy_put), at Meridian's version, under a hash of its own."""
    path = CONTRACTS / "schema" / "snapshot.schema.json"
    snap = load(CONTRACTS / "testdata" / "snapshot" / "claims-portal.json")
    put = load(CONTRACTS / "testdata" / "policies" / "meridian.json")["app_policy_put"]
    if not schema_check("snapshot: claims-portal", path, snap):
        return

    def problems(doc):
        out = []
        want = effective(org, {put["rule"]: put["request"]})
        if {k: v for k, v in doc.items() if k != "hash"} != {k: v for k, v in want.items() if k != "hash"}:
            out.append("it is not Meridian's with claims-portal's addition applied, the stricter state per gate, at Meridian's version")
        if doc["hash"] == org["hash"]:
            out.append("its hash is Meridian's; an app's additions are in its verdict cache's key")
        return out

    for p in problems(snap):
        errors.append(f"snapshot: claims-portal: {p}")
    auth = lambda d: next(r for r in d["rules"] if r["id"] == put["rule"])
    for name, mutate in (("the org's state kept in the gate the addition raised", lambda d: auth(d).pop("production", None)),
                         ("a state before production weaker than the addition's", lambda d: auth(d).update(production="advisory")),
                         ("a version of its own", lambda d: d.update(version=d["version"] + 1)),
                         ("the org's hash", lambda d: d.update(hash=org["hash"]))):
        probe = copy.deepcopy(snap)
        mutate(probe)
        if not problems(probe):
            errors.append(f"selftest: claims-portal's effective snapshot with {name} was not refused")


# ---------------------------------------------------------------- seam 3 — System libraries and build tools (design/PATCHING.md §2, §3)
# Whose each layer is, by the buildpack that wrote it (PATCHING.md §2): a runtime provider's hold what Canyon's
# builder ships, a runtime and what it bundles or a buildpack's own helpers, an installer's the app's own. Every
# other buildpack's is unknown.
PLATFORM_RUNTIMES = {"paketo-buildpacks/" + b for b in ("node-engine", "cpython", "bellsoft-liberica", "go-dist", "ca-certificates", "spring-boot")}
PLATFORM_INSTALLERS = {"paketo-buildpacks/" + b for b in ("npm-install", "yarn-install", "pnpm-install", "pip-install", "pipenv-install",
                                                         "poetry-install", "conda-env-update", "bundle-install", "composer-install",
                                                         "go-mod-vendor", "maven", "gradle")}
PLATFORM_COUNTED = ("os", "runtime", "lifecycle")
PLATFORM_STALE_SECONDS = 5 * 24 * 3600
PLATFORM_NOTES = "no fix published, or Canyon can't tell what put it there"
SEVERITIES = ("critical", "high", "medium", "low")


def purl_parts(purl):
    """A purl's type, namespace and name by the purl spec's own parse (scheme, then the subpath after `#`, the
    qualifiers after `?`, the version after the last `@`, the type before the first `/`, the name the last
    segment), each segment percent-decoded; None for no purl."""
    if not purl or not purl.startswith("pkg:"):
        return None
    rest = purl[len("pkg:"):].split("#", 1)[0].split("?", 1)[0].strip("/")
    typ, _, path = rest.partition("/")
    head, at, _ = path.rpartition("@")
    if at and "/" not in path[len(head):]:
        path = head
    segs = [urllib.parse.unquote(p) for p in path.split("/") if p]
    return typ.lower(), "/".join(segs[:-1]), segs[-1] if segs else ""


def prop(component, name):
    return next((p["value"] for p in component.get("properties", []) if p["name"] == name), None)


def platform_owners(diff_ids, label):
    """Each layer's owner from the image's io.buildpacks.lifecycle.metadata (PATCHING.md §2): the run image's
    layers up to its top one, the lifecycle's launcher and config layers, the app's and the sbom layer (its
    documents skipped, what is listed there the build put), a buildpack's by its
    id, None for a layer the label names twice over or not at all. None for the whole image when no layer can be
    owned: no label, one that does not parse, a top layer not the image's, or no layer the run image's."""
    named = {}
    try:
        meta = json.loads(label) if label else {}
    except ValueError:
        meta = None
    if not isinstance(meta, dict):
        return None
    add = lambda sha, owner: named.setdefault(sha, set()).add(owner)
    top = (meta.get("runImage") or {}).get("topLayer")
    if top in diff_ids:
        for d in diff_ids[:diff_ids.index(top) + 1]:
            add(d, "run-image")
    for key in ("launcher", "config"):
        if (meta.get(key) or {}).get("sha"):
            add(meta[key]["sha"], "lifecycle")
    if (meta.get("sbom") or {}).get("sha"):
        add(meta["sbom"]["sha"], "app")
    for layer in meta.get("app") or []:
        add(layer["sha"], "app")
    for bp in meta.get("buildpacks") or []:
        for layer in (bp.get("layers") or {}).values():
            if layer.get("sha"):
                add(layer["sha"], bp["key"])
    owners = {d: next(iter(named[d])) if len(named.get(d, ())) == 1 else None for d in diff_ids}
    if not label or top not in diff_ids or "run-image" not in owners.values():
        return None
    return owners


def platform_class(owner, pkg_type, purl):
    """PATCHING.md §2: a component's class is f(layer, package type). Go's standard library takes its binary's
    layer's class but in the app's own Go build, where it is go-dist's; an installer's own binary is a runtime's."""
    typ, ns, name = purl_parts(purl) or (None, "", "")
    if owner == "lifecycle":
        return "lifecycle"
    if typ == "deb":
        return "os" if owner == "run-image" else "unknown"
    if owner in PLATFORM_RUNTIMES or (owner in PLATFORM_INSTALLERS and pkg_type == "gobinary"):
        return "runtime"
    if typ == "golang" and ns == "" and name == "stdlib" and owner in ("app", "paketo-buildpacks/go-build"):
        return "runtime"
    if owner == "app" or owner in PLATFORM_INSTALLERS:
        return "app"
    return "unknown"


def ts_seconds(ts):
    return datetime.datetime.strptime(ts, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=datetime.timezone.utc).timestamp()


def platform_read(doc):
    """routes.md's image read and receipt row, derived from the fixture's image answer and re-matches at its
    read_at under its rule state (finding.md, System libraries and build tools): (read, row)."""
    img, read_at, state = doc["image"], doc["read_at"], doc["rule_state"]
    diff_ids = img["diff_ids"]
    owners = platform_owners(diff_ids, img.get("lifecycle_metadata"))
    label_reads = owners is not None
    owners = owners or {}
    packages = []  # (component, layer, class)
    for c in img["sbom"]["components"]:
        if c["type"] != "library":
            continue
        layer = prop(c, "aquasecurity:trivy:LayerDiffID")
        cls = platform_class(owners.get(layer), prop(c, "aquasecurity:trivy:PkgType"), c.get("purl")) if layer in owners else "unknown"
        packages.append((c, layer if layer in owners else None, cls))
    covered = [d for d in diff_ids if any(layer == d and cls != "app" for _, layer, cls in packages)]
    newest = {}
    for m in doc["matches"]:
        if m["diff_id"] not in newest or m["answer"]["db_built_at"] > newest[m["diff_id"]]["answer"]["db_built_at"]:
            newest[m["diff_id"]] = m
    name_of = lambda c: (c["group"] + "/" if c.get("group") else "") + c["name"]
    findings, notes, why = [], set(), None
    if not label_reads:
        why = "could not check: a stored scan of the image does not read"
    elif not covered:  # no complete report says the image is clean: it lists nothing of Canyon's
        why = "could not check: the image scan listed no package Canyon put there"
    for d in covered:
        m = newest.get(d)
        if m is None:
            why = why or "could not check: the image has not been matched yet"
        elif ts_seconds(m["answer"]["db_built_at"]) > ts_seconds(read_at):
            why = why or "could not check: a stored scan of the image does not read"
        elif ts_seconds(read_at) - ts_seconds(m["answer"]["db_built_at"]) > PLATFORM_STALE_SECONDS:
            why = why or "could not check: its vulnerability database is over 5 days old"
    if why is None:
        for d in covered:
            answer = newest[d]["answer"]
            mine = {c["bom-ref"]: (c, cls) for c, layer, cls in packages if layer == d}
            for c, cls in mine.values():
                if cls != "app" and (not c.get("purl") or c["bom-ref"] in answer.get("unscanned", [])):
                    notes.add((name_of(c), c.get("version", ""), d, cls, "not_checked"))
            for v in (v for r in answer["report"]["Results"] for v in r.get("Vulnerabilities", [])):
                ref = v.get("PkgIdentifier", {}).get("BOMRef")
                if ref not in mine or v.get("Layer", {}).get("DiffID") != d:
                    why = "could not check: a match names a package the image does not hold"
                    break
                c, cls = mine[ref]
                if cls == "app":
                    continue
                if cls not in PLATFORM_COUNTED:
                    notes.add((name_of(c), c["version"], d, cls, "unknown"))
                elif not v.get("FixedVersion"):
                    notes.add((name_of(c), c["version"], d, cls, "no_fix"))
                else:
                    findings.append(platform_finding(v, c, name_of(c), cls, d, owners.get(d), state))
        for c, layer, cls in packages:
            if layer is None:
                notes.add((name_of(c), c.get("version", ""), None, cls, "not_checked"))
    db_built_at = min((newest[d]["answer"]["db_built_at"] for d in covered if d in newest), default=None)
    if why is not None:
        findings, notes = [], set()
    rank = {s: n for n, s in enumerate(SEVERITIES)}
    findings.sort(key=lambda f: (rank[f["properties"]["canyon"]["severity"]], f["dependency"]["name"], f["dependency"]["version"], f["dependency"]["vulnerability"]))
    position = {d: n for n, d in enumerate(diff_ids)}
    note_rows = [{"name": n, "version": v, "layer": l, "class": c, "why": w}
                 for n, v, l, c, w in sorted(notes, key=lambda t: (position.get(t[2], len(diff_ids)), t[0], t[1], t[4]))]
    counted = len({(f["dependency"]["name"], f["dependency"]["version"]) for f in findings if f["properties"]["canyon"]["severity"] in ("critical", "high")})
    if why is None:
        why = f"{counted} known-vulnerable package{'' if counted == 1 else 's'} with a published fix, in the system libraries and build tools Canyon adds" if counted else "no known-vulnerable package with a published fix, in the system libraries and build tools Canyon adds"
        if note_rows:
            why += f"; {len(note_rows)} note{'' if len(note_rows) == 1 else 's'}: {PLATFORM_NOTES}"
        status = "checked"
    else:
        status = "could_not_check"
    read = {"env": doc["env"], "image_digest": doc["image_digest"], "state": status, "why": why, "scanned_at": doc["scanned_at"],
            "db_built_at": db_built_at, "read_at": read_at, "layers": [{"diff_id": d, "owner": owners.get(d)} for d in diff_ids],
            "findings": findings, "notes": note_rows}
    row = {"id": PLATFORM_RULE, "name": doc["rule_name"], "blocks": state == "blocking", "passed": status == "checked" and counted == 0, "why": why}
    if db_built_at is not None and status == "checked":
        row.update(db_built_at=db_built_at, read_at=read_at, notes=len(note_rows))
        stdlibs = {(name_of(c), c.get("version")) for c, _, _ in packages if purl_parts(c.get("purl")) == ("golang", "", "stdlib")}
        packages, held = {}, {}  # the counted packages in the findings' order, each advisory once whatever layers hold it
        for f in findings:
            if f["properties"]["canyon"]["severity"] in ("critical", "high"):
                dep = f["dependency"]
                key = (dep["name"], dep["version"])
                advisories = packages.setdefault(key, [])
                if all(a["id"] != dep["vulnerability"] for a in advisories):
                    advisories.append({"id": dep["vulnerability"], "fixed": dep["fixed"]})
                held.setdefault(key, set()).add((position[dep["layer"]], owners.get(dep["layer"]), f["class"]))
        if packages:
            # #2579: where each is, the layers holding it bottom first, each owner and class once, and whether it is Go's standard library
            row["packages"] = []
            for (n, v), a in packages.items():
                layers = []
                for _, owner, cls in sorted(held[(n, v)], key=lambda h: h[0]):
                    if {"owner": owner, "class": cls} not in layers:
                        layers.append({"owner": owner, "class": cls})
                one = {"name": n, "version": v, "advisories": a, "layers": layers}
                if (n, v) in stdlibs:
                    one["go_stdlib"] = True
                row["packages"].append(one)
    return read, row


# Where a counted package is, in a problem's words: never the class's own name (finding.md, its Texts).
PLATFORM_WHERE = {"os": "system libraries", "runtime": "build tools", "lifecycle": "build tools"}


def platform_finding(v, c, name, cls, layer, owner, state):
    """finding.md, System libraries and build tools: one counted vulnerability as a profile result on the image read."""
    sev = (v.get("Severity") or "").lower()
    sev = sev if sev in SEVERITIES else "high"
    vuln, version, fixed = v["VulnerabilityID"], c["version"], v["FixedVersion"]
    uri = "var/lib/dpkg/status" if (purl_parts(c.get("purl")) or ("",))[0] == "deb" else prop(c, "aquasecurity:trivy:FilePath") or "."
    fix = {"os": f"Canyon fixes this: it updates {name} to {fixed} under the app. The app is not rebuilt.",
           "runtime": f"Canyon fixes this in its next build tools update, with a newer {owner if owner in PLATFORM_RUNTIMES or owner in PLATFORM_INSTALLERS else 'paketo-buildpacks/go-dist'}.",
           "lifecycle": "Canyon fixes this in its next build tools update, with a newer app launcher."}[cls]
    blocks = state == "blocking" and sev in ("critical", "high")
    title = f"{vuln} in {name} {version}"[:120]
    canyon = {"severity": sev, "blocks": blocks, "title": title,
              "problem": f"{vuln} is published against {name} {version}, which Canyon put in this app's {PLATFORM_WHERE[cls]}. {fixed} {'fix' if ', ' in fixed else 'fixes'} it.",
              "fix": fix, "policy_name": "No known-vulnerable system libraries or build tools"}
    if blocks:
        canyon.update(required_end_state=f"The image preview runs holds {name} at a version that fixes {vuln}.",
                      do_not=["change the app's code or dependencies for it"])
    dep = {"name": name, "version": version, "vulnerability": vuln, "fixed": fixed}
    if re.fullmatch(r"https://[!-(*-?A-~]+", v.get("PrimaryURL") or ""):
        dep["link"] = v["PrimaryURL"]
    dep["layer"] = layer
    return {"ruleId": PLATFORM_RULE, "level": LEVEL[sev], "message": {"text": title},
            "locations": [{"physicalLocation": {"artifactLocation": {"uri": uri, "uriBaseId": "IMAGE"},
                                                "region": {"startLine": 1, "startColumn": 1, "endLine": 1, "endColumn": 1 + len(f"{name} {version}"), "snippet": {"text": f"{name} {version}"}}}}],
            "partialFingerprints": {"canyon/v1": framed(PLATFORM_RULE, vuln, name, version, cls)},
            "properties": {"canyon": canyon}, "tool": "canyon/promote-gate", "class": cls, "dependency": dep}


def platform_row_errors(label, row):
    """What any platform.known-vuln row must hold, whoever derived it: a match older than 5 days never passes
    (routes.md, The promote receipt)."""
    if row["id"] == PLATFORM_RULE and row["passed"] and ("db_built_at" not in row or ts_seconds(row["read_at"]) - ts_seconds(row["db_built_at"]) > PLATFORM_STALE_SECONDS):
        return [f"app-page: {label} passes {PLATFORM_RULE} on a database built {row.get('db_built_at')}, read {row.get('read_at')}: a match over 5 days old never passes"]
    return []


def seam3_platform(snap):
    """finding.md, System libraries and build tools, and routes.md's image read and receipt row: the fixture's image answer and
    re-matches are the executor's shapes, its read and row what they derive, and each finding a profile result
    under platform.known-vuln alone."""
    path = CONTRACTS / "testdata" / "platform" / "claims-portal.json"
    doc = load(path)
    app_page, executor, finding = (CONTRACTS / "schema" / f"{n}.schema.json" for n in ("app-page", "executor", "finding"))
    if doc is None:
        return
    ok = definition_check("platform: image", executor, "scan_image_result", doc["image"])
    for m in doc["matches"]:
        ok = definition_check(f"platform: match of {m['diff_id']}", executor, "scan_sbom_result", m["answer"]) and ok
        errors.extend(f"platform: {e}" for e in image_layer_errors(doc["image"], m["answer"]))
    ok = definition_check("platform: image read", app_page, "image_read", doc["image_read"]) and ok
    ok = definition_check("platform: receipt row", app_page, "receipt", {"org": "Meridian", "blocked_by": [], "rules": [doc["receipt_row"]] * 4}) and ok
    if not ok:
        return
    rule = next(r for r in snap["rules"] if r["id"] == PLATFORM_RULE)
    if doc["rule_name"] != rule["name"]:
        errors.append(f"platform: the fixture names the rule {doc['rule_name']!r}; the snapshot says {rule['name']!r}")
    log = lambda results: {"version": "2.1.0", "$schema": "https://json.schemastore.org/sarif-2.1.0.json", "runs": [{"columnKind": "unicodeCodePoints",
            "tool": {"driver": {"name": "canyon/promote-gate", "version": "1", "rules": [{"id": PLATFORM_RULE, "name": rule["name"]}]}},
            "results": [{k: v for k, v in r.items() if k not in ("tool", "class", "dependency")} for r in results]}]}
    schema_check("platform: image read findings as a log", finding, log(doc["image_read"]["findings"]))

    def problems(d):
        read, row = platform_read(d)
        out = platform_row_errors("platform receipt row", d["receipt_row"])
        if d["image_read"] != read:
            diff = next((k for k in read if d["image_read"].get(k) != read[k]), "?")
            out.append(f"platform: the image read's {diff} is not what the image answer and its re-matches derive: {json.dumps(read.get(diff))[:600]}")
        if d["receipt_row"] != row:
            out.append(f"platform: the receipt row is {d['receipt_row']}; the image read gives {row}")
        return out

    errors.extend(problems(doc))
    read = doc["image_read"]
    if not read["findings"] or not any(n["why"] == "not_checked" for n in read["notes"]) or not any(n["why"] == "no_fix" for n in read["notes"]):
        errors.append("selftest: the platform fixture has no finding, no not_checked note or no no_fix note, so the checks below check nothing")
        return
    first, purl_less = read["findings"][0], next(n for n in read["notes"] if n["why"] == "not_checked")
    later = "2026-10-19T09:00:01Z"  # the fixture's oldest match, built 2026-10-13T09:00:00Z, is then over 5 days old
    stale = {"read_at": later, "image_read": {**doc["image_read"], "read_at": later}, "receipt_row": {**doc["receipt_row"], "read_at": later}}
    for name, mutate in (
            ("a purl-less component read as clean", lambda d: d["image_read"].update(notes=[n for n in d["image_read"]["notes"] if n != purl_less])),
            ("a finding with no fix counted", lambda d: next(v for m in d["matches"] for r in m["answer"]["report"]["Results"] for v in r.get("Vulnerabilities", [])
                                                                if v["VulnerabilityID"] == first["dependency"]["vulnerability"]).pop("FixedVersion")),
            ("an older match read instead of the newest", lambda d: d["matches"].remove(max((m for m in d["matches"] if sum(n["diff_id"] == m["diff_id"] for n in d["matches"]) > 1),
                                                                                             key=lambda m: m["answer"]["db_built_at"]))),
            ("npm-install as a runtime provider", lambda d: PLATFORM_RUNTIMES.add("paketo-buildpacks/npm-install")),
            ("a lifecycle label that does not parse", lambda d: d["image"].update(lifecycle_metadata='{"runImage":')),
            ("an image with no lifecycle label", lambda d: d["image"].pop("lifecycle_metadata")),
            ("a run image's top layer that is not the image's", lambda d: d["image"].update(lifecycle_metadata=d["image"]["lifecycle_metadata"].replace('"topLayer":"sha256:2222', '"topLayer":"sha256:9222'))),
            ("a database built after the read", lambda d: d["matches"][0]["answer"].update(db_built_at="2026-10-15T09:00:01Z")),
            ("an image scan listing nothing of Canyon's", lambda d: d["image"]["sbom"].update(components=[c for c in d["image"]["sbom"]["components"] if c["type"] != "library"])),
            ("a 6-day-old match read as current", lambda d: d.update(stale))):
        probe = copy.deepcopy(doc)
        mutate(probe)
        found = problems(probe)
        PLATFORM_RUNTIMES.discard("paketo-buildpacks/npm-install")
        if not found:
            errors.append(f"selftest: the platform fixture with {name} was not refused")
    if not platform_row_errors("selftest", {**doc["receipt_row"], "passed": True, "why": "no known-vulnerable package with a published fix, in the system libraries and build tools Canyon adds", "read_at": later}):
        errors.append("selftest: a receipt row that passes on a 6-day-old match was not refused")
    image_elsewhere = copy.deepcopy(first)
    image_elsewhere["ruleId"] = "deps.known-vuln"
    bare = copy.deepcopy(first)
    del bare["locations"][0]["physicalLocation"]["artifactLocation"]["uriBaseId"]
    for name, result in (("an IMAGE finding under deps.known-vuln", image_elsewhere), ("a platform.known-vuln finding without IMAGE", bare)):
        if accepts(finding, log([result])):
            errors.append(f"selftest: a finding log with {name} was accepted")
    for name, row in (("platform fields on another rule", {"id": "stack.blessed", "name": "Approved stacks only", "blocks": False, "passed": True, "why": "built with Node.js",
                                                         "db_built_at": "2026-10-13T09:00:00Z", "read_at": "2026-10-15T09:00:00Z", "notes": 0}),
                      ("a database date without when it was read", {k: v for k, v in doc["receipt_row"].items() if k != "read_at"})):
        if takes(app_page, "receipt", {"org": "Meridian", "blocked_by": [], "rules": [row] * 4}):
            errors.append(f"selftest: a receipt row with {name} was accepted")


# finding.md, *The built app*: what deps.known-vuln's row before production counts of the image preview runs.
DEPS_RULE = "deps.known-vuln"
DEPS_FILES = {"package-lock.json": "npm", "yarn.lock": "npm", "pnpm-lock.yaml": "npm", "bun.lock": "npm", "package.json": "npm",
              "requirements.txt": "pypi", "Pipfile.lock": "pypi", "poetry.lock": "pypi", "uv.lock": "pypi", "pyproject.toml": "pypi",
              "pom.xml": "maven", "gradle.lockfile": "maven", "go.mod": "golang", "go.sum": "golang", "Gemfile.lock": "gem",
              "composer.lock": "composer", "Cargo.lock": "cargo", "packages.lock.json": "nuget"}
DEPS_ARCHIVES = (".jar", ".war", ".ear")
BUILT_APP_CLASSES = ("app", "unknown")


def package_key(typ, name, version):
    """A package as the built app compares it with what the review reported: its purl type, name and version, a
    pypi name as its spec normalises it."""
    if typ == "pypi":
        name = re.sub(r"[-_.]+", "-", name).lower()
    return (typ, name, version)


def key_of_component(c):
    typ, ns, name = purl_parts(c.get("purl"))
    full = f"{ns}:{name}" if typ == "maven" and ns else f"{ns}/{name}" if ns else name
    return package_key(typ, full, c.get("version", ""))


def key_of_reported(result):
    """A review finding's package: its dependency's name and version, its file's purl type by the file's name alone;
    None when the file names none, so the finding stands for no package of the image."""
    dep = dependency_of({**result, "tool": "sarif/trivy"})
    uri = result["locations"][0]["physicalLocation"]["artifactLocation"]["uri"] if result.get("locations") else ""
    base = uri.rsplit("/", 1)[-1]
    typ = DEPS_FILES.get(base) or ("maven" if base.endswith(DEPS_ARCHIVES) else None)
    if dep is None or typ is None:
        return None
    return package_key(typ, dep["name"], dep["version"])


def built_app_row(doc):
    """routes.md, The promote receipt, deps.known-vuln's row reads the built app too: (passed, why) of a row whose
    review half passes with doc's review_why, no approval covering any package."""
    return built_app_read(doc)[:2]


def built_app_read(doc, approved=frozenset()):
    """built_app_row's (passed, why), with each package whose every fingerprint (finding.md, The built app,
    Fingerprint) is in approved approved rather than counted, and the row's built_app listing (#2610)."""
    v, passed = doc["version"], doc["review_why"]
    cut = lambda s: s[:200]
    base, _ = platform_read({**doc, "rule_state": "off", "rule_name": "-"})
    if base["state"] != "checked":
        return False, cut(f"{v}'s built app could not be checked: {base['why'].removeprefix('could not check: ')}"), None
    img, read_at = doc["image"], doc["read_at"]
    diff_ids = img["diff_ids"]
    owners = platform_owners(diff_ids, img.get("lifecycle_metadata"))
    layer_of = lambda c: prop(c, "aquasecurity:trivy:LayerDiffID")
    packages = [(c, layer_of(c), platform_class(owners.get(layer_of(c)), prop(c, "aquasecurity:trivy:PkgType"), c.get("purl")))
                for c in img["sbom"]["components"] if c["type"] == "library" and layer_of(c) in owners]
    layers = [d for d in diff_ids if any(layer == d and cls in BUILT_APP_CLASSES for _, layer, cls in packages)]
    newest = {}
    for m in doc["matches"]:
        if m["diff_id"] not in newest or m["answer"]["db_built_at"] > newest[m["diff_id"]]["answer"]["db_built_at"]:
            newest[m["diff_id"]] = m
    why = None
    for d in layers:
        m = newest.get(d)
        if m is None:
            why = why or "the image has not been matched yet"
        elif ts_seconds(m["answer"]["db_built_at"]) > ts_seconds(read_at):
            why = why or "a stored scan of the image does not read"
        elif ts_seconds(read_at) - ts_seconds(m["answer"]["db_built_at"]) > PLATFORM_STALE_SECONDS:
            why = why or "its vulnerability database is over 5 days old"
    if why:
        return False, cut(f"{v}'s built app could not be checked: {why}"), None
    reported = {k for k in (key_of_reported(r) for r in doc["reported"] if r["ruleId"] == DEPS_RULE or r["ruleId"].startswith(DEPS_RULE + ".")) if k}
    counted, unchecked = {}, set()  # (name, version) -> [its most severe rank, {advisory: fixed}, [its fingerprints]]
    rank = {s: n for n, s in enumerate(SEVERITIES)}
    for d in layers:
        answer = newest[d]["answer"]
        held = {c["bom-ref"] for c, layer, _ in packages if layer == d}
        mine = {c["bom-ref"]: c for c, layer, cls in packages if layer == d and cls in BUILT_APP_CLASSES}
        class_of = {c["bom-ref"]: cls for c, layer, cls in packages if layer == d and cls in BUILT_APP_CLASSES}
        for ref, c in mine.items():
            if not c.get("purl") or ref in answer.get("unscanned", []):
                unchecked.add((ref, d))
        for vul in (x for r in answer["report"]["Results"] for x in r.get("Vulnerabilities", [])):
            ref = vul.get("PkgIdentifier", {}).get("BOMRef")
            if vul.get("Layer", {}).get("DiffID") != d or ref not in held:
                return False, cut(f"{v}'s built app could not be checked: a match names a package the image does not hold"), None
            c = mine.get(ref)
            sev = (vul.get("Severity") or "").lower()
            sev = sev if sev in SEVERITIES else "high"
            if c is None or (ref, d) in unchecked or sev not in ("critical", "high") or key_of_component(c) in reported:
                continue
            name = (c["group"] + "/" if c.get("group") else "") + c["name"]
            one = counted.setdefault((name, c["version"]), [rank[sev], {}, []])
            one[0] = min(one[0], rank[sev])
            one[1].setdefault(vul["VulnerabilityID"], vul.get("FixedVersion", ""))
            fp = framed(DEPS_RULE, vul["VulnerabilityID"], name, c["version"], class_of[ref])
            if fp not in one[2]:
                one[2].append(fp)
    for c in img["sbom"]["components"]:  # a package trivy names no layer of the image for is never matched: not checked
        if c["type"] == "library" and layer_of(c) not in owners:
            unchecked.add((c["bom-ref"], None))
    ranked = sorted(counted, key=lambda k: (counted[k][0], k[0], k[1]))
    covered = [k for k in ranked if set(counted[k][2]) <= approved]  # approved whole, never by one advisory of several
    order = [k for k in ranked if k not in covered]
    listing = [{"name": k[0], "version": k[1], "advisories": [{"id": i, "fixed": f} for i, f in counted[k][1].items()], "fingerprints": counted[k][2],
                "approved": k in covered} for k in order + covered] or None
    if not order:
        why = f"{passed}; its built app holds no known-vulnerable package its review didn't see"
        if covered:
            why = f"{passed}; its built app's known-vulnerable packages its review didn't see are approved: {covered[0][0]} {covered[0][1]}"
            why += f" and {len(covered) - 1} more" if len(covered) > 1 else ""
        if unchecked:
            why += f"; Canyon couldn't check {len(unchecked)} package{'' if len(unchecked) == 1 else 's'} in it"
        return True, cut(why), listing
    name, version = order[0]
    if len(order) > 1:
        return False, cut(f"{len(order)} known-vulnerable packages are in {v}'s built app, but its review didn't see them: {name} {version} and {len(order) - 1} more"), listing
    advisories = counted[order[0]][1]
    if len(advisories) > 1:
        fix = f"{len(advisories)} advisories are published against it"
    else:
        vid, fixed = next(iter(advisories.items()))
        fix = f"{fixed} {'fix' if ', ' in fixed else 'fixes'} it" if fixed else f"no fix is published for {vid}"
    return False, cut(f"{name} {version} is in {v}'s built app, but its review didn't see it: {fix}"), listing


def seam3_built_app():
    """finding.md, The built app, and routes.md's deps.known-vuln row: the fixture's image answer and re-matches are
    the executor's shapes, and its row is what they and the review's findings derive."""
    doc = load(CONTRACTS / "testdata" / "platform" / "built-app.json")
    if doc is None:
        return
    executor = CONTRACTS / "schema" / "executor.schema.json"
    ok = definition_check("built app: image", executor, "scan_image_result", doc["image"])
    for m in doc["matches"]:
        ok = definition_check(f"built app: match of {m['diff_id']}", executor, "scan_sbom_result", m["answer"]) and ok
        errors.extend(f"built app: {e}" for e in image_layer_errors(doc["image"], m["answer"]))
    if not ok:
        return
    want, got = (doc["row"]["passed"], doc["row"]["why"]), built_app_row(doc)
    if got != want:
        errors.append(f"built app: the row is {want}; the image, its re-matches and the review's findings give {got}")
    app_layer = "sha256:" + "6" * 64
    vulns = lambda d: (v for m in d["matches"] for r in m["answer"]["report"]["Results"] for v in r.get("Vulnerabilities", []))
    respelled = {**copy.deepcopy(doc["reported"][0]), "locations": [{"physicalLocation": {"artifactLocation": {"uri": "requirements.txt"}}}],
                 "properties": {"canyon": {"severity": "critical", "problem": "Package: URLlib3\nInstalled Version: 1.26.5\nVulnerability CVE-2023-43804"}}}
    for name, mutate in (
            ("a package the review reported counted again", lambda d: d.update(reported=[])),
            ("an unknown package left uncounted", lambda d: next(v for v in vulns(d) if v["VulnerabilityID"] == "CVE-2023-43804").update(Severity="LOW")),
            ("a Python package the review named in another spelling counted again", lambda d: d["reported"].append(respelled)),
            ("the app's own layer not matched", lambda d: d.update(matches=[m for m in d["matches"] if m["diff_id"] != app_layer])),
            ("a 6-day-old match of the app's own layer read as current",
             lambda d: next(m for m in d["matches"] if m["diff_id"] == app_layer)["answer"].update(db_built_at="2026-10-09T08:59:59Z"))):
        probe = copy.deepcopy(doc)
        mutate(probe)
        if built_app_row(probe) == want:
            errors.append(f"selftest: the built-app fixture with {name} was not refused")
    # a clean built app names what it could not check, a package trivy named no layer for among them
    clean = copy.deepcopy(doc)
    for m in clean["matches"]:
        for r in m["answer"]["report"]["Results"]:
            r.pop("Vulnerabilities", None)
    placed = copy.deepcopy(clean)
    placed["image"]["sbom"]["components"] = [c for c in placed["image"]["sbom"]["components"] if c["bom-ref"] != "pkg:npm/left-pad@1.3.0"]
    if not built_app_row(clean)[1].endswith("; Canyon couldn't check 3 packages in it") or built_app_row(placed) == built_app_row(clean):
        errors.append(f"selftest: the built-app fixture's package with no layer is not named as not checked: {built_app_row(clean)}")


def seam3_built_app_approval():
    """routes.md, The promote receipt and the approve rows; finding.md, The built app (#2610): a package only the
    built app holds, approved through the dependency's approve by its fingerprints. The body names one package the
    row lists, the answer approves each under deps.known-vuln, one audit row names the package from the promote
    receipt, and the row with that approval, and with every package's, is what built-app.json derives."""
    built = load(CONTRACTS / "testdata" / "platform" / "built-app.json")
    doc = load(CONTRACTS / "testdata" / "platform" / "built-app-approval.json")
    if built is None or doc is None:
        return
    schema = CONTRACTS / "schema" / "app-page.schema.json"
    shapes = (("approve_findings", "approve_findings_request"), ("approved_findings", "approved_findings"), ("audit", "event"))
    ok = all([definition_check(f"built app approval: {k}", schema, d, doc[k]) for k, d in shapes])
    rule = {"$ref": f"{load(schema)['$id']}#/definitions/receipt/properties/rules/items"}
    for e in Draft7Validator(rule, registry=SCHEMAS).iter_errors(doc["row"]):
        errors.append(f"built app approval: row: {e.message}")
        ok = False
    if not ok:
        return
    body, answer, row = doc["approve_findings"], doc["approved_findings"], doc["audit"]
    _, _, before = built_app_read(built)
    named = [p for p in before or [] if set(body["fingerprints"]) & set(p["fingerprints"])]
    if len(named) != 1 or set(body["fingerprints"]) != set(named[0]["fingerprints"]):
        errors.append("built app approval: the body does not name every fingerprint of one package the row lists")
        return
    want = [{"rule": DEPS_RULE, "fingerprint": fp, "reason": body["reason"], "expires_at": body["expires_at"]} for fp in body["fingerprints"]]
    if [{k: a[k] for k in ("rule", "fingerprint", "reason", "expires_at")} for a in answer["approvals"]] != want:
        errors.append("built app approval: the answer is not one approval under deps.known-vuln per fingerprint, in the body's order")
    if row["action"] != "POST /api/v1/gate-runs/{run}/findings/approve" or row["actor"] != answer["approvals"][0]["approved_by"] or \
            {k: v for k, v in row["detail"].items() if k != "author"} != {"tool": "canyon/promote-gate", "dependency": {"name": named[0]["name"], "version": named[0]["version"]},
                                                                          "findings": [{k: w[k] for k in ("rule", "fingerprint")} for w in want], "reason": body["reason"], "expires_at": body["expires_at"]}:
        errors.append(f"built app approval: the audit row {row['detail']} is not the dependency's approve naming the package from the promote receipt")
    errors.extend(miscategorised("built app approval", [row]))
    passed, why, listing = built_app_read(built, frozenset(body["fingerprints"]))
    if (doc["row"]["passed"], doc["row"]["why"], doc["row"]["built_app"]) != (passed, why, listing):
        errors.append(f"built app approval: the row is {doc['row']['passed']} {doc['row']['why']!r}; built-app.json with the approval gives {passed} {why!r} {listing}")
    every = frozenset(fp for p in before for fp in p["fingerprints"])
    if (doc["all_approved"]["passed"], doc["all_approved"]["why"]) != built_app_read(built, every)[:2]:
        errors.append(f"built app approval: every package approved gives {built_app_read(built, every)[:2]}, not {doc['all_approved']}")


# routes.md, *Patch runs*, *Its lines*: every build.log row a patch run writes, byte for byte, by name.
PATCH_LINES = {
    "proof": "Only the system libraries Canyon adds changed: <version>'s own code, its build settings and how it starts are the same",
    "layers": "More than Canyon's system libraries changed: <what> changed too, so Canyon did not deploy it",
    "refused": "Canyon can't patch <version><why>, so its next deploy picks up the fix",
    "unread": "Canyon could not check the image <version> runs for vulnerabilities: <reason>, so it did not deploy the patch",
    "unrecorded": "Preview runs the patched <version>, but Canyon could not record it: deploy again so Canyon records what runs there",
    "unfinished": "Canyon could not finish patching <version>, so it did not deploy it",
    "rebase-failed": "Canyon could not apply its fix to <version>: <error>, so it did not deploy it",
    "moved": "A newer fix from Canyon came out while it patched <version>, so Canyon did not deploy it: Canyon tries again at its next check",
    "unchecked": "Canyon could not check the patched image for vulnerabilities: <reason>, so it did not deploy it",
    "unchanged": "What <version> runs with is unchanged since it was deployed",
    "changed": "<what> changed since <version> was deployed: deploy again to apply it, then Canyon patches.",
    "regressed": "The patched image has <name> <installed> (<vulnerability>, <severity>), which <version> did not: Canyon did not deploy it",
    "ineffective": "The patched image still has every package the patch was for: Canyon did not deploy it",
    "fixed": "Fixed: <name> <from> → <to> (<vulnerability>)",
    "no-new": "No new vulnerability at high or above",
    "watching": "Watching <env> until <HH:MM> UTC: if the patched app exits or restarts, Canyon puts back the image it replaced",
    "passed": "The watch passed: the patched app neither exited nor restarted",
    "ended": "The watch ended early: <who> <how>",
    "blind": "Canyon could not tell whether the patched app runs, so the patch goes no further: it runs in <env> as deployed",
    "reverted": "Canyon reverted <env> at <HH:MM> UTC: the patched app <how>, so the image it replaced runs again",
    "superseded": "The watch ended: <env> runs another version now, so nothing was put back",
    "revert-failed": "Canyon could not put back the image the patch replaced: <reason>. The patched image stays in <env>: deploy again to replace it",
    "same": "Rebuilt with <newer>: <version>'s code, build settings and dependencies are the same",
    "not-same": "<what> changed when Canyon rebuilt <version>, so it did not deploy it",
    "not-pinned": "Its dependencies changed when Canyon rebuilt <version> (<change>), so it did not deploy it: pin them in a lockfile, then deploy again",
    "rotated": "Canyon can't compare <version>'s build settings: Canyon's key for build settings changed since <version> was built, so it did not deploy the rebuild",
    "build-failed": "Canyon could not rebuild <version>: <error>, so it did not deploy it",
    "rebuild-ineffective": "Canyon rebuilt <version> with <newer>, but the rebuild fixes none of its findings, so it did not deploy it",
}
# routes.md, *A rebuild patch*: what <newer> reads, by the first runtime's buildpack among those that ran.
REBUILD_NEWER = ("a newer Node.js", "a newer Python", "a newer Java", "a newer Go", "newer build tools")
REBUILD_PROOF = "Rebuilt from the same code and the same dependencies with <newer>. A rebuild is a new program, not a copy of what production runs, so a person on your team decides."
CANYON_PATCH = {"kind": "canyon", "subject": "patch", "display": "Canyon"}


def patch_lines_in_routes():
    """The *Its lines* list of routes.md's *Patch runs*, name → line, as written."""
    section = (CONTRACTS / "routes.md").read_text().split("### Patch runs")[1].split("\n### ")[0]
    return dict(re.findall(r"^  - ([a-z-]+): `([^`]+)`", section.split("**Its lines**")[1], re.M))


def plan_causes_in_routes():
    """The causes the identities row's plan names, as routes.md writes them, (why it read no app, each app's): each
    backticked word of the plan's part of the row before its apps, then after, beside the schema's two enums."""
    row = next(l for l in (CONTRACTS / "routes.md").read_text().splitlines() if l.startswith("| `GET /api/v1/patching/identities` |"))
    plan = row.split("**`plan`**")[1].split(" · a read")[0]
    words = lambda text: set(re.findall(r"`([a-z_]+)`", text)) - {"null", "at", "identity", "id", "why", "apps", "project", "on", "off", "patching", "high"}
    none, apps = plan.split("; `apps` each app the plan read")
    return words(none.split("only when it read no app")[1]), words(apps)


def plan_problems(read, ident, app):
    """routes.md, the identities row's plan (#2588), what its schema cannot say: a plan that read no app names none and
    names apps only onto an identity the read lists, at or after it was planned, each app once, sorted; a plan that read
    apps, the fixture's tick after its patch passed, says its app's patch passed."""
    plan, out = read["plan"], []
    if plan is None:
        return out
    apps = [a["project"] for a in plan["apps"]]
    if "why" in plan and apps:
        out.append(f"patch: the plan read no app ({plan['why']}), yet names {apps}")
    if apps and (plan["identity"] is None or "why" in plan):
        out.append("patch: the plan names apps but no identity it planned onto")
    if plan["identity"] is not None and plan["identity"] not in {i["id"] for i in read["identities"]}:
        out.append(f"patch: the plan names {plan['identity']}, an identity the read does not list")
    if plan["identity"] == ident["id"] and ts_seconds(plan["at"]) < ts_seconds(ident["planned_at"]):
        out.append(f"patch: the plan at {plan['at']} came before its identity was planned, {ident['planned_at']}")
    if apps != sorted(set(apps)):
        out.append(f"patch: the plan's apps are not each once, sorted by project: {apps}")
    if "why" not in plan and next((a["why"] for a in plan["apps"] if a["project"] == app), None) != "passed":
        out.append(f"patch: the plan after {app}'s watch passed does not say its patch passed")
    return out


def fixed_lines(fixes):
    """routes.md's line *fixed*, Patch runs step 6: one per package, by name, from and to, its vulnerabilities in order."""
    moves = {}
    for f in fixes:
        moves.setdefault((f["name"], f["from"], f["to"]), []).append(f["vulnerability"])
    return [{"name": n, "from": a, "to": b, "vulnerability": ", ".join(v)} for (n, a, b), v in moves.items()]


def patch_line(key, **fill):
    """A patch line by its name, its placeholders filled from fill."""
    text = PATCH_LINES[key]
    for k, v in fill.items():
        text = text.replace(f"<{k}>", v)
    return text


def patch_line_fill(text):
    """The patch line a build.log row is and what fills its placeholders, (name, {placeholder: [values]}), or
    (None, {}): a placeholder reads as any text here, and patch_case_problems binds the ones a run's rows say."""
    for name, line in PATCH_LINES.items():
        holes = re.findall(r"<([^>]+)>", line)
        m = re.fullmatch("(.+)".join(re.escape(part) for part in re.split(r"<[^>]+>", line)), text)
        if m:
            fill = {}
            for hole, value in zip(holes, m.groups()):
                fill.setdefault(hole, []).append(value)
            return name, fill
    return None, {}


def patch_line_name(text):
    """The name of the patch line a build.log row is, or None."""
    return patch_line_fill(text)[0]


def patch_proof(rebase, planned, version):
    """design/PATCHING.md §4.3, step 4, from /rebase's facts (executor.md): the proof's line, or None when a fact
    says more than the run image changed, or the image is not on the planned run image."""
    b, a = rebase["before"], rebase["after"]
    above = [f["diff_ids"][f["diff_ids"].index(f["top_layer"]) + 1:] if f["top_layer"] in f["diff_ids"] else None for f in (b, a)]
    if None in above or above[0] != above[1] or b["top_layer"] == a["top_layer"] or any(b[k] != a[k] for k in ("build_metadata", "config", "stack_id")):
        return None
    if a["run_image"] != planned:  # GET /builder's form, compared byte for byte (executor.md, POST /rebase)
        return None
    return patch_line("proof", version=version)


def patch_order(rows, proof, version, fixes, end):
    """The case's timeline in routes.md's order: the names (or, for a proof the facts do not give, None) of what must
    come one after another, each a row index or None when missing."""
    def first(pred):
        return next((i for i, r in enumerate(rows) if pred(r)), None)

    def step(name, status):
        return first(lambda r: r["kind"] == "progress" and r["data"]["step"] == name and r["data"]["status"] == status)

    def line(text):
        return first(lambda r: r["kind"] == "build.log" and r["data"]["line"] == text)

    return [("its clean verdict", first(lambda r: r["kind"] == "verdict" and r["data"]["state"] == "clean")),
            ("rebase running", step("rebase", "running")), ("rebase done", step("rebase", "done")),
            ("image-sbom running", step("image-sbom", "running")), ("image-sbom done", step("image-sbom", "done")),
            ("the proof's line", proof and line(proof)),
            ("the run-config's line", line(patch_line("unchanged", version=version))),
            *((f"the fix of {f['name']}", line(patch_line("fixed", **f))) for f in fixed_lines(fixes)),
            ("no new vulnerability", line(PATCH_LINES["no-new"])),
            ("start running", step("start", "running")), ("its deploy.completed", first(lambda r: r["kind"] == "deploy.completed")),
            ("watch running", step("watch", "running")), ("the watching line", first(lambda r: r["kind"] == "build.log" and patch_line_name(r["data"]["line"]) == "watching")),
            *end(step, first)]


def patch_case_problems(label, case, ident):
    """One patch run, passed or reverted: its rows bound to it and in routes.md's order, the proof its rebase's facts give
    before anything deploys, every line one of *Its lines*, its runs read for gate_state and promote_run, and its trail."""
    out = []
    rows, run, preview = case["timeline"], case["run"]["run"], case["preview"]
    digest = case["rebase"]["image"].split("@")[1]
    fixes = case["fixes"]
    reverted = run.get("failure_reason") == "reverted"
    proof = patch_proof(case["rebase"], ident["run_image"], version_name(preview))
    if proof is None:
        out.append(f"{label}: the rebase's facts do not prove only the planned run image changed, yet the run deployed")
    for r in rows:
        if (r.get("run"), r["project"], r["scope"]) != (run["id"], case["project"], "run"):
            out.append(f"{label}: row {r['id']} is {r['scope']} {r.get('run')} of {r['project']}, not the patch run's own")
        if r["kind"] == "build.log":
            name, fill = patch_line_fill(r["data"]["line"])
            hhmm = r["ts"][11:16] if name == "reverted" else case["watch_until"][11:16] if name == "watching" else None
            if name is None:
                out.append(f"{label}: row {r['id']}'s line is none of routes.md's patch lines: {r['data']['line']!r}")
            elif set(fill.get("env", ["preview"])) != {"preview"} or set(fill.get("version", [version_name(preview)])) != {version_name(preview)} \
                    or (hhmm and fill.get("HH:MM") != [hhmm]):
                out.append(f"{label}: row {r['id']}'s line names another env, version or time than its run's: {r['data']['line']!r}")
        if r["kind"] == "deploy.completed" and r["data"]["env"] != "preview":
            out.append(f"{label}: row {r['id']} deploys {r['data']['env']}: a patch run writes preview, production only through an approval")
    if [r["id"] for r in rows] != sorted({r["id"] for r in rows}) or [r["ts"] for r in rows] != sorted(r["ts"] for r in rows):
        out.append(f"{label}: the timeline's ids or times go backwards")

    def passed(step, first):
        return [("watch done", step("watch", "done")), ("the passed line", first(lambda r: r["kind"] == "build.log" and r["data"]["line"] == PATCH_LINES["passed"]))]

    def revert(step, first):
        return [("watch failed", step("watch", "failed")), ("the reverted line", first(lambda r: r["kind"] == "build.log" and patch_line_name(r["data"]["line"]) == "reverted"))]

    order = patch_order(rows, proof, version_name(preview), fixes, revert if reverted else passed)
    index = dict(order)
    deployed_at = index["its deploy.completed"]
    if deployed_at is not None and (index["the proof's line"] is None or index["the proof's line"] > deployed_at):
        out.append(f"{label}: preview was deployed at row {rows[deployed_at]['id']} before any proof row: a patch run deploys only after its proof (routes.md, *Patch runs*)")
    missing = [name for name, at in order if at is None]
    if missing:
        out.append(f"{label}: the timeline has no {', no '.join(missing)}")
    else:
        for (was, i), (now, j) in zip(order, order[1:]):
            if not i < j:
                out.append(f"{label}: {now} comes before {was}; a patch run deploys only after its proof (routes.md, *Patch runs*)")
    last = {"state": "failed", "failure_reason": "reverted"} if reverted else {"state": "clean"}
    if rows[-1]["kind"] != "run.state" or rows[-1]["data"] != last or run["state"] != last["state"]:
        out.append(f"{label}: the run ends {rows[-1]['data']}; its watch ends it {last}")
    done = next((r for r in rows if r["kind"] == "deploy.completed"), None)
    if done is not None and done["data"]["image_digest"] != digest:
        out.append(f"{label}: preview was deployed {done['data']['image_digest']}; the rebase made {digest}")
    newest = {}
    for r in rows:
        if r["kind"] == "progress":
            newest[(r["data"]["step"], r["data"].get("tool"))] = r["data"]
    if run["progress"] != list(newest.values()):
        out.append(f"{label}: the run read's progress is not each step's newest row in the order each first appeared")
    runs = case["runs"]
    me = next((r for r in runs if r["id"] == run["id"]), None)
    if run.get("kind") != "patch" or me is None or me.get("kind") != "patch" or me["started_by"] != CANYON_PATCH:
        out.append(f"{label}: the run read and its runs row are not one patch run, kind patch, started by Canyon")
    # routes.md: gate_state is the newest run's state but a patch run's; promote_run and prod.clean-gate's newest run
    # read a patch run only while preview runs the image it deployed (counts, clean_gate, promote_run_of).
    deployed = {run["id"]: done["data"]["image_digest"] if done else None}
    people = [r for r in runs if r.get("kind") != "patch"]
    if case["gate_state"] != (people[0]["state"] if people else "never_reviewed"):
        out.append(f"{label}: gate_state is {case['gate_state']}; the newest run that is not a patch run says {people[0]['state'] if people else 'never_reviewed'}")
    promote = promote_run_of({"envs": {"preview": preview}, "last_run": runs[0]["id"]}, runs, deployed)
    if case["promote_run"] != promote:
        out.append(f"{label}: promote_run is {case['promote_run']}; preview's package and image make it {promote}")
    passed_gate, why = clean_gate(next((r for r in runs if r["id"] == case["promote_run"]), None), runs, preview, deployed=deployed)
    if not passed_gate:
        out.append(f"{label}: prod.clean-gate does not pass on promote_run {case['promote_run']}: {why}")
    if preview["patch"].get("run") != run["id"] or preview["patch"].get("identity") != ident["id"]:
        out.append(f"{label}: preview's patch names another run or identity than the patch's")
    if (preview["image_digest"], preview["gate_run"]) != ((case["replaced"]["image_digest"], case["replaced"]["gate_run"]) if reverted else (digest, run["id"])) or preview["patch"]["state"] != ("reverted" if reverted else "patched_preview"):
        out.append(f"{label}: preview's row is not what the {'revert put back' if reverted else 'patch deployed'}")
    out += miscategorised(label, case["audit"])
    by_action = {e["action"]: e for e in case["audit"]}
    patched = by_action.get("patch preview")
    want = {"package": run["package"], "run": run["id"], "replaces": case["before"], "image": digest, "proof": "layers",
            "fixed": [{k: f[k] for k in ("name", "from", "to", "vulnerability")} for f in fixes]}
    if patched is None or patched["detail"] != want or patched["actor"] != CANYON_PATCH:
        out.append(f"{label}: no patch preview row by Canyon naming the run, both images and its fixes")
    if reverted and (by_action.get("revert patch preview") or {}).get("detail") != want:
        out.append(f"{label}: no revert patch preview row with the patch's detail")
    return out


def rebuild_problems(doc):
    """routes.md, *Patch runs*, *A rebuild patch*: the rebuild case's identity is its builder's digest and never approved
    for the fleet; its rows are the run's own, in order — its clean verdict, its build, whose output lines are free, its
    image's SBOM, the same_inputs line, the run-config line, its fixes, then the deploy and the watch — every other line one
    of *Its lines*; its trail's patch preview row carries the same_inputs proof with the key version both builds' values
    were compared under; its approval is the rebuild's, its proof naming the runtime the same line names."""
    case, out = doc["rebuild"], []
    ident, rows, run, ap = case["identity"], case["timeline"], case["run"]["run"], case["approval"]
    version = version_name(doc["preview"])
    if ident["builder"].rpartition("@")[2] != ident["id"]:
        out.append(f"rebuild: identity {ident['id']} is not the digest of its builder {ident['builder']}")
    first = lambda pred: next((i for i, r in enumerate(rows) if pred(r)), None)
    step = lambda name, status: first(lambda r: r["kind"] == "progress" and r["data"]["step"] == name and r["data"]["status"] == status)
    line = lambda name: first(lambda r: r["kind"] == "build.log" and patch_line_name(r["data"]["line"]) == name)
    built = (step("build", "running"), step("build", "done"))
    newer = None
    for i, r in enumerate(rows):
        if (r.get("run"), r["scope"]) != (run["id"], "run"):
            out.append(f"rebuild: row {r['id']} is {r['scope']} {r.get('run')}, not the rebuild's own")
        if r["kind"] == "progress" and r["data"]["step"] == "rebase":
            out.append("rebuild: a rebuild patch writes no rebase step")
        if r["kind"] == "build.log" and not (None not in built and built[0] < i < built[1]):  # its build's own output is free
            name, fill = patch_line_fill(r["data"]["line"])
            if name is None or set(fill.get("version", [version])) != {version}:
                out.append(f"rebuild: row {r['id']}'s line is none of routes.md's patch lines for {version}: {r['data']['line']!r}")
            if name == "same":
                newer = fill["newer"][0]
        if r["kind"] == "deploy.completed" and r["data"]["image_digest"] != ap["image"]:
            out.append(f"rebuild: preview was deployed {r['data']['image_digest']}; the approval ships {ap['image']}")
    if newer not in REBUILD_NEWER:
        out.append(f"rebuild: the same line names {newer!r}, none of {REBUILD_NEWER}")
    order = [("its clean verdict", first(lambda r: r["kind"] == "verdict" and r["data"]["state"] == "clean")),
             ("build running", built[0]), ("build done", built[1]), ("image-sbom running", step("image-sbom", "running")), ("image-sbom done", step("image-sbom", "done")),
             ("the same line", line("same")), ("the run-config's line", line("unchanged")), ("a fixed line", line("fixed")), ("no new vulnerability", line("no-new")),
             ("start running", step("start", "running")), ("its deploy.completed", first(lambda r: r["kind"] == "deploy.completed")),
             ("watch running", step("watch", "running")), ("the watching line", line("watching")), ("watch done", step("watch", "done")), ("the passed line", line("passed"))]
    missing = [name for name, at in order if at is None]
    if missing:
        out.append(f"rebuild: the timeline has no {', no '.join(missing)}")
    else:
        for (was, i), (now, j) in zip(order, order[1:]):
            if not i < j:
                out.append(f"rebuild: {now} comes before {was}; a rebuild patch deploys only after its same_inputs proof (routes.md, *A rebuild patch*)")
    if rows[-1]["kind"] != "run.state" or rows[-1]["data"] != {"state": "clean"} or run["state"] != "clean" or run.get("kind") != "patch":
        out.append(f"rebuild: the run is not a patch run that ends clean: {rows[-1]['data']}")
    fixed = [r["data"]["line"] for r in rows if r["kind"] == "build.log" and patch_line_name(r["data"]["line"]) == "fixed"]
    if fixed != [patch_line("fixed", **f) for f in ap["fixes"]] or any(f["class"] not in ("runtime", "lifecycle") for f in ap["fixes"]):
        out.append("rebuild: its fixed lines are not the approval's fixes, each of class runtime or lifecycle")
    out += miscategorised("rebuild", case["audit"])
    patched = next((e for e in case["audit"] if e["action"] == "patch preview"), None)
    detail = (patched or {}).get("detail", {})
    same = detail.get("same_inputs")
    if patched is None or patched["actor"] != CANYON_PATCH or detail.get("proof") != "same_inputs" or (detail.get("run"), detail.get("image"), detail.get("replaces")) != (run["id"], ap["image"], ap["replaces_image"]):
        out.append("rebuild: no patch preview row by Canyon with the same_inputs proof, naming the run and both images")
    elif not isinstance(same, dict) or not isinstance(same.get("values_key"), int) or isinstance(same.get("values_key"), bool) or same["values_key"] < 1:
        out.append("rebuild: the patch preview row's same_inputs names no values-HMAC key id: two builds' values compare only under one key version")
    elif not same.get("buildpacks") or not all(isinstance(b, str) and b for b in same["buildpacks"]) or detail.get("fixed") != ap["fixes"]:
        out.append("rebuild: the patch preview row's same_inputs names no buildpacks, or its fixed are not the approval's fixes")
    if (ap["patch_run"], ap["identity"], ap["version"]) != (run["id"], ident["id"], version):
        out.append("rebuild: the approval names another run, identity or version than the rebuild's")
    if newer in REBUILD_NEWER and ap["proof"] != REBUILD_PROOF.replace("<newer>", newer):
        out.append(f"rebuild: the approval's proof does not name {newer!r}, as its same line does")
    return out


def patch_problems(doc):
    """What the schema cannot say of the fixture: both cases, the passed patch's reads, its approval and the identity."""
    ident = doc["identities"]["identities"][0]
    before = doc["audit"][[e["action"] for e in doc["audit"]].index("patch preview")]["detail"]["replaces"] if any(e["action"] == "patch preview" for e in doc["audit"]) else None
    fixes = [{k: v for k, v in p.items() if k not in ("run", "at")} for p in doc["image_read"]["patched"] if p["run"] == doc["run"]["run"]["id"]]
    out = []
    # The watch lasts CANYON_PATCH_WATCH from its running row: the passed patch's approval says until when, and the
    # reverted one's watch was as long.
    watching = lambda case: next(ts_seconds(r["ts"]) for r in case["timeline"] if r["kind"] == "progress" and r["data"]["step"] == "watch")
    length = ts_seconds(doc["approval"]["watch"]["until"]) - watching(doc)
    for label, case in (("patch", doc), ("patch reverted", doc["reverted"])):
        until = datetime.datetime.fromtimestamp(watching(case) + length, datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        out += patch_case_problems(label, {**case, "project": doc["approval"]["app"], "before": before, "fixes": fixes, "watch_until": until}, ident)
    if patch_lines_in_routes() != PATCH_LINES:
        out.append("patch: routes.md's *Its lines* are not the patch lines validate.py knows, byte for byte")
    out += rebuild_problems(doc)
    run, digest = doc["run"]["run"], doc["rebase"]["image"].split("@")[1]
    kube = doc["kubernetes"]
    for i in doc["identities"]["identities"] + [kube["identity"]]:
        if i["run_image"].rpartition("@")[2] != i["id"]:
            out.append(f"patch: identity {i['id']} is not the digest of its run_image {i['run_image']}")
    out += plan_problems(doc["identities"], ident, doc["approval"]["app"])
    # the tick after platform.known-vuln went off on every app read no app, and keeps no cause the tick before gave
    off = doc["rule_off"]["plan"]
    if off is None or off.get("why") != "rule_off" or off["apps"] or off["identity"] is not None or ts_seconds(off["at"]) <= ts_seconds(doc["identities"]["plan"]["at"]):
        out.append(f"patch: the tick after the rule went off on every app does not read rule_off, no app and no identity, after the tick before: {off}")
    out += plan_problems(doc["rule_off"], ident, doc["approval"]["app"])
    # On Kubernetes run_image is <repository>@sha256:<hex>, any tag stripped — the ClusterBuilder names kpack's copy by tag
    # and digest, the rebaser records it by digest alone — compared byte for byte: the planned digest under another
    # repository is another run image (executor.md, POST /rebase).
    by_digest = re.compile(r"[a-z0-9][a-z0-9.:-]*(/[a-z0-9._-]+)+@sha256:[0-9a-f]{64}")
    if not all(by_digest.fullmatch(r) for r in (kube["identity"]["run_image"], kube["rebase"]["before"]["run_image"], kube["rebase"]["after"]["run_image"])):
        out.append("patch: a Kubernetes run_image is not a reference by digest")
    if patch_proof(kube["rebase"], kube["identity"]["run_image"], "v1.4") is None:
        out.append("patch: the Kubernetes rebase's facts do not prove only its planned run image changed")
    read = doc["image_read"]
    if read["image_digest"] != digest or read["state"] != "checked" or not fixes:
        out.append("patch: the image read is not preview's patched image, checked, naming the run's fixes")
    elif not 0 <= ts_seconds(read["scanned_at"]) - ts_seconds(read["db_built_at"]) <= 5 * 86400:
        out.append(f"patch: the patched image was matched against a database built {read['db_built_at']}, past the 5-day limit at {read['scanned_at']}: never read as checked")
    # routes.md, the image read's patched[]: found by the image's digest, each entry's at when the image env runs last
    # reached env, its row's deployed_at (#2105, the decision of #2556), so a rollback to a patched image reads the rollback's.
    reached = (doc.get(read["env"]) or {}).get("deployed_at")  # the env's own row: preview's, or a production fixture's
    if any(p["at"] != reached for p in read["patched"]):
        out.append(f"patch: a patched[] entry's at is not when the image {read['env']} runs last reached it ({reached})")
    ap = doc["approval"]
    watched = next((r["ts"] for r in doc["timeline"] if r["kind"] == "progress" and r["data"]["step"] == "watch" and r["data"]["status"] == "done"), None)
    if (ap["patch_run"], ap["identity"], ap["fixes"], ap["version"], ap["watch"]["until"]) != (run["id"], ident["id"], fixes, version_name(doc["preview"]), watched):
        out.append("patch: the patch approval names another run, identity, fixes, version or watch than the patch's")
    if ap["replaces_image"] != before:
        out.append(f"patch: the patch approval replaces {ap['replaces_image']}; production ran {before} when it was filed")
    if ap["image"] != digest:
        out.append(f"patch: the patch approval ships {ap['image']}; the patch put {digest} in preview")
    asked = next((e for e in doc["audit"] if e["action"] == "request patch approval"), None)
    if asked is None or (asked["detail"]["code"], asked["detail"]["image"]) != (ap["code"], digest):
        out.append("patch: no request patch approval row naming the approval's code and preview's patched image")
    final = doc["timeline"][-1]["data"]
    if [(r["kind"], r["data"]) for r in doc["org"]] != [("run.transition", {"project": doc["approval"]["app"], "run": run["id"], "state": final["state"], "kind": "patch"})]:
        out.append("patch: the org timeline has no run.transition of the patch run, kind patch, in its final state")
    return out


def seam3_patch():
    """routes.md, *Patch runs* and the patch approval; events.md's patch steps; executor.md's /rebase: a rebase patch of
    claims-portal's preview that passed its watch and one that was reverted, in patch.schema.json's shapes, then what the
    schema cannot say."""
    path = CONTRACTS / "testdata" / "patch" / "claims-portal.json"
    schema = CONTRACTS / "schema" / "patch.schema.json"
    doc = load(path)
    if not schema_check("patch", schema, doc):
        return
    found = patch_problems(doc)
    plan = load(schema)["definitions"]["identities_read"]["properties"]["plan"]["oneOf"][1]["properties"]
    for what, causes, named in zip(("why it read no app", "each app's"), (set(plan["why"]["enum"]), set(plan["apps"]["items"]["properties"]["why"]["enum"])), plan_causes_in_routes()):
        if causes != named:
            found.append(f"patch: the plan's causes of {what} in patch.schema.json are not routes.md's: schema alone {sorted(causes - named)}, routes.md alone {sorted(named - causes)}")
    if found:
        errors.extend(found)
        return
    rows = doc["timeline"]
    at = lambda pred: next(i for i, r in enumerate(rows) if pred(r))
    proof_at = at(lambda r: r["kind"] == "build.log" and patch_line_name(r["data"]["line"]) == "proof")
    deploy_at = at(lambda r: r["kind"] == "deploy.completed")
    watch_at = at(lambda r: r["kind"] == "progress" and r["data"]["step"] == "watch")
    start_at = at(lambda r: r["kind"] == "progress" and r["data"]["step"] == "start")
    other = "sha256:" + "9c" * 32

    def move(i, j):
        return lambda d: d["timeline"].insert(j, d["timeline"].pop(i))

    def move_in(case, row, to):  # the first row of case's timeline row matches, moved to where to's first is
        def mutate(d):
            rows = d[case]["timeline"]
            i, j = next(k for k, r in enumerate(rows) if row(r)), next(k for k, r in enumerate(rows) if to(r))
            rows.insert(j, rows.pop(i))
        return mutate

    for name, mutate in (
            ("a patch run that deploys before its proof row", move(proof_at, deploy_at)),
            ("a proof row of another run", lambda d: d["timeline"][proof_at].update(run="gr_0ther1")),
            ("a patch run that deploys production", lambda d: d["timeline"][deploy_at]["data"].update(env="prod")),
            ("a rebase that changed a layer above the run image", lambda d: d["rebase"]["after"]["diff_ids"].__setitem__(-1, other)),
            ("a rebase that changed how the app starts", lambda d: d["rebase"]["after"].update(config=other)),
            ("a rebase onto the same run image", lambda d: d["rebase"]["after"].update(top_layer=d["rebase"]["before"]["top_layer"], diff_ids=d["rebase"]["before"]["diff_ids"])),
            ("a rebase onto another run image than the plan's", lambda d: d["rebase"]["after"].update(run_image=other)),
            ("a Kubernetes rebase onto the planned digest under another repository", lambda d: d["kubernetes"]["rebase"]["after"]
             .update(run_image="registry.example/mirror/canyon-run@" + d["kubernetes"]["identity"]["id"])),
            ("a Kubernetes run image by digest alone", lambda d: d["kubernetes"]["identity"].update(run_image=d["kubernetes"]["identity"]["id"])),
            ("Kubernetes run images still naming kpack's tag", lambda d: [f.update(run_image=f["run_image"].replace("/canyon-builder@", "/canyon-builder:clusterbuilder-canyon-run-image@"))
                                                                          for f in (d["kubernetes"]["identity"], d["kubernetes"]["rebase"]["before"], d["kubernetes"]["rebase"]["after"])]),
            ("a Kubernetes rebase that changed a layer above the run image", lambda d: d["kubernetes"]["rebase"]["after"]["diff_ids"].__setitem__(-1, other)),
            ("a patch run with no run-config line", lambda d: d["timeline"].remove(next(r for r in d["timeline"] if r["kind"] == "build.log" and r["data"]["line"].startswith("What ")))),
            ("a watch that starts before the deploy", move(watch_at, start_at)),
            ("a build.log line that is no patch line", lambda d: d["timeline"][proof_at + 1]["data"].update(line="Looks fine to Canyon")),
            ("an image read that could not check", lambda d: d["image_read"].update(state="could_not_check", findings=[], notes=[])),
            ("a patch approval naming fixes the image does not have", lambda d: d["approval"]["fixes"][0].update(to="3.0.13-0ubuntu3.6")),
            ("a patch approval over another production image", lambda d: d["approval"].update(replaces_image=other)),
            ("a patch approval shipping another image than the patch's", lambda d: d["approval"].update(image=other)),
            ("a request patch approval row naming another image", lambda d: next(e for e in d["audit"] if e["action"] == "request patch approval")["detail"].update(image=other)),
            ("a patch preview row under governance", lambda d: next(e for e in d["audit"] if e["action"] == "patch preview").update(category="governance")),
            ("a gate_state that is the patch run's", lambda d: d["reverted"].update(gate_state="failed")),
            ("a promote_run that is a reverted patch run", lambda d: d["reverted"].update(promote_run=d["reverted"]["run"]["run"]["id"])),
            ("a reverted run with no revert row", lambda d: d["reverted"]["audit"].remove(next(e for e in d["reverted"]["audit"] if e["action"] == "revert patch preview"))),
            ("an org row without kind", lambda d: d["org"][0]["data"].pop("kind")),
            ("an identity that is not its run image's digest", lambda d: d["identities"]["identities"][0].update(run_image="sha256:" + "5e" * 32)),
            ("a plan that read no app yet names apps", lambda d: d["identities"]["plan"].update(why="org_off")),
            ("a plan naming apps onto no identity", lambda d: d["identities"]["plan"].update(identity=None)),
            ("a plan onto an identity the read does not list", lambda d: d["identities"]["plan"].update(identity=other)),
            ("a plan before its identity was planned", lambda d: d["identities"]["plan"].update(at="2026-10-18T09:00:00Z")),
            ("a plan whose apps are not sorted", lambda d: d["identities"]["plan"]["apps"].reverse()),
            ("a plan naming an app twice", lambda d: d["identities"]["plan"]["apps"].append(d["identities"]["plan"]["apps"][-1])),
            ("a plan after the watch passed that says its app waits for its wave", lambda d: d["identities"]["plan"]["apps"][0].update(why="wave")),
            ("a tick after the rule went off that keeps the last tick's causes", lambda d: d["rule_off"].update(plan={**d["identities"]["plan"], "at": d["rule_off"]["plan"]["at"]})),
            ("a tick after the rule went off that reads failed", lambda d: d["rule_off"]["plan"].update(why="failed")),
            ("a patched[] entry dated by the patch run and not when the image reached preview", lambda d: d["image_read"]["patched"][0].update(at="2026-10-18T10:35:29Z")),
            ("a patched image matched against a stale database", lambda d: d["image_read"].update(db_built_at="2026-10-01T00:00:00Z")),
            ("a watching line naming production", lambda d: next(r for r in d["timeline"] if r["kind"] == "build.log" and r["data"]["line"].startswith("Watching"))["data"]
             .update(line=PATCH_LINES["watching"].replace("<env>", "production").replace("<HH:MM>", "10:35"))),
            ("a watching line naming another time", lambda d: next(r for r in d["timeline"] if r["kind"] == "build.log" and r["data"]["line"].startswith("Watching"))["data"]
             .update(line=PATCH_LINES["watching"].replace("<env>", "preview").replace("<HH:MM>", "00:01"))),
            ("a revert that puts back another run's row", lambda d: d["reverted"]["preview"].update(gate_run="gr_0ther1")),
            ("a rebuild whose same_inputs names no values-HMAC key id", lambda d: next(e for e in d["rebuild"]["audit"] if e["action"] == "patch preview")["detail"]["same_inputs"].pop("values_key")),
            ("a rebuild with the rebase's proof", lambda d: next(e for e in d["rebuild"]["audit"] if e["action"] == "patch preview")["detail"].update(proof="layers")),
            ("a rebuild that deploys before its same line", move_in("rebuild", lambda r: r["kind"] == "build.log" and r["data"]["line"].startswith("Rebuilt with"), lambda r: r["kind"] == "deploy.completed")),
            ("a rebuild that rebases", lambda d: d["rebuild"]["timeline"][5]["data"].update(step="rebase")),
            ("a rebuild identity that is not its builder's digest", lambda d: d["rebuild"]["identity"].update(builder="sha256:" + "b2" * 32)),
            ("a rebuild approval naming another runtime than its same line", lambda d: d["rebuild"]["approval"].update(proof=REBUILD_PROOF.replace("<newer>", "a newer Python"))),
            ("a rebuild line outside its build that is no patch line", lambda d: next(r for r in d["rebuild"]["timeline"] if r["kind"] == "build.log" and r["data"]["line"].startswith("No new"))["data"].update(line="Looks rebuilt to Canyon"))):
        probe = copy.deepcopy(doc)
        mutate(probe)
        if not patch_problems(probe):
            errors.append(f"selftest: the patch fixture with {name} was not refused")
    # the same image promoted: production's image read is dated by production's own row, and passes
    promoted = "2026-10-19T09:00:00Z"
    probe = copy.deepcopy(doc)
    probe["prod"] = {**doc["preview"], "url": "http://claims-portal.prod.localhost:7777", "deployed_at": promoted}
    probe["image_read"]["env"] = "prod"
    for p in probe["image_read"]["patched"]:
        p["at"] = promoted
    if patch_problems(probe):
        errors.append(f"selftest: production's image read dated by production's deploy was refused: {patch_problems(probe)}")
    probe["image_read"]["patched"][0]["at"] = doc["preview"]["deployed_at"]
    if not patch_problems(probe):
        errors.append("selftest: production's image read dated by preview's deploy was not refused")
    newer = {**doc["reverted"]["runs"][-1], "id": "gr_c0ffee", "started_at": "2026-10-17T09:00:00Z", "ended_at": "2026-10-17T09:02:00Z"}
    probe = copy.deepcopy(doc)
    probe["reverted"]["runs"].insert(1, newer)
    probe["reverted"]["promote_run"] = newer["id"]
    if patch_problems(probe):
        errors.append(f"selftest: a revert under a newer review of the package was refused: {patch_problems(probe)}")
    routes_lines = patch_lines_in_routes()
    PATCH_LINES["passed"] += "."
    if patch_lines_in_routes() == PATCH_LINES:
        errors.append("selftest: a patch line that is not routes.md's, byte for byte, was not refused")
    PATCH_LINES["passed"] = routes_lines["passed"]
    env = doc["preview"]
    for name, patch, ok in (("held without why", {"state": "held", "since": env["patch"]["since"]}, False),
                            ("asked without its approval", {"state": "asked", "since": env["patch"]["since"], "identity": env["patch"]["identity"]}, False),
                            ("a why on a state that is not held", {**env["patch"], "why": "paused"}, False),
                            ("a state spelled as prose", {**env["patch"], "state": "couldn't rescan"}, False),
                            ("held, paused", {"state": "held", "since": env["patch"]["since"], "why": "paused"}, True),
                            # routes.md, envs.<env>.patch (#2601): a denial names the approval the team denied and its identity
                            ("denied without the approval denied", {"state": "denied", "since": env["patch"]["since"], "identity": env["patch"]["identity"]}, False),
                            ("denied without its identity", {"state": "denied", "since": env["patch"]["since"], "approval": "K7QF-2M9X"}, False),
                            ("denied", {"state": "denied", "since": env["patch"]["since"], "identity": env["patch"]["identity"], "run": env["patch"]["run"], "approval": "K7QF-2M9X"}, True)):
        if takes(schema, "env_patch", patch) != ok:
            errors.append(f"selftest: an env's patch {name} was {'refused' if ok else 'accepted'}")
    for name, probe, ok in (("a rebuild approval with the rebase's proof", {**doc["rebuild"]["approval"], "proof": doc["approval"]["proof"]}, False),
                            ("a rebase approval with a rebuild's proof", {**doc["approval"], "proof": doc["rebuild"]["approval"]["proof"]}, False)):
        if takes(schema, "approval_read", probe) != ok:
            errors.append(f"selftest: {name} was {'refused' if ok else 'accepted'}")
    for name, probe, ok in (("a rebuild identity approved for the fleet", {**doc["rebuild"]["identity"], "approved": {"by": doc["approval"]["approver"] | {"kind": "person", "subject": "auth0|a1"}, "at": "2026-10-25T09:30:00Z", "reason": "fleet"}}, False),
                            ("a rebuild identity naming no builder", {k: v for k, v in doc["rebuild"]["identity"].items() if k != "builder"}, False),
                            ("a rebase identity naming a builder", {**doc["identities"]["identities"][0], "builder": doc["rebuild"]["identity"]["builder"]}, False)):
        if takes(schema, "identity", probe) != ok:
            errors.append(f"selftest: {name} was {'refused' if ok else 'accepted'}")
    held = {"by": {"kind": "person", "subject": "auth0|r1", "display": "Rae"}, "at": "2026-10-18T10:40:00Z", "reason": "incident"}
    if not takes(schema, "approval_read", {**doc["approval"], "state": "outdated", "held": held}):
        errors.append("selftest: a patch approval a hold outdated was refused")
    run = doc["run"]["run"]
    for name, probe, ok in (("not_platform_only without which", {**run, "state": "failed", "failure_reason": "not_platform_only"}, False),
                            ("not_platform_only on another failure", {**run, "state": "failed", "failure_reason": "error", "not_platform_only": "config_changed"}, False),
                            ("a patch run that changed its config", {**run, "state": "failed", "failure_reason": "not_platform_only", "not_platform_only": "config_changed"}, True),
                            ("not_same_inputs without which", {**run, "state": "failed", "failure_reason": "not_same_inputs"}, False),
                            ("not_same_inputs on another failure", {**run, "state": "failed", "failure_reason": "error", "not_same_inputs": "values"}, False),
                            ("a rebuild whose dependencies resolved anew", {**run, "state": "failed", "failure_reason": "not_same_inputs", "not_same_inputs": "dependencies"}, True)):
        if takes(CONTRACTS / "schema" / "app-page.schema.json", "run_read", {"run": probe}) != ok:
            errors.append(f"selftest: a run read with {name} was {'refused' if ok else 'accepted'}")


ENDPOINT_SCHEMES = {"postgres": ("postgres", "postgresql"), "s3": ("s3", "https")}  # routes.md, the report row (#2432)


def endpoint_scheme(endpoint):
    return endpoint.split("://", 1)[0]


def endpoints_problem(declared, entries):
    """What the schema cannot say of a deployed report's resources: each names one of the release's
    resources, at most once, so never more than it has, in that resource type's scheme."""
    types = {r["name"]: r["type"] for r in declared}
    names = [e["name"] for e in entries]
    if len(entries) > len(declared) or len(set(names)) != len(names):
        return "a resource reported twice, or more entries than the release has resources"
    for e in entries:
        if e["name"] not in types:
            return f"{e['name']} is not one of the release's resources"
        if endpoint_scheme(e["endpoint"]) not in ENDPOINT_SCHEMES.get(types[e["name"]], ()):
            return f"{e['name']}'s endpoint is not in its type's scheme"
    return None


def missing_of(declared, entries):
    """What a deployed report that fits leaves out (routes.md, the report row's 409 resources_missing; #2452):
    every resource the release lists that no entry names, sorted."""
    return sorted({r["name"] for r in declared} - {e["name"] for e in entries})


def seam3_release():
    """Hand-off (design/HAND-OFF.md): the platform webhook, a release delivered and reported, the
    sidecar's access pull. What the schema cannot say: the report's digest is the release's image's;
    the body's delivery and event are the headers'; the app's and the sidecar's environment are the
    executor's, one producer — the front door inside is one a Kubernetes /run carries, the access
    file is one too; every address the org calls back is Canyon's; a resource gives what its type gives."""
    doc = load(CONTRACTS / "testdata" / "release" / "meridian.json")
    schema = CONTRACTS / "schema" / "release.schema.json"
    if not schema_check("release", schema, doc):
        return
    deploy, headers = doc["deploy"], doc["headers"]
    digest = deploy["image"].split("@", 1)[1]
    # 1. Live means the release's digest: the report and the refusal both name it.
    if doc["report_deployed"]["digest"] != digest or doc["report_refusal"]["expected"] != digest:
        errors.append(f"release: a deployed report and a digest_mismatch refusal name the release's digest, {digest}")
    # 2. The headers say what the body says.
    if (headers["Canyon-Event"], headers["Canyon-Delivery"]) != (deploy["event"], deploy["delivery"]):
        errors.append("release: Canyon-Event and Canyon-Delivery are the body's event and delivery")
    # 2b. A retry is the same delivery, re-stamped: everything but sent_at equal, and later; the webhook's
    #     last_delivery is that newest attempt.
    again = doc["redelivery"]
    if {k: v for k, v in again.items() if k != "sent_at"} != {k: v for k, v in deploy.items() if k != "sent_at"} or again["sent_at"] <= deploy["sent_at"]:
        errors.append("release: a redelivery is the deploy re-stamped later under the same delivery, nothing else changed")
    # 2c. A test delivery is its own: the ping's answer names the ping's delivery and sent_at (#2439).
    if (doc["webhook_ping"]["delivery"], doc["webhook_ping"]["sent_at"]) != (doc["ping"]["delivery"], doc["ping"]["sent_at"]):
        errors.append("release: webhook_ping answers the ping's own delivery and sent_at")
    last = doc["webhook_get"]["last_delivery"]
    if (last["delivery"], last["release"], last["sent_at"]) != (deploy["delivery"], deploy["release"], again["sent_at"]):
        errors.append("release: webhook_get.last_delivery is the newest attempt of the newest delivery")
    # 3. One producer: the sidecar's and the app's environment are the executor's, and the front door
    #    and the access file inside are ones a Kubernetes /run carries (executor.schema.json).
    issuer, app, env = deploy["app_env"]["CANYON_IDENTITY_ISSUER"], deploy["app"], deploy["environment"]
    side = deploy["sidecar"]["env"]
    if (side["CANYON_IDENTITY_ISSUER"], side["CANYON_IDENTITY_AUDIENCE"], deploy["app_env"]["CANYON_IDENTITY_AUDIENCE"], side["CANYON_TARGET"]) != (issuer, app, app, env):
        errors.append("release: the sidecar's and the app's CANYON_IDENTITY_* and CANYON_TARGET are the release's app, environment and Canyon's URL")
    ex_path, exdoc = CONTRACTS / "schema" / "executor.schema.json", load(CONTRACTS / "testdata" / "executor" / "claims-portal.json")
    try:
        door = json.loads(side["CANYON_FRONT_DOOR"])
    except ValueError:
        door = None
    if not isinstance(door, dict) or door.get("choice") != 1:
        errors.append("release: CANYON_FRONT_DOOR is a choice-1 front door (design/HAND-OFF.md §6)")
    elif not accepts(ex_path, {**exdoc, "kube_run_request": {**exdoc["kube_run_request"], "front_door": door, "access": doc["access"]}}):
        errors.append("release: the sidecar's front door and the access route's answer are not what a Kubernetes /run carries")
    # 3b. The deny-all a stopped environment pulls (#2450) is an access file too: the sidecar reads it as one.
    if not accepts(ex_path, {**exdoc, "kube_run_request": {**exdoc["kube_run_request"], "access": doc["access_deny_all"]}}):
        errors.append("release: access_deny_all is not an access file a Kubernetes /run could carry, and the sidecar reads it as one")
    # 4. Every address the org calls back is Canyon's, under the release and the app.
    if side["CANYON_ACCESS_URL"] != f"{issuer}/access/{app}/{env}":
        errors.append(f"release: CANYON_ACCESS_URL is {issuer}/access/{app}/{env}, got {side['CANYON_ACCESS_URL']}")
    for body in (deploy, doc["remove"], doc["delete_remove"]):
        if body["report_url"] != f"{issuer}/releases/{body['release']}/report":
            errors.append(f"release: report_url is {issuer}/releases/{body['release']}/report, got {body['report_url']}")
    if not deploy["gate_url"].startswith(issuer + "/"):
        errors.append("release: gate_url is on Canyon's public URL")
    # 5. A resource gives what its type gives (GET /api/v1/component-types, the guardrails fixture).
    gives = {t["type"]: t["gives"] for t in load(CONTRACTS / "testdata" / "guardrails" / "preferences.json")["component_types"]["component_types"]}
    for r in deploy["resources"]:
        if r["gives"] != gives.get(r["type"]):
            errors.append(f"release: resource {r['name']} of type {r['type']} gives {gives.get(r['type'])}, got {r['gives']}")
    # 5b. The platform provides what a resource gives, so variables never name it (#2432).
    if set(deploy["variables"]) & {g for r in deploy["resources"] for g in r["gives"]}:
        errors.append("release: variables name a variable a resource gives: the platform provides it with the resource")
    # 5c. A deployed report's endpoints name the release's own resources, each once, in its type's scheme.
    if problem := endpoints_problem(deploy["resources"], doc["report_deployed"].get("resources", [])):
        errors.append(f"release: report_deployed.resources: {problem}")
    for name, entries, ok in (
            ("an endpoint for a resource the release does not name", [{"name": "cache", "endpoint": "postgres://pg.example/x"}], False),
            ("one resource reported twice", [{"name": "db", "endpoint": "postgres://pg.example/a"}, {"name": "db", "endpoint": "postgres://pg.example/b"}], False),
            ("an s3 scheme on a postgres resource", [{"name": "db", "endpoint": "s3://claims-files"}], False),
            ("postgresql on a postgres resource", [{"name": "db", "endpoint": "postgresql://pg.example:5432/claims"}], True),
            ("no resources at all", [], True)):
        if (endpoints_problem(deploy["resources"], entries) is None) != ok:
            errors.append(f"selftest: a deployed report with {name} was {'refused' if ok else 'accepted'}")
    # 5c bis. A deployed report names every resource the release lists (#2452): the fixture's report leaves out
    #     nothing, and the refusal example is what a report leaving every one out is answered — declared minus
    #     reported, sorted — its message naming them joined with ", ".
    refused = doc["report_resources_missing"]
    if missing_of(deploy["resources"], doc["report_deployed"].get("resources", [])):
        errors.append(f"release: report_deployed leaves out {missing_of(deploy['resources'], doc['report_deployed'].get('resources', []))}: it would be refused resources_missing")
    if refused["missing"] != missing_of(deploy["resources"], []):
        errors.append(f"release: report_resources_missing names {refused['missing']}; a report without resources leaves out {missing_of(deploy['resources'], [])}")
    if refused["message"] != f"the report has no endpoint for {', '.join(refused['missing'])}: send one for every resource the release lists":
        errors.append("release: report_resources_missing's message does not name its missing, joined with \", \"")
    two = [{"name": "db", "type": "postgres", "gives": ["DATABASE_URL"]}, {"name": "files", "type": "s3", "gives": ["STORAGE_URL"]}]
    for name, entries, want in (("both named", [{"name": "files"}, {"name": "db"}], []),
                                ("db alone", [{"name": "db"}], ["files"]),
                                ("none", [], ["db", "files"])):
        if missing_of(two, entries) != want:
            errors.append(f"selftest: a report naming {name} leaves out {missing_of(two, entries)}, want {want}")
    # 5d. A project delete's remove is a remove that deletes; the provided rows on the project read
    #     carry an endpoint in their type's scheme.
    if not doc["delete_remove"].get("delete") or {k: v for k, v in doc["delete_remove"].items() if k not in ("delete", "delivery", "release", "report_url", "sent_at")} != {k: v for k, v in doc["remove"].items() if k not in ("delivery", "release", "report_url", "sent_at")}:
        errors.append("release: delete_remove is a remove of the same app and environment, delete true")
    for row in load(CONTRACTS / "testdata" / "app-page" / "claims-portal.json")["resources"]:
        if row["state"] == "provided" and endpoint_scheme(row["endpoint"]) not in ENDPOINT_SCHEMES.get(row["type"], ()):
            errors.append(f"app-page: provided resource {row['name']}'s endpoint is not in its type's scheme")
    # 6. envs.<env> while a release is open is the project read's env shape plus run_by and pending, or,
    #    nothing deployed there yet, env_pending alone — on the project read and on the fleet alike.
    ap_path, ap = CONTRACTS / "schema" / "app-page.schema.json", load(CONTRACTS / "testdata" / "app-page" / "claims-portal.json")
    fl_path, fl = CONTRACTS / "schema" / "fleet.schema.json", load(CONTRACTS / "testdata" / "fleet" / "page.json")
    deployed = {k: v for k, v in ap["status"]["envs"]["preview"].items() if k != "state"}  # whether it runs is said of what Canyon runs alone
    row = next(a for a in fl["apps"] if "preview" in a["envs"] and "url" in a["envs"]["preview"])
    for name, env, ok in (("env_pending laid over a deployed preview", {**deployed, **doc["env_pending"]}, True),
                          ("env_pending alone", doc["env_pending"], True),
                          ("env_pending with a url but nothing else deployed", {**doc["env_pending"], "url": deployed["url"]}, False),
                          ("a pending environment Canyon runs", {**doc["env_pending"], "run_by": "canyon"}, False),
                          ("env_pending laid over a deployed preview Canyon runs", {**deployed, **doc["env_pending"], "run_by": "canyon"}, False),
                          ("a state on a preview the org runs", {**deployed, **doc["env_pending"], "state": "running"}, False),
                          ("a state outside running, stopped and unknown", {**deployed, "state": "crashed"}, False),
                          ("a preview Canyon runs that does not say whether it runs", deployed, False),
                          # #2452: a pod's check-in is said of what the org runs alone, beside no state
                          ("a check-in on a preview the org runs", {**deployed, **doc["env_pending"], "seen_at": "2026-09-27T10:02:00Z"}, True),
                          ("a check-in on a preview Canyon runs", {**deployed, "state": "running", "seen_at": "2026-09-27T10:02:00Z"}, False),
                          ("a check-in that is not a timestamp", {**deployed, **doc["env_pending"], "seen_at": "2 minutes ago"}, False)):
        reads = (("the project read (app-page.schema.json)", ap_path, {**ap, "status": {**ap["status"], "envs": {"preview": env}}}),
                 ("the fleet (fleet.schema.json)", fl_path, {**fl, "apps": [{**a, "envs": {"preview": env}} if a is row else a for a in fl["apps"]]}))
        for read, path, probe in reads:
            if accepts(path, probe) != ok:
                errors.append(f"release: {name} is {'refused' if ok else 'accepted'} as envs.preview on {read}")
    # 7. The schema refuses what the routes refuse.
    for name, key, value, ok in (
            ("a deploy without report_url", "deploy", {k: v for k, v in deploy.items() if k != "report_url"}, False),
            ("an event outside deploy and remove", "deploy", {**deploy, "event": "restart"}, False),
            ("an image by tag", "deploy", {**deploy, "image": deploy["image"].split("@")[0] + ":v1.4"}, False),
            ("a secret value in the sidecar's env", "deploy", {**deploy, "sidecar": {**deploy["sidecar"], "env": {**side, "CANYON_ACCESS_TOKEN": "x"}}}, False),
            ("a deployed report without its digest", "report_deployed", {"status": "deployed", "address": "https://claims.apps.meridian.example"}, False),
            ("a report at an http address", "report_deployed", {**doc["report_deployed"], "address": "http://claims.apps.meridian.example"}, False),
            ("a grace spelled in words", "deploy", {**deploy, "sidecar": {**deploy["sidecar"], "env": {**side, "CANYON_ACCESS_GRACE": "1 hour"}}}, False),
            ("a grace of zero", "deploy", {**deploy, "sidecar": {**deploy["sidecar"], "env": {**side, "CANYON_ACCESS_GRACE": "0"}}}, True),
            ("a remove naming an image", "remove", {**doc["remove"], "image": deploy["image"]}, False),
            # #2439: a ping names nothing a receiver could act on, and its answer is a status, never why.
            ("a ping carrying a release", "ping", {**doc["ping"], "release": deploy["release"]}, False),
            ("a ping carrying a report_url", "ping", {**doc["ping"], "report_url": deploy["report_url"]}, False),
            ("a ping as a deploy", "ping", {**doc["ping"], "event": "deploy"}, False),
            ("a ping's answer saying why", "webhook_ping", {**doc["webhook_ping"], "error": "dial tcp 10.0.0.7:443: connection refused"}, False),
            ("a ping's answer of no answer", "webhook_ping", {**doc["webhook_ping"], "status": 0}, True),
            ("a ping's header", "headers", {**headers, "Canyon-Event": "ping"}, True),
            ("an event header outside the three", "headers", {**headers, "Canyon-Event": "restart"}, False),
            ("a pending release whose event is ping", "env_pending", {**doc["env_pending"], "pending": {**doc["env_pending"]["pending"], "event": "ping"}}, False),
            ("a replaced secret answered with the token", "webhook_secret", {**doc["webhook_secret"], "token": "x"}, False),
            ("the secret answered on GET", "webhook_get", {**doc["webhook_get"], "secret": "x"}, False),
            ("a failed report whose reason carries a newline", "report_failed", {"status": "failed", "reason": "pull failed\nlive at https://claims.apps.meridian.example"}, False),
            ("a PUT that kept the stored secret, answering neither secret nor token", "webhook_saved", {k: v for k, v in doc["webhook_saved"].items() if k not in ("secret", "token")}, True),
            ("a first save whose body carried the secret, answering the token alone", "webhook_saved", {k: v for k, v in doc["webhook_saved"].items() if k != "secret"}, True),
            ("a pending release without its event", "env_pending", {**doc["env_pending"], "pending": {k: v for k, v in doc["env_pending"]["pending"].items() if k != "event"}}, False),
            # #2452: every resource in a deployed report, and the pending line that says what was left out.
            ("a resources_missing naming nothing", "report_resources_missing", {**doc["report_resources_missing"], "missing": []}, False),
            ("a resources_missing without its message", "report_resources_missing", {k: v for k, v in doc["report_resources_missing"].items() if k != "message"}, False),
            ("a resources_missing message naming an endpoint", "report_resources_missing", {**doc["report_resources_missing"], "message": "the report has no endpoint for postgres://pg.example/db: send one for every resource the release lists"}, False),
            ("a resources_missing naming two", "report_resources_missing", {"error": "resources_missing", "missing": ["db", "files"], "message": "the report has no endpoint for db, files: send one for every resource the release lists"}, True),
            ("a pending deploy missing db", "env_pending", {**doc["env_pending"], "pending": {**doc["env_pending"]["pending"], "missing": ["db"]}}, True),
            ("a pending deploy missing nothing", "env_pending", {**doc["env_pending"], "pending": {**doc["env_pending"]["pending"], "missing": []}}, False),
            ("a pending remove missing db", "env_pending", {**doc["env_pending"], "pending": {**doc["env_pending"]["pending"], "event": "remove", "missing": ["db"]}}, False),
            ("an undelivered deploy missing db", "env_pending", {**doc["env_pending"], "pending": {**doc["env_pending"]["pending"], "state": "undelivered", "missing": ["db"]}}, False),
            # #2432: the remove that deletes, and where the platform says each resource is.
            ("a remove saying delete false", "remove", {**doc["remove"], "delete": False}, False),
            ("a project delete's remove without delete", "delete_remove", doc["remove"], False),
            ("a deployed report without resources", "report_deployed", {k: v for k, v in doc["report_deployed"].items() if k != "resources"}, True),
            *((f"an endpoint {why}", "report_deployed", {**doc["report_deployed"], "resources": [{"name": "db", "endpoint": endpoint}]}, ok) for why, endpoint, ok in (
                ("with a password", "postgres://app:hunter2@pg.example/claims", False),
                ("with a user alone", "postgres://app@pg.example/claims", False),
                ("with a query", "postgres://pg.example/claims?sslmode=require", False),
                ("with an empty query", "postgres://pg.example/claims?", False),
                ("with a fragment", "postgres://pg.example/claims#x", False),
                ("with a percent-escape", "postgres://pg.example/cl%61ims", False),
                ("with two path segments", "postgres://pg.example/claims/x", False),
                ("in an unknown scheme", "mysql://pg.example/claims", False),
                ("with a space", "postgres://pg.example/claims x", False),
                ("over 255 bytes", "postgres://" + "a" * 250 + ".example", False),
                ("with a port and a database", "postgres://pg.example:5432/claims_db", True),
                ("at an IP address", "postgres://10.0.4.7:5432/claims", True),
                ("of a bucket", "s3://claims-files", True),
                ("at a cluster's service name", "postgres://claims-db-rw:5432/app", True),
                ("at an IPv6 address", "postgres://[fd00::7]:5432/claims", True),
                ("at an upper-case IPv6 address", "postgres://[FD00::7]:5432/claims", False),
                ("at an upper-case host", "postgres://PG.example/claims", False),
                ("with a port's leading zero", "postgres://pg.example:05432/claims", False),
                ("naming a dot segment", "postgres://pg.example/..", False),
                # parity with the server's net/url parse: the schema takes nothing the route refuses (#2432)
                ("at an all-digit name", "postgres://2130706433/claims", False),
                ("at a name whose last label is all digits", "postgres://0x7f.1/claims", False),
                ("at a label of digits and a hyphen", "postgres://pg.1-2/claims", True),
                ("at a label of digits and a letter", "postgres://pg.1a/claims", True),
                ("at an IPv4 address", "postgres://10.0.4.7/claims", True),
                ("at an IPv4 address with a leading zero", "postgres://10.0.04.7/claims", False),
                ("at the highest port", "postgres://pg.example:65535/claims", True),
                ("past the highest port", "postgres://pg.example:65536/claims", False),
                ("at a label of 63 characters", "postgres://" + "a" * 63 + ".example/claims", True),
                ("at a label of 64 characters", "postgres://" + "a" * 64 + ".example/claims", False),
                ("at a last label of 64 characters", "postgres://pg." + "a" * 64 + "/claims", False))),
            ("a deployed report whose resource names no endpoint", "report_deployed", {**doc["report_deployed"], "resources": [{"name": "db"}]}, False),
            ("an address with a query", "report_deployed", {**doc["report_deployed"], "address": "https://claims.apps.meridian.example/?token=x"}, False),
            ("an address with a userinfo", "report_deployed", {**doc["report_deployed"], "address": "https://u@claims.apps.meridian.example"}, False),
            ("an address ending in a slash", "report_deployed", {**doc["report_deployed"], "address": "https://claims.apps.meridian.example/"}, True),
            ("an address with one path segment", "report_deployed", {**doc["report_deployed"], "address": "https://apps.meridian.example/claims"}, True),
            ("an address with segments of unreserved characters", "report_deployed", {**doc["report_deployed"], "address": "https://apps.meridian.example/Claims/v2.1~x/"}, True),
            ("an address at a one-label host", "report_deployed", {**doc["report_deployed"], "address": "https://claims"}, True),
            ("an address at an upper-case host", "report_deployed", {**doc["report_deployed"], "address": "https://Claims.apps.meridian.example"}, False),
            ("an address at an upper-case IPv6 address", "report_deployed", {**doc["report_deployed"], "address": "https://[FD00::7]/claims"}, False),
            ("an address with an empty segment", "report_deployed", {**doc["report_deployed"], "address": "https://apps.meridian.example//claims"}, False),
            ("an address with a backslash", "report_deployed", {**doc["report_deployed"], "address": "https://apps.meridian.example/a\\b"}, False),
            ("an address with a dot segment", "report_deployed", {**doc["report_deployed"], "address": "https://apps.meridian.example/a/../b"}, False),
            ("an address ending in a dot segment", "report_deployed", {**doc["report_deployed"], "address": "https://apps.meridian.example/."}, False),
            ("an address with a port's leading zero", "report_deployed", {**doc["report_deployed"], "address": "https://apps.meridian.example:000443/"}, False),
            ("an address with a dotted segment name", "report_deployed", {**doc["report_deployed"], "address": "https://apps.meridian.example/v2.1/.well-known"}, True),
            ("an address at an all-digit name", "report_deployed", {**doc["report_deployed"], "address": "https://2130706433/"}, False),
            ("an address at 0x7f.1", "report_deployed", {**doc["report_deployed"], "address": "https://0x7f.1/"}, False),
            ("an address past the highest port", "report_deployed", {**doc["report_deployed"], "address": "https://claims.apps.meridian.example:70000/"}, False),
            ("an address at an IPv4 address and port", "report_deployed", {**doc["report_deployed"], "address": "https://10.0.4.7:8443/"}, True),
            ("an address at a label of 63 characters", "report_deployed", {**doc["report_deployed"], "address": "https://" + "a" * 63 + ".example/"}, True),
            ("an address at a label of 64 characters", "report_deployed", {**doc["report_deployed"], "address": "https://" + "a" * 64 + ".example/"}, False)):
        if accepts(schema, {**doc, key: value}) != ok:
            errors.append(f"selftest: a release fixture with {name} was {'refused' if ok else 'accepted'}")
    ap_doc = load(CONTRACTS / "testdata" / "app-page" / "claims-portal.json")
    provided = next(r for r in ap_doc["resources"] if r["state"] == "provided")
    for name, row, ok in (("a provided resource without its endpoint", {k: v for k, v in provided.items() if k != "endpoint"}, False),
                          ("a provided resource without its env", {k: v for k, v in provided.items() if k != "env"}, False),
                          ("an endpoint on a resource Canyon made", {**provided, "state": "ready"}, False),
                          ("a provided endpoint with a password", {**provided, "endpoint": "s3://key:secret@claims-files"}, False)):
        if accepts(ap_path, {**ap_doc, "resources": [row]}) != ok:
            errors.append(f"selftest: an app-page resource with {name} was {'refused' if ok else 'accepted'}")


def seam3_repo():
    """The deploy repo (design/HAND-OFF.md §3b; routes.md, The deploy repo; #2470): what the schema cannot say.
    canyon.json is the release's own, under Canyon's URL, and the committed one is it; a PUT's absent keys are
    the read's defaults; the files Canyon commits are the contract's set, every object in the release's
    namespace, no token value in any; a pull request is the deploy repo's. The gitops package renders the same
    files byte for byte (gitops/render_test.go)."""
    doc = load(CONTRACTS / "testdata" / "release" / "meridian.json")
    schema = CONTRACTS / "schema" / "release.schema.json"
    deploy, marker = doc["deploy"], doc["canyon_json"]
    issuer = deploy["app_env"]["CANYON_IDENTITY_ISSUER"]
    want = {"canyon": issuer, "app": deploy["app"], "env": deploy["environment"], "release": deploy["release"],
            "gate_run": deploy["gate_run"], "package": deploy["package"]["id"], "version": deploy["package"]["label"]}
    if marker != want:
        errors.append(f"repo: canyon_json is the release's own under Canyon's URL, {want}")
    env_dir = CONTRACTS / "testdata" / "release" / "repo" / "apps" / deploy["app"] / deploy["environment"]
    if load(env_dir / "canyon.json") != marker:
        errors.append("repo: the committed canyon.json is canyon_json")
    files = sorted(str(p.relative_to(env_dir)) for p in env_dir.rglob("*") if p.is_file())
    expected = sorted(["canyon.json", "deployment.yaml", "service.yaml", "networkpolicy.yaml", "externalsecret.yaml", "route.yaml"]
                      + [f"resources/{r['name']}.yaml" for r in deploy["resources"]])
    if files != expected:
        errors.append(f"repo: Canyon commits {expected}, the fixture holds {files}")
    ns = f"canyon-{deploy['app']}-{deploy['environment']}"
    access = f"{deploy['app']}-{deploy['environment']}-access-{deploy['release'].removeprefix('rel_')}"
    for name in files:
        if not name.endswith(".yaml"):
            continue
        text = (env_dir / name).read_text()
        docs = [d for d in text.split("\n---\n") if d.strip()]
        if sum(1 for d in docs if f"\n  namespace: {ns}\n" in "\n" + d) != len(docs):
            errors.append(f"repo: an object of {name} is not in the release's namespace, {ns}")
        if "redacted" in text or "kind: Namespace\n" in text:
            errors.append(f"repo: {name} holds a token value or a Namespace object")
    deployment = (env_dir / "deployment.yaml").read_text()
    if f"name: {access}\n" not in deployment or deployment.count("CANYON_ACCESS_TOKEN") != 2:
        errors.append(f"repo: the Deployment reads CANYON_ACCESS_TOKEN from {access}, key and name, and nothing else of it")
    # #2528: the pod runs on nodes of its image's architecture, the release's platform, and of nothing else.
    if f"\n      nodeSelector:\n        kubernetes.io/arch: {deploy['platform']['architecture']}\n      volumes:" not in deployment:
        errors.append(f"repo: the Deployment's pod is pinned to kubernetes.io/arch {deploy['platform']['architecture']}, the release's platform, alone")
    if f"key: {deploy['release']}\n" not in (env_dir / "externalsecret.yaml").read_text():
        errors.append("repo: the ExternalSecret fetches the release's own access token, keyed by its id")
    # A PUT's absent keys are the read's defaults; the save and the read are the same repo.
    put, saved, got = doc["repo_put"], doc["repo_saved"], doc["repo_get"]
    defaults = {"username": "x-access-token", "api": "github", "api_url": "https://api.github.com", "store": "canyon", "root": "apps", "ca": None}
    for k, v in defaults.items():
        if saved[k] != put.get(k, v):
            errors.append(f"repo: repo_saved.{k} is the PUT's or {v!r}")
    for k in ("url", "username", "preview", "prod", "root", "api", "api_url", "store", "ca", "set_by", "set_at"):
        if saved[k] != got[k]:
            errors.append(f"repo: repo_get.{k} is what the save answered")
    pr = doc["env_pending_repo"]["pending"]["pull_request"]
    if not pr["url"].startswith(got["url"].removesuffix(".git") + "/pull/") or not pr["url"].endswith(f"/{pr['number']}"):
        errors.append("repo: pending.pull_request is the deploy repo's own, its url ending in its number")
    # The schema refuses what the platform-repo routes refuse.
    for name, key, value, ok in (
            ("a repo url with a user and password", "repo_put", {**put, "url": "https://u:p@github.com/meridian/deploy"}, False),
            ("a repo url with a query", "repo_put", {**put, "url": "https://github.com/meridian/deploy?x=1"}, False),
            ("a repo over http", "repo_put", {**put, "url": "http://github.com/meridian/deploy"}, False),
            ("production through a pull request with no pulls API", "repo_put", {**put, "api": "none"}, False),
            ("production pushed with no pulls API", "repo_put", {**put, "api": "none", "prod": {"branch": "main", "mode": "push"}}, True),
            ("a root that climbs out", "repo_put", {**put, "root": "../etc"}, False),
            ("a store that is not a DNS label", "repo_put", {**put, "store": "Canyon Store"}, False),
            ("a read that carries the token", "repo_get", {**got, "token": "redacted"}, False),
            ("a check that passed with problems", "repo_check", {**doc["repo_check"], "passed": True}, False),
            ("a problem outside the codes", "repo_check", {**doc["repo_check"], "problems": [{"code": "remote_said", "message": "remote: denied"}]}, False),
            ("a pull request on a webhook release", "env_pending", {**doc["env_pending_repo"], "pending": {**doc["env_pending_repo"]["pending"], "via": "webhook"}}, False),
            ("a pending with no via", "env_pending", {**doc["env_pending"], "pending": {k: v for k, v in doc["env_pending"]["pending"].items() if k != "via"}}, False)):
        if accepts(schema, {**doc, key: value}) != ok:
            errors.append(f"selftest: a release fixture with {name} was {'refused' if ok else 'accepted'}")


def custom_rules_snapshot(meridian):
    """snapshot.md, Custom rules, on a snapshot with three: two checked by AI, one on and one off, and one
    on checked by code. What the schema cannot say: at most 20; the custom-rules agent is carried exactly
    while one checked by AI is on, after canyon-review, which is first, and claims the custom namespace
    alone; no instructions or plain-words check carry a live key."""
    path = CONTRACTS / "schema" / "snapshot.schema.json"
    snap = load(CONTRACTS / "testdata" / "snapshot" / "custom-rules.json")
    if not schema_check("snapshot: custom rules", path, snap):
        return None

    def problems(doc):
        customs = [r for r in doc["rules"] if r["id"].startswith("custom.")]
        ids = [a["id"] for a in doc["agents"]]
        out = []
        if len(customs) > 20:
            out.append(f"{len(customs)} custom rules; a host takes 20")
        if ("custom-rules" in ids) != any((r["state"] != "off" or r.get("production")) and r["check"]["by"] == "ai" for r in customs):
            out.append("the custom-rules agent is carried exactly while a custom rule checked by AI is on")
        if ids[:1] != ["canyon-review"] or "custom-rules" in ids and doc["agents"][ids.index("custom-rules")]["namespace"] != ["custom"]:
            out.append("canyon-review is the first agent, and custom-rules claims the custom namespace alone")
        if any("custom" in a["namespace"] for a in doc["agents"] if a["id"] != "custom-rules"):
            out.append("an agent other than custom-rules claims the custom namespace")
        if any(LIVE_KEY.search(r["instructions"]) or LIVE_KEY.search(r["check"].get("says", "")) for r in customs):
            out.append("a custom rule's instructions or its check's plain words carry a live key")
        return out

    for p in problems(snap):
        errors.append(f"snapshot: custom rules: {p}")
    if {r["id"] for r in snap["rules"]} - {r["id"] for r in snap["rules"] if r["id"].startswith("custom.")} != {r["id"] for r in meridian["rules"]}:
        errors.append("snapshot: custom rules: the fixture is meridian's rules and its custom rules")
    on = next(r for r in snap["rules"] if r["id"].startswith("custom.") and r["state"] != "off" and r["check"]["by"] == "ai")
    by_code = next(r for r in snap["rules"] if r["id"].startswith("custom.") and r["check"]["by"] == "code")
    rule = lambda **kw: {**on, **kw}
    for name, mutate, schema_refuses in (
            ("instructions on a rule that is not custom", lambda d: d["rules"][0].update(instructions="x"), True),
            ("a custom rule without instructions", lambda d: next(r for r in d["rules"] if r["id"] == on["id"]).pop("instructions"), True),
            ("a slug over 40", lambda d: d["rules"].append(rule(id="custom." + "a" * 41, state="off")), True),
            ("a custom rule checked by AI a scanner produces", lambda d: d["rules"].append(rule(id="custom.other", producer="scanner", state="off")), True),
            ("instructions over 2,000", lambda d: d["rules"].append(rule(id="custom.other", instructions="x" * 2001, state="off")), True),
            ("a 21st custom rule", lambda d: d["rules"].extend(rule(id="custom.r" + chr(97 + i // 26) + chr(97 + i % 26), state="off") for i in range(19)), False),
            ("a control character in instructions", lambda d: d["rules"].append(rule(id="custom.other", instructions="a\x01b", state="off")), True),
            ("a C1 control character in instructions", lambda d: d["rules"].append(rule(id="custom.other", instructions="a\x85b", state="off")), True),
            ("a newline in a custom rule's name", lambda d: d["rules"].append(rule(id="custom.other", name="a\nb", state="off")), True),
            ("a live key in instructions", lambda d: d["rules"].append(rule(id="custom.other", instructions="use sk_live_" + "Q" * 24, state="off")), False),
            ("custom-rules claiming perf too", lambda d: d["agents"][1].update(namespace=["custom", "perf"]), False),
            ("canyon-review claiming custom", lambda d: d["agents"][0].update(namespace=["perf", "custom"]), False),
            ("no custom-rules agent while one is on", lambda d: d.update(agents=d["agents"][:1]), False),
            ("the agent while every one is off", lambda d: [r.update(state="off") for r in d["rules"] if r["id"].startswith("custom.")], False),
            ("the agent while only those checked by code are on", lambda d: [r.update(state="off") for r in d["rules"] if r["id"].startswith("custom.") and r["check"]["by"] == "ai"], False),
            ("a custom rule without its check", lambda d: next(r for r in d["rules"] if r["id"] == on["id"]).pop("check"), True),
            ("a check on a rule that is not custom", lambda d: d["rules"][0].update(check={"by": "ai", "facts_version": 2}), True),
            ("a code check the AI reviews", lambda d: next(r for r in d["rules"] if r["id"] == by_code["id"]).update(producer="llm-review"), True),
            ("an AI check a scanner runs", lambda d: next(r for r in d["rules"] if r["id"] == on["id"]).update(producer="scanner"), True),
            ("a code check that says nothing", lambda d: next(r for r in d["rules"] if r["id"] == by_code["id"])["check"].pop("says"), True),
            ("an AI check with an expression", lambda d: next(r for r in d["rules"] if r["id"] == on["id"])["check"].update(expr="true"), True),
            ("a live key in a check's plain words", lambda d: next(r for r in d["rules"] if r["id"] == by_code["id"])["check"].update(says="use sk_live_" + "Q" * 24), False),
            ("custom-rules before canyon-review", lambda d: d.update(agents=d["agents"][::-1]), False)):
        probe = copy.deepcopy(snap)
        mutate(probe)
        refused = not accepts(path, probe) if schema_refuses else bool(problems(probe))
        if not refused:
            errors.append(f"selftest: a snapshot with {name} was not refused")
    return snap


def custom_rules_policies(snap, finding_schema):
    """routes.md, the custom-rules row: the list is the snapshot's custom rules; a saved version answers
    what it was sent, and its audit row's after is that, its before present exactly past version 1;
    the no-model refusal names a custom rule. And seam 1: a custom rule's finding is high."""
    doc = load(CONTRACTS / "testdata" / "policies" / "meridian.json")
    customs = {r["id"]: r for r in snap["rules"] if r["id"].startswith("custom.")}

    def problems(doc):
        out = []
        listed = {c["rule"]: (c["name"], c["instructions"]) for c in doc["custom_rules"]["custom_rules"]}
        if listed != {i: (r["name"], r["instructions"]) for i, r in customs.items()}:
            out.append("GET /custom-rules does not list the snapshot's custom rules, their names and instructions")
        put = doc["custom_rule_put"]
        got, detail = put["response"]["custom_rule"], put["audit"]["detail"]
        if (got["rule"] != "custom." + put["slug"] or {k: got[k] for k in ("name", "instructions")} != put["request"] or detail["rule"] != got["rule"] or
                detail["version"] != got["version"] or detail["name"]["after"] != got["name"] or detail["instructions"]["after"] != got["instructions"] or
                ("before" in detail["name"]) != (got["version"] > 1) or ("before" in detail["instructions"]) != (got["version"] > 1)):
            out.append("the custom rule saved does not answer what was sent, or its audit row's before and after do not")
        if doc["no_model"]["rule"] not in customs or doc["no_model"]["response"]["message"] != NO_MODEL or NO_MODEL not in route_row("PUT /api/v1/policies/:rule"):
            out.append("the no_model refusal names a rule that is not a custom rule, or not in routes.md's words")
        return out

    for p in problems(doc):
        errors.append(f"policies: {p}")
    put = lambda d: d["custom_rule_put"]
    for name, mutate in (("a list missing a rule", lambda d: d["custom_rules"]["custom_rules"].pop()),
                         ("a save answering another name", lambda d: put(d)["response"]["custom_rule"].update(name="Other")),
                         ("a version 2 audit row with no before", lambda d: put(d)["audit"]["detail"]["name"].pop("before")),
                         ("a no_model on a built-in rule", lambda d: d["no_model"].update(rule="perf.unbounded-refetch")),
                         ("a no_model in other words", lambda d: d["no_model"]["response"].update(message="no model"))):
        probe = copy.deepcopy(doc)
        mutate(probe)
        if not problems(probe):
            errors.append(f"selftest: the custom-rules samples with {name} were not refused")
    on_custom = on_rule(customs)
    # seam 1: a custom rule's finding, high; a lower one is refused.
    golden = load(CONTRACTS / "testdata" / "finding" / "claims-portal.sarif.json")
    res = copy.deepcopy(golden["runs"][1]["results"][0])  # a reviewer result: a custom rule's is one too
    res["ruleId"], res["properties"]["canyon"]["policy_name"] = on_custom["id"], on_custom["name"]
    log = {"version": golden["version"], "$schema": golden["$schema"], "runs": [{"columnKind": golden["runs"][0]["columnKind"],
           "tool": {"driver": {"name": "custom-rules", "version": golden["runs"][1]["tool"]["driver"]["version"],
                               "rules": [{"id": res["ruleId"], "name": res["properties"]["canyon"]["policy_name"]}]}}, "results": [res]}]}
    for severity, note, ok in (("high", "custom rules · dropped 1 invalid result(s)", True), ("medium", None, False),
                               ("high", "ai review · dropped 1 invalid result(s)", False)):
        probe = copy.deepcopy(log)
        if note:
            probe["runs"][0]["invocations"] = [{"executionSuccessful": True, "toolExecutionNotifications": [{"level": "note", "message": {"text": note}}]}]
        c = probe["runs"][0]["results"][0]["properties"]["canyon"]
        c["severity"], c["blocks"] = severity, severity == "high"
        probe["runs"][0]["results"][0]["level"] = LEVEL[severity]
        probe["runs"][0]["results"][0]["partialFingerprints"]["canyon/v1"] = fingerprints(probe)[id(probe["runs"][0]["results"][0])]
        mark = len(errors)
        check_log("selftest", probe, snap, finding_schema)
        passed = len(errors) == mark
        del errors[mark:]
        if passed != ok:
            errors.append(f"selftest: a custom rule's finding at {severity}, noted {note!r}, was {'refused' if ok else 'accepted'}")


# routes.md, the policies row: a custom rule turned on with no model on the host, in these words.
NO_MODEL = "No model is configured on this Canyon host, so a custom rule can't run. Ask whoever runs it to set one up."


def on_rule(customs):
    """The custom rule the fixture has on."""
    return next(r for r in customs.values() if r["state"] != "off")


# ---------------------------------------------------------------- seam 3 — database sessions (design/DATABASE-SESSIONS.md)
DB_LIFETIME = {"preview": 8 * 3600, "prod": 3600}   # §5: never longer, and never past the sign-in it rides on
DB_CHECK = {"kind": "canyon", "subject": "db", "display": "Canyon"}
DB_CHECKS = ("signed_out", "access_changed", "setting_changed", "database_gone", "expired", "held", "idle", "turned_off")
DB_OPENS = ("db session opened", "db write session opened")
DB_UNITS = {"ns": 1e-9, "us": 1e-6, "µs": 1e-6, "μs": 1e-6, "ms": 1e-3, "s": 1, "m": 60, "h": 3600}


def go_duration(text):
    """Seconds in a duration as Go's time.ParseDuration reads it (unsigned, as the schema's pattern allows)."""
    return sum(float(n) * DB_UNITS[u] for n, u in re.findall(r"([0-9]*\.?[0-9]+|[0-9]+\.)(ns|us|µs|μs|ms|s|m|h)", text))



def seam3_licence():
    """The Enterprise licence (#2879): the golden file, the payload its base64 holds, GET's reads to an admin and to a
    member, PUT's body, and the licence_required refusal. Only a licence in force unlocks anything, and a member's read
    says nothing of the licence."""
    golden = load(CONTRACTS / "testdata" / "licence" / "licence.json")
    schema = CONTRACTS / "schema" / "licence.schema.json"
    if golden is None or not schema_check("licence file", schema, golden["file"]):
        return
    # The signature is over the payload's exact bytes, so the file carries them in base64 and never the object.
    raw = base64.b64decode(golden["file"]["payload"], validate=True)
    if json.loads(raw) != golden["payload"]:
        errors.append("licence: the golden file's payload is not the golden payload")
    if len(base64.b64decode(golden["file"]["signature"], validate=True)) != 64:
        errors.append("licence: the golden signature is not 64 bytes, an Ed25519 signature's length")
    if not takes(schema, "payload", golden["payload"]):
        errors.append("licence: the golden payload is not the payload's shape")
    for name in ("admin", "member", "lapsed", "invalid", "none"):
        if not takes(schema, "state", golden[name]):
            errors.append(f"licence: the golden {name} read is not GET /api/v1/licence's shape")
    if not takes(schema, "set", golden["set"]) or json.loads(golden["set"]["licence"]) != golden["file"]:
        errors.append("licence: the golden set is not PUT /api/v1/licence's body holding the golden file")
    if not takes(schema, "refused", golden["refused"]):
        errors.append("licence: the golden refusal is not licence_required's shape")
    admin, member = golden["admin"], golden["member"]
    no = lambda doc, *keys: {k: v for k, v in doc.items() if k not in keys}
    for name, definition, probe in (
            ("a file carrying the payload as an object", None, dict(golden["file"], payload=golden["payload"])),
            ("a file naming a key id", None, dict(golden["file"], key_id="2026")),
            ("a payload with a field Canyon didn't sign for", "payload", dict(golden["payload"], hosts=["canyon.meridian.example"])),
            ("a payload with no seats", "payload", no(golden["payload"], "seats")),
            ("a payload naming no feature", "payload", dict(golden["payload"], features=[])),
            ("a state the contract doesn't name", "state", dict(member, state="expired")),
            ("a lapsed licence unlocking a feature", "state", dict(golden["lapsed"], allows=["outbound_keys"])),
            ("an invalid licence unlocking a feature", "state", dict(golden["invalid"], allows=["group_sync"])),
            ("no licence unlocking a feature", "state", dict(golden["none"], allows=["platform_patching"])),
            ("a feature this server doesn't gate, unlocked", "state", dict(member, allows=["audit_export"])),
            ("a member's read naming the customer", "state", dict(member, customer="Meridian Insurance")),
            ("a member's read naming the expiry", "state", dict(member, expires=admin["expires"])),
            ("a member's read saying why a licence is invalid", "state", dict(golden["none"], state="invalid", problem="unsigned")),
            ("no licence with a source", "state", dict(golden["none"], source="env")),
            ("an admin's read of a licence with no expiry", "state", no(admin, "expires")),
            ("an admin's read of a licence with no grace end", "state", no(admin, "grace_until")),
            ("an admin's read of an invalid licence saying no why", "state", no(golden["invalid"], "problem")),
            ("an invalid licence naming its customer", "state", dict(golden["invalid"], customer="Meridian Insurance")),
            ("a licence that verifies with a problem", "state", dict(admin, problem="unsigned")),
            ("a problem the contract doesn't name", "state", dict(golden["invalid"], problem="expired")),
            ("a set carrying the file as an object", "set", {"licence": golden["file"]}),
            ("a refusal naming no feature", "refused", no(golden["refused"], "feature")),
            ("a refusal naming a feature this server doesn't gate", "refused", dict(golden["refused"], feature="scim")),
            ("a refusal under another code", "refused", dict(golden["refused"], error="license_required"))):
        if (accepts(schema, probe) if definition is None else takes(schema, definition, probe)):
            errors.append(f"selftest: licence: {name} was accepted")


def seam3_telemetry(snap):
    """The daily telemetry report (#2807): the golden report, GET's state around it, and PUT's body. A rule
    is named by a built-in id the snapshot carries or counted as custom; no field names the org."""
    golden = load(CONTRACTS / "testdata" / "telemetry" / "report.json")
    schema = CONTRACTS / "schema" / "telemetry.schema.json"
    report = golden["report"]
    if not schema_check("telemetry report", schema, report):
        return
    # The read answers each report as a JSON string of its exact bytes (#2807's web review): the string, parsed, is the
    # report, which JSON Schema's contentSchema says but never checks, so it is checked here.
    sent = json.dumps(report, separators=(",", ":"))
    state = dict(golden["state"], report=sent, last_report=sent)
    if not takes(schema, "state", state):
        errors.append("telemetry: the golden state is not GET /api/v1/telemetry's shape")
    if not schema_check("telemetry state's report, parsed", schema, json.loads(state["report"])):
        return
    if takes(schema, "state", dict(state, report=report)):
        errors.append("telemetry: the state took a report as an object, not the string of its exact bytes")
    if not takes(schema, "set", golden["set"]):
        errors.append("telemetry: the golden set is not PUT /api/v1/telemetry's body")
    builtin = {r["id"] for r in snap["rules"]} if snap else set()
    for rule in report["rules_fired"]:
        if rule != "custom" and builtin and rule not in builtin:
            errors.append(f"telemetry: rules_fired names {rule}, which is no built-in rule: every other rule is custom")
    # The names are an enum: a custom rule's own name, or an imported tool's, is refused by the seam itself (#2807).
    for name in ("custom.acme-payroll-scan", "sarif.acme-scanner", "canyon-review"):
        if accepts(schema, dict(report, rules_fired={name: 1})):
            errors.append(f"telemetry: rules_fired took {name}, which firedAs never yields")
    # A field the schema has not named is refused, at every level: the report can only grow by a schema change.
    for probe, where in ((dict(report, app="claims-portal"), "the report"), (dict(report, checks=dict(report["checks"], host="x")), "checks")):
        if accepts(schema, probe):
            errors.append(f"telemetry: {where} took a field the schema doesn't name")
    if takes(schema, "state", dict(state, enabled=True)):
        errors.append("telemetry: a state that is on still says why it is off")
    # A count is at most 1,000,000: a crafted 3000000000 would overflow the collector's sums (#2808's review). JSON
    # Schema reads 1.0 and 1e5 as integers by value, so a count's spelling is the collector's check, not this one.
    for probe, where in ((dict(report, apps=3000000000), "apps"), (dict(report, checks=dict(report["checks"], passed=1000001)), "checks.passed"),
                         (dict(report, rules_fired={"custom": 1000001}), "rules_fired"), (dict(report, promotions=-1), "promotions")):
        if accepts(schema, probe):
            errors.append(f"telemetry: {where} took a count out of 0 to 1,000,000")
    # The second version (#2953): period_hours marks it, and then every v2 field is there, so a v2 server that drops
    # one is refused, while a first-version report, which has none of them, is still taken.
    v2 = golden["report_v2"]
    if not schema_check("telemetry report, second version", schema, v2):
        return
    # agents_via (design/specs/mcp.md §7) came after the second version shipped, so a second-version server from before
    # it sends none: it is optional, the one field a report with period_hours may leave out.
    optional = {"period_hours", "agents_via"}
    added = [k for k in v2 if k not in report]
    for field in added:
        if field not in optional and accepts(schema, {k: v for k, v in v2.items() if k != field}):
            errors.append(f"telemetry: a second-version report without {field} was taken")
    if not accepts(schema, {k: v for k, v in v2.items() if k != "agents_via"}):
        errors.append("telemetry: a second-version report from before agents_via, without it, was refused")
    # Reviews by how they were asked for, never by whom: a client's name is refused, and so is a count left out.
    via = v2["agents_via"]
    for probe, where in ((dict(via, cursor=1), "a client's name"), ({k: n for k, n in via.items() if k != "mcp_remote"}, "no mcp_remote"),
                         (dict(via, cli=1000001), "a count past 1,000,000")):
        if accepts(schema, dict(v2, agents_via=probe)):
            errors.append(f"telemetry: agents_via took {where}")
    if not accepts(schema, {k: v for k, v in v2.items() if k not in added}):
        errors.append("telemetry: a first-version report, without period_hours and what it brings, was refused")
    # The licence is its state alone (the owner, 2026-11-15): what it unlocks, its seats, its dates and whose it is never go.
    for extra in ("features", "seats", "expires", "customer"):
        if accepts(schema, dict(v2, licence=dict(v2["licence"], **{extra: 1}))):
            errors.append(f"telemetry: licence took {extra}, which would join an install to a named customer")
    # Sizes are buckets, never exact: a small install can't be told apart by them.
    for probe, where in ((dict(v2, people_total=7), "people_total"), (dict(v2, outbound=dict(v2["outbound"], routes=3)), "outbound.routes")):
        if accepts(schema, probe):
            errors.append(f"telemetry: {where} took an exact size, not a bucket")
    if accepts(schema, dict(v2, failures={"error.acme": 1})) or accepts(schema, dict(v2, stacks={"cobol": 1})):
        errors.append("telemetry: failures or stacks took a key the schema doesn't name")


def seam3_db_sessions():
    """The database-sessions routes, routes.md (Database sessions), and provisioner.md's /people. What the schema
    cannot say: a read lives no longer than its env allows, and each later read of it never has less activity; a
    read's person has no address only as the anonymous admin; the
    query's rows fit its columns, its limit and its timeout's bounds; every call is answered the database session it
    asked for, an attach the open one; the list is every open one, oldest first, as its open row recorded it, yours
    the reader's; the trail opens each once by a person at contributor or above under the org's db_prod, a prod
    write by an owner or an admin and, under prod_approval, in the dashboard, attaches only to an open one as its
    person, one row per sign-in, and closes it at most once by the actor its reason names; one person role per person and database; and
    /people answers within the TTL."""
    doc = load(CONTRACTS / "testdata" / "db-session" / "meridian.json")
    schema = CONTRACTS / "schema" / "db-session.schema.json"
    if not schema_check("db-session", schema, doc):
        return
    org, team, query, audit = doc["org"], doc["team"], doc["preview_query"], doc["audit"]
    calls = {name: doc[name] for name in ("attach", "prod_read", "prod_write", "prod_write_attach")}
    listed = doc["list"]["db_sessions"]
    reads = [query["answer"]["db_session"], *(c["answer"] for c in calls.values()), *listed]
    # 1. A read lives no longer than its env allows, its activity inside its life, its person without an address only
    #    as the anonymous admin of a host with sign-in off; each later read of the same
    #    database session (the answers, then the list) never has less activity than an earlier one.
    for r in reads:
        if r["app"] != team["app"]:
            errors.append(f"db-session: {r['id']} names {r['app']}, the fixture's app is {team['app']}")
        if not 0 < ts_seconds(r["expires_at"]) - ts_seconds(r["opened_at"]) <= DB_LIFETIME[r["env"]]:
            errors.append(f"db-session: {r['id']} expires more than {DB_LIFETIME[r['env']] // 3600} h after it opened, past {r['env']}'s lifetime")
        if not r["opened_at"] <= r["last_activity_at"] <= r["expires_at"]:
            errors.append(f"db-session: {r['id']}'s last activity is outside its life")
        if r["person"]["email"] == "" and r["person"]["subject"] != "anonymous":
            errors.append(f"db-session: {r['id']}'s person has no address, which only the anonymous admin of a host with sign-in off has")
    counters = ("last_activity_at", "connections", "queries", "bytes_from_db", "bytes_to_db")
    seen = {}
    for r in reads:
        if (before := seen.get(r["id"])) and any(r[k] < before[k] for k in counters):
            errors.append(f"db-session: a later read of {r['id']} has less activity than an earlier one")
        seen[r["id"]] = r
    # 2. The query: its rows are its columns wide, counted, within its limit, cut short only at the limit or 10 MiB;
    #    rows_affected only on a write; its timeout within 1 s to 5 min.
    req, ans = query["request"], query["answer"]
    limit = req.get("limit", 500)
    if any(len(row) != len(ans["columns"]) for row in ans["rows"]) or ans["row_count"] != len(ans["rows"]) or ans["row_count"] > limit:
        errors.append("db-session: the query's rows are its columns wide, row_count of them, at most its limit")
    if ans["truncated"] and ans["row_count"] < limit and len(json.dumps(ans["rows"]).encode()) < 10 * 1024 * 1024:
        errors.append("db-session: the query says truncated with fewer rows than its limit and under 10 MiB")
    if ("rows_affected" in ans) != (req["mode"] == "write"):
        errors.append("db-session: a query answers rows_affected exactly when it ran in write mode")
    if "timeout" in req and not 1 <= go_duration(req["timeout"]) <= 300:
        errors.append(f"db-session: the query's timeout {req['timeout']} is outside 1s to 5m")
    s = ans["db_session"]
    if (s["env"], s["resource"]) != (req["env"], req.get("resource", s["resource"])) or s["mode"] not in (req["mode"], "write"):
        errors.append("db-session: the query ran in a database session of another env, resource or mode than it asked")
    if len(req["sql"].encode()) > 64 * 1024:
        errors.append("db-session: the query's sql is over 64 KiB")
    # 3. Every call is answered the database session it asked for, the caller's own: an attach 200 and the one
    #    already open, an open 201 and a new one, its reason the request's.
    for name, call in calls.items():
        q, a = call["request"], call["answer"]
        if (a["env"], a["mode"], a["resource"]) != (q["env"], q["mode"], q.get("resource", a["resource"])) or not a["yours"]:
            errors.append(f"db-session: {name} is answered a database session of another env, mode or resource than it asked, or not the caller's")
        new = call["status"] == 201
        if new and a.get("reason") != q.get("reason"):
            errors.append(f"db-session: {name} opens a database session whose reason is not the request's")
        if (a["opened_at"] != a["last_activity_at"]) if new else (a["opened_at"] >= a["last_activity_at"]):
            errors.append(f"db-session: {name} answers {call['status']} but {a['id']} was {'already open' if new else 'not open'}")
    if calls["attach"]["answer"]["id"] != s["id"] or calls["attach"]["status"] != 200:
        errors.append("db-session: the attach answers 200 and the database session the query opened")
    if calls["prod_write_attach"]["answer"]["id"] != calls["prod_write"]["answer"]["id"] or calls["prod_write_attach"]["status"] != 200:
        errors.append("db-session: the CLI's prod write attaches to the one the dashboard opened")
    # dashboard_required judges only an open of a new prod write: under prod_approval one is a browser's, and an
    # attach from any of the person's sign-ins passes it.
    for name, call in calls.items():
        if org["prod_approval"] and call["status"] == 201 and (call["request"]["env"], call["request"]["mode"]) == ("prod", "write") and call["flow"] != "browser":
            errors.append(f"db-session: {name} opens a prod write from a {call['flow']} sign-in under prod_approval")
    # 4. The trail: each database session opened once, by a person at contributor or above, under db_prod for prod, a
    #    prod write by an owner or an admin and, under prod_approval, in the dashboard; attached to only while open,
    #    by its person; closed at most once, naming its person and its duration, by the actor its reason names.
    errors.extend(miscategorised("db-session", audit))
    opens, closes, roles, attaches = {}, {}, {}, set()
    allowed = {"off": (), "read": ("read",), "write": ("read", "write")}[org["db_prod"]]
    for e in audit:
        d, actor = e["detail"], e["actor"]
        if e["project"] != team["app"]:
            errors.append(f"db-session: {e['id']} names {e['project']}, the fixture's app is {team['app']}")
        if e["action"] in DB_OPENS or e["action"] == "db session attached":
            if actor["kind"] != "person":
                errors.append(f"db-session: {e['id']} is a {actor['kind']}'s; only a person opens or attaches (person_required)")
            if d["env"] == "prod" and d["mode"] not in allowed:
                errors.append(f"db-session: {e['id']} reaches prod {d['mode']} while db_prod is {org['db_prod']}")
            if (d["via_tool"] == "dashboard") != (e["via"] == "dashboard" and d["via_tool"] != "query"):
                errors.append(f"db-session: {e['id']} is via_tool {d['via_tool']} via {e['via']}: dashboard is a browser sign-in's open or attach, connect any other")
        if e["action"] in DB_OPENS:
            if d["db_session"] in opens:
                errors.append(f"db-session: {d['db_session']} is opened twice")
            opens[d["db_session"]] = e
            who = actor["subject"]
            if who not in team["owners"] + team["contributors"] + team["admins"]:
                errors.append(f"db-session: {e['id']} opens a database session for {who}, below contributor")
            if (d["env"], d["mode"]) == ("prod", "write"):
                if who not in team["owners"] + team["admins"]:
                    errors.append(f"db-session: {e['id']} opens a prod write for {who}, who is not an owner or an admin")
                if org["prod_approval"] and (d["via_tool"], e["via"]) != ("dashboard", "dashboard"):
                    errors.append(f"db-session: {e['id']} opens a prod write outside the dashboard under prod_approval")
            if not 0 < ts_seconds(d["expires_at"]) - ts_seconds(e["ts"]) <= DB_LIFETIME[d["env"]]:
                errors.append(f"db-session: {e['id']}'s database session outlives {d['env']}'s lifetime")
            key = (who, d["env"], d["resource"])
            if roles.setdefault(key, d["role"]) != d["role"] or any(k != key and v == d["role"] for k, v in roles.items()):
                errors.append(f"db-session: {e['id']}'s role {d['role']} is not one person role per person and database")
            continue
        o = opens.get(d["db_session"])
        if o is None or d["db_session"] in closes:
            errors.append(f"db-session: {e['id']} {e['action'].split()[-1]} {d['db_session']}, which is not open")
            continue
        if e["action"] == "db session attached":
            if actor != o["actor"] or any(d[k] != o["detail"][k] for k in ("env", "resource", "mode")):
                errors.append(f"db-session: {e['id']} attaches to {d['db_session']} as another person, env, resource or mode")
            if (d["db_session"], e["session"]) in attaches or e["session"] == o.get("session"):
                errors.append(f"db-session: {e['id']} is a second row of {d['db_session']} for one sign-in: one per database session and sign-in, the opener's its open row")
            attaches.add((d["db_session"], e["session"]))
            continue
        closes[d["db_session"]] = e
        if d["person"] != {k: o["actor"][k] for k in ("subject", "display")} or d["duration_s"] != ts_seconds(e["ts"]) - ts_seconds(o["ts"]):
            errors.append(f"db-session: {e['id']} names another person or duration than {d['db_session']}'s open row")
        # The actor is who acted (§10): Canyon for a check, the person for their own close and replace, an owner or an admin who ends another's.
        r = d["reason"]
        if r in DB_CHECKS:
            ok = actor == DB_CHECK
        elif r == "ended":
            ok = actor["kind"] == "person" and actor["subject"] != d["person"]["subject"] and actor["subject"] in team["owners"] + team["admins"]
        else:
            ok = actor == o["actor"]
        if not ok:
            errors.append(f"db-session: {e['id']} closes {d['db_session']} as {r!r} by {actor['subject']}, not the actor that reason names")
        if r == "expired" and e["ts"] < o["detail"]["expires_at"]:
            errors.append(f"db-session: {e['id']} closes {d['db_session']} as expired before its expires_at")
        if r == "held" and (o["detail"]["env"], o["detail"]["mode"]) != ("prod", "write"):
            errors.append(f"db-session: {e['id']} closes a {o['detail']['env']} {o['detail']['mode']} database session as held: the hold closes prod writes")
        if r == "setting_changed" and o["detail"]["env"] != "prod":
            errors.append(f"db-session: {e['id']} closes a preview database session as setting_changed: db_prod governs prod")
        if r == "replaced" and not any(n["ts"] == e["ts"] and n["actor"] == o["actor"] and (n["detail"]["env"], n["detail"]["resource"]) == (o["detail"]["env"], o["detail"]["resource"])
                                       and n["detail"]["mode"] != o["detail"]["mode"] for n in audit if n["action"] in DB_OPENS):
            errors.append(f"db-session: {e['id']} replaces {d['db_session']} with no open of the other mode in the same transaction")
    reasons = load(schema)["definitions"]["close_reason"]["enum"]
    if missing := set(reasons) - {e["detail"]["reason"] for e in closes.values()}:
        errors.append(f"db-session: the trail closes no database session as {sorted(missing)}")
    for name, call in calls.items():
        a = call["answer"]
        # An attach from the opener's sign-in writes no row; one from any other sign-in has its row, at or before it.
        o = opens.get(a["id"])
        if call["status"] == 200 and (o is None or (o["session"] != call["session"] and not any(
                e["action"] == "db session attached" and e["detail"]["db_session"] == a["id"] and e["session"] == call["session"] and e["ts"] <= a["last_activity_at"] for e in audit))):
            errors.append(f"db-session: {name} attaches from a sign-in that neither opened it nor has a db session attached row")
        # A prod request carries a reason exactly where it writes a row (an open, another sign-in's first attach), and the row holds it.
        wrote = next((e for e in audit if e["action"] == "db session attached" and e["detail"]["db_session"] == a["id"] and e["session"] == call["session"] and e["ts"] == a["last_activity_at"]), None)
        if a["env"] == "prod" and wrote and wrote["detail"].get("reason") != call["request"].get("reason"):
            errors.append(f"db-session: {name} is a sign-in's first attach to a prod database session, and its row does not carry its reason")
    # 5. The list is every database session open on the trail, oldest first, each as its open row recorded it and
    #    yours exactly when it is the reader's.
    if [r["id"] for r in listed] != [i for i, e in sorted(opens.items(), key=lambda p: p[1]["ts"]) if i not in closes]:
        errors.append("db-session: the list is every open database session, oldest first, and no closed one")
    if any(r["yours"] != (r["person"]["subject"] == team["reader"]) for r in listed):
        errors.append("db-session: a listed database session's yours is not whether it is the reader's")
    for r in reads:
        o = opens.get(r["id"])
        rec = o and {**{k: o["detail"].get(k) for k in ("env", "resource", "mode", "role", "reason", "expires_at")},
                     "opened_at": o["ts"], "person": {**{k: o["actor"][k] for k in ("subject", "display")}, "email": r["person"]["email"]}}
        if rec is None or rec != {k: r.get(k) for k in rec}:
            errors.append(f"db-session: {r['id']} is not read as its open row recorded it")
    # 6. /people: a login that expires after it was asked for and within the asked TTL.
    p = doc["people"]
    left = ts_seconds(p["answer"]["login"]["expires_at"]) - ts_seconds(p["asked_at"])
    if not 0 < left <= p["request"]["ttl_seconds"]:
        errors.append(f"db-session: /people answered a login expiring {left:.0f} s after it was asked for, the TTL is {p['request']['ttl_seconds']} s")
    # 7. The schemas refuse what the contract refuses.
    read = doc["prod_read"]["answer"]
    login = p["answer"]["login"]
    shut = next(e for e in audit if e["action"] == "db session closed")
    with_query = lambda **kw: {**doc, "preview_query": {**query, "request": {**req, **kw}}}
    for name, probe, ok in (
            ("the golden", doc, True),
            ("a read carrying a password", {**doc, "prod_read": {**doc["prod_read"], "answer": {**read, "password": "x"}}}, False),
            ("a prod read without a reason", {**doc, "prod_read": {**doc["prod_read"], "answer": {k: v for k, v in read.items() if k != "reason"}}}, False),
            ("a reason over 500 characters", {**doc, "prod_read": {**doc["prod_read"], "answer": {**read, "reason": "x" * 501}}}, False),
            ("a reason of spaces", {**doc, "prod_read": {**doc["prod_read"], "answer": {**read, "reason": "   "}}}, False),
            ("a reason of a zero-width space", {**doc, "prod_read": {**doc["prod_read"], "answer": {**read, "reason": "​"}}}, False),
            ("a reason with a bidi override", {**doc, "prod_read": {**doc["prod_read"], "answer": {**read, "reason": "fix ‮invoices"}}}, False),
            ("a close reason outside the enum", {**doc, "audit": [{**shut, "detail": {**shut["detail"], "reason": "logout"}}]}, False),
            ("a /people answer with sslmode require", {**doc, "people": {**p, "answer": {"login": {**login, "sslmode": "require"}}}}, False),
            ("a /people host carrying credentials", {**doc, "people": {**p, "answer": {"login": {**login, "host": "postgres://dana:pw@claims-prod.example"}}}}, False),
            ("a /people host with a port", {**doc, "people": {**p, "answer": {"login": {**login, "host": "claims-prod.example:5433"}}}}, False),
            ("a /people user that is a URL", {**doc, "people": {**p, "answer": {"login": {**login, "user": "postgres://x:y@h"}}}}, False),
            ("a /people host that is an IPv6 literal", {**doc, "people": {**p, "answer": {"login": {**login, "host": "2001:db8::5"}}}}, True),
            ("a /people database that is not an identifier", {**doc, "people": {**p, "answer": {"login": {**login, "database": "claims?sslmode=disable"}}}}, False),
            ("a query limit over 10,000", with_query(limit=10001), False),
            ("a query timeout of 1.5s", with_query(timeout="1.5s"), True),
            ("a query timeout of 5m0s", with_query(timeout="5m0s"), True),
            ("a query timeout that is not a duration", with_query(timeout="5 minutes"), False),
            ("a startup naming the user", {**doc, "relay": {**doc["relay"], "startup": {"user": "postgres"}}}, False),
            ("a startup asking replication", {**doc, "relay": {**doc["relay"], "startup": {"replication": "database"}}}, False),
            ("a cancel without a process id", {**doc, "relay": {**doc["relay"], "cancel": {}}}, False),
            ("a relay closed for an unknown reason", {**doc, "relay": {**doc["relay"], "closes": [{"code": 4000, "reason": "bye"}]}}, False),
            ("session_open without its mode", {**doc, "refusals": [{"status": 409, "body": {"error": "session_open", "message": "m"}}]}, False),
            ("which_database without its resources", {**doc, "refusals": [{"status": 409, "body": {"error": "which_database", "message": "m"}}]}, False),
            ("session_ended without its reason", {**doc, "refusals": [{"status": 410, "body": {"error": "session_ended", "message": "m"}}]}, False),
            ("held answered 403", {**doc, "refusals": [{"status": 403, "body": {"error": "held", "message": "m"}}]}, False)):
        if accepts(schema, probe) != ok:
            errors.append(f"selftest: db-session with {name} was {'refused' if ok else 'accepted'}")
    for name, probe, ok in (
            ("query_failed with its sqlstate", {"error": "query_failed", "message": "relation \"claim\" does not exist", "sqlstate": "42P01"}, True),
            ("query_failed without its sqlstate", {"error": "query_failed", "message": "relation \"claim\" does not exist"}, False),
            ("invalid_sql with a sqlstate", {"error": "invalid_sql", "message": "cannot insert multiple commands into a prepared statement", "sqlstate": "42601"}, False)):
        if takes(CONTRACTS / "schema" / "db-query.schema.json", "refusal", probe) != ok:
            errors.append(f"selftest: a db query refusal, {name}, was {'refused' if ok else 'accepted'}")
    for text, ok in (("30s", True), ("1.5s", True), ("5m0s", True), ("1s", True), ("500ms", False), ("10m", False), ("300001ms", False)):
        if (1 <= go_duration(text) <= 300) != ok:
            errors.append(f"selftest: the query timeout {text} was {'refused' if ok else 'accepted'}")


snapshot = seam5()
finding = seam1(snapshot) if snapshot else None
fleet_tiles = seam3()          # the fleet read owns the tile vocabulary; seam 2's rows carry it
seam3_access()
seam3_hold()
seam3_needs()
if finding:
    fixture_app(finding[0])
    seam2(snapshot, *finding, fleet_tiles)
if snapshot:
    seam4()
    seam4_stopped()
    seam4_rollback()
if finding:
    seam3_app(snapshot, finding[0])
seam3_dependency_approval()
if snapshot:
    seam3_guardrails(snapshot)
    seam3_policies(snapshot)
    seam3_pipeline(snapshot)
    effective_snapshot(snapshot)
    seam3_platform(snapshot)
    seam3_built_app()
    seam3_built_app_approval()
    seam3_patch()
    customs = custom_rules_snapshot(snapshot)
    if customs and finding:
        custom_rules_policies(customs, finding[1])
seam3_secrets()
seam3_integrations()
seam3_front_door()
seam3_release()
seam3_repo()
seam3_egress()
seam4_egress_ca()
seam3_db_sessions()
seam3_telemetry(snapshot)
seam3_licence()
access_lines()

if errors:
    print("\n".join(errors), file=sys.stderr)
    sys.exit(1)
print("contracts valid")
