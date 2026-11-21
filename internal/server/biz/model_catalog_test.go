// 职责: 使用隔离目录与数据库验证批量导入、价格所有权及每日差异检查。
// 关联文件:
//
//	internal/server/biz/model_catalog_preview.go
//	internal/server/biz/model_catalog_apply.go
//	internal/server/biz/model_catalog_schedule.go
//
// 注意事项:
//
//	上游夹具可以在预览和应用之间变化，以验证仅跳过受影响的模型。
package biz

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/model"
	"github.com/looplj/axonhub/internal/objects"
)

const testCatalog = `{"providers":{
 "moonshot":{"models":[{"id":"kimi-k3","canonical_model_id":"moonshotai/kimi-k3","cost":{"input":2.9879}}]},
 "moonshot-ai":{"models":[{"id":"kimi-k3","name":"Kimi K3","canonical_model_id":"moonshotai/kimi-k3","cost":{"input":3,"output":15}},{"id":"new-model","name":"Another model"}]},
 "auriko":{"models":[{"id":"qwen-3.6-plus","canonical_model_id":"alibaba/qwen-3.6-plus","cost":{"input":0.5,"output":3,"cache_read":0.1,"tiers":[{"tier":{"type":"context","size":256000},"cache_write":2.5}],"context_over_200k":{"cache_write":2.5}}}]},
 "relay":{"models":[{"id":"a/shared","canonical_model_id":"dev/shared","version":"1"},{"id":"b/shared","canonical_model_id":"dev/shared","version":"1"},{"id":"a/variant","canonical_model_id":"dev/variant","version":"1"},{"id":"b/variant","canonical_model_id":"dev/variant","version":"2"}]}
}}`

type importFixture struct {
	ctx      context.Context
	svc      *ModelService
	db       *ent.Client
	body     atomic.Value
	requests atomic.Int64
}

func catalogFixture(t *testing.T) *importFixture {
	t.Helper()
	ctx, sys, catalog := newCatalogTestService(t)
	f := &importFixture{ctx: ctx, db: ent.FromContext(ctx)}
	f.svc = NewModelService(ModelServiceParams{Ent: f.db, Catalog: catalog, SystemService: sys})
	f.body.Store(testCatalog)
	useCatalogUpstream(t, ctx, sys, catalog, func(w http.ResponseWriter, _ *http.Request) {
		f.requests.Add(1)
		_, _ = io.WriteString(w, f.body.Load().(string))
	})
	return f
}

func (f *importFixture) changeCatalog(body string) { f.body.Store(body); f.svc.catalog.Invalidate() }

func (f *importFixture) preview(t *testing.T, scope CatalogImportScope) *CatalogImportPreview {
	t.Helper()
	preview, err := f.svc.CatalogImportPreview(f.ctx, scope)
	require.NoError(t, err)
	return preview
}

func (f *importFixture) channel(t *testing.T, status channel.Status, ids ...string) {
	t.Helper()
	require.NoError(t, f.db.Channel.Create().SetType(channel.TypeOpenai).SetName(string(status)).SetStatus(status).SetCredentials(objects.ChannelCredentials{}).SetSupportedModels(ids).SetDefaultTestModel(ids[0]).Exec(f.ctx))
}

func (f *importFixture) apply(t *testing.T, scope CatalogImportScope, preview *CatalogImportPreview, selections ...CatalogImportSelection) []string {
	t.Helper()
	results, err := f.svc.ApplyCatalogImport(f.ctx, scope, preview.Revision, selections)
	require.NoError(t, err)
	return lo.Map(results, func(result CatalogImportOutcome, _ int) string { return string(result.Status) })
}

func selection(t *testing.T, preview *CatalogImportPreview, id string) CatalogImportSelection {
	t.Helper()
	row, found := lo.Find(preview.Rows, func(row *CatalogImportRow) bool { return row.ModelID == id })
	require.True(t, found, id)
	sel := CatalogImportSelection{ModelID: id, Name: row.Name, Developer: row.Developer, Type: "chat", Version: row.Version, Aliases: row.Aliases, QuoteVersions: []string{row.QuoteVersion}}
	if o := row.Preferred; o != nil {
		sel.Provider, sel.SourceModelID = o.Provider, o.ModelID
		for _, rate := range o.Rates {
			if rate.Base != nil {
				sel.Prices = append(sel.Prices, CatalogImportPriceInput{Field: rate.Field, Action: CatalogPriceFollow, Provider: o.Provider, ModelID: o.ModelID})
			}
		}
	}
	return sel
}

