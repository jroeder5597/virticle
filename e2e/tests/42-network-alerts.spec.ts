// Alerts for network links: API shape only. A healthy lab has nothing to raise (a card without link, a degraded
// bond), so this checks that the alert list answers, that every alert has the documented fields, and that no
// network alert exists on a lab where nothing is wrong. The behaviour itself is checked by hand (see the lab
// checklist of the change): make a card of a bridge lose link with `virsh domif-setlink`.
import { expect, test } from "../lib/fixtures";

test.describe("network link alerts", () => {
  test("the alert list answers and every alert is complete", async ({ api }) => {
    const r = await api.get("/alerts");
    expect(Array.isArray(r.alerts)).toBe(true);
    for (const a of r.alerts) {
      expect(typeof a.id).toBe("string");
      expect(["warning", "critical"]).toContain(a.severity);
      expect(a.title.length).toBeGreaterThan(0);
      expect(a.since).toBeGreaterThan(0);
      if (a.kind === "net-link" || a.kind === "net-bond") {
        expect(a.node, a.id).toBeGreaterThan(0);
        expect(a.link, a.id).toMatch(/^\/node\/\d+\/topology$/);
        expect(a.title, a.id).toMatch(/ on .+ (has no link\.|runs on \d+ of \d+ cards\.)$|^LACP is not answered by the switch for /);
      }
    }
  });

  test("a healthy lab has no card or bond alerts", async ({ api }) => {
    const r = await api.get("/alerts");
    const net = r.alerts.filter((a: any) => a.kind === "net-link" || a.kind === "net-bond");
    expect(net.map((a: any) => a.title)).toEqual([]);
  });
});
