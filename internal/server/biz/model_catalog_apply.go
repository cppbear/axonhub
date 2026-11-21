// 职责: 将已预览的目录模型和价格选择逐行写入全局模型库。
// 关联文件:
//
//	internal/server/biz/model_catalog_preview.go
//	internal/server/biz/model.go
//	internal/objects/model.go
//
// 注意事项:
//
//	单行冲突不撤销其他已确认行；更新价格不能替换渠道关联或手工维护的模型资料。
package biz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/model"
	"github.com/looplj/axonhub/internal/objects"
)

var errCatalogConflict = errors.New("model changed since preview")
var errCatalogQuoteChanged = errors.New("catalog offer changed since preview")
var errCatalogSelection = errors.New("invalid catalog selection")

// ApplyCatalogImport re-derives the prices from the reviewed catalog. The
// client supplies choices and versions, never a replacement price snapshot.
func (svc *ModelService) ApplyCatalogImport(ctx context.Context, scope CatalogImportScope, revision string, selections []CatalogImportSelection) ([]CatalogImportOutcome, error) {
	preview, err := svc.CatalogImportPreview(ctx, scope)
	if err != nil {
		return nil, err
	}
	rows := map[string]*CatalogImportRow{}
	for _, row := range preview.Rows {
		for _, id := range row.Aliases {
			rows[id] = row
		}
	}
	used := map[string]bool{}
	result := make([]CatalogImportOutcome, 0, len(selections))
	for _, selection := range selections {
		status := "applied"
		switch {
		case slices.ContainsFunc(selection.Aliases, func(id string) bool { return used[id] }):
			status = "conflict"
		case preview.Revision != revision:
			status = "catalogChanged"
		default:
			err := svc.applyCatalogRow(ctx, scope, preview.Source, rows, selection)
			switch {
			case errors.Is(err, errCatalogQuoteChanged):
				status = "catalogChanged"
			case errors.Is(err, errCatalogSelection):
				status = "invalid"
			case errors.Is(err, errCatalogConflict), ent.IsConstraintError(err), ent.IsNotFound(err):
				status = "conflict"
			case err != nil:
				return result, fmt.Errorf("apply catalog model %q: %w", selection.ModelID, err)
			}
		}
		if status == "applied" {
			for _, id := range selection.Aliases {
				used[id] = true
			}
		}
		result = append(result, CatalogImportOutcome{ModelID: selection.ModelID, Status: status})
	}
	return result, nil
}

