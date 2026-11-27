package kube

import (
	"archive/tar"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/getcanyon-ai/canyon/executor"
)

// ScanNamespace holds the scan Jobs, labelled canyon.scan, under a NetworkPolicy that lets nothing in
// or out but what trivy's clients and server reach (trivy.go).
const ScanNamespace = "canyon-scans"

// scanning is the lock of each tool with a database volume, its scans and its database's refreshes one at
// a time — a volume attaches to one node at a time. A second scan answers queued, and a scan while a
// refresh waits for the lock or holds it answers busy: neither waits inside its call, where it would
// hold one of the scans' work slots (#1643). A tool with no volume takes no lock.
var scanning = map[string]*toolLock{"grype": newToolLock(), executor.JavaDB: newToolLock()} // trivy has a server instead (trivy.go); its Java database, which image scans alone read, a volume as grype's

type toolLock struct {
	mu        sync.Mutex
	changed   *sync.Cond // on mu
	held      bool
	refreshes int // waiting or holding
	// refreshing is a refresh a scan started, still running; failed, that the last one failed, which the
	// next scan that finds no database answers (db_missing).
	refreshing, failed atomic.Bool
	// read is when the database was built as the executor last read it, after each refresh and at each scan's
	// check: GET /scandb answers it, so the read never waits on the lock (#2743).
	read atomic.Pointer[time.Time]
}

func newToolLock() *toolLock {
	l := &toolLock{}
	l.changed = sync.NewCond(&l.mu)
	return l
}

// scan takes the lock for a scan, or answers why not, nothing taken: busy while a refresh waits for it or
// holds it, queued while another scan holds it.
func (l *toolLock) scan() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.refreshes > 0:
		return executor.ErrBusy
	case l.held:
		return executor.ErrQueued
	}
	l.held = true
	return nil
}

// refresh takes the lock for a refresh, turning every scan away from the moment it starts waiting.
func (l *toolLock) refresh() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refreshes++
	l.changed.Broadcast()
	for l.held {
		l.changed.Wait()
	}
	l.held = true
}

// release gives the lock back, refresh saying whether a refresh held it.
func (l *toolLock) release(refresh bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held = false; refresh {
		l.refreshes--
	}
	l.changed.Broadcast()
}

// errTimeout is a database reader past the tool's deadline: the scan answers timeout.
var errTimeout = errors.New("past the tool's deadline")

// fed is the file the exec'd tar leaves once /scan holds everything: the feed container ends, and the
// tool starts.
const fed = "/scan/.fed"

// scanBusybox is busybox 1.37.0's musl build, statically linked on amd64 and arm64 (the probe's is
// not), so the copy the feed leaves at /scan/busybox runs in every tool's image, grype's distroless one
// among them.
const scanBusybox = "docker.io/library/busybox@sha256:5cec3fc171c87218698e85a52af7087de727372aae264a787b8112901a5b0092"

// busyboxRoom is what /scan holds beside an image an image scan reads, and as much again for trivy's
// copies of the files it reads (TMPDIR=/scan/tmp): the busybox the feed leaves there.
const busyboxRoom = 64 << 20

// scanDisk bounds /scan on the node's disk: the tree (maxTree in at most maxEntries entries, each at
// most one more 4 KiB block: 1.8 GiB), opengrep's rules (a 50 MiB tar, so at most 102,400 entries: 0.45
// GiB), the config and the busybox come to 2.3 GiB. Past it the kubelet evicts this pod, not a neighbour.
const scanDisk = "3Gi"

