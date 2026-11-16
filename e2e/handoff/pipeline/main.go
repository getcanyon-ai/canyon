// Command pipeline is the org's pipeline in the hand-off e2e (design/HAND-OFF.md §9 item 8): what an
// org's Argo CD or Actions job does with Canyon's release, in one small server. It takes the release
// webhook and verifies its signature, refuses a stale sent_at and treats a delivery it has seen as
// done (§3), fetches the release's access token under the host token (§5), provides the databases the
// release names on the org's own Postgres (§3, Resources), runs the app and its sidecar in the cluster
// — or removes them, and on a project delete's remove drops those databases — and reports back, with
// where each database is (§4).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/getcanyon-ai/canyon/webhook"
)

// release is the part of release.schema.json's deploy and remove the pipeline acts on.
type release struct {
	Event, Delivery, Release, App, Environment, Image string
	Port                                              int
	AppEnv                                            map[string]string `json:"app_env"`
	Resources                                         []resource        // what the app needs: the platform provides each (design/HAND-OFF.md §3)
	Delete                                            bool              // a project delete's remove: the platform deletes what it provided too
	Sidecar                                           struct {
		Image  string
		Port   int
		Env    map[string]string
		Secret string
	}
	ReportURL string `json:"report_url"`
	SentAt    string `json:"sent_at"`
}

// resource is one of a deploy's resources: its name, its type, and the variables the app reads from it.
type resource struct {
	Name, Type string
	Gives      []string
}

// endpoint is where a provided resource is, as the deployed report names it: never a credential.
type endpoint struct {
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
}

// cluster runs or removes a release's app: kube (client-go) in the e2e and on the test cluster, a test's own in a test.
// run puts values, the variables the platform built for the app's resources, in the app's environment;
// remove of a project delete's remove (r.Delete) takes them away too.
type cluster interface {
	run(ctx context.Context, r release, accessToken string, values map[string]string) (address string, err error)
	remove(ctx context.Context, r release) error
}

// platform provides an app's resources and deletes them: the org's Postgres in the e2e, a test's own in a test.
type platform interface {
	provide(ctx context.Context, app, env string, res resource) (values map[string]string, endpoint string, err error)
	drop(ctx context.Context, app, env string) error
}

type pipeline struct {
	secret    []byte // the platform webhook's signing secret
	hostToken string // what Canyon's report and access-token routes take
	cluster   cluster
	platform  platform // nil: this pipeline provides no resources, and a release that needs one fails
	client    *http.Client
	now       func() time.Time
	mu        sync.Mutex
	seen      map[string]time.Time // delivery → when first seen, kept 30 minutes
	done      sync.WaitGroup       // handlings in flight: a test waits on it
}

// freshness is how old a sent_at the pipeline takes, and how long it remembers a delivery (§3).
const freshness = 30 * time.Minute

func (p *pipeline) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil || !webhook.Verify(p.secret, body, r.Header.Get("Canyon-Signature-256")) {
		http.Error(w, "the signature does not verify", http.StatusUnauthorized)
		return
	}
	var rel release
	sent, terr := time.Time{}, json.Unmarshal(body, &rel)
	if terr == nil {
		sent, terr = time.Parse(time.RFC3339, rel.SentAt)
	}
	if d := p.now().Sub(sent); terr != nil || d > freshness || d < -freshness { // a sent_at ahead too, or its memory would outlive its window
		http.Error(w, "not a fresh release", http.StatusBadRequest)
		return
	}
	if rel.Event == "ping" { // a test delivery (#2439), dispatched on the signed body's event, never the header: verified and fresh, so answered, and nothing run
		w.WriteHeader(http.StatusAccepted)
		return
	}
	p.mu.Lock()
	for d, at := range p.seen {
		if p.now().Sub(at) > freshness {
			delete(p.seen, d)
		}
	}
	_, dup := p.seen[rel.Delivery]
	p.seen[rel.Delivery] = p.now()
	p.mu.Unlock()
	w.WriteHeader(http.StatusAccepted) // accepted: the work runs after the answer, as a pipeline's job does
	if !dup {
		p.done.Add(1)
		go func() { defer p.done.Done(); p.handle(context.Background(), rel) }()
	}
}

