# The documented build has to stamp the version itself.
#
# main.go says version is "overridden at build time via -ldflags", but nothing
# in the repo did the overriding, so the build the README documented produced a
# binary reporting 0.0.0-dev — and "which weft am I running" is the first
# question when behaviour surprises you. A flag people are asked to remember is
# not a build; a dogfooding run stamped its binary by hand and hit exactly this.
#
# Near-zero dependencies is a design constraint (BUILD.md §1.1), and make is
# already on every machine that has a Go toolchain. This adds nothing to the
# binary and nothing to cold start.

BIN     := dist/weft
RELEASE := 0.1.0

# A build from a modified tree is not the commit it names. Saying so is the
# whole point of stamping: a version that silently claims a clean commit sends
# you looking for the bug in code you are not running. `git diff HEAD` covers
# staged and unstaged changes to tracked files, and ignores untracked scratch.
REV   := $(shell git rev-parse --short HEAD 2>/dev/null)
DIRTY := $(shell git diff --quiet HEAD 2>/dev/null || echo dirty)

ifeq ($(REV),)
# A tarball or a machine without git. Unknown is honest; "dirty" would not be.
VERSION := $(RELEASE)-unknown
else
VERSION := $(RELEASE)-$(REV)$(if $(DIRTY),-dirty)
endif

GOFLAGS_LD := -X main.version=$(VERSION)
PREFIX     ?= $(HOME)/.local/bin

.PHONY: build check vet test test-tools smoke fuzz install clean version

# Seconds per fuzz target. Override for a longer hunt: make fuzz FUZZTIME=10m
FUZZTIME ?= 60s

PYTHON ?= python3
# Globbed, not listed. A suite that exists but is named in no build file is
# exactly the failure this target was added to close, and a list is a second
# place to forget.
TOOL_TESTS := $(wildcard tools/test_*.py)

build:
	go build -ldflags "$(GOFLAGS_LD)" -o $(BIN) ./cmd/weft

# The verification ladder, in BUILD.md §4 order: it builds, it runs, it passes.
# smoke depends on build, so a stale binary cannot pass rung 2.
check: vet smoke test test-tools

vet:
	go vet ./...
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed:"; echo "$$unformatted"; exit 1; \
	fi

test:
	go test ./...

# tools/ sits outside the Go build (BUILD.md §1.1) but not outside the ladder.
# Its suites carry most of the coverage for the viewer, and nothing ran them in
# CI — so a green check on a viewer change said nothing about the viewer.
#
# A missing interpreter is a hard failure rather than a skip. Skipping is how
# these went unrun in the first place, and a check that quietly covers less
# than it appears to is worse than one that is honestly absent.
test-tools:
	@command -v $(PYTHON) >/dev/null 2>&1 || { \
	  echo "$(PYTHON) not found, so the tools/ suites cannot run."; \
	  echo "They are part of 'make check' deliberately; use 'make test' for Go alone."; \
	  exit 1; }
	@test -n "$(TOOL_TESTS)" || { echo "no tools/test_*.py found"; exit 1; }
	@for t in $(TOOL_TESTS); do echo "==> $$t"; $(PYTHON) $$t || exit 1; done

smoke: build
	./smoke.sh

# Not part of `check`, and deliberately not in CI: a time-boxed search has a
# nondeterministic result, and a job that goes red for reasons unrelated to the
# change teaches people to ignore CI — the same argument smoke.sh makes about
# pending stages. The seed corpora still run under `make test`, so a fixed bug
# stays fixed without anyone running this.
#
# Go allows one fuzz target per invocation, so this walks them.
fuzz:
	@for pkg_target in \
		"./internal/event FuzzParseRoundTrip" \
		"./internal/graph FuzzFoldInvariants" \
		"./internal/graph FuzzFoldIsDeterministic" \
		"./internal/cli FuzzPermuteKeepsEveryArgument" \
		"./internal/cli FuzzPermuteIsStable" ; do \
		set -- $$pkg_target; \
		echo "== fuzz $$2 ($(FUZZTIME))"; \
		go test $$1 -run $$2 -fuzz $$2 -fuzztime=$(FUZZTIME) || exit 1; \
	done

# Installing the unstamped binary is the failure this file exists to prevent, so
# install goes through build rather than through `go install`, which would need
# the ldflags repeated in a second place to stay correct.
install: build
	@mkdir -p $(PREFIX)
	install -m 0755 $(BIN) $(PREFIX)/weft
	@echo "installed $(PREFIX)/weft ($(VERSION))"

# Useful in CI and when reporting a bug: what a build here would call itself.
version:
	@echo $(VERSION)

clean:
	rm -rf dist
