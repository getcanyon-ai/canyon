package cli

import (
	"context"
	"flag"
	"fmt"
	"strings"
)

// componentType is routes.md's `GET /api/v1/component-types` row, whole: the prose shape of one
// component_menu entry. The manifest's component_types[] is the same menu as bare Score names, for a
// workload file; this verb reads the shape with the notes and the status, never the manifest.
type componentType struct {
	Type   string   `json:"type"`
	Class  string   `json:"class"`
	Gives  []string `json:"gives"`
	Status string   `json:"status"`
	Notes  string   `json:"notes"`
}

type componentTypes struct {
	ComponentTypes []componentType `json:"component_types"`
}

func runResources(ctx context.Context, a *app, args []string) int {
	rest, err := a.parse(args, func(*flag.FlagSet) {})
	if err != nil {
		return a.usage(err)
	}
	if len(rest) > 0 {
		return a.usage(fmt.Errorf("resources takes no arguments, and got %q", rest[0]))
	}
	client, err := a.client()
	if err != nil {
		return a.usage(err)
	}
	var read componentTypes
	if err := client.get(ctx, "/api/v1/component-types", &read); err != nil {
		return a.fail(err)
	}
	a.renderResources(read.ComponentTypes)
	return exitOK // a read carries no verdict
}

// renderResources prints cli.md §6.5: one block per row in the host's order — the snapshot's, and a
// client that re-sorts sorts one answer — then one line that says whether this host can provision
// any of them. `status` is the host's field and nothing here re-derives it: a row that gives an env
// var is not thereby available, and a wrong "available" is a builder declaring a database that
// never arrives.
func (a *app) renderResources(rows []componentType) {
	a.printf("\n")
	if len(rows) == 0 {
		a.printf("  This host offers no component types\n")
		return
	}
	available := 0
	for _, row := range rows {
		if row.Status == "available" {
			available++
		}
		a.printf("  %s\n", a.render.paint(ansiBold, cell(row.Type)))
		a.resourceField("class", a.orElse(cell(row.Class)))
		gives := make([]string, len(row.Gives))
		for i, name := range row.Gives {
			gives[i] = cell(name)
		}
		a.resourceField("gives", a.orElse(strings.Join(gives, " ")))
		a.resourceField("status", a.resourceStatus(row.Status))
		a.resourceField("notes", a.orElse(cell(row.Notes)))
		a.printf("\n")
	}
	if available == 0 {
		for _, line := range wrap("Nothing on this host can be provisioned yet. Declaring one of these in an app is planned, not live.", proseWidth) {
			a.printf("  %s\n", line)
		}
		return
	}
	a.printf("  %d of %d available\n", available, len(rows))
}

// field is one labelled line of a block, the value wrapped under the label, as §3.3's fix and why.
func (a *app) resourceField(label, text string) {
	head := a.render.paint(ansiDim, label) + strings.Repeat(" ", 8-len(label))
	// indent 12: a full line ends where the findings' rule ends
	for _, line := range wrap(text, proseWidth-10) {
		a.printf("    %s%s\n", head, line)
		head = strings.Repeat(" ", 8)
	}
}

// status is the host's word behind the glyph the gate uses for the same fact: pass for a type this
// host provisions today, pending for one it does not yet. Any other word a newer host answers is
// printed as it came, spelled out, and invented into neither.
func (a *app) resourceStatus(s string) string {
	switch s {
	case "available":
		return a.render.paint(ansiGreen, a.render.glyph(statePass)) + " available"
	case "planned":
		return a.render.paint(ansiDim, a.render.glyph(statePending)) + " planned"
	}
	return a.render.paint(ansiDim, a.render.glyph(statePending)) + " " + cell(s)
}
