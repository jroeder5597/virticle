import { useState } from "react";
import { put } from "../api";
import { useRole } from "../auth";
import { ErrorBox, useAction } from "../ui";

export const TAG_RE = /^[A-Za-z0-9][A-Za-z0-9 ._:-]{0,30}$/;
export const MAX_TAGS = 16;

export function TagChips({ tags, onPick }: { tags: string[]; onPick?: (t: string) => void }) {
  if (!tags?.length) return null;
  return (
    <span className="tags">
      {tags.map((t) => (
        <span key={t} className={"tag" + (onPick ? " pick" : "")} onClick={onPick ? () => onPick(t) : undefined} title={onPick ? `Filter by ${t}` : undefined}>
          {t}
        </span>
      ))}
    </span>
  );
}

/** Tag chips with remove buttons and an input to add one; saves on every change. */
export function TagEditor({ tags, base, onChanged }: { tags: string[]; base: string; onChanged: () => void }) {
  const [draft, setDraft] = useState("");
  const act = useAction();
  const viewer = useRole() === "viewer";
  const save = (next: string[]) =>
    act.run(async () => {
      await put(`${base}/tags`, { tags: next });
      onChanged();
    });
  const add = async () => {
    const t = draft.trim();
    if (!t) return;
    if (!TAG_RE.test(t)) return act.run(() => Promise.reject(new Error("Tags are 1-31 characters, start with a letter or digit, and may contain letters, digits, space and . _ : -")));
    if (tags.includes(t)) return setDraft("");
    if (tags.length >= MAX_TAGS) return act.run(() => Promise.reject(new Error("At most 16 tags per VM")));
    if (await save([...tags, t])) setDraft("");
  };
  if (viewer) return tags.length ? <div className="tagedit"><TagChips tags={tags} /></div> : null;
  return (
    <div className="tagedit">
      <span className="tags">
        {tags.map((t) => (
          <span key={t} className="tag">
            {t}
            <button className="ghost" aria-label={`Remove tag ${t}`} disabled={act.busy} onClick={() => save(tags.filter((x) => x !== t))}>
              ✕
            </button>
          </span>
        ))}
      </span>
      <input
        value={draft}
        placeholder="add tag…"
        maxLength={31}
        disabled={act.busy}
        onChange={(e) => setDraft(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter") {
            e.preventDefault();
            add();
          }
        }}
        onBlur={add}
      />
      <ErrorBox error={act.error} />
    </div>
  );
}
