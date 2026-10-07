package schema

import (
	"context"
	"testing"

	"lazarus/internal/config"
)

func TestParseTableList(t *testing.T) {
	raw := `
users	150
orders	5200
products 0
`
	tables := parseTableList(raw)
	if len(tables) != 3 {
		t.Fatalf("len(tables) = %d, want 3", len(tables))
	}
	if tables[0].Name != "orders" || tables[0].RowCount != 5200 {
		t.Errorf("table[0] = %+v, want orders 5200", tables[0])
	}
	if tables[1].Name != "products" || tables[1].RowCount != 0 {
		t.Errorf("table[1] = %+v, want products 0", tables[1])
	}
	if tables[2].Name != "users" || tables[2].RowCount != 150 {
		t.Errorf("table[2] = %+v, want users 150", tables[2])
	}
}

func TestCompareDrift(t *testing.T) {
	baseline := []TableInfo{
		{Name: "users", RowCount: 100},
		{Name: "orders", RowCount: 50},
		{Name: "logs", RowCount: 200},
	}

	current := []TableInfo{
		{Name: "users", RowCount: 110},
		{Name: "orders", RowCount: 0}, // row count collapsed to 0
		{Name: "billing", RowCount: 10}, // new table
		// logs missing
	}

	report := Compare(current, baseline)
	if !report.HasCriticalDrift {
		t.Errorf("expected HasCriticalDrift = true")
	}
	if len(report.MissingTables) != 1 || report.MissingTables[0] != "logs" {
		t.Errorf("MissingTables = %+v, want [logs]", report.MissingTables)
	}
	if len(report.EmptyTables) != 1 || report.EmptyTables[0] != "orders" {
		t.Errorf("EmptyTables = %+v, want [orders]", report.EmptyTables)
	}
	if len(report.NewTables) != 1 || report.NewTables[0] != "billing" {
		t.Errorf("NewTables = %+v, want [billing]", report.NewTables)
	}
}

func TestInspectQuery(t *testing.T) {
	queryFn := func(ctx context.Context, sql string) (string, error) {
		return "users|50\naccounts|100", nil
	}
	tables, err := Inspect(context.Background(), config.EnginePostgres, queryFn)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if len(tables) != 2 {
		t.Fatalf("Inspect() returned %d tables, want 2", len(tables))
	}
}
