package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/getcanyon-ai/canyon/executor"
)

func main() {
	addr := os.Getenv("CANYON_EXECUTOR_ADDR")
	if addr == "" {
		addr = ":7778"
	}
	e, err := executorOf(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	sweepCtx, sweepCancel := context.WithTimeout(context.Background(), time.Minute) // what a crash left behind, before the first request can find it
	e.Sweep(sweepCtx)
	sweepCancel()
	// executor.IdleTimeout: the server drops an idle connection at half of it, before the executor closes it
	srv := &http.Server{Handler: executor.Handler(e), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: executor.IdleTimeout}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("canyon-executor listening on %s", addr) //nolint:gosec // our own env, not user input
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go e.RefreshDBs(ctx) // after the sweep: it removes a refresh a crash cut short
	go e.Rejoin(ctx)     // a joined container recreated since, or while this was down, back on the app networks
	if err := executor.Serve(ctx, srv, ln); err != nil {
		log.Fatal(err)
	}
}

// executorOf is the executor the environment names: CANYON_BUILDER and CANYON_BUILDER_NODE, the images it runs by digest
// (contracts/executor.md, Images and database sources), CANYON_APPS_POOL (unset 172.28.0.0/16, as the
// server's CANYON_APPS_SUBNET), CANYON_EXECUTOR_JOIN and CANYON_EXECUTOR_EGRESS_JOIN. A bad value refuses the start, naming it.
func executorOf(getenv func(string) string) (*executor.Executor, error) {
	e := &executor.Executor{Builder: getenv("CANYON_BUILDER"), NodeBuilder: getenv("CANYON_BUILDER_NODE")}
	var err error
	if e.Lifecycle, err = executor.ImageSetting(getenv, "CANYON_IMAGE_LIFECYCLE"); err != nil {
		return nil, err
	}
	if e.Busybox, err = executor.ImageSetting(getenv, "CANYON_IMAGE_BUSYBOX"); err != nil {
		return nil, err
	}
	if e.RunImage, err = executor.ImageSetting(getenv, "CANYON_IMAGE_RUN"); err != nil {
		return nil, err
	}
	if e.Holder, err = executor.DigestSetting(getenv, "CANYON_IMAGE_EGRESS_HOLDER"); err != nil {
		return nil, err
	}
	if err := executor.RunAptMirror(getenv); err != nil {
		return nil, err
	}
	if err := executor.Configure(getenv); err != nil {
		return nil, err
	}
	if err := executor.Limit(getenv, 2, 5); err != nil { // one host (contracts/executor.md, Busy): a build and a rebase, two scans, a patch scan, a deploy's image scan and the rescan's one
		return nil, err
	}
	if s := getenv("CANYON_APPS_POOL"); s != "" {
		if e.Pool, err = netip.ParsePrefix(s); err != nil {
			return nil, fmt.Errorf("CANYON_APPS_POOL: %w", err)
		}
	}
	if e.Canyon, err = executor.CanyonPools(getenv); err != nil {
		return nil, err
	}
	if e.Allow, err = executor.AllowSetting(getenv, "CANYON_BUILD_ALLOW", e.Refused()...); err != nil {
		return nil, err
	}
	if len(e.Allow) > 0 {
		log.Printf("canyon-executor: builds reach DNS, public TCP 80 and 443, and CANYON_BUILD_ALLOW's %s", strings.Join(e.Allow, ", "))
	}
	if e.AppsAllow, err = executor.AppsAllow(getenv, e.Refused()...); err != nil {
		return nil, err
	}
	if len(e.AppsAllow) > 0 {
		log.Printf("canyon-executor: apps on an open network reach public addresses, their own network, and CANYON_APPS_ALLOW's %s", strings.Join(e.AppsAllow, ", "))
	}
	if s := getenv("CANYON_EXECUTOR_JOIN"); s != "" {
		e.Join = strings.Split(s, ",")
	}
	if s := getenv("CANYON_EXECUTOR_EGRESS_JOIN"); s != "" {
		e.Door = strings.Split(s, ",")
	}
	return e, nil
}