// Scan runs one of the four tools over one package as a Job in canyon-scans (contracts/executor.md, On
// Kubernetes, /scan): the pod is the Docker executor's container as Kubernetes spells it, its init
// container waits for the tree, the rules and the config the executor streams over pods/exec, then the
// tool runs, its stdout read from its log. A tool that reads a vulnerability database runs only on one
// built at most executor.DBMaxAge before the scan starts, mounted read-only at /scan/db, and when it was
// built is on its SARIF answer: else db_stale or, with none, busy while it is downloaded, as on Docker.
// Trivy's image target is fed the app's image from the registry, on its Java database; its sbom target
// the part sbom, as a client of trivy's server.
func (e *Executor) Scan(ctx context.Context, req executor.ScanRequest, parts *multipart.Reader) (executor.ScanResult, error) {
	if err := executor.CheckScan(req); err != nil {
		return executor.ScanResult{}, err
	}
	toolName, tool, ok := executor.ToolFor(req)
	image := tool.Image
	switch req.Tool { // Canyon's own images, which the operator builds
	case "opengrep":
		image = e.Opengrep
	case "eslint":
		image = e.ESLint
	}
	if !ok || image == "" {
		return executor.ScanResult{Tool: req.Tool, NotFinished: "no_image"}, nil
	}
	// feed writes what the feed unpacks into /scan; checked checks the request's parts when no feed runs: a
	// request the contract refuses is 400 whatever the database.
	feed := func(w io.Writer) error { return feedTar(w, req, tool, parts) }
	checked := func() error { return feed(io.Discard) }
	var app *imageTarget // an image target's
	var sbom []byte      // an sbom target's
	var ref name.Reference
	reason, disk := "", resource.MustParse(scanDisk)
	switch req.Target {
	case "sbom": // read, and checked, before any Job: trivy never sees a part it would read in part
		var err error
		sbom, err = executor.SBOMPart(parts)
		if err != nil {
			return executor.ScanResult{}, err
		}
		feed = func(w io.Writer) error { return sbomTar(w, sbom) }
	case "image":
		reason, checked = executor.JavaReason, func() error { return nil } // checked here: its image is read only once the lock is held
		if _, err := parts.NextPart(); !errors.Is(err, io.EOF) {
			return executor.ScanResult{}, fmt.Errorf("%w: an image target takes part json and no other", executor.ErrRequest)
		}
		var err error
		if ref, err = e.imageRef(ctx, req); err != nil {
			return executor.ScanResult{}, err
		}
	}
	if lock := scanning[toolName]; lock != nil {
		if err := lock.scan(); err != nil {
			return executor.ScanResult{}, err
		}
		defer lock.release(false)
	}
	if err := e.policy(ctx, "canyon-scan", "canyon.scan"); err != nil {
		return executor.ScanResult{}, err
	}
	started := time.Now()
	ends := started.Add(tool.Deadline)                              // the database's reader runs within the tool's deadline
	ctx, cancel := context.WithDeadline(ctx, ends.Add(time.Minute)) // the Jobs' own deadlines end them first
	defer cancel()
	res := executor.ScanResult{Tool: req.Tool, Image: image}
	if ref != nil {
		var err error
		switch app, err = e.imageTarget(ctx, ref); {
		case errors.Is(err, errImageOver):
			res.NotFinished, res.Reason = "over_cap", "image"
			return res, nil
		case err != nil:
			return executor.ScanResult{}, err
		}
		feed, disk = app.feed, *resource.NewQuantity(2*app.size+busyboxRoom, resource.BinarySI) // the image, and trivy's copies of the files it reads under TMPDIR
	}
	var built time.Time
	var server *corev1.Pod
	if toolName == "trivy" { // a client of trivy's server: its database is the server's
		var err error
		server, err = e.trivyServer(ctx, image)
		if err == nil {
			built, err = e.serverBuilt(ctx)
		}
		switch {
		case errors.Is(err, executor.ErrBusy):
		case errors.Is(err, errNoServer):
			res.NotFinished = "db_missing"
		case err != nil:
			return executor.ScanResult{}, err
		case started.Sub(built) > executor.DBMaxAge:
			res.NotFinished = "db_stale"
		}
		if err != nil || res.NotFinished != "" { // the parts are still checked: a request the contract refuses is 400 whatever the database
			if ferr := checked(); ferr != nil {
				return executor.ScanResult{}, ferr
			}
			if errors.Is(err, executor.ErrBusy) {
				return executor.ScanResult{}, err
			}
			return res, nil
		}
		tool.Args = clientArgs(tool.Args)
	} else if tool.DB != nil {
		var err error
		if built, err = e.dbBuilt(ctx, toolName, image, tool); err == nil {
			scanning[toolName].read.Store(&built) // what GET /scandb answers (#2743)
		}
		switch {
		case errors.Is(err, errTimeout):
			res.NotFinished = "timeout"
		case err != nil:
			return executor.ScanResult{}, err
		case built.IsZero():
			res.NotFinished = "db_missing"
		case started.Sub(built) > executor.DBMaxAge:
			res.NotFinished = "db_stale"
		case time.Until(ends) < time.Second: // what the reader left runs no tool
			res.NotFinished = "timeout"
		}
		if res.NotFinished != "" { // the parts are still checked: a request the contract refuses is 400 whatever the database
			if err := checked(); err != nil {
				return executor.ScanResult{}, err
			}
			if res.NotFinished == "db_missing" && !scanning[toolName].failed.Swap(false) { // the tool's first scan: its database is downloaded, and the scan asked again
				e.startRefresh(ctx, toolName, tool)
				return executor.ScanResult{}, executor.ErrBusy
			}
			if res.NotFinished == "db_missing" || res.NotFinished == "db_stale" {
				res.Reason = reason
			}
			return res, nil
		}
		tool.Deadline = time.Until(ends)
	}
	run := nonce()
	jobs := e.Client.BatchV1().Jobs(ScanNamespace)
	job := e.scanJob(toolName, run, image, tool, disk)
	if _, err := jobs.Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return executor.ScanResult{}, fmt.Errorf("job %s: %w", job.Name, err)
	}
	defer e.dropJob(context.WithoutCancel(ctx), job.Name)
	pod, unscheduled, err := e.feedOf(ctx, run, job.Name)
	switch {
	case err != nil:
		return executor.ScanResult{}, err
	case pod == nil && unscheduled != "": // the cluster had no room for it: capacity, never Canyon failing (#1637)
		res.NotFinished, res.Reason = "no_room", unscheduled
		return res, nil
	case pod == nil:
		res.NotFinished = "timeout"
		return res, nil
	}
	pr, pw := io.Pipe()
	fedErr := make(chan error, 1)
	go func() { err := feed(pw); _ = pw.CloseWithError(err); fedErr <- err }()
	err = e.Exec(ctx, ScanNamespace, pod.Name, "feed", []string{"sh", "-c", "tar -x -f - -C /scan && cp /bin/busybox /scan/busybox && touch " + fed}, pr)
	_ = pr.Close() // ends the feed's writes, if the exec ended first
	if feedErr := <-fedErr; errors.Is(feedErr, executor.ErrRequest) {
		return executor.ScanResult{}, feedErr
	}
	if err != nil {
		return executor.ScanResult{}, fmt.Errorf("feeding %s: %w", pod.Name, err)
	}
	following, stop := context.WithCancel(ctx)
	defer stop()
	logs := make(chan []byte, 1)
	var logErr error
	go func() { out, err := e.follow(following, run, tool.JSONOut); logErr = err; logs <- out }()
	p, err := e.scanEnd(ctx, job.Name, "canyon.scan=true,canyon.scan-run="+run)
	if err != nil {
		return executor.ScanResult{}, err
	}
	if p == nil { // the Job's deadline, or this call's
		res.NotFinished = "timeout"
		return res, nil
	}
	out := <-logs
	err = logErr
	stdout, unloaded, unparsed, whole := executor.SplitJSON(out, tool.JSONOut)
	switch sarif, ok := executor.SARIF(stdout, tool.URIRoots); {
	case errors.Is(err, executor.ErrOverCap) && (req.Target == "image" || req.Target == "sbom" || req.Target == "packages"):
		res.NotFinished, res.Reason = "over_cap", "report"
	case errors.Is(err, executor.ErrOverCap):
		res.NotFinished = "over_cap"
	case err != nil:
		return executor.ScanResult{}, err
	case req.Target == "packages":
		switch doc, ok := executor.TargetDoc(req.Target, stdout); {
		case ok:
			res.Packages = doc
		case killed(state(p, "tool")):
			res.NotFinished = "killed"
		default:
			res.NotFinished = "not_sarif"
		}
	case req.Target == "image" || req.Target == "sbom":
		switch doc, ok := executor.TargetDoc(req.Target, stdout); {
		case ok && req.Target == "image":
			res.SBOM, res.Lifecycle, res.DiffIDs = doc, app.lifecycle, app.diffIDs
			if image, isDigest := ref.(name.Digest); isDigest && req.App != "" { // an app's image: its SBOM published beside it, for the org's own tools
				referrer, perr := e.pushReferrer(ctx, image, doc)
				if perr != nil { // never the scan's failure: the SBOM is answered all the same
					log.Printf("executor: the SBOM referrer of %s: %v", image, perr)
				}
				res.Referrer = referrer
			}
		case ok:
			if err := executor.Considered(sbom, doc); err != nil { // a package trivy skipped was never matched: no clean answer
				log.Printf("executor: sbom scan: %v", err)
				res.NotFinished, res.Reason = "not_sarif", "unread"
				break
			}
			res.Report, res.DBBuiltAt, res.Unscanned = doc, built.UTC().Format(time.RFC3339), executor.Unscanned(sbom)
		case killed(state(p, "tool")):
			res.NotFinished = "killed"
		default:
			res.NotFinished = "not_sarif"
		}
	case ok && tool.Exits != nil && !slices.Contains(tool.Exits, exitOf(state(p, "tool"))) && killed(state(p, "tool")): // an engine the OOM killer ended makes Opengrep itself exit 2
		res.NotFinished = "killed"
	case ok && tool.Exits != nil && !executor.Scanned(tool.Exits, exitOf(state(p, "tool")), sarif): // its SARIF is not the answer (contracts/executor.md)
		res.NotFinished = "rules_error"
	case ok && !whole: // without its JSON output no file it did not read whole could be named: never a clean scan
		res.NotFinished = "not_sarif"
	case ok:
		res.SARIF, res.Unloaded, res.Unparsed = sarif, unloaded, unparsed
		if tool.DB != nil {
			res.DBBuiltAt = built.UTC().Format(time.RFC3339)
		}
	case killed(state(p, "tool")):
		res.NotFinished = "killed"
	default:
		res.NotFinished = "not_sarif"
	}
	if toolName == "trivy" && res.NotFinished == "not_sarif" { // a server that restarted or went under the scan: a wait, never a scan that did not finish
		if now, err := e.trivyServer(ctx, image); err != nil || now.UID != server.UID || restarts(now) != restarts(server) {
			return executor.ScanResult{}, executor.ErrBusy
		}
	}
	return res, nil
}

