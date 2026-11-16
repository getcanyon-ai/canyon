package intercept

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

// A route's value (contracts/routes.md, Interception, The value): asked for on first use, GET /egress/route-value,
// one ask a route at a time, each valueWait at most and none past the check's own budget from when it began, a refetch
// of the caller included, inside Envoy's 5 s, so a value not read in time is inject_unavailable, never the 503. It is
// held in memory alone, never on disk or in a log line or an error, used only under the rev the policy names for the
// route, and dropped when that rev changes, the route goes or the epoch does.

// valueWait is one ask's bound, inside the check's own (internal/egresscontrol's valueBudget).
const valueWait = 2 * time.Second

// routeKey names one route: its app, its env and its destination.
type routeKey = [3]string

// value is a route's value as GET /egress/route-value answered it, under the write it was sealed with.
type value struct {
	rev   int64
	value string
}

var (
	errValueNotRead  = errors.New("a route value that is not one the schema takes") // never the decoder's own, which may quote a byte of it
	errValueLate     = errors.New("no route value within the check's time")
	errValueRev      = errors.New("a route value sealed under a rev other than the policy's")
	errValueUnknown  = errors.New("a route whose scheme is none this egress-control knows")
	errValueNoFlight = errors.New("the route value's ask ended without an answer")
)

// parseValue takes GET /egress/route-value's 200 only as egress.schema.json's route_value: a rev of 1 or more and a
// value of 1 to 8,192 characters with no control character but a tab, as it goes into a header.
func parseValue(raw []byte) (value, error) {
	var a struct {
		Rev   int64   `json:"rev"`
		Value *string `json:"value"`
	}
	if json.Unmarshal(raw, &a) != nil || a.Value == nil || a.Rev < 1 {
		return value{}, errValueNotRead
	}
	v := *a.Value
	if n := len([]rune(v)); n < 1 || n > 8192 || strings.ContainsFunc(v, func(r rune) bool { return (r < 0x20 && r != '\t') || r == 0x7f }) {
		return value{}, errValueNotRead
	}
	return value{a.Rev, v}, nil
}

// headerOf is the route's header's value made of v by its scheme: bearer, Bearer and v; basic, Basic and v in base64;
// raw, v as it is. A scheme it does not know makes none.
func headerOf(scheme, v string) (string, error) {
	switch scheme {
	case "bearer":
		return "Bearer " + v, nil
	case "basic":
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(v)), nil
	case "raw":
		return v, nil
	}
	return "", errValueUnknown
}

// Value is the header's value of app env's route to destination under rev, made by scheme: the value held under rev,
// else asked for — once however many checks wait on it, valueWait at most, and held under rev while the policy
// installed names the route under it and its epoch is the one the ask began under, before any check is answered, so a
// check after it never asks again — and waited on until deadline. An answer under another rev is no value.
func (i *Interceptor) Value(deadline time.Time, app, env, destination, scheme string, rev int64) (string, error) {
	v, err := i.valueOf(deadline, routeKey{app, env, destination}, rev)
	if err != nil {
		return "", err
	}
	return headerOf(scheme, v)
}

func (i *Interceptor) valueOf(deadline time.Time, k routeKey, rev int64) (string, error) {
	if v, held := i.heldValue(k, rev); held {
		return v, nil
	}
	if !time.Now().Before(deadline) {
		return "", errValueLate
	}
	flight := i.valueFlights.DoChan(k[0]+"\x00"+k[1]+"\x00"+k[2]+"\x00"+strconv.FormatInt(rev, 10), func() (any, error) {
		if v, held := i.heldValue(k, rev); held { // a flight that ended as this check missed it
			return v, nil
		}
		i.mu.Lock()
		epoch := i.epoch
		i.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), valueWait)
		defer cancel()
		raw, err := i.valueAsk(ctx, k[0], k[1], k[2])
		if err != nil {
			return "", err
		}
		got, err := parseValue(raw)
		switch {
		case err != nil:
			return "", err
		case got.rev != rev:
			return "", errValueRev
		}
		i.mu.Lock()
		defer i.mu.Unlock()
		if r, named := i.routes[k]; named && r == got.rev && i.epoch == epoch {
			i.values[k] = got
		}
		return got.value, nil
	})
	late := time.NewTimer(time.Until(deadline))
	defer late.Stop()
	select {
	case <-late.C:
		return "", errValueLate
	case res := <-flight:
		v, ok := res.Val.(string)
		switch {
		case res.Err != nil:
			return "", res.Err
		case !ok:
			return "", errValueNoFlight
		}
		return v, nil
	}
}

// heldValue is the value held of k under rev.
func (i *Interceptor) heldValue(k routeKey, rev int64) (string, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	v, held := i.values[k]
	return v.value, held && v.rev == rev
}

// Installed is told each policy egress-control installs, before it decides with it: its epoch and every route it
// names, by app, env and destination, under its rev. Every value held that it does not name under the same rev is
// dropped, and every one when its epoch is not the one before.
func (i *Interceptor) Installed(epoch string, routes map[[3]string]int64) {
	i.mu.Lock()
	defer i.mu.Unlock()
	for k, v := range i.values {
		if r, named := routes[k]; epoch != i.epoch || !named || r != v.rev {
			delete(i.values, k)
		}
	}
	i.epoch, i.routes = epoch, routes
}
