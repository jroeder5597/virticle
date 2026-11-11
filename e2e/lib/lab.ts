// Runs a shell command inside a guest of the lab through the node's libvirt guest agent, from the machine that runs
// the suite. It needs the lab's ssh config (../lab/ssh_config) and the jump host password in SSHPASS; tests that need
// it call labAvailable() and skip when it is not there. The app itself has no "run a command in the guest" API.
import { execFileSync } from "child_process";
import { resolve } from "path";

// The checkout that holds lab/ssh_config and the lab keys (a git worktree has no keys: point E2E_LAB_ROOT at the main checkout).
const root = process.env.E2E_LAB_ROOT ?? resolve(__dirname, "../..");

export const labAvailable = () => !!process.env.SSHPASS;

const PY = `
import json, subprocess, sys, time, base64
vm, cmd = sys.argv[1], sys.argv[2]
def q(c):
    return json.loads(subprocess.check_output(['virsh', 'qemu-agent-command', vm, json.dumps(c)]))
pid = q({'execute': 'guest-exec', 'arguments': {'path': '/bin/sh', 'arg': ['-c', cmd], 'capture-output': True}})['return']['pid']
for i in range(90):
    s = q({'execute': 'guest-exec-status', 'arguments': {'pid': pid}})['return']
    if s['exited']:
        sys.stdout.write(base64.b64decode(s.get('out-data', '')).decode() + base64.b64decode(s.get('err-data', '')).decode())
        sys.exit(s.get('exitcode', 0))
    time.sleep(1)
sys.exit(99)
`;

/** Output of `sh -c cmd` in guest `vm` (a libvirt domain name) on lab node `alias` (an ssh alias such as n1). */
export function guestSh(alias: string, vm: string, cmd: string): string {
  const sq = (s: string) => "'" + s.replace(/'/g, "'\\''") + "'"; // the remote shell must not expand anything
  return execFileSync("ssh", ["-F", "lab/ssh_config", "-o", "ConnectTimeout=20", alias, `python3 -c ${sq(PY)} ${sq(vm)} ${sq(cmd)}`], { cwd: root, encoding: "utf8", timeout: 120_000 });
}
