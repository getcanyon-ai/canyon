"""Release downloads against a stub of GitHub's (#2755): `make pack-fetch` (.github/fetch.sh), Dockerfile.executor's pack
stage and executor/opengrep.Dockerfile's fetch stage each take their files after two 504s, and refuse a file whose
sha256 is not the pinned one. Run from the repository's root, on Linux, with Docker: python3 .github/pack-download-test.py
"""

import hashlib
import http.server
import io
import os
import subprocess
import sys
import tarfile
import tempfile
import threading

VERSION = "0.40.3"


def archive(text: bytes) -> bytes:
    """A release archive as pack's: one executable file, pack."""
    out = io.BytesIO()
    with tarfile.open(fileobj=out, mode="w:gz") as tar:
        info = tarfile.TarInfo("pack")
        info.size, info.mode = len(text), 0o755
        tar.addfile(info, io.BytesIO(text))
    return out.getvalue()


RIGHT = archive(b"#!/bin/sh\necho pack stub\n")
WRONG = archive(b"#!/bin/sh\necho not the pinned pack\n")
SUM = hashlib.sha256(RIGHT).hexdigest()
asked: dict[str, int] = {}
leaked: list[str] = []


class Stub(http.server.BaseHTTPRequestHandler):
    """/flaky-<case>/<file> answers the case's first two requests 504, then the archive for any file; /wrong/<file>
    answers another archive at once."""

    def do_GET(self):
        case, _, name = self.path.lstrip("/").partition("/")
        asked[case] = asked.get(case, 0) + 1
        if self.headers.get("Authorization"):
            leaked.append(self.path)
        if not name:
            self.send_error(404)
        elif case.startswith("flaky") and asked[case] <= 2:
            self.send_error(504, "Gateway Timeout")
        else:
            body = WRONG if case == "wrong" else RIGHT
            self.send_response(200)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    def log_message(self, fmt, *args):
        print("stub:", fmt % args, file=sys.stderr)


def main() -> int:
    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Stub)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    base = f"http://127.0.0.1:{server.server_port}"
    pins = [f"PACK_SHA256_AMD64={SUM}", f"PACK_SHA256_ARM64={SUM}"]
    failed = []

    def check(what, ok, case, tries):
        if not ok or asked.get(case) != tries:
            failed.append(f"{what}: {'passed' if ok else 'failed'} after {asked.get(case, 0)} requests, want {tries}")

    with tempfile.TemporaryDirectory() as tmp:
        tgz = os.path.join(tmp, "pack.tgz")
        env = {k: v for k, v in os.environ.items() if k != "GH_TOKEN"}
        env["GH_TOKEN"] = "token-the-stub-must-never-see"  # sent to github.com alone
        make = lambda case: subprocess.run(["make", "pack-fetch", f"PACK_URL={base}/{case}", f"PACK_TGZ={tgz}", *pins], env=env).returncode == 0
        check("make pack-fetch after two 504s", make("flaky-make"), "flaky-make", 3)
        check("make pack-fetch of a file whose sha256 is not the pin", not make("wrong"), "wrong", 1)

    build = lambda case: subprocess.run(["docker", "build", "--network", "host", "--target", "pack", "--no-cache", "-f", "Dockerfile.executor",
                                         f"--build-arg=PACK_VERSION={VERSION}", f"--build-arg=PACK_URL={base}/{case}",
                                         *(f"--build-arg={p}" for p in pins), "."]).returncode == 0
    check("Dockerfile.executor's pack stage after two 504s", build("flaky-docker"), "flaky-docker", 3)
    asked.pop("wrong", None)
    check("Dockerfile.executor's pack stage of a file whose sha256 is not the pin", not build("wrong"), "wrong", 1)

    sums = [f"--build-arg={a}={SUM}" for a in ("OPENGREP_SHA256_AMD64", "OPENGREP_SHA256_ARM64", "OPENGREP_LICENSE_SHA256", "OPENGREP_EXCEPTION_SHA256")]
    opengrep = lambda case: subprocess.run(["docker", "build", "--network", "host", "--target", "fetch", "--no-cache", "-f", "executor/opengrep.Dockerfile",
                                            f"--build-arg=OPENGREP_RELEASE={base}/{case}", f"--build-arg=OPENGREP_SOURCE={base}/{case}", *sums, "."]).returncode == 0
    check("executor/opengrep.Dockerfile's fetch, its three files after two 504s", opengrep("flaky-opengrep"), "flaky-opengrep", 5)
    asked.pop("wrong", None)
    check("executor/opengrep.Dockerfile's fetch of a file whose sha256 is not the pin", not opengrep("wrong"), "wrong", 1)
    server.shutdown()
    if leaked:
        failed.append(f"GH_TOKEN sent to the stub, which is not github.com: {leaked}")
    for f in failed:
        print("FAIL:", f, file=sys.stderr)
    print("ok" if not failed else f"{len(failed)} failed")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
