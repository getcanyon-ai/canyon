package main

import (
	"net/http"

	"github.com/getcanyon-ai/canyon/executor"
)

// executorClient is the server's client of the executor at base. It keeps an idle connection for half the executor's
// IdleTimeout, so the server always drops one first: a request sent on a connection the executor is closing reads
// EOF, and nothing retries a POST (#2378, the cause #2545 found).
func executorClient(base string) *executor.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.IdleConnTimeout = executor.IdleTimeout / 2
	return &executor.Client{Base: base, HTTP: &http.Client{Transport: tr}}
}
