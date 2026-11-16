//go:build stack

package e2e

import (
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// Layer 3 end to end (design/EGRESS.md §11, #2146): a credential reaches e2e/echo over HTTP/2 and over grpc-go's
// unary and server-streaming calls, never the app's placeholder; and a destination with no route is never decrypted,
// though its env's interception is live. The client is e2e/testdata/l3call, in the routed app's own network
// namespace, reaching the echo through HTTPS_PROXY alone; the echo answers the SNI, ALPN, credential and client
// certificate that reached it.

const placeholder = "Bearer canyon-adds-the-real-key"

// TestForcedHTTP2CarriesTheRouteCredential: a client that insists on HTTP/2 has its request intercepted, the
// placeholder overwritten with the route's value, and the echo reached over HTTP/2 by the TLS Envoy originates.
func TestForcedHTTP2CarriesTheRouteCredential(t *testing.T) {
	l := layer3(t)
	r := l.get(t, l.root, nil, "https://"+echoName+"/v1/charges")
	if want := "method=GET path=/v1/charges sni=" + echoName + " alpn=h2 client=- auth=Bearer " + routeValue(); r.Status != http.StatusOK || r.Proto != "HTTP/2.0" || r.Body != want {
		t.Errorf("an HTTP/2 call to %s, routed: %+v; want 200 over HTTP/2 and the echo to read %q", echoName, r, want)
	}
}

// TestGRPCCarriesTheRouteCredential: grpc-go's unary and server-streaming calls through the proxy, each with metadata
// of the app's own, reach the echo with the route's value in place of the placeholder, and every message, the
// metadata and the trailers come back.
func TestGRPCCarriesTheRouteCredential(t *testing.T) {
	l := layer3(t)
	exit, out := l.run(t, l.root, nil, "grpc", echoName+":443")
	if exit != 0 {
		t.Fatalf("grpc-go to %s: exit %d\n%s", echoName, exit, out)
	}
	var calls map[string]struct {
		Err             string
		Statuses        []string
		Header, Trailer map[string][]string
	}
	if err := json.Unmarshal([]byte(out), &calls); err != nil {
		t.Fatalf("grpc-go's answer: %v\n%s", err, out)
	}
	for method, want := range map[string][]string{"unary": {"SERVING"}, "streaming": {"SERVING", "SERVING"}} {
		c := calls[method]
		if c.Err != "" || !slices.Equal(c.Statuses, want) || !slices.Equal(c.Header["echo-authorization"], []string{"Bearer " + routeValue()}) ||
			!slices.Equal(c.Header["echo-x-e2e-meta"], []string{"from-the-app"}) || !slices.Equal(c.Trailer["echo-trailer"], []string{"done"}) {
			t.Errorf("grpc-go's %s call to %s, routed: %+v; want %v, the route's value, the app's metadata and the echo's trailer", method, echoName, c, want)
		}
	}
}

// TestPassThroughDestinationIsNeverDecrypted: with the env's interception live, a call to an allowed destination with
// no route is the app's own TLS end to end. The app is shown the echo's own certificate, though it trusts Canyon's
// root too, and the echo is shown the app's own client certificate and placeholder: Envoy, had it ended the TLS, holds
// neither the echo's key nor the app's. The routed call beside it shows what interception looks like from both ends.
func TestPassThroughDestinationIsNeverDecrypted(t *testing.T) {
	l := layer3(t)
	base := "/api/v1/projects/" + routed.project + "/egress/preview/destinations/"
	if code, body := api(t, http.MethodPut, base+echoUntrusted, nil); code/100 != 2 {
		t.Fatalf("allowing %s: %d %s", echoUntrusted, code, body)
	}
	t.Cleanup(func() { api(t, http.MethodDelete, base+echoUntrusted, nil) })

	fixtures, err := filepath.Abs(filepath.Join(".work", "fixtures"))
	if err != nil {
		t.Fatal(err)
	}
	read := func(name string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(fixtures, name)) //nolint:gosec // the stack's own fixtures
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	block, _ := pem.Decode(read("echo-untrusted.crt"))
	if block == nil {
		t.Fatal("echo-untrusted.crt holds no certificate")
	}
	echoLeaf := fmt.Sprintf("%x", sha256.Sum256(block.Bytes))
	appCert, _, certPEM, keyPEM := certFor(t, "the-app", false, nil, nil)
	appLeaf := fmt.Sprintf("%x", sha256.Sum256(appCert.Raw))
	canyonRoot, err := os.ReadFile(filepath.Join(l.root, "root.pem")) //nolint:gosec // the test's own temp dir
	if err != nil {
		t.Fatal(err)
	}
	both := filesIn(t, map[string][]byte{"root.pem": append(append(canyonRoot, '\n'), read("echo-other-ca.pem")...), "app.crt": certPEM, "app.key": keyPEM})
	client := []string{"-e", "CANYON_E2E_CLIENT_CERT=/certs/app.crt", "-e", "CANYON_E2E_CLIENT_KEY=/certs/app.key"}

	want := "method=GET path=/v1/charges sni=" + echoUntrusted + " alpn=h2 client=" + appLeaf + " auth=" + placeholder
	r := l.get(t, both, client, "https://"+echoUntrusted+"/v1/charges") // the PUT answered once every gateway held it (#2799)
	if r.Leaf != echoLeaf {
		t.Errorf("a call to %s, allowed with no route: shown the certificate %s, not the echo's own %s: Envoy ended the app's TLS (%+v)", echoUntrusted, r.Leaf, echoLeaf, r)
	}
	if r.Status != http.StatusOK || r.Body != want {
		t.Errorf("a call to %s, allowed with no route: %+v; want the echo to read %q, the app's own TLS and header", echoUntrusted, r, want)
	}

	routedCall := l.get(t, both, client, "https://"+echoName+"/v1/charges")
	if want := "method=GET path=/v1/charges sni=" + echoName + " alpn=h2 client=- auth=Bearer " + routeValue(); routedCall.Body != want || routedCall.Leaf == "" || routedCall.Leaf == echoLeaf {
		t.Errorf("the routed call beside it, to %s: %+v; want a leaf of Canyon's, and the echo to read %q", echoName, routedCall, want)
	}
}

// interceptedApp is the routed app with interception live for its preview: credential routes on, echoName allowed
// and routed (trustedRoutes), and the l3call client built.
type interceptedApp struct{ app, bin, root string }

// layer3 sets it up until t ends, and waits until Envoy holds echoName's chain: until then a call to it passes
// through untouched and fails, as the app trusts Canyon's root alone.
func layer3(t *testing.T) interceptedApp {
	t.Helper()
	l := interceptedApp{app: routedApp(t), bin: t.TempDir()}
	controlUp(t)
	setVia(t, "canyon")
	t.Cleanup(func() { setVia(t, "off") })
	l.root = trustedRoutes(t, routed.project, echoName)
	if err := os.Chmod(l.bin, 0o755); err != nil { //nolint:gosec // run by a container's own user
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", filepath.Join(l.bin, "l3call"), "./testdata/l3call") //nolint:gosec // the test's own temp dir
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")                                 // the host's architecture, which its Docker runs
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building l3call: %v\n%s", err, out)
	}
	for deadline := time.Now().Add(90 * time.Second); ; time.Sleep(2 * time.Second) {
		exit, out := l.run(t, l.root, nil, "https", "https://"+echoName+"/")
		if exit == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Envoy holds no chain for %s 90 s after its route: exit %d\n%s", echoName, exit, out)
		}
	}
	return l
}

// run is l3call with args in the app's own network namespace, through Envoy's :3128, trusting roots/root.pem, roots
// mounted at /certs.
func (l interceptedApp) run(t *testing.T, roots string, env []string, args ...string) (int, string) {
	t.Helper()
	run := []string{"run", "--rm", "--network", "container:" + l.app, "-v", l.bin + ":/l3:ro", "-v", roots + ":/certs:ro",
		"-e", "HTTPS_PROXY=http://canyon-egress:3128", "-e", "CANYON_E2E_ROOTS=/certs/root.pem"}
	run = append(append(run, env...), "--entrypoint", "/l3/l3call", curl)
	return exitOf(append(run, args...)...)
}

// got is l3call https's answer.
type got struct {
	Status      int
	Proto, Leaf string
	Body        string
}

func (l interceptedApp) get(t *testing.T, roots string, env []string, url string) got {
	t.Helper()
	exit, out := l.run(t, roots, env, "https", url)
	var r got
	if exit != 0 || json.Unmarshal([]byte(out), &r) != nil {
		t.Fatalf("l3call https %s: exit %d\n%s", url, exit, out)
	}
	return r
}
