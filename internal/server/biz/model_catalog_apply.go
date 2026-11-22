// 职责: 将已预览的目录模型和价格选择逐行写入全局模型库。
// 关联文件:
//
//	internal/server/biz/model_catalog_preview.go
//	internal/server/biz/model.go
//	internal/objects/model.go
//
// 注意事项:
//
//	新建模型须在行事务内复核公开请求名；单行冲突不撤销其他行，更新价格保留已有渠道关联。
package biz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"
	"github.com/samber/lo"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/model"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xerrors"
)

var errCatalogConflict = errors.New("model changed since preview")
var errCatalogQuoteChanged = errors.New("catalog offer changed since preview")
var errCatalogSelection = errors.New("invalid catalog selection")

// SQLite cannot upgrade a stale read snapshot to a writer after another connection commits.
func catalogWriteBusy(err error) bool {
	var e *sqlite.Error
	if !errors.As(err, &e) {
		return false
	}
	code := e.Code() & 0xff
	return code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED
}

// ApplyCatalogImport re-derives the prices from the reviewed catalog. The
// client supplies choices and versions, never a replacement price snapshot.
func (svc *ModelService) ApplyCatalogImport(ctx context.Context, scope CatalogImportScope, revision string, selections []CatalogImportSelection) ([]CatalogImportOutcome, error) {
	snapshot, err := svc.catalog.snapshot(ctx, false, true)
	if err != nil {
		return nil, err
	}
	preview, err := svc.catalogPreviewFromSnapshot(ctx, scope, snapshot)
	if err != nil {
		return nil, err
	}
	rows := map[string]*CatalogImportRow{}
	for _, row := range preview.Rows {
		rows[row.ModelID] = row
		for _, id := range row.Members {
			rows[id.ID] = row
		}
	}
	used := map[string]bool{}
	result := make([]CatalogImportOutcome, 0, len(selections))
	for _, selection := range selections {
		status := CatalogApplied
		switch {
		case slices.ContainsFunc(selection.Members, func(m CatalogImportMember) bool { return used[m.ID] }):
			status = CatalogConflict
		case preview.Revision != revision:
			status = CatalogChanged
		default:
			err := svc.RunInTransaction(ctx, func(ctx context.Context) error { return svc.applyCatalogRow(ctx, preview.Source, rows, selection) })
			switch {
			case errors.Is(err, errCatalogQuoteChanged):
				status = CatalogChanged
			case errors.Is(err, errCatalogSelection):
				status = CatalogInvalid
			case errors.Is(err, errCatalogConflict), ent.IsConstraintError(err), ent.IsNotFound(err), catalogWriteBusy(err):
				status = CatalogConflict
			case err != nil:
				return result, fmt.Errorf("apply catalog model %q: %w", selection.ModelID, err)
			}
		}
		if status == CatalogApplied {
			for _, id := range selection.Members {
				used[id.ID] = true
			}
		}
		result = append(result, CatalogImportOutcome{ModelID: selection.ModelID, Status: status})
	}
	return result, nil
}

func (svc *ModelService) applyCatalogRow(ctx context.Context, source string, rows map[string]*CatalogImportRow, sel CatalogImportSelection) error {
	if len(sel.Members) == 0 || strings.TrimSpace(sel.ModelID) == "" {
		return errCatalogSelection
	}
	if sel.Version != "" {
		return svc.updateCatalogPrices(ctx, source, rows[sel.ModelID], sel)
	}
	var parts []*CatalogImportRow
	submitted := map[string]CatalogImportMember{}
	for _, member := range sel.Members {
		if _, duplicate := submitted[member.ID]; duplicate {
			return errCatalogSelection
		}
		if rows[member.ID] == nil {
			return errCatalogConflict
		}
		submitted[member.ID] = member
		parts = append(parts, rows[member.ID])
	}
	parts = lo.Uniq(parts)
	if len(parts) > 1 && !sel.ManualMerge {
		return errCatalogConflict
	}
	choices := map[catalogOfferKey]*CatalogImportOffer{}
	var channelIDs []int
	memberCount := 0
	for _, row := range parts {
		if row.Version != "" || (row.ModelID != sel.ModelID && row.MergeTargetOccupied) {
			return errCatalogConflict
		}
		if row.ModelID != sel.ModelID && (len(parts) < 2 || row.MergeTarget != sel.ModelID) {
			return errCatalogSelection
		}
		memberCount += len(row.Members)
		for _, member := range row.Members {
			original, present := submitted[member.ID]
			if !present || (len(sel.Members) > 1 && original != member) {
				return errCatalogConflict
			}
		}
		for _, o := range row.Offers {
			choices[catalogOfferKey{o.Provider, o.ModelID}] = o
		}
		for _, route := range row.Routes {
			channelIDs = append(channelIDs, route.ChannelID)
		}
	}
	if memberCount != len(sel.Members) || (len(parts) > 1 && rows[sel.ModelID] != nil && !slices.Contains(parts, rows[sel.ModelID])) {
		return errCatalogSelection
	}
	db := svc.entFromContext(ctx)
	if strings.TrimSpace(sel.Name) == "" || strings.TrimSpace(sel.Developer) == "" {
		return errCatalogSelection
	}
	primary := choices[catalogOfferKey{sel.Provider, sel.SourceModelID}]
	if sel.Provider != "" && (primary == nil || primary.Identity != sel.Identity) {
		return errCatalogQuoteChanged
	}
	card := newImportCard(primary)
	if len(sel.Members) > 1 {
		card.CatalogAliases = sortedStringKeys(submitted)
	}
	if err := applyCatalogPrices(card, choices, sel.Prices); err != nil {
		return err
	}
	settings := &objects.ModelSettings{LoadBalancerStrategy: "default", TraceStickyMode: "default"}
	associations, err := catalogAssociations(ctx, db, sel, channelIDs)
	if err != nil {
		return err
	}
	settings.Associations = associations
	typ := model.Type(sel.Type)
	if model.TypeValidator(typ) != nil {
		return errCatalogSelection
	}
	group := sel.Developer
	if primary != nil {
		if family := modelString(primary.entry, "family"); family != "" {
			group = family
		}
	}
	return db.Model.Create().SetDeveloper(sel.Developer).SetModelID(sel.ModelID).SetName(sel.Name).SetGroup(group).SetIcon(sel.Developer).SetType(typ).SetSettings(settings).SetModelCard(card).Exec(ctx)
}

