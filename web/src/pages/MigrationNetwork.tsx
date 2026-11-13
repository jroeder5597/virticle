import { FormEvent, useEffect, useState } from "react";
import { get, put } from "../api";
import { CheckItem, CheckList } from "../Checks";
import Select from "../Select";
import { ErrorBox, Field, Modal, useAction } from "../ui";
import DialogAdvanced from "./DialogAdvanced";

/** One address of a node that can carry migration data (GET /api/nodes/{id}/migration-network). */
export interface MigOption {
  interface: string;
  address: string;
  speedMbps?: number;
  label: string;
}

export interface MigNetInfo {
  online: boolean;
  address: string;
  limit: string;
  management: MigOption;
  options: MigOption[];
  custom?: string;
  show: boolean;
  current: string;
  interface?: string;
  shared: boolean;
  speedMbps?: number;
  limitText: string;
}

export interface MigNetResult extends MigNetInfo {
  warnings?: string[];
  notes?: string[];
}

/** The node's choices; reloaded when `deps` change (the node's own settings or state). */
export function useMigNet(node: number, deps: unknown[]) {
  const [info, setInfo] = useState<MigNetInfo | undefined>();
  const [error, setError] = useState<string | undefined>();
  useEffect(() => {
    let live = true;
    get<MigNetInfo>(`/nodes/${node}/migration-network`).then(
      (d) => live && (setInfo(d), setError(undefined)),
      (e) => live && setError(String(e.message ?? e)),
    );
    return () => {
      live = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [node, ...deps]);
  return { info, error };
}

/** The result of a change as a list: warnings first (a node that could not be checked, a certificate that could not be renewed). */
export function resultItems(r: { warnings?: string[]; notes?: string[] }, saved: string): CheckItem[] {
  return [
    { status: "ok", detail: saved },
    ...(r.warnings ?? []).map((t): CheckItem => ({ status: "warning", detail: t })),
    ...(r.notes ?? []).map((t): CheckItem => ({ status: "info", detail: t })),
  ];
}

const LIMIT_AUTO = "80% of the link (default)";

/** "" | "none" | "custom:800" as the select value and the number. */
function splitLimit(stored: string): { kind: "" | "none" | "custom"; mbit: string } {
  if (stored === "none") return { kind: "none", mbit: "" };
  if (stored.startsWith("custom:")) return { kind: "custom", mbit: stored.slice(7) };
  return { kind: "", mbit: "" };
}

export function joinLimit(kind: string, mbit: string): string {
  return kind === "custom" ? `custom:${mbit.trim()}` : kind;
}

/** The choices of the migration network select: the management address first, then the other addresses; a stored address that matches no card stays visible. */
export function networkOptions(info: MigNetInfo) {
  const opts = [{ value: "", label: info.management.label }, ...info.options.map((o) => ({ value: o.address, label: o.label }))];
  if (info.custom) opts.push({ value: info.custom, label: `Custom (${info.custom})` });
  return opts;
}

/** The select of the migration network. The caller shows it only when `info.show`. */
export function NetworkSelect({ info, value, onChange }: { info: MigNetInfo; value: string; onChange: (v: string) => void }) {
  return (
    <Field label="Migration network" hint="Where other nodes send migration data and file copies.">
      <Select label="Migration network" value={value} onChange={onChange} options={networkOptions(info)} />
    </Field>
  );
}

/** "Limit on a shared link": the choices and, for Custom, the number of Mbit/s. `value` is the stored form. */
export function LimitField({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  const { kind, mbit } = splitLimit(value);
  return (
    <>
      <Field label="Limit on a shared link" hint="Applies when migration shares a network card with management.">
        <Select
          label="Limit on a shared link"
          value={kind}
          onChange={(k) => onChange(joinLimit(k, k === "custom" ? mbit || "800" : ""))}
          options={[
            { value: "", label: LIMIT_AUTO },
            { value: "none", label: "No limit" },
            { value: "custom", label: "Custom (Mbit/s)" },
          ]}
        />
      </Field>
      {kind === "custom" && (
        <Field label="Limit in Mbit/s">
          <input type="number" min={1} max={1000000} step={1} value={mbit} onChange={(e) => onChange(joinLimit("custom", e.target.value))} required />
        </Field>
      )}
    </>
  );
}

/** The dialog behind "Change" on the Settings tab: the network, and under Advanced the limit. */
export function MigrationNetworkDialog({ node, info, onClose, onSaved }: { node: number; info: MigNetInfo; onClose: () => void; onSaved: (r: MigNetResult) => void }) {
  const [addr, setAddr] = useState(info.address && info.address !== info.management.address ? info.address : "");
  const [limit, setLimit] = useState(info.limit);
  const act = useAction();
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    let res: MigNetResult | undefined;
    const ok = await act.run(async () => {
      res = await put<MigNetResult>(`/nodes/${node}/migration-network`, { address: addr, limit });
    });
    if (ok && res) onSaved(res);
  };
  return (
    <Modal title="Migration network" onClose={onClose}>
      <form onSubmit={submit}>
        {info.show && <NetworkSelect info={info} value={addr} onChange={setAddr} />}
        <DialogAdvanced>
          <LimitField value={limit} onChange={setLimit} />
        </DialogAdvanced>
        <ErrorBox error={act.error} />
        <footer>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button className="primary" disabled={act.busy}>
            Save
          </button>
        </footer>
      </form>
    </Modal>
  );
}

export function MigrationResult({ items, onClose }: { items: CheckItem[]; onClose: () => void }) {
  return (
    <div className="migresult">
      <CheckList summary={false} label="Result of the change" items={items} />
      <button className="ghost" onClick={onClose}>
        Dismiss
      </button>
    </div>
  );
}
