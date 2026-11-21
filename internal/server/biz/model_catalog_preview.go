// 职责: 比较全局模型与服务商目录，生成手动导入和每日检查共用的候选清单。
// 关联文件:
//
//	internal/server/biz/catalog.go
//	internal/server/biz/model_catalog_apply.go
//	internal/server/biz/model_catalog_schedule.go
//
// 注意事项:
//
//	同名后缀只用于查找候选；冲突版本须由管理员选择合并。
package biz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/model"
	"github.com/looplj/axonhub/internal/objects"
)

var catalogPriceFields = []string{"input", "output", "cacheRead", "cacheWrite"}
var catalogCostKeys = map[string]string{"input": "input", "output": "output", "cacheRead": "cache_read", "cacheWrite": "cache_write"}

type CatalogImportScope struct {
	Channels              bool
	Providers             []string
	IgnorePrefix, Tracked bool
}

type CatalogImportTier struct {
	Label string
	Value float64
}

type CatalogImportRate struct {
	Field string
	Base  *float64
	Tiers []CatalogImportTier
}

type CatalogImportOffer struct {
	Provider, ModelID, Name, Developer, Canonical, Type string
	Rates                                               []CatalogImportRate
	fingerprint                                         string
	entry                                               map[string]any
}

type CatalogImportLocalPrice struct {
	Field             string
	Value             *float64
	Follow            bool
	Provider, ModelID string
}

type CatalogImportRow struct {
	ModelID, Name, Developer, Version string
	QuoteVersion, MergeTarget         string
	MergeTargetOccupied               bool
	Aliases                           []string
	Archived, Changed                 bool
	Offers                            []*CatalogImportOffer
	Preferred                         *CatalogImportOffer
	Prices                            []CatalogImportLocalPrice
}

type CatalogImportPreview struct {
	Source, UpstreamURL, Revision string
	Providers                     []string
	Rows                          []*CatalogImportRow
}

type CatalogPriceAction string

const (
	CatalogPriceFollow  CatalogPriceAction = "FOLLOW"
	CatalogPriceFixBase CatalogPriceAction = "FIX_BASE"
	CatalogPriceFix     CatalogPriceAction = "FIX"
	CatalogPriceTier    CatalogPriceAction = "TIER"
)

type CatalogImportPriceInput struct {
	Field             string
	Action            CatalogPriceAction
	Provider, ModelID string
	Tier              *string
}

type CatalogImportSelection struct {
	ModelID, Version, Name, Developer, Provider, SourceModelID, Type string
	QuoteVersions                                                    []string
	Aliases                                                          []string
	Prices                                                           []CatalogImportPriceInput
}

type CatalogImportStatus string

const (
	CatalogApplied  CatalogImportStatus = "applied"
	CatalogInvalid  CatalogImportStatus = "invalid"
	CatalogConflict CatalogImportStatus = "conflict"
	CatalogChanged  CatalogImportStatus = "catalogChanged"
)

type CatalogImportOutcome struct {
	ModelID string
	Status  CatalogImportStatus
}

type catalogOfferKey struct{ provider, id string }

type catalogIndex struct {
	offers               map[catalogOfferKey]*CatalogImportOffer
	exact, byLastSegment map[string][]*CatalogImportOffer
	providers            []string
}

func catalogLastIDSegment(id string) string {
	if slash := strings.LastIndexByte(id, '/'); slash >= 0 && slash+1 < len(id) {
		return id[slash+1:]
	}
	return id
}

func catalogRate(raw map[string]any, key string) *float64 {
	n, ok := raw[key].(float64)
	if !ok || n < 0 || math.IsInf(n, 0) || math.IsNaN(n) {
		return nil
	}
	return &n
}

