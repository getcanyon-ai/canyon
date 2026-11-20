// Command canyon-server is the Canyon control plane: the brain the CLI,
// dashboard, and GitHub App courier all talk to (design/ARCHITECTURE.md §1).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/getcanyon-ai/canyon/contracts"
	"github.com/getcanyon-ai/canyon/db"
	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/gate/custom"
	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
	githubapp "github.com/getcanyon-ai/canyon/github-app"
	"github.com/getcanyon-ai/canyon/identity"
	"github.com/getcanyon-ai/canyon/provisioners"
	"github.com/getcanyon-ai/canyon/runs"
	"github.com/getcanyon-ai/canyon/vault"
	"github.com/getcanyon-ai/canyon/web"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// version is stamped at build time: -ldflags "-X main.version=<git describe>".
var version = "dev"

// commands are canyon-server's: with none it runs the server, and any other argument is refused before anything is
// opened, so a typo in a debugging command never boots the server inside a live pod (#2693). Fail closed.
var commands = []string{"egress-control", "rotate-kek", "reissue-db-passwords", "egress-policy-cert"}

const usage = "usage: canyon-server [egress-control | rotate-kek | reissue-db-passwords | egress-policy-cert]\nWith no command, it runs the server.\n"

func main() {
	if len(os.Args) > 1 && !slices.Contains(commands, os.Args[1]) {
		fmt.Fprintf(os.Stderr, "canyon-server: unknown command %q\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if len(os.Args) > 1 && os.Args[1] == "egress-control" { // the check beside Envoy (design/EGRESS.md §3.1): no database, no vault
		if err := egressControl(ctx, os.Getenv, os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}

	keyFile := os.Getenv("CANYON_KEY_FILE") // the server's, rotate-kek's and reissue-db-passwords'.
	if keyFile == "" {
		keyFile = "/var/lib/canyon/keys/kek"
	}
	// The operator's commands, run beside the server: rotate-kek (design/ARCHITECTURE.md, the vault),
	// reissue-db-passwords (contracts/provisioner.md, After a restore) and egress-policy-cert (deploy/k8s/README.md).
	if len(os.Args) > 1 && (os.Args[1] == "rotate-kek" || os.Args[1] == "reissue-db-passwords" || os.Args[1] == "egress-policy-cert") {
		url := os.Getenv("CANYON_DATABASE_URL")
		if url == "" {
			log.Fatal("CANYON_DATABASE_URL is not set")
		}
		pool, err := db.Open(ctx, url, 0)
		if err != nil {
			log.Fatal(err)
		}
		defer pool.Close()
		switch os.Args[1] {
		case "rotate-kek":
			err = rotateKEK(ctx, pool, keyFile, os.Stdout)
		case "egress-policy-cert": // the certificate egress-control pins, which deploy/k8s's install step publishes
			err = printPolicyCert(ctx, pool, os.Stdout)
		default:
			err = reissueDBPasswords(ctx, pool, keyFile, os.Getenv, os.Stdout)
		}
		if err != nil {
			log.Fatal(err)
		}
		return
	}

	addr := os.Getenv("CANYON_ADDR")
	if addr == "" {
		addr = "127.0.0.1:7777"
	}
	mode, err := authModeOf(os.Getenv("CANYON_AUTH")) // unset is oidc
	if err != nil {
		log.Fatal(err)
	}
	var oidc *oidcConfig
	if mode == authOIDC { // the shape is fatal before anything else is touched: a typo never replaces a working process
		if oidc, err = oidcConfigOf(os.Getenv); err != nil {
			log.Fatal(err)
		}
	}
	public, err := publicURLFor(mode, os.Getenv("CANYON_PUBLIC_URL"))
	if err != nil { // fail at boot, not at the first deploy
		log.Fatal(err)
	}
	if err := refuseExposed(mode, addr, public); err != nil {
		log.Fatal(err)
	}
	publish, err := publishedAddrOf(os.Getenv("CANYON_PUBLISHED_ADDR"))
	if err != nil {
		log.Fatal(err)
	}
	gateway, err := publishedGateway(mode, publish) // at boot: a demo published beyond loopback never starts
	if err != nil {
		log.Fatal(err)
	}
	kube, err := kubernetesOf(os.Getenv("CANYON_RUNTIME")) // docker unless kubernetes: the front-door route reads it
	if err != nil {
		log.Fatal(err)
	}
	apps, err := appsSubnetOf(os.Getenv("CANYON_APPS_SUBNET")) // the pool of app networks, refused at accept but for each /24's gateway (appnet.go); unset refuses nothing
	if err != nil {
		log.Fatal(err)
	}
	trusted, err := proxiesOf(os.Getenv("CANYON_TRUSTED_PROXIES")) // the proxies whose X-Forwarded-For names a sign-in's source; unset, the peer does
	if err != nil {
		log.Fatal(err)
	}
	askToken, err := askTokenOf(os.Getenv("CANYON_TLS_ASK_TOKEN")) // a proxy with no address to trust asks /tls/ask with it; unset, none
	if err != nil {
		log.Fatal(err)
	}
	jwksListen, jwksURL, err := jwksAddrOf(os.Getenv("CANYON_APPS_JWKS_ADDR"))
	if err != nil {
		log.Fatal(err)
	}
	var idp *provider // nil under off and demo: no discovery runs and /ready never waits on it
	if oidc != nil {
		log.Printf("canyon-server: auth oidc, issuer %s", oidc.issuer)
		idp = discover(ctx, discoveryClient(), oidc, log.Default())
	} else {
		log.Printf("canyon-server: auth %s, every caller is an admin", mode)
	}
	url := os.Getenv("CANYON_DATABASE_URL")
	if url == "" { // fail closed: left empty, pgx would assemble a connection from PG* variables and .pgpass
		log.Fatal("CANYON_DATABASE_URL is not set")
	}
	conns, err := poolSize(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	pool, err := db.Open(ctx, url, conns)
	if err != nil {
		log.Fatalf("CANYON_DATABASE_URL: %v", err)
	}
	defer pool.Close()
	if pool.Config().ConnConfig.TLSConfig == nil {
		log.Print("canyon-server: Canyon's own database connection is unencrypted: CANYON_DATABASE_URL says sslmode=disable")
	}
	probes, err := db.Open(ctx, url, 1) // readiness's own connection: request traffic never starves /ready (#1693)
	if err != nil {
		log.Fatal(err)
	}
	defer probes.Close()
	if _, err := db.Migrate(ctx, pool); err != nil {
		log.Fatal(err)
	}
	if err := drawEpoch(ctx, pool); err != nil { // the egress policy's, this start's own (design/EGRESS.md §3.2)
		log.Fatal(err)
	}
	if _, err := sweepSessions(ctx, pool); err != nil {
		log.Fatal(err)
	}
	if voided, err := voidApprovals(ctx, pool, mode == authOIDC); err != nil { // before any run is recovered: none reads an approval its rule voids
		log.Fatal(err)
	} else if voided > 0 {
		log.Printf("canyon-server: %d unexpired approval(s) cover no blocking finding: filed before the approver rule (#2204) or with sign-in off; an admin or a security responder approves each again in the dashboard", voided) //nolint:gosec // a count the server's own query answered
	}
	if seeded, err := seedDemo(ctx, pool); err != nil { // the demo host's roster: after the tables exist, before anything reads them
		log.Fatal(err)
	} else if seeded {
		log.Print("canyon-server: seeded the demo fleet roster")
	}
	if err := stampAnonymous(ctx, pool); err != nil { // before the listener: sign-in never reads the anonymous admin's subject as an identity provider's (#2306)
		log.Fatal(err)
	}
	durable, err := runs.NewDurable(ctx, url, "dbos", os.Stderr)
	if err != nil {
		log.Fatal(err)
	}
	repos := os.Getenv("CANYON_REPOS")
	if repos == "" {
		repos = "/var/lib/canyon/repos"
	}
	snap, err := loadSnapshot(contracts.Snapshot)
	if err != nil {
		log.Fatal(err)
	}
	keys, err := vault.Load(keyFile) // generated on first boot; every secret is sealed under it, so it is fatal to lose and refused when foreign
	if err != nil {
		log.Fatal(err)
	}
	if err := refuseNewKeyOverSealedRows(ctx, pool, keys); err != nil {
		log.Fatal(err)
	}
	if err := refuseRowsUnderAMissingKey(ctx, pool, keys); err != nil {
		log.Fatal(err)
	}
	idKey, err := identityKey(ctx, pool, keys) // generated and sealed at the first boot; only its public half is ever served
	if err != nil {
		log.Fatal(err)
	}
	jwks, err := idKey.JWKS()
	if err != nil {
		log.Fatal(err)
	}
	m, review, err := newModels() // a model this build cannot serve fails the boot, not the first turn or run
	if err != nil {
		log.Fatal(err)
	}
	if review.Model == nil {
		log.Print("canyon-server: no model configured; every AI review shows as skipped")
	} else if m == nil {
		log.Printf("canyon-server: AI review on %s; the setup chat is off, as it speaks the Claude API only", review.ModelID)
	}
	executorURL := os.Getenv("CANYON_EXECUTOR_URL")
	if executorURL == "" {
		executorURL = "http://executor:7778"
	}
	policySnap, err := loadPolicySnapshot(contracts.Snapshot)
	if err != nil {
		log.Fatal(err)
	}
	identityImage, err := executor.ImageSetting(os.Getenv, "CANYON_IMAGE_IDENTITY") // the sidecar every release names: by digest, as the executor takes it
	if err != nil {
		log.Fatal(err)
	}
	review.Pool, review.SignedIn = pool, mode == authOIDC
	pipe := &runs.Pipeline{SignInOff: mode != authOIDC, Pool: pool, Repos: repos, Gate: review.Review, Executor: executorClient(executorURL), PublicURL: public.Scheme + "://" + public.Host, JWKSURL: jwksURL, Vault: keys, IdentityImage: identityImage} // the public URL as the token's issuer
	if err := refuseHandOffUnserved(ctx, pool, pipe); err != nil {
		log.Fatal(err)
	}
	if pipe.ScanConcurrency, pipe.BuildConcurrency, pipe.PatchConcurrency, err = capacities(os.Getenv); err != nil { // fail at boot, not at a run's first scan
		log.Fatal(err)
	}
	patches, err := patchesOn(os.Getenv, mode) // fail at boot on a value that is neither on nor off
	if err != nil {
		log.Fatal(err)
	}
	if pipe.RescanEvery, err = patchInterval(os.Getenv); err != nil { // fail at boot, not at the first tick
		log.Fatal(err)
	}
	watch, err := patchWatch(os.Getenv)
	if err != nil { // fail at boot, not at the first patch's watch
		log.Fatal(err)
	}
	pipe.PatchWatch.Store(watch)
	appDB := os.Getenv("CANYON_APP_DATABASE_URL")
	caFile := os.Getenv("CANYON_APP_DATABASE_CA_FILE")
	if pipe.DatabaseCA, err = databaseCAOf(caFile, appDB, kube); err != nil { // fail at boot, not at an app's first query
		log.Fatal(err)
	}
	if pipe.EgressProxy, err = egressProxyOf(os.Getenv("CANYON_EGRESS_PROXY")); err != nil { // fail at boot, not at a deploy under via canyon
		log.Fatal(err)
	}
	behind, err := gatewaySigninOf(os.Getenv("CANYON_KUBE_GATEWAY_SIGNIN"))
	if err != nil {
		log.Fatal(err)
	}
	var builtin provisioners.Driver
	if appDB != "" { // the server apps' databases live on; unset, the built-in driver is off and a deploy declaring postgres fails naming the setting
		b, err := provisioners.NewBuiltin(ctx, appDB, caFile)
		if err != nil {
			log.Fatalf("CANYON_APP_DATABASE_URL: %v", err)
		}
		defer b.Close()
		builtin = b
	}
	if file := os.Getenv("CANYON_PROVISIONERS"); file != "" || builtin != nil { // the platform's driver per type and env; unset, builtin for postgres in both
		if pipe.Drivers, err = provisioners.Load(file, builtin); err != nil {
			log.Fatal(err)
		}
	}
	if err := pipe.Register(durable); err != nil { // before Launch: the registry is read from then on
		log.Fatal(err)
	}
	githubAPI := os.Getenv("CANYON_GITHUB_API")
	if githubAPI == "" {
		githubAPI = "https://api.github.com"
	}
	menu, err := componentsOf(snap.ComponentMenu)
	if err != nil {
		log.Fatal(err)
	}
	orgEnv, err := orgEnvOf(os.Getenv("CANYON_ORG"))
	if err != nil {
		log.Fatal(err)
	}
	telemetry, err := telemetryConfigOf(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	ee, err := wireEnterprise(os.Getenv, keys)
	if err != nil {
		log.Fatal(err)
	}
	pipe.Inject = ee.outbound // before Launch: a resumed run carries what a new one does; nil without Canyon Enterprise
	if ee.licence == nil && os.Getenv("CANYON_LICENSE") != "" {
		log.Print("canyon-server: CANYON_LICENSE is set, but this Canyon is built without Canyon Enterprise, so the licence unlocks nothing")
	}
	dbMaxConns, err := dbMaxConnsOf(os.Getenv("CANYON_DB_MAX_CONNECTIONS"))
	if err != nil {
		log.Fatal(err)
	}
	a := &app{pool: pool, repos: repos, snap: snap, boot: policySnap, vault: keys, durable: durable, pipe: pipe, model: m, reviews: review.Model != nil, modelID: review.ModelID, github: githubAPI, ghWeb: "https://github.com", idKey: idKey, own: public.Host, public: public.Scheme + "://" + public.Host, orgEnv: orgEnv, auth: mode, publish: publish, oidc: oidc, idp: idp, kube: kube, behind: behind, proxies: trusted, askToken: askToken, ready: func(logger *log.Logger) *readiness {
		return newReadiness(ctx, probes, durable, 5*time.Second, logger, idp)
	}, dbSweep: 15 * time.Second, telemetry: telemetry, ee: ee, dbMaxConns: dbMaxConns, replica: durable.GetExecutorID(), notify: true}
	if kube { // before Launch: a resumed run sends what a new one does
		pipe.Access = a.runAccess
	}
	a.translator, _ = review.Model.(custom.Model)                                                                                                            // the Anthropic and OpenAI clients both ask outside the review
	pipe.Org = func(ctx context.Context) string { return a.orgNow(ctx).name }                                                                                // a deploy's access line names the org as it stands (contracts/events.md, build.log)
	pipe.Clears = a.clearsReview                                                                                                                             // before Launch: a resumed promotion clears its review again as a new one does
	pipe.Patcher = ee.patching                                                                                                                               // before Launch: a resumed watch files its patch approval through it; nil in a build without Canyon Enterprise
	pipe.DecidePatch = a.decidePatch                                                                                                                         // the fleet approval decides a patch approval through its approve route
	pipe.Attach = func(ctx context.Context, project, pkg, tool string, raw []byte, unloaded, unparsed []string, ev runs.Evidence, by actor) (int64, error) { // a run's own scans attach as an upload does, without the route's limit
		answer, err := a.attachReport(ctx, project, pkg, raw, by, tool, unloaded, unparsed, ev)
		return answer.id, err
	}
	pipe.ProjectPolicy.Store(func(ctx context.Context, q runs.Querier, project string) (gatepolicy.Snapshot, error) { // the rescan reads a project's images while its rule is on; a patch run's start composes it on its own transaction
		return a.projectPolicy(ctx, q, project)
	})
	pipe.Patches.Store(patches)
	if err := a.patchingAtStart(ctx, patches, log.Default()); err != nil { // a new install without a licence starts patching off (contracts/routes.md, Licence)
		log.Fatal(err)
	}
	pipe.PlatformRead.Store(func(ctx context.Context, project, digest string) (runs.PlatformImage, error) { // the patch plan and a patch run read an image as the receipt does
		f, err := loadImage(ctx, a.pool, project, digest)
		if err != nil {
			return runs.PlatformImage{}, err
		}
		return platformImage(readImage(f, "preview", digest, gatepolicy.Advisory, platformRule, time.Now())), nil
	})
	(&githubapp.Courier{Pool: pool, Vault: keys, Durable: durable, API: githubAPI, Public: a.public, Repos: repos, Exclusions: snap.Exclusions, Log: log.Default(),
		Declare: func(tree string) (map[string]string, map[string]string, *string, error) {
			offered, err := a.provisioned(ctx, slices.Clone(menu))
			if err != nil {
				return nil, nil, nil, err
			}
			return declarations(tree, offered)
		}, // the upload's check and audit, so a pushed tree declares as an uploaded one
		FirstDeclarations: func(ctx context.Context, tx pgx.Tx, project string, declared map[string]string) ([]string, error) {
			return firstDeclarations(ctx, tx, project, declared)
		},
		Policy: func(ctx context.Context, tx pgx.Tx, project string) (gatepolicy.Snapshot, error) {
			return a.projectPolicy(ctx, tx, project)
		}, // as a user's start composes it
		Attach: func(ctx context.Context, project, pkg string, raw []byte, by actor) error {
			_, err := a.attachReport(ctx, project, pkg, raw, by, "", nil, nil, runs.Evidence{}) // any tool: the code-scanning SARIF GitHub holds
			return err
		},
	}).Register(durable)
	stopDurable, err := runs.Launch(context.WithoutCancel(ctx), pool, durable, "dbos") // its row lives until stopDurable, past SIGTERM
	if err != nil {
		log.Fatal(err)
	}
	defer stopDurable()
	if n, err := pipe.Reconcile(ctx, durable); err != nil { // and ends the runs it will not: before the listener, so nothing races the sweep
		log.Fatal(err)
	} else if n > 0 {
		log.Printf("canyon-server: ended %d run(s) the driver could not finish", n)
	}
	go forgetDeleted(ctx, pool, pipe.Executor, log.Default(), time.Second) // off the boot path: the executor may come up after the server
	go pipe.ReportQueues(ctx, durable)                                     // each waiting scan's and build's place in line, onto its run
	if kube {
		go a.recordDiscovery(ctx, log.Default(), time.Minute) // a choice 2 front door saved before its discovery was recorded
		go a.reconcileAccess(ctx, log.Default(), time.Minute)
		go a.rollSidecars(ctx, log.Default(), time.Minute)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	ln = refuseAppNet(ln, apps)
	// Under demo behind a published port, the gateway is served once Docker publishes this container on loopback
	// alone (#2491); a host name unread is "", which no container's Id begins, so the gateway stays closed.
	self, _ := os.Hostname()
	open := publishedGatewayOpen(ctx, gateway, pipe.Executor.Published, self, ln.Addr().(*net.TCPAddr).Port, log.Default(), func(err error) { log.Fatal(err) })
	ln = localPeers(ln, mode, open)              // under off and demo every caller is an admin: this machine's alone are served
	jwksLn, err := net.Listen("tcp", jwksListen) // not refuseAppNet's: apps must reach it, and it answers the key set alone
	if err != nil {
		log.Fatal(err)
	}
	jwksSrv := &http.Server{Handler: jwksHandler(jwks), ReadHeaderTimeout: 10 * time.Second}
	go func() { log.Fatal(jwksSrv.Serve(jwksLn)) }()
	als, alsToken := os.Getenv("CANYON_EGRESS_RECEIVER_ADDR"), os.Getenv("CANYON_EGRESS_RECEIVER_TOKEN") // Envoy's access logs (design/EGRESS.md §2.4); unset, no receiver
	on, err := egressReceiverOn(als, kube, apps, alsToken)
	if err != nil {
		log.Fatal(err)
	}
	var received <-chan struct{} // the receiver's last flush, done; nil with no receiver
	if on {
		alsLn, err := listenReceiver(als)
		if err != nil {
			log.Fatal(err)
		}
		a.egress = newEgressReceiver(pool, pipe.Executor.Callers, log.Default())
		var failed <-chan error
		received, failed = receiveEgress(ctx, alsLn, apps, a.egress, alsToken) // no app, and no host through an app network
		go func() { log.Fatal(<-failed) }()                                    // only an error: a stopped receiver sends nothing
	}
	policy, err := egressPolicyAddr(os.Getenv("CANYON_EGRESS_POLICY_ADDR"), als, kube) // the policy egress-control pulls (design/EGRESS.md §3.2)
	if err != nil {
		log.Fatal(err)
	}
	if policy != "" { // unset on Docker with no receiver, or on Kubernetes, none is served
		if err := serveEgressPolicy(ctx, policy, kube, apps, a, keys, "/var/run/secrets/kubernetes.io/serviceaccount"); err != nil {
			log.Fatal(err)
		}
	}
	if _, err := a.effectivePolicy(ctx, pool); err != nil { // every run starts with a policy composed over the edits: a table the composer cannot read fails the boot, not the first run
		log.Fatal(err)
	}
	if err := serve(ctx, ln, os.Stderr, a); err != nil {
		log.Fatal(err)
	}
	if received != nil {
		<-received
	}
}

// app is what the routes reach: the pool, the repos directory, the policy snapshot, the logger
// serve builds, and the readiness prober built where the server logs.
type app struct {
	pool     *pgxpool.Pool
	repos    string
	snap     snapshot
	vault    *vault.Vault     // the key file: secrets are sealed under it at PUT and opened for the executor calls
	durable  dbos.Client      // the run driver's store: a start enqueues the workflow inside the request's transaction
	own      string           // the public URL's host: the one Host the app surface never proxies
	public   string           // the public URL, scheme and host only: the skill manifest's host, never the request's Host header
	orgEnv   string           // CANYON_ORG as set, "" when it was not: then org_name is the row's to set (design/ONBOARDING.md §1)
	auth     authMode         // CANYON_AUTH; the zero value is off, and only main() reads the environment for it
	kube     bool             // CANYON_RUNTIME=kubernetes: apps run on a cluster, reached through the front door; false is docker
	behind   string           // CANYON_KUBE_GATEWAY_SIGNIN's host: a sign-in behind the cluster's own gateway, which a floored app can't reach; "" unset
	proxies  proxies          // CANYON_TRUSTED_PROXIES (limiter.go)
	askToken string           // CANYON_TLS_ASK_TOKEN: the token a proxy with no trusted address asks /tls/ask with; "" accepts none
	oidc     *oidcConfig      // under oidc, the environment's lists: bootstrap and override of the row's, read beside the session on every request; nil otherwise
	idp      *provider        // under oidc, the issuer as discovered: the sign-in routes' endpoints and verifier; nil otherwise
	limit    *limiter         // set by serve: the flow routes' bound per source address
	puts     *limiter         // set by serve: every report PUT per actor, refused or not, at the per-actor gate-run budget
	signIns  *limiter         // set by serve: GET /auth/app per Canyon session, each call a row
	reports  *limiter         // set by serve: POST /releases/:id/report, bounded per host token, keyed by its hash (design/HAND-OFF.md §4)
	now      func() time.Time // the limiters' clock: time.Now when nil, a test's when set
	grants   grants           // the device flow's memory beside its table: last poll and denials
	log      *log.Logger      // set by serve: the routes log where the request pipeline does
	listen   string           // set by serve: the listener's address, one half of the fleet's banner
	publish  string           // CANYON_PUBLISHED_ADDR: the address the host publishes the listener on, the banner's alone under demo; "" when unset
	model    model            // the host's model, nil when the admin configured none: then no setup panel is offered
	reviews  bool             // the host has a model for AI review, which a custom rule needs to run — on OpenAI without the setup chat
	turns    chan struct{}    // the setup route's slots in flight, made by routesSetup: a host with no model has neither
	pings    chan struct{}    // the one test delivery in flight (POST /platform-webhook/ping), made by routesPlatformWebhook
	checks   chan struct{}    // the one check of the deploy repo in flight (POST /platform-repo/check), made by routesPlatformRepo
	settling settling         // the delivery: repo releases a check-in is settling, one at a time each, and their backoff (#2470)
	ready    func(*log.Logger) *readiness
	github   string          // CANYON_GITHUB_API: where the GitHub App's manifest code is converted
	ghWeb    string          // GitHub's web address, where a person signs in to link their account (#2228)
	pipe     *runs.Pipeline  // runs a promoted image in prod as a deploy run runs preview: the executor, the vault, the app surface
	idKey    *identity.Key   // signs the identity token an app surface hands the app under oidc
	files    accessFiles     // on Kubernetes, the access files handed the executor: one at a time, each newer
	egress   *egressReceiver // takes Envoy's access logs when CANYON_EGRESS_RECEIVER_ADDR is set; nil otherwise

	telemetry telemetryConfig // the daily report's environment (telemetry.go): the zero value sends nothing and runs no sender
	ee        eeHooks         // Canyon Enterprise's code as this server was built with it (enterprise.go): the zero value, a build without it

	dbSweep time.Duration // how often the database sessions' sweep runs (design/DATABASE-SESSIONS.md §6); 0 runs none, as a test runs its passes itself
	notify  bool          // whether the chat channels' tick and sender run (notify.go); a test runs their passes itself
	// The database relays this server carries (routes_db_relay.go), the most it carries at once
	// (CANYON_DB_MAX_CONNECTIONS; 0 is dbHostConnections), and the id it names itself by in relayed_by.
	relays     dbRelays
	dbMaxConns int
	replica    string

	discovery http.RoundTripper // carries a choice 2 front door's discovery fetch: http.DefaultTransport when nil, a test's fake when set

	translator custom.Model // the review's model, asked how a custom rule is checked when it is saved; nil with none
	modelID    string       // the id it is sent

	boot gatepolicy.Snapshot // the policy snapshot as booted: the rule list, and the base effectivePolicy lays the org's edits over
}

// refuseNewKeyOverSealedRows fails the boot that generated a key while secrets sealed under
// another one exist: that is a key volume that did not mount, not a first boot, and serving
// would answer every deploy with an unopenable value and seal new ones under the wrong key.
func refuseNewKeyOverSealedRows(ctx context.Context, pool *pgxpool.Pool, keys *vault.Vault) error {
	if !keys.Generated {
		return nil
	}
	var sealed int // the GitHub App's row, the scanner credential, the front door's client secret, the platform webhook's secret, the deploy repo's token, each release's access token, each project's report key, each identity key, the egress policy's certificate key, each value route's value, each interception root's key and each chat channel's webhook URL count as one each: each is sealed as a secret is
	if err := pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM secrets) + (SELECT count(*) FROM github_app) + (SELECT count(*) FROM scanner_credentials) + (SELECT count(key_ref) FROM front_doors) + (SELECT count(*) FROM platform_webhook) + (SELECT count(*) FROM platform_repo) + (SELECT count(key_ref) FROM releases) + (SELECT count(report_key_ref) FROM projects) + (SELECT count(*) FROM identity_keys) + (SELECT count(*) FROM egress_policy_cert) + (SELECT count(*) FROM egress_routes) + (SELECT count(*) FROM egress_ca_roots) + (SELECT count(*) FROM notify_channels)").Scan(&sealed); err != nil {
		return err
	}
	if sealed > 0 {
		return fmt.Errorf("CANYON_KEY_FILE was generated at this boot but %d row(s) are sealed under another key: the key volume is missing", sealed)
	}
	return nil
}

// refuseHandOffUnserved fails the boot of a server whose row says run_by: org while it lacks a setting
// hand-off needs of it (#2010): every release it sent would name no sidecar, or a sidecar that never starts.
func refuseHandOffUnserved(ctx context.Context, pool *pgxpool.Pool, p *runs.Pipeline) error {
	needs := handOffNeeds(p)
	if len(needs) == 0 {
		return nil
	}
	var runBy string
	if err := pool.QueryRow(ctx, "SELECT run_by FROM org_preferences").Scan(&runBy); err != nil {
		return fmt.Errorf("canyon-server: read run_by: %w", err)
	}
	if runBy != "org" {
		return nil
	}
	lines := make([]string, len(needs))
	for i, n := range needs {
		lines[i] = n.Message
	}
	return errors.New("canyon-server: your platform runs this Canyon's apps (run_by: org), and it can't yet: " + strings.Join(lines, " ") +
		" To have Canyon run them again instead, run UPDATE org_preferences SET run_by = 'canyon' on Canyon's database and restart, then save the front door again: Canyon runs no app behind a front door that has not passed its check of this cluster.")
}

// refuseRowsUnderAMissingKey fails the boot when a secrets row, or any other sealed row, names a key
// version the store did not load — a key file gone from the volume while rows still refer to it,
// or a volume that mounted only part of the set. The server never removes a key file; the
// operator may, once no row names it. Serving would fail every deploy of those rows, so the boot
// names the version and the rows under it instead. A NULL key_ref is version 1: every row written before the label.
func refuseRowsUnderAMissingKey(ctx context.Context, pool *pgxpool.Pool, keys *vault.Vault) error {
	rows, err := pool.Query(ctx, `SELECT 'secret(s)', coalesce(key_ref, 1), count(*) FROM secrets GROUP BY 2
		UNION ALL SELECT 'GitHub App', key_ref, count(*) FROM github_app GROUP BY 2
		UNION ALL SELECT 'scanner credential', key_ref, count(*) FROM scanner_credentials GROUP BY 2
		UNION ALL SELECT 'front door client secret', key_ref, count(*) FROM front_doors WHERE key_ref IS NOT NULL GROUP BY 2
		UNION ALL SELECT 'platform webhook secret', key_ref, count(*) FROM platform_webhook GROUP BY 2
		UNION ALL SELECT 'deploy repo token', key_ref, count(*) FROM platform_repo GROUP BY 2
		UNION ALL SELECT 'release access token(s)', key_ref, count(*) FROM releases WHERE key_ref IS NOT NULL GROUP BY 2
		UNION ALL SELECT 'project report key(s)', report_key_ref, count(*) FROM projects WHERE report_key_ref IS NOT NULL GROUP BY 2
		UNION ALL SELECT 'identity key(s)', key_ref, count(*) FROM identity_keys GROUP BY 2
		UNION ALL SELECT 'egress policy certificate key', key_ref, count(*) FROM egress_policy_cert GROUP BY 2
		UNION ALL SELECT 'egress route value(s)', key_ref, count(*) FROM egress_routes GROUP BY 2
		UNION ALL SELECT 'egress interception root key(s)', key_ref, count(*) FROM egress_ca_roots GROUP BY 2
		UNION ALL SELECT 'chat channel webhook URL(s)', key_ref, count(*) FROM notify_channels GROUP BY 2 ORDER BY 2, 1`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var missing []string
	for rows.Next() {
		var what string
		var version, sealed int
		if err := rows.Scan(&what, &version, &sealed); err != nil {
			return err
		}
		if !keys.Has(version) {
			missing = append(missing, fmt.Sprintf("%d %s under key version %d", sealed, what, version))
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s: no key file for that version is on the volume, and nothing sealed under it can open until it is back", strings.Join(missing, ", "))
	}
	return nil
}

// kubernetesOf reads CANYON_RUNTIME: unset or docker is false, kubernetes true, anything else fails the boot.
func kubernetesOf(s string) (bool, error) {
	switch s {
	case "", "docker":
		return false, nil
	case "kubernetes":
		return true, nil
	}
	return false, fmt.Errorf("CANYON_RUNTIME must be docker or kubernetes, not %q", s)
}

// defaultOrg is the display name a host that names none carries; the receipt's org (schema
// minLength 1) is never empty.
const defaultOrg = "Meridian"

// orgEnvOf holds CANYON_ORG to org_name's own rule (orgRule): a set value is 1 to 128 characters
// of plain text, so whitespace never reads as set and nothing over-length passes through.
func orgEnvOf(s string) (string, error) {
	if s != "" && !plainText(s, 128) {
		return "", fmt.Errorf("CANYON_ORG must be 1 to 128 characters of plain text, org_name's rule (contracts/routes.md)")
	}
	return s, nil
}

// serve answers on ln until ctx is cancelled, then shuts down gracefully. Routes are a frozen
// seam (contracts/routes.md) and register per area in routes_<area>.go files (AGENTS.md). The readiness
// prober is built here so it logs where the server logs.
func serve(ctx context.Context, ln net.Listener, logOut io.Writer, a *app) error {
	logger := log.New(logOut, "", log.LstdFlags)
	a.log = logger
	a.listen = ln.Addr().String()
	now := time.Now
	if a.now != nil {
		now = a.now
	}
	a.limit = newLimiter(now, a.proxies)
	a.signIns = &limiter{now: now, buckets: map[string]*bucket{}, burst: appSignInBurst, rate: appSignInRate}
	a.puts = &limiter{now: now, buckets: map[string]*bucket{}, burst: float64(a.boot.Limits.GateRunsPerActorPerHour), rate: float64(a.boot.Limits.GateRunsPerActorPerHour) / 3600}
	a.reports = &limiter{now: now, buckets: map[string]*bucket{}, burst: 60, rate: 1} // 60 a minute (design/HAND-OFF.md §4)
	mux := http.NewServeMux()
	routesOpen(mux, a.ready(logger), logger, web.Dist, a)
	routesTLS(mux, a)
	routesReleaseReport(mux, a)
	routesReleaseAccess(mux, a)
	api := http.NewServeMux() // everything under /api/v1 sits behind the session stage; the Open group does not
	routesProjects(api, a)
	routesPackages(api, a)
	routesGate(api, a)
	routesReports(api, a)
	routesImageReports(api, a)
	routesTimeline(api, a)
	routesAudit(api, a)
	routesSecrets(api, a)
	routesLogs(api, a)
	routesEnvs(api, a)
	routesPlatform(api, a)
	routesHold(api, a)
	routesFleet(api, a)
	routesSessions(api, a)
	routesDeviceGrants(api, a)
	routesPromote(api, a)
	routesPromotionApprovals(api, a)
	routesPatching(api, a)
	routesDesignSystems(api, a)
	routesPreferences(api, a)
	routesPolicies(api, a)
	routesAppPolicies(api, a)
	routesCustomRules(api, a)
	routesPlaybooks(api, a)
	routesScanners(api, a)
	routesIntegrations(api, a)
	routesFrontDoor(api, a)
	routesEgress(api, a)
	routesPlatformWebhook(api, a)
	routesPlatformRepo(api, a)
	routesSkill(api, a)
	routesComponents(api, a)
	routesAccess(api, a)
	routesGitHub(api, a)
	routesRepositories(api, a)
	routesDBSessions(api, a)
	routesDBQuery(api, a)
	routesDBRelay(api, a)
	routesTelemetry(api, a)
	routesLicence(api, a)
	if a.model != nil { // unrouted without one, so the catch-all answers the contract's 404 and the screen is told setup_available: false
		routesSetup(api, a)
	}
	mux.Handle("/api/v1/", http.StripPrefix("/api/v1", a.session(api)))
	srv := &http.Server{
		Handler:           pipeline(a.loopbackOnly(a.surfaces(mux, http.DefaultTransport)), logger),
		ReadHeaderTimeout: 10 * time.Second,
	}
	srv.RegisterOnShutdown(a.shutdownRelays) // a hijacked relay is no request Shutdown waits on: it ends canyon_restarting
	logger.Printf("canyon-server %s listening on %s", version, ln.Addr())
	logger.Printf("Canyon is up: open %s", a.public) // the line a person scanning `compose up` looks for (#2019)

	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	if a.dbSweep > 0 {
		go a.sweepDBSessions(ctx, logger, a.dbSweep)
		go a.sweepRelays(ctx, logger, dbRelayCheck)
	}
	if a.telemetry.every > 0 {
		go a.telemetryLoop(ctx, logger)
	}
	if a.notify {
		go a.notifyLoop(ctx, logger) // the org's chat channels, told when production needs a person
	}

	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// publicURLOf is CANYON_PUBLIC_URL as the skill manifest hands it out: http or https, a host, and
// nothing else — no user, path, query or fragment, since a skill file joins routes onto it. A
// trailing slash alone is forgiven and dropped.
func publicURLOf(s string) (*neturl.URL, error) {
	u, err := neturl.Parse(s)
	if err != nil { // never the parser's error: it quotes the value, and a value can carry a credential
		return nil, errors.New("CANYON_PUBLIC_URL does not parse as a URL: it must be http(s)://host[:port] and nothing else")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, errors.New("CANYON_PUBLIC_URL must be http(s)://host[:port] and nothing else")
	}
	u.Path = ""
	return u, nil
}

// poolSize is CANYON_DATABASE_MAX_CONNS, the most connections the server's pool opens: 20 unset, a
// whole number from 4 to 500 set, so a burst of requests never waits on pgxpool's default of four on a
// small node (#1693).
func poolSize(getenv func(string) string) (int32, error) {
	v := getenv("CANYON_DATABASE_MAX_CONNS")
	if v == "" {
		return 20, nil
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil || n < 4 || n > 500 {
		return 0, fmt.Errorf("CANYON_DATABASE_MAX_CONNS is %q: a whole number from 4 to 500", v)
	}
	return int32(n), nil
}

// patchesOn is whether this server patches (runs.Pipeline.Patches): CANYON_PATCHING, the operator's opt-in
// (beta; contracts/routes.md, Patch runs), on or off, unset off, any other value refused by name — and sign-in
// on, since with it off nobody can decide a patch approval, so patching reads off (design/PATCHING.md §4.5).
func patchesOn(getenv func(string) string, mode authMode) (bool, error) {
	switch v := getenv("CANYON_PATCHING"); v {
	case "on":
		return mode == authOIDC, nil
	case "", "off":
		return false, nil
	default:
		return false, fmt.Errorf("CANYON_PATCHING is %q: on or off", v)
	}
}

// patchInterval is CANYON_PATCH_INTERVAL, how often patch.rescan re-matches the platform layers of the
// images that run (design/PATCHING.md §3.2): unset is 0, runs.DefaultRescan; set, a Go duration that
// divides a minute, an hour or a day evenly, so every replica's ticks fall on the same instants.
func patchInterval(getenv func(string) string) (time.Duration, error) {
	v := getenv("CANYON_PATCH_INTERVAL")
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err == nil {
		_, err = runs.RescanSchedule(d)
	}
	if err != nil {
		return 0, fmt.Errorf("CANYON_PATCH_INTERVAL is %q: a duration that divides a minute, an hour or a day evenly, as 6h or 30m (%v)", v, err)
	}
	return d, nil
}

// patchWatch is CANYON_PATCH_WATCH, how long a patched preview is watched for an exit or a restart before the
// patch goes further (design/PATCHING.md §4.4): unset is 0, runs.DefaultPatchWatch; set, a Go duration of at
// least a minute — the watch reads the container every 30 s — and at most a day.
func patchWatch(getenv func(string) string) (time.Duration, error) {
	v := getenv("CANYON_PATCH_WATCH")
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < time.Minute || d > 24*time.Hour {
		return 0, fmt.Errorf("CANYON_PATCH_WATCH is %q: a duration from 1m to 24h, as 15m", v)
	}
	return d, nil
}

// capacities is CANYON_SCAN_CONCURRENCY, CANYON_BUILD_CONCURRENCY and CANYON_PATCH_CONCURRENCY, the scans,
// builds and patch runs the host runs at once (contracts/finding.md, Canyon-run tools; design/PATCHING.md
// §4.2): unset is 0, the queues' Docker defaults; anything but a whole number from 1 to 100 is refused by
// name.
func capacities(getenv func(string) string) (scans, builds, patches int, err error) {
	read := func(name string, least int) (int, error) {
		v := getenv(name)
		if v == "" {
			return 0, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < least || n > 100 {
			return 0, fmt.Errorf("%s is %q: a whole number from %d to 100", name, v, least)
		}
		return n, nil
	}
	if scans, err = read("CANYON_SCAN_CONCURRENCY", 2); err != nil { // one of them is grype's (runs.GrypeQueue)
		return 0, 0, 0, err
	}
	if builds, err = read("CANYON_BUILD_CONCURRENCY", 1); err != nil {
		return 0, 0, 0, err
	}
	patches, err = read("CANYON_PATCH_CONCURRENCY", 1)
	return scans, builds, patches, err
}
