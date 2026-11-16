---
slug: rules/iac.misconfig
title: "Infrastructure config passes its benchmark checks"
description: "Terraform, CloudFormation, Kubernetes, Helm and Dockerfile settings a benchmark check flags, found by Checkov and Trivy."
---

`iac.misconfig` · Advisory by default · found by Checkov and Trivy

## What it finds

A setting in the app's infrastructure files that a benchmark check flags. Two tools report into this
rule:

- **Checkov**, over Terraform, CloudFormation and Dockerfiles by default. An admin can narrow the
  frameworks or skip checks. Every Checkov result lands here.
- **Trivy**, whose misconfiguration scan reads Kubernetes, Helm and Azure ARM files. Every result of
  a Trivy rule tagged `misconfiguration` lands here.

For example, this Terraform security group, open to the world on port 22, gets Checkov's
check `CKV_AWS_24`:

```hcl
resource "aws_security_group" "sg" {
  ingress {
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }
}
```

**Checkov's Dockerfile checks never block**, whatever this rule's state. Canyon builds every app
with buildpacks and never from a Dockerfile, so such a finding says so: "Canyon
builds this app with buildpacks and never builds from this Dockerfile, so this does not block; it
matters only where something else builds from it."

## Why it matters

The infrastructure the file describes is deployed with the gap the check names, such as a server
port anyone on the internet can reach.

## How to fix it

For a Checkov finding, Canyon prints Checkov's own guidance for the check. For a Trivy finding, it
prints:

> Set what the check names to the value it asks for, in the file at this line.

## Check the fix

`canyon check` runs the same review `canyon deploy` runs, without deploying.

## If you think it's wrong

Canyon doesn't read `checkov:skip` comments, a `.checkov.yaml`, or a resource's Checkov skip
metadata, so none of them silences it. To skip a check for every app, an admin lists it under
Guardrails → Scanners → Checkov → Configure.

If the code is right as it is, [have the finding approved](/build/fix-or-approve/#approve-it-instead).
An admin can set the rule to Off or Blocking under Guardrails → Policies. Set to Blocking, it stops a deploy for its high
and critical findings, except Checkov's Dockerfile checks.

Each tool runs only while its own rule, [Findings from checkov](/rules/sarif.checkov/)
(`sarif.checkov`) or [Findings from trivy](/rules/sarif.trivy/) (`sarif.trivy`), is Advisory or
Blocking, and Canyon runs it on your Canyon.
