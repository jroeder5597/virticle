import { Api, sweep } from "./lib/api";
import { env } from "./lib/env";

export default async function () {
  const api = await Api.login();
  const left = await sweep(api, true);
  console.log(`global teardown: ${left.length} leftover object(s) with prefix "${env.prefix}" removed`);
  await api.dispose();
}
