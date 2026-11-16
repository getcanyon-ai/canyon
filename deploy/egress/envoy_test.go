// Package egress_test holds Canyon's Envoy bootstrap, envoy.yaml, to design/EGRESS.md §2.1: the facts that
// hold before any Envoy runs. CI's envoy --mode validate checks the same file, filled in by envoy.sh, in
// the pinned Envoy itself.
package egress_test

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/getcanyon-ai/canyon/internal/egresscontrol"
)

const als = "type.googleapis.com/envoy.extensions.access_loggers.grpc.v3.HttpGrpcAccessLogConfig"

// bootstrap is envoy.yaml, decoded: maps, lists and scalars.
func bootstrap(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("envoy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var b map[string]any
	if err := yaml.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	return b
}

// at is the value at path in v, each step a map's key or a list's index; nil where there is none.
func at(v any, path ...any) any {
	for _, step := range path {
		switch s := step.(type) {
		case string:
			m, _ := v.(map[string]any)
			v = m[s]
		case int:
			l, _ := v.([]any)
			if s >= len(l) {
				return nil
			}
			v = l[s]
		}
	}
	return v
}

// walk calls see on every map in v, depth first.
func walk(v any, see func(map[string]any)) {
	switch n := v.(type) {
	case map[string]any:
		see(n)
		for _, c := range n {
			walk(c, see)
		}
	case []any:
		for _, c := range n {
			walk(c, see)
		}
	}
}

// hcm is the connection manager of the one listener, on :3128.
func hcm(t *testing.T, b map[string]any) map[string]any {
	t.Helper()
	listeners, _ := at(b, "static_resources", "listeners").([]any)
	if len(listeners) != 1 || at(listeners[0], "address", "socket_address", "port_value") != 3128 {
		t.Fatalf("the bootstrap's listeners are %v; want one, on 3128", listeners)
	}
	m, _ := at(listeners[0], "filter_chains", 0, "filters", 0, "typed_config").(map[string]any)
	if at(listeners[0], "filter_chains", 0, "filters", 0, "name") != "envoy.filters.network.http_connection_manager" || m == nil {
		t.Fatalf("the listener's first filter is %v, not the connection manager", at(listeners[0], "filter_chains", 0, "filters", 0, "name"))
	}
	return m
}

func TestBootstrapHasNoTCPAdminAndNoFileLog(t *testing.T) {
	b := bootstrap(t)
	admin, _ := b["admin"].(map[string]any)
	address, _ := admin["address"].(map[string]any)
	pipe, _ := address["pipe"].(map[string]any)
	if len(admin) != 1 || len(address) != 1 || pipe["path"] != egresscontrol.AdminSocket || pipe["mode"] != 0o600 || len(pipe) != 2 {
		t.Errorf("admin is %v, not the Unix socket %s egress-control reads, mode 0600, alone", admin, egresscontrol.AdminSocket)
	}
	var logs []any
	walk(b, func(m map[string]any) {
		for _, key := range []string{"access_log_path", "profile_path"} {
			if _, ok := m[key]; ok {
				t.Errorf("the bootstrap writes a file: %s %v", key, m[key])
			}
		}
		if l, ok := m["access_log"].([]any); ok {
			logs = append(logs, l...)
		} else if _, ok := m["access_log"]; ok {
			t.Errorf("an access_log that is no list: %v", m["access_log"])
		}
	})
	if len(logs) != 1 || slices.ContainsFunc(logs, func(l any) bool { return at(l, "typed_config", "@type") != als }) {
		t.Fatalf("the access logs are %v; want ALS alone", logs)
	}
	grpc := at(hcm(t, b), "access_log", 0, "typed_config", "common_config", "grpc_service") // the one log, the connection manager's
	if at(grpc, "envoy_grpc", "cluster_name") != "receiver" || at(grpc, "initial_metadata", 0, "key") != "authorization" ||
		at(grpc, "initial_metadata", 0, "value") != "Bearer @receiver_token@" || at(grpc, "initial_metadata", 1) != nil {
		t.Errorf("ALS goes to %v, not the receiver with authorization: Bearer <CANYON_EGRESS_RECEIVER_TOKEN> alone", grpc)
	}
}

// TestBootstrapRoutesPorts80And443Alone holds every route to the ports design/EGRESS.md §2.1 names, so no
// one asks Envoy for the server's receiver on 7781, the one port of Canyon's networks the floor admits.
func TestBootstrapRoutesPorts80And443Alone(t *testing.T) {
	hosts, _ := at(hcm(t, bootstrap(t)), "route_config", "virtual_hosts").([]any)
	if len(hosts) != 1 {
		t.Fatalf("%d virtual hosts; want one", len(hosts))
	}
	routes, _ := at(hosts[0], "routes").([]any)
	if len(routes) == 0 {
		t.Fatal("no route")
	}
	for i, route := range routes {
		kind, authority := "", (*regexp.Regexp)(nil)
		if _, ok := at(route, "match").(map[string]any)["connect_matcher"]; ok {
			kind = "connect"
		}
		headers, _ := at(route, "match", "headers").([]any)
		for _, h := range headers {
			switch at(h, "name") {
			case ":scheme":
				kind, _ = at(h, "string_match", "exact").(string)
			case ":authority":
				re, _ := at(h, "string_match", "safe_regex", "regex").(string)
				authority = regexp.MustCompile(`^(?:` + re + `)$`) // RE2's full match, as Envoy's
			}
		}
		admits := map[string][]string{"connect": {"api.stripe.com:443"}, "https": {"api.stripe.com", "api.stripe.com:443"}, "http": {"api.stripe.com", "api.stripe.com:80"}}[kind]
		if admits == nil || authority == nil {
			t.Errorf("route %d matches %v: neither a CONNECT nor an absolute-form http or https request whose :authority it names", i, at(route, "match"))
			continue
		}
		for _, a := range admits {
			if !authority.MatchString(a) {
				t.Errorf("route %d (%s) refuses %s", i, kind, a)
			}
		}
		other := map[string]string{"connect": "api.stripe.com:80", "https": "api.stripe.com:80", "http": "api.stripe.com:443"}[kind]
		for _, a := range []string{"10.214.3.2:7781", "api.stripe.com:7781", "api.stripe.com:8443", "api.stripe.com:0443", "api.stripe.com:", other} {
			if authority.MatchString(a) {
				t.Errorf("route %d (%s) admits %s", i, kind, a)
			}
		}
		if kind == "connect" && at(route, "route", "max_stream_duration", "max_stream_duration") != "900s" {
			t.Errorf("a tunnel lives %v, not 15 minutes", at(route, "route", "max_stream_duration"))
		}
		if kind != "connect" && at(route, "route", "timeout") != "900s" {
			t.Errorf("route %d (%s) gives a call %v, not a tunnel's 15 minutes", i, kind, at(route, "route", "timeout"))
		}
	}
}

// TestBootstrapChecksEveryCallFirst: egress-control's check comes before anything is resolved or sent upstream,
// after the RBAC filter alone, over gRPC to its cluster on loopback, within 5 s, the refetch's 4 s and room; a check that does not answer is a 503 with Canyon's line,
// never a call let through, and it is sent none of the app's headers (design/EGRESS.md §3.1; contracts/routes.md,
// The check).
func TestBootstrapChecksEveryCallFirst(t *testing.T) {
	b := bootstrap(t)
	m := hcm(t, b)
	filters, _ := m["http_filters"].([]any)
	var names []any
	for _, f := range filters {
		names = append(names, at(f, "name"))
	}
	if want := []any{"envoy.filters.http.rbac", "envoy.filters.http.ext_authz", "envoy.filters.http.set_filter_state", "envoy.filters.http.dynamic_forward_proxy",
		"envoy.filters.http.router"}; !slices.Equal(names, want) {
		t.Fatalf("the HTTP filters are %v; want %v, the check first but for the RBAC filter that refuses a forged x-canyon-intercept", names, want)
	}
	keys := func(v any) []string { m, _ := v.(map[string]any); return slices.Sorted(maps.Keys(m)) }
	for i, f := range filters { // never disabled, is_optional or a matcher that skips one
		if got := keys(f); !slices.Equal(got, []string{"name", "typed_config"}) {
			t.Errorf("HTTP filter %d is %v; want its name and typed_config alone", i, got)
		}
	}
	check := at(filters[1], "typed_config")
	if got, want := keys(check), []string{"@type", "allowed_headers", "clear_route_cache", "failure_mode_allow", "grpc_service", "status_on_error", "transport_api_version"}; !slices.Equal(got, want) {
		t.Errorf("the check sets %v; want %v alone: any other key may skip it, filter_enabled_metadata or with_request_body among them", got, want)
	}
	upgrades, _ := m["upgrade_configs"].([]any)
	for i, u := range upgrades { // an upgrade with filters of its own would run a chain without the check
		if got := keys(u); !slices.Equal(got, []string{"upgrade_type"}) {
			t.Errorf("the connection manager's upgrade_configs[%d] is %v; want its upgrade_type alone", i, got)
		}
	}
	for _, c := range []struct {
		path []any
		want any
	}{
		{[]any{"@type"}, "type.googleapis.com/envoy.extensions.filters.http.ext_authz.v3.ExtAuthz"},
		{[]any{"transport_api_version"}, "V3"},
		{[]any{"failure_mode_allow"}, false},
		{[]any{"status_on_error", "code"}, "ServiceUnavailable"},
		{[]any{"grpc_service", "envoy_grpc", "cluster_name"}, "control"},
		{[]any{"grpc_service", "timeout"}, "5s"},
		{[]any{"allowed_headers", "patterns", 0, "exact"}, ":method"},
		{[]any{"allowed_headers", "patterns", 1, "exact"}, "proxy-authorization"}, // a pod's token on Kubernetes (design/EGRESS.md §4.3), which the check's OK removes
		{[]any{"allowed_headers", "patterns", 2}, nil},
		{[]any{"clear_route_cache"}, true},
	} {
		if got := at(check, c.path...); got != c.want {
			t.Errorf("the check's %v is %v; want %v", c.path, got, c.want)
		}
	}
	clusters, _ := at(b, "static_resources", "clusters").([]any)
	var control any
	for _, c := range clusters {
		if at(c, "name") == "control" {
			control = c
		}
	}
	if a := at(control, "load_assignment", "endpoints", 0, "lb_endpoints", 0, "endpoint", "address", "socket_address"); at(a, "address") != "@control_address@" || at(a, "port_value") != "@control_port@" {
		t.Errorf("the check's cluster is at %v; want envoy.sh's CANYON_EGRESS_CONTROL_ADDR", a)
	}
	if at(control, "typed_extension_protocol_options", "envoy.extensions.upstreams.http.v3.HttpProtocolOptions", "explicit_http_config", "http2_protocol_options") == nil {
		t.Error("the check's cluster speaks no HTTP/2: gRPC needs it")
	}
	mappers, _ := at(m, "local_reply_config", "mappers").([]any)
	filter := at(mappers, 0, "filter", "and_filter", "filters")
	if len(mappers) != 2 || at(filter, 0, "status_code_filter", "comparison", "op") != "EQ" || at(filter, 0, "status_code_filter", "comparison", "value", "default_value") != 503 ||
		at(filter, 1, "response_flag_filter", "flags", 0) != "UAEX" || at(filter, 1, "response_flag_filter", "flags", 1) != nil || at(filter, 2) != nil ||
		at(mappers, 0, "body", "inline_string") != "Canyon's egress check did not answer, so this call was not made." {
		t.Errorf("the local replies are %v; want the check's 503 (UAEX) answered with Canyon's line first, and upstream_failed after it (TestUpstreamFailedOnlyForAnAbsoluteFormInject)", mappers)
	}
}

// TestNothingButTheCheckWritesItsMetadata: the receiver trusts the metadata namespace envoy.filters.http.ext_authz
// as the check's own (contracts/routes.md, Egress control), so no filter but the check — no header_to_metadata, no
// Lua, no set_metadata — runs, and nothing in the bootstrap names that namespace but the check itself and the one
// local reply that reads it; and no route's per-filter config turns the check off for it. The listener takes the connection's own peer, which the
// check reads as the caller's: no listener filter, PROXY protocol's among them, rewrites it.
func TestNothingButTheCheckWritesItsMetadata(t *testing.T) {
	b := bootstrap(t)
	listeners, _ := at(b, "static_resources", "listeners").([]any)
	for _, l := range listeners {
		if f, ok := l.(map[string]any)["listener_filters"]; ok {
			t.Errorf("listener %v has listener filters %v: one may rewrite the peer the check reads", at(l, "name"), f)
		}
		if chains, _ := at(l, "filter_chains").([]any); len(chains) != 1 || at(chains[0], "filters", 0) == nil || at(chains[0], "filters", 1) != nil {
			t.Errorf("listener %v has filter chains %v; want one, of the connection manager alone", at(l, "name"), chains)
		}
	}
	named := 0
	walk(b, func(m map[string]any) {
		for _, key := range []string{"use_proxy_proto", "proxy_protocol", "filter_metadata", "typed_filter_metadata", "metadata_namespace", "typed_per_filter_config"} {
			if v, ok := m[key]; ok {
				t.Errorf("the bootstrap sets %s: %v", key, v)
			}
		}
		for _, v := range m {
			if v == "envoy.filters.http.ext_authz" {
				named++
			}
		}
	})
	if named != 2 {
		t.Errorf("envoy.filters.http.ext_authz is named %d times; want twice, the check's own filter and upstream_failed's metadata_filter, which reads it", named)
	}
}

// TestEnvoyShFailsClosed runs envoy.sh with a value the server or Envoy would not take: each stops it,
// naming the variable, before it writes a bootstrap or starts Envoy (an envoy on PATH that says so).
func TestEnvoyShFailsClosed(t *testing.T) {
	good := map[string]string{"CANYON_EGRESS_RECEIVER_ADDR": "10.214.3.2:7781", "CANYON_EGRESS_RECEIVER_TOKEN": strings.Repeat("a1", 16), "CANYON_EGRESS_RESOLVERS": "8.8.8.8,8.8.4.4", "CANYON_EGRESS_CONCURRENCY": "4", "CANYON_EGRESS_CONTROL_ADDR": "127.0.0.1:7783"}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "envoy"), []byte("#!/bin/sh\necho envoy started\n"), 0o755); err != nil { //nolint:gosec // the test's stand-in for Envoy, executable
		t.Fatal(err)
	}
	for _, c := range []struct{ key, value string }{
		{"CANYON_EGRESS_RECEIVER_TOKEN", strings.Repeat("a", 31)},
		{"CANYON_EGRESS_RECEIVER_TOKEN", strings.Repeat("a", 513)},
		{"CANYON_EGRESS_RECEIVER_TOKEN", strings.Repeat("a", 31) + "|"},
		{"CANYON_EGRESS_RECEIVER_TOKEN", strings.Repeat("a", 31) + "&"},
		{"CANYON_EGRESS_RECEIVER_TOKEN", strings.Repeat("a", 31) + `"`},
		{"CANYON_EGRESS_RESOLVERS", "8.8.8.8,not-an-address"},
		{"CANYON_EGRESS_RESOLVERS", "8.8.8.8,256.1.1.1"},
		{"CANYON_EGRESS_RESOLVERS", "8.8.8"},
		{"CANYON_EGRESS_RESOLVERS", "8.8.8.08"},
		{"CANYON_EGRESS_RESOLVERS", "8.8.8.8 "},
		{"CANYON_EGRESS_RESOLVERS", ","},
		{"CANYON_EGRESS_RECEIVER_ADDR", "canyon-server:7781"},
		{"CANYON_EGRESS_RECEIVER_ADDR", "10.214.3.2"},
		{"CANYON_EGRESS_RECEIVER_ADDR", "10.214.3.2:70000"},
		{"CANYON_EGRESS_CONCURRENCY", "0"},
		{"CANYON_EGRESS_CONCURRENCY", "four"},
		{"CANYON_EGRESS_CONCURRENCY", "17"},
		{"CANYON_EGRESS_CONTROL_ADDR", "0.0.0.0:7783"},
		{"CANYON_EGRESS_CONTROL_ADDR", "10.214.3.3:7783"},
		{"CANYON_EGRESS_CONTROL_ADDR", "localhost:7783"},
		{"CANYON_EGRESS_CONTROL_ADDR", "127.0.0.1"},
		{"CANYON_EGRESS_CONTROL_ADDR", "127.0.0.1:0"},
		{"CANYON_EGRESS_CONTROL_ADDR", "127.0.0.256:7783"},
	} {
		cmd := exec.Command("sh", "envoy.sh")
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
		for k, v := range good {
			if k == c.key {
				v = c.value
			}
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		out, err := cmd.CombinedOutput()
		if err == nil || strings.Contains(string(out), "envoy started") || !strings.Contains(string(out), c.key) {
			t.Errorf("envoy.sh with %s=%q: %v, and its output does not stop on %s before Envoy starts:\n%s", c.key, c.value, err, c.key, out)
		}
	}
}

