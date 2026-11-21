// 职责: 展示模型各项报价、阶梯条件和已有价格来源，并构造明确的导入选择。
// 关联文件:
//   frontend/src/features/models/components/models-catalog-import.tsx
//   frontend/src/features/models/data/catalog-import.ts
//   frontend/src/locales/en.json
//   frontend/src/locales/zh.json
// 注意事项:
//   未绑定来源的历史价格默认保留，选择某档报价会保存为固定单价。
import { useTranslation } from 'react-i18next';
import { Input } from '@/components/ui/input';
import {
  priceFields,
  type ImportRow,
  type Offer,
  type PriceSelection,
  type ImportSelection,
  type PriceField,
} from '../data/catalog-import';
import { modelTypeSchema } from '../data/schema';

export const selectStyle = 'h-9 max-w-full rounded-md border bg-background px-2 text-sm';
export const offerKey = (offer: { provider: string; modelID: string }) => JSON.stringify([offer.provider, offer.modelID]);
export function useReferencePriceFormat() {
  const { t, i18n } = useTranslation();
  return (value: number) =>
    t('currencies.format', { val: value, currency: 'USD', locale: i18n.language, minimumFractionDigits: 0, maximumFractionDigits: 12 });
}

export function defaultSelection(row: ImportRow, offer = row.preferred, replaceSource = false): ImportSelection {
  const prices: PriceSelection[] = [];
  for (const field of priceFields) {
    const local = row.prices.find((p) => p.field === field);
    if (row.version && !local?.follow) continue;
    const source = local && !replaceSource ? row.offers.find((o) => o.provider === local.provider && o.modelID === local.modelID) : offer;
    const rate = source?.rates.find((r) => r.field === field);
    if (!source || rate?.base == null) continue;
    if (local && local.value === rate.base && local.provider === source.provider && local.modelID === source.modelID) continue;
    prices.push({ field, action: 'FOLLOW', provider: source.provider, modelID: source.modelID });
  }
  return {
    type: offer?.type ? (modelTypeSchema.safeParse(offer.type).data ?? '') : 'chat',
    quoteVersions: row.mergedVersions ?? [row.quoteVersion],
    modelID: row.modelID,
    aliases: row.aliases,
    version: row.version,
    name: row.version ? row.name : offer?.name || row.modelID,
    developer: row.version ? row.developer : offer?.developer || '',
    provider: offer?.provider || '',
    sourceModelID: offer?.modelID || '',
    prices,
  };
}

