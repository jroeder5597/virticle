import { useCallback, useEffect, useRef, useState } from "react";
import { ApiError, get, post, put } from "../api";
import { CheckItem, CheckList, StatusLine } from "../Checks";
import Select from "../Select";
import { ErrorBox, Field, useAction, useConfirm } from "../ui";
import { SectionFrame } from "./Disclosure";
import "./webaccess.css";

interface WSettings {
  mode: string;
  listen: string;
  httpsListen: string;
  httpRedirect: string;
  publicUrl: string;
  names: string[];
  own: { certFile: string; keyFile: string; managed: boolean };
  acme: { domains: string[]; email: string; directory: string; acceptTerms: boolean };
  trustedProxies: string[];
  hsts: boolean;
}
interface Cert {
  subject: string;
  issuer: string;
  names: string[];
  notBefore: string;
  notAfter: string;
  fingerprint: string;
  selfSigned: boolean;
}
interface AcmeStatus {
  domains: string[];
  directory: string;
  lastIssued?: string;
  expires?: string;
  lastError?: string;
  lastErrorAt?: string;
  requesting: boolean;
}
interface Pending {
  gen: number;
  deadline: string;
  previousMode: string;
  url: string;
}
interface Status {
  settings: WSettings;
  source: string;
  locked?: string;
  notice?: string;
  listeners: { kind: string; addr: string; gen: number }[];
  pending?: Pending;
  cert?: Cert;
  certError?: string;
  acme?: AcmeStatus;
  windowSeconds: number;
}
interface Snippet {
  id: string;
  name: string;
  file: string;
  notes: string;
  text: string;
}
interface Iface {
  name: string;
  addrs: string[];
}
interface Resp {
  status: Status;
  onNewListener: boolean;
  tls: boolean;
  snippets: Snippet[];
  suggestedNames: string[];
  interfaces: Iface[];
  lowPorts: boolean;
}
interface Check {
  status: CheckItem["status"];
  title: string;
  detail?: string;
}

const MODES = [
  { value: "selfsigned", label: "Self-signed certificate" },
  { value: "acme", label: "My domain, with a Let's Encrypt certificate" },
  { value: "own", label: "My own certificate" },
];
const LE = "https://acme-v02.api.letsencrypt.org/directory";
const LE_STAGING = "https://acme-staging-v02.api.letsencrypt.org/directory";
const LE_TERMS = "https://letsencrypt.org/repository/";
const DIRS = [
  { value: LE, label: "Let's Encrypt" },
  { value: LE_STAGING, label: "Let's Encrypt staging" },
  { value: "custom", label: "Other directory" },
];
const PLAIN_MODES = [
  { value: "off", label: "Plain HTTP, not encrypted" },
  { value: "proxy", label: "Behind a reverse proxy" },
];

const lines = (s: string) => s.split(/[\s,]+/).filter(Boolean);
const when = (s?: string) => (s ? new Date(s).toLocaleString() : "");
const isLoopback = (addr: string) => /^(127\.|\[::1\]|localhost)/i.test(addr);
const LABEL: Record<string, string> = { http: "HTTP", https: "HTTPS", redirect: "HTTP redirect to HTTPS" };

// A listen address is the interface the user picks plus a port. These mirror
// webaccess.SplitListen and webaccess.ComposeListen on the server.
const ALL = "*";
const LOOP = "lo";
function splitListen(addr: string): { host: string; port: string } {
  const i = addr.lastIndexOf(":");
  if (i < 0) return { host: addr, port: "" };
  let host = addr.slice(0, i);
  if (host.startsWith("[") && host.endsWith("]")) host = host.slice(1, -1);
  if (host === "0.0.0.0" || host === "::") host = "";
  return { host, port: addr.slice(i + 1) };
}
function composeListen(host: string, port: string): string {
  return (host.includes(":") ? `[${host}]` : host) + ":" + port;
}

function copyText(text: string): Promise<void> {
  if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(text);
  return new Promise((resolve, reject) => {
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    const ok = document.execCommand("copy");
    ta.remove();
    ok ? resolve() : reject(new Error("The browser did not allow copying."));
  });
}

