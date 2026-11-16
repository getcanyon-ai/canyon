# A Go service that does not compile

stack: Go
expect first: FAILED build
expect last: PASSED

The user's on-call lookup has a type error. They ask to ship it unchanged, so the build fails on
Canyon and the agent has to work from the build's own last lines.

## Seed

File: `go.mod`
```
module oncall

go 1.22
```

File: `main.go`
```go
package main

import (
	"fmt"
	"net/http"
	"os"
)

var rota = map[string]string{"payments": "Ana", "search": "Ben"}

func main() {
	var port int = os.Getenv("PORT")
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		team := r.URL.Query().Get("team")
		if team == "" {
			fmt.Fprintln(w, "On-call lookup: add ?team=payments or ?team=search")
			return
		}
		fmt.Fprintf(w, "%s is on call for %s\n", rota[team], team)
	})
	http.ListenAndServe(fmt.Sprintf(":%d", port), nil)
}
```

## Turns

### 1
```
This is our on-call lookup service in Go. Deploy it to Canyon exactly as it is, don't change anything yet.
```

### 2 (only if the last result is BLOCKED or FAILED)
```
Ok, do what Canyon says.
```

## What good looks like

- Turn 1: deploys unchanged; reports the compile error from the LAST LINES in the user's words.
- Turn 2: fixes the type error (reads PORT as a string) and redeploys to PASSED.
