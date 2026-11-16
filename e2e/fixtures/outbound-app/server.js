// CASES.md O1: an app read through its logs alone, so nobody signs in to it. It calls target.txt's URL through
// Node's own fetch, which leaves through the proxy variables, and dials the same host and port straight,
// which does not, and says each answer on a line of its own: at its start and every 10 seconds.
const { readFileSync } = require("node:fs");
const { createServer } = require("node:http");
const { connect } = require("node:net");
const { join } = require("node:path");

const target = new URL(readFileSync(join(__dirname, "target.txt"), "utf8").trim());
const port = Number(target.port) || (target.protocol === "https:" ? 443 : 80);

async function call() {
  try {
    return String((await fetch(target, { redirect: "manual", signal: AbortSignal.timeout(8000) })).status);
  } catch (err) {
    return err.cause?.code ?? err.name; // never a number: that word is an answer from the target alone
  }
}

// dial is e2e/fixtures/gen.sh's /probe: one TCP connect, and "timeout" is the real socket timeout alone.
function dial() {
  return new Promise((resolve) => {
    const socket = connect({ host: target.hostname, port, timeout: 3000 });
    const done = (result) => {
      socket.destroy();
      resolve(result);
    };
    socket.on("connect", () => done("connected"));
    socket.on("timeout", () => done("timeout"));
    socket.on("error", (err) => {
      if (err.code === "ECONNREFUSED") return done("refused");
      if (err.code === "ENOTFOUND" || err.code === "EAI_AGAIN") return done("unresolved");
      done(err.code ?? "error");
    });
  });
}

async function probe() {
  console.log(`outbound fetch ${await call()}`);
  console.log(`outbound dial ${await dial()}`);
}

probe();
setInterval(probe, 10000);
createServer((_, res) => res.end("ok\n")).listen(process.env.PORT || 3000);
