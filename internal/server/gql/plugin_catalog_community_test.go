// 职责: 在同一隔离站点核对社区插件摘要和原生复核预览。
// 关联文件:
//
//	plugins/community/model-catalog-check/plugin.js
//	internal/server/gql/plugin_endpoint_test.go
//	internal/server/biz/model_catalog_preview.go

package gql

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/plugin"
)

func TestCommunityCatalogCheck(t *testing.T) {
	const id = "local.model-catalog-check"
	const catalog = `{"providers":{
      "moonshot":{"models":[{"id":"A","canonical_model_id":"moonshot/A","cost":{"input":0,"output":15}},{"id":"B"}]},
      "relay":{"models":[{"id":"one/nested/family","canonical_model_id":"dev/family"},{"id":"two/family","canonical_model_id":"dev/family"}]}
    }}`
	fx := newPluginEndpointFixture(t, "community_catalog_check", &http.Client{Transport: pluginRoundTrip(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(catalog))}, nil
	})})
	ctx := fx.setupCtx
	fx.db.User.UpdateOneID(fx.readerID).SetScopes([]string{}).ExecX(ctx)
	fx.db.Channel.UpdateOneID(fx.channelID).SetSupportedModels([]string{"A", "unmatched", "one/nested/family", "two/family"}).SaveX(ctx)
	card := &objects.ModelCard{Cost: objects.ModelCardCost{Input: 3, Output: 9}, CatalogAliases: []string{"A"}, CatalogPrices: map[string]objects.CatalogPrice{
		"input": {Follow: true, Provider: "moonshot", ModelID: "A", Canonical: "moonshot/A"},
	}}
	row := fx.db.Model.Create().SetName("Local").SetModelID("local-A").SetDeveloper("moonshot").SetIcon("").SetGroup("").SetSettings(&objects.ModelSettings{}).SetModelCard(card).SaveX(ctx)
	pack, _, err := plugin.PackDirectory(filepath.Join("..", "..", "..", "plugins", "community", "model-catalog-check"))
	require.NoError(t, err)
	installed, err := fx.manager.Install(ctx, pack, nil, nil)
	require.NoError(t, err)
	require.NoError(t, fx.manager.Enable(ctx, installed.Plugin.ID, []string{"read_channels"}, nil))

	for _, tc := range []struct {
		channels, ignorePrefix bool
		providers, newIDs      []string
	}{
		{false, false, []string{}, nil},
		{false, false, []string{"moonshot"}, []string{"B"}},
		{true, true, []string{}, []string{"family", "unmatched"}},
	} {
		require.NoError(t, fx.manager.UpdateSettings(ctx, installed.Plugin.ID, 0, map[string]any{"providers": tc.providers, "channels": tc.channels, "ignorePrefix": tc.ignorePrefix}))
		_, payload, raw := fx.postAdmin(fx.ownerToken, fmt.Sprintf(`mutation {runPluginTask(id:"gid://axonhub/Plugin/%d",key:"check"){status}}`, installed.Plugin.ID))
		require.NotContains(t, payload, "errors", raw)
		require.Contains(t, raw, "succeeded", raw)
		_, payload, raw = fx.postPlugin(fx.readerToken, id, `{pluginKV(key:"summary"){value}}`)
		require.NotContains(t, payload, "errors", raw)
		summary := payload["data"].(map[string]any)["pluginKV"].(map[string]any)["value"].(string)
		var got struct {
			Scope struct {
				Channels, IgnorePrefix bool
				Providers              []string
			}
			FetchedAt          string
			Available, Changed int
		}
		require.NoError(t, json.Unmarshal([]byte(summary), &got))
		require.Equal(t, tc.channels, got.Scope.Channels)
		require.Equal(t, tc.ignorePrefix, got.Scope.IgnorePrefix)
		require.Equal(t, tc.providers, got.Scope.Providers)
		require.NotEmpty(t, got.FetchedAt)
		require.Equal(t, len(tc.newIDs), got.Available)
		require.Equal(t, 1, got.Changed)
		require.NotContains(t, summary, "local-A")
		const previewQuery = `query Preview($scope:CatalogImportScope!){catalogImportPreview(scope:$scope){rows{modelID changed}}}`
		scope := map[string]any{"channels": tc.channels, "providers": tc.providers, "ignorePrefix": tc.ignorePrefix, "review": true}
		_, payload, raw = fx.post("/admin/graphql", fx.ownerToken, previewQuery, map[string]any{"scope": scope})
		require.NotContains(t, payload, "errors", raw)
		rows := payload["data"].(map[string]any)["catalogImportPreview"].(map[string]any)["rows"].([]any)
		require.Len(t, rows, 1+got.Available)
		require.Contains(t, rows, map[string]any{"modelID": "local-A", "changed": true})
		for _, id := range tc.newIDs {
			require.Contains(t, rows, map[string]any{"modelID": id, "changed": false})
		}
		_, payload, _ = fx.post("/admin/graphql", fx.readerToken, previewQuery, map[string]any{"scope": scope})
		require.Equal(t, "FORBIDDEN", graphqlErrorCode(t, payload))
	}
	_, payload, _ := fx.postAdmin(fx.readerToken, `mutation {applyCatalogImport(scope:{channels:false,providers:[],ignorePrefix:false,review:true},revision:"",selections:[]){status}}`)
	require.Equal(t, "FORBIDDEN", graphqlErrorCode(t, payload))
	require.Equal(t, card, fx.db.Model.GetX(ctx, row.ID).ModelCard)
	require.Equal(t, 1, fx.db.Model.Query().CountX(ctx))
}