func offerRates(entry map[string]any) []CatalogImportRate {
	cost, _ := entry["cost"].(map[string]any)
	rawTiers, _ := cost["tiers"].([]any)
	result := make([]CatalogImportRate, 0, len(catalogPriceFields))
	for _, field := range catalogPriceFields {
		key := catalogCostKeys[field]
		rate := CatalogImportRate{Field: field, Base: catalogRate(cost, key), Tiers: []CatalogImportTier{}}
		for _, raw := range rawTiers {
			tier, _ := raw.(map[string]any)
			threshold, _ := tier["tier"].(map[string]any)
			if value := catalogRate(tier, key); value != nil && threshold != nil {
				label := modelString(threshold, "type")
				if size, ok := threshold["size"].(float64); ok {
					label = label + " " + strconv.FormatFloat(size, 'f', -1, 64)
				}
				if label != "" {
					rate.Tiers = append(rate.Tiers, CatalogImportTier{label, *value})
				}
			}
		}
		// Older catalogs expose just this compatibility field.
		if len(rawTiers) == 0 {
			legacy, _ := cost["context_over_200k"].(map[string]any)
			if v := catalogRate(legacy, key); v != nil {
				rate.Tiers = append(rate.Tiers, CatalogImportTier{"context >200k", *v})
			}
		}
		result = append(result, rate)
	}
	return result
}

func catalogModelType(raw string) string {
	switch raw {
	case "image":
		return string(model.TypeImageGeneration)
	case "video":
		return string(model.TypeVideoGeneration)
	case "reranking":
		return string(model.TypeRerank)
	default:
		return strings.ReplaceAll(raw, "-", "_")
	}
}

func indexCatalog(data []byte) (*catalogIndex, error) {
	var catalog catalogFile
	if err := json.Unmarshal(data, &catalog); err != nil {
		return nil, fmt.Errorf("decode model catalog: %w", err)
	}
	idx := &catalogIndex{offers: map[catalogOfferKey]*CatalogImportOffer{}, exact: map[string][]*CatalogImportOffer{}, byLastSegment: map[string][]*CatalogImportOffer{}, providers: sortedStringKeys(catalog.Providers)}
	for _, provider := range idx.providers {
		for _, entry := range catalog.Providers[provider].Models {
			id := modelString(entry, "id")
			if id == "" {
				continue
			}
			key := catalogOfferKey{provider, id}
			if _, exists := idx.offers[key]; exists {
				continue
			}
			canonical := modelString(entry, "canonical_model_id")
			developer, _, ok := strings.Cut(canonical, "/")
			if !ok {
				developer = ""
			}
			switch developer {
			case "moonshotai":
				developer = "moonshot"
			case "deepseek-ai":
				developer = "deepseek"
			case "qwen":
				developer = "alibaba"
			}
			if developer == "" && slices.Contains(DefaultDeveloperIDs, provider) {
				developer = provider
			}
			name := modelString(entry, "display_name")
			if name == "" {
				name = modelString(entry, "name")
			}
			if name == "" {
				name = id
			}
			offer := &CatalogImportOffer{Provider: provider, ModelID: id, Name: name, Developer: developer, Canonical: canonical, Rates: offerRates(entry), Type: catalogModelType(modelString(entry, "type")), entry: entry}
			idx.offers[key] = offer
			idx.exact[id] = append(idx.exact[id], offer)
			suffix := catalogLastIDSegment(id)
			idx.byLastSegment[suffix] = append(idx.byLastSegment[suffix], offer)
		}
	}
	return idx, nil
}

func preferredOffer(offers []*CatalogImportOffer) *CatalogImportOffer {
	if len(offers) == 1 {
		return offers[0]
	}
	var official *CatalogImportOffer
	var identity string
	for _, o := range offers {
		if o.Canonical == "" {
			continue
		}
		if identity != "" && identity != o.Canonical {
			return nil
		}
		identity = o.Canonical
		owner, _, _ := strings.Cut(o.Canonical, "/")
		if !(o.Provider == owner || (owner == "moonshotai" && o.Provider == "moonshot-ai")) {
			continue
		}
		if official != nil {
			return nil
		}
		official = o
	}
	return official
}

func shareCatalogIdentity(ids []string, idx *catalogIndex) bool {
	identity := map[string]string{}
	for _, id := range ids {
		offers := idx.exact[id]
		if len(offers) == 0 {
			return false
		}
		for _, offer := range offers {
			if offer.Canonical == "" {
				return false
			}
			for _, key := range []string{"canonical_model_id", "version", "release_date"} {
				value := modelString(offer.entry, key)
				if value == "" {
					continue
				}
				if known := identity[key]; known != "" && known != value {
					return false
				}
				identity[key] = value
			}
		}
	}
	return true
}