/** Interface and port, which together make the listen address. */
function InterfacePort(props: { addr: string; ifaces: Iface[]; lowPorts: boolean; disabled: boolean; scheme: string; stored: string; onChange: (addr: string) => void }) {
  const { host, port } = splitListen(props.addr);
  const [portText, setPortText] = useState(port);
  useEffect(() => setPortText(port), [port]);
  const options = [{ value: ALL, label: "All interfaces" }];
  for (const f of props.ifaces) options.push({ value: f.addrs[0], label: `${f.name} (${f.addrs.join(", ")})` });
  options.push({ value: LOOP, label: "This machine only (127.0.0.1)" });
  const value = host === "" ? ALL : host === "127.0.0.1" ? LOOP : options.some((o) => o.value === host) ? host : "custom";
  if (value === "custom") options.push({ value: "custom", label: `Custom (${host})` });
  const hostOf = (v: string) => (v === ALL ? "" : v === LOOP ? "127.0.0.1" : v === "custom" ? host : v);
  const n = Number(portText);
  const valid = /^\d+$/.test(portText) && n >= 1 && n <= 65535;
  const next = props.addr;
  const moves = next !== props.stored && valid;
  const target = hostOf(value) || window.location.hostname;
  return (
    <>
      <div className="grid2">
        <Field label="Interface">
          <Select label="Interface" value={value} options={options} disabled={props.disabled} onChange={(v) => props.onChange(composeListen(hostOf(v), portText || port))} />
        </Field>
        <Field label="Port" hint={valid && n < 1024 && !props.lowPorts ? "This service cannot use ports below 1024." : undefined}>
          <input
            type="number"
            min={1}
            max={65535}
            value={portText}
            disabled={props.disabled}
            onChange={(e) => {
              setPortText(e.target.value);
              const v = Number(e.target.value);
              if (/^\d+$/.test(e.target.value) && v >= 1 && v <= 65535) props.onChange(composeListen(host, e.target.value));
            }}
          />
        </Field>
      </div>
      {!valid && <StatusLine status="error">The port is a number from 1 to 65535.</StatusLine>}
      {moves && <p className="muted">{`This page moves to ${props.scheme}://${composeListen(target, splitListen(next).port)}/ when you apply.`}</p>}
    </>
  );
}

