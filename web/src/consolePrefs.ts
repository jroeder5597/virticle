// What a click on the console preview does. A per-user preference (web/src/prefs.ts), kept on the server.
import { setPref, usePref } from "./prefs";

export type ClickMode = "tab" | "inline";

export const setClickMode = (m: ClickMode) => setPref("consoleClick", m);

/** The current choice; every component showing it updates when it changes. */
export function useClickMode(): [ClickMode, (m: ClickMode) => void] {
  const v = usePref("consoleClick");
  return [v === "inline" ? "inline" : "tab", setClickMode];
}

export const CLICK_LABELS: Record<ClickMode, string> = { tab: "in a new tab", inline: "inside the card" };
