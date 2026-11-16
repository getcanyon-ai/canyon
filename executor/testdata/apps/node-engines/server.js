const http = require("http");

http
  .createServer((req, res) => res.end(`running on node ${process.versions.node}\n`))
  .listen(process.env.PORT || 3000);
