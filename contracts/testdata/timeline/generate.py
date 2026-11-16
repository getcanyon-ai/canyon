#!/usr/bin/env python3
"""The recorded claims-portal run — the seam-2 golden fixture — derived from the seam-1 golden
SARIF log so the two cannot drift. contracts/validate.py imports rows() and checks them; the
JSON is not committed (1,152 lines, AGENTS rule 3). To materialise it next to this file:
    python3 contracts/testdata/timeline/generate.py
"""
import copy
import json
import pathlib

ROOT = pathlib.Path(__file__).resolve().parents[3]
GOLDEN = ROOT / "contracts" / "testdata" / "finding" / "claims-portal.sarif.json"
OUT = ROOT / "contracts" / "testdata" / "timeline" / "claims-portal-run.json"

PROJECT = "claims-portal"
RUN1, RUN2 = "gr_7kd2mq", "gr_9p4x1c"


def rows():
    log = json.loads(GOLDEN.read_text())
    only = lambda i: {"version": log["version"], "$schema": log["$schema"], "runs": [log["runs"][i]]}
    rows = []

    def row(scope, kind, data, run=None, sec=0):
        r = {"id": len(rows) + 1, "ts": f"2026-09-02T09:{31 + sec // 60:02d}:{sec % 60:02d}Z", "scope": scope}
        if run:
            r["run"] = run
        r.update({"project": PROJECT, "kind": kind, "data": data})
        rows.append(r)

    def gate(run, t, scanners_log, review_log, gitleaks_log, verdict, tiles, deploy=None):
        row("run", "run.state", {"state": "pending", "queue_position": 0}, run, t)
        row("org", "run.transition", {"project": PROJECT, "run": run, "state": "pending"}, None, t)
        row("org", "audit.appended", {"event_id": f"ev_{run[3:]}"}, None, t)  # the start's audit row commits with the start
        row("run", "run.state", {"state": "running"}, run, t + 1)
        if run == RUN1:  # the host's first scan: trivy waits while the executor downloads its database
            row("run", "scan.waiting", {"tool": "trivy", "note": "downloading its vulnerability database — the first scan on this host"}, run, t + 2)
            row("run", "progress", {"step": "scan", "tool": "trivy", "status": "queued", "position": 2}, run, t + 2)  # its turn in the host's scan queue
            row("run", "progress", {"step": "scan", "tool": "trivy", "status": "running"}, run, t + 3)
            row("run", "progress", {"step": "scan", "tool": "trivy", "status": "done", "duration_ms": 2100}, run, t + 5)  # the running time alone
        row("run", "producer.sarif", {"producer": "canyon/scanners", "sarif": scanners_log}, run, t + 5)
        row("run", "check.started", {"check": "ai-review"}, run, t + 5)
        row("run", "check.progress", {"check": "ai-review", "note": "reading 14 files…"}, run, t + 5)
        row("run", "check.completed", {"check": "ai-review", "status": "findings", "findings": 3, "duration_ms": 4200}, run, t + 5)  # the reviewer's two routes and the refetch
        row("run", "producer.sarif", {"producer": "canyon-review", "sarif": review_log}, run, t + 5)
        row("run", "producer.sarif", {"producer": "sarif/gitleaks", "sarif": gitleaks_log}, run, t + 5)  # a tool Canyon runs is its row, never a check
        row("run", "verdict", verdict, run, t + 6)
        end, app = t + 6, {"state": verdict["state"], "blocking": verdict["blocking"], "owner": "Dana Whitfield", "harness": "Claude Code"}
        if deploy:  # a deploying run builds and deploys after a clean verdict, and ends after its deploy row
            end, app["preview"] = deploy()
            row("org", "audit.appended", {"event_id": f"ev_dep{run[3:]}"}, None, end)  # the deploy's own audit row
        row("run", "run.state", {"state": verdict["state"]}, run, end)
        row("org", "run.transition", {"project": PROJECT, "run": run, "state": verdict["state"]}, None, end)
        row("org", "app.updated", {"project": PROJECT, "row": app}, None, end)
        row("org", "tiles.updated", {"tiles": tiles}, None, end)

    # Run 1 — the hero verdict: gitleaks' Stripe key blocks; the reviewer's three advisory findings do not.
    gate(RUN1, 0,
         only(0), only(1), only(2),
         {"state": "blocked", "blocking": 1, "warnings": 3, "findings": log},
         {"secrets_exposed": 2, "blocked": 2, "approved": 0, "never_reviewed": 5, "unowned": 2, "failed": 0, "refused": 0})

    # The fix lands ("Simulate Dana's agent pushing the fix"). Run 2 — the key is gone, the advisories remain, it deploys.
    clean = copy.deepcopy(log)
    clean["runs"][2]["results"] = []
    def deploy():  # the host's first build, too: it waited for Canyon's build image (executor.md, GET /builder's waited_ms)
        row("run", "build.image", {"waited_ms": 278000}, RUN2, 300)
        for line in ["[builder] Next.js 15.0.3 · detected", "[builder] pack build · paketo-buildpacks/nodejs", "[builder] image sha256:4f1c…e2a9 · 48s"]:
            row("run", "build.log", {"line": line}, RUN2, 300)
        url = "http://claims-portal.preview.localhost:7777"
        row("run", "deploy.completed", {"env": "preview", "url": url, "package": "pkg_7d3e8f", "image_digest": "sha256:4f1c0b9a7e2a9d3c"}, RUN2, 312)
        return 312, url
    gate(RUN2, 240,
         {"version": "2.1.0", "$schema": log["$schema"], "runs": [clean["runs"][0]]}, {"version": "2.1.0", "$schema": log["$schema"], "runs": [clean["runs"][1]]},
         {"version": "2.1.0", "$schema": log["$schema"], "runs": [clean["runs"][2]]},
         {"state": "clean", "blocking": 0, "warnings": 3, "findings": clean},
         {"secrets_exposed": 1, "blocked": 1, "approved": 0, "never_reviewed": 5, "unowned": 2, "failed": 0, "refused": 0}, deploy)

    return rows


if __name__ == "__main__":
    out = rows()
    OUT.write_text(json.dumps(out, indent=2) + "\n")
    print(f"wrote {OUT.relative_to(ROOT)}: {len(out)} rows")