func catalogLocalPrices(card *objects.ModelCard) []CatalogImportLocalPrice {
	result := make([]CatalogImportLocalPrice, 0, 4)
	for _, field := range catalogPriceFields {
		price := CatalogImportLocalPrice{Field: field}
		if card != nil {
			binding := card.CatalogPrices[field]
			price.Follow, price.Provider, price.ModelID = binding.Follow, binding.Provider, binding.ModelID
			price.Value = card.ReferencePrice(field)
		}
		result = append(result, price)
	}
	return result
}

func modelCatalogVersion(m *ent.Model) string {
	return fmt.Sprintf("%d/%s", m.ID, m.UpdatedAt.UTC().Format(time.RFC3339Nano))
}

func (o *CatalogImportOffer) rate(field string) *CatalogImportRate {
	if o != nil {
		for i := range o.Rates {
			if o.Rates[i].Field == field {
				return &o.Rates[i]
			}
		}
	}
	return nil
}

func (svc *ModelService) CatalogImportPreview(ctx context.Context, scope CatalogImportScope) (*CatalogImportPreview, error) {
	snapshot, err := svc.catalog.Snapshot(ctx, false)
	if err != nil {
		return nil, err
	}
	return svc.catalogPreviewFromSnapshot(ctx, scope, snapshot)
}

