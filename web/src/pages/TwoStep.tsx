// Two-step sign-in with an authenticator app: the card on My account, the set-up steps (also used by the
// page that opens for a user who must set it up) and the dialogs to turn it off or make new recovery codes.
import { FormEvent, useEffect, useState } from "react";
import { post } from "../api";
import { ErrorBox, Field, Modal, useAction } from "../ui";
import "./signin.css";

export interface TwoStepState {
  enabled: boolean;
  required: boolean;
  recoveryLeft: number;
  external: boolean;
  source: "local" | "oidc" | "ldap";
}

export const APPS_LINE = "Works with Google Authenticator, Microsoft Authenticator, Duo Mobile and other authenticator apps.";

export function TwoStepCard({ st, reload }: { st: TwoStepState; reload: () => void }) {
  const [dialog, setDialog] = useState<"on" | "off" | "codes" | undefined>();
  if (st.external)
    return (
      <div className="card">
        <h2>Two-step sign-in</h2>
        <p className="muted">You sign in through your identity provider, and its second factor applies. Change it there.</p>
      </div>
    );
  return (
    <div className="card" aria-label="Two-step sign-in">
      <h2>Two-step sign-in</h2>
      <p className="muted">{APPS_LINE}</p>
      <p>
        <span className={"badge " + (st.enabled ? "s-running" : "s-shutoff")}>{st.enabled ? "On" : "Off"}</span>
        {st.enabled && <span className="muted">{`  ${st.recoveryLeft} recovery code${st.recoveryLeft === 1 ? "" : "s"} left`}</span>}
        {st.required && !st.enabled && <span className="muted">  An administrator requires it.</span>}
      </p>
      {!st.enabled ? (
        <button className="primary" onClick={() => setDialog("on")}>
          Turn on two-step sign-in
        </button>
      ) : (
        <div className="row">
          <button onClick={() => setDialog("codes")}>New recovery codes</button>
          {!st.required && (
            <button className="danger" onClick={() => setDialog("off")}>
              Turn off two-step sign-in
            </button>
          )}
        </div>
      )}
      {dialog === "on" && (
        <Modal title="Turn on two-step sign-in" onClose={() => { setDialog(undefined); reload(); }}>
          <TwoStepSetup onDone={() => { setDialog(undefined); reload(); }} />
        </Modal>
      )}
      {dialog === "off" && <ProveIt title="Turn off two-step sign-in" button="Turn off" path="/account/2fa/disable" danger onClose={() => setDialog(undefined)} onDone={reload} />}
      {dialog === "codes" && <ProveIt title="New recovery codes" button="Make new codes" path="/account/2fa/recovery-codes" showCodes onClose={() => setDialog(undefined)} onDone={reload} />}
    </div>
  );
}

interface Begin {
  secret: string;
  uri: string;
  qr: { size: number; path: string };
}

/** The set-up steps: scan, confirm with one code, then the recovery codes. */
export function TwoStepSetup({ onDone }: { onDone: () => void }) {
  const [begin, setBegin] = useState<Begin | undefined>();
  const [code, setCode] = useState("");
  const [codes, setCodes] = useState<string[] | undefined>();
  const start = useAction();
  const confirm = useAction();
  useEffect(() => {
    start.run(async () => setBegin(await post<Begin>("/account/2fa/begin")));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    let made: string[] = [];
    if (await confirm.run(async () => (made = (await post<{ codes: string[] }>("/account/2fa/confirm", { code })).codes))) setCodes(made);
  };
  if (codes) return <RecoveryCodes codes={codes} onDone={onDone} />;
  return (
    <form onSubmit={submit} className="ts-setup">
      <p className="muted">{APPS_LINE}</p>
      <ErrorBox error={start.error} />
      {begin && (
        <>
          <p>
            <strong>1.</strong> Scan this code with your authenticator app.
          </p>
          <Qr qr={begin.qr} />
          <p className="muted">
            Cannot scan it? Enter this key by hand (time based, 6 digits):
          </p>
          <p>
            <code className="ts-secret">{begin.secret.replace(/(.{4})/g, "$1 ").trim()}</code>
          </p>
          <Field label="Code from the app" hint="2. Enter the 6 digits the app shows now.">
            <input value={code} onChange={(e) => setCode(e.target.value)} inputMode="numeric" autoComplete="one-time-code" maxLength={8} required autoFocus />
          </Field>
        </>
      )}
      <ErrorBox error={confirm.error} />
      <footer>
        <button className="primary" disabled={!begin || confirm.busy}>
          Turn on
        </button>
      </footer>
    </form>
  );
}

function Qr({ qr }: { qr: { size: number; path: string } }) {
  // Always dark on white, in both themes: phone cameras need the contrast.
  return (
    <svg className="ts-qr" viewBox={`0 0 ${qr.size} ${qr.size}`} role="img" aria-label="QR code for your authenticator app" shapeRendering="crispEdges">
      <rect width={qr.size} height={qr.size} fill="#ffffff" />
      <path d={qr.path} fill="#000000" />
    </svg>
  );
}

function RecoveryCodes({ codes, onDone }: { codes: string[]; onDone: () => void }) {
  const [copied, setCopied] = useState(false);
  const text = "Virticle recovery codes\nEach code works once.\n\n" + codes.join("\n") + "\n";
  const download = () => {
    const url = URL.createObjectURL(new Blob([text], { type: "text/plain" }));
    const a = document.createElement("a");
    a.href = url;
    a.download = "virticle-recovery-codes.txt";
    a.click();
    URL.revokeObjectURL(url);
  };
  return (
    <div>
      <p>
        <strong>Two-step sign-in is on.</strong> Keep these recovery codes somewhere safe. Each works once if you lose your phone. They are not shown again.
      </p>
      <ul className="ts-codes" aria-label="Recovery codes">
        {codes.map((c) => (
          <li key={c}>
            <code>{c}</code>
          </li>
        ))}
      </ul>
      <div className="row">
        <button type="button" onClick={() => navigator.clipboard?.writeText(text).then(() => setCopied(true))}>
          {copied ? "Copied" : "Copy"}
        </button>
        <button type="button" onClick={download}>
          Download
        </button>
      </div>
      <footer>
        <button className="primary" onClick={onDone}>
          I have saved them
        </button>
      </footer>
    </div>
  );
}

/** Password and a current code, to turn it off or to make new recovery codes. */
function ProveIt(props: { title: string; button: string; path: string; danger?: boolean; showCodes?: boolean; onClose: () => void; onDone: () => void }) {
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const [codes, setCodes] = useState<string[] | undefined>();
  const act = useAction();
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    let made: string[] | undefined;
    if (await act.run(async () => (made = (await post<{ codes?: string[] }>(props.path, { password, code })).codes))) {
      props.onDone();
      if (props.showCodes && made) setCodes(made);
      else props.onClose();
    }
  };
  return (
    <Modal title={props.title} onClose={props.onClose}>
      {codes ? (
        <RecoveryCodes codes={codes} onDone={props.onClose} />
      ) : (
        <form onSubmit={submit}>
          <Field label="Password">
            <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} required autoFocus autoComplete="current-password" />
          </Field>
          <Field label="Code from the app" hint="The 6 digits the app shows now, or a recovery code.">
            <input value={code} onChange={(e) => setCode(e.target.value)} required autoComplete="one-time-code" />
          </Field>
          <ErrorBox error={act.error} />
          <footer>
            <button type="button" onClick={props.onClose}>
              Cancel
            </button>
            <button className={props.danger ? "danger" : "primary"} disabled={act.busy}>
              {props.button}
            </button>
          </footer>
        </form>
      )}
    </Modal>
  );
}
