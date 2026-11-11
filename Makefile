.PHONY: build web test check e2e

# One binary with the web UI embedded.
build: web
	go build -o bin/virticle ./cmd/virticle

web:
	cd web && npm ci --no-audit --no-fund && npm run build

test:
	go vet ./... && go test ./...

# go vet, go test and the web type-check build; no browser, no lab.
check:
	go vet ./...
	go test ./...
	cd web && npm ci --no-audit --no-fund && npm run build

# Browser suite against a running controller and lab: see e2e/README.md for the E2E_* variables.
e2e:
	cd e2e && npm ci --no-audit --no-fund && npx playwright test
