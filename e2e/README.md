# End-to-end browser tests

Playwright (Chromium only) drives the real UI of a running controller against a real lab
(two libvirt nodes) and checks the resulting state through the API as well.

## Run

    make e2e            # or: cd e2e && npm ci && npx playwright test

Playwright is pinned to 1.65.0 (Chromium build 1254, expected in `~/.cache/ms-playwright`;
run `npx playwright install chromium` only if it is missing).

Configuration is by environment:

| variable | meaning | default |
|---|---|---|
| `E2E_BASE_URL` | controller URL | `http://127.0.0.1:8095` |
| `E2E_USER`, `E2E_PASS` | an administrator account (required) | `admin`, none |
| `E2E_NODE1`, `E2E_NODE2` | hostnames (or addresses) of the two nodes as listed under Nodes | `vspike-n1`, `vspike-n2` |
| `E2E_PREFIX` | name prefix of everything the suite creates | `e2e-` |
| `E2E_CIRROS`, `E2E_DEBIAN` | existing images in pool `default` on node 1 | `cirros-0.6.3.qcow2`, `wa-debian13.qcow2` |

The controller must already have both nodes registered. Node 1 needs the two images in pool
`default` (they are only read, never modified) and internet access (a 21 MB download from
download.cirros-cloud.net, and `apt` inside the Debian guest).

## What it creates

Only objects whose name starts with `E2E_PREFIX`: VMs (cirros, one Debian 512 MB with cloud-init,
templates, linked clones), volumes (also in `default`), a directory pool
`/var/lib/libvirt/<prefix>pool`, a NAT network `10.79.x.0/24`, backups, users, API tokens.
Peak load is two 128 MB VMs, or one 512 MB VM. Specs run serially (one worker) and each cleans up
in `afterAll`; the global setup and teardown sweep any leftover object with the prefix through the
API and print what they removed. A removed pool leaves its (empty) directory on the node.

Nothing without the prefix is touched. The evacuation test unticks every VM that is not its own
before running.

## Layout

`tests/NN-*.spec.ts` one file per area; `lib/` holds the API client, fixtures and UI helpers.
`14-visual` writes dark-mode and phone-width screenshots to `test-results/visual/` and fails on
unreadable text or sideways scrolling. The Debian test waits up to 7 minutes for the guest agent.
