.PHONY: build web nodebin test check e2e dist man package clean

# Build metadata, stamped into the binary (internal/buildinfo).
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo dev)
COMMIT  ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG      = virticle/internal/buildinfo
LDFLAGS  = -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE)

# One binary with the web UI and the node command embedded. The web build runs
# tsc first, so a TypeScript error stops the build.
build: web nodebin
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/virticle ./cmd/virticle

web:
	cd web && npm ci --no-audit --no-fund && npm run build

# The node command (cmd/virticle-node) for linux/amd64 and linux/arm64, static,
# embedded into the controller by internal/nodebin and uploaded by "node prepare".
nodebin:
	mkdir -p internal/nodebin/dist
	for arch in amd64 arm64; do \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags '-s -w' \
			-o internal/nodebin/dist/virticle-node_linux_$$arch ./cmd/virticle-node || exit 1; \
	done

test:
	go vet ./... && go test ./...

# go vet, go test and the web type-check build; no browser, no lab.
check:
	go vet ./...
	go test ./...
	cd web && npm ci --no-audit --no-fund && npm run build

# Release binaries for linux/amd64 and linux/arm64. No CGO is needed (the
# SQLite driver is modernc.org/sqlite, pure Go), so these are static
# binaries cross-compiled without a C toolchain.
dist: web nodebin
	for arch in amd64 arm64; do \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' \
			-o dist/virticle_linux_$$arch/virticle ./cmd/virticle || exit 1; \
	done

# Compressed man page for the packages (generated; not committed).
man:
	gzip -9nc packaging/virticle.1 > packaging/virticle.1.gz

# .tar.gz, .deb, .rpm and checksums into dist/ without publishing anything.
# Needs goreleaser and nfpm's dependencies; see docs/install.md.
package:
	goreleaser release --snapshot --clean

# Browser suite against a running controller and lab: see e2e/README.md for the E2E_* variables.
e2e:
	cd e2e && npm ci --no-audit --no-fund && npx playwright test

clean:
	rm -rf bin dist packaging/virticle.1.gz internal/nodebin/dist/virticle-node_*
