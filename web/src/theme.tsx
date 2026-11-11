// Colour theme: dark by default, light on request, remembered per browser.
// public/theme-init.js applies the stored choice before first paint.
import { useSyncExternalStore } from "react";
import { Moon, Sun } from "lucide-react";

export type Theme = "dark" | "light";
const KEY = "vp-theme";
const META: Record<Theme, string> = { dark: "#2C3942", light: "#F8F8F8" };

const current = (): Theme => (document.documentElement.getAttribute("data-theme") === "light" ? "light" : "dark");

function subscribe(cb: () => void) {
  const obs = new MutationObserver(cb);
  obs.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
  return () => obs.disconnect();
}

export function applyTheme(t: Theme) {
  const d = document.documentElement;
  d.setAttribute("data-theme", t);
  d.style.colorScheme = t;
  document.querySelector('meta[name="theme-color"]')?.setAttribute("content", META[t]);
  try {
    localStorage.setItem(KEY, t);
  } catch {
    /* private mode: the choice just does not persist */
  }
}

/** The active theme; re-renders when it changes. */
export function useTheme(): { theme: Theme; setTheme: (t: Theme) => void; toggle: () => void } {
  const theme = useSyncExternalStore(subscribe, current, () => "dark" as Theme);
  return { theme, setTheme: applyTheme, toggle: () => applyTheme(current() === "dark" ? "light" : "dark") };
}

/** Reads a CSS variable of the active theme, for canvas/JS consumers. Call again after a theme change. */
export const cssVar = (name: string, fallback = "") => getComputedStyle(document.documentElement).getPropertyValue(name).trim() || fallback;

export function ThemeToggle() {
  const { theme, toggle } = useTheme();
  const to = theme === "dark" ? "light" : "dark";
  return (
    <button className="ghost icon-btn theme-toggle" onClick={toggle} aria-label={`Switch to ${to} theme`} title={`Switch to ${to} theme`}>
      {theme === "dark" ? <Sun size={16} aria-hidden="true" /> : <Moon size={16} aria-hidden="true" />}
    </button>
  );
}
