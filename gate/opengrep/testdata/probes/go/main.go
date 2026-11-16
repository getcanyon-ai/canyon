package main

import (
	"database/sql"
	"net/http"
)

func handler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, _ := db.Query("SELECT * FROM claims WHERE owner = '" + r.URL.Query().Get("owner") + "'")
		defer rows.Close()
	}
}
