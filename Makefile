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

agent:
	cd agent && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
	  go build -ldflags="-s -w" -o ../images/fos-agent ./cmd/fos-agent

.PHONY: agent
