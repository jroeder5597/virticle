// One WebSocket carries every server event. Components subscribe and refetch
// when something they show has changed, so changes made outside the app (with
// virsh, for instance) appear without a reload.
import { useCallback, useEffect, useRef, useState } from "react";
import { ServerEvent, wsURL } from "./api";

type Listener = (e: ServerEvent) => void;
const listeners = new Set<Listener>();
let socket: WebSocket | null = null;
let retry = 1000;
let wanted = false;

function connect() {
  if (!wanted || socket) return;
  const ws = new WebSocket(wsURL("/events"));
  socket = ws;
  ws.onopen = () => {
    retry = 1000;
    // Anything may have changed while we were disconnected.
    listeners.forEach((l) => l({ type: "node", detail: "resync" }));
  };
  ws.onmessage = (m) => {
    const e = JSON.parse(m.data) as ServerEvent;
    listeners.forEach((l) => l(e));
  };
  ws.onclose = () => {
    socket = null;
    if (wanted) setTimeout(connect, retry);
    retry = Math.min(retry * 2, 15000);
  };
}

export function startEvents() {
  wanted = true;
  connect();
}
export function stopEvents() {
  wanted = false;
  socket?.close();
}

export function useEvents(fn: Listener) {
  const ref = useRef(fn);
  ref.current = fn;
  useEffect(() => {
    const l: Listener = (e) => ref.current(e);
    listeners.add(l);
    return () => void listeners.delete(l);
  }, []);
}

// Network and pool events are only interesting to the pages that show them.
const defaultWhen = (e: ServerEvent) => (e.type === "task" ? e.task?.status !== "running" : e.type !== "network" && e.type !== "pool" && e.type !== "tags");
export const orEvents = (...types: ServerEvent["type"][]) => (e: ServerEvent) => types.includes(e.type) || defaultWhen(e);

/**
 * Load data and keep it fresh: refetches when `key` changes and whenever a
 * server event passes `when` (default: every node or domain event, and every
 * finished task).
 */
export function useLive<T>(load: () => Promise<T>, key: string, when?: (e: ServerEvent) => boolean) {
  const [data, setData] = useState<T | undefined>();
  const [error, setError] = useState<string | undefined>();
  const loadRef = useRef(load);
  loadRef.current = load;
  const seq = useRef(0);

  const reload = useCallback(() => {
    const mine = ++seq.current;
    loadRef.current().then(
      (d) => {
        if (mine === seq.current) {
          setData(d);
          setError(undefined);
        }
      },
      (e) => mine === seq.current && setError(String(e.message ?? e)),
    );
  }, []);

  useEffect(() => {
    setData(undefined);
    reload();
  }, [key, reload]);

  useEvents((e) => {
    const relevant = when ? when(e) : defaultWhen(e);
    if (relevant) reload();
  });
  return { data, error, reload };
}

/** Subscribe outside a component (a module-level store). Returns the unsubscribe function. */
export function onEvent(fn: Listener): () => void {
  listeners.add(fn);
  return () => void listeners.delete(fn);
}
