// A themed pool picker: a native select cannot draw a capacity bar. Shows, for the selected pool and for
// every option, the name, percent used, "X used of Y" and a thin bar coloured by level (ok, warning, critical).
// Colour is never the only signal: the percentage is always written out.
import { useEffect, useState } from "react";
import Combo from "./Combo";
import { bytes, get, Pool } from "./api";
import { poolLevel } from "./poolLevel";
import "./poolselect.css";

type Limits = { warn: number; crit: number };
let limits: Limits | undefined;
let limitsAsked: Promise<void> | undefined;

/** The warning and critical limits the controller raises pool alerts at (fetched once per page load). */
function usePoolLimits(): Limits {
  const [v, setV] = useState<Limits>(limits ?? { warn: 85, crit: 95 });
  useEffect(() => {
    if (limits) return;
    limitsAsked ??= get<{ poolWarn?: number; poolCritical?: number }>("/alerts").then(
      (r) => {
        limits = { warn: r.poolWarn || 85, crit: r.poolCritical || 95 };
      },
      () => {
        limitsAsked = undefined;
      },
    );
    let live = true;
    limitsAsked.then(() => live && limits && setV(limits));
    return () => {
      live = false;
    };
  }, []);
  return v;
}

function PoolFace({ p, need, lim }: { p: Pool; need?: number; lim: Limits }) {
  const u = poolLevel(p.allocation, p.capacity, lim.warn, lim.crit);
  const inactive = p.state !== "running";
  const short = !inactive && need !== undefined && p.capacity > 0 && p.available < need;
  return (
    <span className="ps-face">
      <span className="ps-row">
        <span className="ps-name">{p.name}</span>
        <span className="num ps-pct">{inactive ? "inactive" : `${u.pct.toFixed(0)}% used`}</span>
      </span>
      <span className="ps-row ps-sub">
        <span className="num ps-use">
          {bytes(p.allocation)} used of {bytes(p.capacity)}
        </span>
        {short && <span className="ps-warn">not enough free space</span>}
      </span>
      <span className="ps-bar" aria-hidden="true">
        <span className={"ps-fill " + u.level} style={{ width: u.pct + "%" }} />
      </span>
    </span>
  );
}

/**
 * `pools` is the list the dialog already loaded. `needBytes` marks pools with less free space than the
 * requested size; they stay selectable (the server decides). Inactive pools are listed but cannot be chosen.
 */
export default function PoolSelect(props: { pools: Pool[]; value: string; onChange: (name: string) => void; label: string; needBytes?: number; disabled?: boolean }) {
  const lim = usePoolLimits();
  const items = props.pools.map((p) => ({ key: p.name, disabled: p.state !== "running", pool: p }));
  return (
    <Combo
      items={items}
      value={props.value}
      onChange={props.onChange}
      label={props.label}
      disabled={props.disabled}
      face={(i) => (i ? <PoolFace p={i.pool} need={props.needBytes} lim={lim} /> : <span className="ps-face muted">No pool</span>)}
      option={(i) => <PoolFace p={i.pool} need={props.needBytes} lim={lim} />}
    />
  );
}
