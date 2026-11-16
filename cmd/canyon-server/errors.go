package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/getcanyon-ai/canyon/runs"
)

// writeJSON answers with one JSON document.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr answers in the contract's error shape: {error, message, ...context}.
func writeErr(w http.ResponseWriter, status int, code, message string, extra map[string]any) {
	body := map[string]any{"error": code, "message": message}
	for k, v := range extra {
		body[k] = v
	}
	writeJSON(w, status, body)
}

// notOnCanyon is the 404 for an app this host does not hold (design/EXPERIENCE.md D5), naming it as
// asked. A name that is no project id cannot be on any Canyon and is quoted, so a control byte it
// carries never reaches a terminal raw.
func notOnCanyon(id string) string {
	if !runs.ValidProject.MatchString(id) {
		id = strconv.Quote(id)
	}
	return id + " isn't on this Canyon yet. Deploy it first: canyon deploy"
}

// decodeJSON reads one JSON object of at most 1 MiB into v and refuses anything after it,
// answering the contract's 400 itself; false means the response is written.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "the body is not one JSON object of at most 1 MiB", nil)
		return false
	}
	if _, err := dec.Token(); err != io.EOF { //nolint:errorlint // sentinel from the decoder itself
		writeErr(w, http.StatusBadRequest, "bad_request", "trailing data after the JSON object", nil)
		return false
	}
	return true
}