func (svc *ModelService) catalogPreviewFromSnapshot(ctx context.Context, scope CatalogImportScope, snapshot CatalogSnapshot) (*CatalogImportPreview, error) {
	idx, err := indexCatalog(snapshot.Data)
	if err != nil {
		return nil, err
	}
	revision := sha256.Sum256([]byte(snapshot.upstreamURL + "\x00" + snapshot.Source))
	publicURL, err := url.Parse(snapshot.upstreamURL)
	if err != nil {
		return nil, fmt.Errorf("parse catalog address: %w", err)
	}
	publicURL.User, publicURL.RawQuery, publicURL.Fragment, publicURL.ForceQuery = nil, "", "", false
	preview := &CatalogImportPreview{UpstreamURL: publicURL.String(), Source: snapshot.Source, Revision: hex.EncodeToString(revision[:]), Providers: idx.providers, Rows: []*CatalogImportRow{}}
	db := svc.entFromContext(ctx)
	local, err := db.Model.Query().Select(model.FieldID, model.FieldModelID, model.FieldName, model.FieldDeveloper, model.FieldStatus, model.FieldUpdatedAt, model.FieldModelCard).All(ctx)
	if err != nil {
		return nil, fmt.Errorf("load global models: %w", err)
	}
	existing := map[string]*ent.Model{}
	importedIDs := map[string]string{}
	aliases := map[string][]string{}
	offers := map[string][]*CatalogImportOffer{}
	for _, m := range local {
		existing[m.ModelID] = m
		if m.ModelCard != nil {
			for _, alias := range m.ModelCard.CatalogAliases {
				importedIDs[alias] = m.ModelID
			}
		}
		if scope.Tracked && m.Status != model.StatusArchived && m.ModelCard != nil {
			for _, binding := range m.ModelCard.CatalogPrices {
				if binding.Follow {
					aliases[m.ModelID] = []string{m.ModelID}
					break
				}
			}
		}
	}
	// An exact local ID takes precedence over an alias recorded on another card.
	for id := range existing {
		importedIDs[id] = id
	}
	mergeTargets := map[string]string{}
	if scope.Channels {
		channels, err := db.Channel.Query().Where(channel.StatusIn(channel.StatusEnabled, channel.StatusDisabled)).Select(channel.FieldSupportedModels).All(ctx)
		if err != nil {
			return nil, fmt.Errorf("load configured channel models: %w", err)
		}
		groups := map[string][]string{}
		ids := map[string]bool{}
		for _, ch := range channels {
			for _, id := range ch.SupportedModels {
				if id != "" {
					ids[id] = true
				}
			}
		}
		for _, id := range sortedStringKeys(ids) {
			if publicID, ok := importedIDs[id]; ok {
				aliases[publicID] = append(aliases[publicID], id)
				continue
			}
			group := id
			if scope.IgnorePrefix {
				group = catalogLastIDSegment(id)
			}
			groups[group] = append(groups[group], id)
		}
		for target, group := range groups {
			// A provider may also supply the bare ID in the combined watch scope.
			if scope.IgnorePrefix && importedIDs[target] == "" && !slices.Contains(group, target) &&
				slices.ContainsFunc(idx.exact[target], func(o *CatalogImportOffer) bool { return slices.Contains(scope.Providers, o.Provider) }) {
				group = append(group, target)
				slices.Sort(group)
			}
			occupied := importedIDs[target] != ""
			if len(group) > 1 && !occupied && shareCatalogIdentity(group, idx) {
				aliases[target] = group
				continue
			}
			for _, id := range group {
				aliases[id] = []string{id}
				if scope.IgnorePrefix && (len(group) > 1 || occupied) {
					mergeTargets[id] = target
				}
			}
		}
	}
	// Provider rows can overlap a channel group before that group has been imported.
	channelGroups := map[string]string{}
	for publicID, ids := range aliases {
		for _, id := range ids {
			channelGroups[id] = publicID
		}
	}
	for key, offer := range idx.offers {
		if !slices.Contains(scope.Providers, key.provider) {
			continue
		}
		id := key.id
		if grouped, ok := channelGroups[id]; ok {
			id = grouped
		} else if parent := importedIDs[id]; parent != "" {
			id = parent
		}
		aliases[id] = append(aliases[id], key.id)
		offers[id] = append(offers[id], offer)
	}
	for _, id := range sortedStringKeys(aliases) {
		slices.Sort(aliases[id])
		target := mergeTargets[id]
		row := &CatalogImportRow{
			ModelID: id, Name: id, Aliases: slices.Compact(aliases[id]),
			MergeTarget: target, MergeTargetOccupied: target != "" && importedIDs[target] != "",
			Offers: []*CatalogImportOffer{}, Prices: []CatalogImportLocalPrice{},
		}
		candidates := offers[id]
		if len(candidates) == 0 {
			for _, alias := range row.Aliases {
				candidates = append(candidates, idx.exact[alias]...)
			}
			if scope.IgnorePrefix && len(candidates) == 0 {
				candidates = idx.byLastSegment[catalogLastIDSegment(id)]
			}
		}
		row.Preferred = preferredOffer(candidates)
		if m := existing[id]; m != nil {
			row.Name, row.Developer, row.Version, row.Archived = m.Name, m.Developer, modelCatalogVersion(m), m.Status == model.StatusArchived
			row.Prices = catalogLocalPrices(m.ModelCard)
			for _, p := range row.Prices {
				if p.Provider == "" {
					continue
				}
				o := idx.offers[catalogOfferKey{p.Provider, p.ModelID}]
				if o == nil {
					continue
				}
				candidates = append(candidates, o)
				if len(offers[id]) == 0 && p.Follow {
					row.Preferred = o
				}
				if r := o.rate(p.Field); p.Follow && r != nil && r.Base != nil && (p.Value == nil || *p.Value != *r.Base) {
					row.Changed = true
				}
			}
		} else if row.Preferred != nil {
			row.Name, row.Developer = row.Preferred.Name, row.Preferred.Developer
		}
		seen := map[*CatalogImportOffer]bool{}
		for _, o := range candidates {
			if !seen[o] {
				row.Offers = append(row.Offers, o)
				seen[o] = true
			}
		}
		sort.Slice(row.Offers, func(i, j int) bool {
			a, b := row.Offers[i], row.Offers[j]
			if a.Provider != b.Provider {
				return a.Provider < b.Provider
			}
			return a.ModelID < b.ModelID
		})
		// Include only this candidate and its offers so unrelated catalog updates
		// do not invalidate a whole administrator-reviewed batch.
		hash := sha256.New()
		aliasesJSON, _ := json.Marshal(row.Aliases)
		hash.Write(aliasesJSON)
		for _, offer := range row.Offers {
			if offer.fingerprint == "" {
				raw, err := json.Marshal(offer.entry)
				if err != nil {
					return nil, fmt.Errorf("fingerprint catalog offer: %w", err)
				}
				sum := sha256.Sum256(raw)
				offer.fingerprint = hex.EncodeToString(sum[:])
			}
			fmt.Fprintf(hash, "%q/%s", offer.Provider, offer.fingerprint)
		}
		row.QuoteVersion = hex.EncodeToString(hash.Sum(nil))
		if !scope.Tracked || row.Version == "" || (row.Changed && !row.Archived) {
			preview.Rows = append(preview.Rows, row)
		}
	}
	return preview, nil
}
