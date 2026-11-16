// claims-portal: the demo's sample app, the version preview runs. Canyon builds and runs it like any other.
const http = require("node:http");

http.createServer((_, res) => {
  res.writeHead(200, { "content-type": "text/html; charset=utf-8" });
  res.end("<!doctype html><title>claims-portal</title><h1>claims-portal</h1><p>A sample app on the Canyon demo.</p>");
}).listen(process.env.PORT || 3000);
