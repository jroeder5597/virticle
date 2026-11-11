import { useEffect, useRef } from "react";
import { ServerEvent } from "./api";
import { useEvents, useLive } from "./live";

/**
 * useLive, but a burst of events (several nodes reporting at once) causes one
 * refetch, `ms` after the last event. For lists that cost one request for all nodes.
 * The "resync" sent when the event socket opens is skipped when the data was fetched a moment ago.
 */
export function useLiveDebounced<T>(load: () => Promise<T>, key: string, when: (e: ServerEvent) => boolean, ms = 500) {
  const fetchedAt = useRef(0);
  const r = useLive(
    () => {
      fetchedAt.current = Date.now();
      return load();
    },
    key,
    () => false,
  );
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>();
  const reload = useRef(r.reload);
  reload.current = r.reload;
  useEvents((e) => {
    if (!when(e)) return;
    if (e.detail === "resync" && Date.now() - fetchedAt.current < 3000) return;
    clearTimeout(timer.current);
    timer.current = setTimeout(() => reload.current(), ms);
  });
  useEffect(() => () => clearTimeout(timer.current), []);
  return r;
}
