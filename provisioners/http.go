package provisioners

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"
)

const (
	maxBody    = 1 << 20  // an adapter's answer
	maxMessage = 1024     // characters of a pending or failed message
	maxOutput  = 64 << 10 // one output's value, the secrets routes' limit
)

// HTTP hands Ensure and Delete to a platform team's adapter (contracts/provisioner.md); the adapter holds the cloud credentials, Canyon holds a bearer token.
type HTTP struct {
	base   *url.URL
	token  string
	client *http.Client
}

// NewHTTP is a driver for the adapter at base, authenticated by the token in tokenFile. caFile, when set, is the only CA its certificate is verified against; unset, the system roots are.
func NewHTTP(base, tokenFile, caFile string) (*HTTP, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil || u.Scheme != "https" && (u.Scheme != "http" || !loopback(u.Hostname())) {
		return nil, fmt.Errorf("adapter url %q: must be https (http only on loopback), with no credentials in it", base)
	}
	raw, err := os.ReadFile(tokenFile) //nolint:gosec // the platform's own config names the file
	if err != nil {
		return nil, fmt.Errorf("adapter token: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" || strings.ContainsFunc(token, func(r rune) bool { return r < '!' || r > '~' }) {
		return nil, fmt.Errorf("adapter token file %s must hold one token of printable characters", tokenFile)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile) //nolint:gosec // the platform's own config names the file
		if err != nil {
			return nil, fmt.Errorf("adapter ca: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("adapter ca file %s holds no PEM certificate", caFile)
		}
		transport.TLSClientConfig.RootCAs = roots
	}
	return &HTTP{base: u, token: token, client: &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second, // answers are at most 1 MiB, so one deadline covers the call
		// Following a redirect would carry the token wherever it points: the 3xx is the answer.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func loopback(host string) bool { return host == "localhost" || host == "127.0.0.1" || host == "::1" }

// answer is an adapter's JSON body; each field is read only where the contract has it.
type answer struct {
	Outputs map[string]string `json:"outputs"`
	Message string            `json:"message"`
}

// Ensure asks the adapter to make the key's resource exist: PUT {base}/{project}/{env}/{name}.
func (h *HTTP) Ensure(ctx context.Context, key Key, typ string) (Result, error) {
	promised, ok := outputs[typ]
	if !ok {
		return failed("unknown resource type %q", typ), nil
	}
	body, _ := json.Marshal(map[string]string{"type": typ}) // a map of strings always marshals
	a, r, err := h.call(ctx, http.MethodPut, key, body, http.StatusOK)
	if err != nil || r.State != "" {
		return r, err
	}
	out := map[string]string{}
	for _, name := range promised { // only the promised names: any other key is dropped here
		v := a.Outputs[name]
		if v == "" || len(v) > maxOutput {
			return failed("adapter answered ready with %s empty, missing or over 64 KiB", name), nil
		}
		out[name] = v
	}
	return Result{State: Ready, Outputs: out, Message: plain(a.Message)}, nil
}

// Delete asks the adapter to destroy the key's resource: DELETE {base}/{project}/{env}/{name}.
func (h *HTTP) Delete(ctx context.Context, key Key) (Result, error) {
	_, r, err := h.call(ctx, http.MethodDelete, key, nil, http.StatusNoContent)
	if err != nil || r.State != "" {
		return r, err
	}
	return Result{State: Gone}, nil
}

// call sends one request. It answers the body and no Result when the status is want; otherwise
// the Result the answer means: 202 pending, 5xx, 408, 429 or a transport error transient (the
// error), and anything else failed (3xx, 4xx, another 2xx, over 1 MiB, a 202 or want not JSON).
func (h *HTTP) call(ctx context.Context, method string, key Key, body []byte, want int) (answer, Result, error) {
	var a answer
	if err := key.check(); err != nil {
		return a, failed("%v", err), nil
	}
	req, err := http.NewRequestWithContext(ctx, method, h.base.JoinPath(key.Project, key.Env, key.Name).String(), bytes.NewReader(body))
	if err != nil {
		return a, Result{}, err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return a, Result{}, fmt.Errorf("adapter %s %s: %w", method, key, err)
	}
	defer func() { _ = resp.Body.Close() }()
	status := resp.StatusCode
	if status >= 500 || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests {
		return a, Result{}, fmt.Errorf("adapter %s %s: answered %d", method, key, status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	switch {
	case err != nil:
		return a, Result{}, fmt.Errorf("adapter %s %s: %w", method, key, err)
	case len(raw) > maxBody:
		return a, failed("adapter's answer is over 1 MiB"), nil
	case len(raw) > 0 && json.Unmarshal(raw, &a) != nil && (status == want || status == http.StatusAccepted):
		return a, failed("adapter's %d answer is not JSON", status), nil
	case status == want:
		return a, Result{}, nil
	case status == http.StatusAccepted:
		return a, Result{State: Pending, Message: plain(a.Message)}, nil
	case a.Message != "":
		return a, Result{State: Failed, Message: plain(a.Message)}, nil
	}
	return a, failed("adapter answered %d", status), nil
}

func failed(format string, args ...any) Result {
	return Result{State: Failed, Message: fmt.Sprintf(format, args...)}
}

// plain is a message fit to show a person: no control characters, at most 1024 characters.
func plain(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	if r := []rune(s); len(r) > maxMessage {
		return string(r[:maxMessage])
	}
	return s
}

// Name is the adapter's URL: the same adapter under a new key keeps it, another adapter never has it.
func (h *HTTP) Name() string { return h.base.String() }
