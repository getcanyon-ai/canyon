// claims-portal's next version, as the demo tells it: a claims search whose SQL is built from the
// request, so the review blocks it (sql.string-concat). The demo never builds it: the review stops it
// first. Its claims live in SQLite, in this process's memory, so it needs no database beside it.
const express = require("express");
const { DatabaseSync } = require("node:sqlite");

const db = new DatabaseSync(":memory:");
db.exec(`
  CREATE TABLE claims (policy_number TEXT, claim TEXT, status TEXT);
  INSERT INTO claims VALUES
    ('MRD-1001', 'Rear bumper, car park collision', 'Open'),
    ('MRD-1002', 'Windscreen, stone chip', 'Settled'),
    ('MRD-1003', 'Side mirror, hit while parked', 'In review');
`);

const app = express();
app.use(express.static(__dirname + "/public"));

app.get("/claims", (req, res) => {
  const rows = db.prepare("SELECT * FROM claims WHERE policy_number = '" + req.query.policy + "'").all();
  res.json(rows);
});

app.listen(process.env.PORT || 3000);
