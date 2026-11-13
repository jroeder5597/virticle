// All configuration comes from the environment so the suite can run against any lab.
export const env = {
  base: process.env.E2E_BASE_URL ?? "http://127.0.0.1:8095",
  user: process.env.E2E_USER ?? "admin",
  pass: process.env.E2E_PASS ?? "",
  // Hostnames (or addresses) of the two lab nodes as the controller lists them.
  node1: process.env.E2E_NODE1 ?? "vspike-n1",
  node2: process.env.E2E_NODE2 ?? "vspike-n2",
  // The ssh alias (in lab/ssh_config) of node 1, for the tests that run a command on the node with `virsh` (guest clock).
  sshNode1: process.env.E2E_SSH_NODE1 ?? "n1",
  // Every object the suite creates starts with this; the global teardown removes only such objects.
  prefix: process.env.E2E_PREFIX ?? "e2e-",
  // Existing images in pool "default" on node 1 (never modified).
  cirros: process.env.E2E_CIRROS ?? "cirros-0.6.3.qcow2",
  debian: process.env.E2E_DEBIAN ?? "wa-debian13.qcow2",
};
export const P = env.prefix;
export const authFile = ".auth/admin.json";