func TestCatalogImportScopesAndConflicts(t *testing.T) {
	f := catalogFixture(t)
	f.channel(t, channel.StatusEnabled, "kimi-k3", "a/shared", "b/shared", "a/variant")
	f.channel(t, channel.StatusDisabled, "kimi-k3", "b/variant", "unknown")
	f.channel(t, channel.StatusArchived, "archived-only")
	scope := CatalogImportScope{Channels: true}
	exact := f.preview(t, scope)
	require.Len(t, exact.Rows, 6)
	scope.IgnorePrefix = true
	preview := f.preview(t, scope)
	require.Len(t, preview.Rows, 5)
	combined := f.preview(t, CatalogImportScope{Channels: true, Providers: []string{"relay"}, IgnorePrefix: true, Tracked: true})
	require.Len(t, combined.Rows, 5)
	require.Equal(t, []string{"a/shared", "b/shared"}, selection(t, combined, "shared").Aliases)
	kimi := selection(t, preview, "kimi-k3")
	require.Equal(t, "moonshot-ai", kimi.Provider)
	shared := selection(t, preview, "shared")
	require.Equal(t, []string{"a/shared", "b/shared"}, shared.Aliases)
	shared.Developer = "dev"
	a, b := selection(t, preview, "a/variant"), selection(t, preview, "b/variant")
	a.ModelID = "variant"
	a.Aliases = append(a.Aliases, b.Aliases...)
	a.QuoteVersions = append(a.QuoteVersions, b.QuoteVersions...)
	slices.Sort(a.QuoteVersions)
	unknown := selection(t, preview, "unknown")
	unknown.Developer = "custom"
	unknown.Name = kimi.Name
	require.Equal(t, []string{"applied", "conflict", "applied", "applied"}, f.apply(t, scope, preview, kimi, unknown, shared, a))
	require.Equal(t, []string{"conflict"}, f.apply(t, scope, preview, kimi))
	m := f.db.Model.Query().Where(model.ModelIDEQ("shared")).OnlyX(f.ctx)
	require.Equal(t, model.StatusDisabled, m.Status)
	require.Len(t, m.Settings.Associations, 2)
	require.Equal(t, "b/shared", m.Settings.Associations[1].ModelID.ModelID)
	after := f.preview(t, scope)
	require.Len(t, after.Rows, 4)
	f.changeCatalog(strings.Replace(testCatalog, `"input":3`, `"input":4`, 1))
	kimi = selection(t, after, "kimi-k3")
	unknown = selection(t, after, "unknown")
	unknown.Developer = "custom"
	require.Equal(t, []string{"catalogChanged", "applied"}, f.apply(t, scope, after, kimi, unknown))
	fresh := f.preview(t, scope)
	m = f.db.Model.Query().Where(model.ModelIDEQ("kimi-k3")).OnlyX(f.ctx)
	f.db.Model.UpdateOneID(m.ID).SetRemark("edited since preview").ExecX(f.ctx)
	require.Equal(t, []string{"conflict"}, f.apply(t, scope, fresh, selection(t, fresh, "kimi-k3")))
}

