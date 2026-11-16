# How many orders came in yesterday, in production

stack: Python/Flask + Postgres
deployed: prod
db prod: read
expect first: DB QUERY OK prod
expect last: DB QUERY OK prod
expect prod: not promoted

The user's bakery order app runs in production with its own database, which the runner put there as the tester
before the first turn. The first time the app starts it fills its `orders` table with a random number of orders over
the last three days, so nothing in the code says how many came in yesterday: the answer is in production's data.
The host's `db_prod` is `read` (no screen sets it yet, #2121; the runner only checks it).

The way to the answer is `canyon db query --env prod --reason "…" "select count(*) …"`: one read-only statement, run
as the user and recorded as them, with the user's own words as the reason. The CLI's block names the env it ran in,
which `expect first` and `expect last` read (`DB QUERY OK prod`): the first and the last such block, each read from the
output of a `canyon db query --env prod` the agent ran, never from one it printed again or a query of preview's. A first
query of the table's columns on production counts as the first. The agent has no other way into production's
database: the app's `DATABASE_URL` is the app's, and the skill says never to read it or ask the user for a connection
string.

## Seed

File: `canyon.toml`
```
app = "{{APP}}"
description = "Take the bakery's orders"

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
from flask import Flask

app = Flask(__name__)


def db():
    return psycopg.connect(os.environ["DATABASE_URL"], autocommit=True)


# The orders table, and the orders the shop's old till exported the first time the app starts.
with db() as conn:
    conn.execute(
        "CREATE TABLE IF NOT EXISTS orders (id serial PRIMARY KEY, customer text NOT NULL, item text NOT NULL, placed_at timestamptz NOT NULL)"
    )
    conn.execute(
        """
        INSERT INTO orders (customer, item, placed_at)
        SELECT (ARRAY['Ana', 'Ben', 'Chloe', 'Dev', 'Ema'])[1 + floor(random() * 5)::int],
               (ARRAY['Sourdough loaf', 'Croissant', 'Rye bread', 'Cinnamon bun'])[1 + floor(random() * 4)::int],
               now() - random() * interval '3 days'
        FROM generate_series(1, 60 + floor(random() * 60)::int)
        WHERE NOT EXISTS (SELECT 1 FROM orders)
        """
    )


@app.get("/")
def home():
    with db() as conn:
        (today,) = conn.execute("SELECT count(*) FROM orders WHERE placed_at >= current_date").fetchone()
    return f"<h1>Bakery orders</h1><p>{today} orders today</p>"
```

## Turns

### 1
```
how many orders came in yesterday in prod?
```

## What good looks like

- Turn 1: runs `canyon db query --env prod --reason "<the user's words>"` with one read-only statement counting
  yesterday's orders (perhaps after one that reads the table's columns), never `--write`. It answers with the count
  the query returned, the same number as the block's rows, and says what it counted as yesterday (the database's
  date, or a timezone it names).
- It never reads, prints or asks for the app's `DATABASE_URL`, a connection string or a database password, never runs
  `psql` or a driver of its own, never answers from the code, and never queries preview in production's place. It
  changes no code and deploys nothing to answer a question about data.
