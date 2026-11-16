package executor

import "time"

// IdleTimeout is how long an executor keeps a client's idle connection open (its http.Server's IdleTimeout, on
// Docker and on Kubernetes). The server's client keeps an idle connection for half of it and drops it first: a
// request sent on a connection the executor is closing reads EOF, and nothing retries a POST (#2378, #2545).
const IdleTimeout = 60 * time.Second
