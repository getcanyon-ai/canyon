package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	maxLine     = 64 << 10
	defaultTail = 200
	maxTail     = 10000
)

// maxBody bounds a /build request on the wire (the tree is bounded again by untar), and treeIdle how
// long one read of it may wait for bytes; vars so a test can lower them. builds and scans bound the
// /build and /scan calls in flight (contracts/executor.md, Busy), each its own: Limit sets them.
var (
	maxBody  int64 = maxTree + 64<<20
	treeIdle       = 60 * time.Second
	builds         = make(chan struct{}, 4)
	scans          = make(chan struct{}, 4)
)

// Limit sets the work in flight from CANYON_EXECUTOR_BUILDS and CANYON_EXECUTOR_SCANS, unset being the
// host's defaults, before the executor serves; anything but a whole number from 1 to 100 is refused by
// name. They are set at or above the server's queues, so busy means real overload.
func Limit(getenv func(string) string, defaultBuilds, defaultScans int) error {
	read := func(name string, otherwise int) (int, error) {
		v := getenv(name)
		if v == "" {
			return otherwise, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			return 0, fmt.Errorf("%s is %q: a whole number from 1 to 100", name, v)
		}
		return n, nil
	}
	b, err := read("CANYON_EXECUTOR_BUILDS", defaultBuilds)
	if err != nil {
		return err
	}
	s, err := read("CANYON_EXECUTOR_SCANS", defaultScans)
	if err != nil {
		return err
	}
	builds, scans = make(chan struct{}, b), make(chan struct{}, s)
	return nil
}

// Calls are the calls of contracts/executor.md that Handler serves: *Executor runs them on
// Docker, and the Kubernetes executor on a cluster. Handler answers the error kinds each call's
// contract names — ErrRequest 400, ErrNoContainer and a /run's or /scan's ErrNoImage 404, ErrInUse, a /run's ErrInternal and ErrOpen and a /scan's *Refused 409, ErrBusy 503 — and any other
// error 502; a /build's is the stream's last line.
type Calls interface {
	Build(ctx context.Context, req BuildRequest, tree io.Reader, out io.Writer) (BuildResult, error)
	Run(ctx context.Context, req RunRequest) (RunResult, error)
	BuilderImage(ctx context.Context) (string, error)
	// Collect starts the image cleanup that follows a /run's 200, sparing the images keep names, and
	// returns at once: it neither delays nor fails the answer, and the request's end does not cut it short.
	Collect(ctx context.Context, app string, keep []string)
	Stop(ctx context.Context, app, target string) error
	Forget(ctx context.Context, app string) error
	Running(ctx context.Context, app, target string) (string, error)
	RunningOf(ctx context.Context, containers []Container, why bool) (Runs, error)
	Logs(ctx context.Context, app, target string, tail int, follow bool, out io.Writer, ready func()) error
	Scan(ctx context.Context, req ScanRequest, parts *multipart.Reader) (ScanResult, error)
}

