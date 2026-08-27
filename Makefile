.PHONY: all check fmt fmt-check vet build binary windows test race e2e run

# tunneld and its dependencies are pure Go. Forcing CGO off keeps every build
# identical across hosts, produces a dependency-free binary that runs on a
# scratch/distroless base, and sidesteps broken toolchains (e.g. windows-11-arm
# runners ship an x86_64 gcc that can't assemble runtime/cgo's arm64 stubs).
export CGO_ENABLED = 0

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
# If this repo ever grows a live e2e tier gated on testing.Short() — one that
# mints a real tunnel — this lane must pass -short too: without it the live
# cases start running under the detector on every CI run, which is exactly the
# traffic the tiering exists to avoid.
race:
	CGO_ENABLED=1 go test -race ./...

# End-to-end: the harness builds the tunneld binary and drives its offline
# paths. -count=1 disables go test caching, since the harness builds the binary
# at runtime and the cache key wouldn't otherwise pick up source changes.
e2e:
	go test -count=1 -v ./e2e

# Run tunneld from source. Arguments go in ARGS rather than as trailing words,
# because make would parse a bare --url as one of its own flags:
#   make run ARGS="--url http://localhost:3000"
#   make run ARGS="--url http://localhost:3000 --url http://localhost:4000"
run:
	go run . $(ARGS)
