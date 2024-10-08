package sqlite

import (
	"context"
	"strings"

	"github.com/YuukanOO/seelf/internal/deployment/domain"
	"github.com/YuukanOO/seelf/pkg/event"
	"github.com/YuukanOO/seelf/pkg/monad"
	"github.com/YuukanOO/seelf/pkg/storage"
	"github.com/YuukanOO/seelf/pkg/storage/sqlite"
	"github.com/YuukanOO/seelf/pkg/storage/sqlite/builder"
)

type (
	AppsStore interface {
		domain.AppsReader
		domain.AppsWriter
	}

	appsStore struct {
		db *sqlite.Database
	}
)

func NewAppsStore(db *sqlite.Database) AppsStore {
	return &appsStore{db}
}

func (s *appsStore) CheckAppNamingAvailability(
	ctx context.Context,
	name domain.AppName,
	production domain.EnvironmentConfig,
	staging domain.EnvironmentConfig,
) (domain.EnvironmentConfigRequirement, domain.EnvironmentConfigRequirement, error) {
	r, err := builder.
		Query[appNamingResult](`
		SELECT
			NOT EXISTS(SELECT 1 FROM apps WHERE name = ? AND production_target = ?) AS production_available
			,EXISTS(SELECT 1 FROM targets WHERE id = ? AND cleanup_requested_at IS NULL) AS production_target_exists
			,NOT EXISTS(SELECT 1 FROM apps WHERE name = ? AND staging_target = ?) AS staging_available
			,EXISTS(SELECT 1 FROM targets WHERE id = ? AND cleanup_requested_at IS NULL) AS staging_target_exists
		`, name, production.Target(), production.Target(), name, staging.Target(), staging.Target()).
		One(s.db, ctx, appNameUniquenessResultMapper)

	if err != nil {
		return domain.EnvironmentConfigRequirement{}, domain.EnvironmentConfigRequirement{}, err
	}

	return domain.NewEnvironmentConfigRequirement(production, r.productionTargetFound, r.productionAvailable),
		domain.NewEnvironmentConfigRequirement(staging, r.stagingTargetFound, r.stagingAvailable),
		nil
}

func (s *appsStore) CheckAppNamingAvailabilityByID(
	ctx context.Context,
	id domain.AppID,
	production monad.Maybe[domain.EnvironmentConfig],
	staging monad.Maybe[domain.EnvironmentConfig],
) (
	productionRequirement domain.EnvironmentConfigRequirement,
	stagingRequirement domain.EnvironmentConfigRequirement,
	err error,
) {
	productionValue, hasProductionTarget := production.TryGet()
	stagingValue, hasStagingTarget := staging.TryGet()

	if !hasProductionTarget && !hasStagingTarget {
		return productionRequirement, stagingRequirement, err
	}

	var (
		sql  strings.Builder
		args = make([]any, 0, 5)
	)

	// This one is a bit tricky because the request depends on how many target we should check.
	sql.WriteString("SELECT ")

	if hasProductionTarget {
		sql.WriteString(`
		NOT EXISTS(SELECT 1 FROM apps WHERE apps.id != src.id AND apps.name = src.name AND production_target = ?) AS production_available
		,EXISTS(SELECT 1 FROM targets WHERE id = ? AND cleanup_requested_at IS NULL) AS production_target_exists`)
		args = append(args, productionValue.Target(), productionValue.Target())
	} else {
		sql.WriteString("0 AS production_available, 0 AS production_target_exists")
	}

	if hasStagingTarget {
		sql.WriteString(`
		,NOT EXISTS(SELECT 1 FROM apps WHERE apps.id != src.id AND apps.name = src.name AND staging_target = ?) AS staging_available
		,EXISTS(SELECT 1 FROM targets WHERE id = ? AND cleanup_requested_at IS NULL) AS staging_target_exists`)
		args = append(args, stagingValue.Target(), stagingValue.Target())
	} else {
		sql.WriteString(", 0 AS staging_available, 0 AS staging_target_exists")
	}

	sql.WriteString(" FROM apps src WHERE src.id = ?")
	args = append(args, id)

	r, err := builder.
		Query[appNamingResult](sql.String(), args...).
		One(s.db, ctx, appNameUniquenessResultMapper)

	if err != nil {
		return productionRequirement, stagingRequirement, err
	}

	if hasProductionTarget {
		productionRequirement = domain.NewEnvironmentConfigRequirement(productionValue, r.productionTargetFound, r.productionAvailable)
	}

	if hasStagingTarget {
		stagingRequirement = domain.NewEnvironmentConfigRequirement(stagingValue, r.stagingTargetFound, r.stagingAvailable)
	}

	return productionRequirement, stagingRequirement, err
}

