package egress_test

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/syntax"

	"github.com/getcanyon-ai/canyon/internal/egresscontrol"
)

// The most workers envoy.sh lets Envoy run is the one the receiver's streams are sized from (contracts/routes.md, the
// receiver; #35): its refusal of CANYON_EGRESS_CONCURRENCY, `[ "$concurrency" -le N ]` as the shell parses it, names
// egresscontrol.ConcurrencyMost, so each of N workers' two streams has a place. The chart's copy of the script is
// this one, as deploy/k8s's is (TestTheEgressGatewayRunsDeployEgressAsItIs).
func TestEnvoyShsConcurrencyIsTheReceiversStreams(t *testing.T) {
	raw, err := os.ReadFile("envoy.sh")
	if err != nil {
		t.Fatal(err)
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(string(raw)), "envoy.sh")
	if err != nil {
		t.Fatal(err)
	}
	var most []int
	syntax.Walk(file, func(n syntax.Node) bool {
		c, ok := n.(*syntax.CallExpr)
		if !ok || len(c.Args) != 5 || c.Args[0].Lit() != "[" || c.Args[2].Lit() != "-le" || c.Args[4].Lit() != "]" || len(c.Args[1].Parts) != 1 {
			return true
		}
		quoted, ok := c.Args[1].Parts[0].(*syntax.DblQuoted)
		if !ok || len(quoted.Parts) != 1 {
			return true
		}
		if p, ok := quoted.Parts[0].(*syntax.ParamExp); ok && p.Param.Value == "concurrency" {
			if n, err := strconv.Atoi(c.Args[3].Lit()); err == nil {
				most = append(most, n)
			}
		}
		return true
	})
	if len(most) != 1 || most[0] != egresscontrol.ConcurrencyMost {
		t.Errorf(`envoy.sh refuses a concurrency over %v; want one [ "$concurrency" -le %d ], egresscontrol.ConcurrencyMost`, most, egresscontrol.ConcurrencyMost)
	}
	if egresscontrol.ReceiverStreams != egresscontrol.LogsPerWorker*egresscontrol.ConcurrencyMost {
		t.Errorf("the receiver takes %d streams; %d workers at most hold %d", egresscontrol.ReceiverStreams, egresscontrol.ConcurrencyMost,
			egresscontrol.LogsPerWorker*egresscontrol.ConcurrencyMost)
	}

	chart, err := os.ReadFile("../helm/canyon/templates/egress.yaml")
	if err != nil {
		t.Fatal(err)
	}
	indented := "    " + strings.ReplaceAll(strings.TrimSuffix(string(raw), "\n"), "\n", "\n    ")
	indented = strings.ReplaceAll(indented, "\n    \n", "\n\n") // a blank line carries no indent
	if !strings.Contains(string(chart), indented) {
		t.Error("the chart's envoy.sh (deploy/helm/canyon/templates/egress.yaml) is not this one: copy it over")
	}
}
