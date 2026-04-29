BUILDROOT_VERSION ?= 2026.02
KERNEL_VERSION    ?= 6.18.22

.PHONY: all build kernel fs clean test

all: build

build:
	./build.sh -n

kernel:
	./build.sh -k -n

fs:
	./build.sh -f -n

test:
	cd agent && go test ./...

clean:
	rm -rf build/output images/

fos-agent:
	cd agent && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
	  go build \
	    -ldflags="-s -w \
	      -X github.com/nemvince/fos-next/internal/version.Version=$(shell git describe --tags --always 2>/dev/null || echo dev) \
	      -X github.com/nemvince/fos-next/internal/version.Commit=$(shell git rev-parse --short HEAD 2>/dev/null || echo unknown) \
	      -X github.com/nemvince/fos-next/internal/version.BuildDate=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)" \
	    -o ../images/fos-agent ./cmd/fos-agent

.PHONY: fos-agent
