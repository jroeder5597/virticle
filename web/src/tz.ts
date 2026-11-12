// UTC and browser time zone conversion for schedule times. Pure functions: `now` and the zone are arguments.
// Schedules are stored in UTC; the conversion is made for the NEXT occurrence, so daylight saving is right for that date.

export const browserZone = () => Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";

const pad = (n: number) => String(n).padStart(2, "0");

interface Wall {
  y: number;
  mo: number;
  d: number;
  h: number;
  mi: number;
}

/** The wall clock of an instant in a zone. */
function wallIn(t: number, zone: string): Wall {
  const parts = new Intl.DateTimeFormat("en-US", { timeZone: zone, hourCycle: "h23", year: "numeric", month: "numeric", day: "numeric", hour: "numeric", minute: "numeric" }).formatToParts(new Date(t));
  const g = (k: string) => Number(parts.find((p) => p.type === k)?.value);
  return { y: g("year"), mo: g("month"), d: g("day"), h: g("hour") % 24, mi: g("minute") };
}

/** Minutes the zone is ahead of UTC at an instant. */
export function offsetMinutes(t: number, zone: string): number {
  const w = wallIn(t, zone);
  return Math.round((Date.UTC(w.y, w.mo - 1, w.d, w.h, w.mi) - Math.floor(t / 60000) * 60000) / 60000);
}

/** Short zone name at an instant ("EST", "GMT+5:30"). */
export function zoneName(t: number, zone: string): string {
  return new Intl.DateTimeFormat("en-US", { timeZone: zone, timeZoneName: "short" }).formatToParts(new Date(t)).find((p) => p.type === "timeZoneName")?.value ?? zone;
}

const dayNo = (y: number, mo: number, d: number) => Math.round(Date.UTC(y, mo - 1, d) / 86400000);

/** Whole days the local date is ahead of the UTC date of the same instant: -1 previous day, 0 same, 1 next day. */
function dayShift(t: number, zone: string): number {
  const l = wallIn(t, zone);
  const u = new Date(t);
  return dayNo(l.y, l.mo, l.d) - dayNo(u.getUTCFullYear(), u.getUTCMonth() + 1, u.getUTCDate());
}

export const parseHM = (s: string): [number, number] | undefined => {
  const m = /^([01]\d|2[0-3]):([0-5]\d)$/.exec(s);
  return m ? [Number(m[1]), Number(m[2])] : undefined;
};

export interface Conversion {
  /** The other side's time as HH:MM. */
  time: string;
  /** -1: the other side's date is the day before, 1: the day after, 0: the same day. */
  dayShift: number;
  /** The instant of the next occurrence (ms since the epoch). */
  at: number;
  /** Zone abbreviation at that instant, on the other side. */
  zoneName: string;
}

/** The next time after `now` that the UTC clock reads `hm`, and what the clock in `zone` reads then. */
export function utcToLocal(hm: string, now: number, zone: string): Conversion | undefined {
  const p = parseHM(hm);
  if (!p) return undefined;
  const n = new Date(now);
  let at = Date.UTC(n.getUTCFullYear(), n.getUTCMonth(), n.getUTCDate(), p[0], p[1]);
  if (at <= now) at += 86400000;
  const w = wallIn(at, zone);
  return { time: `${pad(w.h)}:${pad(w.mi)}`, dayShift: dayShift(at, zone), at, zoneName: zoneName(at, zone) };
}

/**
 * The next time after `now` that the clock in `zone` reads `hm`, and what the UTC clock reads then. Around a
 * daylight-saving change the instant is found for the date that is next, so the answer is for that date.
 * A wall time that does not exist (the hour skipped in spring) is taken as the instant after the gap.
 */
export function localToUtc(hm: string, now: number, zone: string): Conversion | undefined {
  const p = parseHM(hm);
  if (!p) return undefined;
  const today = wallIn(now, zone);
  for (let add = 0; add < 3; add++) {
    const guess = Date.UTC(today.y, today.mo - 1, today.d + add, p[0], p[1]);
    let at = guess - offsetMinutes(guess, zone) * 60000;
    at = guess - offsetMinutes(at, zone) * 60000; // the offset at the instant itself, across a change
    if (at > now) {
      const u = new Date(at);
      return { time: `${pad(u.getUTCHours())}:${pad(u.getUTCMinutes())}`, dayShift: dayShift(at, zone) * -1, at, zoneName: "UTC" };
    }
  }
  return undefined;
}

/** A moment as "Tue 3 Mar 2026, 22:00 EST" in a zone. */
export function formatInstant(t: number, zone: string): string {
  const s = new Intl.DateTimeFormat("en-GB", { timeZone: zone, weekday: "short", day: "numeric", month: "short", year: "numeric", hour: "2-digit", minute: "2-digit", hourCycle: "h23" }).format(new Date(t));
  return `${s} ${zoneName(t, zone)}`;
}
