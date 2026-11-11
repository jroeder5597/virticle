// Turns text into key presses for a guest with a US keyboard layout.
// KeyboardEvent.code names are physical keys, which is what QEMU's extended
// key event wants; the keysym is what the guest's layout maps them to.

export type Stroke = { keysym: number; code: string; shift: boolean };

const SHIFTED: Record<string, string> = {
  "~": "`", "!": "1", "@": "2", "#": "3", "$": "4", "%": "5", "^": "6", "&": "7", "*": "8", "(": "9", ")": "0",
  "_": "-", "+": "=", "{": "[", "}": "]", "|": "\\", ":": ";", '"': "'", "<": ",", ">": ".", "?": "/",
};
const PUNCT: Record<string, string> = {
  "`": "Backquote", "-": "Minus", "=": "Equal", "[": "BracketLeft", "]": "BracketRight", "\\": "Backslash",
  ";": "Semicolon", "'": "Quote", ",": "Comma", ".": "Period", "/": "Slash", " ": "Space",
};
const NUM = "Digit"; // KeyboardEvent.code prefix of the number row

/** Strokes for the text, plus how many characters have no US-layout key (they are skipped). */
export function strokesFor(text: string): { strokes: Stroke[]; skipped: number } {
  const strokes: Stroke[] = [];
  let skipped = 0;
  for (const ch of text.replace(/\r\n?/g, "\n")) {
    if (ch === "\n") strokes.push({ keysym: 0xff0d, code: "Enter", shift: false });
    else if (ch === "\t") strokes.push({ keysym: 0xff09, code: "Tab", shift: false });
    else if (ch >= "a" && ch <= "z") strokes.push({ keysym: ch.charCodeAt(0), code: "Key" + ch.toUpperCase(), shift: false });
    else if (ch >= "A" && ch <= "Z") strokes.push({ keysym: ch.charCodeAt(0), code: "Key" + ch, shift: true });
    else if (ch >= "0" && ch <= "9") strokes.push({ keysym: ch.charCodeAt(0), code: NUM + ch, shift: false });
    else if (PUNCT[ch]) strokes.push({ keysym: ch.charCodeAt(0), code: PUNCT[ch], shift: false });
    else if (SHIFTED[ch]) {
      const base = SHIFTED[ch];
      strokes.push({ keysym: ch.charCodeAt(0), code: base >= "0" && base <= "9" ? NUM + base : PUNCT[base], shift: true });
    } else skipped++;
  }
  return { strokes, skipped };
}

export const XK = {
  Control_L: 0xffe3, Shift_L: 0xffe1, Alt_L: 0xffe9, Super_L: 0xffeb, Tab: 0xff09, Print: 0xff61,
  F: (n: number) => 0xffbe + n - 1,
};
export const CODE = { Control_L: "ControlLeft", Shift_L: "ShiftLeft", Alt_L: "AltLeft", Super_L: "MetaLeft", Tab: "Tab", Print: "PrintScreen" };
