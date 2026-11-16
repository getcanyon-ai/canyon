// Command app is the hand-off journey's app: a real one that keeps its data in the database the org's
// platform provided it, reached by the DATABASE_URL the platform put in its environment. GET / answers
// handoff; POST /notes?v=<value> keeps a value; GET /notes answers every value kept, one a line.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	ctx := context.Background()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		log.Fatal("app: DATABASE_URL is not set: the platform provided no database")
	}
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		log.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(2 * time.Second) { // the database may still be on its way
		if _, err = db.Exec(ctx, "CREATE TABLE IF NOT EXISTS notes (v text NOT NULL)"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			log.Fatalf("app: the database never answered: %v", err)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("handoff\n")) })
	mux.HandleFunc("POST /notes", func(w http.ResponseWriter, r *http.Request) {
		if _, err := db.Exec(r.Context(), "INSERT INTO notes (v) VALUES ($1)", r.URL.Query().Get("v")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /notes", func(w http.ResponseWriter, r *http.Request) {
		rows, err := db.Query(r.Context(), "SELECT v FROM notes")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var kept []string
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			kept = append(kept, v)
		}
		_, _ = w.Write([]byte(strings.Join(kept, "\n") + "\n"))
	})
	log.Fatal((&http.Server{Addr: ":3000", Handler: mux, ReadHeaderTimeout: 10 * time.Second}).ListenAndServe())
}
