// Tag colours for the whole UI, from ONE GET /api/tags shared by the tree, the list and the VM page.
// Loaded when the first chip mounts, refreshed (debounced, and only while the tab is visible) when a
// "tags" event arrives or the event socket reconnects. No polling.
import { useSyncExternalStore } from "react";
import { del, get, put } from "./api";
import { onEvent } from "./live";

export interface TagInfo {
  name: string;
  color?: string;
  count: number;
}

type Colors = Record<string, string>;
let colors: Colors = {};
let loaded = false;
let inflight = false;
let stale = false;
let timer: number | undefined;
const subs = new Set<() => void>();
let off: (() => void) | undefined;

function setColors(next: Colors) {
  if (JSON.stringify(next) === JSON.stringify(colors)) return;
  colors = next;
  subs.forEach((f) => f());
}

async function load() {
  if (inflight) {
    stale = true;
    return;
  }
  inflight = true;
  try {
    const list = await get<TagInfo[]>("/tags");
    const next: Colors = {};
    for (const t of list) if (t.color) next[t.name] = t.color;
    setColors(next);
    loaded = true;
  } catch {
    /* chips stay neutral; the next event or mount tries again */
  } finally {
    inflight = false;
    if (stale) {
      stale = false;
      later();
    }
  }
}

function later() {
  if (document.visibilityState === "hidden") {
    pendingWhileHidden = true;
    return;
  }
  window.clearTimeout(timer);
  timer = window.setTimeout(load, 250);
}
let pendingWhileHidden = false;
const shown = () => {
  if (document.visibilityState !== "hidden" && pendingWhileHidden && subs.size) {
    pendingWhileHidden = false;
    load();
  }
};

function subscribe(f: () => void) {
  subs.add(f);
  if (subs.size === 1) {
    off = onEvent((e) => {
      if (e.type === "tags" || (e.type === "node" && e.detail === "resync")) later();
    });
    document.addEventListener("visibilitychange", shown);
  }
  if (!loaded && !inflight) load();
  return () => {
    subs.delete(f);
    if (subs.size === 0) {
      off?.();
      document.removeEventListener("visibilitychange", shown);
    }
  };
}

/** Colour by tag name (#rrggbb); tags without an entry are uncoloured. */
export function useTagColors(): Colors {
  return useSyncExternalStore(subscribe, () => colors);
}

/** Sets (or, with null, removes) a tag's colour for every VM. Throws the server's message on failure. */
export async function setTagColor(name: string, color: string | null) {
  const path = `/tags/${encodeURIComponent(name)}/color`;
  if (color) await put(path, { color });
  else await del(path);
  const next = { ...colors };
  if (color) next[name] = color.toLowerCase();
  else delete next[name];
  setColors(next);
}

/** Every tag in use, with its colour (fetched fresh; for the suggestion list). */
export const fetchTags = () => get<TagInfo[]>("/tags");
