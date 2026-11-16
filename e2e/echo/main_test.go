package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
)

// The echo answers grpc-go's own Health/Check client SERVING, with the Authorization it was sent as header metadata, over
// h2; and an HTTPS request with one line of what reached it (#2144, #2146).
func TestTheEchoAnswersWhatReachedIt(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(echo))
	srv.EnableHTTP2 = true
	srv.TLS = &tls.Config{ClientAuth: tlsFor(nil).ClientAuth, MinVersion: tls.VersionTLS12} // the echo's own ask for a client certificate
	srv.StartTLS()
	defer srv.Close()
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())

	conn, err := grpc.NewClient(strings.TrimPrefix(srv.URL, "https://"), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: roots, ServerName: "example.com", MinVersion: tls.VersionTLS12})))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer x", "x-e2e-meta", "m"), 5*time.Second)
	defer cancel()
	var header, trailer metadata.MD
	res, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{}, grpc.Header(&header), grpc.Trailer(&trailer))
	if err != nil || res.GetStatus() != healthpb.HealthCheckResponse_SERVING || strings.Join(header.Get("echo-authorization"), ",") != "Bearer x" ||
		strings.Join(header.Get("echo-x-e2e-meta"), ",") != "m" || strings.Join(trailer.Get("echo-trailer"), ",") != "done" {
		t.Fatalf("Health/Check: %v, %v, header %v, trailer %v", res, err, header, trailer)
	}
	stream, err := healthpb.NewHealthClient(conn).Watch(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	served := 0
	for {
		res, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil || res.GetStatus() != healthpb.HealthCheckResponse_SERVING {
			t.Fatalf("Health/Watch's message %d: %v, %v", served+1, res, err)
		}
		served++
	}
	if header, _ := stream.Header(); served != 2 || strings.Join(header.Get("echo-authorization"), ",") != "Bearer x" || strings.Join(stream.Trailer().Get("echo-trailer"), ",") != "done" {
		t.Errorf("Health/Watch: %d messages, header %v, trailer %v; want 2, the authorization and done", served, header, stream.Trailer())
	}
	if _, err := healthpb.NewHealthClient(conn).List(ctx, &healthpb.HealthListRequest{}); err == nil || !strings.Contains(err.Error(), "Unimplemented") {
		t.Errorf("another method: %v, want Unimplemented", err)
	}

	client := srv.Client()
	client.Transport.(*http.Transport).TLSClientConfig.ServerName = "example.com"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/echo?x=1", nil)
	req.Header.Set("Authorization", "Bearer y")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if want := "method=GET path=/v1/echo sni=example.com alpn=h2 client=- auth=Bearer y"; string(body) != want {
		t.Errorf("an HTTPS request: %q, want %q", body, want)
	}

	cert := srv.TLS.Certificates[0] // any certificate will do: the echo asks for one and verifies none
	// a transport of its own, so a TLS connection of its own: the first request's HTTP/2 connection, made with no
	// certificate, may still be open, and a request on it would show client=- (1 run in 7 on the laptop)
	withCert := client.Transport.(*http.Transport).Clone()
	withCert.TLSClientConfig.Certificates = []tls.Certificate{cert}
	resp, err = (&http.Client{Transport: withCert}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ = io.ReadAll(resp.Body)
	if want := fmt.Sprintf(" client=%x ", sha256.Sum256(cert.Certificate[0])); !strings.Contains(string(body), want) {
		t.Errorf("a request with a client certificate: %q, want %q", body, want)
	}
}

// Each name gets its own certificate, and a handshake naming none the trusted one.
func TestEachNameGetsItsOwnCertificate(t *testing.T) {
	trusted, untrusted := &tls.Certificate{}, &tls.Certificate{}
	cfg := tlsFor(map[string]*tls.Certificate{"echo.canyon-floor.test": trusted, "echo-untrusted.canyon-floor.test": untrusted})
	for name, want := range map[string]*tls.Certificate{"echo.canyon-floor.test": trusted, "echo-untrusted.canyon-floor.test": untrusted, "": trusted} {
		if got, _ := cfg.GetCertificate(&tls.ClientHelloInfo{ServerName: name}); got != want {
			t.Errorf("%q got another name's certificate", name)
		}
	}
}
