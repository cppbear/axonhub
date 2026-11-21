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
	"github.com/looplj/axonhub/internal/pkg/xerrors"
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
		status := CatalogApplied
		switch {
		case slices.ContainsFunc(selection.Aliases, func(id string) bool { return used[id] }):
			status = CatalogConflict
		case preview.Revision != revision:
			status = CatalogChanged
		default:
			err := svc.applyCatalogRow(ctx, preview.Source, rows, selection)
			switch {
			case errors.Is(err, errCatalogQuoteChanged):
				status = CatalogChanged
			case errors.Is(err, errCatalogSelection):
				status = CatalogInvalid
			case errors.Is(err, errCatalogConflict), ent.IsConstraintError(err), ent.IsNotFound(err):
				status = CatalogConflict
			case err != nil:
				return result, fmt.Errorf("apply catalog model %q: %w", selection.ModelID, err)
			}
		}
		if status == CatalogApplied {
			for _, id := range selection.Aliases {
				used[id] = true
			}
		}
		result = append(result, CatalogImportOutcome{ModelID: selection.ModelID, Status: status})
	}
	return result, nil
}

func (svc *ModelService) applyCatalogRow(ctx context.Context, source string, rows map[string]*CatalogImportRow, sel CatalogImportSelection) error {
	if len(sel.Aliases) == 0 || strings.TrimSpace(sel.ModelID) == "" {
		return errCatalogSelection
	}
	merged := false
	versions := map[string]bool{}
	seenAliases := map[string]bool{}
	submitted := map[string]bool{}
	choices := map[catalogOfferKey]*CatalogImportOffer{}
	for _, id := range sel.Aliases {
		if submitted[id] {
			return errCatalogSelection
		}
		submitted[id] = true
		row := rows[id]
		if row == nil {
			return errCatalogConflict
		}
		for _, alias := range row.Aliases {
			seenAliases[alias] = true
		}
		versions[row.QuoteVersion] = true
		if row.ModelID != sel.ModelID {
			if row.MergeTargetOccupied && row.MergeTarget == sel.ModelID {
				return errCatalogConflict
			}
			if row.MergeTarget != sel.ModelID || row.Version != "" || len(sel.Aliases) < 2 {
				return errCatalogSelection
			}
			merged = true
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
	// A bare candidate with the target ID must be part of a manual merge.
	if merged && rows[sel.ModelID] != nil && !submitted[sel.ModelID] {
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
	for _, id := range sel.Aliases {
		settings.Associations = append(settings.Associations, &objects.ModelAssociation{Type: "model", ModelID: &objects.ModelIDAssociation{ModelID: id}})
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
