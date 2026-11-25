# Every command AGENTS.md documents lives here; CI calls these same targets.
GOLANGCI_LINT_VERSION ?= v2.13.2
GOBIN ?= $(shell go env GOPATH)/bin
PKG ?= ./...
RUN ?= .
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
SARIF_SCHEMA_URL ?= https://json.schemastore.org/sarif-2.1.0.json

.PHONY: setup build builder-node cli cli-release images-release install-test lint test test-all test-rest test-shards test-affected test-bounds tidy-check demo demo-down demo-check demo-check-steps tour-check preflight vendor-sarif pack-fetch pack-install test-kube test-e2e test-cluster-unit e2e-cluster-egress e2e-up e2e-down e2e e2e-ui e2e-ui-typecheck e2e-handoff e2e-handoff-repo e2e-cluster-handoff-repo tokens tokens-check deploy-bundle-check third-party-notices third-party-notices-check images-load web web-dev web-test opengrep-rules-test vendor-sast-rules docs-links docs-site docs-cli telemetry-docs

setup: ## Go modules + the pinned linter
	go mod download
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

web/node_modules: web/package.json web/package-lock.json ## npm ci only when the lockfile moved
	cd web && npm ci
	@touch web/node_modules

web: web/node_modules ## the dashboard bundle into web/dist, which the Go build embeds
	( cd web && npm run build ); s=$$?; mkdir -p web/dist && touch web/dist/.gitkeep; exit $$s # vite empties dist, and //go:embed needs the marker back even when the build failed

web-dev: web/node_modules ## vite on :5173, proxying /api and /health to a canyon-server on 127.0.0.1:7777
	cd web && npm run dev

web-test: web/node_modules ## the dashboard's tests
	cd web && npm test

build: web cli ## bin/canyon-server (dashboard inside) and bin/canyon, both version stamped from git
	go build -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/canyon-server ./cmd/canyon-server

cli: ## bin/canyon on its own — the CLI embeds no dashboard, so it needs neither node nor the bundle
	go build -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/canyon ./cmd/canyon

CLI_PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

cli-release: ## bin/release/: the CLI per platform as canyon_<version>_<os>_<arch>.tar.gz (.zip on windows), each with LICENSE, NOTICE and THIRD_PARTY_NOTICES, plus SHA256SUMS — what the release workflow uploads
	rm -rf bin/release && mkdir -p bin/release
	set -e; for p in $(CLI_PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; name=canyon_$(VERSION)_$${os}_$${arch}; exe=canyon; [ $$os = windows ] && exe=canyon.exe; \
	  mkdir -p bin/release/$$name; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/release/$$name/$$exe ./cmd/canyon; \
	  cp LICENSE NOTICE THIRD_PARTY_NOTICES bin/release/$$name/; \
	  if [ $$os = windows ]; then (cd bin/release && python3 -m zipfile -c $$name.zip $$name); else tar --owner=0 --group=0 --numeric-owner -C bin/release -czf bin/release/$$name.tar.gz $$name; fi; \
	  rm -rf bin/release/$$name; \
	done
	cd bin/release && sha256sum canyon_* > SHA256SUMS
	sed 's/^version=dev$$/version=$(VERSION)/' cli/install.sh > bin/release/install.sh # the curl | sh installer, stamped with this release (cli.md §8.1)

# Canyon's nine images built from a Dockerfile, name:Dockerfile[:target], each built from the repository root: its five images on
# Kubernetes and its two scanner images (the Kubernetes install guide, docs/src/content/docs/self-host/kubernetes.md),
# and the laptop demo's Docker executor and sample apps, which canyon-deploy.tar.gz's compose override names
# (scripts/publish-release.sh).
IMAGES := canyon-server:Dockerfile canyon-executor-kube:Dockerfile.executor-kube canyon-identity:Dockerfile.identity \
  canyon-run:executor/run.Dockerfile canyon-build:executor/build.Dockerfile \
  canyon-opengrep:executor/opengrep.Dockerfile canyon-eslint:executor/eslint.Dockerfile \
  canyon-executor:Dockerfile.executor canyon-demo-apps:Dockerfile:demo-apps

# Each image's org.opencontainers.image.licenses: Canyon's licences on the images that are Canyon's (LICENSE:
# AGPL-3.0-only, with Apache-2.0 where the image holds a package under an Apache-2.0 LICENSE (cli/, contracts/,
# internal/answered/, ...) and 0BSD where it holds deploy/gitops/), and an SPDX expression for what is inside the ones that are mostly third-party software
# (NOTICE, Third-party tools inside Canyon's images). An image with none here builds nowhere.
# canyon-server links contracts/, serves the dashboard, which embeds deploy/gitops/, and embeds GitLab's sast-rules
# (gate/opengrep/sast-rules.tar: MIT, Apache-2.0 and LGPL-3.0 rules, NOTICE), and links the ee/ packages (Elastic-2.0,
# ee/LICENSE), the only binary that does; canyon-demo-apps holds
# the canyon CLI, Apache-2.0 throughout; the executors and canyon-identity link internal/answered (Apache-2.0), and
# canyon-executor adds the Docker CLI and pack (Apache-2.0).
IMAGE_LICENCE = case $$name in \
  (canyon-server) echo 'AGPL-3.0-only AND Apache-2.0 AND 0BSD AND MIT AND LGPL-3.0-only AND Elastic-2.0' ;; \
  (canyon-executor|canyon-executor-kube|canyon-identity|canyon-demo-apps) echo 'AGPL-3.0-only AND Apache-2.0' ;; \
  (canyon-opengrep) echo 'LGPL-2.1-only WITH OCaml-LGPL-linking-exception' ;; \
  (canyon-eslint) echo 'MIT AND Apache-2.0 AND BSD-2-Clause AND BSD-3-Clause AND ISC AND 0BSD AND BlueOak-1.0.0 AND Python-2.0 AND (MIT OR CC0-1.0) AND AGPL-3.0-only' ;; \
  (canyon-build|canyon-run) echo Apache-2.0 ;; \
  (*) echo "no licence label for $$name (the Makefile's IMAGE_LICENCE)" >&2; exit 1 ;; esac

# One image of IMAGES, $$img, built as the release builds it, up to its output: sets name and licence.
IMAGE_BUILD = name=$${img%%:*}; file=$${img\#*:}; target=; \
  case $$file in *:*) target="--target $${file\#*:}"; file=$${file%%:*} ;; esac; \
  licence=$$($(IMAGE_LICENCE)); \
  docker buildx build -f $$file $$target --build-arg VERSION=$(VERSION) --provenance=false $${GOPROXY:+--build-arg "GOPROXY=$$GOPROXY"} \
    --build-arg PACK_VERSION=$(PACK_VERSION) --build-arg PACK_SHA256_AMD64=$(PACK_SHA256_AMD64) --build-arg PACK_SHA256_ARM64=$(PACK_SHA256_ARM64) \
    --label org.opencontainers.image.source=$(SOURCE) --label "org.opencontainers.image.licenses=$$licence"

# images-release's push: release.yml's rehearsal sets false, building each image to its digest and pushing nothing.
PUSH ?= true
images-release: ## push each of the ten images for one PLATFORM (linux/amd64) to REPO/<name> by digest, untagged but for canyon-builder-node's VERSION-<arch>, writing bin/images/<name> = its digest — what the release workflow joins per architecture; needs a docker-container buildx builder and a login to REPO
	rm -rf bin/images && mkdir -p bin/images
	set -e; for img in $(IMAGES); do $(IMAGE_BUILD) --platform $(PLATFORM) \
	    --output type=image,name=$(REPO)/$$name,push-by-digest=true,name-canonical=true,push=$(PUSH) \
	    --metadata-file bin/images/$$name.json .; \
	  jq -er '."containerimage.digest"' bin/images/$$name.json > bin/images/$$name; rm bin/images/$$name.json; \
	done
	$(MAKE) --no-print-directory builder-node BUILDER_NODE=canyon-builder-node:release
	set -e; if [ "$(PUSH)" = true ]; then ref=$(REPO)/canyon-builder-node:$(VERSION)-$$(uname -m); \
	  docker tag canyon-builder-node:release $$ref && docker push -q $$ref >/dev/null; \
	  docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' $$ref | grep -m1 "^$(REPO)/canyon-builder-node@" | cut -d@ -f2 > bin/images/canyon-builder-node; \
	  [ -s bin/images/canyon-builder-node ] || { echo "$$ref was pushed but Docker names no digest for it in $(REPO)" >&2; exit 1; }; \
	else docker image inspect --format '{{.Id}}' canyon-builder-node:release > bin/images/canyon-builder-node; fi # pack makes it in the daemon: the one image pushed by a tag

images-load: ## build each of the ten images as images-release does, for this machine's platform, into the local Docker as canyon-load/<name>, and check what each carries: its licence label, LICENSE, NOTICE and THIRD_PARTY_NOTICES (this tree's) in Canyon's images, and ee/LICENSE (the Elastic License 2.0) in canyon-server, which links ee/, and Opengrep's licences and source pointer in canyon-opengrep; every image is checked, and every miss named, before it fails (CI's images job)
	mkdir -p bin/image-files
	set -e; bad=; for img in $(IMAGES); do $(IMAGE_BUILD) --load -t canyon-load/$$name .; \
	  got=$$(docker image inspect -f '{{index .Config.Labels "org.opencontainers.image.licenses"}}' canyon-load/$$name); \
	  [ "$$got" = "$$licence" ] || bad="$$bad\n  canyon-load/$$name is labelled [$$got], not [$$licence]"; \
	  case $$name in canyon-server) files="LICENSE NOTICE THIRD_PARTY_NOTICES ee/LICENSE"; dir=/usr/share/doc/canyon ;; \
	    canyon-executor|canyon-executor-kube|canyon-identity|canyon-demo-apps) files="LICENSE NOTICE THIRD_PARTY_NOTICES"; dir=/usr/share/doc/canyon ;; \
	    canyon-opengrep) files="LICENSE LICENSE.linking-exception SOURCE"; dir=/usr/share/licenses/opengrep ;; *) files= ;; esac; \
	  c=$$(docker create canyon-load/$$name); \
	  for f in $$files; do rm -f bin/image-files/$$f; mkdir -p bin/image-files/$$(dirname $$f); \
	    if ! docker cp $$c:$$dir/$$f bin/image-files/$$f >/dev/null 2>&1; then bad="$$bad\n  canyon-load/$$name has no $$dir/$$f"; \
	    elif [ $$name != canyon-opengrep ] && ! cmp -s bin/image-files/$$f $$f; then bad="$$bad\n  canyon-load/$$name's $$dir/$$f is not this tree's $$f"; fi; \
	  done; docker rm $$c >/dev/null; \
	  echo "canyon-load/$$name  licences=$$got  $$(for f in $$files; do printf '%s ' $$dir/$$f; done)"; \
	done; rm -rf bin/image-files; \
	$(MAKE) --no-print-directory builder-node BUILDER_NODE=canyon-load/canyon-builder-node; \
	got=$$(docker image inspect -f '{{index .Config.Labels "org.opencontainers.image.licenses"}}' canyon-load/canyon-builder-node); \
	[ "$$got" = Apache-2.0 ] || bad="$$bad\n  canyon-load/canyon-builder-node is labelled [$$got], not [Apache-2.0]"; \
	if [ -n "$$bad" ]; then printf "images-load:$$bad\n"; exit 1; fi; echo "every image carries its licence label and its notices"

