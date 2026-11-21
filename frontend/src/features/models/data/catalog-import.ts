// 职责: 请求模型目录预览、批量应用和站点定时检查设置。
// 关联文件:
//   frontend/src/features/models/components/models-catalog-import.tsx
//   internal/server/gql/model.graphql
//   frontend/src/features/models/data/models.ts
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { graphqlRequest } from '@/gql/graphql';

export const priceFields = ['input', 'output', 'cacheRead', 'cacheWrite'] as const;
export type PriceField = (typeof priceFields)[number];
export interface ImportScope {
  channels: boolean;
  providers: string[];
  ignorePrefix: boolean;
  tracked: boolean;
}
export interface ImportRate {
  field: PriceField;
  base: number | null;
  tiers: { label: string; value: number }[];
}
export interface Offer {
  provider: string;
  modelID: string;
  name: string;
  developer: string;
  canonical: string;
  type: string;
  rates: ImportRate[];
}
export interface LocalRate {
  field: PriceField;
  value: number | null;
  follow: boolean;
  provider: string;
  modelID: string;
}
export interface ImportRow {
  mergedVersions?: string[];
  modelID: string;
  name: string;
  developer: string;
  aliases: string[];
  version: string;
  quoteVersion: string;
  archived: boolean;
  changed: boolean;
  preferred: Offer | null;
  offers: Offer[];
  prices: LocalRate[];
}
export interface CatalogPreview {
  providers: string[];
  revision: string;
  source: string;
  upstreamURL: string;
  rows: ImportRow[];
}
export interface PriceSelection {
  field: PriceField;
  action: 'FOLLOW' | 'FIX' | 'FIX_BASE' | 'TIER';
  provider: string;
  modelID: string;
  tier?: string;
}
export interface ImportSelection {
  modelID: string;
  name: string;
  developer: string;
  aliases: string[];
  version: string;
  provider: string;
  type: string;
  sourceModelID: string;
  quoteVersions: string[];
  prices: PriceSelection[];
}
export interface CheckSettings {
  enabled: boolean;
  time: string;
  timezone: string;
  providers: string[];
  ignorePrefix: boolean;
}
export interface CheckStatus {
  settings: CheckSettings;
  nextCheck: string | null;
  lastAttempt: string | null;
  lastSuccess: string | null;
  failed: boolean;
  available: number;
  changed: number;
}
export interface Outcome {
  modelID: string;
  status: string;
}

const offerFields = 'provider modelID name developer canonical type rates { field base tiers { label value } }';
const statusFields = 'settings { enabled time timezone providers ignorePrefix } nextCheck lastAttempt lastSuccess failed available changed';
const previewQuery = `query CatalogImportPreview($scope: CatalogImportScope!) {
  catalogImportPreview(scope: $scope) {
    source upstreamURL revision providers rows { modelID name developer aliases version quoteVersion archived changed
      offers { ${offerFields} } preferred { ${offerFields} } prices { field value follow provider modelID }
    }
  }
}`;
export function useCatalogPreview(scope: ImportScope) {
  return useQuery({
    queryKey: ['catalog-import', scope],
    queryFn: async ({ signal }) =>
      (await graphqlRequest<{ catalogImportPreview: CatalogPreview }>(previewQuery, { scope }, undefined, { signal })).catalogImportPreview,
    refetchOnWindowFocus: false,
    staleTime: 60_000,
  });
}
export function useCatalogApply() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: async (variables: { scope: ImportScope; revision: string; selections: ImportSelection[] }) =>
      (
        await graphqlRequest<{ applyCatalogImport: Outcome[] }>(
          `mutation ApplyCatalogImport($scope: CatalogImportScope!, $revision: String!, $selections: [CatalogImportSelection!]!) {
        applyCatalogImport(scope: $scope, revision: $revision, selections: $selections) { modelID status }
      }`,
          variables
        )
      ).applyCatalogImport,
    onSuccess: () => {
      client.invalidateQueries({ queryKey: ['models'] });
      client.invalidateQueries({ queryKey: ['catalog-import'] });
    },
  });
}
export function useCatalogCheckStatus() {
  return useQuery({
    queryKey: ['model-catalog-check'],
    queryFn: async () =>
      (await graphqlRequest<{ catalogCheckStatus: CheckStatus }>(`query CatalogCheckStatus { catalogCheckStatus { ${statusFields} } }`))
        .catalogCheckStatus,
    refetchInterval: 60_000,
  });
}
export function useSaveCatalogSchedule() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: async (input: CheckSettings) =>
      (
        await graphqlRequest<{ saveCatalogCheckSettings: CheckStatus }>(
          `mutation SaveCatalogCheckSettings($input: CatalogCheckSettingsInput!) { saveCatalogCheckSettings(input: $input) { ${statusFields} } }`,
          { input }
        )
      ).saveCatalogCheckSettings,
    onSuccess: (data) => client.setQueryData(['model-catalog-check'], data),
  });
}
export function useCheckCatalogNow() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: async () =>
      (
        await graphqlRequest<{ checkModelCatalogNow: CheckStatus }>(
          `mutation CheckModelCatalogNow { checkModelCatalogNow { ${statusFields} } }`
        )
      ).checkModelCatalogNow,
    onSuccess: (data) => {
      client.setQueryData(['model-catalog-check'], data);
      client.invalidateQueries({ queryKey: ['catalog-import'] });
    },
  });
}
