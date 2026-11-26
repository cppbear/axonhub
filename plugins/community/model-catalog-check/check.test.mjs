import assert from "node:assert/strict";
import test from "node:test";
import { onTask } from "./plugin.js";
import { countAvailable } from "./matching.js";

function offer(id, canonical = "dev/x", version = "1") {
  return { id, canonical, identity: JSON.stringify([canonical, version, ""]) };
}

test("prefix matching uses the native importer's identity and target occupancy rules", () => {
  const ids = new Set(["mirror/one/x", "other/x"]);
  const compatible = [offer("mirror/one/x"), offer("other/x", "dev/x", "")];
  const available = (offers, occupied = []) => countAvailable(ids, new Set(occupied), ids, true, offers);
  assert.equal(available(compatible), 1);
  assert.equal(available(compatible, ["x"]), 2);
  assert.equal(available([offer("mirror/one/x"), offer("other/x", "dev/x", "2")]), 2);
  assert.equal(available([offer("mirror/one/x")]), 2);
  assert.equal(available([offer("mirror/one/x"), offer("other/x", "")]), 2);
  assert.equal(countAvailable(ids, new Set(), ids, false, compatible), 2);
  const aliases = new Set(["dev/x", "other/x"]);
  assert.equal(countAvailable(aliases, new Set(), aliases, true, [offer("other/x")]), 1);
  assert.equal(countAvailable(aliases, new Set(), aliases, true, [offer("other/x"), offer("second/x")]), 2);
});

test("pages all sources, counts once per model, publishes only complete upstream results", () => {
  const reads = [];
  let lastSummary, badge, failure;
  const binding = { follow: true, provider: "original", modelID: "bound", canonical: "dev/x", value: 3 };
  const ctx = {
    config: { providers: [" extra ", "extra"] },
    graphql(document, variables) {
      const next = variables.after === null;
      const connection = (nodes) => ({ edges: nodes.map((node) => ({ node })), pageInfo: { hasNextPage: next, endCursor: "next" } });
      if (document.includes("query Channels")) {
        return { data: { channels: connection([{ supportedModels: next ? ["unmatched", "shared"] : ["shared", "archived-alias"] }]) } };
      }
      if (document.includes("query Models")) {
        const model = next ? { modelID: "local", status: "enabled", catalogAliases: [], catalogPrices: [
          { ...binding, field: "input" }, { ...binding, field: "output" }, { ...binding, follow: false, field: "cacheRead" },
        ] } : { modelID: "archived", status: "archived", catalogAliases: ["archived-alias"], catalogPrices: [{ ...binding, field: "input" }] };
        return { data: { models: connection([model]) } };
      }
      reads.push(variables);
      if (!next && failure === "cursor") return { errors: [{ message: "snapshot changed" }] };
      const offers = next ? [{ provider: "extra", modelID: "shared", rates: [] }] : [
        { provider: "original", modelID: "bound", canonical: "dev/x", rates: [{ field: "input", base: 0 }, { field: "output", base: 4 }] },
        { provider: "original", modelID: "not-subscribed", rates: [] },
      ];
      return { data: { modelCatalogOffers: { source: !next && failure === "fallback" ? "fallback" : "upstream", fetchedAt: "2026-11-26", offers, nextCursor: next ? "second" : null } } };
    },
    kv: { set(_key, value) { lastSummary = JSON.parse(value); } },
    ui: { setBadge(_target, value) { if (failure === "badge") throw Error("badge failed"); badge = value; } },
  };
  onTask(ctx);
  const first = lastSummary;
  assert.deepEqual(first, { scope: { channels: true, providers: ["extra"], ignorePrefix: false }, fetchedAt: "2026-11-26", available: 2, changed: 1 });
  assert.deepEqual(badge, { count: 3 });
  assert.deepEqual(reads.map((r) => [r.refresh, r.after, r.providers]), [[true, null, ["extra", "original"]], [false, "second", ["extra", "original"]]]);
  ctx.config = { channels: false, providers: [] };
  for (failure of ["cursor", "fallback"]) {
    assert.throws(() => onTask(ctx));
    assert.equal(lastSummary, first);
    assert.deepEqual(badge, { count: 3 });
  }
  failure = "badge";
  assert.throws(() => onTask(ctx), /badge failed/);
  assert.deepEqual(lastSummary.scope, { channels: false, providers: [], ignorePrefix: false });
  assert.deepEqual(badge, { count: 3 });
  failure = undefined;
  onTask(ctx);
  assert.deepEqual(badge, { count: 1 });
});
