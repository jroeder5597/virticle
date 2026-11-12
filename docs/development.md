# Development

## Layout

| Path | Holds |
| --- | --- |
| `cmd/virticle` | Entry point and subcommands. |
| `internal/` | API, libvirt access over SSH, scheduler, store (SQLite, pure Go). |
| `web/` | React and TypeScript UI, embedded into the binary from `web/dist`. |
| `third_party/go-libvirt` | Vendored libvirt RPC client. |
| `e2e/` | Playwright browser suite. See [e2e/README.md](../e2e/README.md). |
| `lab/` | Scripts that build a nested test lab of libvirt nodes. |
| `packaging/`, `.goreleaser.yaml` | Service file, package scripts, release pipeline. |

## Build and test

Needs Go (version in `go.mod`) and Node.js 20.

    make build      # web UI, then bin/virticle
    make test       # go vet and unit tests
    make check      # the above plus the web type-check build
    make e2e        # browser suite; needs a running controller and a lab

`go build` alone fails until the web UI has been built once.

## Container image

    docker build -t virticle .

- In a container without `/dev/net/tun` (an LXC, for example), build with
  `podman build --network=host`.
- The image is distroless, runs as user 65532 and keeps state in `/data`.

## Release

1. Tag: `git tag v1.2.3 && git push origin v1.2.3`.
2. The `release` workflow runs goreleaser and creates a **draft** GitHub
   release with `.tar.gz`, `.deb`, `.rpm` (amd64, arm64) and `checksums.txt`.
3. Review the draft and publish it.

Local dry run that publishes nothing: `make package` (needs goreleaser).

Not set up yet: a container registry, package signing, a licence.
