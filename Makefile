.PHONY: build web test

# One binary with the web UI embedded.
build: web
	go build -o bin/virt-proj ./cmd/virt-proj

web:
	cd web && npm ci --no-audit --no-fund && npm run build

test:
	go vet ./... && go test ./...
