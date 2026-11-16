// endorsement-hub: the demo's sample app, a stand-in Canyon builds and runs like any other.
const http = require("node:http");

http.createServer((_, res) => {
  res.writeHead(200, { "content-type": "text/html; charset=utf-8" });
  res.end("<!doctype html><title>endorsement-hub</title><h1>endorsement-hub</h1><p>A sample app on the Canyon demo.</p>");
}).listen(process.env.PORT || 3000);
