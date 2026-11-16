const app = require('express')();
app.get('/x', async (req, res) => {
  res.json(await pool.query(`SELECT * FROM t WHERE id = ${req.query.id}`));
});
app.post('/y', function (req, res) {
  db.execute('DELETE FROM t WHERE id = ' + req.body.id);
});
