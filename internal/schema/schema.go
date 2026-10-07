// Package schema inspects database tables and row counts after restore,
// detecting table dropouts and unexpected row count collapses across runs.
package schema

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"lazarus/internal/config"
	"lazarus/internal/sandbox"
)

// TableInfo represents a table or collection with its estimated/exact row count.
type TableInfo struct {
	Name     string `json:"name"`
	RowCount int64  `json:"row_count"`
}

// DriftReport compares the current restored schema against a previous baseline.
type DriftReport struct {
	TotalTables      int         `json:"total_tables"`
	Tables           []TableInfo `json:"tables,omitempty"`
	MissingTables    []string    `json:"missing_tables,omitempty"`
	EmptyTables      []string    `json:"empty_tables,omitempty"`
	NewTables        []string    `json:"new_tables,omitempty"`
	HasCriticalDrift bool        `json:"has_critical_drift"`
}

// Inspect queries the local or container database and extracts all user tables and row counts.
func Inspect(ctx context.Context, engine config.Engine, queryFn func(ctx context.Context, query string) (string, error)) ([]TableInfo, error) {
	querySQL := introspectionQuery(engine)
	if querySQL == "" {
		return nil, nil
	}

	raw, err := queryFn(ctx, querySQL)
	if err != nil {
		return nil, fmt.Errorf("schema introspection query failed: %w", err)
	}

	return parseTableList(raw), nil
}

func introspectionQuery(engine config.Engine) string {
	switch engine {
	case config.EnginePostgres:
		return "SELECT tablename, 1 FROM pg_catalog.pg_tables WHERE schemaname = 'public' ORDER BY tablename;"
	case config.EngineMySQL:
		return "SELECT table_name, table_rows FROM information_schema.tables WHERE table_schema = '" + sandbox.DBName() + "' ORDER BY table_name;"
	case config.EngineSQLite:
		return "SELECT name, 1 FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name;"
	case config.EngineMongoDB:
		return "db.getCollectionNames().sort().map(c => c + '|' + db.getCollection(c).countDocuments()).join('\\n')"
	default:
		return ""
	}
}

// parseTableList parses lines formatted as "table_name [delimiter] count".
func parseTableList(raw string) []TableInfo {
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	var tables []TableInfo
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		// Split by tab, pipe, or space
		var name string
		var count int64 = -1

		if strings.Contains(l, "|") {
			parts := strings.Split(l, "|")
			name = strings.TrimSpace(parts[0])
			if len(parts) > 1 {
				count, _ = strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
			}
		} else {
			fields := strings.Fields(l)
			name = fields[0]
			if len(fields) > 1 {
				count, _ = strconv.ParseInt(fields[1], 10, 64)
			}
		}

		if name != "" {
			tables = append(tables, TableInfo{Name: name, RowCount: count})
		}
	}
	sort.Slice(tables, func(i, j int) bool {
		return tables[i].Name < tables[j].Name
	})
	return tables
}

// Compare checks current tables against the baseline snapshot.
func Compare(current, baseline []TableInfo) DriftReport {
	currMap := make(map[string]int64, len(current))
	for _, t := range current {
		currMap[t.Name] = t.RowCount
	}

	baseMap := make(map[string]int64, len(baseline))
	for _, t := range baseline {
		baseMap[t.Name] = t.RowCount
	}

	var missing []string
	var empty []string
	var newTables []string

	for bName, bCount := range baseMap {
		cCount, exists := currMap[bName]
		if !exists {
			missing = append(missing, bName)
		} else if bCount > 0 && cCount == 0 {
			empty = append(empty, bName)
		}
	}

	for cName := range currMap {
		if _, exists := baseMap[cName]; !exists && len(baseline) > 0 {
			newTables = append(newTables, cName)
		}
	}

	sort.Strings(missing)
	sort.Strings(empty)
	sort.Strings(newTables)

	return DriftReport{
		TotalTables:      len(current),
		Tables:           current,
		MissingTables:    missing,
		EmptyTables:      empty,
		NewTables:        newTables,
		HasCriticalDrift: len(missing) > 0 || len(empty) > 0,
	}
}

// CompareWithNames checks current tables against expected table names.
func CompareWithNames(current []TableInfo, expectedNames []string) DriftReport {
	baseline := make([]TableInfo, len(expectedNames))
	for i, name := range expectedNames {
		baseline[i] = TableInfo{Name: name, RowCount: 1}
	}
	return Compare(current, baseline)
}