// TestBootstrapBoundsIdleConnections: a client that never finishes its headers, a stream, a tunnel's
// among them, that moves no byte, or a connection with no request open — one that never sends a byte among
// them — is closed: never held for ever (#2287).
func TestBootstrapBoundsIdleConnections(t *testing.T) {
	m := hcm(t, bootstrap(t))
	for key, most := range map[string]time.Duration{"request_headers_timeout": 10 * time.Second, "stream_idle_timeout": 5 * time.Minute, "idle_timeout": time.Minute} {
		v := m[key]
		if key == "idle_timeout" {
			v = at(m, "common_http_protocol_options", key)
		}
		s, _ := v.(string)
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 || d > most {
			t.Errorf("%s is %v; want a bound above 0 and at most %v", key, v, most)
		}
	}
}

// TestBootstrapShortensIdleTimeoutsUnderConnectionPressure: from half of Envoy's connections, reaching it at
// 90% of them, the overload manager scales the idle connection's timeout down, no lower than 5 s, below its
// own 60 s, so idle holders are shed before the cap is reached. It scales that one timer alone: an idle
// stream's, a byte every few seconds defeats, and scaled it would cut every app's silent model-API call while
// one app floods (design/EGRESS.md §2.1, #2287).
func TestBootstrapShortensIdleTimeoutsUnderConnectionPressure(t *testing.T) {
	b := bootstrap(t)
	actions, _ := at(b, "overload_manager", "actions").([]any)
	var reduce any
	for _, a := range actions {
		if at(a, "name") == "envoy.overload_actions.reduce_timeouts" {
			reduce = a
		}
	}
	if triggers, _ := at(reduce, "triggers").([]any); len(triggers) != 1 || at(triggers[0], "name") != "envoy.resource_monitors.global_downstream_max_connections" ||
		at(triggers[0], "scaled", "scaling_threshold") != 0.5 || at(triggers[0], "scaled", "saturation_threshold") != 0.9 {
		t.Fatalf("reduce_timeouts is %v; want one trigger, the active connections, scaled from 0.5 to 0.9", reduce)
	}
	if at(reduce, "typed_config", "@type") != "type.googleapis.com/envoy.config.overload.v3.ScaleTimersOverloadActionConfig" {
		t.Errorf("reduce_timeouts' config is %v", at(reduce, "typed_config", "@type"))
	}
	factors, _ := at(reduce, "typed_config", "timer_scale_factors").([]any)
	base, _ := at(hcm(t, b), "common_http_protocol_options", "idle_timeout").(string)
	ceiling, cerr := time.ParseDuration(base)
	least, _ := at(factors, 0, "min_timeout").(string)
	floor, err := time.ParseDuration(least)
	if len(factors) != 1 || at(factors, 0, "timer") != "HTTP_DOWNSTREAM_CONNECTION_IDLE" || least != "5s" || err != nil || cerr != nil || floor >= ceiling {
		t.Errorf("reduce_timeouts scales %v; want HTTP_DOWNSTREAM_CONNECTION_IDLE alone, no lower than 5s, below its own %v", factors, base)
	}
}

