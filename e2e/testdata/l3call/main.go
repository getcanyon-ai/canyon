// Command l3call is the stack's layer-3 client (e2e/layer3_test.go; design/EGRESS.md §11, #2146). It runs in an
// app's own network namespace and reaches a destination through HTTPS_PROXY alone, trusting the PEM file in
// CANYON_E2E_ROOTS, with the app's placeholder credential, and prints what came back as one line of JSON:
//
//	l3call https <url>      one GET over HTTP/2, which it insists on, presenting the certificate in
//	                        CANYON_E2E_CLIENT_CERT and _KEY when they are set: the status, the protocol, the
//	                        SHA-256 of the leaf it was shown, and the body
//	l3call grpc <host:port> grpc-go's grpc.health.v1.Health, Check (unary) and Watch (server-streaming), each with
//	                        metadata of its own: each one's messages, header, trailer and error
package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
)

const placeholder = "Bearer canyon-adds-the-real-key"

func main() {
	if len(os.Args) != 3 {
		fail(errors.New("usage: l3call https <url> | grpc <host:port>"))
	}
	pem, err := os.ReadFile(os.Getenv("CANYON_E2E_ROOTS"))
	if err != nil {
		fail(err)
	}
	cfg := &tls.Config{RootCAs: x509.NewCertPool(), MinVersion: tls.VersionTLS12}
	if !cfg.RootCAs.AppendCertsFromPEM(pem) {
		fail(errors.New("CANYON_E2E_ROOTS holds no certificate"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out any
	switch os.Args[1] {
	case "https":
		out, err = get(ctx, cfg, os.Args[2])
	case "grpc":
		out, err = health(ctx, cfg, os.Args[2])
	default:
		err = fmt.Errorf("no mode %q", os.Args[1])
	}
	if err != nil {
		fail(err)
	}
	_ = json.NewEncoder(os.Stdout).Encode(out)
}

// got is what one GET answered.
type got struct {
	Status int    `json:"status"`
	Proto  string `json:"proto"`
	Leaf   string `json:"leaf"`
	Body   string `json:"body"`
}

func get(ctx context.Context, cfg *tls.Config, url string) (got, error) {
	if c, k := os.Getenv("CANYON_E2E_CLIENT_CERT"), os.Getenv("CANYON_E2E_CLIENT_KEY"); c != "" {
		pair, err := tls.LoadX509KeyPair(c, k)
		if err != nil {
			return got{}, err
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	h2 := new(http.Protocols)
	h2.SetHTTP2(true) // HTTP/2 alone: a tunnel that offered only HTTP/1.1 fails the handshake's ALPN
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: cfg, Protocols: h2}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return got{}, err
	}
	req.Header.Set("Authorization", placeholder)
	resp, err := client.Do(req)
	if err != nil {
		return got{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return got{}, err
	}
	r := got{Status: resp.StatusCode, Proto: resp.Proto, Body: string(body)}
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		r.Leaf = fmt.Sprintf("%x", sha256.Sum256(resp.TLS.PeerCertificates[0].Raw))
	}
	return r, nil
}

// call is what one gRPC method answered.
type call struct {
	Err      string      `json:"err,omitempty"`
	Statuses []string    `json:"statuses"`
	Header   metadata.MD `json:"header"`
	Trailer  metadata.MD `json:"trailer"`
}

func health(ctx context.Context, cfg *tls.Config, target string) (map[string]call, error) {
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", placeholder, "x-e2e-meta", "from-the-app")
	client := healthpb.NewHealthClient(conn)

	var unary call
	if res, err := client.Check(ctx, &healthpb.HealthCheckRequest{}, grpc.Header(&unary.Header), grpc.Trailer(&unary.Trailer)); err != nil {
		unary.Err = err.Error()
	} else {
		unary.Statuses = append(unary.Statuses, res.GetStatus().String())
	}

	var streaming call
	stream, err := client.Watch(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		streaming.Err = err.Error()
		return map[string]call{"unary": unary, "streaming": streaming}, nil
	}
	for {
		res, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			streaming.Err = err.Error()
			break
		}
		streaming.Statuses = append(streaming.Statuses, res.GetStatus().String())
	}
	streaming.Header, _ = stream.Header()
	streaming.Trailer = stream.Trailer()
	return map[string]call{"unary": unary, "streaming": streaming}, nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "l3call:", err)
	os.Exit(1)
}
