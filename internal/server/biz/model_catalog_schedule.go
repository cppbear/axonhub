// 职责: 保存全站目录检查范围与每日时刻，并汇总待管理员确认的差异。
// 关联文件:
//
//	internal/server/biz/model_catalog_preview.go
//	internal/server/biz/system.go
//	internal/server/biz/fx_module.go
//
// 注意事项:
//
//	调度只记录检查结果。手动预览不会订阅服务商，也不会改变每日任务范围。
package biz

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/aptible/supercronic/cronexpr"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/system"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/pkg/xerrors"
	"github.com/looplj/axonhub/internal/server/scheduler"
)

const catalogCheckSettingsKey = "model_catalog_check_settings"
const catalogCheckResultKey = "model_catalog_check_result"

type CatalogCheckSettings struct {
	Enabled      bool     `json:"enabled"`
	Time         string   `json:"time"`
	Timezone     string   `json:"timezone"`
	Providers    []string `json:"providers"`
	IgnorePrefix bool     `json:"ignorePrefix"`
}

type CatalogCheckStatus struct {
	Settings           CatalogCheckSettings
	NextCheck          *time.Time
	LastAttempt        *time.Time
	LastSuccess        *time.Time
	Failed             bool
	Available, Changed int
	ScheduledDay       string `json:",omitempty"`
}

func (s CatalogCheckSettings) next(now time.Time) (time.Time, error) {
	at, err := time.Parse("15:04", s.Time)
	if err != nil {
		return time.Time{}, xerrors.ValidationError("catalog check time must be in HH:MM format")
	}
	if s.Timezone == "" || s.Timezone == "Local" {
		return time.Time{}, xerrors.ValidationError("a site timezone is required")
	}
	zone, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return time.Time{}, xerrors.ValidationError("unknown catalog check timezone")
	}
	rule, err := cronexpr.Parse(fmt.Sprintf("%d %d * * *", at.Minute(), at.Hour()))
	if err != nil {
		return time.Time{}, err
	}
	return rule.Next(now.In(zone)), nil
}

func (svc *ModelService) CatalogCheckStatus(ctx context.Context) (*CatalogCheckStatus, error) {
	state, _, err := svc.readCatalogCheck(ctx)
	return state, err
}

func (svc *ModelService) readCatalogCheck(ctx context.Context) (*CatalogCheckStatus, string, error) {
	settings := CatalogCheckSettings{Time: "08:30", Providers: []string{}}
	value, err := svc.systemService.getSystemValue(ctx, catalogCheckSettingsKey)
	if err == nil {
		if err = json.Unmarshal([]byte(value), &settings); err != nil {
			return nil, "", err
		}
	} else if !ent.IsNotFound(err) {
		return nil, "", err
	}
	state := &CatalogCheckStatus{}
	value, err = svc.systemService.getSystemValue(ctx, catalogCheckResultKey)
	if err == nil {
		if err = json.Unmarshal([]byte(value), state); err != nil {
			return nil, "", err
		}
	} else if !ent.IsNotFound(err) {
		return nil, "", err
	}
	// The counts and timestamps describe the last completed check, including after scope changes.
	state.Settings = settings
	state.NextCheck = nil
	if settings.Enabled {
		next, err := settings.next(time.Now())
		if err != nil {
			return nil, "", err
		}
		state.NextCheck = &next
	}
	return state, value, nil
}

func (svc *ModelService) SaveCatalogCheckSettings(ctx context.Context, settings CatalogCheckSettings) (*CatalogCheckStatus, error) {
	if _, err := settings.next(time.Now()); err != nil {
		return nil, err
	}
	slices.Sort(settings.Providers)
	settings.Providers = slices.Compact(settings.Providers)
	data, err := json.Marshal(settings)
	if err != nil {
		return nil, err
	}
	if err = svc.systemService.setSystemValue(ctx, catalogCheckSettingsKey, string(data)); err != nil {
		return nil, err
	}
	return svc.CatalogCheckStatus(ctx)
}