func (svc *ModelService) updateCatalogPrices(ctx context.Context, source string, row *CatalogImportRow, sel CatalogImportSelection) error {
	db := svc.entFromContext(ctx)
	current, err := lockedModel(ctx, db, db.Model.Query().Where(model.ModelIDEQ(sel.ModelID)))
	if err != nil {
		return err
	}
	version, err := ModelEditVersion(current)
	if err != nil {
		return err
	}
	if version != sel.Version || current.Status == model.StatusArchived || source == catalogSourceFallback {
		return errCatalogConflict
	}
	if len(sel.Prices) == 0 {
		return nil
	}
	offers := map[catalogOfferKey]*CatalogImportOffer{}
	if row != nil && row.ModelID == sel.ModelID {
		for _, o := range row.Offers {
			offers[catalogOfferKey{o.Provider, o.ModelID}] = o
		}
	}
	next := objects.ModelCard{}
	if current.ModelCard != nil {
		next = *current.ModelCard
		next.CatalogPrices = maps.Clone(next.CatalogPrices)
	}
	if err := applyCatalogPrices(&next, offers, sel.Prices); err != nil {
		return err
	}
	if reflect.DeepEqual(current.ModelCard, &next) {
		return nil
	}
	return db.Model.UpdateOneID(current.ID).SetModelCard(&next).Exec(ctx)
}

// Resolve each reviewed (channel, upstream model) against its currently exposed request names.
func catalogAssociations(ctx context.Context, db *ent.Client, sel CatalogImportSelection, channelIDs []int) ([]*objects.ModelAssociation, error) {
	type key struct {
		channel int
		actual  string
	}
	choices := map[key]string{}
	for _, choice := range sel.Routes {
		choices[key{choice.ChannelID, choice.Actual}] = choice.Request
	}
	if len(choices) != len(sel.Routes) {
		return nil, errCatalogConflict
	}
	var current map[string][]CatalogImportRoute
	if len(channelIDs) > 0 {
		query := db.Channel.Query().Where(channel.IDIn(lo.Uniq(channelIDs)...))
		if db.Driver().Dialect() != dialect.SQLite {
			query.Modify(func(s *sql.Selector) { s.ForUpdate() })
		}
		var err error
		current, err = catalogChannelRoutes(ctx, query)
		if err != nil {
			return nil, err
		}
	}
	associations := []*objects.ModelAssociation{}
	for _, member := range sel.Members {
		routes := current[member.ID]
		if len(routes) == 0 {
			associations = append(associations, &objects.ModelAssociation{Type: "model", ModelID: &objects.ModelIDAssociation{ModelID: member.ID}})
		}
		for _, route := range routes {
			k := key{route.ChannelID, route.Actual}
			request, ok := choices[k]
			if !ok {
				return nil, errCatalogConflict
			}
			delete(choices, k)
			if request == "" && len(route.Requests) == 0 {
				continue
			}
			if !slices.Contains(route.Requests, request) {
				return nil, errCatalogConflict
			}
			associations = append(associations, &objects.ModelAssociation{Type: "channel_model", ChannelModel: &objects.ChannelModelAssociation{ChannelID: route.ChannelID, ModelID: request}})
		}
	}
	if len(choices) > 0 {
		return nil, errCatalogConflict
	}
	return associations, nil
}

