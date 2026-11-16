package executor

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
)

// serverContainer is the container GET /published reads: the name docker-compose.yml gives the server, and
// the first CANYON_EXECUTOR_JOIN joins by default.
const serverContainer = "canyon-server"

// Binding is one port binding GET /published answers (contracts/executor.md, GET /published): the
// container's port and protocol, Docker's key, and the host address and port it is published on, as Docker
// spells them: an empty HostIP is every address, an empty HostPort one Docker picks.
type Binding struct {
	Port     string `json:"port"`
	HostIP   string `json:"host_ip"`
	HostPort string `json:"host_port"`
}

// Published is GET /published's answer: the running canyon-server's Id, the bindings it was created with
// (HostConfig.PortBindings) and those it holds now (NetworkSettings.Ports).
type Published struct {
	ID      string    `json:"id"`
	Created []Binding `json:"created"`
	Held    []Binding `json:"held"`
}

// Published answers canyon-server's Id and port bindings, each list's once, sorted. The facts alone: the
// server judges them. A daemon that does not answer, no such container, one that is not running, or an
// inspect it cannot read is an error: a stopped container holds nothing and vouches for nothing.
func (e *Executor) Published(ctx context.Context) (Published, error) {
	var stderr strings.Builder
	cmd := child(ctx, e.docker(), "container", "inspect", "--format",
		`{"id":{{json .Id}},"running":{{json .State.Running}},"created":{{json .HostConfig.PortBindings}},"held":{{json .NetworkSettings.Ports}}}`, serverContainer)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return Published{}, fmt.Errorf("docker container inspect %s: %w: %s", serverContainer, err, strings.TrimSpace(stderr.String()))
	}
	var inspect struct {
		ID            string
		Running       bool
		Created, Held map[string][]struct{ HostIP, HostPort string }
	}
	if err := json.Unmarshal(out, &inspect); err != nil {
		return Published{}, fmt.Errorf("docker container inspect %s: its port bindings are not read: %w", serverContainer, err)
	}
	if !inspect.Running {
		return Published{}, fmt.Errorf("%s is not running: a stopped container holds no binding", serverContainer)
	}
	return Published{ID: inspect.ID, Created: bindingsOf(inspect.Created), Held: bindingsOf(inspect.Held)}, nil
}

// bindingsOf is docker's map of port to bindings as a list, each once, sorted.
func bindingsOf(ports map[string][]struct{ HostIP, HostPort string }) []Binding {
	bindings := []Binding{}
	for port, held := range ports {
		for _, b := range held {
			bindings = append(bindings, Binding{Port: port, HostIP: b.HostIP, HostPort: b.HostPort})
		}
	}
	slices.SortFunc(bindings, func(a, b Binding) int {
		return cmp.Or(cmp.Compare(a.Port, b.Port), cmp.Compare(a.HostIP, b.HostIP), cmp.Compare(a.HostPort, b.HostPort))
	})
	return slices.Compact(bindings)
}

// publishedHandler serves GET /published with published, the Docker executor's (contracts/executor.md, GET
// /published): 502 when it cannot tell.
func publishedHandler(published func(ctx context.Context) (Published, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		answer, err := published(r.Context())
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = encode(w, answer)
	}
}

// bindingPort and containerID are executor.schema.json's published_result patterns, verbatim.
var (
	bindingPort = regexp.MustCompile(`^[0-9]{1,5}/(tcp|udp|sctp)$`)
	containerID = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Published asks where Docker publishes the server (contracts/executor.md, GET /published). Anything but 200
// is an error, the 404 of the Kubernetes executor or one from before the call included, and so is an answer
// out of the contract's shape: a caller reads no binding from either.
func (c *Client) Published(ctx context.Context) (Published, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/published", nil)
	if err != nil {
		return Published{}, err
	}
	resp, err := c.http().Do(hreq)
	if err != nil {
		return Published{}, fmt.Errorf("executor: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Published{}, fmt.Errorf("executor: GET /published answered %d%s", resp.StatusCode, message(resp.Body))
	}
	var res struct {
		ID      *string    `json:"id"`
		Created *[]Binding `json:"created"`
		Held    *[]Binding `json:"held"`
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	dec.DisallowUnknownFields()
	err = dec.Decode(&res)
	if err == nil {
		if after := dec.Decode(&struct{}{}); after != io.EOF { //nolint:errorlint // the decoder's own sentinel
			err = fmt.Errorf("something after the object: %v", after)
		}
	}
	if err == nil && (res.ID == nil || res.Created == nil || res.Held == nil) {
		err = errors.New("id, created and held, each present and not null")
	}
	if err == nil && !containerID.MatchString(*res.ID) {
		err = fmt.Errorf("id %q is not a container's Id", *res.ID)
	}
	if err == nil {
		for _, b := range slices.Concat(*res.Created, *res.Held) {
			if !bindingPort.MatchString(b.Port) {
				err = fmt.Errorf("a binding's port %q is not a port and protocol", b.Port)
				break
			}
		}
	}
	if err != nil {
		return Published{}, fmt.Errorf("executor: GET /published answered out of the contract: %w", err)
	}
	return Published{ID: *res.ID, Created: *res.Created, Held: *res.Held}, nil
}