// TestBootstrapBoundsEveryStreamAt15Minutes: a call has 15 minutes, as a tunnel does — a request whose body
// trickles in too, which the route's timeout, counted from its last byte, never ends (design/EGRESS.md §2.1).
func TestBootstrapBoundsEveryStreamAt15Minutes(t *testing.T) {
	if d := at(hcm(t, bootstrap(t)), "common_http_protocol_options", "max_stream_duration"); d != "900s" {
		t.Errorf("the connection manager's max_stream_duration is %v; want 900s", d)
	}
}

// TestBootstrapSpeaksHTTP1Alone: :3128 speaks HTTP/1.1, as proxy clients do, never h2c, whose one connection
// would carry streams without end past every bound counted in connections (design/EGRESS.md §2.1).
func TestBootstrapSpeaksHTTP1Alone(t *testing.T) {
	if c := hcm(t, bootstrap(t))["codec_type"]; c != "HTTP1" {
		t.Errorf("the connection manager's codec_type is %v; want HTTP1", c)
	}
}

// TestClustersHoldAsManyAsEnvoyAccepts: each upstream cluster, the DNS cache they share and the check's cluster
// admit as many connections, requests and pending ones as Envoy holds downstream, never Envoy's default 1,024, so
// a few apps' calls never queue every other app's, nor overflow the check into 503s (design/EGRESS.md §2.1).
func TestClustersHoldAsManyAsEnvoyAccepts(t *testing.T) {
	b := bootstrap(t)
	most := at(b, "overload_manager", "resource_monitors", 1, "typed_config", "max_active_downstream_connections")
	if at(b, "overload_manager", "resource_monitors", 1, "name") != "envoy.resource_monitors.global_downstream_max_connections" || most != 16384 {
		t.Fatalf("Envoy's active connections are capped at %v; want 16384", most)
	}
	clusters, _ := at(b, "static_resources", "clusters").([]any)
	for _, c := range clusters {
		if name := at(c, "name"); name == "plain" || name == "tls" || name == "control" || name == "intercept" || name == "intercepted" {
			thresholds, _ := at(c, "circuit_breakers", "thresholds").([]any)
			if len(thresholds) != 1 || at(thresholds[0], "max_connections") != most || at(thresholds[0], "max_pending_requests") != most || at(thresholds[0], "max_requests") != most {
				t.Errorf("cluster %s's circuit breakers are %v; want max_connections, max_pending_requests and max_requests at %v", name, at(c, "circuit_breakers"), most)
			}
			if name == "control" || name == "intercept" || name == "intercepted" { // the check, the internal hop, sub-clusters: no DNS cache of their own
				continue
			}
			if p := at(c, "cluster_type", "typed_config", "dns_cache_config", "dns_cache_circuit_breaker", "max_pending_requests"); p != most {
				t.Errorf("cluster %s's DNS cache admits %v pending resolutions; want %v", name, p, most)
			}
		}
	}
}

