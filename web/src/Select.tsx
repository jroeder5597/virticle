// The themed dropdown: a Combo that looks and sizes like a text input.
// Options come from the `options` prop (no option children). More than 8 options, or `searchable`,
// adds a search box. The visible <Field> label names it (`label` must repeat that text).
import Combo, { ComboItem } from "./Combo";

export interface SelectOption {
  value: string;
  label: string;
  disabled?: boolean;
  /** Options with the same group are listed together under a heading. */
  group?: string;
  /** A second, muted line in the list. */
  hint?: string;
  /** A state dot before the label: "n-online", "n-offline", "v-running" ... (classes of .dot). */
  dot?: string;
}

export interface SelectProps {
  value: string;
  onChange: (value: string) => void;
  /** Accessible name; the same text as the Field label. */
  label: string;
  options: SelectOption[];
  disabled?: boolean;
  searchable?: boolean;
  /** Shown when the value matches no option. */
  placeholder?: string;
  /** Size to the content instead of filling the row (toolbar filters). */
  auto?: boolean;
  mono?: boolean;
  className?: string;
}

type Item = ComboItem & { o: SelectOption };

const Lab = ({ o, mono, hint }: { o: SelectOption; mono?: boolean; hint?: boolean }) => (
  <>
    <span className={"sel-lab" + (mono ? " mono" : "")}>
      {o.dot && <span className={"dot " + o.dot} aria-hidden="true" />}
      {o.label}
    </span>
    {hint && o.hint && <span className="cb-hint">{o.hint}</span>}
  </>
);

export default function Select(p: SelectProps) {
  const items: Item[] = p.options.map((o) => ({ key: "v:" + o.value, text: o.label, search: o.hint, disabled: o.disabled, group: o.group, o }));
  return (
    <Combo
      items={items}
      value={"v:" + p.value}
      onChange={(k) => p.onChange(k.slice(2))}
      label={p.label}
      disabled={p.disabled}
      searchLabel={p.searchable || p.options.length > 8 ? "Search " + p.label.toLowerCase() : undefined}
      className={"sel-field" + (p.auto ? " sel-auto" : "") + (p.className ? " " + p.className : "")}
      face={(i) => (i ? <Lab o={i.o} mono={p.mono} /> : <span className="sel-lab sel-ph">{p.placeholder ?? ""}</span>)}
      option={(i) => <Lab o={i.o} mono={p.mono} hint />}
      empty={<span>Nothing to choose.</span>}
    />
  );
}
