# Multi-stage build: web UI, then the Go binary (pure Go, no CGO), then a
# distroless static image that runs as a non-root user.
#   docker build -t virticle .
#   docker run -p 8443:8443 -v virticle-data:/data virticle

FROM --platform=$BUILDPLATFORM node:20-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
WORKDIR /src
COPY go.mod go.sum ./
COPY third_party ./third_party
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY web/embed.go ./web/embed.go
COPY --from=web /src/web/dist ./web/dist
# The node command for both node architectures, embedded by internal/nodebin.
RUN for a in amd64 arm64; do \
      CGO_ENABLED=0 GOOS=linux GOARCH=$a go build -trimpath -ldflags "-s -w" \
        -o internal/nodebin/dist/virticle-node_linux_$a ./cmd/virticle-node || exit 1; \
    done
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X virticle/internal/buildinfo.Version=$VERSION -X virticle/internal/buildinfo.Commit=$COMMIT -X virticle/internal/buildinfo.Date=$DATE" \
    -o /out/virticle ./cmd/virticle
# The data directory, owned by the runtime user so a new volume is writable.
RUN mkdir /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/virticle /usr/bin/virticle
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
VOLUME /data
EXPOSE 8443
# No -listen: the controller serves HTTPS with a generated self-signed
# certificate (kept in /data) on port 8443 until the Web access page says
# otherwise.
ENTRYPOINT ["/usr/bin/virticle"]
CMD ["-data", "/data"]