// clientArgs are trivy's scan arguments as a client of its server: the server holds the database, the
// client keeps its own scan cache in /tmp.
func clientArgs(args []string) []string {
	out := slices.Clone(args)
	if i := slices.Index(out, "--cache-dir"); i >= 0 && i+1 < len(out) {
		out[i+1] = "/tmp/trivy"
	}
	return append(out[:len(out)-1:len(out)-1], "--server", trivyURL, out[len(out)-1]) // the tree stays the last argument
}

// follow is what the executor keeps of the tool container's log, its stdout, as it streams
// (executor.Kept), followed from the container's start: the kubelet rotates a running container's log
// past 10 Mi, and a read after the end would find only the newest file.
func (e *Executor) follow(ctx context.Context, run string, jsonOut bool) ([]byte, error) {
	p, err := e.scanPod(ctx, run, func(p corev1.Pod) bool {
		s := state(&p, "tool")
		return s.Running != nil || s.Terminated != nil || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
	})
	if err != nil {
		return nil, err
	}
	stream, err := e.Client.CoreV1().Pods(ScanNamespace).GetLogs(p.Name, &corev1.PodLogOptions{Container: "tool", Follow: true}).Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("the log of %s: %w", p.Name, err)
	}
	defer func() { _ = stream.Close() }()
	return executor.Kept(stream, jsonOut)
}

// logOf is a container's log, to max bytes and one more, so that a longer one reads as over.
func (e *Executor) logOf(ctx context.Context, pod, container string, max int64) ([]byte, error) {
	stream, err := e.Client.CoreV1().Pods(ScanNamespace).GetLogs(pod, &corev1.PodLogOptions{Container: container}).Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("the log of %s: %w", pod, err)
	}
	defer func() { _ = stream.Close() }()
	out, err := io.ReadAll(io.LimitReader(stream, max+1))
	if err != nil {
		return nil, fmt.Errorf("the log of %s: %w", pod, err)
	}
	return out, nil
}

// dbBuilt answers when the tool's database was built: the value under BuiltAt in the JSON its reader
// prints — a Job like a scan's, labelled canyon.scan and so reaching nothing, the volume read-only at /db
// and the reader's stderr discarded. Zero, as on Docker, when there is no volume yet or it was never
// bound (no refresh has run on it; a reader would wait for it to its deadline), the reader exits 1 (no
// database the scan's user can read) or what it prints does not parse; errTimeout past the tool's deadline.
func (e *Executor) dbBuilt(ctx context.Context, tool, image string, t executor.Tool) (time.Time, error) {
	switch claim, err := e.Client.CoreV1().PersistentVolumeClaims(ScanNamespace).Get(ctx, "canyon-scandb-"+tool, metav1.GetOptions{}); {
	case apierrors.IsNotFound(err) || err == nil && claim.Status.Phase != corev1.ClaimBound:
		return time.Time{}, nil
	case err != nil:
		return time.Time{}, fmt.Errorf("volume canyon-scandb-%s: %w", tool, err)
	}
	run := nonce()
	job := e.readerJob(tool, run, image, t)
	if _, err := e.Client.BatchV1().Jobs(ScanNamespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return time.Time{}, fmt.Errorf("job %s: %w", job.Name, err)
	}
	defer e.dropJob(context.WithoutCancel(ctx), job.Name)
	p, err := e.scanEnd(ctx, job.Name, "canyon.scan=true,canyon.scan-run="+run)
	if err != nil || p == nil {
		return time.Time{}, cmp.Or(err, errTimeout)
	}
	switch s := state(p, "reader").Terminated; {
	case s != nil && s.ExitCode == 1:
		return time.Time{}, nil
	case s == nil || s.ExitCode != 0:
		return time.Time{}, fmt.Errorf("%s's database reader %s: %+v", tool, p.Name, s)
	}
	out, err := e.logOf(ctx, p.Name, "reader", 1<<20)
	if err != nil {
		return time.Time{}, err
	}
	var meta map[string]json.RawMessage
	var built time.Time
	if json.Unmarshal(out, &meta) != nil || json.Unmarshal(meta[t.DB.BuiltAt], &built) != nil {
		return time.Time{}, nil
	}
	return built, nil
}

