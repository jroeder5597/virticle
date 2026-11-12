import { ReactNode } from "react";

/** The one collapsed "Advanced" part of a dialog. Closed by default; `open` and `onToggle` let the dialog open it itself. */
export default function DialogAdvanced({ children, open, onToggle }: { children: ReactNode; open?: boolean; onToggle?: (open: boolean) => void }) {
  return (
    <details className="cloudinit" open={open} onToggle={onToggle ? (e) => onToggle((e.target as HTMLDetailsElement).open) : undefined}>
      <summary>Advanced</summary>
      <div className="dlgadv">{children}</div>
    </details>
  );
}
