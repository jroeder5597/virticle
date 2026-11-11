// The one tag chip. A tag with a colour gets it as background and the text colour that keeps it readable
// (WCAG AA); an uncoloured tag keeps the neutral chip. Colours come from the shared store (tagStore.ts).
import type { CSSProperties, ReactNode } from "react";
import { textFor } from "./tagColor";
import { useTagColors } from "./tagStore";
import "./tags.css";

export function chipStyle(color?: string): CSSProperties | undefined {
  return color ? { background: color, color: textFor(color) } : undefined;
}

export function TagChip({ name, color, className, children, title }: { name: string; color?: string; className?: string; children?: ReactNode; title?: string }) {
  return (
    <span className={"tag" + (color ? " colored" : "") + (className ? " " + className : "")} style={chipStyle(color)} title={title}>
      {children ?? name}
    </span>
  );
}

/** Read-only chips for a list of tag names; with `onPick` each chip is a filter button. */
export function TagChips({ tags, onPick }: { tags?: string[]; onPick?: (t: string) => void }) {
  const colors = useTagColors();
  if (!tags?.length) return null;
  return (
    <span className="tags">
      {tags.map((t) =>
        onPick ? (
          <span
            key={t}
            className={"tag pick" + (colors[t] ? " colored" : "")}
            role="button"
            tabIndex={0}
            title={`Filter by ${t}`}
            style={chipStyle(colors[t])}
            onClick={() => onPick(t)}
            onKeyDown={(e) => {
              if (e.key === "Enter" || e.key === " ") {
                e.preventDefault();
                onPick(t);
              }
            }}
          >
            {t}
          </span>
        ) : (
          <TagChip key={t} name={t} color={colors[t]} />
        ),
      )}
    </span>
  );
}