// startRefresh starts the tool's refresh for a scan that found no database, unless the one a scan
// started still runs. It outlives the scan, which answers busy; whether it failed is the next scan's.
func (e *Executor) startRefresh(ctx context.Context, name string, t executor.Tool) {
	lock := scanning[name]
	if !lock.refreshing.CompareAndSwap(false, true) {
		return
	}
	e.collecting.Go(func() {
		err := e.refreshDB(context.WithoutCancel(ctx), name, t)
		if err != nil {
			log.Printf("executor: refresh %s database: %v", name, err)
		}
		lock.failed.Store(err != nil)
		lock.refreshing.Store(false)
	})
}

// reread keeps when the tool's database was built, read by its reader Job; a read that fails keeps what was read
// before.
func (e *Executor) reread(ctx context.Context, name string, t executor.Tool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), executor.DBDeadline)
	defer cancel()
	built, err := e.dbBuilt(ctx, name, t.Image, t)
	if err != nil {
		log.Printf("executor: read %s database's age: %v", name, err)
		return
	}
	scanning[name].read.Store(&built)
}

// ScanDBs is each vulnerability database's age (contracts/executor.md, GET /scandb; #2743), in tool order: trivy's
// server's as its GET /version answers it now, none while it has no ready pod; the claims' as the executor last
// read them, none for one it has not read.
func (e *Executor) ScanDBs(ctx context.Context) ([]executor.ScanDB, error) {
	trivy, err := e.serverBuilt(ctx)
	var dns *net.DNSError
	if err != nil && !errors.Is(err, syscall.ECONNREFUSED) && (!errors.As(err, &dns) || !dns.IsNotFound) { // a server not there yet, or not ready, is none
		log.Printf("executor: %v", err)
	}
	dbs := []executor.ScanDB{executor.ScanDBOf("trivy", trivy)}
	for name, lock := range scanning {
		var built time.Time
		if at := lock.read.Load(); at != nil {
			built = *at
		}
		dbs = append(dbs, executor.ScanDBOf(name, built))
	}
	slices.SortFunc(dbs, func(a, b executor.ScanDB) int { return strings.Compare(a.Tool, b.Tool) })
	return dbs, nil
}

