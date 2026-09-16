VERSION := $(shell cat VERSION)
LDFLAGS := -X github.com/hausfold/scruff/internal/commands.Version=$(VERSION)

.PHONY: build test fmt vet check clean score

build:
	go build -ldflags "$(LDFLAGS)" -o scruff ./cmd/scruff

# The suite runs in PARALLEL, and `--no-parallelize-across-files` is what makes
# that free. bats spreads FILES with GNU parallel, but spreads the TESTS WITHIN
# a file with a bash semaphore of its own — and this is one file, so the flag
# skips the GNU-parallel requirement entirely: nothing new to install, here or
# on a runner. Without it bats aborts, "Cannot execute 8 jobs without GNU
# parallel".
#
# 8, and not the core count. The suite is latency-bound — fork/exec and git's
# fsyncs, 327 times over — not CPU-bound, so it keeps paying well past however
# many cores are underneath. Measured on the gate's own runners, five
# sibling-paired runs (macos-26-arm64, 3 cores): serial 190s mean, `--jobs 8`
# 76s, a median 118s off. `--jobs 2` is SLOWER than serial at 233s, because
# bats' parallel path carries a per-test cost that only enough concurrency
# hides; `--jobs 12` saves no more and stretches the heaviest case from 4.6s to
# 8.0s, which is the headroom the `watch` cases' WATCH_TIMEOUT lives in. The
# table is docs/ci.md's.
#
# BATS_JOBS=1 runs it serially. Interleaved output is the one thing parallel
# costs, and it costs it exactly when you are reading a failure — so that is
# the way back, not a reason to leave the other 118s on the floor.
#
# -T makes every case report its own duration, in CI's log and yours. Nothing
# reads it automatically; it is there so the next person asking where the time
# goes can answer it by reading, instead of deriving it from log timestamps.
BATS_JOBS ?= 8
BATS_FLAGS := -T $(if $(filter-out 1,$(BATS_JOBS)),--jobs $(BATS_JOBS) --no-parallelize-across-files)

# The acceptance suite. It is black-box — it drives the built binary with shim
# gh/lsof on PATH — so it is the same suite the bash `wt` runs against, and
# WT_UNDER_TEST still points it at any other implementation for comparison.
#
# `go test` runs first, and covers the one thing a black-box suite structurally
# can't: code that edits a file belonging to ANOTHER tool (Claude Code's
# ~/.claude.json), where most of the assertion is about what came through the
# rewrite untouched.
test: build
	go test ./...
	bats $(BATS_FLAGS) test/scruff.bats

# What fraction of the 0.1 contract holds today. Every remaining failure should
# be an unimplemented command, never a wrong behaviour in an implemented one.
score: build
	@bats $(BATS_FLAGS) test/scruff.bats 2>&1 | grep -c '^ok ' | tr -d ' ' | xargs -I{} echo "{} / $$(grep -c '^@test' test/scruff.bats) passing"
	@bats $(BATS_FLAGS) test/scruff.bats 2>&1 | grep '^not ok' | sed 's/^not ok [0-9]* //;s/:.*//' | sort | uniq -c | sort -rn

fmt:
	gofmt -w ./cmd ./internal ./skills.go

vet:
	go vet ./...

check: fmt vet test

clean:
	rm -rf scruff .gocache dist
