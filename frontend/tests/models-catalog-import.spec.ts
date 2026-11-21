// 职责: 验证全选跨页、默认排除阶梯价和已勾选报价在后台刷新时的保护。
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
  type Selection = { quoteVersions: string[]; prices: { field: string; action: string; tier?: string }[] };
  let applied: Selection[] = [];
  await page.route('**/admin/graphql', async route => {
    const body = route.request().postDataJSON();
    if (body.operationName === 'OnboardingInfo') {
      return route.fulfill({json:{data:{onboardingInfo:{onboarded:true,systemModelSetting:{onboarded:true},autoDisableChannel:{onboarded:true}}}}});
    }
    if (body.operationName === 'ApplyCatalogImport') {
      applied = body.variables.selections;
      return route.fulfill({json:{data:{applyCatalogImport:body.variables.selections.map((row:{modelID:string}) => ({modelID:row.modelID,status:'catalogChanged'}))}}});
    }
    if (body.operationName !== 'CatalogImportPreview') return route.fallback();
    reads++;
    const rows = Array.from({length:28}, (_, i) => {
      const modelID = i < 27 ? 'batch-'+i : 'outside';
      const offer = {provider,modelID,name:modelID,developer:provider,canonical:'',type:'chat',rates:[
        {field:'input',base:revision === 'first'?1:2,tiers:[]},
        {field:'cacheWrite',base:null,tiers:[{label:'context 256000',value:2.5}]},
      ]};
      return {modelID,name:modelID,developer:provider,aliases:[modelID],version:'',quoteVersion:revision,changed:false,archived:false,offers:[offer],preferred:offer,prices:[]};
    });
    return route.fulfill({json:{data:{catalogImportPreview:{providers:[provider],source:'upstream',upstreamURL:'https://example.test/catalog',revision:'same-source',rows}}}});
  });
  await gotoAndEnsureAuth(page,'/models');
  await page.getByTestId('catalog-import-open').click();
  const dialog = page.getByRole('dialog');
  await dialog.getByRole('textbox').fill('batch-');
  await expect(dialog.getByText(/Models: 27|共 27 个模型/)).toBeVisible();
  await dialog.getByRole('button',{name:/Select all applicable|全选筛选/}).click();
  await expect(dialog.getByTestId('catalog-import-apply')).toContainText('27');
  await dialog.getByRole('button',{name:/^batch-0 /}).click();
  const cache = dialog.getByRole('combobox',{name:/Cache Write|缓存写入/});
  const tier = await cache.locator('option').filter({hasText:'2.5'}).getAttribute('value');
  expect(tier).toBeTruthy();
  await cache.selectOption(tier!);

  await page.clock.install();
  await page.clock.fastForward(61_000);
  revision = 'second';
  const previousReads = reads;
  await page.evaluate(()=>window.dispatchEvent(new Event('offline')));
  await page.evaluate(()=>window.dispatchEvent(new Event('online')));
  await expect.poll(()=>reads).toBeGreaterThan(previousReads);
  await dialog.getByTestId('catalog-import-apply').click();
  await expect.poll(()=>applied.length).toBe(27);
  expect(applied.every(row=>row.quoteVersions[0]==='first')).toBe(true);
  expect(applied.flatMap(row=>row.prices).filter(price=>price.action==='TIER')).toEqual([
    expect.objectContaining({field:'cacheWrite',tier:'context 256000'}),
  ]);
});
