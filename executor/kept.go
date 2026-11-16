package executor

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// kept is what the executor keeps of a tool's stdout as it streams (contracts/executor.md, /scan): its
// SARIF with each result's region snippet and context region dropped — the attach builds every snippet
// from the package's own tree, and a minified line copied into each result is what passed the cap
// (#1684) — and, for a tool under ThenJSON, the NUL and, of its JSON output, its errors alone. What it
// keeps, and what it holds unread at once, are capped at maxScanOut (errOverCap); the stdout itself is
// bounded by the tool's deadline alone. A stdout that is not one SARIF document — cut short, or with
// anything after it — is kept as far as it parsed, so its reader answers not_sarif; the rest is read and
// discarded, so the tool never blocks on a full pipe. A read that fails is its error.
func kept(r io.Reader, jsonOut bool) ([]byte, error) {
	out, src := &capped{max: maxScanOut}, &source{r: r}
	dec := json.NewDecoder(src)
	dec.UseNumber()
	src.held = func() int64 { return src.read - dec.InputOffset() }
	err := walk(dec, out, "top")
	switch {
	case err == nil && jsonOut:
		err = errorsOf(dec.Buffered(), src, out)
	case err == nil:
		if _, end := dec.Token(); end != io.EOF { // a second value, or bytes after the first
			out.Reset()
		}
	}
	if errors.Is(err, errOverCap) {
		return nil, errOverCap
	}
	src.held = nil
	_, _ = io.Copy(io.Discard, src)
	if src.err != nil {
		return nil, src.err
	}
	return out.Bytes(), nil
}

// source is the stdout, counting what it has read and refusing to read on once a decoder holds more
// than maxScanOut of it unread — one result or string that long — and keeping its first error.
type source struct {
	r    io.Reader
	read int64
	held func() int64
	err  error
}

func (s *source) Read(p []byte) (int, error) {
	if s.held != nil && s.held() > maxScanOut {
		return 0, errOverCap
	}
	n, err := s.r.Read(p)
	s.read += int64(n)
	if err != nil && err != io.EOF && s.err == nil {
		s.err = err
	}
	return n, err
}

// capped is a buffer that refuses to grow past max.
type capped struct {
	bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if c.Len()+len(p) > c.max {
		return 0, errOverCap
	}
	return c.Buffer.Write(p)
}

// walk copies one JSON value from dec to w, decoding each result of each run one at a time and
// dropping its snippets; where is the value's place: "top", "runs", "run", "results", or "" anywhere
// else.
func walk(dec *json.Decoder, w io.Writer, where string) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		b, _ := json.Marshal(tok) // a string, json.Number, bool or nil, as it was
		_, err := w.Write(b)
		return err
	}
	open, end := string(delim), map[json.Delim]string{'{': "}", '[': "]"}[delim]
	if _, err := io.WriteString(w, open); err != nil {
		return err
	}
	for n := 0; dec.More(); n++ {
		if n > 0 {
			if _, err := io.WriteString(w, ","); err != nil {
				return err
			}
		}
		next := ""
		if delim == '{' {
			key, err := dec.Token()
			if err != nil {
				return err
			}
			b, _ := json.Marshal(key)
			if _, err := w.Write(append(b, ':')); err != nil {
				return err
			}
			switch {
			case where == "top" && key == "runs":
				next = "runs"
			case where == "run" && key == "results":
				next = "results"
			}
		} else {
			switch where {
			case "runs":
				next = "run"
			case "results":
				if err := result(dec, w); err != nil {
					return err
				}
				continue
			}
		}
		if err := walk(dec, w, next); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	_, err = io.WriteString(w, end)
	return err
}

// result copies one SARIF result without its locations' region snippets and context regions.
func result(dec *json.Decoder, w io.Writer) error {
	var res map[string]json.RawMessage
	if err := dec.Decode(&res); err != nil {
		return err
	}
	var locations []map[string]json.RawMessage
	if json.Unmarshal(res["locations"], &locations) == nil {
		for _, l := range locations {
			var physical map[string]json.RawMessage
			if json.Unmarshal(l["physicalLocation"], &physical) != nil {
				continue
			}
			delete(physical, "contextRegion")
			var region map[string]json.RawMessage
			if json.Unmarshal(physical["region"], &region) == nil && region != nil {
				delete(region, "snippet")
				physical["region"], _ = json.Marshal(region)
			}
			l["physicalLocation"], _ = json.Marshal(physical)
		}
		res["locations"], _ = json.Marshal(locations)
	}
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// errorsOf reads, after the SARIF — what its decoder had buffered, then src — the NUL and the JSON output
// ThenJSON prints, and writes the NUL and a document of the output's errors alone; the rest of it is read
// and dropped as it streams. No NUL, or output that is not one whole JSON object, writes nothing more,
// and the reader answers not_sarif.
func errorsOf(buffered io.Reader, src *source, w io.Writer) error {
	tail := &counted{r: io.MultiReader(buffered, src)}
	for b := make([]byte, 1); ; {
		if _, err := io.ReadFull(tail, b); err != nil {
			return cut(err)
		}
		if b[0] == 0 {
			break
		}
		if b[0] != ' ' && b[0] != '\n' && b[0] != '\r' && b[0] != '\t' {
			return nil
		}
	}
	dec, at := json.NewDecoder(tail), tail.n
	src.held = func() int64 { return tail.n - at - dec.InputOffset() }
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return cut(err)
	}
	errs := json.RawMessage(nil)
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return cut(err)
		}
		if key == "errors" {
			err = dec.Decode(&errs)
		} else {
			err = walk(dec, io.Discard, "")
		}
		if err != nil {
			return cut(err)
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return cut(err)
	}
	if _, err := dec.Token(); err != io.EOF { // nothing may follow the one document
		return cut(err)
	}
	doc := []byte("\x00{}")
	if errs != nil {
		doc = append(append([]byte("\x00{\"errors\":"), errs...), '}')
	}
	_, err := w.Write(doc)
	return err
}

// cut is a tail that ended or broke: the cap passes through, any other end is not_sarif.
func cut(err error) error {
	if errors.Is(err, errOverCap) {
		return err
	}
	return nil
}

// counted is a reader counting what it has read.
type counted struct {
	r io.Reader
	n int64
}

func (c *counted) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
