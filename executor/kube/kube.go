// Package kube is the executor of contracts/executor.md on Kubernetes (On Kubernetes): the calls the
// Docker executor serves, answered with Deployments in canyon-apps.
package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/identity"
	"github.com/getcanyon-ai/canyon/internal/names"
)

// Namespace holds every app's objects.
const Namespace = "canyon-apps"

// jwksURL is the key set canyon-identity serves in the app's pod, the app's CANYON_IDENTITY_JWKS_URL.
const jwksURL = "http://127.0.0.1:4182/.well-known/jwks.json"

// Executor runs each app and target as a Deployment in Namespace with its Service, NetworkPolicy,
// access ConfigMap and Secrets. Zero Poll and Deadline mean 2 s and 300 s.
type Executor struct {
	Client   kubernetes.Interface
	Registry string              // CANYON_KUBE_REGISTRY, the host every image lives under
	DBCIDRs  []netip.Prefix      // CANYON_KUBE_DB_CIDRS, where the apps database listens on 5433
	Identity string              // CANYON_IMAGE_IDENTITY, canyon-identity's image, by digest
	Dynamic  dynamic.Interface   // kpack's Builds and ClusterBuilder
	Auth     authn.Authenticator // the registry's; nil is anonymous; NewECRAuth on ECR
	ECR      ECR                 // the registry's API when it is ECR; nil for another registry
	Opengrep string              // CANYON_IMAGE_OPENGREP, Canyon's own opengrep image, by digest; "" answers no_image
	ESLint   string              // CANYON_IMAGE_ESLINT, Canyon's own ESLint image, by digest; "" answers no_image
	// OAuth2Proxy, Busybox and BusyboxStatic are CANYON_IMAGE_OAUTH2_PROXY, CANYON_IMAGE_BUSYBOX and
	// CANYON_IMAGE_BUSYBOX_STATIC, each by digest; "" runs the release's pin (oauth2Proxy, probeImage,
	// scanBusybox).
	OAuth2Proxy, Busybox, BusyboxStatic string
	// AppCPU and AppMemory are CANYON_KUBE_APP_CPU_REQUEST and CANYON_KUBE_APP_MEMORY_REQUEST, what each app
	// container requests; "" is 10m and 256Mi, measured on the AWS test cluster (contracts/executor.md; #1637).
	AppCPU, AppMemory string
	ScanDBCIDRs       []netip.Prefix // CANYON_KUBE_SCANDB_CIDRS: where a refresh Job reaches every TCP port
	SigninDirect      []Direct       // CANYON_KUBE_SIGNIN_DIRECT: the sign-in hosts a floored pod's sidecars reach past the gateway
	// Exec runs command in a container of a pod with stdin, over pods/exec: how a scan's feed reaches it.
	Exec  func(ctx context.Context, namespace, pod, container string, command []string, stdin io.Reader) error
	Trivy http.RoundTripper // carries the GET /version that says how old trivy's server's database is; nil is http.DefaultTransport
	Ask   http.RoundTripper // carries the ask of a pod's /answered (contracts/executor.md); nil is answered.Direct
	// Gateway opens TCP to Canyon's egress gateway at a /run's egress.proxy, host:3128, from the executor's
	// own pod (contracts/executor.md, On Kubernetes); nil dials it, 2 s at most.
	Gateway func(ctx context.Context, hostport string) error
	// Arch is the CPU architecture a /run's image is made for, which its pods are pinned to (contracts/executor.md,
	// On Kubernetes; #2440); nil reads it from the image's config in the registry (imageArch).
	Arch func(ctx context.Context, image string) (string, error)
	Poll time.Duration // how often a rollout is looked at
	// BuildDeadline is how long a /build may take, 29 minutes unset: under the server's 30, so a build the
	// cluster had no room for is answered while the server still listens.
	BuildDeadline time.Duration
	Deadline      time.Duration // how long a new pod has to be available, the Deployment's progress deadline
	goneWait      time.Duration // a test's bound on the wait for the previous template's pods; zero is the contract's
	Roots         string        // the system roots a /run's egress.ca joins in system-and-canyon.pem; "" = executor.SystemRoots
	probe         probeHooks
	watch         *watcher // the Builds /build waits on, once WatchBuilds has started

	collecting sync.WaitGroup // what answered calls left running: /run's image cleanups, the database refresh a /scan started
}

