.PHONY: all check clean fmt fmt-check vet build binary binaries image windows test race e2e run

# tunneld and its dependencies are pure Go. Forcing CGO off keeps every build
# identical across hosts, produces a dependency-free binary that runs on a
# scratch/distroless base, and sidesteps broken toolchains (e.g. windows-11-arm
# runners ship an x86_64 gcc that can't assemble runtime/cgo's arm64 stubs).
export CGO_ENABLED = 0

# The release a binary reports from `tunneld version`, stamped through the
# ldflag v1alpha1/version.go documents. CI passes the exact tag; here git
# describe gives the nearest tag, plus distance and -dirty when the build is
# off a commit or an uncommitted tree, so the binary says so. Override on the
# command line to stamp something else: make binaries VERSION=v0.0.19.
# Outside a checkout it is empty and the binary falls back to build info.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null)

# Default: everything CI runs except the race lane (needs a C toolchain — run
# `make race` for it) and the auto-bump release step.
all: fmt-check vet build windows test e2e

# Compose the common pre-push checklist. Mirrors the CI matrix.
check: fmt-check vet windows test e2e

# gofmt the tree in place.
fmt:
	gofmt -w .

# Fail if anything in the tree is not gofmt-clean.
fmt-check:
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then echo "gofmt found unformatted files:"; echo "$$out"; exit 1; fi

# Static analysis across every package.
vet:
	go vet ./...

# Build the whole module for the host platform.
build:
	go build ./...

# Build just the tunneld binary into the working directory.
binary:
	go build -o tunneld .

# Build the tunneld binary for every platform the npm package ships, into
# dist/. Targets are named the way node names platforms — process.platform,
# then process.arch — so the launcher finds its binary by string
# concatenation. The rule maps that name back to Go's: the first word is GOOS
# once win32 becomes windows, the second is GOARCH once x64 becomes amd64,
# and win32 carries .exe, which basename strips before the split. -trimpath
# keeps build paths out of the binary so the same source gives the same bytes
# on any host; VERSION is stamped so `tunneld version` names the release.
PLATFORMS := linux-x64 linux-arm64 darwin-x64 darwin-arm64 win32-x64 win32-arm64
BINARIES := $(foreach p,$(PLATFORMS),dist/tunneld-$(p)$(if $(findstring win32,$(p)),.exe))

# Wipes dist/ first so nothing stale ships alongside — the npm package
# globs dist/*, and a renamed platform would otherwise leave its old binary
# behind. Building one binary by name does not wipe the others.
binaries:
	rm -rf dist
	$(MAKE) $(BINARIES)

.PHONY: $(BINARIES)
$(BINARIES): dist/tunneld-%:
	GOOS=$(subst win32,windows,$(word 1,$(subst -, ,$(basename $*)))) \
	GOARCH=$(subst x64,amd64,$(word 2,$(subst -, ,$(basename $*)))) \
	go build -trimpath \
	  -ldflags="-s -w -X github.com/tunnel-pizza/tunneld/v1alpha1.version=$(VERSION)" \
	  -o $@ .

# Cross-compile + vet for Windows. A build-only smoke so the binary doesn't
# quietly stop building on the other major target.
windows:
	GOOS=windows go vet ./...
	GOOS=windows go build ./...

# Unit tests: every package's own *_test.go, plus the godoc examples in lib.
test:
	go test ./...

# Every package under the race detector — the same lane CI runs, runnable
# locally to reproduce a CI race find. The recipe-line CGO_ENABLED=1 overrides
# the global export above: the detector links through cgo. Kept out of `all`
# for that reason — it is the one target needing a C toolchain.
#
# -short is what keeps the live e2e row out of this lane. That row mints a real
# tunnel, and running it here would mint a second one per CI push to re-check
# what the e2e lane already checked, under a detector that only slows it down.
race:
	CGO_ENABLED=1 go test -short -race ./...

# End-to-end: the harness builds the tunneld binary and every example binary and
# drives them. -count=1 disables go test caching, since the harness builds those
# at runtime and the cache key wouldn't otherwise pick up source changes.
#
# One row runs live and mints a real tunnel, so this target needs the public
# internet. `go test -short ./e2e` is the same lane without it.
e2e:
	go test -count=1 -v ./e2e

# Run an example by name:
#   make run basic
#   make run multi-origin
#   make run attach
#
# Examples are real programs: each starts the origins it exposes, opens a
# tunnel, and blocks until interrupted. Nothing else needs to be running —
# except `attach`, whose origin is a container it cannot spawn, so it wants
# `docker run -d --rm --name tunneld-demo -it alpine sh` first.
#
# Only bare words forward — make reads a leading -- as one of its own options,
# so anything with flags goes through go run directly:
#   go run ./examples/basic --url http://localhost:8080
#   go run . --url http://localhost:3000
run: image
	cd examples/$(word 2,$(MAKECMDGOALS)) && go run . $(wordlist 3,$(words $(MAKECMDGOALS)),$(MAKECMDGOALS))

# Swallow the example name and forwarded args (extra goals) so make doesn't error.
%:
	@:

# Build the container image for the host platform. CI builds it multi-arch and
# pushes to ghcr; this is the same Dockerfile, so a local build catches a break
# before a tag does.
image:
	docker build --build-arg VERSION=$(VERSION) -t tunneld:local .

# Remove what building and running leave behind.
#
# Everything is named, never swept: `docker system prune` would take containers
# and volumes this repo never created, and a clean target that can ruin an
# unrelated afternoon is one nobody runs.
#
# Every cached tunnel spec on the machine goes, not just this project's. The
# cache is one flat directory of files named for the run that wrote them — the
# working directory and the origins, hashed — so nothing in it says which
# project a file came from, and a clean that removed some and left others would
# be the harder behaviour to explain.
#
# The compose example keeps a cache in a named volume, which is what
# `down --volumes` removes; the example's own teardown deliberately does not
# pass that flag, because there the point is to keep it between runs.
#
# Each docker line is prefixed with - so a machine without docker, or a stack
# that was already down, still finishes the rest.
clean:
	rm -f tunneld tunneld.exe
	rm -rf dist
	rm -rf "$$HOME/Library/Caches/.tunneld" "$${XDG_CACHE_HOME:-$$HOME/.cache}/.tunneld"
	go clean -testcache
	-docker compose -p tunneld-example down --volumes --remove-orphans 2>/dev/null
	-docker rm -f tunneld-example 2>/dev/null
	-docker image rm -f tunneld:local 2>/dev/null
	rm -rf $${TMPDIR:-/tmp}/tunneld-example-*
