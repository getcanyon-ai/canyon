// The tour's claims tracker (canyon demo tour, chapter 1): it lists every claim. Its claims live in
// SQLite, in this process's memory, so it needs no database beside it.
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
  res.json(db.prepare("SELECT * FROM claims").all());
});

app.listen(process.env.PORT || 3000);
