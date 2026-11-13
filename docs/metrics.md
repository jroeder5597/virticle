# Metrics for Prometheus and Grafana

The controller serves the current values of every VM and node at `/metrics` in the Prometheus text format. Grafana, a
Prometheus server, VictoriaMetrics, the Grafana Agent and similar tools can read it. The page is built from the
controller's memory: a scrape makes no calls to libvirt or the nodes.

For the history in the web UI (7 days, charts), see [Performance](user-guide.md#performance).

## Get a token

`/metrics` is never open. It needs an API token as a bearer token; the **viewer** role is enough.

1. Sign in, open **Account**, **API tokens**, create a token with the role *Viewer*.
2. Copy the secret (it is shown once).
3. Test it: `curl -k -H "Authorization: Bearer <token>" https://virticle.example:8443/metrics`

## Prometheus `scrape_config`

```yaml
scrape_configs:
  - job_name: virticle
    metrics_path: /metrics
    scheme: https                    # http when the controller serves plain HTTP
    scrape_interval: 15s             # the collector samples every 15 seconds
    authorization:
      type: Bearer
      credentials: virticle_xxxxxxxxxxxxxxxx    # or credentials_file: /etc/prometheus/virticle.token
    tls_config:
      insecure_skip_verify: true     # self-signed certificate; with your own CA use ca_file instead
    static_configs:
      - targets: ["virticle.example:8443"]
```

A scrape faster than 15 seconds only repeats the same numbers. The `node` label is the node's host name (or its address
when the name is not known); `vm` is the VM name and `uuid` its UUID, which stays the same when the VM moves between
nodes.

## Metrics

Names start with `virticle_`. Units are base units: seconds, bytes, ratios, degrees Celsius. Counters are the raw
cumulative counters (they restart from zero when a VM restarts or a node reboots; `rate()` handles that). A metric the
node or libvirt does not report is left out, never written as zero.

