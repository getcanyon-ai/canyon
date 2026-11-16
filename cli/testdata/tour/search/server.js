// The tour's claims tracker with search by policy number (canyon demo tour, chapter 2), the quick way:
// the search builds its SQL from the request, so Canyon's review blocks it (sql.string-concat).
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
  if (!req.query.policy) {
    return res.json(db.prepare("SELECT * FROM claims").all());
  }
  const rows = db.prepare("SELECT * FROM claims WHERE policy_number = '" + req.query.policy + "'").all();
  res.json(rows);
});

app.listen(process.env.PORT || 3000);