export function PriceEditor({
  row,
  draft,
  onChange,
}: {
  row: ImportRow;
  draft: ImportSelection;
  onChange: (value: ImportSelection) => void;
}) {
  const { t } = useTranslation();
  const offer = row.offers.find((o) => o.provider === draft.provider && o.modelID === draft.sourceModelID);
  const formatPrice = useReferencePriceFormat();
  const updateField = (field: PriceField, value: string) => {
    const prices = draft.prices.filter((p) => p.field !== field);
    if (value) prices.push({ field, ...JSON.parse(value) });
    onChange({ ...draft, prices });
  };
  const optionValue = (action: PriceSelection['action'], source?: { provider: string; modelID: string }, tier?: string) =>
    JSON.stringify({ action, provider: source?.provider ?? '', modelID: source?.modelID ?? '', tier });
  function selectOffer(next: Offer | null) {
    const defaults = defaultSelection(row, next, true);
    const previous = defaultSelection(row, offer);
    for (const field of ['name', 'developer', 'type'] as const) {
      if (draft[field] !== previous[field]) defaults[field] = draft[field];
    }
    onChange(defaults);
  }
  return (
    <div className='space-y-3 rounded-md border p-3'>
      {!row.version && (
        <div className='grid gap-2 sm:grid-cols-3'>
          <label className='text-sm'>
            {t('models.fields.name')}
            <Input value={draft.name} onChange={(e) => onChange({ ...draft, name: e.target.value })} />
          </label>
          <label className='text-sm'>
            {t('models.fields.developer')}
            <Input value={draft.developer} onChange={(e) => onChange({ ...draft, developer: e.target.value })} />
          </label>
          <label className='grid text-sm'>
            {t('models.fields.type')}
            <select className={selectStyle} value={draft.type} onChange={(e) => onChange({ ...draft, type: e.target.value })}>
              {!draft.type && <option value=''>{t('models.import.pickType')}</option>}
              {modelTypeSchema.options.map((type) => (
                <option key={type} value={type}>
                  {t('models.types.' + type)}
                </option>
              ))}
            </select>
          </label>
        </div>
      )}
      {row.aliases.length > 1 && (
        <p className='text-xs break-words'>
          {t('models.import.aliases')}: {row.aliases.join(', ')}
        </p>
      )}
      <label className='flex flex-wrap items-center gap-2 text-sm'>
        {t('models.import.priceSource')}
        <select
          className={selectStyle}
          value={offer ? offerKey(offer) : ''}
          onChange={(e) => selectOffer(row.offers.find((o) => offerKey(o) === e.target.value) ?? null)}
        >
          <option value=''>{t('models.import.noSource')}</option>
          {row.offers.map((o) => (
            <option key={offerKey(o)} value={offerKey(o)}>
              {o.provider} · {o.modelID}
            </option>
          ))}
        </select>
      </label>
      <p className='text-muted-foreground text-xs'>{t('models.import.unit')}</p>
      <div className='overflow-x-auto'>
        <table className='w-full text-left text-sm'>
          <thead>
            <tr className='text-muted-foreground'>
              <th>{t('models.modelCard.cost')}</th>
              <th>{t('models.import.currentPrice')}</th>
              <th>{t('models.import.referencePrice')}</th>
              <th>{t('models.import.onApply')}</th>
            </tr>
          </thead>
          <tbody>
            {priceFields.map((field) => {
              const local = row.prices.find((r) => r.field === field);
              const action = draft.prices.find((r) => r.field === field);
              const actionValue = action ? optionValue(action.action, action.action === 'FIX' ? undefined : action, action.tier) : '';
              const source = row.offers.find((o) => o.provider === action?.provider && o.modelID === action?.modelID) ?? offer;
              const rate = source?.rates.find((r) => r.field === field);
              return (
                <tr key={field} className='border-t'>
                  <td className='py-2 pr-2'>{t('models.modelCard.' + field)}</td>
                  <td className='pr-2 tabular-nums'>
                    {local?.value == null ? '—' : formatPrice(local.value)}
                    {local && (
                      <div className='text-muted-foreground text-xs'>
                        {local.follow
                          ? t('models.import.following', { provider: local.provider, model: local.modelID })
                          : t('models.import.manual')}
                      </div>
                    )}
                  </td>
                  <td className='pr-2 tabular-nums'>
                    {rate?.base == null ? '—' : formatPrice(rate.base)}
                    {rate?.tiers.length ? (
                      <details>
                        <summary className='cursor-pointer text-xs'>{t('models.import.tiers')}</summary>
                        {rate.tiers.map((tier, i) => (
                          <div key={i} className='text-xs'>
                            {tier.label} · {formatPrice(tier.value)}
                          </div>
                        ))}
                      </details>
                    ) : null}
                  </td>
                  <td>
                    <select
                      className={selectStyle}
                      aria-label={t('models.modelCard.' + field)}
                      value={actionValue}
                      onChange={(e) => updateField(field, e.target.value)}
                    >
                      <option value=''>{t('models.import.keep')}</option>
                      {local?.follow && <option value={optionValue('FIX')}>{t('models.import.fixCurrent')}</option>}
                      {row.offers.map((o) => {
                        const quote = o.rates.find((r) => r.field === field);
                        if (!quote || (quote.base == null && !quote.tiers.length)) return null;
                        return (
                          <optgroup key={offerKey(o)} label={o.provider + ' · ' + o.modelID}>
                            {quote.base != null && (
                              <>
                                <option value={optionValue('FOLLOW', o)}>
                                  {t('models.import.follow')} · {formatPrice(quote.base)}
                                </option>
                                <option value={optionValue('FIX_BASE', o)}>
                                  {t('models.import.fixBase')} · {formatPrice(quote.base)}
                                </option>
                              </>
                            )}
                            {quote.tiers.map((tier) => (
                              <option key={tier.label} value={optionValue('TIER', o, tier.label)}>
                                {t('models.import.fixTier', { tier: tier.label, price: formatPrice(tier.value) })}
                              </option>
                            ))}
                          </optgroup>
                        );
                      })}
                    </select>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
      {row.offers.some((o) => o.rates.some((rate) => rate.tiers.length > 0)) && (
        <p className='text-muted-foreground text-xs'>{t('models.import.flat')}</p>
      )}
    </div>
  );
}
