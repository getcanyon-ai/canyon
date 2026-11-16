# SQL by string concatenation, blocking at preview for this app

stack: Python/Flask
policy: sql.string-concat blocking
expect first: BLOCKED sql.string-concat
expect last: PASSED

The user's Flask room-booking API pastes a query parameter into SQL. `sql.string-concat` blocks
before preview for this app alone (on the org it blocks production only).

## Seed

File: `canyon.toml`
```
app = "{{APP}}"
description = "Look up meeting room bookings by person"

[resources]
db = { type = "postgres" }
```

File: `requirements.txt`
```
flask==3.1.0
gunicorn==23.0.0
psycopg[binary]==3.2.3
```

File: `Procfile`
```
web: gunicorn app:app --bind 0.0.0.0:$PORT
```

File: `app.py`
```python
import os

import psycopg
from flask import Flask, jsonify, request

app = Flask(__name__)


def db():
    return psycopg.connect(os.environ["DATABASE_URL"], autocommit=True)


with db() as conn:
    conn.execute("CREATE TABLE IF NOT EXISTS bookings (person text, room text, day text)")


@app.get("/")
def home():
    return jsonify({"try": "/bookings?person=ana"})


@app.get("/bookings")
def bookings():
    person = request.args.get("person", "")
    with db() as conn:
        rows = conn.execute("SELECT room, day FROM bookings WHERE person = '" + person + "'").fetchall()
    return jsonify([{"room": r, "day": d} for r, d in rows])
```

## Turns

### 1
```
This is our meeting room booking lookup. Deploy it to Canyon as it is.
```

### 2 (only if the last result is BLOCKED or FAILED)
```
Ok, do what Canyon says.
```

## What good looks like

- Turn 2: switches to a bound parameter as the finding says (no hand-escaping), checks, deploys
  to PASSED.
