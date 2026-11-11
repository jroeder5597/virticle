// A hardware section that is collapsed on the Hardware tab (title plus a one-line summary of its current settings)
// and shown plainly in an Edit Settings dialog. The open state is remembered per browser.
import { ReactNode, useState } from "react";
import { ChevronDown, ChevronRight } from "lucide-react";
import { getPref } from "../storage";

const key = (id: string) => `virticle.hw.open.${id}`;

function readOpen(id: string): boolean {
  try {
    return getPref(key(id), `vp.hw.open.${id}`) === "1";
  } catch {
    return false;
  }
}

export function SectionFrame(props: { id: string; title: string; summary: string; bare?: boolean; children: ReactNode }) {
  const [open, setOpen] = useState(() => readOpen(props.id));
  if (props.bare) return <section className="hwbare" aria-label={props.title}>{props.children}</section>;
  const toggle = () => {
    const next = !open;
    setOpen(next);
    try {
      localStorage.setItem(key(props.id), next ? "1" : "0");
    } catch {
      /* private window: the choice just is not remembered */
    }
  };
  const Chevron = open ? ChevronDown : ChevronRight;
  return (
    <section className="card disc-card full" id={props.id}>
      <button className="disc-head" aria-expanded={open} aria-controls={props.id + "-body"} onClick={toggle}>
        <Chevron size={16} aria-hidden="true" />
        <span className="disc-title">{props.title}</span>
        <span className="disc-sum">{props.summary}</span>
      </button>
      {open && (
        <div className="disc-body" id={props.id + "-body"}>
          {props.children}
        </div>
      )}
    </section>
  );
}

/** A warning or note in one line; the longer explanation opens on demand. */
export function OneLine(props: { tone?: "note" | "error"; text: string; details?: ReactNode; summary?: string }) {
  return (
    <div className={"oneline " + (props.tone ?? "")}>
      <span>{props.text}</span>
      {props.details && (
        <details>
          <summary>{props.summary ?? "Details"}</summary>
          <div className="muted">{props.details}</div>
        </details>
      )}
    </div>
  );
}