func TestCatalogImportTierAndManualEdits(t *testing.T) {
	f := catalogFixture(t)
	scope := CatalogImportScope{Providers: []string{"auriko"}}
	preview := f.preview(t, scope)
	sel := selection(t, preview, "qwen-3.6-plus")
	require.Len(t, sel.Prices, 3)
	rate := preview.Rows[0].Preferred.rate("cacheWrite")
	require.Nil(t, rate.Base)
	require.Equal(t, []CatalogImportTier{{"context 256000", 2.5}}, rate.Tiers)
	require.Equal(t, []string{"applied"}, f.apply(t, scope, preview, sel))
	m := f.db.Model.Query().OnlyX(f.ctx)
	require.Equal(t, []string{"cacheWrite"}, MissingReferencePrices(m.ModelCard))
	preview = f.preview(t, scope)
	sel = selection(t, preview, m.ModelID)
	sel.Prices = []CatalogImportPriceInput{{Field: "cacheWrite", Action: CatalogPriceTier, Provider: "auriko", ModelID: m.ModelID, Tier: lo.ToPtr("context 256000")}}
	require.Equal(t, []string{"applied"}, f.apply(t, scope, preview, sel))
	m = f.db.Model.GetX(f.ctx, m.ID)
	require.Equal(t, 2.5, m.ModelCard.Cost.CacheWrite)
	require.False(t, m.ModelCard.CatalogPrices["cacheWrite"].Follow)
	next := *m.ModelCard
	next.CatalogPrices = nil
	m, err := f.svc.UpdateModel(f.ctx, m.ID, &ent.UpdateModelInput{Name: lo.ToPtr("Renamed"), ModelCard: &next})
	require.NoError(t, err)
	require.True(t, m.ModelCard.CatalogPrices["input"].Follow)
	next = *m.ModelCard
	next.Cost.Input = 0
	next.Cost.CacheRead = 0
	m, err = f.svc.UpdateModelWithPriceEdits(f.ctx, m.ID, &ent.UpdateModelInput{ModelCard: &next}, []ManualPriceEdit{{Field: "input"}, {Field: "cacheRead", Missing: true}}, &m.UpdatedAt)
	require.NoError(t, err)
	require.Equal(t, 0.0, *m.ModelCard.ReferencePrice("input"))
	require.Nil(t, m.ModelCard.ReferencePrice("cacheRead"))
	require.False(t, m.ModelCard.CatalogPrices["input"].Follow)
	require.True(t, m.ModelCard.CatalogPrices["output"].Follow)
	f.changeCatalog(strings.ReplaceAll(testCatalog, "2.5", "4.5"))
	require.False(t, f.preview(t, scope).Rows[0].Changed)
}

func TestCatalogCheckOnlyReportsFollowedFieldsAndSubscribedProviders(t *testing.T) {
	f := catalogFixture(t)
	scope := CatalogImportScope{Providers: []string{"moonshot-ai"}}
	preview := f.preview(t, scope)
	sel := selection(t, preview, "kimi-k3")
	sel.Prices[1].Action = CatalogPriceFixBase
	require.Equal(t, []string{"applied"}, f.apply(t, scope, preview, sel))
	f.changeCatalog(strings.Replace(testCatalog, `"input":3,"output":15`, `"input":4,"output":20`, 1))
	state, err := f.svc.CheckModelCatalog(f.ctx)
	require.NoError(t, err)
	require.False(t, state.Settings.Enabled)
	require.Zero(t, state.Available)
	require.Equal(t, 1, state.Changed)
	cfg := CatalogCheckSettings{Enabled: true, Time: "08:30", Timezone: "Asia/Shanghai", Providers: scope.Providers}
	previousSuccess := state.LastSuccess
	state, err = f.svc.SaveCatalogCheckSettings(f.ctx, cfg)
	require.NoError(t, err)
	require.NotNil(t, state.NextCheck)
	require.Equal(t, previousSuccess, state.LastSuccess)
	now := time.Date(2026, 11, 22, 0, 30, 0, 0, time.UTC)
	state, err = f.svc.checkModelCatalog(f.ctx, now, true)
	require.NoError(t, err)
	require.Equal(t, 1, state.Available)
	require.Equal(t, 1, state.Changed)
	m := f.db.Model.Query().OnlyX(f.ctx)
	require.Equal(t, 3.0, m.ModelCard.Cost.Input)
	require.Equal(t, 15.0, m.ModelCard.Cost.Output)
	f.changeCatalog(`{"providers":{}}`)
	again, err := f.svc.checkModelCatalog(f.ctx, now.Add(time.Second), true)
	require.NoError(t, err)
	require.Equal(t, state.LastSuccess, again.LastSuccess)
	again, err = f.svc.CheckModelCatalog(f.ctx)
	require.NoError(t, err)
	require.Zero(t, again.Changed)
	f.changeCatalog("offline")
	failed, err := f.svc.CheckModelCatalog(f.ctx)
	require.NoError(t, err)
	require.True(t, failed.Failed)
	require.Equal(t, again.LastSuccess, failed.LastSuccess)
	fallback := f.preview(t, CatalogImportScope{Providers: []string{"moonshot"}})
	require.Equal(t, catalogSourceFallback, fallback.Source)
	selected := selection(t, fallback, m.ModelID)
	require.Equal(t, []string{"conflict"}, f.apply(t, CatalogImportScope{Providers: []string{"moonshot"}}, fallback, selected))
	other, ok := lo.Find(fallback.Rows, func(r *CatalogImportRow) bool { return r.Version == "" && r.Preferred != nil })
	require.True(t, ok)
	selected = selection(t, fallback, other.ModelID)
	selected.Developer = "moonshot"
	require.Equal(t, []string{"applied"}, f.apply(t, CatalogImportScope{Providers: []string{"moonshot"}}, fallback, selected))
}

