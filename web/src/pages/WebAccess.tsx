import { useCallback, useEffect, useRef, useState } from "react";
import { ApiError, get, post, put } from "../api";
import { CheckItem, CheckList, StatusLine } from "../Checks";
import Select from "../Select";
import { ErrorBox, Field, useAction, useConfirm } from "../ui";
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
interface Resp {
  status: Status;
  onNewListener: boolean;
  tls: boolean;
  snippets: Snippet[];
  suggestedNames: string[];
}
interface Check {
  status: CheckItem["status"];
  title: string;
  detail?: string;
}

const MODES = [
  { value: "off", label: "Off (HTTP)" },
  { value: "selfsigned", label: "Self-signed certificate" },
  { value: "own", label: "Own certificate" },
  { value: "acme", label: "ACME (Let's Encrypt)" },
  { value: "proxy", label: "Behind a reverse proxy" },
];
const LE = "https://acme-v02.api.letsencrypt.org/directory";
const LE_STAGING = "https://acme-staging-v02.api.letsencrypt.org/directory";
const DIRS = [
  { value: LE, label: "Let's Encrypt" },
  { value: LE_STAGING, label: "Let's Encrypt staging" },
  { value: "custom", label: "Other directory" },
];

const lines = (s: string) => s.split(/[\s,]+/).filter(Boolean);
const when = (s?: string) => (s ? new Date(s).toLocaleString() : "");
const isLoopback = (addr: string) => /^(127\.|\[::1\]|localhost)/i.test(addr);
const LABEL: Record<string, string> = { http: "HTTP", https: "HTTPS", redirect: "HTTP redirect to HTTPS" };

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
  const body = () => ({ settings: draft, certPem, keyPem });

  const check = () =>
    act.run(async () => {
      const r = await post<{ checks: Check[] }>("/web/check", body());
      setChecks(r.checks);
    });
  const apply = async () => {
    const ok = await confirm.ask({
      title: "Apply web access settings",
      message: `The new listener starts now and the old one stays for ${st.windowSeconds} seconds. Open the new address and choose Keep this setting there, or the previous settings return.`,
      confirm: "Apply",
    });
    if (!ok) return;
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

  const mode = draft.mode;
  const tls = mode === "selfsigned" || mode === "own" || mode === "acme";
  const acmeDir = draft.acme.directory === LE || draft.acme.directory === LE_STAGING ? draft.acme.directory : "custom";
  const plainOpen = st.settings.mode === "off" && !isLoopback(st.settings.listen);

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
            <p className="wa-fp">
              <span className="muted">SHA-256 fingerprint</span>
              <code>{st.cert.fingerprint}</code>
            </p>
            {st.certError && <StatusLine status="error">{st.certError}</StatusLine>}
          </>
        )}
        {st.acme && (
          <>
            <StatusLine status={st.acme.lastIssued ? "ok" : "info"}>{st.acme.lastIssued ? `Last issued ${when(st.acme.lastIssued)}` : "No certificate issued yet"}</StatusLine>
            {st.acme.expires && <StatusLine status="ok">Expires {when(st.acme.expires)}</StatusLine>}
            <StatusLine status={st.acme.lastError ? "error" : "ok"}>{st.acme.lastError ? `Last error: ${st.acme.lastError}` : "No error"}</StatusLine>
            {st.acme.requesting && <StatusLine status="info">Requesting</StatusLine>}
            <div className="actions">
              <button disabled={act.busy || locked} onClick={requestAcme}>
                Request now
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
        className="card"
        onSubmit={(e) => {
          e.preventDefault();
          apply();
        }}
      >
        <h2>Settings</h2>
        <Field label="Mode">
          <Select
            label="Mode"
            value={mode}
            options={MODES}
            disabled={locked}
            onChange={(v) => edit(v === "selfsigned" && draft.names.length === 0 ? { mode: v, names: resp.suggestedNames } : { mode: v })}
          />
        </Field>

        {!tls && (
          <Field label="HTTP address" hint={mode === "proxy" ? "Where the proxy connects. Keep it on loopback." : "host:port. 127.0.0.1:8080 reaches only this machine."}>
            <input value={draft.listen} disabled={locked} onChange={(e) => edit({ listen: e.target.value })} />
          </Field>
        )}
        {mode === "off" && !isLoopback(draft.listen) && <StatusLine status="warning">Sign-ins travel unencrypted</StatusLine>}

        {tls && (
          <div className="grid2">
            <Field label="HTTPS address" hint="host:port, for example :8443">
              <input value={draft.httpsListen} disabled={locked} onChange={(e) => edit({ httpsListen: e.target.value })} />
            </Field>
            <Field label="HTTP redirect address" hint="Optional, for example :80. Only redirects to HTTPS.">
              <input value={draft.httpRedirect} disabled={locked} onChange={(e) => edit({ httpRedirect: e.target.value })} />
            </Field>
          </div>
        )}

        {mode !== "off" && (
          <Field label="Public URL" hint={mode === "proxy" ? "What users type, for example https://virticle.example.org" : "Optional. Origin checks accept it."}>
            <input value={draft.publicUrl} disabled={locked} onChange={(e) => edit({ publicUrl: e.target.value })} />
          </Field>
        )}

        {mode === "selfsigned" && (
          <Field label="Names" hint="Host names and IP addresses in the certificate, separated by commas or lines.">
            <textarea rows={3} value={draft.names.join("\n")} disabled={locked} onChange={(e) => edit({ names: lines(e.target.value) })} />
          </Field>
        )}

        {mode === "own" && (
          <>
            <Field label="Names" hint="Optional. Each must be covered by the certificate.">
              <textarea rows={2} value={draft.names.join("\n")} disabled={locked} onChange={(e) => edit({ names: lines(e.target.value) })} />
            </Field>
            <Field label="Certificate chain (PEM)" hint={draft.own.managed ? "A certificate is stored. Paste to replace it." : "Paste the certificate, then intermediates."}>
              <textarea rows={4} value={certPem} disabled={locked} spellCheck={false} onChange={(e) => { dirty.current = true; setChecks(undefined); setCertPem(e.target.value); }} />
            </Field>
            <Field label="Private key (PEM)" hint="Stored in the data directory with mode 0600. Never shown again.">
              <textarea rows={4} value={keyPem} disabled={locked} spellCheck={false} onChange={(e) => { dirty.current = true; setChecks(undefined); setKeyPem(e.target.value); }} />
            </Field>
            <div className="grid2">
              <Field label="Or certificate file" hint="Re-read when it changes and on SIGHUP.">
                <input value={draft.own.managed ? "" : draft.own.certFile} disabled={locked} onChange={(e) => edit({ own: { ...draft.own, managed: false, certFile: e.target.value } })} />
              </Field>
              <Field label="Or key file">
                <input value={draft.own.managed ? "" : draft.own.keyFile} disabled={locked} onChange={(e) => edit({ own: { ...draft.own, managed: false, keyFile: e.target.value } })} />
              </Field>
            </div>
          </>
        )}

        {mode === "acme" && (
          <>
            <Field label="Domain names" hint="Separated by commas or lines. Each must resolve to this machine.">
              <textarea rows={2} value={draft.acme.domains.join("\n")} disabled={locked} onChange={(e) => edit({ acme: { ...draft.acme, domains: lines(e.target.value) } })} />
            </Field>
            <div className="grid2">
              <Field label="Contact e-mail" hint="Optional. For expiry notices.">
                <input type="email" value={draft.acme.email} disabled={locked} onChange={(e) => edit({ acme: { ...draft.acme, email: e.target.value } })} />
              </Field>
              <Field label="Directory">
                <Select label="Directory" value={acmeDir} options={DIRS} disabled={locked} onChange={(v) => edit({ acme: { ...draft.acme, directory: v === "custom" ? "https://" : v } })} />
              </Field>
            </div>
            {acmeDir === "custom" && (
              <Field label="Directory URL">
                <input value={draft.acme.directory} disabled={locked} onChange={(e) => edit({ acme: { ...draft.acme, directory: e.target.value } })} />
              </Field>
            )}
            <label className="wa-check">
              <input type="checkbox" checked={draft.acme.acceptTerms} disabled={locked} onChange={(e) => edit({ acme: { ...draft.acme, acceptTerms: e.target.checked } })} /> I accept the certificate authority's terms of service
            </label>
            <p className="muted">TLS-ALPN-01 runs on the HTTPS port, which must be reachable as port 443. HTTP-01 uses the redirect address, reachable as port 80.</p>
          </>
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

        <ErrorBox error={act.error} />
        {checks && <CheckList items={checks} label="Checks" />}
        <div className="actions">
          <button type="button" disabled={act.busy || locked} onClick={check}>
            Check
          </button>
          <button className="primary" disabled={act.busy || locked || !!pend}>
            Apply
          </button>
        </div>
      </form>

      {mode === "proxy" &&
        snippets.map((s) => <SnippetCard key={s.id} s={s} />)}
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
