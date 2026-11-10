// Thin client for the JSON API. Errors carry the server's message, which for
// libvirt failures is libvirt's own error text.

export class ApiError extends Error {
  constructor(public status: number, message: string) {
    super(message);
  }
}

let onUnauthorized: () => void = () => {};
export function setUnauthorizedHandler(fn: () => void) {
  onUnauthorized = fn;
}

export async function api<T = unknown>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch("/api" + path, {
    method,
    headers: body === undefined ? undefined : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  let data: any = null;
  try {
    data = await res.json();
  } catch {
    /* empty or non-JSON body */
  }
  if (!res.ok) {
    if (res.status === 401 && path !== "/login" && path !== "/setup") onUnauthorized();
    throw new ApiError(res.status, data?.error ?? `${res.status} ${res.statusText}`);
  }
  return data as T;
}

export const get = <T,>(path: string) => api<T>("GET", path);
export const post = <T = unknown,>(path: string, body: unknown = {}) => api<T>("POST", path, body);
export const put = <T = unknown,>(path: string, body: unknown = {}) => api<T>("PUT", path, body);
export const del = <T = unknown,>(path: string) => api<T>("DELETE", path);

/** Upload a file as a new volume, reporting progress from 0 to 1. */
export function uploadVolume(node: number, pool: string, file: File, onProgress: (p: number) => void): Promise<void> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open("PUT", `/api/nodes/${node}/pools/${encodeURIComponent(pool)}/volumes/${encodeURIComponent(file.name)}`);
    xhr.upload.onprogress = (e) => e.lengthComputable && onProgress(e.loaded / e.total);
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) return resolve();
      let msg = `${xhr.status} ${xhr.statusText}`;
      try {
        msg = JSON.parse(xhr.responseText).error ?? msg;
      } catch {
        /* keep the status text */
      }
      reject(new ApiError(xhr.status, msg));
    };
    xhr.onerror = () => reject(new Error("upload failed: connection lost"));
    xhr.send(file);
  });
}

export function wsURL(path: string): string {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${location.host}/api${path}`;
}

// --- types mirrored from the server ---

export interface NodeStatus {
  id: number;
  addr: string;
  user: string;
  jump?: string;
  state: "connecting" | "online" | "offline";
  error?: string;
  hostname?: string;
}
export interface NodeInfo {
  hostname: string;
  cpuModel: string;
  cpus: number;
  memoryKiB: number;
  freeKiB: number;
  libvirtVersion: string;
}
export interface VM {
  node: number;
  nodeName: string;
  uuid: string;
  name: string;
  state: string;
  vcpus: number;
  memoryKiB: number;
  autostart: boolean;
  template: boolean;
  sealed?: "linux" | "windows";
}
export interface VMDetail extends VM {
  title?: string;
  description?: string;
  arch: string;
  machine: string;
  firmware: string;
  boot: string[];
  disks: { target: string; bus: string; device: string; format?: string; source?: string; readOnly: boolean }[];
  nics: { mac: string; type: string; source: string; model?: string; ips: string[] }[];
  hasVNC: boolean;
  hasSerial: boolean;
  tpm: boolean;
  passthroughDevices: number;
  agent: "connected" | "disconnected" | "absent";
  snapshots: string[];
  xml: string;
}
export interface Pool {
  name: string;
  uuid: string;
  type: string;
  target?: string;
  state: string;
  autostart: boolean;
  capacity: number;
  allocation: number;
  available: number;
}
export interface Volume {
  name: string;
  path: string;
  format?: string;
  capacity: number;
  allocation: number;
}
export interface Network {
  name: string;
  active: boolean;
  autostart: boolean;
  bridge?: string;
  forward: string;
  address?: string;
}
export interface Task {
  id: number;
  kind: string;
  target: string;
  node: string;
  user: string;
  status: "running" | "ok" | "failed";
  error?: string;
  progress: number;
  started: number;
  finished?: number;
}
export interface ServerEvent {
  type: "node" | "domain" | "task";
  node?: number;
  name?: string;
  uuid?: string;
  detail?: string;
  task?: Task;
}

export function bytes(n: number): string {
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i++;
  }
  return `${n >= 100 || i === 0 ? n.toFixed(0) : n.toFixed(1)} ${units[i]}`;
}
