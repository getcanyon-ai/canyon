package executortest

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/executor"
)

// The fake answers /scan as the executor does, read by the real client: busy with its Retry-After,
// then the SARIF it was given, or not_finished instead; and it keeps the request it was asked.
func TestTheFakeScansAsTheExecutorDoes(t *testing.T) {
	f := New(t)
	f.SARIF = `{"version":"2.1.0","runs":[]}`
	f.Busy.Store(1)
	c, req := &executor.Client{Base: f.URL}, executor.ScanRequest{App: "claims-portal", Package: "pkg_7d3e8f", Tool: "trivy", Settings: &executor.ScanSettings{SkipPaths: []string{"docs"}}}
	scan := func() (executor.ScanResult, error) { return c.Scan(t.Context(), req, strings.NewReader("a tar"), nil) }
	var busy *executor.Busy
	if _, err := scan(); !errors.As(err, &busy) || busy.RetryAfter != time.Second {
		t.Fatalf("busy first: %v", err)
	}
	if res, err := scan(); err != nil || string(res.SARIF) != f.SARIF || res.NotFinished != "" {
		t.Fatalf("then the SARIF: %+v %v", res, err)
	}
	if asked, _ := f.Scanned(); !reflect.DeepEqual(asked, req) {
		t.Fatalf("asked %+v", asked)
	}
	f.NotFinished = "timeout"
	if res, err := scan(); err != nil || res.NotFinished != "timeout" || res.SARIF != nil || f.Scans.Load() != 3 {
		t.Fatalf("not finished: %+v %v after %d scans", res, err, f.Scans.Load())
	}
}
