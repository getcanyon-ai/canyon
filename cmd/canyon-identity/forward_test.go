package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// gatewayStub stands in for Canyon's egress gateway: it records each request's line and Proxy-Authorization, opens a
// CONNECT to anything but refused.example as Envoy does and echoes what crosses it, refuses refused.example 403 with
// Envoy's Canyon-Egress header, and answers an absolute-form request 200 "through".
type gatewayStub struct {
	ln   net.Listener
	mu   sync.Mutex
	seen []string
}

func newGatewayStub(t *testing.T) *gatewayStub {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := &gatewayStub{ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go g.serve(c)
		}
	}()
	return g
}

func (g *gatewayStub) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	g.mu.Lock()
	g.seen = append(g.seen, req.Method+" "+req.RequestURI+" | "+req.Header.Get("Proxy-Authorization"))
	g.mu.Unlock()
	switch {
	case req.Method == http.MethodConnect && req.Host == "refused.example:443":
		_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nCanyon-Egress: not_allowed\r\nContent-Length: 7\r\n\r\nrefused")
	case req.Method == http.MethodConnect:
		_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\n\r\n")
		_, _ = io.Copy(c, br)
	default:
		_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 7\r\nConnection: close\r\n\r\nthrough")
	}
}

func (g *gatewayStub) requests() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.seen...)
}

// forwarding is a forwarder to g, its token's file holding token, every dial it makes recorded, served on loopback.
func forwarding(t *testing.T, g *gatewayStub, token string) (f *forwarder, proxy string, file string, dialed func() []string) {
	t.Helper()
	file = filepath.Join(t.TempDir(), "token")
	if token != "" {
		if err := os.WriteFile(file, []byte(token+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f, err := newForwarder("http://"+g.ln.Addr().String(), file, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var addrs []string
	real := f.dial
	f.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		addrs = append(addrs, address)
		mu.Unlock()
		return real(ctx, network, address)
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv.URL, file, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), addrs...) }
}

// connectThrough opens a CONNECT to target through proxy, sending auth as Proxy-Authorization when it is not
// empty, and answers the status line and, once open, what the tunnel echoes of hello.
func connectThrough(t *testing.T, proxy, target, auth, hello string) (status, echoed string) {
	t.Helper()
	u, _ := url.Parse(proxy)
	c, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	head := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if auth != "" {
		head += "Proxy-Authorization: " + auth + "\r\n"
	}
	if _, err := io.WriteString(c, head+"\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Sprintf("%d %s %s", resp.StatusCode, resp.Header.Get("Canyon-Egress"), body), ""
	}
	if _, err := io.WriteString(c, hello); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(hello))
	if _, err := io.ReadFull(br, buf); err != nil {
		t.Fatal(err)
	}
	return resp.Status, string(buf)
}

// design/EGRESS.md §4.3: the forwarder sends the pod's projected token, read for each call, to the gateway it was
// started with and nowhere else, whatever a request names, and replaces any Proxy-Authorization the app sent.
func TestTheForwarderSendsThePodsTokenToTheGatewayAlone(t *testing.T) {
	g := newGatewayStub(t)
	_, proxy, file, dialed := forwarding(t, g, "first.pod.token")

	if status, echoed := connectThrough(t, proxy, "api.stripe.com:443", "Bearer forged-by-the-app", "hello"); !strings.HasPrefix(status, "200") || echoed != "hello" {
		t.Errorf("a CONNECT: %q, echoed %q; want the tunnel open both ways", status, echoed)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: func(*http.Request) (*url.URL, error) { return url.Parse(proxy) }}}
	req, _ := http.NewRequest(http.MethodGet, "http://elsewhere.example:8443/collect?x=1", nil)
	req.Header.Set("Proxy-Authorization", "Bearer forged-by-the-app")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "through" {
		t.Errorf("an absolute-form request: %d %q", resp.StatusCode, body)
	}
	if err := os.WriteFile(file, []byte("rotated.pod.token"), 0o600); err != nil { // the kubelet rotates it
		t.Fatal(err)
	}
	if status, _ := connectThrough(t, proxy, "refused.example:443", "", "x"); status != "403 not_allowed refused" {
		t.Errorf("the gateway's refusal reaches the app as %q, want 403 not_allowed refused", status)
	}

	want := []string{
		"CONNECT api.stripe.com:443 | Bearer first.pod.token",
		"GET http://elsewhere.example:8443/collect?x=1 | Bearer first.pod.token",
		"CONNECT refused.example:443 | Bearer rotated.pod.token",
	}
	if got := g.requests(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the gateway was asked:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, a := range dialed() {
		if a != g.ln.Addr().String() {
			t.Errorf("the forwarder dialled %s: only the gateway %s is ever dialled", a, g.ln.Addr())
		}
	}
}

// With no token to read nothing is relayed, and a request no proxy takes is refused before anything is dialled.
func TestTheForwarderRelaysNothingWithoutAToken(t *testing.T) {
	g := newGatewayStub(t)
	_, proxy, _, dialed := forwarding(t, g, "")
	if status, _ := connectThrough(t, proxy, "api.stripe.com:443", "Bearer forged-by-the-app", "x"); !strings.HasPrefix(status, "502") {
		t.Errorf("a CONNECT with no token's file: %q, want 502", status)
	}
	resp, err := http.Get(proxy + "/") // origin-form: the forwarder itself, which serves nothing
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an origin-form request: %d, want 400", resp.StatusCode)
	}
	if len(dialed()) != 0 || len(g.requests()) != 0 {
		t.Errorf("dialled %v, the gateway asked %v: want nothing", dialed(), g.requests())
	}
}

// The forwarder takes Canyon's gateway as http://<host>:<port> alone, and its token's file with it, and listens on
// the pod's loopback alone.
func TestTheForwarderTakesTheGatewayAndTokenTogether(t *testing.T) {
	for _, c := range []struct{ proxy, token string }{
		{"http://canyon-egress.canyon.svc:3128", ""},
		{"", "/var/run/canyon-egress/token"},
		{"https://canyon-egress.canyon.svc:3128", "/t"},
		{"http://canyon-egress.canyon.svc", "/t"},
		{"http://user:pass@canyon-egress.canyon.svc:3128", "/t"},
		{"http://canyon-egress.canyon.svc:3128/elsewhere", "/t"},
		{"http://canyon-egress.canyon.svc:3128/?to=elsewhere", "/t"},
	} {
		if _, err := newForwarder(c.proxy, c.token, log.New(io.Discard, "", 0)); err == nil {
			t.Errorf("newForwarder(%q, %q) was taken", c.proxy, c.token)
		}
	}
	if f, err := newForwarder("http://canyon-egress.canyon.svc:3128", "/var/run/canyon-egress/token", log.New(io.Discard, "", 0)); err != nil || f.gateway != "canyon-egress.canyon.svc:3128" {
		t.Errorf("Canyon's gateway: %+v %v", f, err)
	}
	if host, _, _ := net.SplitHostPort(forwardOn); host != "127.0.0.1" {
		t.Errorf("the forwarder listens on %s, want the pod's loopback alone", forwardOn)
	}
}