| Name | Type | Labels | Unit | Meaning |
| --- | --- | --- | --- | --- |
| `virticle_build_info` | gauge | `version`, `goversion` | 1 | The controller build |
| `virticle_node_up` | gauge | `node` | 0 or 1 | The controller is connected to the node |
| `virticle_node_vms_running` | gauge | `node` | count | VMs running or paused |
| `virticle_node_cpu_seconds_total` | counter | `node`, `mode` (user, system, idle, iowait, steal) | seconds | CPU time, all cores |
| `virticle_node_load1`, `_load5`, `_load15` | gauge | `node` | load | Load average |
| `virticle_node_memory_total_bytes` | gauge | `node` | bytes | Memory of the node |
| `virticle_node_memory_available_bytes` | gauge | `node` | bytes | Available without swapping |
| `virticle_node_memory_cached_bytes` | gauge | `node` | bytes | Page cache and buffers |
| `virticle_node_swap_used_bytes` | gauge | `node` | bytes | Swap in use |
| `virticle_node_network_receive_bytes_total`, `_transmit_bytes_total` | counter | `node`, `device` | bytes | Per card and bridge |
| `virticle_node_network_receive_errors_total`, `_transmit_errors_total` | counter | `node`, `device` | errors | |
| `virticle_node_network_receive_drops_total`, `_transmit_drops_total` | counter | `node`, `device` | packets | |
| `virticle_node_disk_read_bytes_total`, `_written_bytes_total` | counter | `node`, `device` | bytes | Per block device (whole disks) |
| `virticle_node_disk_reads_total`, `_writes_total` | counter | `node`, `device` | operations | |
| `virticle_node_disk_busy_seconds_total` | counter | `node`, `device` | seconds | Time with requests in flight |
| `virticle_node_storage_pool_capacity_bytes`, `_allocation_bytes`, `_available_bytes` | gauge | `node`, `pool` | bytes | Read once a minute |
| `virticle_node_gpu_utilization_ratio` | gauge | `node`, `gpu`, `name` | ratio 0 to 1 | NVIDIA only, see below |
| `virticle_node_gpu_memory_used_bytes`, `_memory_total_bytes` | gauge | `node`, `gpu`, `name` | bytes | NVIDIA only |
| `virticle_node_gpu_temperature_celsius` | gauge | `node`, `gpu`, `name` | degrees | NVIDIA only |
| `virticle_vm_state` | gauge | `node`, `vm`, `uuid` | 1 running, 2 paused, 0 otherwise | Every VM the node knows |
| `virticle_vm_vcpus` | gauge | `node`, `vm`, `uuid` | count | Running VMs |
| `virticle_vm_cpu_seconds_total` | counter | `node`, `vm`, `uuid` | seconds | CPU time, all virtual CPUs |
| `virticle_vm_cpu_delay_seconds_total` | counter | `node`, `vm`, `uuid` | seconds | Time the virtual CPUs waited for a host CPU |
| `virticle_vm_memory_assigned_bytes` | gauge | `node`, `vm`, `uuid` | bytes | Memory given to the VM |
| `virticle_vm_memory_used_bytes` | gauge | `node`, `vm`, `uuid` | bytes | Used inside the guest (needs the balloon driver) |
| `virticle_vm_memory_resident_bytes` | gauge | `node`, `vm`, `uuid` | bytes | Resident on the host |
| `virticle_vm_disk_read_bytes_total`, `_written_bytes_total` | counter | `node`, `vm`, `uuid`, `device` | bytes | Per VM disk |
| `virticle_vm_disk_reads_total`, `_writes_total` | counter | same | operations | |
| `virticle_vm_disk_read_seconds_total`, `_write_seconds_total` | counter | same | seconds | Time spent on requests |
| `virticle_vm_network_receive_bytes_total`, `_transmit_bytes_total` | counter | `node`, `vm`, `uuid`, `device` | bytes | Per VM network card |
| `virticle_vm_network_receive_drops_total`, `_transmit_drops_total`, `_receive_errors_total`, `_transmit_errors_total` | counter | same | packets, errors | |

GPU: only NVIDIA cards that the node itself uses (not passed through to a VM), and only when `nvidia-smi` exists on the
node. A GPU passed through to a VM is invisible to the node. AMD and Intel GPUs are not read.

## Useful Grafana queries

```promql
# CPU use of a VM in percent of its virtual CPUs
100 * rate(virticle_vm_cpu_seconds_total{vm="web1"}[2m]) / virticle_vm_vcpus{vm="web1"}

# The five VMs using most CPU
topk(5, rate(virticle_vm_cpu_seconds_total[5m]))

# VM network in bits per second (counters are bytes)
8 * sum by (vm) (rate(virticle_vm_network_receive_bytes_total[2m]))

# Average disk latency of a VM disk in milliseconds
1000 * rate(virticle_vm_disk_read_seconds_total{vm="db1"}[2m]) / rate(virticle_vm_disk_reads_total{vm="db1"}[2m])

# Node CPU busy percent (not idle, not waiting for disk)
100 - 100 * sum by (node) (rate(virticle_node_cpu_seconds_total{mode=~"idle|iowait"}[2m])) / sum by (node) (rate(virticle_node_cpu_seconds_total[2m]))

# Node memory used
virticle_node_memory_total_bytes - virticle_node_memory_available_bytes

# Storage pool full in percent
100 * virticle_node_storage_pool_allocation_bytes / virticle_node_storage_pool_capacity_bytes

# A node that is not connected
virticle_node_up == 0
```

## Notes

- The web UI history is kept by the controller itself (7 days). Prometheus keeps its own history for as long as you
  configure it; the two do not depend on each other.
- Nothing is exposed without a token, and the token's role is only read-only.
- Cost: one libvirt call and one small command on each node every 15 seconds; a scrape adds nothing to that.