func (s *appsStore) HasAppsOnTarget(ctx context.Context, target domain.TargetID) (domain.HasAppsOnTarget, error) {
	r, err := builder.
		Query[bool](`
		SELECT EXISTS(SELECT 1 FROM apps WHERE production_target = ? OR staging_target = ?)`,
		target, target).
		Extract(s.db, ctx)

	return domain.HasAppsOnTarget(r), err
}

func (s *appsStore) GetByID(ctx context.Context, id domain.AppID) (domain.App, error) {
	return builder.
		Query[domain.App](`
		SELECT
			id
			,name
			,version_control_url
			,version_control_token
			,production_target
			,production_version
			,production_vars
			,staging_target
			,staging_version
			,staging_vars
			,cleanup_requested_at
			,cleanup_requested_by
			,created_at
			,created_by
		FROM apps
		WHERE id = ?`, id).
		One(s.db, ctx, domain.AppFrom)
}

func (s *appsStore) Write(c context.Context, apps ...*domain.App) error {
	return sqlite.WriteAndDispatch(s.db, c, apps, func(ctx context.Context, e event.Event) error {
		switch evt := e.(type) {
		case domain.AppCreated:
			return builder.
				Insert("apps", builder.Values{
					"id":                 evt.ID,
					"name":               evt.Name,
					"production_target":  evt.Production.Target(),
					"production_version": evt.Production.Version(),
					"production_vars":    evt.Production.Vars(),
					"staging_target":     evt.Staging.Target(),
					"staging_version":    evt.Staging.Version(),
					"staging_vars":       evt.Staging.Vars(),
					"created_at":         evt.Created.At(),
					"created_by":         evt.Created.By(),
				}).
				Exec(s.db, ctx)
		case domain.AppEnvChanged:
			// This is safe to interpolate the column name here since events are raised by our
			// own code.
			return builder.
				Update("apps", builder.Values{
					string(evt.Environment) + "_target":  evt.Config.Target(),
					string(evt.Environment) + "_version": evt.Config.Version(),
					string(evt.Environment) + "_vars":    evt.Config.Vars(),
				}).
				F("WHERE id = ?", evt.ID).
				Exec(s.db, ctx)
		case domain.AppVersionControlConfigured:
			return builder.
				Update("apps", builder.Values{
					"version_control_url":   evt.Config.Url(),
					"version_control_token": evt.Config.Token(),
				}).
				F("WHERE id = ?", evt.ID).
				Exec(s.db, ctx)
		case domain.AppVersionControlRemoved:
			return builder.
				Update("apps", builder.Values{
					"version_control_url":   nil,
					"version_control_token": nil,
				}).
				F("WHERE id = ?", evt.ID).
				Exec(s.db, ctx)
		case domain.AppCleanupRequested:
			return builder.
				Update("apps", builder.Values{
					"cleanup_requested_at": evt.Requested.At(),
					"cleanup_requested_by": evt.Requested.By(),
				}).
				F("WHERE id = ?", evt.ID).
				Exec(s.db, ctx)
		case domain.AppDeleted:
			return builder.
				Command("DELETE FROM apps WHERE id = ?", evt.ID).
				Exec(s.db, ctx)
		default:
			return nil
		}
	})
}

type appNamingResult struct {
	productionAvailable   bool
	productionTargetFound bool
	stagingAvailable      bool
	stagingTargetFound    bool
}

func appNameUniquenessResultMapper(s storage.Scanner) (r appNamingResult, err error) {
	err = s.Scan(
		&r.productionAvailable,
		&r.productionTargetFound,
		&r.stagingAvailable,
		&r.stagingTargetFound,
	)

	return r, err
}