// TestBootstrapResolvesNamesAsGiven: every resolver Envoy has looks a name up as given, never under the
// search domains of its resolv.conf — on Kubernetes the cluster's own, where api.stripe.com.svc.cluster.local
// would be asked first.
func TestBootstrapResolvesNamesAsGiven(t *testing.T) {
	n := 0
	walk(bootstrap(t), func(m map[string]any) {
		if m["name"] != "envoy.network.dns_resolver.cares" {
			return
		}
		n++
		if at(m, "typed_config", "dns_resolver_options", "no_default_search_domain") != true {
			t.Errorf("a resolver without no_default_search_domain: %v", m["typed_config"])
		}
	})
	if n == 0 {
		t.Fatal("the bootstrap names no c-ares resolver")
	}
}

// TestBootstrapLogsATunnelAsItOpens: a tunnel is its row from the moment its CONNECT is answered, not up to 15 minutes
// later, and Envoy's exit never loses it (#2684). Every entry carries the tag that says this Envoy logs that entry, the
// one the receiver reads, without which it would count each tunnel at its last entry too (contracts/routes.md).
func TestBootstrapLogsATunnelAsItOpens(t *testing.T) {
	m := hcm(t, bootstrap(t))
	if at(m, "access_log_options", "flush_log_on_tunnel_successfully_established") != true {
		t.Errorf("the access log's options are %v: Envoy logs no tunnel until it ends", at(m, "access_log_options"))
	}
	tags, _ := at(m, "access_log", 0, "typed_config", "common_config", "custom_tags").([]any)
	if len(tags) != 1 || at(tags, 0, "tag") != egresscontrol.OpenEntryTag || at(tags, 0, "literal", "value") != "1" {
		t.Errorf("the access log's tags are %v; want %s, a literal, alone", tags, egresscontrol.OpenEntryTag)
	}
}

