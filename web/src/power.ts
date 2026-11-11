// Power actions: one set of labels and confirmation texts for the VM page, the tree and the lists.
import type { ConfirmSpec } from "./ui";

export type PowerAction = "start" | "shutdown" | "poweroff" | "reboot" | "reset" | "pause" | "resume";

export const powerLabel: Record<PowerAction, string> = {
  start: "Power On",
  shutdown: "Shutdown (graceful)",
  poweroff: "Halt (Force Shutdown)",
  reboot: "Reboot (Graceful)",
  reset: "Reset (Hard Reboot)",
  pause: "Pause",
  resume: "Resume",
};

const what: Partial<Record<PowerAction, { title: string; confirm: string; one: (n: string) => string; many: string }>> = {
  shutdown: {
    title: "Shutdown (graceful)",
    confirm: "Shutdown",
    one: (n) => `${n} is asked to shut down through ACPI, as if its power button were pressed. The guest closes its programs and powers off. A guest that ignores the request keeps running.`,
    many: "These VMs are asked to shut down through ACPI. The guests close their programs and power off.",
  },
  reboot: {
    title: "Reboot (Graceful)",
    confirm: "Reboot",
    one: (n) => `${n} is asked to restart through ACPI. The guest closes its programs, restarts and is unavailable for a short while.`,
    many: "These VMs are asked to restart. The guests close their programs and are unavailable for a short while.",
  },
  reset: {
    title: "Reset (Hard Reboot)",
    confirm: "Reset",
    one: (n) => `${n} is reset like pressing the hardware reset button. The guest restarts at once without closing anything: unsaved data is lost.`,
    many: "These VMs are reset like pressing the hardware reset button. The guests restart at once: unsaved data is lost.",
  },
  poweroff: {
    title: "Halt (Force Shutdown)",
    confirm: "Halt",
    one: (n) => `${n} is powered off at once, like pulling the plug. Unsaved data in the guest is lost and its disks may need a repair at the next start.`,
    many: "These VMs are powered off at once, like pulling the plug. Unsaved data in the guests is lost.",
  },
};

/** The confirmation for a power action, or undefined for actions that are not destructive. */
export function powerConfirm(a: string, names: string[]): ConfirmSpec | undefined {
  const w = what[a as PowerAction];
  if (!w || names.length === 0) return undefined;
  return names.length === 1
    ? { title: w.title, message: w.one(names[0]), confirm: w.confirm }
    : { title: `${w.title}: ${names.length} VMs`, message: w.many, confirm: w.confirm, items: names };
}
