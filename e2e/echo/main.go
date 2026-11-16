// Command echo is the E2E stack's outside service for injected calls (design/EGRESS.md §11; #2144, #2146): HTTPS on
// :443 under a certificate from the test CA e2e/fixtures/gen.sh makes, which only the gateway's bundle trusts here
// (e2e/compose.e2e.yml). It answers each request with what reached it, one line: "method=<m> path=<p> sni=<name>
// alpn=<proto> auth=<its Authorization>", and a gRPC Health/Check with SERVING and that Authorization as the header
// metadata echo-authorization. A second name, echo-untrusted, answers under a certificate from a CA no bundle holds.
package main

import (
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// names is each name's certificate and key, under /fixtures (gen.sh's output).
var names = map[string][2]string{
	"echo.canyon-floor.test":           {"/fixtures/echo.crt", "/fixtures/echo.key"},
	"echo-untrusted.canyon-floor.test": {"/fixtures/echo-untrusted.crt", "/fixtures/echo-untrusted.key"},
}

func main() {
	certs := map[string]*tls.Certificate{}
	for name, files := range names {
		c, err := tls.LoadX509KeyPair(files[0], files[1])
		if err != nil {
			log.Fatal(err)
		}
		certs[name] = &c
	}
	srv := &http.Server{
		Addr:              ":443",
		Handler:           http.HandlerFunc(echo),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         tlsFor(certs),
	}
	log.Fatal(srv.ListenAndServeTLS("", ""))
}

// tlsFor serves each name its own certificate, and a handshake naming none the trusted one.
func tlsFor(certs map[string]*tls.Certificate) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, ClientAuth: tls.RequestClientCert, GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if c, ok := certs[hello.ServerName]; ok {
			return c, nil
		}
		return certs["echo.canyon-floor.test"], nil
	}}
}

// echo answers what reached it: a gRPC Health/Check as gRPC, anything else as one line.
func echo(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		healthCheck(w, r, auth)
		return
	}
	var sni, alpn string
	client := "-" // the SHA-256 of the client's own certificate, which only a TLS the client itself ends here carries
	if r.TLS != nil {
		sni, alpn = r.TLS.ServerName, r.TLS.NegotiatedProtocol
		if len(r.TLS.PeerCertificates) > 0 {
			client = fmt.Sprintf("%x", sha256.Sum256(r.TLS.PeerCertificates[0].Raw))
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = fmt.Fprintf(w, "method=%s path=%s sni=%s alpn=%s client=%s auth=%s", r.Method, r.URL.Path, sni, alpn, client, auth) //nolint:gosec // plain text to a test's client, which reads what reached the echo
}

// healthCheck answers grpc.health.v1.Health/Check SERVING, the Authorization it was sent as echo-authorization: one
// length-prefixed message, HealthCheckResponse{status: SERVING} (field 1, varint 1), then grpc-status 0. Any other
// method is UNIMPLEMENTED.
func healthCheck(w http.ResponseWriter, r *http.Request, auth string) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<16))
	w.Header().Set("Content-Type", "application/grpc")
	w.Header().Set("Echo-Authorization", auth)
	w.Header().Set("Echo-X-E2e-Meta", r.Header.Get("X-E2e-Meta"))
	w.Header().Set("Trailer", "Grpc-Status, Echo-Trailer")
	frames := map[string]int{"/grpc.health.v1.Health/Check": 1, "/grpc.health.v1.Health/Watch": 2}[r.URL.Path] // Watch streams
	if frames == 0 {
		w.WriteHeader(http.StatusOK)
		w.Header().Set("Grpc-Status", "12")
		return
	}
	serving := []byte{0, 0, 0, 0, 2, 0x08, 0x01} // one HealthCheckResponse{status: SERVING}, framed
	for range frames {
		_, _ = w.Write(serving)
		_ = http.NewResponseController(w).Flush() // a client that left reads no more frames
	}
	w.Header().Set("Echo-Trailer", "done")
	w.Header().Set("Grpc-Status", "0")
}
