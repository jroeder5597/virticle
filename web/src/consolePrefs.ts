// What a click on the console preview does. Per browser, like the theme.
import { useSyncExternalStore } from "react";
import { getPref } from "./storage";

export type ClickMode = "tab" | "inline";
const KEY = "virticle-console-click";
const listeners = new Set<() => void>();
let fallback: ClickMode | undefined; // the choice when storage is unavailable

const read = (): ClickMode => {
  try {
    const v = getPref(KEY, "vp-console-click");
    if (v === "inline" || v === "tab") return v;
  } catch {
    /* storage blocked */
  }
  return fallback ?? "tab";
};

export function setClickMode(m: ClickMode) {
  fallback = m;
  try {
    localStorage.setItem(KEY, m);
  } catch {
    /* private mode: the choice lasts until reload */
  }
  listeners.forEach((l) => l());
}

/** The current choice; every component showing it updates when it changes. */
export function useClickMode(): [ClickMode, (m: ClickMode) => void] {
  const mode = useSyncExternalStore(
    (cb) => {
      listeners.add(cb);
      window.addEventListener("storage", cb);
      return () => {
        listeners.delete(cb);
        window.removeEventListener("storage", cb);
      };
    },
    read,
    () => "tab" as ClickMode,
  );
  return [mode, setClickMode];
}

export const CLICK_LABELS: Record<ClickMode, string> = { tab: "in a new tab", inline: "inside the card" };