// RefreshDBs refreshes the database of each tool whose volume exists — one a scan asked for — now, then
// every executor.DBEvery, one refresh at a time, until ctx is done: the executor's own schedule. A
// refresh that fails is logged.
func (e *Executor) RefreshDBs(ctx context.Context) {
	tick := time.NewTicker(executor.DBEvery)
	defer tick.Stop()
	for {
		for name := range scanning {
			t, _ := executor.ToolOf(name)
			if t.DB == nil {
				continue
			}
			switch _, err := e.Client.CoreV1().PersistentVolumeClaims(ScanNamespace).Get(ctx, "canyon-scandb-"+name, metav1.GetOptions{}); {
			case apierrors.IsNotFound(err):
			case err != nil:
				log.Printf("executor: volume canyon-scandb-%s: %v", name, err)
			default:
				if err := e.refreshDB(ctx, name, t); err != nil {
					log.Printf("executor: refresh %s database: %v", name, err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// refreshDB downloads the tool's database into the volume canyon-scandb-<tool>, a 5 Gi ReadWriteOnce
// claim of the cluster's default class made when missing, from a Job labelled canyon.scandb whose pods
// reach DNS and TCP 443 at public addresses, and ScanDBCIDRs. It holds the tool's lock alone: no scan reads a
// database half-written.
func (e *Executor) refreshDB(ctx context.Context, name string, t executor.Tool) error {
	lock := scanning[name]
	lock.refresh()
	defer lock.release(true)
	defer e.reread(ctx, name, t)                                             // under the lock, whatever the refresh did: the age is the database's own (#2743)
	ctx, cancel := context.WithTimeout(ctx, executor.DBDeadline+time.Minute) // the Job's own deadline ends it first
	defer cancel()
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "canyon-scandb-" + name, Namespace: ScanNamespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("5Gi")}},
		},
	}
	if _, err := e.Client.CoreV1().PersistentVolumeClaims(ScanNamespace).Create(ctx, claim, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("volume %s: %w", claim.Name, err)
	}
	if err := e.policy(ctx, "canyon-scandb", "canyon.scandb", e.refreshEgress()...); err != nil {
		return err
	}
	run := nonce()
	job := e.refreshJob(name, run, t)
	if _, err := e.Client.BatchV1().Jobs(ScanNamespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("job %s: %w", job.Name, err)
	}
	defer e.dropJob(context.WithoutCancel(ctx), job.Name)
	switch p, err := e.scanEnd(ctx, job.Name, "canyon.scandb=true,canyon.scan-run="+run); {
	case err != nil:
		return err
	case p == nil:
		return fmt.Errorf("job %s: past its deadline", job.Name)
	case p.Status.Phase != corev1.PodSucceeded: // with what the tool said, as on Docker: a source it could not reach is named there
		ctr := "refresh"
		if s := state(p, "sweep").Terminated; s != nil && s.ExitCode != 0 { // grype never ran
			ctr = "sweep"
		}
		said, err := e.logOf(ctx, p.Name, ctr, 8<<10)
		if err != nil {
			said = []byte(err.Error())
		}
		return fmt.Errorf("job %s: %s %+v: %s", job.Name, ctr, state(p, ctr).Terminated, strings.TrimSpace(string(said)))
	}
	return nil
}

// exitOf is how a container that ended exited; -1, no scan, for one that has not.
func exitOf(s corev1.ContainerState) int {
	if s.Terminated == nil {
		return -1
	}
	return int(s.Terminated.ExitCode)
}

// killed is a container the out-of-memory killer or a signal ended, never the tool exiting on its own.
func killed(s corev1.ContainerState) bool {
	t := s.Terminated
	return t != nil && (t.Reason == "OOMKilled" || t.ExitCode >= 128)
}

// feedTar writes the tar the feed container unpacks into /scan: the tree under tree/, checked as a
// build's is, opengrep's rules under rules/, and /scan/<tool>.yaml, which the executor writes. A tool
// that reads no config has its skip paths left out of the tree instead. The parts are the request's
// after its json: the tree, then rules for opengrep alone.
func feedTar(w io.Writer, req executor.ScanRequest, tool executor.Tool, parts *multipart.Reader) error {
	tw := tar.NewWriter(w)
	p, err := parts.NextPart()
	if err != nil || p.FormName() != "tree" {
		return fmt.Errorf("%w: second part must be tree", executor.ErrRequest)
	}
	var skip func(string) bool
	if tool.Config == nil && req.Settings != nil { // a packages target's tree is the tree as it came
		skip = func(entry string) bool {
			for _, s := range req.Settings.SkipPaths {
				if entry == s || strings.HasPrefix(entry, s+"/") {
					return true
				}
			}
			return false
		}
	}
	if err := tw.WriteHeader(&tar.Header{Name: "tree/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil { // an empty package is still a tree, as on Docker
		return err
	}
	if err := copyTree(p, tw, "tree/", skip, nil); err != nil {
		return fmt.Errorf("%w: tree: %v", executor.ErrRequest, err)
	}
	switch p, err := parts.NextPart(); {
	case errors.Is(err, io.EOF) && req.Tool != "opengrep":
	case err != nil || p.FormName() != "rules" || req.Tool != "opengrep": // opengrep with no rules would find nothing
		return fmt.Errorf("%w: after tree, opengrep takes part rules and no other tool takes a part", executor.ErrRequest)
	default:
		rules := &countingReader{r: io.LimitReader(p, executor.MaxRules+1)} // counted, not cut: a cut on a block boundary reads as a shorter archive
		err := copyTree(rules, tw, "rules/", nil, nil)
		if _, _ = io.Copy(io.Discard, rules); rules.n > executor.MaxRules {
			return fmt.Errorf("%w: rules exceed %d bytes", executor.ErrRequest, executor.MaxRules)
		}
		if err != nil {
			return fmt.Errorf("%w: rules: %v", executor.ErrRequest, err)
		}
	}
	if tool.Config != nil {
		config := tool.Config(*req.Settings)
		if err := tw.WriteHeader(&tar.Header{Name: req.Tool + ".yaml", Mode: 0o644, Size: int64(len(config)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := tw.Write(config); err != nil {
			return err
		}
	}
	return tw.Close()
}

// sbomTar writes the tar the feed unpacks into /scan for an sbom target: the part, checked as on Docker,
// as sbom.cdx.json.
func sbomTar(w io.Writer, b []byte) error {
	tw := tar.NewWriter(w)
	if err := tw.WriteHeader(&tar.Header{Name: "sbom.cdx.json", Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := tw.Write(b); err != nil {
		return err
	}
	return tw.Close()
}

// imageTarget is an app's image, or the run image, as an image scan reads it from the registry: the size of its tarball, laid
// out as docker save writes it, its io.buildpacks.lifecycle.metadata label and its layers' diff IDs in order.
type imageTarget struct {
	ref       name.Reference
	img       v1.Image
	size      int64
	lifecycle string
	diffIDs   []string
}

// imageRef is the request's image when it is this app's in the registry, by digest, as /run takes it, or,
// with no app, the run image GET /builder answers now, kpack's copy, read with the same credential and so
// only in CANYON_KUBE_REGISTRY (contracts/executor.md, The run image): ErrRequest for another reference,
// *Refused run_image_moved for another run image.
func (e *Executor) imageRef(ctx context.Context, req executor.ScanRequest) (name.Reference, error) {
	if req.App == "" {
		run, err := e.RunImageID(ctx)
		if err != nil {
			return nil, err
		}
		if req.Image != run {
			return nil, &executor.Refused{Reason: "run_image_moved", Message: fmt.Sprintf("the run image is now %s, not %s: plan again", run, req.Image)}
		}
		ref, err := name.NewDigest(run, name.StrictValidation)
		if err != nil {
			return nil, err
		}
		if ref.Context().RegistryStr() != e.Registry { // the executor's credential goes to its own registry alone
			return nil, fmt.Errorf("the run image %s is not in %s (CANYON_KUBE_REGISTRY): the executor sends its registry credential nowhere else", run, e.Registry)
		}
		return ref, nil
	}
	if repo, sum, ok := strings.Cut(req.Image, "@sha256:"); !ok || repo != e.Registry+"/canyon/"+req.App || !digest.MatchString(sum) {
		return nil, refuse("image must be %s/canyon/%s@sha256:<64 hex>, never a tag", e.Registry, req.App)
	}
	ref, err := e.ref(req.Image)
	if err != nil {
		return nil, refuse("%v", err)
	}
	return ref, nil
}

// errImageOver is an image past an image scan's bounds: over_cap, reason image.
var errImageOver = errors.New("image past its bounds")

// cappedManifests reads at most executor.MaxImageConfig and one more byte of a manifest's body, whatever its
// HEAD named: go-containerregistry would read up to 100 MiB, and a body cut short fails its digest.
type cappedManifests struct{ http.RoundTripper }

func (c cappedManifests) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := c.RoundTripper.RoundTrip(r)
	if err == nil && strings.Contains(r.URL.Path, "/manifests/") {
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.LimitReader(resp.Body, executor.MaxImageConfig+1), resp.Body}
	}
	return resp, err
}

// image reads ref's manifest from the registry with the executor's own credential, only once its HEAD names
// a size above zero and within executor.MaxImageConfig: ErrNoImage (404) for one the registry does not hold,
// errImageOver for one past the bound.
func (e *Executor) image(ctx context.Context, ref name.Reference) (v1.Image, error) {
	head, err := remote.Head(ref, e.registry(ctx)...)
	switch {
	case notFound(err):
		return nil, fmt.Errorf("%w: %s", executor.ErrNoImage, ref)
	case err != nil:
		return nil, fmt.Errorf("image %s: %w", ref, err)
	case head.Size <= 0 || head.Size > executor.MaxImageConfig: // a size unknown (-1) is no bound at all
		return nil, errImageOver
	case !head.MediaType.IsImage():
		return nil, fmt.Errorf("image %s is a %s, not an image's manifest", ref, head.MediaType)
	}
	desc, err := remote.Get(ref, append(e.registry(ctx), remote.WithTransport(cappedManifests{remote.DefaultTransport}))...)
	switch {
	case err != nil:
		return nil, fmt.Errorf("image %s: %w", ref, err)
	case int64(len(desc.Manifest)) != head.Size: // its digest matched, its HEAD did not say what it served
		return nil, fmt.Errorf("image %s: the manifest served %d bytes, its HEAD named %d", ref, len(desc.Manifest), head.Size)
	}
	img, err := desc.Image()
	if err != nil {
		return nil, fmt.Errorf("image %s: %w", ref, err)
	}
	return img, nil
}

// imageTarget reads the image's manifest (image) and config, the config only once its descriptor names a
// size above zero and within executor.MaxImageConfig, so nothing past a bound is held in memory (a layer's
// size, too, must be known): errImageOver for one past a bound — its layers' compressed bytes past
// CANYON_SCAN_IMAGE_MAX, more than executor.MaxImageLayers, a lifecycle label past executor.MaxLifecycle.
// Its layers are read only as feed streams them.
func (e *Executor) imageTarget(ctx context.Context, ref name.Reference) (*imageTarget, error) {
	img, err := e.image(ctx, ref)
	if err != nil {
		return nil, err
	}
	m, err := img.Manifest()
	if err != nil {
		return nil, fmt.Errorf("image %s: %w", ref, err)
	}
	size, unknown := m.Config.Size, m.Config.Size <= 0
	for _, l := range m.Layers {
		size, unknown = size+l.Size, unknown || l.Size <= 0
	}
	if unknown || m.Config.Size > executor.MaxImageConfig || len(m.Layers) > executor.MaxImageLayers || size > executor.ImageMax() {
		return nil, errImageOver
	}
	config, err := img.ConfigFile() // read to the descriptor's size alone, which go-containerregistry holds it to
	if err != nil {
		return nil, fmt.Errorf("image %s: %w", ref, err)
	}
	a := &imageTarget{ref: ref, img: img, lifecycle: config.Config.Labels[executor.LifecycleLabel]}
	for _, d := range config.RootFS.DiffIDs {
		a.diffIDs = append(a.diffIDs, d.String())
	}
	switch {
	case len(a.lifecycle) > executor.MaxLifecycle || len(a.diffIDs) > executor.MaxImageLayers:
		return nil, errImageOver
	case len(a.diffIDs) == 0:
		return nil, fmt.Errorf("image %s: no layers", ref)
	}
	if a.size, err = tarball.CalculateSize(map[name.Reference]v1.Image{ref: img}); err != nil {
		return nil, fmt.Errorf("image %s: %w", ref, err)
	}
	return a, nil
}

// feed writes the tar the feed unpacks into /scan for an image target: tmp/, trivy's TMPDIR, and the image
// as image.tar, readable by the scan's user.
func (a *imageTarget) feed(w io.Writer) error {
	tw := tar.NewWriter(w)
	if err := tw.WriteHeader(&tar.Header{Name: "tmp/", Mode: 0o1777, Typeflag: tar.TypeDir}); err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: "image.tar", Mode: 0o644, Size: a.size, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if err := tarball.Write(a.ref, a.img, tw); err != nil {
		return fmt.Errorf("image %s: %w", a.ref, err)
	}
	return tw.Close()
}

// scanJob is the scan's Job: the pinned busybox waits for its feed, then the tool runs, its stderr
// discarded — the kubelet merges it into the log — and a tool with JSON output under executor.ThenJSON,
// its log the SARIF, a NUL and that output; in 2 CPU and 2 Gi, /tmp a memory emptyDir, /scan an
// emptyDir of at most disk, which the tool's container requests, and the tool's database, if it reads
// one, read-only at /scan/db.
func (e *Executor) scanJob(tool, run, image string, t executor.Tool, disk resource.Quantity) *batchv1.Job {
	mounts := []corev1.VolumeMount{{Name: "scan", MountPath: "/scan"}, {Name: "tmp", MountPath: "/tmp"}}
	volumes := []corev1.Volume{
		{Name: "scan", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &disk}}},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}},
	}
	if t.DB != nil && tool != "trivy" { // trivy's database is its server's
		mounts, volumes = append(mounts, corev1.VolumeMount{Name: "db", MountPath: "/scan/db", ReadOnly: true}), append(volumes, dbVolume(tool, true))
	}
	command := stdoutOnly(t.Entrypoint...)
	if t.JSONOut {
		command = append([]string{"/scan/busybox", "sh", "-c", executor.ThenJSON, "sh"}, t.Entrypoint...)
	}
	cpu := "250m"
	if tool == "trivy" { // a client: the server matches; measured 59 ms of CPU a scan (#1685)
		cpu = "50m"
	}
	limits := resources(cpu, "512Mi", "2", "2Gi")
	limits.Requests[corev1.ResourceEphemeralStorage] = disk
	job := podJob("canyon-scan-"+tool+"-"+run, "canyon.scan", run, t.Deadline, corev1.PodSpec{
		InitContainers: []corev1.Container{{
			Name: "feed", Image: cmp.Or(e.BusyboxStatic, scanBusybox), Command: []string{"sh", "-c", "until [ -e " + fed + " ]; do sleep 1; done"},
			VolumeMounts: mounts[:1], Resources: resources("10m", "16Mi", "100m", "64Mi"),
		}},
		Containers: []corev1.Container{{
			Name: "tool", Image: image, Command: command, Args: t.Args, Env: envOf(t.Env),
			VolumeMounts: mounts, Resources: limits,
		}},
		Volumes: volumes,
	})
	if tool == "trivy" { // its pod reaches DNS and the server (canyon-scan-trivy)
		job.Spec.Template.Labels[clientLabel] = "true"
	}
	return job
}

// readerJob is the Job that reads when the tool's database was built: the pinned busybox copies itself
// to /scan for the reader, which runs as the scan's tool does, its database read-only at /db.
func (e *Executor) readerJob(tool, run, image string, t executor.Tool) *batchv1.Job {
	entry := t.Entrypoint
	if t.DB.ReadWith != "" {
		entry = []string{t.DB.ReadWith}
	}
	scan := corev1.VolumeMount{Name: "scan", MountPath: "/scan"}
	return podJob("canyon-scan-read-"+tool+"-"+run, "canyon.scan", run, t.Deadline, corev1.PodSpec{
		InitContainers: []corev1.Container{{
			Name: "busybox", Image: cmp.Or(e.BusyboxStatic, scanBusybox), Command: []string{"cp", "/bin/busybox", "/scan/busybox"},
			VolumeMounts: []corev1.VolumeMount{scan}, Resources: resources("10m", "16Mi", "100m", "64Mi"),
		}},
		Containers: []corev1.Container{{
			Name: "reader", Image: image, Command: stdoutOnly(append(slices.Clone(entry), t.DB.Read...)...), Env: envOf(t.DB.ReadEnv),
			VolumeMounts: []corev1.VolumeMount{scan, {Name: "db", MountPath: "/db", ReadOnly: true}}, Resources: resources("250m", "512Mi", "2", "2Gi"),
		}},
		Volumes: []corev1.Volume{
			{Name: "scan", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: new(resource.MustParse("16Mi"))}}}, // the busybox alone
			dbVolume(tool, true),
		},
	})
}