// compareCheckResult gives one replica the daily check and prevents an older
// check finishing later from replacing the result of a newer manual check.
func (svc *ModelService) compareCheckResult(ctx context.Context, old string, state *CatalogCheckStatus) (string, bool, error) {
	data, err := json.Marshal(state)
	if err != nil {
		return "", false, err
	}
	value := string(data)
	db := svc.entFromContext(ctx)
	if old == "" {
		err = db.System.Create().SetKey(catalogCheckResultKey).SetValue(value).Exec(ctx)
		if ent.IsConstraintError(err) {
			return "", false, nil
		}
	} else {
		var count int
		count, err = db.System.Update().Where(system.KeyEQ(catalogCheckResultKey), system.ValueEQ(old)).SetValue(value).Save(ctx)
		if err == nil && count == 0 {
			return "", false, nil
		}
	}
	if err != nil {
		return "", false, fmt.Errorf("save catalog check result: %w", err)
	}
	svc.systemService.invalidateSystemValueCache(ctx, catalogCheckResultKey)
	return value, true, nil
}

func (svc *ModelService) CheckModelCatalog(ctx context.Context) (*CatalogCheckStatus, error) {
	return svc.checkModelCatalog(ctx, time.Now().UTC(), false)
}

func (svc *ModelService) checkModelCatalog(ctx context.Context, now time.Time, scheduled bool) (*CatalogCheckStatus, error) {
	return authz.RunAsHost(ctx, svc.entFromContext(ctx), "check-model-catalog", func(ctx context.Context) (*CatalogCheckStatus, error) {
		state, old, err := svc.readCatalogCheck(ctx)
		if err != nil {
			return nil, err
		}
		cfg := state.Settings
		if scheduled {
			if !cfg.Enabled {
				return state, nil
			}
			minute := now.Truncate(time.Minute)
			due, err := cfg.next(minute.Add(-time.Minute))
			if err != nil {
				return nil, err
			}
			day := due.Format(time.DateOnly)
			if !due.Equal(minute) || state.ScheduledDay == day {
				return state, nil
			}
			state.ScheduledDay = day
		}
		state.LastAttempt = &now
		token, claimed, err := svc.compareCheckResult(ctx, old, state)
		if err != nil {
			return nil, err
		}
		if !claimed {
			return svc.CatalogCheckStatus(ctx)
		}
		snapshot, err := svc.catalog.snapshot(ctx, false, true)
		var preview *CatalogImportPreview
		if err == nil && snapshot.Source == catalogSourceUpstream {
			preview, err = svc.catalogPreviewFromSnapshot(ctx, CatalogImportScope{Channels: true, Tracked: true, Providers: cfg.Providers, IgnorePrefix: cfg.IgnorePrefix}, snapshot)
		}
		state.Failed = err != nil || preview == nil
		if !state.Failed {
			success := time.Now().UTC()
			state.LastSuccess = &success
			state.Available, state.Changed = 0, 0
			for _, row := range preview.Rows {
				if row.Version == "" {
					state.Available++
				} else if row.Changed && !row.Archived {
					state.Changed++
				}
			}
		}
		_, _, saveErr := svc.compareCheckResult(ctx, token, state)
		if saveErr != nil {
			return nil, saveErr
		}
		if err != nil {
			return nil, err
		}
		return svc.CatalogCheckStatus(ctx)
	})
}

func (svc *ModelService) registerCatalogCheck(ctx context.Context, s *scheduler.Scheduler) error {
	// Read the persisted wall time each tick, so changes take effect on every replica
	// and rollback of a settings mutation cannot leave an in-memory schedule behind.
	return s.Register(ctx, scheduler.TaskSpec{Name: "model-catalog-check", Description: "Check global model catalog changes", CronExpr: "* * * * *", Timezone: "UTC"}, func(ctx context.Context) {
		_, err := svc.checkModelCatalog(ctx, time.Now().UTC(), true)
		if err != nil {
			log.Warn(ctx, "scheduled model catalog check failed", log.Cause(err))
		}
	})
}
