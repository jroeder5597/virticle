// Settings, Sign-in: two-step sign-in policy, the identity provider (OIDC) and the directory (LDAP / Active Directory).
// Three collapsed sections, each with a one-line status; each saves only its own part.
import { FormEvent, ReactNode, useCallback, useEffect, useState } from "react";
import { get, post, put } from "../api";
import { CheckStatus, StatusDot } from "../Checks";
import Select from "../Select";
import { ErrorBox, Field, useAction } from "../ui";
import { SectionFrame } from "./Disclosure";
import DialogAdvanced from "./DialogAdvanced";
import { APPS_LINE } from "./TwoStep";
import "./signin.css";

interface Oidc {
  enabled: boolean;
  name: string;
  issuer: string;
  clientId: string;
  adminGroup: string;
  operatorGroup: string;
  defaultRole: string;
  groupsClaim: string;
  userClaim: string;
  scopes: string;
  clientSecretSet: boolean;
  redirectUrl: string;
}
interface Ldap {
  enabled: boolean;
  url: string;
  bindDn: string;
  baseDn: string;
  adminGroup: string;
  operatorGroup: string;
  defaultRole: string;
  userFilter: string;
  groupFilter: string;
  nameAttr: string;
  memberOfAttr: string;
  caCert: string;
  allowPlain: boolean;
  timeoutSeconds: number;
  bindPasswordSet: boolean;
}
interface Config {
  require2fa: "nobody" | "admins" | "everyone";
  oidc: Oidc;
  ldap: Ldap;
  twoStepUsers: number;
}
interface Step {
  ok: boolean;
  text: string;
}

const roleOptions = [
  { value: "none", label: "No access" },
  { value: "viewer", label: "Viewer" },
];

export default function SignInSettings() {
  const [cfg, setCfg] = useState<Config | undefined>();
  const [error, setError] = useState<string | undefined>();
  const load = useCallback(() => get<Config>("/signin").then(setCfg, (e) => setError(String(e.message ?? e))), []);
  useEffect(() => {
    load();
  }, [load]);
  if (!cfg) return <ErrorBox error={error} />;
  // Saving one section sends the others as they were saved.
  const save = async (part: Partial<{ require2fa: string; oidc: object; ldap: object }>) => {
    const next = await put<Config>("/signin", { require2fa: cfg.require2fa, oidc: cfg.oidc, ldap: cfg.ldap, ...part });
    setCfg(next);
  };
  const policyText: Record<string, string> = { nobody: "", admins: "On: required for administrators", everyone: "On: required for everyone" };
  const twoStepSummary = policyText[cfg.require2fa] || (cfg.twoStepUsers > 0 ? `Optional, ${cfg.twoStepUsers} user${cfg.twoStepUsers === 1 ? " has" : "s have"} it on` : "Off");
  return (
    <>
      <div className="head">
        <h1>Sign-in</h1>
      </div>
      <p className="muted">The local administrator account always works, whatever is set here.</p>
      <div className="si">
        <SectionFrame id="si-twostep" title="Two-step sign-in" summary={twoStepSummary}>
          <TwoStepSection cfg={cfg} save={save} />
        </SectionFrame>
        <SectionFrame id="si-oidc" title="Identity provider (OIDC)" summary={cfg.oidc.enabled ? `On: ${cfg.oidc.name || cfg.oidc.issuer}` : "Off"}>
          <OidcSection cfg={cfg.oidc} save={(oidc) => save({ oidc })} />
        </SectionFrame>
        <SectionFrame id="si-ldap" title="Directory (LDAP or Active Directory)" summary={cfg.ldap.enabled ? `On: ${cfg.ldap.url}` : "Off"}>
          <LdapSection cfg={cfg.ldap} save={(ldap) => save({ ldap })} />
        </SectionFrame>
      </div>
    </>
  );
}

function Saved({ ok }: { ok: boolean }) {
  return ok ? <span className="muted" role="status">Saved.</span> : null;
}