// refreshJob is the Job that downloads the tool's database into its volume, writable at /db: the
// image's entrypoint and the refresh's argv, as the scan's user, whose group fsGroup makes the volume's
// (grype keeps its database directory its own user's alone, so the scans' user must write it).
func (e *Executor) refreshJob(tool, run string, t executor.Tool) *batchv1.Job {
	nobody := int64(65534)
	var sweep []corev1.Container
	if t.DB.Partials != "" { // what a refresh cut short left, removed before the tool runs; it makes no network calls
		sweep = []corev1.Container{{
			Name: "sweep", Image: cmp.Or(e.BusyboxStatic, scanBusybox), Command: []string{"sh", "-c", "rm -rf /db/" + t.DB.Partials},
			VolumeMounts: []corev1.VolumeMount{{Name: "db", MountPath: "/db"}}, Resources: resources("10m", "16Mi", "100m", "64Mi"),
		}}
	}
	return podJob("canyon-scandb-"+tool+"-"+run, "canyon.scandb", run, executor.DBDeadline, corev1.PodSpec{
		SecurityContext: &corev1.PodSecurityContext{FSGroup: &nobody},
		InitContainers:  sweep,
		Containers: []corev1.Container{{
			Name: "refresh", Image: t.Image, Command: append(refreshWith(t), t.DB.Refresh...), Env: envOf(t.DB.Env),
			VolumeMounts: []corev1.VolumeMount{{Name: "db", MountPath: "/db"}, {Name: "tmp", MountPath: "/tmp"}}, Resources: resources("250m", "512Mi", "2", "2Gi"),
		}},
		Volumes: []corev1.Volume{
			dbVolume(tool, false),
			{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}},
		},
	})
}

