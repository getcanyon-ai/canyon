package main

import (
	"net/http"
	"testing"

	"github.com/getcanyon-ai/canyon/executor"
)

// The server drops an idle connection to the executor before the executor closes it (executor.IdleTimeout): a
// POST sent on a connection the executor is closing reads EOF, and nothing retries it (#2378, the cause #2545 found).
func TestTheServerDropsAnIdleExecutorConnectionFirst(t *testing.T) {
	c := executorClient("http://executor.test")
	var tr *http.Transport
	if c.HTTP != nil {
		tr, _ = c.HTTP.Transport.(*http.Transport)
	}
	if tr == nil || tr.IdleConnTimeout <= 0 || tr.IdleConnTimeout >= executor.IdleTimeout {
		kept := "http.DefaultTransport's 90s"
		if tr != nil {
			kept = tr.IdleConnTimeout.String()
		}
		t.Fatalf("the server keeps an idle executor connection %s; want more than 0 and less than the executor's %s", kept, executor.IdleTimeout)
	}
}
