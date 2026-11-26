import { countAvailable, lastSegment } from "./matching.js";

const channelsQuery = `query Channels($after: Cursor) {
  channels(first: 100, after: $after, where: {statusIn: [enabled, disabled]}) {
    edges { node { supportedModels } } pageInfo { hasNextPage endCursor }
  }
}`;
const modelsQuery = `query Models($after: Cursor) {
  models(first: 100, after: $after) {
    edges { node { modelID status catalogAliases catalogPrices { field value follow provider modelID canonical } } }
    pageInfo { hasNextPage endCursor }
  }
}`;
const offersQuery = `query Offers($providers: [String!], $after: String, $refresh: Boolean!, $first: Int!) {
  modelCatalogOffers(providers: $providers, after: $after, refresh: $refresh, first: $first) {
    source fetchedAt nextCursor offers { provider modelID canonical identity rates { field base } }
  }
}`;

function query(ctx, document, variables) {
  const result = ctx.graphql(document, variables);
  if (result.errors?.length) throw new Error(result.errors[0].message);
  return result.data;
}

function readNodes(ctx, document, field, visit) {
  let after = null;
  do {
    const page = query(ctx, document, { after })[field];
    for (const { node } of page.edges) visit(node);
    if (page.pageInfo.hasNextPage && !page.pageInfo.endCursor) throw new Error("Incomplete " + field + " page");
    after = page.pageInfo.hasNextPage ? page.pageInfo.endCursor : null;
  } while (after);
}

export function onTask(ctx) {
  const providers = [...new Set((ctx.config.providers ?? []).map((id) => id.trim()).filter(Boolean))].sort();
  const includeChannels = ctx.config.channels ?? true;
  const scope = { channels: includeChannels, providers, ignorePrefix: includeChannels && (ctx.config.ignorePrefix ?? false) };
  const channels = new Set();
  if (scope.channels) {
    readNodes(ctx, channelsQuery, "channels", ({ supportedModels }) => {
      for (const id of supportedModels ?? []) if (id) channels.add(id);
    });
  }

  const imported = new Set();
  const followed = new Map();
  const sources = new Set(providers);
  readNodes(ctx, modelsQuery, "models", (model) => {
    imported.add(model.modelID);
    for (const alias of model.catalogAliases) imported.add(alias);
    if (model.status === "archived") return;
    for (const price of model.catalogPrices) {
      if (!price.follow || !price.provider || !price.modelID) continue;
      sources.add(price.provider);
      const key = JSON.stringify([price.provider, price.modelID]);
      if (!followed.has(key)) followed.set(key, []);
      followed.get(key).push({ modelID: model.modelID, price });
    }
  });

  const candidates = new Set(channels);
  const changed = new Set();
  // Only channel IDs and their possible merge targets need identity evidence.
  const mergeIDs = new Set();
  if (scope.ignorePrefix) {
    for (const id of channels) {
      mergeIDs.add(id);
      mergeIDs.add(lastSegment(id));
    }
  }
  const identities = [];
  // An empty provider filter means the whole catalog. If only exact channel IDs
  // matter, a single upstream offer suffices to verify freshness.
  const checkSourceOnly = !mergeIDs.size && sources.size === 0;
  const filter = mergeIDs.size ? [] : [...sources];
  let after = null;
  let fetchedAt;
  do {
    const { modelCatalogOffers: page } = query(ctx, offersQuery, {
      providers: filter, after, refresh: after === null, first: checkSourceOnly ? 1 : 100,
    });
    if (page.source !== "upstream") throw new Error("The upstream model catalog is unavailable");
    fetchedAt = page.fetchedAt;
    for (const offer of page.offers) {
      if (providers.includes(offer.provider)) candidates.add(offer.modelID);
      if (mergeIDs.has(offer.modelID) || mergeIDs.has(offer.canonical)) {
        identities.push({ id: offer.modelID, canonical: offer.canonical, identity: offer.identity });
      }
      for (const binding of followed.get(JSON.stringify([offer.provider, offer.modelID])) ?? []) {
        const { price } = binding;
        const rate = offer.rates.find((rate) => rate.field === price.field);
        if (price.canonical === offer.canonical && rate?.base != null && rate.base !== price.value) {
          changed.add(binding.modelID);
        }
      }
    }
    after = checkSourceOnly ? null : page.nextCursor;
  } while (after);

  const available = countAvailable(candidates, imported, channels, scope.ignorePrefix, identities);
  const count = available + changed.size;
  ctx.kv.set("summary", JSON.stringify({ scope, fetchedAt, available, changed: changed.size }));
  ctx.ui.setBadge({ type: "action", key: "catalog-check" }, count ? { count } : null);
}