// refreshWith is what a refresh's argv follows: the database's own entrypoint, else the image's.
func refreshWith(t executor.Tool) []string {
	if t.DB.RefreshWith != "" {
		return []string{t.DB.RefreshWith}
	}
	return slices.Clone(t.Entrypoint)
}

// podJob is a Job of canyon-scans running spec's pod once, until deadline: the Job labelled label, the pod
// label and canyon.scan-run: run. The pod holds no service-account token and no service links, runs
// Docker's default seccomp profile — a pod runs unconfined unless it asks, or its kubelet defaults to it —
// and each container as the scan's user, 65534, with a read-only root filesystem, every capability
// dropped and no privilege escalation.
func podJob(name, label, run string, deadline time.Duration, spec corev1.PodSpec) *batchv1.Job {
	nobody := int64(65534)
	locked := &corev1.SecurityContext{RunAsUser: &nobody, RunAsGroup: &nobody, RunAsNonRoot: new(true), ReadOnlyRootFilesystem: new(true), AllowPrivilegeEscalation: new(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}
	for _, cs := range [][]corev1.Container{spec.InitContainers, spec.Containers} {
		for i := range cs {
			cs[i].SecurityContext = locked
		}
	}
	spec.SecurityContext = cmp.Or(spec.SecurityContext, &corev1.PodSecurityContext{})
	spec.SecurityContext.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	spec.RestartPolicy, spec.AutomountServiceAccountToken, spec.EnableServiceLinks = corev1.RestartPolicyNever, new(false), new(false)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ScanNamespace, Labels: map[string]string{label: "true"}},
		Spec: batchv1.JobSpec{
			BackoffLimit: new(int32(0)), ActiveDeadlineSeconds: new(int64(deadline / time.Second)),
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{label: "true", "canyon.scan-run": run}}, Spec: spec},
		},
	}
}

// stdoutOnly runs argv under the busybox the feed leaves at /scan/busybox with its stderr discarded, so
// its log is its stdout alone: the kubelet merges the two.
func stdoutOnly(argv ...string) []string {
	return append([]string{"/scan/busybox", "sh", "-c", `exec "$@" 2>/dev/null`, "sh"}, argv...)
}

// dbVolume is the tool's database, the claim canyon-scandb-<tool>.
func dbVolume(tool string, readOnly bool) corev1.Volume {
	return corev1.Volume{Name: "db", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "canyon-scandb-" + tool, ReadOnly: readOnly}}}
}

// envOf is each KEY=value as the container's environment.
func envOf(vars []string) []corev1.EnvVar {
	var env []corev1.EnvVar
	for _, v := range vars {
		k, val, _ := strings.Cut(v, "=")
		env = append(env, value(k, val))
	}
	return env
}

// policy writes the NetworkPolicy name, selecting every pod of canyon-scans labelled label, letting
// nothing in and nothing out but egress, before each pod that relies on it: created, or rewritten to this
// release's rules, so a rule an earlier release wrote never outlives it. Only this executor writes it,
// so another write that raced this one (AlreadyExists, Conflict) wrote the same.
func (e *Executor) policy(ctx context.Context, name, label string, egress ...networkingv1.NetworkPolicyEgressRule) error {
	return e.policyWith(ctx, name, label, nil, egress...)
}

