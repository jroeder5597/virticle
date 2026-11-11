// Pool fullness, graded by the same limits as the pool-usage alerts.
export type PoolLevel = "ok" | "warn" | "crit";

export interface PoolUse {
  /** Used fraction in percent, 0 for a pool of unknown size. */
  pct: number;
  level: PoolLevel;
}

/** Percent used and the level: below `warn` ok, from `warn` up to `crit` warn, `crit` and above crit. */
export function poolLevel(used: number, total: number, warn = 85, crit = 95): PoolUse {
  if (!(total > 0) || !(used > 0)) return { pct: 0, level: "ok" };
  const pct = Math.min(100, (used / total) * 100);
  return { pct, level: pct >= crit ? "crit" : pct >= warn ? "warn" : "ok" };
}
