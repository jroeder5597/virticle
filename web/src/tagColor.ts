// Pure colour helpers for tags: parsing, the text colour that keeps a chip readable, HSV conversion.

export const HEX_RE = /^#[0-9a-fA-F]{6}$/;

/** Ten presets; the chip text colour is chosen per colour, so they work on both themes. */
export const PRESETS = ["#e5484d", "#f76808", "#ffc53d", "#30a46c", "#12a594", "#0091ff", "#6e56cf", "#d6409f", "#8b8d98", "#3b4a54"];

const DARK_TEXT = "#0b0f12";
const LIGHT_TEXT = "#ffffff";

export function rgb(hex: string): [number, number, number] {
  const n = parseInt(hex.slice(1), 16);
  return [(n >> 16) & 255, (n >> 8) & 255, n & 255];
}

export function luminance(hex: string): number {
  const f = (v: number) => {
    v /= 255;
    return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4);
  };
  const [r, g, b] = rgb(hex);
  return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b);
}

/** WCAG contrast ratio between two #rrggbb colours. */
export function contrast(a: string, b: string): number {
  const la = luminance(a),
    lb = luminance(b);
  return (Math.max(la, lb) + 0.05) / (Math.min(la, lb) + 0.05);
}

/**
 * The text colour for a chip of this background: white or a near-black, whichever contrasts more. Where
 * neither reaches 4.5:1 (mid-tones near the crossover) pure black is used, which always does.
 */
export function textFor(bg: string): string {
  const w = contrast(bg, LIGHT_TEXT),
    d = contrast(bg, DARK_TEXT);
  if (Math.max(w, d) >= 4.5) return w >= d ? LIGHT_TEXT : DARK_TEXT;
  return "#000000";
}

export function hsvToHex(h: number, s: number, v: number): string {
  h = ((h % 360) + 360) % 360;
  const c = v * s,
    x = c * (1 - Math.abs(((h / 60) % 2) - 1)),
    m = v - c;
  const [r, g, b] = h < 60 ? [c, x, 0] : h < 120 ? [x, c, 0] : h < 180 ? [0, c, x] : h < 240 ? [0, x, c] : h < 300 ? [x, 0, c] : [c, 0, x];
  const t = (u: number) => Math.round((u + m) * 255).toString(16).padStart(2, "0");
  return "#" + t(r) + t(g) + t(b);
}

export function hexToHsv(hex: string): [number, number, number] {
  const [r, g, b] = rgb(hex).map((u) => u / 255);
  const max = Math.max(r, g, b),
    min = Math.min(r, g, b),
    d = max - min;
  let h = 0;
  if (d) h = max === r ? ((g - b) / d + 6) % 6 : max === g ? (b - r) / d + 2 : (r - g) / d + 4;
  return [h * 60, max ? d / max : 0, max];
}