// handle does what the release asks and reports how it went.
func (p *pipeline) handle(ctx context.Context, rel release) {
	report := map[string]any{"status": "removed"}
	var err error
	switch rel.Event {
	case "deploy":
		var token, address string
		var provided []endpoint
		values := map[string]string{}
		if token, err = p.accessToken(ctx, rel); err == nil {
			provided, err = p.provide(ctx, rel, values)
		}
		if err == nil {
			address, err = p.cluster.run(ctx, rel, token, values)
		}
		_, digest, _ := strings.Cut(rel.Image, "@")
		report = map[string]any{"status": "deployed", "address": address, "digest": digest}
		if len(provided) > 0 {
			report["resources"] = provided
		}
	case "remove":
		err = p.cluster.remove(ctx, rel)
		if err == nil && rel.Delete && p.platform != nil { // only a project delete's remove deletes data: an undeploy's keeps it
			err = p.platform.drop(ctx, rel.App, rel.Environment)
		}
	default:
		err = fmt.Errorf("an event this pipeline does not run: %q", rel.Event)
	}
	if err != nil { // one line, at most the 1024 code points the report takes (release.schema.json), or the report itself is refused
		reason := []rune(strings.Join(strings.Fields(err.Error()), " "))
		report = map[string]any{"status": "failed", "reason": string(reason[:min(len(reason), 1024)])}
	}
	if err := p.call(ctx, http.MethodPost, rel.ReportURL, report, nil); err != nil {
		log.Printf("pipeline: report on %s: %v", rel.Release, err)
	}
}

// provide makes each resource the release names and adds the variables each gives to values; it answers
// where each one is, for the report.
func (p *pipeline) provide(ctx context.Context, rel release, values map[string]string) ([]endpoint, error) {
	var out []endpoint
	for _, res := range rel.Resources {
		if p.platform == nil {
			return nil, fmt.Errorf("this platform provides no %s resources", res.Type)
		}
		got, at, err := p.platform.provide(ctx, rel.App, rel.Environment, res)
		if err != nil {
			return nil, err
		}
		maps.Copy(values, got)
		out = append(out, endpoint{Name: res.Name, Endpoint: at})
	}
	return out, nil
}

// accessToken is GET /releases/:id/access-token, the report URL's sibling, under the host token.
func (p *pipeline) accessToken(ctx context.Context, rel release) (string, error) {
	var answer struct{ Name, Value string }
	if err := p.call(ctx, http.MethodGet, strings.TrimSuffix(rel.ReportURL, "/report")+"/access-token", nil, &answer); err != nil {
		return "", err
	}
	if answer.Name != rel.Sidecar.Secret || answer.Value == "" {
		return "", errors.New("the access token route answered no " + rel.Sidecar.Secret)
	}
	return answer.Value, nil
}

func (p *pipeline) call(ctx context.Context, method, url string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.hostToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: %s", method, url, resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// main runs the pipeline against whichever cluster client-go's standard loading names ($KUBECONFIG, or
// the pod's own service account), so the one binary serves kind in CI and the AWS test cluster alike. Its
// settings: PIPELINE_LISTEN, PIPELINE_WEBHOOK_SECRET, PIPELINE_HOST_TOKEN, PIPELINE_NAMESPACE,
// PIPELINE_APPS_DOMAIN (the reported address is https://<app>.<env>.<domain>), PIPELINE_CA_FILE
// (a CA the sidecar trusts for Canyon's URL, when Canyon's certificate is the test cluster's own) and
// PIPELINE_DATABASE_URL (the org's Postgres, as its admin, where it provides postgres resources; unset,
// it provides none).
func main() {
	if os.Getenv("PIPELINE_WEBHOOK_SECRET") == "" || os.Getenv("PIPELINE_HOST_TOKEN") == "" { // an empty key verifies anything signed with it: refuse to start
		log.Fatal("pipeline: PIPELINE_WEBHOOK_SECRET and PIPELINE_HOST_TOKEN are required")
	}
	k, err := newKube(os.Getenv("PIPELINE_NAMESPACE"), os.Getenv("PIPELINE_APPS_DOMAIN"), os.Getenv("PIPELINE_CA_FILE"))
	if err != nil {
		log.Fatal(err)
	}
	p := &pipeline{secret: []byte(os.Getenv("PIPELINE_WEBHOOK_SECRET")), hostToken: os.Getenv("PIPELINE_HOST_TOKEN"), cluster: k,
		client: &http.Client{Timeout: 30 * time.Second}, now: time.Now, seen: map[string]time.Time{}}
	if admin := os.Getenv("PIPELINE_DATABASE_URL"); admin != "" {
		pg, err := newPostgresPlatform(admin)
		if err != nil {
			log.Fatal(err)
		}
		p.platform = pg
	}
	log.Fatal((&http.Server{Addr: os.Getenv("PIPELINE_LISTEN"), Handler: p, ReadHeaderTimeout: 10 * time.Second}).ListenAndServe())
}