func (svc *ModelService) applyCatalogRow(ctx context.Context, scope CatalogImportScope, source string, rows map[string]*CatalogImportRow, sel CatalogImportSelection) error {
	if len(sel.Aliases) == 0 || strings.TrimSpace(sel.ModelID) == "" {
		return errCatalogSelection
	}
	validID := false
	versions := map[string]bool{}
	seenAliases := map[string]bool{}
	choices := map[catalogOfferKey]*CatalogImportOffer{}
	for _, id := range sel.Aliases {
		row := rows[id]
		if row == nil {
			return errCatalogConflict
		}
		for _, alias := range row.Aliases {
			seenAliases[alias] = true
		}
		versions[row.QuoteVersion] = true
		if row.ModelID == sel.ModelID {
			validID = true
		}
		if row.ModelID != sel.ModelID && (!scope.IgnorePrefix || catalogSuffix(id) != catalogSuffix(sel.ModelID) || row.Version != "") {
			return errCatalogSelection
		}
		if row.Archived {
			return errCatalogConflict
		}
		for _, o := range row.Offers {
			choices[catalogOfferKey{o.Provider, o.ModelID}] = o
		}
	}
	if !slices.Equal(sortedStringKeys(versions), sel.QuoteVersions) {
		return errCatalogQuoteChanged
	}
	if len(seenAliases) != len(sel.Aliases) {
		return errCatalogSelection
	}
	if !validID && !(scope.IgnorePrefix && catalogSuffix(sel.Aliases[0]) == sel.ModelID) {
		return errCatalogSelection
	}
	db := svc.entFromContext(ctx)
	current, err := db.Model.Query().Where(model.ModelIDEQ(sel.ModelID)).Only(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return err
	}
	var card *objects.ModelCard
	primary := choices[catalogOfferKey{sel.Provider, sel.SourceModelID}]
	if sel.Provider != "" && primary == nil {
		return errCatalogSelection
	}
	if current != nil {
		if sel.Version != modelCatalogVersion(current) || current.Status == model.StatusArchived || source == catalogSourceFallback {
			return errCatalogConflict
		}
		card = current.ModelCard
	} else {
		if sel.Version != "" {
			return errCatalogConflict
		}
		if strings.TrimSpace(sel.Name) == "" || strings.TrimSpace(sel.Developer) == "" {
			return errCatalogSelection
		}
		card = newImportCard(primary)
		if len(sel.Aliases) > 1 {
			card.CatalogAliases = slices.Clone(sel.Aliases)
		}
	}
	if card == nil {
		card = &objects.ModelCard{}
	}
	if card.CatalogPrices == nil {
		card.CatalogPrices = map[string]objects.CatalogPrice{}
	}
	if err := applyCatalogPrices(card, choices, sel.Prices); err != nil {
		return err
	}
	if current != nil {
		if len(sel.Prices) == 0 {
			return nil
		}
		return db.Model.UpdateOneID(current.ID).Where(model.UpdatedAtEQ(current.UpdatedAt)).SetModelCard(card).Exec(ctx)
	}
	settings := &objects.ModelSettings{LoadBalancerStrategy: "default", TraceStickyMode: "default"}
	seen := map[string]bool{}
	for _, id := range sel.Aliases {
		if !seen[id] {
			settings.Associations = append(settings.Associations, &objects.ModelAssociation{Type: "model", ModelID: &objects.ModelIDAssociation{ModelID: id}})
			seen[id] = true
		}
	}
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

func applyCatalogPrices(card *objects.ModelCard, offers map[catalogOfferKey]*CatalogImportOffer, actions []CatalogImportPriceInput) error {
	seen := map[string]bool{}
	for _, a := range actions {
		dst := card.Cost.Field(a.Field)
		if dst == nil || seen[a.Field] {
			return errCatalogSelection
		}
		seen[a.Field] = true
		if a.Action == CatalogPriceFix {
			b := card.CatalogPrices[a.Field]
			b.Follow = false
			card.CatalogPrices[a.Field] = b
			continue
		}
		offer := offers[catalogOfferKey{a.Provider, a.ModelID}]
		rate := offer.rate(a.Field)
		if rate == nil {
			return errCatalogSelection
		}
		value := rate.Base
		switch a.Action {
		case CatalogPriceFollow, CatalogPriceFixBase:
			if value == nil {
				return errCatalogSelection
			}
		case CatalogPriceTier:
			value = nil
			for _, tier := range rate.Tiers {
				if a.Tier != nil && tier.Label == *a.Tier {
					value = &tier.Value
					break
				}
			}
			if value == nil {
				return errCatalogSelection
			}
		default:
			return errCatalogSelection
		}
		*dst = *value
		card.CatalogPrices[a.Field] = objects.CatalogPrice{Provider: a.Provider, ModelID: a.ModelID, Follow: a.Action == CatalogPriceFollow}
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

// Old clients submit the whole card. Carry forward its tracking metadata and
// detach only rates that were edited (including explicitly setting zero).
type ManualPriceEdit struct {
	Field   string
	Missing bool
}

func preserveCatalogPrices(old, next *objects.ModelCard, edited []ManualPriceEdit) {
	if old == nil {
		return
	}
	next.CatalogPrices = maps.Clone(old.CatalogPrices)
	next.CatalogAliases = slices.Clone(old.CatalogAliases)
	for field, b := range next.CatalogPrices {
		oldValue := old.Cost.Field(field)
		if oldValue != nil && (*oldValue != *next.Cost.Field(field)) {
			b.Follow, b.Missing = false, false
			next.CatalogPrices[field] = b
		}
	}
	for _, edit := range edited {
		if next.Cost.Field(edit.Field) == nil {
			continue
		}
		if next.CatalogPrices == nil {
			next.CatalogPrices = map[string]objects.CatalogPrice{}
		}
		next.CatalogPrices[edit.Field] = objects.CatalogPrice{Missing: edit.Missing}
	}
}
