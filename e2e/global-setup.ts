import { mkdirSync } from "fs";
import { Api, sweep } from "./lib/api";
import { authFile, env } from "./lib/env";

export default async function () {
  if (!env.pass) throw new Error("set E2E_USER and E2E_PASS (an administrator of the controller)");
  const api = await Api.login();
  mkdirSync(".auth", { recursive: true });
  await api.ctx.storageState({ path: authFile });
  for (const n of [env.node1, env.node2]) await api.nodeId(n); // fails early when a node is missing
  const left = await sweep(api, true);
  console.log(`global setup: removed ${left.length} leftover object(s) with prefix "${env.prefix}"`);
  await api.dispose();
}
