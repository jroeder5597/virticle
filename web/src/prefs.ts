// Per-user UI preferences. The server (GET/PUT /api/me/prefs) is the source of truth, so a choice follows the
// user to another browser. A copy in localStorage, per user name, only avoids a flash of the wrong state on
// load: it is shown at once and replaced by the server's value when that arrives.
// A change applies immediately and is saved in the background; if the save fails the UI keeps the choice
// and the next change sends it again. No error is shown for a preference.
import { useSyncExternalStore } from "react";
import { get, put } from "./api";

type Prefs = Record<string, string>;
const MIRROR = "vp-prefs:";
const subs = new Set<() => void>();
let user = "";
let values: Prefs = {};
const unsaved = new Set<string>(); // changed here, not yet confirmed by the server
let saving = false;

const mirrorKey = () => MIRROR + user;
function readMirror(): Prefs {
  try {
    const m = JSON.parse(localStorage.getItem(mirrorKey()) ?? "{}");
    return m && typeof m === "object" ? m : {};
  } catch {
    return {};
  }
}
function writeMirror() {
  try {
    localStorage.setItem(mirrorKey(), JSON.stringify(values));
  } catch {
    /* storage unavailable: the choice lasts for this page and the server keeps it */
  }
}
const notify = () => subs.forEach((f) => f());

/** Cheap and idempotent: call while rendering once the user is known, so the first paint uses the mirror. */
export function bindPrefs(name: string) {
  if (name === user) return;
  user = name;
  unsaved.clear();
  values = name ? readMirror() : {};
}

// Old per-browser setting, adopted once when the server has no value of its own.
const LEGACY: Record<string, string> = { consoleClick: "vp-console-click" };

/** Fetch the server's values and reconcile; choices made in the meantime win. */
export async function loadPrefs() {
  const who = user;
  let remote: Prefs;
  try {
    remote = await get<Prefs>("/me/prefs");
  } catch {
    return; // keep the mirror; the next sign-in or change tries again
  }
  if (who !== user) return;
  const next: Prefs = { ...remote };
  for (const k of unsaved) if (values[k] !== undefined) next[k] = values[k];
  for (const [k, legacyKey] of Object.entries(LEGACY)) {
    if (next[k] !== undefined) continue;
    try {
      const v = localStorage.getItem(legacyKey);
      if (v) {
        next[k] = v;
        unsaved.add(k);
      }
    } catch {
      /* ignore */
    }
  }
  values = next;
  writeMirror();
  notify();
  if (unsaved.size) void save();
}

async function save() {
  if (saving) return;
  saving = true;
  try {
    while (unsaved.size) {
      const keys = [...unsaved];
      const body: Prefs = {};
      keys.forEach((k) => (body[k] = values[k]));
      try {
        await put("/me/prefs", body);
      } catch {
        return; // kept in `unsaved`: sent again with the next change
      }
      keys.forEach((k) => values[k] === body[k] && unsaved.delete(k));
    }
  } finally {
    saving = false;
  }
}

export function setPref(key: string, value: string) {
  values = { ...values, [key]: value };
  unsaved.add(key);
  writeMirror();
  notify();
  void save();
}

export const getPref = (key: string): string | undefined => values[key];

/** One preference; re-renders when it changes. */
export function usePref(key: string): string | undefined {
  return useSyncExternalStore(
    (cb) => {
      subs.add(cb);
      return () => void subs.delete(cb);
    },
    () => values[key],
    () => undefined,
  );
}