# The bundle check's project and the images it boots, built from this tree.
BUNDLE_IMAGES := canyon-server canyon-executor canyon-demo-apps

deploy-bundle-check: ## canyon-deploy.tar.gz as scripts/publish-release.sh builds it (scripts/deploy-bundle.sh), from HEAD, with server, executor and demo-apps built from this tree in place of a release's, checked by scripts/release-archives-check.sh (licensed and clean) and scripts/deploy-bundle-check.sh: compose accepts it, nothing in it builds, every file it names is in it, and its stack comes up healthy, answers canyon fleet at localhost and refuses another Host 421, and the team setup the guide writes (its .env and Caddyfile) is complete (CI's deploy-bundle job; it starts the laptop demo's containers)
	rm -rf bin/bundle && mkdir -p bin/bundle
	docker compose --profile demo-apps build $${GOPROXY:+--build-arg "GOPROXY=$$GOPROXY"} server executor demo-apps
	set -e; p=$$(docker compose config --format json | jq -r .name); for i in $(BUNDLE_IMAGES); do docker tag $$p-$${i#canyon-} canyon-bundle/$$i:check; echo canyon-bundle/$$i:check; done > bin/bundle/images.txt
	$(MAKE) --no-print-directory builder-node BUILDER_NODE=canyon-bundle/canyon-builder-node:check && echo canyon-bundle/canyon-builder-node:check >> bin/bundle/images.txt
	scripts/deploy-bundle.sh HEAD bin/bundle/images.txt bin/bundle/canyon-deploy.tar.gz
	scripts/release-archives-check.sh bin/bundle
	scripts/deploy-bundle-check.sh bin/bundle/canyon-deploy.tar.gz docs/src/content/docs/self-host/docker.md

install-test: ## bin/release/install.sh against a local copy of this platform's release: the stamped version installs and runs, and a tampered SHA256SUMS installs nothing (CI runs this)
	$(MAKE) --no-print-directory cli-release VERSION=v0.0.0-installtest CLI_PLATFORMS=$$(go env GOOS)/$$(go env GOARCH)
	@d=$$(mktemp -d); trap 'rm -rf "$$d"' EXIT; \
	 mkdir -p "$$d/bin" && echo old > "$$d/stub" && ln -s "$$d/stub" "$$d/bin/canyon"; \
	 out=$$(CANYON_RELEASE_URL=file://$(CURDIR)/bin/release CANYON_INSTALL_DIR=$$d/bin PATH=/usr/bin:/bin sh bin/release/install.sh 2>&1) || { echo "$$out"; echo "install.sh failed"; exit 1; }; \
	 echo "$$out"; \
	 [ "$$("$$d/bin/canyon" --version)" = "canyon v0.0.0-installtest" ] || { echo "the installed canyon is not the stamped release"; exit 1; }; \
	 scripts/release-archives-check.sh bin/release || exit 1; \
	 [ "$$(cat "$$d/stub")" = old ] || { echo "install.sh wrote through the canyon it replaced, a symlink"; exit 1; }; \
	 echo "$$out" | grep -q "is not on your PATH" || { echo "no PATH hint for a directory off PATH"; exit 1; }; \
	 cp -R bin/release "$$d/bad"; awk '{ print "0000000000000000000000000000000000000000000000000000000000000000", $$2 }' bin/release/SHA256SUMS > "$$d/bad/SHA256SUMS"; \
	 if bad=$$(CANYON_RELEASE_URL=file://$$d/bad CANYON_INSTALL_DIR=$$d/bin2 PATH=/usr/bin:/bin sh bin/release/install.sh 2>&1); then echo "install.sh installed from a release whose SHA256SUMS does not match"; exit 1; fi; \
	 echo "$$bad" | grep -q "SHA256SUMS says" || { echo "install.sh failed, but not on the checksum: $$bad"; exit 1; }; \
	 [ ! -e "$$d/bin2/canyon" ] || { echo "a refused install left a canyon behind"; exit 1; }; \
	 echo "install.sh replaces a canyon in place, installs the stamped release and refuses a mismatched checksum"

lint: ## golangci-lint (includes gofmt + goimports)
	$(GOBIN)/golangci-lint run ./...

test: ## one package / one test: make test PKG=./cmd/canyon-server RUN=TestServer, COUNT=20 to run it 20 times
	go test -race -run '$(RUN)' $(if $(COUNT),-count=$(COUNT)) $(PKG)

test-all: ## the full suite: every package but the sharded ones, then each sharded one in N shards at once
	$(MAKE) --no-print-directory test-rest
	$(MAKE) --no-print-directory test-shards

# the packages too slow for one go test process, whose tests test-shards runs split by name: the server's and runs.
# A shard's -timeout is 25m: its longest takes 482 s, and a runner 2.65 times slow would take it 21 minutes (#124)
SHARDED := github.com/getcanyon-ai/canyon/cmd/canyon-server github.com/getcanyon-ai/canyon/runs
# the build flags every test target below runs with, and finds its packages with
RACE := -race
N ?= 2
BASE ?= origin/main

test-rest: ## every package but the sharded ones, whose tests test-shards runs
	pkgs=$$(go list $(RACE) ./...) || exit 1; rest=$$(printf '%s\n' $$pkgs | grep -vxF $(addprefix -e ,$(SHARDED))) || exit 1; go test $(RACE) $$rest

test-shards: ## each sharded package's tests (SHARDED: the server's, runs) split by name into N (2) go test processes run at once, or SHARD_PKG=<package> SHARD=i/n the i-th of n of one package alone (CI's matrices: the server's 4, runs 3); every test a package lists must be in exactly one shard before any runs, and each shard must run exactly its own tests (internal/tools/testsel)
	set -e; for p in $(or $(SHARD_PKG),$(SHARDED)); do go run ./internal/tools/testsel shards $(if $(SHARD),-shard $(SHARD),-n $(N)) $$p $(RACE) -timeout 25m; done

test-affected: ## only the packages a change since BASE's merge base (origin/main) can affect, from Go's own graph under -race (internal/tools/testsel), plus the declared readers of a changed directory's Go source; every package runs when a changed file is not Go source (go.mod, go.sum, this Makefile, db/migrations, testdata, goldens, docs, contracts) or is Go source no package builds (a build tag, a testdata app). The test cache stays on; a sharded package runs in N shards
	@pkgs=$$(go run ./internal/tools/testsel affected -base $(BASE) -- $(RACE)) || exit 1; \
	 [ -n "$$pkgs" ] || { echo "no package affected"; exit 0; }; \
	 rest=$$(printf '%s\n' $$pkgs | grep -vxF $(addprefix -e ,$(SHARDED))); \
	 if [ -n "$$rest" ]; then go test $(RACE) $$rest || exit 1; fi; \
	 for p in $(SHARDED); do if printf '%s\n' $$pkgs | grep -qxF $$p; then $(MAKE) --no-print-directory test-shards SHARD_PKG=$$p || exit 1; fi; done

test-bounds: ## custom rules' cost bounds, measured at the facts' largest (gate/custom, internal/tools/ruleeval): without -race, which makes CEL's cost tracker some fifteen times slower and its allocations no measure, so test-rest skips them (#2638)
	go test -v -run 'AtTheBounds' ./gate/custom ./internal/tools/ruleeval

tokens: ## regenerate web/src/tokens.css and docs/src/styles/tokens.css from designsystem/canyon/dashboard.tokens.json
	python3 designsystem/canyon/build-css.py

tokens-check: ## web/src/tokens.css is generated, not typed — rendered in memory and diffed, the tree is never touched
	python3 designsystem/canyon/build-css.py --check

third-party-notices: web/node_modules ## regenerate THIRD_PARTY_NOTICES: the licence texts of every Go module the release binaries link (per platform they ship for), of the dashboard's npm packages and of the third-party files vendored here (internal/tools/thirdparty); fails on a component with no licence file, or a licence outside AGENTS.md's list and the tool's exceptions
	go run ./internal/tools/thirdparty -cli-platforms '$(CLI_PLATFORMS)' THIRD_PARTY_NOTICES

third-party-notices-check: web/node_modules ## THIRD_PARTY_NOTICES is generated, not typed — rendered in memory and compared, the tree is never touched (CI's test-rest)
	go run ./internal/tools/thirdparty -check -cli-platforms '$(CLI_PLATFORMS)' THIRD_PARTY_NOTICES

docs-links: ## every relative link, anchor and image in the Markdown outside design/ and the docs site's pages resolves in this tree: lychee, offline (CI's docs workflow)
	lychee --offline --include-fragments --no-progress $$(git ls-files '*.md' ':!design/' ':!docs/src/')

docs-site: ## the docs site into docs/dist (docs/README.md); the build fails on a link to a page or heading that doesn't exist
	cd docs && npm ci && npm run build

docs-cli: ## rewrite the docs site's CLI reference from every command's --help; go test fails while it differs (cli/reference_test.go)
	CANYON_UPDATE_DOCS=1 go test ./cli -run TestTheCLIReferenceIsTheHelp -count=1

telemetry-docs: ## rewrite TELEMETRY.md from contracts/schema/telemetry.schema.json; go test fails while it differs (internal/tools/telemetrydoc)
	CANYON_UPDATE_DOCS=1 go test ./internal/tools/telemetrydoc -run TestTelemetryMDIsTheSchema -count=1

docs-screens: ## retake the admin guides' dashboard screenshots (docs/public/screens) from the laptop demo running at localhost:7777 (docs/README.md)
	cd e2e/ui && npm ci --silent && npx playwright install chromium
	cd docs && npm ci --silent && node scripts/screens.mjs

preflight: ## most of what CI runs, on one machine, to debug a red CI check (a push runs the checks it calls for on CI): the dashboard's tests and bundle, vet, lint, the race tests three times over (a race shows up in repetition, not in one run), tidy, the generated tokens, compose config, and the real compose boot.
	$(MAKE) web-test
	$(MAKE) web
	go vet ./...
	$(MAKE) lint
	go test -race -count=3 ./...
	$(MAKE) tidy-check
	$(MAKE) tokens-check
	docker compose config -q
	$(MAKE) demo-check


tidy-check: ## go.mod/go.sum are tidy — prints the diff and fails if not, touches nothing
	go mod tidy -diff

demo: ## the README's demo: the stack and its four sample apps (the demo-apps profile); refused, with the fix, when another program holds 127.0.0.1:7777
	@$(PORT_FREE)
	docker compose --profile demo-apps up --build

# The dashboard's port is the one compose publishes, 127.0.0.1:7777. Held by any program but this stack's own server, the
# demo stops here and names the fix (design/EXPERIENCE.md: compose fails loudly with the fix if the port is taken; #717):
# Docker's own error does not say it, and under Colima the port is forwarded or not without a word. curl exits 7 when
# nothing accepts the connection, asked past any proxy the shell names; without curl the check is skipped.
PORT_FREE = [ -n "$$(docker ps -q --filter name=^canyon-server$$ 2>/dev/null)" ] || ! command -v curl >/dev/null || \
	{ curl -q --noproxy '*' -s -o /dev/null --max-time 2 http://127.0.0.1:7777/; [ $$? -eq 7 ]; } || \
	{ echo "Port 7777 on 127.0.0.1 is in use by another program, and Canyon's dashboard needs it."; \
	  echo "Fix: stop that program, then run make demo again. To see which program it is: lsof -nP -iTCP:7777 -sTCP:LISTEN"; exit 1; }

demo-down: ## the demo gone: the stack with its volumes, first, so nothing starts another app; then every Canyon app container and network on this Docker host, found by the executor's canyon.app label (contracts/executor.md) and nothing it did not label; a network goes once nothing is on it; then the build holders, scans and canyon-builds the executor labels canyon.build and canyon.scan; then the apps' build caches and the scanners' databases no container uses, named canyon-cache-<app>-build and -launch (contracts/executor.md, /forget) and canyon-scandb-<tool>, gigabytes of them (#2697). Images stay: canyon-builder alone is a 1.3 GB download to fetch again
	-@f=$$(docker ps -aq --filter label=canyon.check-forward); [ -z "$$f" ] || { docker rm -f $$f >/dev/null; $(DEMO_PUBLIC_DEL); } # a demo-check that was killed: its forwards, and its stand-in address with them; nothing otherwise
	docker compose --profile demo-apps down -v
	@c=$$(docker ps -aq --filter label=canyon.app); [ -z "$$c" ] || docker rm -f $$c >/dev/null
	@n=$$(docker network ls -q --filter label=canyon.app); [ -z "$$n" ] || docker network rm $$n >/dev/null
	@c=$$(docker ps -aq --filter label=canyon.build; docker ps -aq --filter label=canyon.scan); [ -z "$$c" ] || docker rm -f $$c >/dev/null
	@n=$$(docker network ls -q --filter label=canyon.build); [ -z "$$n" ] || docker network rm $$n >/dev/null
	@v=$$(docker volume ls -q --filter dangling=true --filter name=^canyon-cache- --filter name=^canyon-scandb-); [ -z "$$v" ] || docker volume rm $$v >/dev/null

tour-check: cli ## the docs' Take the tour on the laptop demo built from this tree: the commands canyon demo tour has an agent run, chapter by chapter, as the agent reads them, and every output its page pastes held to the run (scripts/tour-check.sh; the tour workflow; it starts the demo's containers and removes them)
	@$(PORT_FREE)
	scripts/tour-check.sh

# Invariant 11's stand-ins: two apps the executor runs from a busybox httpd on :3000 (the executor's e2e pin),
# and their tags, which demo-down leaves as it leaves every app image.
DEMO_APP_BASE := busybox@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e
DEMO_DOWN = f=$$(docker ps -aq --filter label=canyon.check-forward); [ -z "$$f" ] || docker rm -f $$f >/dev/null 2>&1; $(DEMO_PUBLIC_DEL); \
	$(MAKE) --no-print-directory demo-down; docker image rm canyon/appnet-a:check canyon/appnet-b:check >/dev/null 2>&1
# The host's own forward to its loopback publish, which Docker Desktop, OrbStack and Colima give host.docker.internal:
# socat 1.8.0.3's multi-arch index, pinned by digest, standing in for it on the Docker host's own addresses (#2503).
DEMO_SOCAT := alpine/socat@sha256:beb4a68d9e4fe6b0f21ea774a0fde6c31f580dde6368939ed70100c5385b015e
# A public address of the Docker host's own, as a server with a public IP on its interface has: TEST-NET-2's, on the
# host's loopback for the check alone, in place before the apps start and gone with the check (#2503's review).
DEMO_PUBLIC := 198.51.100.7
DEMO_PUBLIC_DEL = docker run --rm --network host --cap-add NET_ADMIN $(DEMO_APP_BASE) ip addr del $(DEMO_PUBLIC)/32 dev lo >/dev/null 2>&1

demo-check: ## the boot path end to end: compose up postgres, server and executor, wait for /ready, check the image really carries the dashboard and the demo roster, prove invariants 11 and 13 with two apps the executor runs, tear down (CI runs this); Ctrl-C or any failed step tears down too
	@trap '$(DEMO_DOWN); exit 130' INT TERM; $(MAKE) --no-print-directory demo-check-steps || { $(DEMO_DOWN); exit 1; }

demo-check-steps:
	docker compose -f docker-compose.yml -f e2e/compose.telemetry-off.yml build $${GOPROXY:+--build-arg "GOPROXY=$$GOPROXY"} server executor
	docker compose -f docker-compose.yml -f e2e/compose.telemetry-off.yml up -d postgres server executor
	@for i in $$(seq 1 45); do curl -sf http://127.0.0.1:7777/ready && echo && exit 0; sleep 2; done; \
	 echo "server never became ready"; docker compose logs --tail=60 postgres server; $(DEMO_DOWN); exit 1
	scripts/telemetry-off-check.sh docker # the server sends no usage report: the runner's CI never reaches it
	curl -sf http://127.0.0.1:7777/ | grep -q 'id="root"' || { echo "the image serves no dashboard at /"; $(DEMO_DOWN); exit 1; }
	@fatal=$$(docker compose logs app-postgres | grep FATAL); [ -z "$$fatal" ] || { echo "app-postgres logged FATAL before anyone but its healthcheck connected (#2019):"; echo "$$fatal" | tail -3; $(DEMO_DOWN); exit 1; }; \
	 echo "app-postgres's log has no FATAL line" # before the probes below, whose refused logins are FATALs by design
	@roster=$$(curl -sf http://127.0.0.1:7777/api/v1/fleet); for app in commission-calc claims-portal quote-engine claims-archive fnol-intake endorsement-hub premium-ledger; do \
	   echo "$$roster" | grep -q "\"id\":\"$$app\"" || { echo "the demo roster is missing $$app: the seed did not run, or this volume already held a project"; docker compose logs --tail=60 server; $(DEMO_DOWN); exit 1; }; \
	 done; echo "the demo roster serves all seven apps" # anonymous sessions are granted on first contact, so no cookie
	@published=$$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' canyon-server | sed -n 's/^CANYON_PUBLISHED_ADDR=//p'); port=$$(docker port canyon-server 7777/tcp); \
	 [ "$$published" = "$$port" ] || { echo "CANYON_PUBLISHED_ADDR is [$$published], but docker publishes the server on [$$port]: the banner would describe a port the host does not have"; $(DEMO_DOWN); exit 1; }; \
	 curl -sf http://127.0.0.1:7777/api/v1/fleet | grep -q '"banner":"loopback"' || { echo "the fleet's banner is not loopback on a server published on $$port"; $(DEMO_DOWN); exit 1; }; \
	 echo "the server is published on $$port, as CANYON_PUBLISHED_ADDR says, and its banner reads loopback"
	@docker pull -q curlimages/curl:8.11.1 >/dev/null # pulled on its own line: a pull that fails must fail the check, never pass as "refused"
	@$(DEMO_PUBLIC_DEL); docker run --rm --network host --cap-add NET_ADMIN $(DEMO_APP_BASE) ip addr add $(DEMO_PUBLIC)/32 dev lo || { echo "$(DEMO_PUBLIC) could not be put on the Docker host's loopback"; $(DEMO_DOWN); exit 1; } # before the apps start, whose floors take the host's addresses then
	@id=$$(printf 'FROM $(DEMO_APP_BASE)\nCMD ["httpd", "-f", "-p", "3000"]\n' | docker build -q -t canyon/appnet-a:check -t canyon/appnet-b:check -) || { $(DEMO_DOWN); exit 1; }; \
	 for app in appnet-a appnet-b; do \
	   docker run --rm --network canyon-internal curlimages/curl:8.11.1 -sS --fail-with-body -m 60 -H 'content-type: application/json' \
	     -d "{\"app\":\"$$app\",\"target\":\"preview\",\"image\":\"canyon/$$app@$$id\",\"env\":{},\"network\":\"canyon-app-$$app\"}" http://executor:7778/run \
	   && echo || { echo "the executor did not run $$app"; docker compose logs --tail=30 executor; $(DEMO_DOWN); exit 1; }; \
	 done
	@b=$$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' $$(docker inspect -f '{{index .Config.Labels "canyon.floor"}}' appnet-b-preview)); \
	 [ -n "$$b" ] || { echo "appnet-b-preview runs in no floor holder with an address (contracts/executor.md, An app reaches no private, link-local or host address)"; $(DEMO_DOWN); exit 1; }; \
	 docker run --rm --network container:appnet-a-preview curlimages/curl:8.11.1 -sS -m 5 -o /dev/null http://$$b:3000/; code=$$?; \
	 if [ $$code -eq 0 ]; then echo "app A reached app B's container at $$b: two apps share a network"; $(DEMO_DOWN); exit 1; fi; \
	 if [ $$code -ne 7 ] && [ $$code -ne 28 ]; then echo "the probe of app B failed for a reason other than no route to it (curl exit $$code)"; $(DEMO_DOWN); exit 1; fi; \
	 echo "app A's network cannot reach app B's container" # by address: the name does not even resolve there
	@for app in appnet-a appnet-b; do docker run --rm --network container:$$app-preview curlimages/curl:8.11.1 -sS -m 5 http://canyon-server:7777/api/v1/fleet; code=$$?; \
	 if [ $$code -eq 0 ]; then echo; echo "$$app reached the control plane: CANYON_APPS_SUBNET is unset or is not the executor's CANYON_APPS_POOL, and sign-in off served a peer off this machine"; $(DEMO_DOWN); exit 1; fi; \
	 if [ $$code -ne 52 ] && [ $$code -ne 56 ]; then echo "the in-network curl failed for a reason other than a closed connection (curl exit $$code)"; $(DEMO_DOWN); exit 1; fi; \
	 done; echo "neither app's network can reach the control plane" # each on its own /24; the same curl image executor/e2e_test.go pins; 52/56 is the connection closed at accept, and nothing else counts
	@gw=$$(docker network inspect -f '{{range .IPAM.Config}}{{.Gateway}}{{end}}' canyon-app-appnet-a); \
	 docker run --rm --network container:appnet-a-preview curlimages/curl:8.11.1 -sS -m 5 -o /dev/null http://$$gw:7777/; code=$$?; \
	 if [ $$code -ne 7 ] && [ $$code -ne 28 ]; then echo "an app reached the host's published port through its own gateway (curl exit $$code): publish 7777 on 127.0.0.1 alone"; $(DEMO_DOWN); exit 1; fi; \
	 echo "an app's own gateway does not lead to the published port" # the guard serves each /24's gateway: only the 127.0.0.1 publish keeps the host's proxy off them
	@docker pull -q $(DEMO_SOCAT) >/dev/null # on its own line, as curl's: a pull that fails must fail the check
	@hg=$$(docker run --rm --add-host host.docker.internal:host-gateway $(DEMO_APP_BASE) awk '$$2 == "host.docker.internal" { print $$1; exit }' /etc/hosts); \
	 gw=$$(docker network inspect -f '{{range .IPAM.Config}}{{.Gateway}}{{end}}' canyon-app-appnet-a); \
	 [ -n "$$hg" ] && [ -n "$$gw" ] || { echo "no host-gateway address [$$hg] or gateway of canyon-app-appnet-a [$$gw] to probe"; $(DEMO_DOWN); exit 1; }; \
	 local=$$(docker run --rm --network host $(DEMO_APP_BASE) ip addr show) || { echo "the Docker host's own addresses could not be listed"; $(DEMO_DOWN); exit 1; }; reached=; \
	 for addr in $$hg $$gw $(DEMO_PUBLIC); do \
	   if echo "$$local" | grep -q " inet $$addr/"; then \
	     docker run -d --name canyon-check-forward-$$addr --label canyon.check-forward --network host $(DEMO_SOCAT) TCP-LISTEN:7777,bind=$$addr,fork,reuseaddr TCP:127.0.0.1:7777 >/dev/null || { echo "the host's forward on $$addr:7777 did not start"; $(DEMO_DOWN); exit 1; }; \
	     for i in $$(seq 1 15); do docker run --rm --network host curlimages/curl:8.11.1 -sf -o /dev/null -H 'Host: localhost' http://$$addr:7777/api/v1/fleet && break; sleep 1; done; \
	     docker run --rm --network host curlimages/curl:8.11.1 -sf -o /dev/null -H 'Host: localhost' http://$$addr:7777/api/v1/fleet || { echo "the host's forward on $$addr:7777 does not serve the host as an admin: refusing an app there would prove nothing"; $(DEMO_DOWN); exit 1; }; \
	   fi; \
	   docker run --rm --network container:appnet-a-preview curlimages/curl:8.11.1 -sS -m 5 -o /dev/null -H 'Host: localhost' http://$$addr:7777/api/v1/fleet; code=$$?; \
	   if [ $$code -ne 7 ] && [ $$code -ne 28 ]; then echo "app A reached the host at $$addr:7777, which loops to the demo's loopback publish (curl exit $$code): served as this machine, an app is an admin (#2503)"; reached="$$reached $$addr"; fi; \
	 done; f=$$(docker ps -aq --filter label=canyon.check-forward); [ -z "$$f" ] || docker rm -f $$f >/dev/null; \
	 [ -z "$$reached" ] || { $(DEMO_DOWN); exit 1; }; \
	 echo "app A reaches no address of the host's, its gateway, host-gateway ($$hg) and a public one ($(DEMO_PUBLIC)) included, even where the host forwards one to the demo's loopback publish" # Docker Desktop, OrbStack and Colima forward host.docker.internal to the Mac's own 127.0.0.1:7777; here socat stands in for that forward on any address of the Docker host's
	@reached=; for net in canyon-state canyon-internal canyon-provisioning canyon-egress-ctl; do ip=$$(docker inspect -f '{{(index .NetworkSettings.Networks "'$$net'").IPAddress}}' canyon-server); \
	   [ -n "$$ip" ] || { echo "canyon-server has no address on $$net"; $(DEMO_DOWN); exit 1; }; \
	   docker run --rm --network container:appnet-a-preview curlimages/curl:8.11.1 -sS -m 5 -o /dev/null http://$$ip:7777/ready; code=$$?; \
	   if [ $$code -ne 7 ] && [ $$code -ne 28 ]; then echo "app A connected to the server's $$net address $$ip (curl exit $$code): only Docker's isolation between networks keeps an app off $$net's gateway, which the demo serves (#2491)"; reached="$$reached $$net"; fi; \
	 done; [ -z "$$reached" ] || { $(DEMO_DOWN); exit 1; }; \
	 echo "app A cannot connect to the server's address on canyon-state, canyon-internal, canyon-provisioning or canyon-egress-ctl" # every network the server is on but the apps': 52/56 would be the peer rule closing it, the packet there, masqueraded from that network's gateway
	@docker run --rm --network container:appnet-a-preview curlimages/curl:8.11.1 -sSf -m 5 http://canyon-server:7780/.well-known/jwks.json | grep -q '"keys"' || { echo "app A cannot fetch the identity key set from canyon-server:7780"; $(DEMO_DOWN); exit 1; }; \
	 code=$$(docker run --rm --network container:appnet-a-preview curlimages/curl:8.11.1 -s -m 5 -o /dev/null -w '%{http_code}' http://canyon-server:7780/api/v1/fleet); \
	 [ "$$code" = 404 ] || { echo "the key-set listener answered /api/v1/fleet with $$code, not 404"; $(DEMO_DOWN); exit 1; }; \
	 echo "app A fetches the key set from canyon-server:7780, and nothing else is served there"
	@out=$$(docker run --rm --network container:appnet-a-preview -e PGCONNECT_TIMEOUT=5 postgres:16-alpine psql postgres://canyon:canyon@app-postgres/canyon -c 'select 1' 2>&1); code=$$?; \
	 if [ $$code -eq 0 ]; then echo "an app container logged into app-postgres as its admin: its pg_hba or the server's pinned address is wrong"; $(DEMO_DOWN); exit 1; fi; \
	 echo "$$out" | grep -q 'pg_hba.conf rejects' && echo "app A's network cannot log into app-postgres as its admin" || { echo "psql failed for a reason other than pg_hba refusing it (a DNS or timeout failure proves nothing): $$out"; $(DEMO_DOWN); exit 1; }
	@for app in appnet-a appnet-b; do docker run --rm --network container:canyon-server curlimages/curl:8.11.1 -sS -m 5 -o /dev/null http://$$app-preview:3000/ || { echo "the server cannot reach $$app"; $(DEMO_DOWN); exit 1; }; done; \
	 docker run --rm --network container:canyon-server -e PGCONNECT_TIMEOUT=5 postgres:16-alpine psql -tA -c 'select 1' postgres://canyon:canyon@app-postgres/canyon >/dev/null || { echo "the server's admin login no longer reaches app-postgres from its pinned address"; $(DEMO_DOWN); exit 1; }; \
	 curl -sf -o /dev/null http://127.0.0.1:7777/ready || { echo "the host no longer reaches the server once it is on an app network"; $(DEMO_DOWN); exit 1; }; \
	 echo "the server reaches both apps and app-postgres's admin login, and the host still reaches the server"
	@docker run --rm --network canyon-internal curlimages/curl:8.11.1 -sS -m 5 -o /dev/null http://canyon-server:7777/ready; code=$$?; \
	 if [ $$code -ne 52 ] && [ $$code -ne 56 ]; then echo "a container on canyon-internal, neither loopback nor its gateway, got curl exit $$code from the server, not a close at accept: under sign-in off it would be an admin (#2449)"; $(DEMO_DOWN); exit 1; fi; \
	 echo "the server closes a peer off this machine at accept, and serves the host through its network's gateway" # a container keeps its own address, as a client Docker forwards with DNAT does
	@for app in appnet-a appnet-b; do docker run --rm --network canyon-internal curlimages/curl:8.11.1 -sS --fail-with-body -m 30 -H 'content-type: application/json' -d "{\"app\":\"$$app\",\"target\":\"preview\"}" http://executor:7778/stop \
	   && ! docker network inspect canyon-app-$$app >/dev/null 2>&1 || { echo "/stop of $$app's last container left its network"; $(DEMO_DOWN); exit 1; }; done; \
	 echo "/stop removes each app's network with its last container"
	@nets=$$(docker inspect -f '{{range $$k, $$v := .NetworkSettings.Networks}}{{$$k}} {{end}}' $$(docker compose ps -q postgres)); \
	 if [ "$$nets" != "canyon-state " ]; then echo "Canyon's Postgres is on [$$nets], not canyon-state alone: the executor could reach it"; $(DEMO_DOWN); exit 1; fi; \
	 echo "Canyon's Postgres is on canyon-state alone"
	@id=$$(docker image inspect -f '{{.Id}}' canyon/appnet-a:check); docker run --rm --network canyon-internal curlimages/curl:8.11.1 -sS --fail-with-body -m 60 -H 'content-type: application/json' \
	   -d "{\"app\":\"appnet-a\",\"target\":\"preview\",\"image\":\"canyon/appnet-a@$$id\",\"env\":{},\"network\":\"canyon-app-appnet-a\"}" http://executor:7778/run >/dev/null || { echo "the executor did not run appnet-a again"; $(DEMO_DOWN); exit 1; }; \
	 [ "$$(docker inspect -f '{{index .Config.Labels "canyon.app"}} {{index .Config.Labels "canyon.target"}}' appnet-a-preview)" = "appnet-a preview" ] && [ "$$(docker network inspect -f '{{index .Labels "canyon.app"}}' canyon-app-appnet-a)" = appnet-a ] \
	 || { echo "the app's container or network does not carry its canyon.app label"; $(DEMO_DOWN); exit 1; }; \
	 echo "a running app's container and network carry canyon.app, which demo-down finds"
	@o=$$(mktemp); printf 'services:\n  server:\n    ports: ["0.0.0.0:7779:7777"]\n' > $$o; \
	 docker compose -f docker-compose.yml -f e2e/compose.telemetry-off.yml -f $$o up -d --no-deps server >/dev/null 2>&1; up=$$?; rm -f $$o; \
	 [ $$up -eq 0 ] || { echo "the server did not start again with a throwaway publish on 0.0.0.0:7779"; $(DEMO_DOWN); exit 1; }; \
	 for i in $$(seq 1 45); do [ "$$(docker inspect -f '{{.State.Status}}' canyon-server)" = exited ] && break; sleep 2; done; \
	 docker compose logs server | grep -q 'CANYON_AUTH=demo serves this machine alone, but Docker publishes its port 7777/tcp on 0.0.0.0 (host port 7779)' \
	 && [ "$$(docker inspect -f '{{.State.Status}}' canyon-server)" = exited ] \
	 || { echo "published on 0.0.0.0:7779 beside 127.0.0.1:7777, the server did not stop naming the binding (#2491):"; docker compose logs --tail=20 server; $(DEMO_DOWN); exit 1; }; \
	 echo "published beyond loopback, the demo server stops and names the binding" # the shipped 127.0.0.1 publish served every step above
	@$(DEMO_PUBLIC_DEL) || { echo "$(DEMO_PUBLIC) could not be taken off the Docker host's loopback"; exit 1; }
	$(MAKE) --no-print-directory demo-down
	docker image rm canyon/appnet-a:check canyon/appnet-b:check >/dev/null
	@left=$$(docker ps -aq --filter label=canyon.app; docker network ls -q --filter label=canyon.app; docker ps -aq --filter label=canyon.check-forward); [ -z "$$left" ] || { echo "make demo-down left app containers or networks, or demo-check its forwards, behind: $$left"; exit 1; }; \
	 ! docker run --rm --network host $(DEMO_APP_BASE) ip addr show dev lo | grep -q " inet $(DEMO_PUBLIC)/" || { echo "demo-check left $(DEMO_PUBLIC) on the Docker host's loopback"; exit 1; }; \
	 echo "make demo-down left no app container or network behind, and demo-check no forward or stand-in address"


vendor-sarif: ## re-fetch contracts/vendor/sarif-2.1.0.schema.json — a diff after means upstream moved
	curl -sSfL $(SARIF_SCHEMA_URL) | python3 -c "import json,sys; print(json.dumps(json.load(sys.stdin), separators=(',', ':'), ensure_ascii=False))" > $@.tmp
	mv $@.tmp contracts/vendor/sarif-2.1.0.schema.json

PACK_VERSION := 0.40.3
PACK_SHA256_AMD64 := 2f18dd0e63a181af411e648ed7467f4f081dc458ffacbd11644d5b911fc063ce
PACK_SHA256_ARM64 := d84fbb295e2b11775616c22c2f3ada562141fe3c437298775e7b8892ff43c846

PACK_URL ?= https://github.com/buildpacks/pack/releases/download/v$(PACK_VERSION)

# The lifecycle canyon-builder-node carries: the pinned canyon-builder's own (executor/builder.Dockerfile), from its
# release, each architecture's tarball held to its sha256 (#174). A bump of the builder that moves its lifecycle moves these.
LIFECYCLE_VERSION := 0.21.29
LIFECYCLE_SHA256_AMD64 := d261ccc62f8b00f4c63d7d137b371f369eb706e3ab6aedb3ff67eda4aa7d1ac2
LIFECYCLE_SHA256_ARM64 := 1237e4314158511f5435a34e3c74be6d4959da7e9b65c3067af1246182984212
BUILDER_NODE ?= canyon-builder-node:local

builder-node: ## canyon-builder-node for this machine's platform into the local Docker as BUILDER_NODE (scripts/builder-node.sh): canyon-build, the pinned nodejs buildpack and lifecycle, with canyon-run:noble and Canyon's two build variables its own; needs pack (make pack-install)
	LIFECYCLE_VERSION=$(LIFECYCLE_VERSION) LIFECYCLE_SHA256_AMD64=$(LIFECYCLE_SHA256_AMD64) LIFECYCLE_SHA256_ARM64=$(LIFECYCLE_SHA256_ARM64) SOURCE=$(SOURCE) scripts/builder-node.sh $(BUILDER_NODE)
PACK_TGZ ?= /tmp/pack.tgz

pack-fetch: ## the pinned pack release for this machine into $(PACK_TGZ), its sha256 checked, by .github/fetch.sh: a failed download, a GitHub 504 among them, is tried again with backoff, and GH_TOKEN goes to github.com alone (#2755)
	@arch=$$(uname -m); case $$arch in x86_64) f=pack-v$(PACK_VERSION)-linux.tgz; sum=$(PACK_SHA256_AMD64);; aarch64|arm64) f=pack-v$(PACK_VERSION)-linux-arm64.tgz; sum=$(PACK_SHA256_ARM64);; *) echo "unsupported arch $$arch"; exit 1;; esac; \
	.github/fetch.sh "$(PACK_URL)/$$f" $$sum $(PACK_TGZ)

pack-install: pack-fetch ## the pinned pack CLI into /usr/local/bin — one place for the version and the sha256 (Dockerfile.executor reads the same values as build args)
	sudo tar -xzf $(PACK_TGZ) -C /usr/local/bin pack && rm $(PACK_TGZ)

# GitLab's sast-rules, pinned by tag and commit: the rules Canyon sends Opengrep (gate/opengrep).
SAST_RULES_TAG := v2.10.1
SAST_RULES_COMMIT := 53bf5cf6df3c51b6c02110f5a638b5e6213666cd

vendor-sast-rules: ## re-vendor gate/opengrep/sast-rules.tar from GitLab's sast-rules at the pinned commit, permissive folders only — a diff after means upstream moved
	go run gate/opengrep/vendor.go $(SAST_RULES_TAG) $(SAST_RULES_COMMIT) gate/opengrep/sast-rules.tar

opengrep-rules-test: ## the vendored sast-rules over the probe apps in gate/opengrep/testdata/probes, and Canyon's own rule (canyon.yaml) over its cases in testdata/canyon, offline, in Canyon's Opengrep image built from executor/opengrep.Dockerfile: each probe's expected rule (its file `expect`) must report, or the bundle misses what it ships for
	@img=$$(docker build -q - < executor/opengrep.Dockerfile) && \
	 docker run --rm --network none --entrypoint sh -v $(CURDIR)/gate/opengrep:/src:ro -w /src $$img -c 'mkdir /tmp/rules && tar -xf sast-rules.tar -C /tmp/rules && \
	   for p in testdata/probes/*/; do want=$$(cat $$p/expect); opengrep scan --quiet --json --no-rewrite-rule-ids --config /tmp/rules $$p | grep -q "\"check_id\": *\"$$want\"" && echo "ok  $$p $$want" || { echo "FAIL $$p: $$want not reported"; exit 1; }; done && \
	   out=$$(opengrep scan --quiet --json --config canyon.yaml testdata/canyon/flagged) && \
	   for e in testdata/canyon/flagged/*; do echo "$$out" | grep -q "\"path\": *\"$$e" && echo "ok  $$e" || { echo "FAIL $$e: canyon.yaml reports nothing"; exit 1; }; done && \
	   n=$$(opengrep scan --quiet --sarif --config canyon.yaml testdata/canyon/flagged/next-app | grep -o "\"ruleId\"" | wc -l) && \
	   if [ "$$n" -ne 1 ]; then echo "FAIL testdata/canyon/flagged/next-app: $$n results for its one splice, want 1"; exit 1; fi && \
	   if opengrep scan --quiet --json --config canyon.yaml testdata/canyon/clean | grep -q "\"check_id\""; then echo "FAIL testdata/canyon/clean: canyon.yaml reports"; exit 1; fi; echo "ok  testdata/canyon/clean"'

test-kube: ## the kube-tagged tests (executor/kube/kind_test.go) against the cluster KUBECONFIG names — kind from .github/kind/up.sh in CI — its executor reached by port-forward
	go test -tags kube -count=1 -v -timeout 30m -run '$(RUN)' ./executor/kube

test-e2e: builder-node ## builds and serves the fixture apps with the real builder, and with the Node builder make builder-node makes (needs Docker + pack, on Linux)
	# Convention, not a list: every test of executor/e2e_test.go has EndToEnd or E2E in its name, and no
	# unit test of the package does. A list left the sweep's out of CI (#1147).
	go test -tags e2e -count=1 -run 'EndToEnd|E2E' -v -timeout 52m ./executor # the Node builder's trees, built twice each, added about ten minutes (#174)

test-cluster-unit: ## the cluster harness's own pieces (e2e/cluster/README.md, Build): no test cluster needed, no $E2E_* required
	go vet -tags cluster ./e2e/cluster/...
	cd e2e/ui && npm ci --silent && npm run unit
	# Convention, not a list to maintain: a pure helper's unit test is named TestUnit<Something>; a
	# case (TestMaker, TestSecretsAndScanners, TestIsolationAndImages, …) never is, and this pattern
	# never runs one. A new case PR adds a TestUnit... test and touches nothing here.
	go test -tags cluster -run '^TestUnit' -count=1 -v ./e2e/cluster/...

e2e-cluster: ## the cluster suite (e2e/cluster/CASES.md) in its order, against the Canyon at $$E2E_CANYON_URL (README, What the test cluster provides): the Go cases and the Playwright cluster project, one process, the run's output grepped for every secret at the end
	cd e2e/ui && npm ci --silent && npx playwright install chromium
	e2e/fixtures/gen.sh e2e/.work/fixtures
	go test -tags cluster -count=1 -v -timeout 180m -run '^TestSuite$$' ./e2e/cluster/...

e2e-cluster-egress: ## the way out of one app of its own on a Canyon that holds other people's (e2e/cluster/CASES.md O1): no fresh install, no setting changed, against the Canyon at $$E2E_CANYON_URL; it fails while the host's via is off
	cd e2e/ui && npm ci --silent && npx playwright install chromium
	e2e/fixtures/gen.sh e2e/.work/fixtures
	go test -tags cluster -count=1 -v -timeout 45m -run '^TestOutbound$$' ./e2e/cluster/...

e2e-cluster-handoff-repo: ## the cluster suite's hand-off through a deploy repo alone (e2e/cluster/CASES.md H4-H8), against the Canyon at $$E2E_CANYON_URL and the test cluster's deploy repo, Argo CD, External Secrets and CloudNativePG (README, What the test cluster provides): it switches run_by to org and back
	cd e2e/ui && npm ci --silent && npx playwright install chromium
	e2e/fixtures/gen.sh e2e/.work/fixtures
	go test -tags cluster -count=1 -v -timeout 120m -run '^TestHandOffRepo$$' ./e2e/cluster/...

e2e-handoff: web ## the hand-off journey (e2e/handoff/README.md): Canyon under run_by: org against the cluster KUBECONFIG names, .github/kind/up.sh calico's in CI, on the Postgres at $$HANDOFF_DATABASE_URL; the admin sets it up in the dashboard, in Chromium (e2e/ui's handoff project)
	cd e2e/ui && npm ci --silent && npx playwright install chromium
	go test -tags handoff -count=1 -v -timeout 25m ./e2e/handoff

e2e-handoff-repo: web ## the hand-off journey through a deploy repo (e2e/handoff/README.md): as e2e-handoff, delivery: repo to a Gitea repo Argo CD syncs, on the cluster .github/kind/platform.sh set up
	cd e2e/ui && npm ci --silent && npx playwright install chromium
	go test -tags handoffrepo -count=1 -v -timeout 40m ./e2e/handoff

E2E_COMPOSE = docker compose $(if $(OIDC),--env-file e2e/.work/fixtures/oidc.env) -f docker-compose.yml -f e2e/compose.e2e.yml $(if $(MODEL),-f e2e/compose.model.yml) $(if $(OIDC),-f deploy/docker/team.yml -f e2e/compose.oidc.yml)

e2e-up: ## the E2E stack (e2e/compose.e2e.yml; MODEL=1 adds the stub as the reviewer's model; OIDC=1 runs it as the team setup, deploy/docker/team.yml, signing in through the stub's issuer: e2e/compose.oidc.yml): fixtures generated, the multi-arch builder pulled, up until /ready
	e2e/fixtures/gen.sh e2e/.work/fixtures
	e2e/fixtures/egress-trust.sh e2e/.work/fixtures
	mkdir -p e2e/.work/log
	docker pull -q paketobuildpacks/ubuntu-noble-builder
	$(E2E_COMPOSE) up -d --build
	@for i in $$(seq 1 45); do curl -sf http://127.0.0.1:7777/ready && echo && exit 0; sleep 2; done; \
	 echo "server never became ready"; $(E2E_COMPOSE) logs --tail=60 server stub; exit 1
	scripts/telemetry-off-check.sh docker # both Canyons, the server and canyon-b, send no usage report (e2e/compose.e2e.yml)

e2e-ui: ## the UX journeys (e2e/ui) on the Docker projects alone -- the cluster project is e2e-cluster's: the dashboard in a browser against a stack e2e-up started, every surface watched; reports in e2e/ui/.work/report. SHARD=i/n runs Playwright's i-th of n shards alone (CI's matrix)
	cd e2e/ui && npm ci --silent && npx playwright install chromium && npx playwright test --project=desktop --project=phone $(if $(SHARD),--shard=$(SHARD))

e2e-ui-typecheck: ## e2e/ui type-checked on its own -- a run only transforms the files it loads, so nothing else catches a type error in an unused helper or a cluster journey no run reaches yet
	cd e2e/ui && npm ci --silent && npx tsc --noEmit

e2e-down: ## the E2E stack and its volumes, gone
	$(E2E_COMPOSE) down -v

e2e: ## the journeys against a stack e2e-up started (MODEL=1 on both runs J9, the AI reviewer against the stub); they carry the stack build tag, so go test ./... never runs them. The server's and executor's logs are kept in e2e/.work/log, pass or fail
	go test -tags stack -count=1 -v -timeout 90m ./e2e/...; rc=$$?; \
	 for s in server executor; do $(E2E_COMPOSE) logs --no-color --no-log-prefix $$s > e2e/.work/log/$$s.log 2>&1; done; exit $$rc
