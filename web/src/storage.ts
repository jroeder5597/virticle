// Reads a per-browser setting. When the setting is not stored under `key` yet
// but is under `legacyKey` (what versions before the rename used), the value
// is moved over once, so users keep their theme and tree state.
// Throws when storage is blocked, like localStorage itself.
export function getPref(key: string, legacyKey: string): string | null {
  const v = localStorage.getItem(key);
  if (v !== null) return v;
  const old = localStorage.getItem(legacyKey);
  if (old === null) return null;
  try {
    localStorage.setItem(key, old);
    localStorage.removeItem(legacyKey);
  } catch {
    /* read-only storage: the old value is still used */
  }
  return old;
}
