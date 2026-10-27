# Developer conveniences. Mirrors what .github/workflows does, so a release
# build can be reproduced and checked locally before tagging.

BINARY  := cli-to-api
PKG     := ./cmd/cli-to-api
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
DIST    := dist

PLATFORMS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64

.PHONY: all build test vet fmt fmt-check check snapshot clean

all: check build

## build: build for the host platform
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)

## check: the same gates CI enforces
check: fmt-check vet test

fmt:
	gofmt -w .

fmt-check:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "Not gofmt'd:"; echo "$$unformatted"; exit 1; \
	fi

vet:
	go vet ./...

test:
	go test -race ./...

## snapshot: cross-compile every release target into dist/ with checksums.
## Produces the exact archive names the install scripts expect.
snapshot: clean
	@mkdir -p $(DIST)
	@set -eu; \
	for platform in $(PLATFORMS); do \
		goos="$${platform%%/*}"; goarch="$${platform##*/}"; \
		bin="$(BINARY)"; \
		if [ "$$goos" = "windows" ]; then bin="$(BINARY).exe"; fi; \
		echo "==> $$goos/$$goarch"; \
		rm -rf $(DIST)/stage; mkdir -p $(DIST)/stage; \
		GOOS="$$goos" GOARCH="$$goarch" CGO_ENABLED=0 \
			go build -trimpath -ldflags "$(LDFLAGS)" -o "$(DIST)/stage/$$bin" $(PKG); \
		base="$(BINARY)_$(VERSION)_$${goos}_$${goarch}"; \
		if [ "$$goos" = "windows" ]; then \
			(cd $(DIST)/stage && zip -q -X "../$$base.zip" "$$bin"); \
		else \
			tar -czf "$(DIST)/$$base.tar.gz" -C $(DIST)/stage "$$bin"; \
		fi; \
	done; \
	rm -rf $(DIST)/stage; \
	cd $(DIST); \
	if command -v sha256sum >/dev/null 2>&1; then sum="sha256sum"; else sum="shasum -a 256"; fi; \
	: > checksums.txt; \
	for f in *.tar.gz *.zip; do \
		[ -f "$$f" ] || continue; \
		$$sum -- "$$f" >> checksums.txt; \
	done; \
	cat checksums.txt; \
	ls -l

clean:
	rm -rf $(DIST) $(BINARY) $(BINARY).exe
