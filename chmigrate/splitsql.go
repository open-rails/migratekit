package chmigrate

import "github.com/open-rails/migratekit/internal/coremigrate"

// splitSQL splits SQL into statements and strips comments. ClickHouse DDL has
// no transactional semantics, so each statement is executed individually. The
// implementation is shared with the Postgres no-transaction path.
func splitSQL(sql string) []string { return coremigrate.SplitStatements(sql) }