// clusterNamed is the bootstrap's static cluster name, nil for none.
func clusterNamed(b map[string]any, name string) any {
	clusters, _ := at(b, "static_resources", "clusters").([]any)
	for _, c := range clusters {
		if at(c, "name") == name {
			return c
		}
	}
	return nil
}

// TestBootstrapInterceptsOnlyWhatTheCheckMarks (design/EGRESS.md §4.2; contracts/routes.md, Interception): the RBAC
// filter, first, refuses any request already carrying x-canyon-intercept; the first route sends a CONNECT to 443 that
// carries it, which only the check's OK sets, to the one internal cluster, every other call taking the routes it took
// before; set_filter_state carries the check's caller and destination into the tunnel, once, nothing when the check
// named none; and the one internal cluster reaches the internal listener egress-control serves.
func TestBootstrapInterceptsOnlyWhatTheCheckMarks(t *testing.T) {
	b := bootstrap(t)
	m := hcm(t, b)
	filters, _ := m["http_filters"].([]any)
	rbac := at(filters[0], "typed_config", "rules")
	if at(rbac, "action") != "DENY" || len(at(rbac, "policies").(map[string]any)) != 1 ||
		at(rbac, "policies", "forged-intercept", "permissions", 0, "header", "name") != "x-canyon-intercept" ||
		at(rbac, "policies", "forged-intercept", "permissions", 0, "header", "present_match") != true ||
		at(rbac, "policies", "forged-intercept", "permissions", 1) != nil || at(rbac, "policies", "forged-intercept", "principals", 0, "any") != true {
		t.Errorf("the RBAC filter's rules are %v; want every request carrying x-canyon-intercept denied, and nothing else", rbac)
	}
	state, _ := at(filters[2], "typed_config", "on_request_headers").([]any)
	for i, object := range []string{"caller", "destination"} {
		want := map[string]any{"object_key": "canyon." + object, "factory_key": "envoy.string", "shared_with_upstream": "ONCE", "skip_if_empty": true,
			"format_string": map[string]any{"text_format_source": map[string]any{"inline_string": "%DYNAMIC_METADATA(envoy.filters.http.ext_authz:" + object + ")%"}, "omit_empty_values": true}}
		if got, _ := at(state, i).(map[string]any); !maps.EqualFunc(got, want, func(a, b any) bool { return fmt.Sprint(a) == fmt.Sprint(b) }) {
			t.Errorf("set_filter_state's object %d is %v; want %v", i, got, want)
		}
	}
	if len(state) != 2 {
		t.Errorf("set_filter_state sets %d objects; want canyon.caller and canyon.destination alone", len(state))
	}
	routes, _ := at(m, "route_config", "virtual_hosts", 0, "routes").([]any)
	first := routes[0]
	headers, _ := at(first, "match", "headers").([]any)
	if _, connect := at(first, "match").(map[string]any)["connect_matcher"]; !connect || len(headers) != 2 || at(headers, 1, "name") != "x-canyon-intercept" ||
		at(headers, 1, "string_match", "exact") != "1" || at(first, "route", "cluster") != "intercept" {
		t.Errorf("the first route is %v; want a CONNECT carrying x-canyon-intercept 1 sent to the cluster intercept", first)
	}
	for i, r := range routes[1:] {
		walk(r, func(n map[string]any) {
			if n["name"] == "x-canyon-intercept" || n["cluster"] == "intercept" {
				t.Errorf("route %d names x-canyon-intercept or the internal cluster: %v", i+1, r)
			}
		})
	}
	internal := clusterNamed(b, "intercept")
	if at(internal, "load_assignment", "endpoints", 0, "lb_endpoints", 0, "endpoint", "address", "envoy_internal_address", "server_listener_name") != egresscontrol.InterceptListener ||
		at(internal, "transport_socket", "name") != "envoy.transport_sockets.internal_upstream" {
		t.Errorf("the internal cluster is %v; want internal_upstream to egress-control's listener %s", internal, egresscontrol.InterceptListener)
	}
	if at(b, "bootstrap_extensions", 0, "name") != "envoy.bootstrap.internal_listener" {
		t.Errorf("the bootstrap's extensions are %v; want the internal listener's", at(b, "bootstrap_extensions"))
	}
	dynamic := at(b, "dynamic_resources")
	if got := slices.Sorted(maps.Keys(dynamic.(map[string]any))); !slices.Equal(got, []string{"ads_config", "lds_config"}) ||
		at(dynamic, "lds_config", "ads") == nil || at(dynamic, "ads_config", "api_type") != "GRPC" ||
		at(dynamic, "ads_config", "grpc_services", 0, "envoy_grpc", "cluster_name") != "control" || at(dynamic, "ads_config", "grpc_services", 1) != nil {
		t.Errorf("the dynamic resources are %v; want listeners alone, by ADS from egress-control's cluster control: no clusters come by xDS", dynamic)
	}
}

