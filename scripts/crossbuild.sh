#!/bin/sh
# Compiles every package for each release target with cgo off; the first failure
# stops the script. MIPS needs softfloat: OpenWrt kernels ship without FPU emulation.
set -eu
cd "$(dirname "$0")/.."

build() {
	echo "== $*"
	env CGO_ENABLED=0 "$@" go build ./...
}

build GOOS=linux GOARCH=amd64
build GOOS=linux GOARCH=arm64
build GOOS=linux GOARCH=arm GOARM=7
build GOOS=linux GOARCH=arm GOARM=5
build GOOS=linux GOARCH=mips GOMIPS=softfloat
build GOOS=linux GOARCH=mipsle GOMIPS=softfloat
build GOOS=windows GOARCH=amd64
build GOOS=windows GOARCH=arm64
build GOOS=darwin GOARCH=amd64
build GOOS=darwin GOARCH=arm64