func TestCatalogModelTypes(t *testing.T) {
	for raw, want := range map[string]string{"image": "image_generation", "video": "video_generation", "reranking": "rerank", "image-generation": "image_generation", "embedding": "embedding", "audio": "audio"} {
		require.Equal(t, want, catalogModelType(raw))
	}
}

func TestCatalogImportProtectsLegacyPricesAndWriteOnlyEditor(t *testing.T) {
	f := catalogFixture(t)
	m := f.db.Model.Create().SetModelID("kimi-k3").SetName("Existing").SetGroup("custom").SetDeveloper("custom").SetIcon("custom").SetStatus(model.StatusEnabled).
		SetSettings(&objects.ModelSettings{}).SetModelCard(&objects.ModelCard{Cost: objects.ModelCardCost{Input: 0, Output: 9}}).SaveX(f.ctx)
	scope := CatalogImportScope{Providers: []string{"moonshot-ai"}}
	preview := f.preview(t, scope)
	row, ok := lo.Find(preview.Rows, func(r *CatalogImportRow) bool { return r.ModelID == m.ModelID })
	require.True(t, ok)
	require.False(t, row.Changed)
	require.False(t, row.Prices[0].Follow)
	require.Equal(t, 0.0, *row.Prices[0].Value)
	sel := selection(t, preview, m.ModelID)
	sel.Prices = []CatalogImportPriceInput{{Field: "output", Provider: "moonshot-ai", ModelID: m.ModelID, Action: CatalogPriceFollow}}
	require.Equal(t, []string{"applied"}, f.apply(t, scope, preview, sel))
	m = f.db.Model.GetX(f.ctx, m.ID)
	require.Equal(t, "Existing", m.Name)
	require.Equal(t, model.StatusEnabled, m.Status)
	require.Zero(t, m.ModelCard.Cost.Input)
	require.Equal(t, 15.0, m.ModelCard.Cost.Output)
	// A write-only role may submit a whole model card when editing the name.
	ctx := contexts.WithUser(ent.NewContext(context.Background(), f.db), &ent.User{ID: 1, Scopes: []string{"write_channels"}})
	_, readErr := f.db.Model.Query().Where(model.IDEQ(m.ID)).OnlyID(ctx)
	require.Error(t, readErr)
	card := *m.ModelCard
	card.CatalogPrices = nil
	m, err := f.svc.UpdateModel(ctx, m.ID, &ent.UpdateModelInput{ModelCard: &card, Name: lo.ToPtr("Renamed")})
	require.NoError(t, err)
	require.True(t, m.ModelCard.CatalogPrices["output"].Follow)
}