// Handler serves the calls of contracts/executor.md. /build is multipart with the json
// part first and the tree (a tar) second, so the tree streams into the build without buffering;
// the response is one {"line": …} object per build-log line, then the BuildResult, or an
// {"error": …} line — the headers are long gone by then, so failure is a line, never a 500.
func Handler(e Calls) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fail(w, http.StatusNotFound, "no such call: POST /build, POST /run, POST /stop, POST /forget, POST /scan, POST /rebase, GET /builder, GET /running, POST /running or GET /logs")
	})
	mux.HandleFunc("POST /build", func(w http.ResponseWriter, r *http.Request) {
		select {
		case builds <- struct{}{}:
			defer func() { <-builds }()
		default:
			w.Header().Set("Retry-After", "10")
			fail(w, http.StatusServiceUnavailable, "busy")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody) // before the multipart reader captures the body
		mr, err := r.MultipartReader()
		if err != nil {
			fail(w, http.StatusBadRequest, "multipart body required: part json, then part tree")
			return
		}
		var req BuildRequest
		p, err := mr.NextPart()
		if err != nil || p.FormName() != "json" || decode(p, &req) != nil {
			fail(w, http.StatusBadRequest, "first part must be json, at most 1 MiB, nothing after the object")
			return
		}
		if p, err = mr.NextPart(); err != nil || p.FormName() != "tree" {
			fail(w, http.StatusBadRequest, "second part must be tree")
			return
		}
		if !validApp(req.App) || !validPackage(req.Package) {
			fail(w, http.StatusBadRequest, "app and package must be [a-z0-9-_] names")
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		out := &lineWriter{w: w}
		// a rolling read deadline: the tree may stream for as long as it keeps arriving, never for as long as a trickle wants
		res, err := e.Build(r.Context(), req, &rolling{r: p, rc: http.NewResponseController(w)}, out)
		out.flush()
		var undetected *Undetected
		var noRoom *NoRoom
		var host *HostFailed
		switch {
		case errors.As(err, &noRoom):
			_ = encode(out.w, map[string]string{"error": err.Error(), "no_room": noRoom.Reason})
			return
		case errors.As(err, &undetected):
			line := map[string]any{"error": err.Error(), "builds": undetected.Builds}
			if undetected.Errored {
				line["errored"] = true
			}
			_ = encode(out.w, line)
			return
		case errors.As(err, &host):
			_ = encode(out.w, map[string]any{"error": err.Error(), "own": true})
			return
		case err != nil:
			_ = encode(out.w, map[string]string{"error": err.Error()})
			return
		}
		_ = encode(out.w, res)
	})
	mux.HandleFunc("POST /scan", func(w http.ResponseWriter, r *http.Request) {
		refuse := func(msg string) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = encode(w, map[string]string{"error": "invalid_request", "message": msg})
		}
		select { // /scan's own limit on work in flight
		case scans <- struct{}{}:
			defer func() { <-scans }()
		default:
			w.Header().Set("Retry-After", "10")
			fail(w, http.StatusServiceUnavailable, "busy")
			return
		}
		r.Body = http.MaxBytesReader(w, io.NopCloser(&rolling{r: r.Body, rc: http.NewResponseController(w)}), maxBody+maxRules)
		mr, err := r.MultipartReader()
		var p *multipart.Part
		if err == nil {
			p, err = mr.NextPart()
		}
		if err != nil || p.FormName() != "json" {
			refuse("multipart body required: part json, then part tree, then for opengrep part rules; for an sbom target part sbom; for an image target part json alone")
			return
		}
		var req ScanRequest
		dec := json.NewDecoder(io.LimitReader(p, 1<<20))
		dec.DisallowUnknownFields() // the request is an allow-list: any other key is refused
		if err := dec.Decode(&req); err != nil || dec.More() {
			refuse(fmt.Sprintf("json part: one object of at most 1 MiB, only the contract's keys: %v", err))
			return
		}
		res, err := e.Scan(r.Context(), req, mr)
		var refused *Refused
		switch {
		case errors.Is(err, ErrRequest):
			refuse(err.Error())
		case errors.As(err, &refused): // not the run image GET /builder answers now (contracts/executor.md, The run image)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = encode(w, map[string]string{"error": refused.Reason, "message": refused.Message})
		case errors.Is(err, ErrNoImage): // an image target's image this host does not hold, as /run's
			fail(w, http.StatusNotFound, err.Error())
		case errors.Is(err, ErrQueued): // another scan of the tool runs: busy with no reason, as the limit is
			w.Header().Set("Retry-After", "10")
			fail(w, http.StatusServiceUnavailable, "busy")
		case errors.Is(err, ErrBusy): // the tool's database: the run says what it waits on (contracts/executor.md, Busy)
			w.Header().Set("Retry-After", "10")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = encode(w, map[string]string{"error": "busy", "reason": "database"})
		case err != nil:
			fail(w, http.StatusBadGateway, err.Error())
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = encode(w, res)
		}
	})
	mux.HandleFunc("POST /run", func(w http.ResponseWriter, r *http.Request) {
		var req RunRequest
		if err := decode(r.Body, &req); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		res, err := e.Run(r.Context(), req)
		if errors.Is(err, ErrRequest) {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if errors.Is(err, ErrInternal) || errors.Is(err, ErrOpen) {
			fail(w, http.StatusConflict, err.Error())
			return
		}
		if errors.Is(err, ErrNoImage) {
			fail(w, http.StatusNotFound, err.Error())
			return
		}
		var exited *Exited
		if errors.As(err, &exited) { // the app's own lines ride with the reason (contracts/executor.md, /run)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_ = encode(w, exited)
			return
		}
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		e.Collect(r.Context(), req.App, req.Keep) // started before the answer, so whoever waits after reading it waits for the cleanup too
		w.Header().Set("Content-Type", "application/json")
		_ = encode(w, res)
	})
	mux.HandleFunc("POST /forget", func(w http.ResponseWriter, r *http.Request) {
		var req ForgetRequest
		if err := decode(r.Body, &req); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		switch err := e.Forget(r.Context(), req.App); {
		case errors.Is(err, ErrRequest):
			fail(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, ErrInUse):
			fail(w, http.StatusConflict, err.Error())
		case err != nil:
			fail(w, http.StatusBadGateway, err.Error())
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("POST /stop", func(w http.ResponseWriter, r *http.Request) {
		var req StopRequest
		if err := decode(r.Body, &req); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		err := e.Stop(r.Context(), req.App, req.Target)
		if errors.Is(err, ErrRequest) {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /builder", func(w http.ResponseWriter, r *http.Request) {
		began := time.Now()
		builder, err := e.BuilderImage(r.Context())
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		answer := map[string]any{"builder": builder}
		if d, ok := e.(interface {
			RunImageID(context.Context) (string, error)
		}); ok { // the run image the next /build builds onto and a /rebase rebases onto, on an executor that has one
			if answer["run_image"], err = d.RunImageID(r.Context()); err != nil {
				fail(w, http.StatusBadGateway, err.Error())
				return
			}
		}
		if d, ok := e.(interface{ Prepared() time.Time }); ok { // Canyon's own images, built while this answer waited (#2711)
			if waited := d.Prepared().Sub(began).Milliseconds(); waited > 0 {
				answer["waited_ms"] = waited
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = encode(w, answer)
	})
	mux.HandleFunc("GET /scandb", func(w http.ResponseWriter, r *http.Request) { // how old each vulnerability database is (#2743)
		d, ok := e.(interface {
			ScanDBs(context.Context) ([]ScanDB, error)
		})
		if !ok {
			http.NotFound(w, r)
			return
		}
		dbs, err := d.ScanDBs(r.Context())
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = encode(w, map[string][]ScanDB{"databases": dbs})
	})
	mux.HandleFunc("GET /running", func(w http.ResponseWriter, r *http.Request) {
		image, err := e.Running(r.Context(), r.URL.Query().Get("app"), r.URL.Query().Get("target"))
		switch {
		case errors.Is(err, ErrRequest):
			fail(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, ErrNoContainer):
			fail(w, http.StatusNotFound, err.Error())
		case err != nil:
			fail(w, http.StatusBadGateway, err.Error())
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = encode(w, map[string]string{"image": image})
		}
	})
	if d, ok := e.(*Executor); ok { // the Kubernetes executor routes its own (executor/kube.Handler)
		mux.HandleFunc("POST /callers", CallersHandler(d.Callers))
		mux.HandleFunc("POST /rebase", RebaseHandler(d.Rebase))         // pack rebase, on the daemon (contracts/executor.md, POST /rebase)
		mux.HandleFunc("GET /published", publishedHandler(d.Published)) // where Docker publishes the server (contracts/executor.md, GET /published)
	}
	mux.HandleFunc("POST /networks/recreate", func(w http.ResponseWriter, r *http.Request) {
		var req RecreateRequest
		if err := decode(r.Body, &req); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		d, ok := e.(*Executor)
		if !ok { // Kubernetes: an app's NetworkPolicy is written at every /run (contracts/executor.md, POST /networks/recreate)
			fail(w, http.StatusBadRequest, "this executor runs on Kubernetes: an app's NetworkPolicy is written at every /run, and there is no network to recreate")
			return
		}
		switch err := d.RecreateNetwork(r.Context(), req.App, req.Internal); {
		case errors.Is(err, ErrRequest):
			fail(w, http.StatusBadRequest, err.Error())
		case err != nil:
			fail(w, http.StatusBadGateway, err.Error())
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("POST /running", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Containers []Container `json:"containers"`
			Why        bool        `json:"why"`
		}
		if err := decode(r.Body, &req); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		runs, err := e.RunningOf(r.Context(), req.Containers, req.Why)
		switch {
		case errors.Is(err, ErrRequest):
			fail(w, http.StatusBadRequest, err.Error())
		case err != nil:
			fail(w, http.StatusBadGateway, err.Error())
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = encode(w, runs)
		}
	})
	mux.HandleFunc("GET /logs", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		tail := defaultTail
		if s := q.Get("tail"); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 0 || n > maxTail {
				fail(w, http.StatusBadRequest, "tail must be an integer 0..10000")
				return
			}
			tail = n
		}
		follow := true
		switch q.Get("follow") {
		case "", "true":
		case "false":
			follow = false
		default:
			fail(w, http.StatusBadRequest, "follow must be true or false")
			return
		}
		out := &streamWriter{w: w}
		err := e.Logs(r.Context(), q.Get("app"), q.Get("target"), tail, follow, out, out.start) // r.Context() ends with the client, and the child with it
		switch {
		case out.started: // the stream has begun: whatever ended it, the client sees the end of the body
			if err != nil {
				log.Printf("executor: logs stream ended: %v", err)
				out.end(err) // on its reason, never on a silence that reads as a quiet app (contracts/executor.md)
			}
		case errors.Is(err, ErrRequest):
			fail(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, ErrNoContainer):
			fail(w, http.StatusNotFound, err.Error())
		case err != nil:
			fail(w, http.StatusBadGateway, err.Error())
		default:
			out.start() // an empty log that ended: 200 with nothing in it
		}
	})
	return mux
}

// streamWriter passes bytes through as they come, flushing each write. The 200 goes out when
// Logs says it is ready, after the container inspect, so a 404 is still available before it and
// a silent container still answers headers at once.
type streamWriter struct {
	w       http.ResponseWriter
	started bool
	mid     bool // the last byte out was not a newline
}

func (s *streamWriter) start() {
	if !s.started {
		s.started = true
		s.w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		s.w.WriteHeader(http.StatusOK)
		_ = http.NewResponseController(s.w).Flush() // the status line leaves now, not with the first byte
	}
}

func (s *streamWriter) Write(p []byte) (int, error) {
	s.start()
	n, err := s.w.Write(p) //nolint:gosec // text/plain to the server, which passes it on as text: never HTML
	if n > 0 {
		s.mid = p[n-1] != '\n'
	}
	_ = http.NewResponseController(s.w).Flush() // a wrapper that cannot flush is not an error we can act on
	return n, err
}

// end is the one line of the executor's own a failed read ends on, on a line of its own.
func (s *streamWriter) end(err error) {
	line := "canyon: this log could not be read: " + err.Error() + "\n"
	if s.mid {
		line = "\n" + line
	}
	_, _ = s.Write([]byte(line))
}

// decode reads one JSON object of at most 1 MiB and refuses anything after it.
func decode(r io.Reader, v any) error {
	dec := json.NewDecoder(io.LimitReader(r, 1<<20))
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF { //nolint:errorlint // sentinel from the decoder itself
		return errors.New("trailing data after the JSON object")
	}
	return nil
}

func fail(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = encode(w, map[string]string{"error": msg})
}

// encode writes one JSON object per line without HTML escaping: build logs are full of > and &.
func encode(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// lineWriter turns the build log into one {"line": …} object per line as lines complete,
// keeping a partial line across writes (exec hands the log over in 32 KiB chunks). A line
// longer than maxLine is emitted in maxLine pieces so a runaway line cannot buffer without
// bound; a trailing \r is dropped so CRLF logs read as lines.
type lineWriter struct {
	w      http.ResponseWriter
	tail   []byte
	capped bool // the last piece was cut at maxLine: a newline that follows ends that line, it is not an empty one
}

func (l *lineWriter) Write(p []byte) (int, error) {
	n := len(p) // an io.Writer reports what it consumed, not what it kept
	if l.capped && len(p) > 0 && p[0] == '\n' {
		p, l.capped = p[1:], false
	}
	l.tail = append(l.tail, p...)
	for {
		i := bytes.IndexByte(l.tail, '\n')
		if i < 0 && len(l.tail) < maxLine {
			return n, nil
		}
		l.capped = i < 0 || i > maxLine
		if l.capped {
			i = maxLine
		}
		_ = encode(l.w, map[string]string{"line": strings.TrimSuffix(string(l.tail[:i]), "\r")})
		l.tail = l.tail[i:]
		if len(l.tail) > 0 && l.tail[0] == '\n' {
			l.tail, l.capped = l.tail[1:], false
		}
	}
}

func (l *lineWriter) flush() {
	if len(l.tail) > 0 {
		_ = encode(l.w, map[string]string{"line": strings.TrimSuffix(string(l.tail), "\r")})
		l.tail = nil
	}
}

// rolling arms the request's read deadline for each read of the tree, and clears it once the read
// returns: net/http reads on after the body's end to notice the client leave, and a deadline armed
// while that read waits ends the request mid-build (canyonv2-amvo).
type rolling struct {
	r  io.Reader
	rc *http.ResponseController
}

func (x *rolling) Read(p []byte) (int, error) {
	_ = x.rc.SetReadDeadline(time.Now().Add(treeIdle))
	defer func() { _ = x.rc.SetReadDeadline(time.Time{}) }()
	return x.r.Read(p)
}

// CallersHandler serves POST /callers with callers, each executor's own (contracts/executor.md, POST
// /callers): 400 on a body out of contract, 502 when the executor cannot tell.
func CallersHandler(callers func(ctx context.Context, addresses []string) ([]Caller, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Addresses []string `json:"addresses"`
		}
		if err := decode(r.Body, &req); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		held, err := callers(r.Context(), req.Addresses)
		switch {
		case errors.Is(err, ErrRequest):
			fail(w, http.StatusBadRequest, err.Error())
		case err != nil:
			fail(w, http.StatusBadGateway, err.Error())
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = encode(w, map[string][]Caller{"callers": held})
		}
	}
}
