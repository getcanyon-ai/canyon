// The executor's e2e build step (contracts/executor.md, invariant 12), run by npm as the app's own postinstall:
// it opens TCP to each of PROBE_TARGETS, which a build must not reach, then to PROBE_PUBLIC, which it must, says
// what each did, and exits 1 unless every one held.
const net = require("node:net");

const open = (target) =>
  new Promise((resolve) => {
    const at = target.lastIndexOf(":");
    const socket = net.connect({ host: target.slice(0, at), port: Number(target.slice(at + 1)), timeout: 5000 });
    socket.on("connect", () => {
      socket.destroy();
      resolve("connected");
    });
    socket.on("timeout", () => {
      socket.destroy();
      resolve("timed out");
    });
    socket.on("error", (err) => resolve(err.code || String(err)));
  });

(async () => {
  const targets = (process.env.PROBE_TARGETS || "").split(",").filter(Boolean);
  const pub = process.env.PROBE_PUBLIC || "";
  if (targets.length === 0 || pub === "") {
    console.log("probe: PROBE_TARGETS and PROBE_PUBLIC are unset");
    process.exit(1);
  }
  let held = true;
  for (const target of targets) {
    const said = await open(target);
    if (said === "connected") {
      console.log(`probe: reached ${target}`);
      held = false;
    } else if (said === "timed out") {
      console.log(`probe: timed out ${target}`); // the floor rejects; a silence is something else
    } else {
      console.log(`probe: refused ${target} (${said})`);
    }
  }
  const said = await open(pub);
  if (said === "connected") {
    console.log(`probe: reached ${pub}`);
  } else {
    console.log(`probe: did not reach ${pub} (${said})`);
    held = false;
  }
  process.exit(held ? 0 : 1);
})();
