// /console/:node/:uuid — the console alone, full window: no top bar, no tree.
import { useEffect } from "react";
import { useParams, useSearchParams } from "react-router-dom";
import { get, VMDetail } from "../api";
import { useRole } from "../auth";
import { useLive } from "../live";
import { SerialConsole, VNCConsole } from "./Console";
import "./console.css";

export default function ConsolePage() {
  const { node: nodeParam, uuid = "" } = useParams();
  const node = Number(nodeParam);
  const [params] = useSearchParams();
  const viewer = useRole() === "viewer";
  const vm = useLive(
    () => get<VMDetail>(`/nodes/${node}/vms/${uuid}`),
    `${node}/${uuid}`,
    (e) => (e.type === "node" ? true : e.type === "domain" ? e.uuid === uuid : false),
  );
  const d = vm.data;
  useEffect(() => {
    document.title = d?.name ?? "Console";
    return () => void (document.title = "virt-proj");
  }, [d?.name]);

  const msg = (text: string) => <div className="empty" style={{ margin: 24 }}>{text}</div>;
  if (viewer) return msg("The console is available to operators and administrators.");
  if (!d) return vm.error ? msg(vm.error) : null;
  if (d.state === "shutoff") return msg(`${d.name} is shut off. Start it and this page connects by itself.`);
  const serial = params.get("kind") === "serial" || !d.hasVNC;
  if (serial && !d.hasSerial) return msg(`${d.name} has neither a VNC display nor a serial console.`);
  return serial ? <SerialConsole node={node} uuid={uuid} variant="page" /> : <VNCConsole node={node} uuid={uuid} name={d.name} variant="page" />;
}