func TestCatalogImportNestedChannelIDs(t *testing.T) {
	f := catalogFixture(t)
	f.channel(t, channel.StatusEnabled, "a/vendor/x", "b/vendor/x", "a/vendor/only")
	scope := CatalogImportScope{Channels: true, IgnorePrefix: true}
	preview := f.preview(t, scope)
	require.Len(t, preview.Rows, 3)
	a, b := selection(t, preview, "a/vendor/x"), selection(t, preview, "b/vendor/x")
	a.ModelID, a.Developer = "x", "dev"
	a.Aliases = append(a.Aliases, b.Aliases...)
	a.QuoteVersions = append(a.QuoteVersions, b.QuoteVersions...)
	slices.Sort(a.QuoteVersions)
	require.Equal(t, []string{"applied"}, f.apply(t, scope, preview, a))
	stored := f.db.Model.Query().Where(model.ModelIDEQ("x")).OnlyX(f.ctx)
	require.ElementsMatch(t, []string{"a/vendor/x", "b/vendor/x"}, stored.ModelCard.CatalogAliases)
	require.Len(t, stored.Settings.Associations, 2)
	for i, id := range a.Aliases {
		require.Equal(t, id, stored.Settings.Associations[i].ModelID.ModelID)
	}
	selection(t, preview, "a/vendor/only")
}

func TestCatalogImportRecognizesStoredAliasesAcrossScopes(t *testing.T) {
	f := catalogFixture(t)
	f.channel(t, channel.StatusEnabled, "a/shared", "b/shared")
	scope := CatalogImportScope{Channels: true, IgnorePrefix: true}
	preview := f.preview(t, scope)
	group := selection(t, preview, "shared")
	group.Developer = "dev"
	require.Equal(t, []string{"applied"}, f.apply(t, scope, preview, group))
	for _, scope := range []CatalogImportScope{{Providers: []string{"relay"}}, {Channels: true, Tracked: true}} {
		for _, row := range f.preview(t, scope).Rows {
			require.NotContains(t, []string{"a/shared", "b/shared"}, row.ModelID)
		}
	}
}

func TestCatalogImportPreservesUnrelatedExistingMergeTarget(t *testing.T) {
	f := catalogFixture(t)
	f.channel(t, channel.StatusEnabled, "a/shared", "b/shared")
	f.db.Model.Create().SetModelID("shared").SetName("Different model").SetGroup("other").SetDeveloper("other").SetIcon("other").SetSettings(&objects.ModelSettings{}).SetModelCard(&objects.ModelCard{}).SaveX(f.ctx)
	p := f.preview(t, CatalogImportScope{Channels: true, Tracked: true, IgnorePrefix: true})
	require.Len(t, p.Rows, 2)
	for _, row := range p.Rows {
		require.True(t, row.MergeTargetOccupied)
		require.Equal(t, "shared", row.MergeTarget)
	}
	for _, id := range []string{"a/shared", "b/shared"} {
		require.Empty(t, selection(t, p, id).Version)
	}
}

func TestCatalogCheckFetchesAfterPreview(t *testing.T) {
	f := catalogFixture(t)
	f.preview(t, CatalogImportScope{Channels: true})
	reads := f.requests.Load()
	f.body.Store("offline")
	state, err := f.svc.CheckModelCatalog(f.ctx)
	require.NoError(t, err)
	require.True(t, state.Failed)
	require.Equal(t, reads+1, f.requests.Load())
	require.Nil(t, state.LastSuccess)
	f.body.Store(testCatalog)
	state, err = f.svc.CheckModelCatalog(f.ctx)
	require.NoError(t, err)
	require.False(t, state.Failed)
	require.Equal(t, reads+2, f.requests.Load())
	require.NotNil(t, state.LastSuccess)
}

func TestCatalogAutomaticallyMergesVerifiedNestedIDs(t *testing.T) {
	f := catalogFixture(t)
	f.changeCatalog(strings.NewReplacer("a/shared", "a/vendor/shared", "b/shared", "b/vendor/shared").Replace(testCatalog))
	f.channel(t, channel.StatusEnabled, "a/vendor/shared", "b/vendor/shared")
	scope := CatalogImportScope{Channels: true, IgnorePrefix: true}
	preview := f.preview(t, scope)
	require.Len(t, preview.Rows, 1)
	group := selection(t, preview, "shared")
	require.Equal(t, []string{"a/vendor/shared", "b/vendor/shared"}, group.Aliases)
	group.Developer = "dev"
	require.Equal(t, []string{"applied"}, f.apply(t, scope, preview, group))
}
