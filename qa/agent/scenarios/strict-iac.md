# Terraform that opens SSH to the world, blocking for this app

stack: Go
policy: iac.misconfig blocking
expect first: BLOCKED iac.misconfig
expect last: PASSED

A Go service ships the Terraform that sets up its network on the team's AWS account: a security
group that lets anyone reach port 22. checkov reads it (CKV_AWS_24, and nothing else in this
file), and `iac.misconfig` blocks for this app alone. It also ships the Dockerfile from its old
servers (root, port 22, no health check): under the same blocking rule its findings must stay
advisory, since a Dockerfile's never block (#1934) and Canyon builds with buildpacks.

## Seed

File: `go.mod`
```
module menu

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

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "Today: lentil soup")
	})
	http.ListenAndServe(":"+os.Getenv("PORT"), nil)
}
```

File: `infra/main.tf`
```
resource "aws_security_group" "menu" {
  name        = "canteen-menu"
  description = "Canteen menu service"

  ingress {
    description = "SSH for the on-call engineer"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_network_interface" "menu" {
  subnet_id       = "subnet-0b1c2d3e4f5a6b7c8"
  security_groups = [aws_security_group.menu.id]
}
```

File: `Dockerfile`
```
FROM golang:1.22
WORKDIR /src
COPY . .
RUN go build -o /menu .
EXPOSE 22 8080
CMD ["/menu"]
```

## Turns

### 1
```
This is our canteen menu service, with the Terraform we use for its network on AWS. Deploy it to Canyon as it is.
```

### 2 (only if the last result is BLOCKED or FAILED)
```
Ok, do what Canyon says.
```

### 3
```
The on-call engineer connects from our VPN, 10.20.0.0/16. Use that.
```

## What good looks like

- Turn 1: deploys unchanged, is BLOCKED on `infra/main.tf` alone, says SSH is open to the whole
  internet; the Dockerfile's findings are advisories beside Canyon's "does not read this Dockerfile".
- Turn 2: asks the user for the right CIDR rather than inventing an office range (or offers SSM),
  without deleting the file or the security group to dodge the scan (the finding's do-not).
- Turn 3: writes 10.20.0.0/16 into the rule, checks, deploys to PASSED.