// frontDoor is /run's front_door: how people reach the app (contracts/executor.md, The front door).
type frontDoor struct {
	Choice   int     `json:"choice"`
	Gateway  string  `json:"gateway,omitempty"`
	Issuer   string  `json:"issuer,omitempty"`
	Audience string  `json:"audience,omitempty"`
	JWKSURL  string  `json:"jwks_url,omitempty"`
	ALBARN   string  `json:"alb_arn,omitempty"`
	Cookie   string  `json:"cookie,omitempty"`
	Signin   *signin `json:"signin,omitempty"`
	Domain   string  `json:"domain"`
	Form     string  `json:"form"`
}

// signin is how oauth2-proxy signs people in for choice 2.
type signin struct {
	Issuer       string `json:"issuer"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret,omitempty"`
}

// access is the app's access file as contracts/schema/executor.schema.json spells it; decoding into it
// is the check, and the ConfigMap holds the bytes /run was handed.
type access struct {
	UpdatedAt     string `json:"updated_at"`
	GroupsClaim   string `json:"groups_claim"`
	SubjectIssuer string `json:"subject_issuer"`
	Members       struct {
		Domains []string `json:"domains"`
		Emails  []string `json:"emails"`
	} `json:"members"`
	Admins struct {
		Emails []string `json:"emails"`
		Claim  string   `json:"claim"`
		Value  string   `json:"value"`
	} `json:"admins"`
	Roles   map[string]string `json:"roles"`
	Entries []identity.Grant  `json:"entries"`
}

// updatedAt is access.json's updated_at, whole seconds UTC: two of them order as strings.
const updatedAt = "2006-01-02T15:04:05Z"

var (
	envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	digest  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	domain  = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
	https   = regexp.MustCompile(`^https://[^@/?#\s]+(/\S*)?$`)
	token   = regexp.MustCompile(`^[\x21-\x7e]+$`) // at most 1024: validToken
	cookie  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	albARN  = regexp.MustCompile(`^arn:aws:elasticloadbalancing:[a-z]{2}-[a-z]+-[0-9]:[0-9]{12}:loadbalancer/app/[A-Za-z0-9-]{1,32}/[0-9a-f]{16}$`)
	// databaseCA is executor.schema.json's run_request database_ca pattern, verbatim; at most 256 KiB.
	databaseCA = regexp.MustCompile(`^(-----BEGIN CERTIFICATE-----\n([A-Za-z0-9+/=]{1,64}\n)+-----END CERTIFICATE-----\n)+$`)
)

// validApp is the seam's project id, at most 55 characters, so <app>-preview names a Service (63).
func validApp(app string) bool { return len(app) <= 55 && names.ValidProject(app) }

func validTarget(target string) bool { return target == "preview" || target == "prod" }

func validToken(s string) bool { return len(s) <= 1024 && token.MatchString(s) }

func validHTTPS(s string) bool { return len(s) <= 2048 && https.MatchString(s) }

func refuse(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{executor.ErrRequest}, a...)...)
}

