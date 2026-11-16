package kube

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/getcanyon-ai/canyon/executor"
)

// Handler serves every call of contracts/executor.md on Kubernetes: executor.Handler's, PUT /access,
// POST /probe, POST /sidecars and GET /platform, which the Docker executor does not route, and POST /callers, which each
// answers its own way.
func Handler(e *Executor) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", executor.Handler(e))
	mux.HandleFunc("POST /callers", executor.CallersHandler(e.Callers))
	mux.HandleFunc("POST /rebase", executor.RebaseHandler(e.Rebase)) // kpack's rebase (contracts/executor.md, POST /rebase)
	mux.HandleFunc("PUT /access", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			App    string          `json:"app"`
			Target string          `json:"target"`
			Access json.RawMessage `json:"access"`
		}
		if err := decode(r.Body, &req); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		switch err := e.PutAccess(r.Context(), req.App, req.Target, req.Access); {
		case errors.Is(err, executor.ErrRequest):
			fail(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, executor.ErrNoContainer):
			fail(w, http.StatusNotFound, err.Error())
		case errors.Is(err, executor.ErrStale):
			fail(w, http.StatusConflict, "stale")
		case err != nil:
			fail(w, http.StatusBadGateway, err.Error())
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("POST /sidecars", func(w http.ResponseWriter, r *http.Request) {
		res, err := e.Sidecars(r.Context())
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	})
	mux.HandleFunc("GET /platform", func(w http.ResponseWriter, r *http.Request) {
		switch res, err := e.Platform(r.Context(), r.URL.Query().Get("image")); {
		case errors.Is(err, executor.ErrRequest):
			fail(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, executor.ErrNoImage): // the contract's platform_gone: an older executor's 404 names no reason
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error(), "reason": "image_gone"})
		case err != nil:
			fail(w, http.StatusBadGateway, err.Error())
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(res)
		}
	})
	mux.HandleFunc("POST /probe", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Domain string `json:"domain"`
			Form   string `json:"form"`
		}
		if err := decode(r.Body, &req); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		switch res, err := e.Probe(r.Context(), req.Domain, req.Form); {
		case errors.Is(err, executor.ErrRequest):
			fail(w, http.StatusBadRequest, err.Error())
		case err != nil:
			fail(w, http.StatusBadGateway, err.Error())
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(res)
		}
	})
	return mux
}

// decode reads one JSON object of at most 1 MiB, only the call's keys, and nothing after it.
func decode(r io.Reader, v any) error {
	dec := json.NewDecoder(io.LimitReader(r, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data after the JSON object")
	}
	return nil
}

func fail(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
