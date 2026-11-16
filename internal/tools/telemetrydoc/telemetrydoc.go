// Package telemetrydoc writes TELEMETRY.md from contracts/schema/telemetry.schema.json (#2807): the page lists every
// field the daily report can carry, in the schema's order, so it can never drift from what is sent. Its test fails
// while the two differ; make telemetry-docs rewrites the page.
package telemetrydoc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

const head = `# Telemetry

A Canyon server sends one report of counts a day. This page lists every field of it. It is generated from
[contracts/schema/telemetry.schema.json](contracts/schema/telemetry.schema.json), the schema the report is
held to, so the two never differ.

## What is sent

| Field | Example | What it is |
|---|---|---|
`

const tail = `
## Never sent

Code, file names or paths, repo or app names, hostnames, domains, cluster names, user names, emails, IP
addresses, finding text, custom rule names or content, secrets, environment variables, AI review content. If a
field could identify a company or a person, it isn't sent. Custom rules are never named: they count only as
` + "`custom`" + ` in ` + "`rules_fired`" + `.

## Turning it off

- An admin runs ` + "`canyon telemetry off`" + `, or turns it off in Settings › Telemetry, where an admin also turns it back on.
- ` + "`CANYON_TELEMETRY=0`" + ` or ` + "`DO_NOT_TRACK=1`" + ` on the server, or ` + "`telemetry.enabled: false`" + ` in the Helm values.
- It is off by itself when ` + "`CI`" + ` is set, and when the server can't reach the collector at its first send.
- It's on by default. ` + "`canyon telemetry show`" + ` prints the next report exactly as it is sent.
- It goes once a day, over HTTPS, to ` + "`https://usage.myagentcontrol.ai/v1/report`" + `.

To get a new instance id, run ` + "`DELETE FROM telemetry;`" + ` on Canyon's database: the server makes a new one.
`

// Render is TELEMETRY.md for the schema: one table row per property, in the schema's own order.
func Render(schema []byte) (string, error) {
	var doc struct {
		Properties json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schema, &doc); err != nil {
		return "", err
	}
	dec := json.NewDecoder(bytes.NewReader(doc.Properties)) // the keys in the file's order, which a map would lose
	if _, err := dec.Token(); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(head)
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return "", err
		}
		var field struct {
			Description string            `json:"description"`
			Examples    []json.RawMessage `json:"examples"`
		}
		if err := dec.Decode(&field); err != nil {
			return "", err
		}
		if field.Description == "" || len(field.Examples) == 0 {
			return "", fmt.Errorf("%v: every field has a description and an example", key)
		}
		var example bytes.Buffer
		if err := json.Compact(&example, field.Examples[0]); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "| `%v` | `%s` | %s |\n", key, strings.ReplaceAll(example.String(), "|", `\|`), strings.ReplaceAll(field.Description, "|", `\|`))
	}
	b.WriteString(tail)
	return b.String(), nil
}
