// Helpers for schedule forms: the "When" fields in the browser's local time, and the "Next Run" line.
// Used by the snapshot schedule and the backup schedule. The server stores and runs daily times in UTC.
import { useEffect, useState } from "react";
import Select from "./Select";
import { Field } from "./ui";
import { browserZone, formatInstant, localToUtc, utcToLocal } from "./tz";
import "./schedtime.css";

/** What the user enters: a daily time in local time, or a number of hours. */
export interface When {
  mode: "daily" | "interval";
  /** HH:MM, browser time. */
  at: string;
  hours: number;
  /** An existing interval that is not a whole number of hours, in minutes. */
  oddMinutes?: number;
}

interface Stored {
  mode: "daily" | "interval";
  at?: string;
  everyMinutes?: number;
}

/** The form values of a stored schedule (UTC time becomes local; minutes become hours, rounded, at least 1). */
export function whenFrom(s?: Stored): When {
  // A new schedule starts at 03:00 local time; a stored daily time is shown in local time.
  const at = s?.mode === "daily" && s.at ? (utcToLocal(s.at, Date.now(), browserZone())?.time ?? s.at) : "03:00";
  const min = s?.mode === "interval" ? (s.everyMinutes ?? 60) : 60;
  return { mode: s?.mode ?? "daily", at, hours: Math.max(1, Math.round(min / 60)), oddMinutes: min % 60 === 0 ? undefined : min };
}

/** The fields to send: the local time becomes UTC, hours become minutes. */
export function whenBody(w: When): Stored {
  if (w.mode === "interval") return { mode: "interval", everyMinutes: Math.round(w.hours) * 60 };
  return { mode: "daily", at: localToUtc(w.at, Date.now(), browserZone())?.time ?? w.at };
}

/** "Daily at 22:00" or "Every 6 hours" for a stored schedule, in local time. */
export function describeWhen(s: Stored): string {
  if (s.mode === "interval") {
    const m = s.everyMinutes ?? 0;
    return m % 60 === 0 ? `Every ${m / 60} ${m === 60 ? "hour" : "hours"}` : `Every ${m} minutes`;
  }
  const at = s.at ?? "";
  return `Daily at ${utcToLocal(at, Date.now(), browserZone())?.time ?? at}`;
}

/** The "When" and "Time" or "Every" fields of a schedule form. */
export function ScheduleWhen({ value, onChange }: { value: When; onChange: (w: When) => void }) {
  return (
    <>
      <Field label="When">
        <Select
          label="When"
          value={value.mode}
          onChange={(v) => onChange({ ...value, mode: v as When["mode"] })}
          options={[
            { value: "daily", label: "Daily at a fixed time" },
            { value: "interval", label: "At a fixed interval" },
          ]}
        />
      </Field>
      {value.mode === "daily" ? (
        <Field label="Time">
          <input type="time" value={value.at} onChange={(e) => onChange({ ...value, at: e.target.value })} />
        </Field>
      ) : (
        <Field
          label="Every (hours)"
          hint={value.oddMinutes ? `This schedule ran every ${value.oddMinutes} minutes; saving rounds that to whole hours.` : undefined}
        >
          <input type="number" min={1} max={8784} value={value.hours} onChange={(e) => onChange({ ...value, hours: Number(e.target.value) })} />
        </Field>
      )}
    </>
  );
}

export interface NextRunProps {
  /** Unix seconds, from the server (the same code the scheduler decides with); absent when no run is planned. */
  nextRun?: number;
  runner?: "this" | "disabled" | "other";
  otherController?: string;
  enabled: boolean;
  /** Seconds between scheduler passes. */
  tick?: number;
  /** A shut-off VM gets no snapshot. */
  skipsStopped?: boolean;
  onElapsed?: () => void;
}

/** "Next Run: <local time>", refreshed when that moment passes; one timer, stopped while the tab is hidden. */
export function NextRunLine(p: NextRunProps) {
  const [, tick] = useState(0);
  useEffect(() => {
    if (!p.nextRun) return;
    const at = p.nextRun * 1000;
    let h: ReturnType<typeof setTimeout> | undefined;
    const stop = () => {
      if (h) clearTimeout(h);
      h = undefined;
    };
    const arm = () => {
      stop();
      const ms = at - Date.now();
      if (ms <= 0 || document.visibilityState === "hidden") return;
      h = setTimeout(
        () => {
          if (Date.now() < at) return arm(); // a very long wait is done in steps
          tick((n) => n + 1);
          p.onElapsed?.();
        },
        Math.min(ms, 2 ** 31 - 1),
      );
    };
    const onVis = () => {
      if (document.visibilityState === "hidden") stop();
      else {
        tick((n) => n + 1);
        arm();
      }
    };
    arm();
    document.addEventListener("visibilitychange", onVis);
    return () => {
      stop();
      document.removeEventListener("visibilitychange", onVis);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [p.nextRun]);

  const zone = browserZone();
  if (p.runner === "disabled")
    return (
      <p className="nextrun" role="status">
        <strong>Next Run:</strong> none (this controller runs with -no-scheduler)
      </p>
    );
  if (p.runner === "other")
    return (
      <p className="nextrun" role="status">
        <strong>Next Run:</strong> none here, another controller{p.otherController ? ` (${p.otherController})` : ""} runs it
      </p>
    );
  if (!p.enabled) return <p className="nextrun"><strong>Next Run:</strong> none (paused)</p>;
  if (!p.nextRun) return null;
  const t = p.nextRun * 1000;
  const passed = Date.now() >= t;
  return (
    <p className="nextrun" role="status">
      <strong>Next Run:</strong> {passed ? <>due now</> : <span className="nr-local">{formatInstant(t, zone)}</span>}
      {p.skipsStopped && <span className="muted"> Skipped while the VM is shut off.</span>}
    </p>
  );
}