// check refuses a /run the contract refuses, and answers its front door, the front door as
// canyon-identity reads it (CANYON_FRONT_DOOR: no client secret), and its access file.
func (e *Executor) check(req executor.RunRequest) (frontDoor, []byte, access, error) {
	var door frontDoor
	var file access
	switch {
	case !validApp(req.App):
		return door, nil, file, refuse("app must be a project id of at most 55 characters")
	case !validTarget(req.Target):
		return door, nil, file, refuse("target must be preview or prod")
	case req.Network != "canyon-app-"+req.App:
		return door, nil, file, refuse("network must be canyon-app-%s", req.App)
	case req.Identity.JWKSURL != jwksURL:
		return door, nil, file, refuse("identity.jwks_url must be %s, the key set canyon-identity serves in the pod", jwksURL)
	case req.Access == nil || req.FrontDoor == nil:
		return door, nil, file, refuse("access and front_door are required on Kubernetes")
	case req.Run != "" && !executor.ValidRun(req.Run):
		return door, nil, file, refuse("run is the server's run id, gr_ and [a-z0-9]")
	case req.DatabaseCA != nil && (len(*req.DatabaseCA) > 256<<10 || !databaseCA.MatchString(*req.DatabaseCA)):
		return door, nil, file, refuse("database_ca must be PEM certificates and nothing else, at most 256 KiB")
	}
	repo, sum, ok := strings.Cut(req.Image, "@sha256:")
	if !ok || repo != e.Registry+"/canyon/"+req.App || !digest.MatchString(sum) {
		return door, nil, file, refuse("image must be %s/canyon/%s@sha256:<64 hex>, never a tag", e.Registry, req.App)
	}
	for k, v := range req.Env {
		if !envName.MatchString(k) || k == "PORT" || strings.HasPrefix(k, "CANYON_IDENTITY_") || strings.IndexByte(v, 0) >= 0 || len(v) > 64<<10 {
			return door, nil, file, refuse("env %q: a name [A-Za-z_][A-Za-z0-9_]*, not PORT or CANYON_IDENTITY_*, a value without NUL of at most 64 KiB", k)
		}
	}
	if _, _, err := proxied(req, nil); err != nil {
		return door, nil, file, err
	}
	file, err := parseAccess(req.Access)
	if err != nil {
		return door, nil, file, err
	}
	if err := strict(req.FrontDoor, &door); err != nil {
		return door, nil, file, refuse("front_door: %v", err)
	}
	if err := door.check(); err != nil {
		return door, nil, file, err
	}
	sidecar := door
	if door.Signin != nil {
		s := *door.Signin
		s.ClientSecret = "" // oauth2-proxy alone reads it, from the signin Secret
		sidecar.Signin = &s
	}
	doorJSON, err := json.Marshal(sidecar)
	return door, doorJSON, file, err
}

// check is the schema's front_door: choice 1 through an oidc or an alb gateway, or choice 2 with
// signin, each with its own fields and no other's.
func (d frontDoor) check() error {
	if err := checkAddress(d.Domain, d.Form); err != nil {
		return err
	}
	s := d.Signin
	oidc := d.Choice == 1 && d.Gateway == "oidc" && s == nil && d.ALBARN == "" &&
		validHTTPS(d.Issuer) && validToken(d.Audience) && validHTTPS(d.JWKSURL) && cookie.MatchString(d.Cookie)
	alb := d.Choice == 1 && d.Gateway == "alb" && s == nil && d.Issuer == "" && d.Audience == "" && d.JWKSURL == "" &&
		albARN.MatchString(d.ALBARN) && (d.Cookie == "" || cookie.MatchString(d.Cookie))
	two := d.Choice == 2 && d.Gateway == "" && d.Cookie == "" && d.Issuer == "" && d.Audience == "" && d.JWKSURL == "" && d.ALBARN == "" &&
		s != nil && validHTTPS(s.Issuer) && validToken(s.ClientID) && validToken(s.ClientSecret)
	if oidc || alb || two {
		return nil
	}
	return refuse("front_door is choice 1 with gateway oidc (issuer, audience, jwks_url, cookie) or alb (alb_arn, cookie?), or choice 2 with signin (issuer, client_id, client_secret), and no other field")
}

// checkAddress is front_door's domain and form, as /run and /probe take them.
func checkAddress(d, form string) error {
	switch {
	case len(d) > 189 || !domain.MatchString(d) || strings.HasPrefix(d, "preview.") || strings.HasPrefix(d, "prod."):
		return refuse("front_door.domain must be a lower-case DNS name whose first label is neither preview nor prod")
	case form != "app.env.domain" && form != "app-env.domain":
		return refuse("front_door.form must be app.env.domain or app-env.domain")
	}
	return nil
}

// strict decodes raw, refusing a field the type does not name.
func strict(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v) // raw is one value: the request's decoder cut it out
}

// address is the app's host under the front door's domain and form.
func (d frontDoor) address(app, target string) string {
	if d.Form == "app.env.domain" {
		return app + "." + target + "." + d.Domain
	}
	return app + "-" + target + "." + d.Domain
}
