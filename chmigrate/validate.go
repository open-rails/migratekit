package chmigrate

import (
	"context"
	"fmt"
	"io/fs"

	"github.com/open-rails/migratekit"
)

// ValidateMigrations validates that every migration in fsys has been applied
// for the app named in config. Read-only startup gate: never creates tables.
// Mirrors migratekit.ValidatePostgresMigrations for the ClickHouse driver.
func ValidateMigrations(ctx context.Context, config *Config, fsys fs.FS) error {
	migrations, err := migratekit.LoadFromFS(fsys)
	if err != nil {
		return fmt.Errorf("failed to load %s migrations: %w", config.App, err)
	}

	migrator := New(config)
	if err := migrator.ValidateAllApplied(ctx, migrations); err != nil {
		return fmt.Errorf("%s migrations not applied: %w", config.App, err)
	}
	return nil
}
