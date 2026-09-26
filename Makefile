BIN      := ./pr-manager
ADDR     ?= 127.0.0.1:8765
REPO     ?=
AUTHOR   ?=
LIMIT    ?= 20
# API keys are read from ENV_FILE only if they aren't already exported.
# Copy .env.example to .env (git-ignored) and fill it in.
ENV_FILE ?= .env
# Extra serve flags, e.g. make ui SERVE_FLAGS=-review-dry-run
SERVE_FLAGS ?=
DEV_FLAGS = -cache .cache -codemap .cache/map $(if $(CODEMAP_CONFIG),-codemap-config $(CODEMAP_CONFIG))
# Code map: optional scoring config, workspace (a directory with code/<repo>),
# repos to force, lookup target.
CODEMAP_CONFIG ?=
WORKSPACE ?= $(PR_MANAGER_WORKSPACE)
MAP_REPO ?=
TARGET ?=

KEYS = if [ -f "$(ENV_FILE)" ]; then \
	eval "$$(grep -E '^(ANTHROPIC|OPENAI)_API_KEY=.+' "$(ENV_FILE)" | sed -E 's/^([A-Z_]+)=(.*)$$/: "$${\1:=\2}"; export \1/')"; \
	fi;

.PHONY: build desktop test ui serve stop triage-prs clean-cache codemap codemap-rank codemap-lookup

build:
	go build -o $(BIN) .

# Native Wails app with the cgo tree-sitter parser. Build on the target OS.
desktop:
	scripts/build-desktop.sh

test:
	go vet ./... && go test ./...

# Start the UI server and open it in the browser.
ui: build
	@$(KEYS) PR_MANAGER_WORKSPACE="$(WORKSPACE)" $(BIN) serve $(DEV_FLAGS) -addr $(ADDR) $(SERVE_FLAGS)

serve: build
	@$(KEYS) PR_MANAGER_WORKSPACE="$(WORKSPACE)" $(BIN) serve $(DEV_FLAGS) -addr $(ADDR) $(SERVE_FLAGS)

# Stop a running UI server.
stop:
	@pkill -f '$(BIN) serve' && echo stopped || echo "not running"

# Triage an author's PRs into the UI cache (make triage-prs REPO=o/r AUTHOR=login).
triage-prs: build
	@test -n "$(REPO)" -a -n "$(AUTHOR)" || { echo "usage: make triage-prs REPO=owner/repo AUTHOR=login"; exit 1; }
	@$(KEYS) PR_MANAGER_WORKSPACE="$(WORKSPACE)" $(BIN) prs $(DEV_FLAGS) -repo $(REPO) -author $(AUTHOR) -limit $(LIMIT)

clean-cache:
	rm -rf .cache/results

# Rebuild the code map in .cache/map (re-extracts only repos that changed).
# make codemap WORKSPACE=~/ws MAP_REPO=api forces one repo; FORCE=1 forces all.
codemap:
	@PR_MANAGER_WORKSPACE="$(WORKSPACE)" CODEMAP_CONFIG="$(CODEMAP_CONFIG)" scripts/codemap.sh build $(if $(MAP_REPO),-repo $(MAP_REPO)) $(if $(FORCE),-force)

# Re-score from cached graphs after editing CODEMAP_CONFIG.
codemap-rank:
	@CODEMAP_CONFIG="$(CODEMAP_CONFIG)" scripts/codemap.sh rank

# make codemap-lookup TARGET='api/internal/user/server.go:(*Server).GetProfile'
codemap-lookup:
	@scripts/codemap.sh lookup $(TARGET)
