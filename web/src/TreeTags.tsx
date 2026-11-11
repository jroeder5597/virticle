// Tags on a tree row: small chips after the VM name, on the row's one fixed-height line. They never wrap and
// never widen the sidebar. The chips that do not fit are hidden and an ellipsis marks that there are more;
// hovering the area (or focusing the row, or tapping it) lists ALL tags.
//
// Fitting is decided by ONE shared ResizeObserver (it fires when a row's tag box changes size, not on every
// event) plus one measurement when a row's tags change or a web font arrives; reads and writes are batched
// so a tree of many rows does not thrash the layout.
import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { chipStyle } from "./TagChip";
import { useTagColors } from "./tagStore";
import "./tags.css";

const MORE_W = 14; // room kept for the ellipsis

function fit(boxes: HTMLElement[]) {
  const strips = boxes.map((b) => b.firstElementChild as HTMLElement | null);
  // 1. show everything (inline display, not a class: React rewrites className when a colour arrives),
  strips.forEach((s) => s && Array.from(s.children).forEach((c) => ((c as HTMLElement).style.display = "")));
  // 2. read,
  const reads = boxes.map((b, i) => {
    const s = strips[i];
    if (!s) return null;
    const chips = Array.from(s.children) as HTMLElement[];
    const edges = chips.map((c) => c.offsetLeft + c.offsetWidth);
    return { avail: b.clientWidth, chips, edges, total: edges.length ? edges[edges.length - 1] : 0 };
  });
  // 3. write.
  boxes.forEach((b, i) => {
    const r = reads[i];
    if (!r) return;
    const was = !!b.dataset.over;
    if (r.total <= r.avail) delete b.dataset.over;
    else {
      b.dataset.over = "1";
      // the first chip always stays (clipped if need be), so a long tag still shows something
      r.chips.forEach((c, j) => j > 0 && r.edges[j] > r.avail - MORE_W && (c.style.display = "none"));
    }
    if (was !== !!b.dataset.over) b.dispatchEvent(new Event("tgfit"));
  });
}

let observer: ResizeObserver | undefined;
const watched = new Set<HTMLElement>();
const shared = () => (observer ??= new ResizeObserver((entries) => fit(entries.map((e) => e.target as HTMLElement))));
// A web font arriving changes the chips' widths without changing the box: measure again once.
if (typeof document !== "undefined" && document.fonts) document.fonts.addEventListener("loadingdone", () => fit([...watched]));

export const TreeTags = function TreeTags({ tags }: { tags: string[] }) {
  const colors = useTagColors();
  const box = useRef<HTMLSpanElement>(null);
  const [pop, setPop] = useState<DOMRect | null>(null);
  const [, bump] = useState(0);
  const key = tags.join("\u0000");

  useEffect(() => {
    const el = box.current!;
    shared().observe(el);
    watched.add(el);
    const f = () => bump((n) => n + 1);
    el.addEventListener("tgfit", f);
    return () => {
      shared().unobserve(el);
      watched.delete(el);
      el.removeEventListener("tgfit", f);
    };
  }, []);
  useLayoutEffect(() => {
    if (box.current) fit([box.current]);
  }, [key]);

  // List all tags while the pointer is over the area or the row has the keyboard focus. The pointer may
  // arrive before the fit (hovering shows the row's buttons, which narrows the box), so the request is
  // remembered and the list appears as soon as some tags are hidden.
  const over = () => !!box.current?.dataset.over;
  const show = () => {
    const row = box.current?.closest<HTMLElement>(".trow");
    if (row) setPop(row.getBoundingClientRect());
  };
  const hide = () => setPop(null);
  useEffect(() => {
    const row = box.current?.closest<HTMLElement>(".trow");
    if (!row) return;
    const focus = (e: Event) => e.target === row && show();
    row.addEventListener("focus", focus);
    row.addEventListener("blur", hide);
    return () => {
      row.removeEventListener("focus", focus);
      row.removeEventListener("blur", hide);
    };
  }, []);
  useEffect(() => {
    if (!pop) return;
    const esc = (e: KeyboardEvent) => e.key === "Escape" && hide();
    const away = (e: Event) => !box.current?.contains(e.target as Node) && hide();
    window.addEventListener("keydown", esc);
    window.addEventListener("pointerdown", away);
    window.addEventListener("scroll", hide, true);
    return () => {
      window.removeEventListener("keydown", esc);
      window.removeEventListener("pointerdown", away);
      window.removeEventListener("scroll", hide, true);
    };
  }, [pop]);

  return (
    <span ref={box} className="tgbox" data-testid="tree-tags" onMouseEnter={show} onMouseLeave={hide} onClick={() => (pop ? hide() : show())}>
      <span className="tgstrip">
        {tags.map((t) => (
          <span key={t} className={"tag small" + (colors[t] ? " colored" : "")} style={chipStyle(colors[t])}>
            {t}
          </span>
        ))}
      </span>
      <span className="tmore" aria-hidden="true">
        {"…"}
      </span>
      {pop && over() && <Popover at={pop} tags={tags} colors={colors} />}
    </span>
  );
};

function Popover({ at, tags, colors }: { at: DOMRect; tags: string[]; colors: Record<string, string> }) {
  const el = useRef<HTMLDivElement>(null);
  useLayoutEffect(() => {
    const p = el.current!;
    const w = p.offsetWidth,
      h = p.offsetHeight;
    const top = at.bottom + 2 + h > window.innerHeight - 4 ? at.top - 2 - h : at.bottom + 2;
    p.style.left = `${Math.max(4, Math.min(at.left + 20, window.innerWidth - w - 4))}px`;
    p.style.top = `${Math.max(4, Math.min(top, window.innerHeight - h - 4))}px`;
    p.style.visibility = "visible";
  }, [at]);
  return createPortal(
    <div ref={el} className="tagpop" role="tooltip" aria-label="All tags" style={{ visibility: "hidden" }}>
      {tags.map((t) => (
        <span key={t} className={"tag" + (colors[t] ? " colored" : "")} style={chipStyle(colors[t])}>
          {t}
        </span>
      ))}
    </div>,
    document.body,
  );
}
