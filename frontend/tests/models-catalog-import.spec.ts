// 职责: 验证跨页全选、手机端阶梯价选择和已勾选报价在后台刷新时的保护。
// 关联文件:
//   frontend/src/features/models/components/models-catalog-import.tsx
//   frontend/src/features/models/components/models-catalog-prices.tsx
//   frontend/src/features/models/data/catalog-import.ts
// 注意事项:
//   仅替换目录预览与应用响应，避免修改测试站点已有模型。
import { expect, test } from '@playwright/test';
import { gotoAndEnsureAuth } from './auth.utils';

test('a filtered batch keeps the quotes originally reviewed across a background refresh', async ({ page }) => {
  let revision = 'first';
  const provider = 'model-test';
  let reads = 0;
  type Selection = { prices: { field: string; action: string; expected?: number; tier?: string }[] };
  let applied: Selection[] = [];
  await page.route('**/admin/graphql', async (route) => {
    const body = route.request().postDataJSON();
    if (body.operationName === 'OnboardingInfo') {
      const onboardingInfo = { onboarded: true, systemModelSetting: { onboarded: true }, autoDisableChannel: { onboarded: true } };
      return route.fulfill({ json: { data: { onboardingInfo } } });
    }
    if (body.operationName === 'ApplyCatalogImport') {
      applied = body.variables.selections;
      const outcomes = body.variables.selections.map(({ modelID }: { modelID: string }) => ({ modelID, status: 'catalogChanged' }));
      return route.fulfill({ json: { data: { applyCatalogImport: outcomes } } });
    }
    if (body.operationName !== 'CatalogImportPreview') return route.fallback();
    reads++;
    const rows = Array.from({ length: 28 }, (_, i) => {
      const modelID = i < 27 ? 'batch-' + i : 'outside';
      const offer = {
        provider, modelID, name: modelID, developer: provider, identity: 'original', type: 'chat',
        rates: [
          { field: 'input', base: revision === 'first' ? 1 : 2, tiers: [] },
          { field: 'cacheWrite', base: null, tiers: [{ label: 'context 256000', value: 2.5 }] },
        ],
      };
      return {
        modelID, name: modelID, developer: provider, members: [{id:modelID,identity:'original'}], routes: [], version: '',
        mergeTarget: '', mergeTargetOccupied: false, changed: false, archived: false, offers: [offer], preferred: offer, prices: [],
      };
    });
    const preview = { providers: [provider], source: 'upstream', upstreamURL: 'https://example.test/catalog', revision: 'same-source', rows };
    return route.fulfill({ json: { data: { catalogImportPreview: preview } } });
  });
  await gotoAndEnsureAuth(page, '/models');
  await page.getByTestId('catalog-import-open').click();
  const dialog = page.getByRole('dialog');
  await dialog.getByRole('textbox').fill('batch-');
  await expect(dialog.getByText(/Models: 27|共 27 个模型/)).toBeVisible();
  await dialog.getByTestId('catalog-import-select-all').click();
  await expect(dialog.getByTestId('catalog-import-apply')).toContainText('27');
  await dialog.getByTestId('catalog-import-expand-batch-0').click();
  await page.setViewportSize({ width: 390, height: 844 });
  const priceRow = dialog.getByTestId('catalog-price-row-cacheWrite');
  await priceRow.scrollIntoViewIfNeeded();
  const cache = priceRow.getByTestId('catalog-price-action-cacheWrite');
  const labelBox = await priceRow.getByRole('rowheader').boundingBox();
  const controlBox = await cache.boundingBox();
  const listBox = await dialog.getByTestId('catalog-import-rows').boundingBox();
  expect(labelBox!.x).toBeGreaterThanOrEqual(listBox!.x);
  expect(controlBox!.x + controlBox!.width).toBeLessThanOrEqual(listBox!.x + listBox!.width);
  const tier = await cache.locator('option').filter({ hasText: '2.5' }).getAttribute('value');
  expect(tier).toBeTruthy();
  await cache.selectOption(tier!);

  await page.clock.install();
  await page.clock.fastForward(61_000);
  revision = 'second';
  const previousReads = reads;
  await page.evaluate(() => window.dispatchEvent(new Event('offline')));
  await page.evaluate(() => window.dispatchEvent(new Event('online')));
  await expect.poll(() => reads).toBeGreaterThan(previousReads);
  await dialog.getByTestId('catalog-import-apply').click();
  await expect.poll(() => applied.length).toBe(27);
  expect(applied.every((row) => row.prices.find(p => p.field === 'input')?.expected === 1)).toBe(true);
  expect(applied.flatMap((row) => row.prices).filter(price=>price.action === 'TIER')).toEqual([
    expect.objectContaining({ field: 'cacheWrite', tier: 'context 256000', expected: 2.5 }),
  ]);
});