function Check({ checked, onChange, children, hint }: { checked: boolean; onChange: (v: boolean) => void; children: ReactNode; hint?: string }) {
  return (
    <label className="si-check">
      <input type="checkbox" checked={checked} onChange={(e) => onChange(e.target.checked)} /> {children}
      {hint && <small className="muted">{hint}</small>}
    </label>
  );
}

function Report({ steps }: { steps?: Step[] }) {
  if (!steps) return null;
  return (
    <ul className="si-report" aria-label="Test result">
      {steps.map((s, i) => (
        <li key={i}>
          <StatusDot status={(s.ok ? "ok" : "error") as CheckStatus} />
          <span>{s.text}</span>
        </li>
      ))}
    </ul>
  );
}

function TwoStepSection({ cfg, save }: { cfg: Config; save: (p: { require2fa: string }) => Promise<void> }) {
  const [value, setValue] = useState(cfg.require2fa);
  const [saved, setSaved] = useState(false);
  const act = useAction();
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setSaved(false);
    if (await act.run(() => save({ require2fa: value }))) setSaved(true);
  };
  return (
    <form className="si-form" onSubmit={submit}>
      <p className="muted">{APPS_LINE} Each user turns it on under My account. People who sign in through an identity provider use that provider's own second factor.</p>
      <Field label="Require two-step sign-in for" hint="Someone who must have it and has not set it up is taken to the set-up page right after signing in.">
        <Select
          label="Require two-step sign-in for"
          value={value}
          onChange={(v) => setValue(v as Config["require2fa"])}
          options={[
            { value: "nobody", label: "Nobody (each user decides)" },
            { value: "admins", label: "Administrators" },
            { value: "everyone", label: "Everyone" },
          ]}
        />
      </Field>
      <ErrorBox error={act.error} />
      <div className="row">
        <button className="primary" disabled={act.busy}>
          Save
        </button>
        <Saved ok={saved} />
      </div>
    </form>
  );
}

