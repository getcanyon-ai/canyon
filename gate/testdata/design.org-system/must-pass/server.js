const express = require("express");

const app = express();
app.get("/orders", (req, res) => res.json([]));
app.listen(process.env.PORT || 8080);
