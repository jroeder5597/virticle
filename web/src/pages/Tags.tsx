import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { X } from "lucide-react";
import { put } from "../api";
import { useRole } from "../auth";
import { ErrorBox, useAction, useConfirm } from "../ui";
import ColorPicker from "../ColorPicker";
import { chipStyle, TagChip, TagChips } from "../TagChip";
import { fetchTags, setTagColor, TagInfo, useTagColors } from "../tagStore";

export { TagChips };
export const TAG_RE = /^[A-Za-z0-9][A-Za-z0-9 ._:-]{0,30}$/;
export const MAX_TAGS = 16;
const TAG_RULES = "Tags are 1-31 characters, start with a letter or digit, and may contain letters, digits, space and . _ : -";
const SHOWN = 8; // suggestions listed at most: the list never scrolls

/**
 * The tags of a VM, shown next to its name. Operators click a chip for its colour, remove one with the x
 * (visible on hover and keyboard focus, then asks first) and add with the "+ Add Tag" button, which turns
 * into an input that suggests the tags that already exist, with their colours. Viewers see plain chips.
 */
export function TagEditor({ tags, base, vmName, onChanged }: { tags: string[]; base: string; vmName: string; onChanged: () => void }) {
  const act = useAction();
  const confirm = useConfirm();
  const viewer = useRole() === "viewer";
  const colors = useTagColors();
  const [adding, setAdding] = useState(false);
  const [draft, setDraft] = useState("");
  const [known, setKnown] = useState<TagInfo[]>([]);
  const [active, setActive] = useState(-1);
  const [picker, setPicker] = useState<{ tag: string; el: HTMLElement } | null>(null);
  const addBtn = useRef<HTMLButtonElement>(null);
  const input = useRef<HTMLInputElement>(null);
  const sugg = useRef<HTMLUListElement>(null);

  // The tags that exist, fetched once when the input opens.
  useEffect(() => {
    if (adding) fetchTags().then(setKnown, () => {});
  }, [adding]);

  const save = (next: string[]) =>
    act.run(async () => {
      await put(`${base}/tags`, { tags: next });
      onChanged();
    });
  const closeAdd = () => {
    setAdding(false);
    setDraft("");
    setActive(-1);
    requestAnimationFrame(() => addBtn.current?.focus());
  };
  const assign = async (t: string) => {
    t = t.trim();
    if (!t) return;
    if (!TAG_RE.test(t)) return act.run(() => Promise.reject(new Error(TAG_RULES)));
    if (tags.includes(t)) return closeAdd();
    if (tags.length >= MAX_TAGS) return act.run(() => Promise.reject(new Error("At most 16 tags per VM")));
    if (await save([...tags, t])) closeAdd();
  };
  const remove = async (t: string) => {
    const ok = await confirm.ask({ title: "Remove tag", message: `Remove tag ${t} from VM ${vmName}?`, confirm: "Remove tag" });
    if (ok) save(tags.filter((x) => x !== t));
  };

  // keep the suggestion list inside the window
  useLayoutEffect(() => {
    const u = sugg.current;
    if (!u) return;
    u.style.left = "0px";
    const over = u.getBoundingClientRect().right - (window.innerWidth - 4);
    if (over > 0) u.style.left = `${-over}px`;
  });

  if (viewer) return tags.length ? <span className="tagedit"><TagChips tags={tags} /></span> : null;

  const needle = draft.trim().toLowerCase();
  const matches = known.filter((k) => !tags.includes(k.name) && (!needle || k.name.toLowerCase().includes(needle)));
  const list = matches.slice(0, SHOWN);

  return (
    <span className="tagedit">
      {tags.map((t) => (
        <span key={t} className={"tag editable" + (colors[t] ? " colored" : "") + (picker?.tag === t ? " open" : "")} style={chipStyle(colors[t])} data-tag={t}>
          <ChipBody name={t} busy={act.busy} onColor={(el) => setPicker(picker?.tag === t ? null : { tag: t, el })} onRemove={() => remove(t)} />
        </span>
      ))}
      {adding ? (
        <span className="tagadd">
          <input
            ref={input}
            autoFocus
            value={draft}
            placeholder="Tag name"
            aria-label="Tag name"
            aria-autocomplete="list"
            aria-expanded={list.length > 0}
            aria-controls="tag-suggest"
            role="combobox"
            maxLength={31}
            disabled={act.busy}
            onChange={(e) => {
              setDraft(e.target.value);
              setActive(-1);
            }}
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                e.preventDefault();
                assign(active >= 0 && list[active] ? list[active].name : draft);
              } else if (e.key === "ArrowDown") {
                e.preventDefault();
                setActive((a) => Math.min(list.length - 1, a + 1));
              } else if (e.key === "ArrowUp") {
                e.preventDefault();
                setActive((a) => Math.max(-1, a - 1));
              } else if (e.key === "Escape") {
                e.preventDefault();
                closeAdd();
              }
            }}
            onBlur={() => !draft.trim() && closeAdd()}
          />
          {list.length > 0 && (
            <ul ref={sugg} id="tag-suggest" className="tagsuggest" role="listbox" aria-label="Existing tags" onMouseDown={(e) => e.preventDefault()}>
              {list.map((k, i) => (
                <li key={k.name} role="option" aria-selected={i === active} className={i === active ? "on" : ""} onClick={() => assign(k.name)}>
                  <TagChip name={k.name} color={k.color} />
                  <span className="muted">{k.count}</span>
                </li>
              ))}
              {matches.length > SHOWN && <li className="more muted">Keep typing to narrow the list.</li>}
            </ul>
          )}
        </span>
      ) : (
        <button ref={addBtn} type="button" className="tagaddbtn" disabled={act.busy} onClick={() => setAdding(true)}>
          + Add Tag
        </button>
      )}
      <ErrorBox error={act.error} />
      {confirm.dialog}
      {picker && (
        <ColorPicker
          name={picker.tag}
          current={colors[picker.tag]}
          anchor={picker.el}
          onApply={(hex) => setTagColor(picker.tag, hex)}
          onRemove={() => setTagColor(picker.tag, null)}
          onClose={() => {
            const el = picker.el;
            setPicker(null);
            el.querySelector<HTMLElement>(".tagname")?.focus();
          }}
        />
      )}
    </span>
  );
}

function ChipBody({ name, busy, onColor, onRemove }: { name: string; busy: boolean; onColor: (el: HTMLElement) => void; onRemove: () => void }) {
  return (
    <>
      <button type="button" className="tagname" title="Change colour" aria-label={`Colour of tag ${name}`} aria-haspopup="dialog" onClick={(e) => onColor(e.currentTarget.parentElement!)}>
        {name}
      </button>
      <button type="button" className="tagx" aria-label={`Remove tag ${name}`} disabled={busy} onClick={onRemove}>
        <X size={12} aria-hidden="true" />
      </button>
    </>
  );
}
