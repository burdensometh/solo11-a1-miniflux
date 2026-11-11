// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package database

import (
	"strings"
	"testing"
)

func TestSchemaVersionAccountsForConcurrentIndexMigrations(t *testing.T) {
	if got, want := schemaVersion, len(migrations)+len(concurrentIndexMigrations); got != want {
		t.Fatalf("schemaVersion = %d, want %d", got, want)
	}

	// The concurrent index migration must sit exactly one version after
	// the last transactional migration, with no gap or overlap.
	if concurrentIndexMigrationVersion != len(migrations) {
		t.Fatalf("concurrentIndexMigrationVersion = %d, want %d", concurrentIndexMigrationVersion, len(migrations))
	}

	if _, ok := concurrentIndexMigrations[concurrentIndexMigrationVersion]; !ok {
		t.Fatalf("no concurrent index migration registered at version position %d", concurrentIndexMigrationVersion)
	}
}

func TestConcurrentIndexSpecsAreSelfConsistent(t *testing.T) {
	for version, spec := range concurrentIndexMigrations {
		if spec.tableName == "" || spec.indexName == "" || spec.createDDL == "" {
			t.Fatalf("migration v%d: tableName, indexName and createDDL must all be set", version+1)
		}
		if !strings.Contains(spec.createDDL, spec.indexName) {
			t.Fatalf("migration v%d: createDDL %q does not create index %q", version+1, spec.createDDL, spec.indexName)
		}
		if !strings.Contains(strings.ToUpper(spec.createDDL), "CREATE INDEX CONCURRENTLY") {
			t.Fatalf("migration v%d: createDDL must use CREATE INDEX CONCURRENTLY: %q", version+1, spec.createDDL)
		}
	}
}
