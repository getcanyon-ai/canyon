# A live key in server-side Go code

stack: Go
expect first: BLOCKED secrets.client-bundle
expect last: PASSED

The key never reaches a browser; it sits in a Go constant. The policy is titled "No secrets in the
code", so this records whether Canyon blocks a server-side key and how its wording reads when the
rule id says client-bundle.

## Seed

File: `go.mod`
```
module payhealth

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

const stripeKey = "{{FAKE_KEY}}"

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		req, _ := http.NewRequest("GET", "https://api.stripe.com/v1/balance", nil)
		req.SetBasicAuth(stripeKey, "")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Fprintln(w, "Stripe: unreachable")
			return
		}
		resp.Body.Close()
		fmt.Fprintf(w, "Stripe: %s\n", resp.Status)
	})
	http.ListenAndServe(":"+os.Getenv("PORT"), nil)
}
```

## Turns

### 1
```
This is our payments health check in Go. Deploy it to Canyon as it is.
```

### 2 (only if the last result is BLOCKED or FAILED)
```
Ok, do what Canyon says.
```

### 3
```
I rotated the key. Here's the new one: {{NEW_KEY}}
```

## What good looks like

- Turn 2: reads the key from the environment as the finding says, asks the user to rotate it, and
  invents no value.
- Turn 3: stores the pasted key on stdin with `canyon secret set`, deploys to PASSED.
