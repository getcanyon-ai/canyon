package main

import (
	"fmt"
	"strings"
)

// spdxExpr is a parsed SPDX licence expression (SPDX 2.3, Annex D): a licence id, or an AND or
// OR of expressions. A WITH exception is kept on its id, so only an exception listed whole matches.
type spdxExpr struct {
	op   string // "", "AND", "OR"
	id   string // when op is ""
	args []spdxExpr
}

func (e spdxExpr) satisfied(ok func(id string) bool) bool {
	switch e.op {
	case "AND":
		for _, a := range e.args {
			if !a.satisfied(ok) {
				return false
			}
		}
		return true
	case "OR":
		for _, a := range e.args {
			if a.satisfied(ok) {
				return true
			}
		}
		return false
	}
	return ok(e.id)
}

// parseSPDX parses s, where AND binds tighter than OR and parentheses group.
func parseSPDX(s string) (spdxExpr, error) {
	toks := strings.Fields(strings.NewReplacer("(", " ( ", ")", " ) ").Replace(s))
	p := &spdxParser{toks: toks}
	e, err := p.or()
	if err != nil {
		return spdxExpr{}, err
	}
	if p.i != len(toks) {
		return spdxExpr{}, fmt.Errorf("unexpected %q", toks[p.i])
	}
	return e, nil
}

type spdxParser struct {
	toks []string
	i    int
}

func (p *spdxParser) or() (spdxExpr, error) { return p.binary("OR", p.and) }

func (p *spdxParser) and() (spdxExpr, error) { return p.binary("AND", p.term) }

func (p *spdxParser) binary(op string, next func() (spdxExpr, error)) (spdxExpr, error) {
	e, err := next()
	if err != nil {
		return e, err
	}
	args := []spdxExpr{e}
	for p.i < len(p.toks) && p.toks[p.i] == op {
		p.i++
		e, err := next()
		if err != nil {
			return e, err
		}
		args = append(args, e)
	}
	if len(args) == 1 {
		return args[0], nil
	}
	return spdxExpr{op: op, args: args}, nil
}

func (p *spdxParser) term() (spdxExpr, error) {
	if p.i == len(p.toks) {
		return spdxExpr{}, fmt.Errorf("expression ends early")
	}
	t := p.toks[p.i]
	p.i++
	switch t {
	case "(":
		e, err := p.or()
		if err != nil {
			return e, err
		}
		if p.i == len(p.toks) || p.toks[p.i] != ")" {
			return spdxExpr{}, fmt.Errorf("unclosed (")
		}
		p.i++
		return e, nil
	case ")", "AND", "OR", "WITH":
		return spdxExpr{}, fmt.Errorf("unexpected %q", t)
	}
	if p.i+1 < len(p.toks) && p.toks[p.i] == "WITH" {
		t += " WITH " + p.toks[p.i+1]
		p.i += 2
	}
	return spdxExpr{id: t}, nil
}
