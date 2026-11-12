// First-run offer to set up the controller's own machine as a node. This controller already has nodes and the test
// changes none: it checks the documented fields of GET /api/local-node and that the start panel is absent on the
// Datacenter page. Not covered: the panel itself, its SSH-not-running line and the prepare and auto-add flow, which
// need a controller with no nodes.
import { expect, test } from "../lib/fixtures";

test("local-node API returns the documented fields", async ({ api }) => {
  const r = await api.get("/local-node");
  expect(typeof r.capable).toBe("boolean");
  expect(typeof r.kvmDevice).toBe("boolean");
  expect(typeof r.sshReachable).toBe("boolean");
  expect(typeof r.registered).toBe("boolean");
  expect(typeof r.hostname).toBe("string");
  expect(r.address).toMatch(/^[0-9.]+$/);
  if (!r.capable) expect(r.reason).toBeTruthy();
});

test("no start panel when nodes exist", async ({ page, api }) => {
  expect((await api.nodes()).length).toBeGreaterThan(0);
  await page.goto("/datacenter");
  await expect(page.getByRole("heading", { name: "Nodes", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Set up this machine as a node" })).toHaveCount(0);
  await expect(page.getByText("No nodes yet.")).toHaveCount(0);
});
