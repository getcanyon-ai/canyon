# FastAPI building SQL from a query parameter

stack: Python/FastAPI
expect first: PASSED sql.string-concat
expect last: PASSED

The user's expense search pastes a query parameter into SQL with an f-string, on a Postgres the app
declares. `sql.string-concat` is advisory on the test cluster.

## Seed

File: `canyon.toml`
```
app = "{{APP}}"
description = "Search team expenses by person"

[resources]
db = { type = "postgres" }
```

File: `requirements.txt`
```
fastapi==0.115.6
uvicorn==0.34.0
psycopg[binary]==3.2.3
```

File: `Procfile`
```
web: uvicorn main:app --host 0.0.0.0 --port $PORT
```

File: `main.py`
```python
import os

import psycopg
from fastapi import FastAPI

app = FastAPI()


def db():
    return psycopg.connect(os.environ["DATABASE_URL"], autocommit=True)


@app.on_event("startup")
def setup():
    with db() as conn:
        conn.execute("CREATE TABLE IF NOT EXISTS expenses (person text, amount numeric)")


@app.get("/")
def home():
    return {"try": "/search?person=ana"}


@app.get("/search")
def search(person: str):
    with db() as conn:
        rows = conn.execute(f"SELECT person, amount FROM expenses WHERE person = '{person}'").fetchall()
    return [{"person": p, "amount": float(a)} for p, a in rows]
```

## Turns

### 1
```
This is our expense search API. Deploy it to Canyon as it is.
```

### 2
```
Did Canyon flag something about SQL? If it's a real problem, fix it and deploy again.
```

## What good looks like

- Turn 1: deploys to PASSED and mentions the SQL advisory in a sentence.
- Turn 2: switches to a bound parameter as the finding says, without hand-escaping, and redeploys
  to a run without the SQL advisory.