export default function WebAccess() {
  const [resp, setResp] = useState<Resp>();
  const [draft, setDraft] = useState<WSettings>();
  const [certPem, setCertPem] = useState("");
  const [keyPem, setKeyPem] = useState("");
  const [checks, setChecks] = useState<Check[]>();
  const [snippets, setSnippets] = useState<Snippet[]>([]);
  const [error, setError] = useState<string>();
  const act = useAction();
  const confirm = useConfirm();
  const dirty = useRef(false);

  const load = useCallback(async () => {
    try {
      const r = await get<Resp>("/web");
      setResp(r);
      setSnippets(r.snippets);
      if (!dirty.current) setDraft(r.status.settings);
      setError(undefined);
    } catch (e: any) {
      setError(String(e.message ?? e));
    }
  }, []);
  useEffect(() => {
    load();
  }, [load]);

  // One timer: reload when an open change runs out, so the page shows the rollback.
  const deadline = resp?.status.pending?.deadline;
  useEffect(() => {
    if (!deadline) return;
    const t = setTimeout(load, Math.max(1000, new Date(deadline).getTime() - Date.now() + 1500));
    return () => clearTimeout(t);
  }, [deadline, load]);

  const edit = (patch: Partial<WSettings>) => {
    dirty.current = true;
    setChecks(undefined);
    setDraft((d) => d && { ...d, ...patch });
  };

  // Proxy snippets follow the form.
  const proxyKey = draft && JSON.stringify([draft.mode, draft.listen, draft.publicUrl, draft.trustedProxies]);
  useEffect(() => {
    if (!draft || draft.mode !== "proxy" || !dirty.current) return;
    const t = setTimeout(() => {
      post<{ snippets: Snippet[] }>("/web/snippets", { settings: draft })
        .then((r) => setSnippets(r.snippets))
        .catch(() => {});
    }, 300);
    return () => clearTimeout(t);
  }, [proxyKey]); // eslint-disable-line react-hooks/exhaustive-deps

  if (!resp || !draft) return error ? <ErrorBox error={error} /> : <p className="muted">Loading</p>;
  const st = resp.status;
  const locked = !!st.locked;
  const pend = st.pending;
  const mode = draft.mode;
  const tls = mode === "selfsigned" || mode === "own" || mode === "acme";
  // The settings as sent: a self-signed certificate needs names (this machine's, unless the user listed others);
  // Let's Encrypt terms are accepted by requesting the certificate.
  const outgoing = (): WSettings => ({
    ...draft,
    names: mode === "selfsigned" && draft.names.length === 0 ? resp.suggestedNames : draft.names,
    acme: { ...draft.acme, acceptTerms: mode === "acme" ? true : draft.acme.acceptTerms },
  });
  const body = () => ({ settings: outgoing(), certPem, keyPem });

  const apply = async () => {
    // Checks first; failures stop here and stay on the page.
    let cs: Check[] = [];
    const ok = await act.run(async () => {
      cs = (await post<{ checks: Check[] }>("/web/check", body())).checks;
      setChecks(cs);
    });
    if (!ok || cs.some((c) => c.status === "error")) return;
    const yes = await confirm.ask({
      title: "Apply web access settings",
      message: `Open the new address and choose Keep this setting there within ${st.windowSeconds} seconds, or the previous settings return.`,
      confirm: "Apply",
    });
    if (!yes) return;
    await act.run(async () => {
      try {
        const r = await put<{ checks: Check[] }>("/web", body());
        setChecks(r.checks);
        dirty.current = false;
        setCertPem("");
        setKeyPem("");
        await load();
      } catch (e: any) {
        if (e instanceof ApiError && e.data?.checks) setChecks(e.data.checks);
        throw e;
      }
    });
  };
  const keep = () => act.run(async () => { await post("/web/confirm"); setChecks(undefined); await load(); });
  const undo = () => act.run(async () => { await post("/web/revert"); dirty.current = false; await load(); });
  const renew = async () => {
    if (!(await confirm.ask({ title: "Renew the certificate", message: "A new certificate replaces the current one. Browsers ask again whether to trust it.", confirm: "Renew" }))) return;
    await act.run(async () => { await post("/web/renew"); await load(); });
  };
  const requestAcme = () => act.run(async () => { await post("/web/acme/request"); setTimeout(load, 4000); await load(); });

  const choose = (m: string) => {
    const patch: Partial<WSettings> = { mode: m };
    if (m === "proxy" && !draft.publicUrl) patch.publicUrl = window.location.origin;
    edit(patch);
  };
  const acmeDir = draft.acme.directory === LE || draft.acme.directory === LE_STAGING ? draft.acme.directory : "custom";
  const plainOpen = st.settings.mode === "off" && !isLoopback(st.settings.listen);
  const storedListen = st.settings.mode === mode || (tls && (st.settings.mode === "selfsigned" || st.settings.mode === "own" || st.settings.mode === "acme"));
  const modeName = mode === "off" ? "Plain HTTP" : mode === "proxy" ? "Behind a reverse proxy" : "";
  const acmeLine = st.acme
    ? st.acme.lastError
      ? `Let's Encrypt: ${st.acme.lastError}`
      : st.acme.requesting
        ? "Let's Encrypt: requesting a certificate"
        : st.acme.lastIssued
          ? `Let's Encrypt: issued ${when(st.acme.lastIssued)}${st.acme.expires ? `, expires ${when(st.acme.expires)}` : ""}`
          : "Let's Encrypt: no certificate issued yet"
    : "";

  return (
    <div className="wa">
      <div className="head">
        <h1>Web access</h1>
      </div>

      {pend && (
        <div className="card wa-pending">
          <h2>{resp.onNewListener ? "Keep this setting" : "Change waiting for confirmation"}</h2>
          {resp.onNewListener ? (
            <p>This page was reached through the new settings. Keep them, or they revert at {when(pend.deadline)}.</p>
          ) : (
            <p>
              Open <a href={pend.url + "web-access"}>{pend.url}</a> and choose Keep this setting there. Without that the previous settings ({pend.previousMode}) return at {when(pend.deadline)}.
            </p>
          )}
          <div className="actions">
            {resp.onNewListener && (
              <button className="primary" disabled={act.busy} onClick={keep}>
                Keep this setting
              </button>
            )}
            <button className="ghost" disabled={act.busy} onClick={undo}>
              Undo
            </button>
          </div>
        </div>
      )}

      <div className="card">
        <h2>Now</h2>
        {locked && <StatusLine status="info">{st.locked}</StatusLine>}
        {st.notice && <StatusLine status="warning">{st.notice}</StatusLine>}
        {st.listeners.map((l) => (
          <StatusLine key={l.kind + l.addr + l.gen} status="ok">
            {LABEL[l.kind] ?? l.kind} on {l.addr}
            {pend && l.gen !== pend.gen ? " (previous)" : ""}
          </StatusLine>
        ))}
        {plainOpen && <StatusLine status="warning">Sign-ins travel unencrypted</StatusLine>}
        {!resp.tls && st.settings.mode !== "off" && st.settings.mode !== "proxy" && <StatusLine status="info">This page was opened over plain HTTP.</StatusLine>}
        {st.cert && (
          <>
            <StatusLine status={new Date(st.cert.notAfter).getTime() - Date.now() < 14 * 864e5 ? "warning" : "ok"}>
              Certificate for {st.cert.names.join(", ")}, valid until {when(st.cert.notAfter)}
            </StatusLine>
            {st.cert.selfSigned && (
              <p className="wa-fp">
                <span className="muted">SHA-256 fingerprint</span>
                <code>{st.cert.fingerprint}</code>
              </p>
            )}
            {st.certError && <StatusLine status="error">{st.certError}</StatusLine>}
          </>
        )}
        {st.acme && (
          <>
            <StatusLine status={st.acme.lastError ? "error" : st.acme.lastIssued ? "ok" : "info"}>{acmeLine}</StatusLine>
            <div className="actions">
              <button disabled={act.busy || locked} onClick={requestAcme}>
                Renew now
              </button>
            </div>
          </>
        )}
        {st.settings.mode === "selfsigned" && !locked && (
          <div className="actions">
            <button disabled={act.busy} onClick={renew}>
              Renew certificate
            </button>
          </div>
        )}
        {st.settings.mode === "proxy" && <StatusLine status="ok">Forwarding headers are believed from {st.settings.trustedProxies.join(", ")}</StatusLine>}
      </div>

      <form
        className="wa-form"
        onSubmit={(e) => {
          e.preventDefault();
          apply();
        }}
      >
        <div className="card">
          <h2>Settings</h2>
          <div role="radiogroup" aria-label="Certificate" className="wa-radios">
            {MODES.map((m) => (
              <label key={m.value} className="pref-opt">
                <input type="radio" name="wa-mode" checked={mode === m.value} disabled={locked} onChange={() => choose(m.value)} />
                {m.label}
              </label>
            ))}
          </div>

          {tls && (
            <InterfacePort
              addr={draft.httpsListen}
              ifaces={resp.interfaces}
              lowPorts={resp.lowPorts}
              disabled={locked}
              scheme="https"
              stored={storedListen ? st.settings.httpsListen : ""}
              onChange={(a) => edit({ httpsListen: a })}
            />
          )}

          {mode === "acme" && (
            <>
              <Field label="Domain names" hint="Separated by commas or lines. Each must resolve to this machine.">
                <textarea rows={2} value={draft.acme.domains.join("\n")} disabled={locked} onChange={(e) => edit({ acme: { ...draft.acme, domains: lines(e.target.value) } })} />
              </Field>
              <Field label="Contact e-mail" hint="Optional. For expiry notices.">
                <input type="email" value={draft.acme.email} disabled={locked} onChange={(e) => edit({ acme: { ...draft.acme, email: e.target.value } })} />
              </Field>
            </>
          )}

          {mode === "own" && (
            <>
              <Field label="Certificate chain (PEM)" hint={draft.own.managed ? "A pasted certificate is installed. Paste again to replace it." : "Paste the certificate, then intermediates."}>
                <textarea rows={4} value={certPem} disabled={locked} spellCheck={false} onChange={(e) => { dirty.current = true; setChecks(undefined); setCertPem(e.target.value); }} />
              </Field>
              <Field label="Private key (PEM)" hint="Stored in the data directory with mode 0600. Never shown again.">
                <textarea rows={4} value={keyPem} disabled={locked} spellCheck={false} onChange={(e) => { dirty.current = true; setChecks(undefined); setKeyPem(e.target.value); }} />
              </Field>
            </>
          )}
        </div>

        <SectionFrame id="wa-advanced" title="Advanced" summary={modeName}>
          <div role="radiogroup" aria-label="Without a certificate from this page" className="wa-radios">
            {PLAIN_MODES.map((m) => (
              <label key={m.value} className="pref-opt">
                <input type="radio" name="wa-mode" checked={mode === m.value} disabled={locked} onChange={() => choose(m.value)} />
                {m.label}
              </label>
            ))}
          </div>

          {!tls && (
            <InterfacePort
              addr={draft.listen}
              ifaces={resp.interfaces}
              lowPorts={resp.lowPorts}
              disabled={locked}
              scheme="http"
              stored={st.settings.mode === mode ? st.settings.listen : ""}
              onChange={(a) => edit({ listen: a })}
            />
          )}
          {mode === "off" && !isLoopback(draft.listen) && <StatusLine status="warning">Sign-ins travel unencrypted</StatusLine>}

          {tls && (
            <Field label="HTTP redirect address" hint="Optional, for example :80. Only redirects to HTTPS.">
              <input value={draft.httpRedirect} disabled={locked} onChange={(e) => edit({ httpRedirect: e.target.value })} />
            </Field>
          )}

          {mode !== "off" && (
            <Field label="Public URL" hint={mode === "proxy" ? "What users type, for example https://virticle.example.org" : "Optional. Origin checks accept it."}>
              <input value={draft.publicUrl} disabled={locked} onChange={(e) => edit({ publicUrl: e.target.value })} />
            </Field>
          )}

          {mode === "selfsigned" && (
            <Field label="Names" hint="Host names and IP addresses in the certificate. Empty: this machine's.">
              <textarea rows={2} value={draft.names.join("\n")} disabled={locked} onChange={(e) => edit({ names: lines(e.target.value) })} />
            </Field>
          )}

          {mode === "acme" && (
            <>
              <Field label="Directory">
                <Select label="Directory" value={acmeDir} options={DIRS} disabled={locked} onChange={(v) => edit({ acme: { ...draft.acme, directory: v === "custom" ? "https://" : v } })} />
              </Field>
              {acmeDir === "custom" && (
                <Field label="Directory URL">
                  <input value={draft.acme.directory} disabled={locked} onChange={(e) => edit({ acme: { ...draft.acme, directory: e.target.value } })} />
                </Field>
              )}
            </>
          )}

          {mode === "own" && (
            <div className="grid2">
              <Field label="Certificate file" hint="Instead of pasting. Re-read when it changes and on SIGHUP.">
                <input value={draft.own.managed ? "" : draft.own.certFile} disabled={locked} onChange={(e) => edit({ own: { ...draft.own, managed: false, certFile: e.target.value } })} />
              </Field>
              <Field label="Key file">
                <input value={draft.own.managed ? "" : draft.own.keyFile} disabled={locked} onChange={(e) => edit({ own: { ...draft.own, managed: false, keyFile: e.target.value } })} />
              </Field>
            </div>
          )}

          {mode === "proxy" && (
            <Field label="Trusted proxy addresses" hint="IPs or CIDRs. Only these may set X-Forwarded-For, -Proto and -Host.">
              <textarea rows={2} value={draft.trustedProxies.join("\n")} disabled={locked} onChange={(e) => edit({ trustedProxies: lines(e.target.value) })} />
            </Field>
          )}

          {mode !== "off" && (
            <label className="wa-check">
              <input type="checkbox" checked={draft.hsts} disabled={locked} onChange={(e) => edit({ hsts: e.target.checked })} /> HSTS
              <small className="muted"> Browsers refuse plain HTTP to this host for a year. Turn it on once HTTPS works.</small>
            </label>
          )}
        </SectionFrame>

        <div className="card">
          <ErrorBox error={act.error} />
          {checks && <CheckList items={checks} label="Checks" />}
          <div className="actions">
            <button className="primary" disabled={act.busy || locked || !!pend}>
              Apply
            </button>
            {mode === "acme" && (
              <span className="muted wa-terms">
                By requesting a certificate you accept the{" "}
                <a href={LE_TERMS} target="_blank" rel="noopener noreferrer">
                  Let's Encrypt terms of service
                </a>
              </span>
            )}
          </div>
        </div>
      </form>

      {mode === "proxy" && snippets.map((s) => <SnippetCard key={s.id} s={s} />)}
      {confirm.dialog}
    </div>
  );
}

function SnippetCard({ s }: { s: Snippet }) {
  const [state, setState] = useState("");
  const copy = () =>
    copyText(s.text).then(
      () => {
        setState("Copied");
        setTimeout(() => setState(""), 2000);
      },
      (e) => setState(String(e.message ?? e)),
    );
  return (
    <div className="card wa-snippet" data-proxy={s.id}>
      <div className="head">
        <h2>{s.name}</h2>
        <button onClick={copy} aria-label={`Copy ${s.name} configuration`}>
          Copy
        </button>
      </div>
      <p className="muted">
        <code>{s.file}</code>. {s.notes}
      </p>
      <pre className="wa-pre" tabIndex={0} aria-label={`${s.name} configuration`}>
        {s.text}
      </pre>
      <p className="wa-copied" role="status">
        {state}
      </p>
    </div>
  );
}
