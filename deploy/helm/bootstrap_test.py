"""The chart's bootstrap (canyon/files/bootstrap.py), its front-door step against a stub of Canyon's API (#2514).

An unchanged door that is not enforcing is saved again with no forceUpdate. That save is not a change: refused by
the probe (409 probe_failed) or unable to run it (any 5xx, the executor still rolling out after the upgrade), it
leaves the door as it was, not enforcing, so nothing builds (fail closed), and the Job ends 0 with Canyon's
message, so the upgrade completes. Every other answer fails the Job: another 409, any other 4xx, a changed door
refused, and forceUpdate of an enforcing door refused. Standard library only; helm.yml runs it.
"""
import contextlib
import http.server
import importlib.util
import io
import json
import os
import pathlib
import tempfile
import threading
import unittest

DOOR = {"choice": 1, "gateway": "alb", "alb_arn": "arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/kind/0123456789abcdef",
        "domain": "apps.example.test", "form": "app.env.domain"}
NOTE = "Not checked on this cluster: Canyon builds nothing here until a save finds NetworkPolicy enforced."


class Canyon(http.server.BaseHTTPRequestHandler):
    """GET /api/v1/front-door answers the stored door; PUT answers what the case set, and is counted."""

    stored, put, puts = None, (200, {}), 0

    def answer(self, status, body):
        raw = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):  # noqa: N802, the handler's name
        self.answer(200, {"front_door": Canyon.stored, "probe_note": "" if (Canyon.stored or {}).get("enforcing") else NOTE})

    def do_PUT(self):  # noqa: N802
        self.rfile.read(int(self.headers["Content-Length"]))
        Canyon.puts += 1
        self.answer(*Canyon.put)

    def log_message(self, *_):
        pass


class FrontDoor(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Canyon)
        threading.Thread(target=cls.server.serve_forever, daemon=True).start()
        url = f"http://127.0.0.1:{cls.server.server_port}"
        cfg = tempfile.NamedTemporaryFile("w", suffix=".json", delete=False)
        json.dump({"pocketId": {"url": ""}, "canyon": {"url": url}, "frontDoor": DOOR}, cfg)
        cfg.close()
        os.environ.update(CANYON_BOOTSTRAP_CONFIG=cfg.name, STATIC_API_KEY="unused")
        path = pathlib.Path(__file__).parent / "canyon" / "files" / "bootstrap.py"
        spec = importlib.util.spec_from_file_location("bootstrap", path)
        cls.bootstrap = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(cls.bootstrap)

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()

    def run_step(self, stored, put, force=False):
        """The step's log, and its exit message when it fails the Job (None when it ends 0)."""
        Canyon.stored, Canyon.put, Canyon.puts = stored, put, 0
        self.bootstrap.CFG["forceFrontDoor"] = force
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            try:
                self.bootstrap.set_front_door()
            except SystemExit as e:
                return out.getvalue(), str(e.code)
        return out.getvalue(), None

    def test_an_enforcing_door_unchanged_is_left(self):
        log, failed = self.run_step({**DOOR, "enforcing": True}, (500, {}))
        self.assertIsNone(failed)
        self.assertEqual(Canyon.puts, 0, log)

    def test_a_door_not_enforcing_is_saved_again(self):
        log, failed = self.run_step({**DOOR, "enforcing": False}, (200, {"front_door": {**DOOR, "enforcing": True}}))
        self.assertIsNone(failed, log)
        self.assertEqual(Canyon.puts, 1, log)

    def test_a_heal_refused_by_the_probe_ends_0_quoting_its_reason(self):
        reason = "The certificate at canyon-probe.preview.apps.example.test does not cover it."
        log, failed = self.run_step({**DOOR, "enforcing": False}, (409, {"error": "probe_failed", "message": reason}))
        self.assertIsNone(failed, log)
        self.assertIn(reason, log)  # the probe's own cause, never told as another
        self.assertIn(NOTE, log)

    def test_a_heal_whose_probe_cannot_run_ends_0(self):
        for status, error in ((502, "probe_unavailable"), (500, "internal"), (503, "unavailable")):
            with self.subTest(status=status):
                log, failed = self.run_step({**DOOR, "enforcing": False}, (status, {"error": error, "message": "the executor is rolling out"}))
                self.assertIsNone(failed, log)
                self.assertIn(error, log)
                self.assertIn("the executor is rolling out", log)

    def test_any_other_refusal_of_a_heal_fails_the_job(self):
        for status, error in ((409, "not_kubernetes"), (409, "signin_unsupported"), (400, "invalid_front_door"), (401, "unauthorized"), (403, "admin_required")):
            with self.subTest(status=status, error=error):
                log, failed = self.run_step({**DOOR, "enforcing": False}, (status, {"error": error, "message": "no"}))
                self.assertIsNotNone(failed, log)

    def test_a_changed_door_refused_fails_the_job(self):
        for status in (409, 502):
            with self.subTest(status=status):
                log, failed = self.run_step({**DOOR, "domain": "old.example.test", "enforcing": False}, (status, {"error": "probe_failed", "message": "no"}))
                self.assertIsNotNone(failed, log)

    def test_a_forced_save_of_an_enforcing_door_refused_fails_the_job(self):
        for status in (409, 502):
            with self.subTest(status=status):
                log, failed = self.run_step({**DOOR, "enforcing": True}, (status, {"error": "probe_failed", "message": "no"}), force=True)
                self.assertIsNotNone(failed, log)


if __name__ == "__main__":
    unittest.main()
