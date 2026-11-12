import { ReactNode, useEffect, useRef, useState } from "react";
import { ChevronDown, ChevronRight } from "lucide-react";
import "./hw.css";

/**
 * The one collapsed "Advanced" part of a dialog or form. Closed every time the dialog opens, never remembered.
 * The contents stay mounted while it is closed, so they keep their values and take part in form validation: when a
 * required or invalid field inside is hit by a submit, the section opens and that field gets focus.
 * `open` and `onToggle` are optional, for a dialog that wants to open it itself.
 */
export default function DialogAdvanced({ children, open, onToggle }: { children: ReactNode; open?: boolean; onToggle?: (open: boolean) => void }) {
  const [own, setOwn] = useState(false);
  const isOpen = open ?? own;
  const pending = useRef<HTMLElement | null>(null);
  const set = (v: boolean) => {
    setOwn(v);
    onToggle?.(v);
  };
  useEffect(() => {
    const el = pending.current;
    if (el && isOpen) {
      pending.current = null;
      el.focus();
      (el as HTMLInputElement).reportValidity?.();
    }
  }, [isOpen]);
  const Chevron = isOpen ? ChevronDown : ChevronRight;
  return (
    <div
      className="dlg-adv"
      onInvalidCapture={(e) => {
        if (isOpen || pending.current) return;
        pending.current = e.target as HTMLElement;
        set(true);
      }}
    >
      <button type="button" className="disc-head" aria-expanded={isOpen} onClick={() => set(!isOpen)}>
        <Chevron size={16} aria-hidden="true" />
        <span className="disc-title">Advanced</span>
      </button>
      <div className="dlgadv" hidden={!isOpen}>{children}</div>
    </div>
  );
}
