const express = require('express');
const app = express();
app.get('/', (req, res) => res.send('shop'));
app.listen(process.env.PORT || 8080);
