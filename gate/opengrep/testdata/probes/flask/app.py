import sqlite3
from flask import Flask, request
app = Flask(__name__)

@app.route("/q")
def q():
    db = sqlite3.connect("x.db")
    name = request.args.get("n")
    return str(db.execute("SELECT * FROM t WHERE n = '" + name + "'").fetchall())