// TestUpstreamTLSNeverResumes: Envoy originates TLS to a destination it adds a credential for — the absolute-form
// branch's cluster tls and an inner request's intercepted — verifying its certificate by the image's own roots and its
// name by SNI, never resuming a session, which a shared CDN address could carry from one destination to another
// (contracts/routes.md, Interception, Upstream). intercepted keeps no DNS cache: its sub-clusters resolve as :3128's
// resolvers do, so the served chains' dynamic forward proxy holds no resolver of its own.
func TestUpstreamTLSNeverResumes(t *testing.T) {
	b := bootstrap(t)
	for _, name := range []string{"tls", egresscontrol.InterceptedCluster} {
		c := clusterNamed(b, name)
		tls := at(c, "transport_socket", "typed_config")
		if at(tls, "max_session_keys") != 0 || at(tls, "common_tls_context", "validation_context", "trusted_ca", "filename") != "/etc/ssl/certs/ca-certificates.crt" ||
			at(c, "typed_extension_protocol_options", "envoy.extensions.upstreams.http.v3.HttpProtocolOptions", "upstream_http_protocol_options", "auto_sni") != true ||
			at(c, "typed_extension_protocol_options", "envoy.extensions.upstreams.http.v3.HttpProtocolOptions", "upstream_http_protocol_options", "auto_san_validation") != true {
			t.Errorf("cluster %s's upstream TLS is %v; want max_session_keys 0, the image's roots, SNI and its SAN checked", name, tls)
		}
	}
	intercepted := clusterNamed(b, egresscontrol.InterceptedCluster)
	if at(intercepted, "cluster_type", "typed_config", "sub_clusters_config") == nil || at(intercepted, "cluster_type", "typed_config", "dns_cache_config") != nil ||
		at(intercepted, "typed_dns_resolver_config", "typed_config", "dns_resolver_options", "no_default_search_domain") != true {
		t.Errorf("cluster %s is %v; want sub-clusters resolved by :3128's resolvers, no DNS cache", egresscontrol.InterceptedCluster, intercepted)
	}
}