function OidcSection({ cfg, save }: { cfg: Oidc; save: (o: object) => Promise<void> }) {
  const [d, setD] = useState({ ...cfg, defaultRole: cfg.defaultRole || "none" });
  const [secret, setSecret] = useState("");
  const [steps, setSteps] = useState<Step[] | undefined>();
  const [saved, setSaved] = useState(false);
  const [copied, setCopied] = useState(false);
  const act = useAction();
  const testing = useAction();
  const edit = (p: Partial<Oidc>) => {
    setD({ ...d, ...p });
    setSaved(false);
  };
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setSaved(false);
    if (await act.run(() => save({ ...d, clientSecret: secret }))) {
      setSecret("");
      setSaved(true);
    }
  };
  const test = () => testing.run(async () => setSteps((await post<{ steps: Step[] }>("/signin/oidc/test", { issuer: d.issuer, scopes: d.scopes, groupsClaim: d.groupsClaim })).steps));
  return (
    <form className="si-form" onSubmit={submit}>
      <p className="muted">Lets people sign in with an account at your identity provider. The provider's own second factor applies.</p>
      <Check checked={d.enabled} onChange={(v) => edit({ enabled: v })}>
        Allow sign-in with an identity provider
      </Check>
      <Field label="Provider name" hint={`The button says "Sign in with ${d.name || "..."}".`}>
        <input value={d.name} onChange={(e) => edit({ name: e.target.value })} placeholder="Microsoft Entra ID" />
      </Field>
      <Field label="Issuer URL">
        <input value={d.issuer} onChange={(e) => edit({ issuer: e.target.value })} placeholder="https://login.example.org/realms/main" />
      </Field>
      <div className="grid2">
        <Field label="Client ID">
          <input value={d.clientId} onChange={(e) => edit({ clientId: e.target.value })} autoComplete="off" />
        </Field>
        <Field label="Client secret" hint={cfg.clientSecretSet ? "A secret is saved. Leave empty to keep it." : undefined}>
          <input type="password" value={secret} onChange={(e) => setSecret(e.target.value)} placeholder={cfg.clientSecretSet ? "Saved" : ""} autoComplete="new-password" />
        </Field>
      </div>
      <Field label="Redirect URL" hint="Register this address at the provider.">
        <div className="si-redirect">
          <code>{cfg.redirectUrl}</code>
          <button type="button" onClick={() => navigator.clipboard?.writeText(cfg.redirectUrl).then(() => setCopied(true))}>
            {copied ? "Copied" : "Copy"}
          </button>
        </div>
      </Field>
      <Field label="Group that gives the administrator role">
        <input value={d.adminGroup} onChange={(e) => edit({ adminGroup: e.target.value })} />
      </Field>
      <Field label="Group that gives the operator role">
        <input value={d.operatorGroup} onChange={(e) => edit({ operatorGroup: e.target.value })} />
      </Field>
      <Field label="Everyone else who signs in gets">
        <Select label="Everyone else who signs in gets" value={d.defaultRole} onChange={(v) => edit({ defaultRole: v })} options={roleOptions} />
      </Field>
      <DialogAdvanced>
        <Field label="Groups claim" hint="The token field that lists the user's groups.">
          <input value={d.groupsClaim} onChange={(e) => edit({ groupsClaim: e.target.value })} placeholder="groups" />
        </Field>
        <Field label="User name claim" hint="Falls back to the e-mail address.">
          <input value={d.userClaim} onChange={(e) => edit({ userClaim: e.target.value })} placeholder="preferred_username" />
        </Field>
        <Field label="Scopes" hint="Add groups when the provider needs it.">
          <input value={d.scopes} onChange={(e) => edit({ scopes: e.target.value })} placeholder="openid profile email" />
        </Field>
      </DialogAdvanced>
      <ErrorBox error={act.error ?? testing.error} />
      <Report steps={steps} />
      <div className="row">
        <button className="primary" disabled={act.busy}>
          Save
        </button>
        <button type="button" disabled={testing.busy || !d.issuer} onClick={test}>
          Test
        </button>
        <Saved ok={saved} />
      </div>
    </form>
  );
}

