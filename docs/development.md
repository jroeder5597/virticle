# Development

## Layout

| Path | Holds |
| --- | --- |
| `cmd/virticle` | Entry point and subcommands. |
| `cmd/virticle-node` | The node command: the one program a node lets the controller run as root. Tiny main. |
| `internal/` | API, libvirt access over SSH, scheduler, store (SQLite, pure Go). |
| `internal/nodecmd` | All node-side code (verbs, path confinement). Standard library and `x/sys` only; it must not import the controller packages. Start with its `doc.go`. |
| `internal/nodebin` | Embeds the built node command for linux/amd64 and linux/arm64. |
| `web/` | React and TypeScript UI, embedded into the binary from `web/dist`. |
| `third_party/go-libvirt` | Vendored libvirt RPC client. |
| `e2e/` | Playwright browser suite. See [e2e/README.md](https://github.com/jacobanthonyroeder/virticle/blob/main/e2e/README.md) in the repository (packages do not ship it). |
| `lab/` | Scripts that build a nested test lab of libvirt nodes. |
| `packaging/`, `.goreleaser.yaml` | Service file, package scripts, release pipeline. |

## Build and test

Needs Go (version in `go.mod`) and Node.js 20.

    make build      # web UI and node command, then bin/virticle
    make nodebin    # only the node command, for both architectures
    make test       # go vet and unit tests
    make check      # the above plus the web type-check build
    make e2e        # browser suite; needs a running controller and a lab

`go build` alone fails until the web UI has been built once.

The node command is built by `make nodebin` into `internal/nodebin/dist/`
(`virticle-node_linux_amd64`, `virticle-node_linux_arm64`; not committed) and
embedded into the controller. `go vet` and `go test` work without it; a
controller built without it reports "built without the node command" when
`virticle node prepare` needs the binary. `node prepare` detects the node's
architecture with `uname -m` and uploads the matching file to
`/usr/local/libexec/virticle-node`. The release build (goreleaser) and the
container build run the same step first. Raise `internal/nodeproto.Version`
when a verb changes in a way an older controller or node cannot use.

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
4. Publishing starts the `pkgrepo` workflow, which rebuilds the signed apt and
   dnf repository from all published releases and deploys it to GitHub Pages
   (`packaging/repo/build-repo.sh`; one-time setup in
   [install.md](install.md#install-from-the-package-repository)).

Local dry run that publishes nothing: `make package` (needs goreleaser).

Not set up yet: a container registry. The files of a release are not signed;
the package repository is.

## Documentation screenshots

The images in `docs/img` are 1280 pixels wide, taken in the dark theme from a
running controller with real content. Before taking one, replace anything that
must not be published: addresses of the lab, host names, jump hosts, e-mail
addresses, passwords and keys. Look at every image afterwards. When the UI
changes, retake the images that show it; `python3` with Pillow can reduce the
size (quantise to 256 colours).