func applyCatalogPrices(card *objects.ModelCard, offers map[catalogOfferKey]*CatalogImportOffer, actions []CatalogImportPriceInput) error {
	if card.CatalogPrices == nil {
		card.CatalogPrices = map[string]objects.CatalogPrice{}
	}
	seen := map[ReferencePriceField]bool{}
	for _, a := range actions {
		dst := card.Cost.Field(string(a.Field))
		if dst == nil || seen[a.Field] {
			return errCatalogSelection
		}
		seen[a.Field] = true
		if a.Action == CatalogPriceFix {
			b := card.CatalogPrices[string(a.Field)]
			b.Follow = false
			card.CatalogPrices[string(a.Field)] = b
			continue
		}
		offer := offers[catalogOfferKey{a.Provider, a.ModelID}]
		rate := offer.rate(a.Field)
		if rate == nil || offer.Identity != a.Identity {
			return errCatalogQuoteChanged
		}
		value := rate.Base
		switch a.Action {
		case CatalogPriceFollow, CatalogPriceFixBase:
		// Use the reviewed base rate.
		case CatalogPriceTier:
			value = nil
			for _, tier := range rate.Tiers {
				if a.Tier != nil && tier.Label == *a.Tier {
					value = &tier.Value
					break
				}
			}
		default:
			return errCatalogSelection
		}
		if value == nil || a.Expected == nil || *value != *a.Expected {
			return errCatalogQuoteChanged
		}
		*dst = *value
		card.CatalogPrices[string(a.Field)] = objects.CatalogPrice{Provider: a.Provider, ModelID: a.ModelID, Follow: a.Action == CatalogPriceFollow}
	}
	return nil
}

func newImportCard(o *CatalogImportOffer) *objects.ModelCard {
	card := &objects.ModelCard{CatalogPrices: map[string]objects.CatalogPrice{}}
	for _, field := range catalogPriceFields {
		card.CatalogPrices[field] = objects.CatalogPrice{Missing: true}
	}
	if o == nil {
		return card
	}
	// Catalog keys shared with the global card include modalities, limit and reasoning.
	var metadata struct {
		Reasoning        objects.ModelCardReasoning
		Modalities       objects.ModelCardModalities
		Limit            objects.ModelCardLimit
		Vision           *bool
		Temperature      bool
		Knowledge        string
		ToolCall         bool   `json:"tool_call"`
		ReleaseDate      string `json:"release_date"`
		LastUpdated      string `json:"last_updated"`
		ReasoningOptions []struct {
			Type   string
			Values []string
		} `json:"reasoning_options"`
	}
	raw, _ := json.Marshal(o.entry)
	if json.Unmarshal(raw, &metadata) != nil {
		return card
	}
	card.Reasoning, card.Modalities, card.Limit = metadata.Reasoning, metadata.Modalities, metadata.Limit
	card.Vision = slices.Contains(card.Modalities.Input, "image")
	if metadata.Vision != nil {
		card.Vision = *metadata.Vision
	}
	card.Temperature, card.ToolCall, card.Knowledge = metadata.Temperature, metadata.ToolCall, metadata.Knowledge
	card.ReleaseDate, card.LastUpdated = metadata.ReleaseDate, metadata.LastUpdated
	for _, option := range metadata.ReasoningOptions {
		if option.Type == "effort" {
			card.ReasoningEfforts = option.Values
			break
		}
	}
	return card
}

func MissingReferencePrices(card *objects.ModelCard) []string {
	fields := []string{}
	if card != nil {
		for _, field := range catalogPriceFields {
			if card.CatalogPrices[field].Missing {
				fields = append(fields, field)
			}
		}
	}
	return fields
}

type ReferencePriceField string

type ManualPriceEdit struct {
	Field   ReferencePriceField
	Missing bool
}

// Old clients submit the whole card. Preserve the source of each unchanged price.
func preserveCatalogPrices(old, next *objects.ModelCard) {
	if old == nil {
		return
	}
	next.CatalogPrices = maps.Clone(old.CatalogPrices)
	next.CatalogAliases = slices.Clone(old.CatalogAliases)
	for field, binding := range next.CatalogPrices {
		oldValue := old.Cost.Field(field)
		if oldValue != nil && *oldValue != *next.Cost.Field(field) {
			binding.Follow, binding.Missing = false, false
			next.CatalogPrices[field] = binding
		}
	}
}

func applyManualPriceEdits(card *objects.ModelCard, edits []ManualPriceEdit) error {
	seen := map[ReferencePriceField]bool{}
	for _, edit := range edits {
		if card == nil {
			return xerrors.ValidationError("price edits require a model card")
		}
		field := string(edit.Field)
		value := card.Cost.Field(field)
		if value == nil || seen[edit.Field] {
			return xerrors.ValidationError(fmt.Sprintf("invalid or repeated reference price field %q", field))
		}
		seen[edit.Field] = true
		if card.CatalogPrices == nil {
			card.CatalogPrices = map[string]objects.CatalogPrice{}
		}
		if edit.Missing {
			*value = 0
		}
		card.CatalogPrices[field] = objects.CatalogPrice{Missing: edit.Missing}
	}
	return nil
}