function LdapSection({ cfg, save }: { cfg: Ldap; save: (o: object) => Promise<void> }) {
  const [d, setD] = useState({ ...cfg, defaultRole: cfg.defaultRole || "none" });
  const [pw, setPw] = useState("");
  const [testUser, setTestUser] = useState("");
  const [testPw, setTestPw] = useState("");
  const [steps, setSteps] = useState<Step[] | undefined>();
  const [saved, setSaved] = useState(false);
  const act = useAction();
  const testing = useAction();
  const edit = (p: Partial<Ldap>) => {
    setD({ ...d, ...p });
    setSaved(false);
  };
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setSaved(false);
    if (await act.run(() => save({ ...d, bindPassword: pw }))) {
      setPw("");
      setSaved(true);
    }
  };
  const test = () =>
    testing.run(async () => setSteps((await post<{ steps: Step[] }>("/signin/ldap/test", { ...d, bindPassword: pw, testUser, testPassword: testPw })).steps));
  return (
    <form className="si-form" onSubmit={submit}>
      <p className="muted">People sign in on the normal form with their directory name and password. A local account of the same name always wins.</p>
      <Check checked={d.enabled} onChange={(v) => edit({ enabled: v })}>
        Allow sign-in with a directory account
      </Check>
      <Field label="Server URL" hint="ldaps:// is encrypted from the start; ldap:// is switched to an encrypted connection (StartTLS).">
        <input value={d.url} onChange={(e) => edit({ url: e.target.value })} placeholder="ldaps://dc.example.org" />
      </Field>
      <div className="grid2">
        <Field label="Bind user" hint="A service account that may search the directory.">
          <input value={d.bindDn} onChange={(e) => edit({ bindDn: e.target.value })} placeholder="cn=virticle,ou=service,dc=example,dc=org" autoComplete="off" />
        </Field>
        <Field label="Bind password" hint={cfg.bindPasswordSet ? "A password is saved. Leave empty to keep it." : undefined}>
          <input type="password" value={pw} onChange={(e) => setPw(e.target.value)} placeholder={cfg.bindPasswordSet ? "Saved" : ""} autoComplete="new-password" />
        </Field>
      </div>
      <Field label="Base DN" hint="Where users and groups are searched.">
        <input value={d.baseDn} onChange={(e) => edit({ baseDn: e.target.value })} placeholder="dc=example,dc=org" />
      </Field>
      <Field label="Group that gives the administrator role" hint="A group name or its full DN.">
        <input value={d.adminGroup} onChange={(e) => edit({ adminGroup: e.target.value })} />
      </Field>
      <Field label="Group that gives the operator role">
        <input value={d.operatorGroup} onChange={(e) => edit({ operatorGroup: e.target.value })} />
      </Field>
      <Field label="Everyone else who signs in gets">
        <Select label="Everyone else who signs in gets" value={d.defaultRole} onChange={(v) => edit({ defaultRole: v })} options={roleOptions} />
      </Field>
      <DialogAdvanced>
        <Field label="User filter" hint="{user} is the typed name.">
          <input value={d.userFilter} onChange={(e) => edit({ userFilter: e.target.value })} placeholder="(|(uid={user})(sAMAccountName={user})(mail={user}))" />
        </Field>
        <Field label="Group filter" hint="{dn} is the user's DN, {user} the name. Active Directory also finds nested groups.">
          <input value={d.groupFilter} onChange={(e) => edit({ groupFilter: e.target.value })} placeholder="(|(member={dn})(uniqueMember={dn})(memberUid={user}))" />
        </Field>
        <div className="grid2">
          <Field label="User name attribute" hint="Empty: uid, then sAMAccountName, then mail.">
            <input value={d.nameAttr} onChange={(e) => edit({ nameAttr: e.target.value })} placeholder="uid" />
          </Field>
          <Field label="Group membership attribute">
            <input value={d.memberOfAttr} onChange={(e) => edit({ memberOfAttr: e.target.value })} placeholder="memberOf" />
          </Field>
        </div>
        <Field label="CA certificate" hint="Paste the directory's CA certificate (PEM) when it is not a public one.">
          <textarea rows={4} value={d.caCert} onChange={(e) => edit({ caCert: e.target.value })} className="mono" spellCheck={false} />
        </Field>
        <Field label="Timeout in seconds">
          <input type="number" min={1} max={60} value={d.timeoutSeconds || ""} onChange={(e) => edit({ timeoutSeconds: Number(e.target.value) || 0 })} placeholder="5" />
        </Field>
        <Check checked={d.allowPlain} onChange={(v) => edit({ allowPlain: v })}>
          Allow plain LDAP without encryption
        </Check>
        {d.allowPlain && <p className="si-warn">Passwords travel unencrypted. Use this only on a network you trust completely.</p>}
      </DialogAdvanced>
      <h3>Test</h3>
      <div className="grid2">
        <Field label="Test user name">
          <input value={testUser} onChange={(e) => setTestUser(e.target.value)} autoComplete="off" />
        </Field>
        <Field label="Test password (optional)" hint="Checks the user's password too. It is not stored.">
          <input type="password" value={testPw} onChange={(e) => setTestPw(e.target.value)} autoComplete="new-password" />
        </Field>
      </div>
      <ErrorBox error={act.error ?? testing.error} />
      <Report steps={steps} />
      <div className="row">
        <button className="primary" disabled={act.busy}>
          Save
        </button>
        <button type="button" disabled={testing.busy || !d.url} onClick={test}>
          Test
        </button>
        <Saved ok={saved} />
      </div>
    </form>
  );
}
