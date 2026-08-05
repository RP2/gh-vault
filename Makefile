# Makefile for gh-vault
# Canonical supply chain security workflow.
# Always run `make vendor` after changing dependencies.

.PHONY: all vendor check verify clean tools help

# Resolve GOPATH dynamically (handles non-default setups).
GOPATH := $(shell go env GOPATH 2>/dev/null || echo "$$HOME/go")

# Pin toolchain — prevents go.mod from triggering silent toolchain downloads.
export GOTOOLCHAIN := local

# govulncheck version — bump quarterly or on advisories.
# See: https://github.com/golang/vuln/releases
GOVULNCHECK_VERSION := v1.1.4

# ---------- Targets ----------

# help: Show available targets (default goal).
help:
	@echo "Targets:"
	@echo "  make vendor  - Tidy, vendor, scrub AI files, verify build + integrity"
	@echo "  make check   - Run govulncheck (requires: make tools first)"
	@echo "  make tools   - Install pinned govulncheck"
	@echo "  make verify  - Re-check build + tests against vendor/"
	@echo "  make clean   - Remove build cache"
	@echo "  make all     - Full workflow: vendor → check → verify"
	@echo ""
	@echo "WARNING: 'make all' mutates go.mod and vendor/."

# vendor: Tidy, vendor, scrub AI files, verify build + integrity.
# WHY: The only safe sequence. Order matters:
#   1. tidy resolves the module graph (needs network, writes go.mod/go.sum)
#   2. vendor materializes it (needs network, writes vendor/)
#   3. scrub prevents prompt injection from upstream deps
#   4. build proves it compiles (uses vendor/)
#   5. mod verify proves hashes match
vendor:
	@echo "==> Tidying go.mod..."
	go mod tidy
	@echo "==> Vendoring dependencies..."
	go mod vendor
	@echo "==> Removing AI instruction files from vendor/..."
	@# Known AI instruction files that upstream deps may ship.
	@# Keep this list in sync with new AI tools.
	@# Bump: add new entries as tools are discovered.
	find vendor -type f \( \
		-iname 'CLAUDE.md' -o \
		-iname 'GEMINI.md' -o \
		-iname 'AGENTS.md' -o \
		-iname '.cursorrules' -o \
		-iname '.windsurfrules' -o \
		-iname '.clinerules' -o \
		-iname '.goosehints' -o \
		-iname 'CONVENTIONS.md' -o \
		-ipath '*/.github/copilot-instructions.md' -o \
		-ipath '*/.github/instructions/*.md' \
	\) -delete -print | { count=$$(( $$(wc -l) )); echo "  Removed $$count file(s)"; }
	@# Verify no AI instruction files remain.
	@if find vendor -type f \( \
		-iname 'CLAUDE.md' -o -iname 'GEMINI.md' -o -iname 'AGENTS.md' -o \
		-iname '.cursorrules' -o -iname '.windsurfrules' -o -iname '.clinerules' -o \
		-iname '.goosehints' -o -iname 'CONVENTIONS.md' -o \
		-ipath '*/.github/copilot-instructions.md' -o \
		-ipath '*/.github/instructions/*.md' \
	\) | grep -q .; then \
		echo "ERROR: AI instruction files still present after scrub!"; \
		exit 1; \
	fi
	@echo "==> Verifying build with vendored dependencies..."
	go build -mod=vendor ./...
	@echo "==> Verifying module integrity..."
	go mod verify
	@echo "==> Vendor complete."

# check: Run govulncheck against vendored dependencies.
# WHY: Detects known CVEs before they reach production.
# Requires network for fresh vuln database (non-reproducible by design).
check:
	@echo "==> Running security vulnerability check..."
	@if [ -x "$(GOPATH)/bin/govulncheck" ]; then \
		$(GOPATH)/bin/govulncheck ./...; \
	elif command -v govulncheck >/dev/null 2>&1; then \
		govulncheck ./...; \
	else \
		echo "govulncheck not found. Run 'make tools' to install."; \
		exit 1; \
	fi

# tools: Install pinned govulncheck into GOPATH/bin.
# WHY: Pins the scanner version for reproducibility.
# Run once per machine; `make check` uses the installed binary.
tools:
	@echo "==> Installing govulncheck $(GOVULNCHECK_VERSION)..."
	GOBIN=$(GOPATH)/bin go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	@if [ -x "$(GOPATH)/bin/govulncheck" ]; then \
		echo "==> Installed to $(GOPATH)/bin/govulncheck"; \
	else \
		echo "ERROR: govulncheck not found at $(GOPATH)/bin/govulncheck after install."; \
		echo "  Check GOPATH=$(GOPATH) and go env GOBIN."; \
		exit 1; \
	fi

# verify: Independent re-check that vendor/ is buildable and tests pass.
# WHY: Catches drift if vendor/ is edited directly outside `make vendor`.
# Uses -mod=vendor explicitly on every command (no global GOFLAGS).
verify:
	@echo "==> Verifying module integrity..."
	go mod verify
	@echo "==> Verifying build with vendored dependencies..."
	go build -mod=vendor ./...
	@echo "==> Running tests with vendored dependencies..."
	go test -mod=vendor ./...

# clean: Remove build cache only. vendor/ is preserved (committed source).
clean:
	@echo "==> Cleaning build cache..."
	go clean
	@echo "==> Clean complete. (vendor/ preserved — it is committed source)"

# all: Full workflow. Security check runs early to fail fast.
all: vendor check verify