// TestUpstreamFailedOnlyForAnAbsoluteFormInject: on :3128 Canyon's upstream_failed line answers only a request that is
// no CONNECT, whose check's OK named a destination — an absolute-form call the check added a credential to — and
// whose connection, handshake, certificate or name failed before it was sent (UF, DF); a plain CONNECT never reads it,
// and an intercepted one that finds no internal listener stays 503 (contracts/routes.md, Interception, Upstream). The
// metadata_filter sets match_if_key_not_found false: Envoy's default matches a call the check named no destination
// for, which answered a floor's refusal of a plain http:// call 502 upstream_failed on the stack.
func TestUpstreamFailedOnlyForAnAbsoluteFormInject(t *testing.T) {
	mappers, _ := at(hcm(t, bootstrap(t)), "local_reply_config", "mappers").([]any)
	if len(mappers) != 2 {
		t.Fatalf("%d local replies; want the check's 503 and upstream_failed", len(mappers))
	}
	m := mappers[1]
	and, _ := at(m, "filter", "and_filter", "filters").([]any)
	if len(and) != 3 || fmt.Sprint(at(and, 0, "response_flag_filter", "flags")) != "[UF DF]" ||
		at(and, 1, "metadata_filter", "matcher", "filter") != "envoy.filters.http.ext_authz" || at(and, 1, "metadata_filter", "matcher", "path", 0, "key") != "destination" ||
		at(and, 1, "metadata_filter", "matcher", "value", "present_match") != true || at(and, 1, "metadata_filter", "match_if_key_not_found") != false ||
		at(and, 2, "header_filter", "header", "name") != ":method" || at(and, 2, "header_filter", "header", "string_match", "exact") != "CONNECT" ||
		at(and, 2, "header_filter", "header", "invert_match") != true {
		t.Errorf("upstream_failed answers %v; want UF or DF, a destination the check named, and no CONNECT, all three", and)
	}
	if at(m, "status_code") != 502 || at(m, "body_format_override", "text_format_source", "inline_string") !=
		"Canyon could not connect to %DYNAMIC_METADATA(envoy.filters.http.ext_authz:destination)% or verify its certificate, so it did not send this call." {
		t.Errorf("upstream_failed is %v; want 502 with the contract's line, naming the destination", m)
	}
	headers := map[string]any{}
	for _, h := range at(m, "headers_to_add").([]any) {
		headers[fmt.Sprint(at(h, "header", "key"))] = at(h, "header", "value")
	}
	if fmt.Sprint(headers) != "map[Canyon-Egress:upstream_failed content-type:text/plain; charset=utf-8]" {
		t.Errorf("upstream_failed carries %v; want Canyon-Egress upstream_failed and the check's content-type", headers)
	}
}
