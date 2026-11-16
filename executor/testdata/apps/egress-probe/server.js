require("node:http")
  .createServer((_, res) => res.end("ok\n"))
  .listen(process.env.PORT || 3000);
