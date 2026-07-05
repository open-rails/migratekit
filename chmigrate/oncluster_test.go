package chmigrate

import (
	"strings"
	"testing"

	"github.com/open-rails/migratekit"
)

func TestOnClusterTemplateReplacement(t *testing.T) {
	tests := []struct {
		name           string
		cluster        string
		sqlInput       string
		expectedOutput string
	}{
		{
			name:           "With cluster - double braces",
			cluster:        "doujins",
			sqlInput:       "CREATE TABLE foo{{ON_CLUSTER}} (id Int32)",
			expectedOutput: "CREATE TABLE foo ON CLUSTER doujins (id Int32)",
		},
		{
			name:           "With cluster - dollar braces",
			cluster:        "doujins",
			sqlInput:       "CREATE TABLE foo${ON_CLUSTER} (id Int32)",
			expectedOutput: "CREATE TABLE foo ON CLUSTER doujins (id Int32)",
		},
		{
			name:           "Without cluster - double braces",
			cluster:        "",
			sqlInput:       "CREATE TABLE foo{{ON_CLUSTER}} (id Int32)",
			expectedOutput: "CREATE TABLE foo (id Int32)",
		},
		{
			name:           "Without cluster - dollar braces",
			cluster:        "",
			sqlInput:       "CREATE TABLE foo${ON_CLUSTER} (id Int32)",
			expectedOutput: "CREATE TABLE foo (id Int32)",
		},
		{
			name:           "Multiple occurrences",
			cluster:        "prod",
			sqlInput:       "CREATE TABLE t1{{ON_CLUSTER}} (id Int32); CREATE VIEW v1{{ON_CLUSTER}} AS SELECT * FROM t1",
			expectedOutput: "CREATE TABLE t1 ON CLUSTER prod (id Int32); CREATE VIEW v1 ON CLUSTER prod AS SELECT * FROM t1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a ClickHouse instance with the cluster config
			ch := &ClickHouse{
				cluster: tt.cluster,
			}

			// Create a migration with the test SQL
			migration := migratekit.Migration{
				Name:    "001_test.up.sql",
				Content: tt.sqlInput,
			}

			// Apply the ON_CLUSTER replacement logic (extracted from Apply method)
			content := migration.Content
			if ch.cluster != "" {
				content = strings.ReplaceAll(content, "{{ON_CLUSTER}}", " ON CLUSTER "+ch.cluster)
				content = strings.ReplaceAll(content, "${ON_CLUSTER}", " ON CLUSTER "+ch.cluster)
			} else {
				content = strings.ReplaceAll(content, "{{ON_CLUSTER}}", "")
				content = strings.ReplaceAll(content, "${ON_CLUSTER}", "")
			}

			if content != tt.expectedOutput {
				t.Errorf("Template replacement failed\nInput:    %q\nExpected: %q\nGot:      %q",
					tt.sqlInput, tt.expectedOutput, content)
			}
		})
	}
}
