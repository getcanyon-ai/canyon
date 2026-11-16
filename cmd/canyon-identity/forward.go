package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// forwardOn is the forwarder's address, the pod's loopback alone: no other pod reaches it.
const forwardOn = "127.0.0.1:4184"

// forwarder is a pod's way out on Kubernetes from layer 3 (design/EGRESS.md §4.3): the proxy the app and the pod's
// sidecars are pointed at, which relays each call to Canyon's egress gateway — the one address it was started with,
// never one a request names — carrying Proxy-Authorization: Bearer and the pod's projected token, read again for
// every call as the kubelet rotates it. Whatever Proxy-Authorization a caller sent is replaced, and with no token
// to read nothing is relayed.
type forwarder struct {
	gateway string // Canyon's egress gateway, host:port
	token   string // the projected token's file, mounted into this container alone
	dial    func(ctx context.Context, network, address string) (net.Conn, error)
	log     *log.Logger
}

// newForwarder is the forwarder of proxy, Canyon's egress gateway as CANYON_IDENTITY_EGRESS_PROXY names it,
// http://<host>:<port> and nothing more, and token, the projected token's file.
func newForwarder(proxy, token string, logger *log.Logger) (*forwarder, error) {
	u, err := url.Parse(proxy)
	if err != nil || u.Scheme != "http" || u.Hostname() == "" || u.Port() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || token == "" {
		return nil, errors.New("CANYON_IDENTITY_EGRESS_PROXY is Canyon's egress gateway, http://<host>:<port>, and CANYON_IDENTITY_EGRESS_TOKEN the projected token's file: both or neither")
	}
	return &forwarder{gateway: u.Host, token: token, dial: (&net.Dialer{Timeout: 10 * time.Second}).DialContext, log: logger}, nil
}

// bearer is the header's value: the token's file, read now. A file the kubelet has not written, or an empty
// one, names no token.
func (f *forwarder) bearer() (string, error) {
	raw, err := os.ReadFile(f.token)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(raw))
	if token == "" || strings.ContainsAny(token, " \r\n") {
		return "", errors.New("the projected token's file holds no token")
	}
	return "Bearer " + token, nil
}

func (f *forwarder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect && !r.URL.IsAbs() { // a proxy takes CONNECT or an absolute-form request alone
		http.Error(w, "canyon-identity: this is the pod's proxy: send CONNECT or an absolute-form request", http.StatusBadRequest)
		return
	}
	auth, err := f.bearer()
	if err != nil {
		f.log.Printf("canyon-identity: forwarder: no token to send: %v", err)
		http.Error(w, "canyon-identity: this pod's token for Canyon's egress gateway can't be read, so the call was not sent", http.StatusBadGateway)
		return
	}
	up, err := f.dial(r.Context(), "tcp", f.gateway)
	if err != nil {
		http.Error(w, "canyon-identity: Canyon's egress gateway can't be reached", http.StatusBadGateway)
		return
	}
	defer func() { _ = up.Close() }()
	out := &http.Request{Method: r.Method, URL: r.URL, Host: r.Host, Header: r.Header.Clone(), Body: r.Body, ContentLength: r.ContentLength,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Close: true}
	out.Header.Del("Proxy-Connection")
	out.Header.Set("Proxy-Authorization", auth) // replaces whatever the caller sent
	if r.Method == http.MethodConnect {
		out.URL, out.Body, out.ContentLength = &url.URL{Host: r.Host}, nil, 0
		f.tunnel(w, up, out)
		return
	}
	if err := out.WriteProxy(up); err != nil {
		http.Error(w, "canyon-identity: Canyon's egress gateway can't be reached", http.StatusBadGateway)
		return
	}
	resp, err := http.ReadResponse(bufio.NewReader(up), out)
	if err != nil {
		http.Error(w, "canyon-identity: Canyon's egress gateway answered nothing", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	relay(w, resp)
}

// tunnel asks the gateway for out's tunnel and, once it answers 200, joins the caller to it; any other answer is
// relayed as the gateway gave it, its refusal's reason included.
func (f *forwarder) tunnel(w http.ResponseWriter, up net.Conn, out *http.Request) {
	if _, err := fmt.Fprintf(up, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: %s\r\n\r\n", out.Host, out.Host, out.Header.Get("Proxy-Authorization")); err != nil {
		http.Error(w, "canyon-identity: Canyon's egress gateway can't be reached", http.StatusBadGateway)
		return
	}
	br := bufio.NewReader(up)
	resp, err := http.ReadResponse(br, out)
	if err != nil {
		http.Error(w, "canyon-identity: Canyon's egress gateway answered nothing", http.StatusBadGateway)
		return
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		relay(w, resp)
		return
	}
	client, buffered, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer func() { _ = client.Close() }()
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, buffered); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, br); done <- struct{}{} }()
	<-done
}

func relay(w http.ResponseWriter, resp *http.Response) {
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
