// Helpers for schedule forms: the UTC time next to the browser's time zone, and the "Next Run" line.
// Used by the snapshot schedule and the backup schedule.
import { useEffect, useState } from "react";
import { browserZone, describeLocal, describeUtc, formatInstant, formatUtc, localToUtc } from "./tz";
import { HelpTip } from "./ui";
import "./schedtime.css";

/** Under a UTC time input: what the time is for the user, and a way to type the local time instead. */
export function ScheduleTimeHelper({ at, onSet }: { at: string; onSet: (utc: string) => void }) {
  const zone = browserZone();
  const [local, setLocal] = useState("");
  const now = Date.now();
  const text = describeUtc(at, now, zone);
  const back = local ? describeLocal(local, now, zone) : undefined;
  return (
    <div className="tzhelp">
      <p className="tzline" role="status" aria-label="Time zone conversion">
        {text ?? "Enter a time to see it in your time zone."}
      </p>
      <div className="tzset">
        <label>
          <span>Set from local time</span>
          <input type="time" value={local} onChange={(e) => setLocal(e.target.value)} aria-label="Local time" />
        </label>
        <button
          type="button"
          disabled={!local || !localToUtc(local, now, zone)}
          onClick={() => {
            const c = localToUtc(local, Date.now(), zone);
            if (c) onSet(c.time);
          }}
        >
          Use as UTC time
        </button>
      </div>
      {back && <p className="tzline muted">{back}</p>}
      <HelpTip label="About the time zone conversion">
        <p>The schedule is stored in UTC and does not move. When daylight saving time changes, the local time shifts by an hour.</p>
      </HelpTip>
    </div>
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

/** "Next Run: <local time> (<UTC time>)", refreshed when that moment passes; one timer, stopped while the tab is hidden. */
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
      <strong>Next Run:</strong>{" "}
      {passed ? (
        <>due now ({formatUtc(t)})</>
      ) : (
        <>
          <span className="nr-local">{formatInstant(t, zone)}</span> <span className="muted nr-utc">({formatUtc(t)})</span>
        </>
      )}
      {p.skipsStopped && <span className="muted"> Skipped while the VM is shut off.</span>}
    </p>
  );
}