// policyWith is policy letting ingress in as well.
func (e *Executor) policyWith(ctx context.Context, name, label string, ingress []networkingv1.NetworkPolicyIngressRule, egress ...networkingv1.NetworkPolicyEgressRule) error {
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ScanNamespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{label: "true"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress:     ingress,
			Egress:      egress,
		},
	}
	nps := e.Client.NetworkingV1().NetworkPolicies(ScanNamespace)
	cur, err := nps.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = nps.Create(ctx, np, metav1.CreateOptions{})
	case err == nil:
		cur.Spec = np.Spec
		_, err = nps.Update(ctx, cur, metav1.UpdateOptions{})
	}
	if err != nil && !apierrors.IsAlreadyExists(err) && !apierrors.IsConflict(err) {
		return fmt.Errorf("networkpolicy %s: %w", name, err)
	}
	return nil
}

// scanPod polls this scan's pod until ready says it is, or ctx ends.
func (e *Executor) scanPod(ctx context.Context, run string, ready func(corev1.Pod) bool) (*corev1.Pod, error) {
	for {
		list, err := e.Client.CoreV1().Pods(ScanNamespace).List(ctx, metav1.ListOptions{LabelSelector: "canyon.scan=true,canyon.scan-run=" + run})
		if err != nil {
			return nil, fmt.Errorf("the scan's pod: %w", err)
		}
		for i := range list.Items {
			if ready(list.Items[i]) {
				return &list.Items[i], nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(e.poll()):
		}
	}
}

// feedOf waits for the scan's pod to run its feed and answers it; or, when the Job's deadline ends it
// first, nil and the scheduler's message while the pod was last seen unscheduled — its PodScheduled
// condition False with reason Unschedulable, quoted as the scheduler wrote it — "" when it was placed.
// A Job that failed otherwise (its pod evicted) is an error naming why.
func (e *Executor) feedOf(ctx context.Context, run, job string) (*corev1.Pod, string, error) {
	unscheduled := "" // as last seen: the Job deletes the pod at its deadline
	for {
		list, err := e.Client.CoreV1().Pods(ScanNamespace).List(ctx, metav1.ListOptions{LabelSelector: "canyon.scan=true,canyon.scan-run=" + run})
		if ctx.Err() != nil {
			return nil, unscheduled, nil
		}
		if err != nil {
			return nil, "", fmt.Errorf("the scan's pod: %w", err)
		}
		for i := range list.Items {
			p := &list.Items[i]
			if state(p, "feed").Running != nil {
				return p, "", nil
			}
			for _, c := range p.Status.Conditions {
				switch {
				case c.Type != corev1.PodScheduled:
				case c.Status == corev1.ConditionTrue: // placed since: whatever it waits on now is not room
					unscheduled = ""
				case c.Reason == corev1.PodReasonUnschedulable:
					unscheduled = c.Message
				}
			}
		}
		j, err := e.Client.BatchV1().Jobs(ScanNamespace).Get(ctx, job, metav1.GetOptions{})
		if err == nil {
			for _, c := range j.Status.Conditions {
				switch {
				case c.Status != corev1.ConditionTrue || c.Type != batchv1.JobFailed && c.Type != batchv1.JobFailureTarget:
				case c.Reason == batchv1.JobReasonDeadlineExceeded:
					return nil, unscheduled, nil
				default:
					return nil, "", fmt.Errorf("job %s failed before its pod ran: %s", job, c.Reason)
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil, unscheduled, nil
		case <-time.After(e.poll()):
		}
	}
}

// scanEnd waits for the Job's verdict, then answers its pod, which pods selects, as it ended; nil when the verdict is
// the Job's deadline — which ends the pod with a signal, as the out-of-memory killer does, so only the
// Job tells timeout from killed — or when this call's bound runs out.
func (e *Executor) scanEnd(ctx context.Context, job, pods string) (*corev1.Pod, error) {
	for {
		j, err := e.Client.BatchV1().Jobs(ScanNamespace).Get(ctx, job, metav1.GetOptions{})
		if ctx.Err() != nil {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("job %s: %w", job, err)
		}
		for _, c := range j.Status.Conditions {
			switch {
			case c.Status != corev1.ConditionTrue || c.Type != batchv1.JobComplete && c.Type != batchv1.JobFailed:
			case c.Reason == batchv1.JobReasonDeadlineExceeded:
				return nil, nil
			default:
				list, err := e.Client.CoreV1().Pods(ScanNamespace).List(ctx, metav1.ListOptions{LabelSelector: pods})
				if err != nil {
					return nil, fmt.Errorf("the pod of %s: %w", job, err)
				}
				for i, p := range list.Items {
					if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
						return &list.Items[i], nil
					}
				}
				return nil, fmt.Errorf("job %s is %s and no pod of it has ended", job, c.Type)
			}
		}
		select {
		case <-ctx.Done():
			return nil, nil
		case <-time.After(e.poll()):
		}
	}
}

// dropJob deletes a Job of canyon-scans and waits, within two minutes, until its pod is gone: the tool's
// lock is held until then, so the next pod to mount a ReadWriteOnce volume never finds it still attached
// to another node (Multi-Attach).
func (e *Executor) dropJob(ctx context.Context, name string) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	background := metav1.DeletePropagationBackground
	if err := e.Client.BatchV1().Jobs(ScanNamespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &background}); err != nil && !apierrors.IsNotFound(err) {
		log.Printf("executor: deleting scan job %s: %v", name, err)
		return
	}
	for {
		list, err := e.Client.CoreV1().Pods(ScanNamespace).List(ctx, metav1.ListOptions{LabelSelector: "batch.kubernetes.io/job-name=" + name})
		if err == nil && len(list.Items) == 0 {
			return
		}
		select {
		case <-ctx.Done():
			log.Printf("executor: the pod of scan job %s outlived it: %v", name, cmp.Or(err, ctx.Err()))
			return
		case <-time.After(e.poll()):
		}
	}
}

// SweepScans deletes every Job labelled canyon.scan or canyon.scandb, and its pod, that a crash left
// (invariant 8): a refresh left running would write a database no lock guards. The binary calls it once,
// at start, before it serves.
func (e *Executor) SweepScans(ctx context.Context) {
	for _, label := range []string{"canyon.scan=true", "canyon.scandb=true"} {
		list, err := e.Client.BatchV1().Jobs(ScanNamespace).List(ctx, metav1.ListOptions{LabelSelector: label})
		if err != nil {
			log.Printf("executor: scan sweep: %v", err)
			continue
		}
		for _, j := range list.Items {
			e.dropJob(ctx, j.Name)
		}
	}
}
