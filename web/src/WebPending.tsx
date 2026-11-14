import { useCallback, useEffect, useRef, useState } from "react";
import { ApiError, get, post } from "./api";
import { StatusLine } from "./Checks";
import { ErrorBox, Modal } from "./ui";
import "./webpending.css";

interface Pending {
  secondsLeft: number;
  previousMode: string;
  url: string;
}
interface Web {
  status: { pending?: Pending };
  onNewListener: boolean;
}

/**
 * A web access change that waits for confirmation, on any page: a dialog when the app loads, then a line above
 * the page if the dialog is closed. The Settings > Web access page has its own confirm and shows neither.
 */
export default function WebPending({ page }: { page: string }) {
  const [web, setWeb] = useState<{ pending: Pending; onNew: boolean }>();
  const [left, setLeft] = useState(0);
  const [ended, setEnded] = useState(false);
  const [closed, setClosed] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const was = useRef<string>();
  const seen = useRef(false); // a change was shown at least once, so a missing one means it ended

  const load = useCallback(async () => {
    try {
      const r = await get<Web>("/web");
      const p = r.status.pending;
      if (p) {
        seen.current = true;
        setWeb({ pending: p, onNew: r.onNewListener });
        setLeft(p.secondsLeft);
        setEnded(false);
      } else {
        setWeb(undefined);
        if (seen.current) setEnded(true);
      }
    } catch {
      // The listener moved back, so this page lost its connection: the change is over.
      setWeb(undefined);
      if (seen.current) setEnded(true);
    }
  }, []);

  // When the app loads, and when the user leaves the Settings page (a change may have been applied there).
  useEffect(() => {
    if (page !== "/web-access" && (was.current === undefined || was.current === "/web-access")) void load();
    was.current = page;
  }, [page, load]);

  // One tick a second while a change waits; at zero look again once the server has had time to undo it.
  const waiting = !!web;
  useEffect(() => {
    if (!waiting) return;
    const t = setInterval(() => setLeft((n) => Math.max(0, n - 1)), 1000);
    return () => clearInterval(t);
  }, [waiting]);
  useEffect(() => {
    if (!waiting || left > 0) return;
    const t = setTimeout(load, 2000);
    return () => clearTimeout(t);
  }, [waiting, left, load]);

  const run = async (path: string) => {
    setBusy(true);
    setError(undefined);
    try {
      await post(path);
      setWeb(undefined);
      seen.current = path === "/web/revert";
      setEnded(path === "/web/revert");
      setClosed(false);
    } catch (e: any) {
      if (path === "/web/revert" && !(e instanceof ApiError)) {
        // Undoing closes the listener this page is on: the lost connection means it worked.
        setWeb(undefined);
        seen.current = true;
        setEnded(true);
      } else setError(String(e.message ?? e));
    } finally {
      setBusy(false);
    }
  };
  const dismissEnded = () => {
    setEnded(false);
    seen.current = false;
  };

  if (page === "/web-access") return null;
  if (ended)
    return (
      <Modal title="Web access change ended" onClose={dismissEnded}>
        <p>The change was not kept, so the previous web access settings are back in use. If this page stops responding, open the previous address.</p>
        <div className="actions">
          <button className="primary" onClick={dismissEnded}>
            Close
          </button>
        </div>
      </Modal>
    );
  if (!web) return null;
  const { pending: p, onNew } = web;
  const sentence = onNew
    ? `Web access was changed to ${p.url}. It is undone in ${left} seconds unless you keep it.`
    : `Web access was changed to ${p.url}. Open that address and choose Keep this setting there, or it is undone in ${left} seconds.`;
  const buttons = (
    <>
      {onNew ? (
        <button className="primary" disabled={busy} onClick={() => run("/web/confirm")}>
          Keep this setting
        </button>
      ) : (
        <a className="button primary" href={p.url}>
          Open new address
        </a>
      )}
      <button disabled={busy} onClick={() => run("/web/revert")}>
        Undo now
      </button>
    </>
  );
  if (closed)
    return (
      <div className="wa-banner" data-wa="pending">
        <StatusLine status="warning">{sentence}</StatusLine>
        {buttons}
        <ErrorBox error={error} />
      </div>
    );
  return (
    <Modal title="Keep the new web access setting?" onClose={() => setClosed(true)}>
      <p>{sentence}</p>
      <ErrorBox error={error} />
      <div className="actions">{buttons}</div>
    </Modal>
  );
}
