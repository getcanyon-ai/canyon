// Command canyon-collector receives the daily telemetry reports Canyon servers send (#2808). Everything it keeps is
// in this directory: the report as contracts/schema/telemetry.schema.json allows it and the time it arrived, for 13
// months. See deploy/collector for running it on one box.
package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags)
	if err := run(logger); err != nil {
		logger.Fatal(err)
	}
}

// run reads the environment, makes the table, and serves until a signal. CANYON_COLLECTOR_DATABASE_URL and
// CANYON_COLLECTOR_READ_KEY are required; CANYON_COLLECTOR_ADDR defaults to :8080; CANYON_COLLECTOR_PROXIED=1 reads
// the sender's address from the X-Forwarded-For the TLS front appends, for the limiter alone.
func run(logger *log.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	url, key := os.Getenv("CANYON_COLLECTOR_DATABASE_URL"), os.Getenv("CANYON_COLLECTOR_READ_KEY")
	if url == "" {
		return errors.New("CANYON_COLLECTOR_DATABASE_URL is required")
	}
	if err := readKeyOf(key); err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, tableSQL); err != nil {
		return err
	}
	schema, err := compileSchema()
	if err != nil {
		return err
	}
	s := pgStore{pool}
	go func() { // one pass a day: a report older than 13 months is deleted
		for t := time.NewTicker(day); ; {
			if err := s.prune(ctx); err != nil && ctx.Err() == nil {
				logger.Printf("canyon-collector: deleting old reports: %v", err)
			}
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	}()
	c := &collector{schema: schema, limit: newLimiter(time.Now), store: s, readKey: key, proxied: os.Getenv("CANYON_COLLECTOR_PROXIED") == "1", log: logger}
	failed := func(err error) { logger.Printf("canyon-collector: counting dropped reports: %v", err) }
	flushed := c.drops.flushing(ctx, s, flushEvery, failed)
	addr := os.Getenv("CANYON_COLLECTOR_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	srv := &http.Server{Addr: addr, Handler: c.routes(), ReadHeaderTimeout: headerTime, ReadTimeout: bodyTime, IdleTimeout: idleLimit, MaxHeaderBytes: maxHeader,
		ErrorLog: log.New(io.Discard, "", 0)} // net/http's own lines name the peer's address: none is written anywhere
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe() }()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), flushLimit)
	defer cancel()
	err = srv.Shutdown(shutdown)
	<-flushed                                            // the minute's write, if one is on its way, has returned and put back what it failed to write
	if ferr := c.drops.flush(shutdown, s); ferr != nil { // the last counts, once no request is left to add to them
		failed(ferr)
	}
	return err
}
