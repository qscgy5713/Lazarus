package restore

import (
	"strings"
	"testing"
)

func TestScanRolesFindsOwnershipAndGrants(t *testing.T) {
	dump := `
--
-- PostgreSQL database dump
--
SET statement_timeout = 0;
CREATE TABLE public.users (id integer NOT NULL);
ALTER TABLE public.users OWNER TO app_user;
ALTER SCHEMA public OWNER TO postgres;
GRANT SELECT ON TABLE public.users TO readonly_user;
COPY public.users (id) FROM stdin;
1
\.
`

	got := scanRoles(strings.NewReader(dump))

	want := map[string]bool{"app_user": true, "postgres": true, "readonly_user": true}
	if len(got) != len(want) {
		t.Fatalf("scanRoles() = %v, want %d roles", got, len(want))
	}
	for _, role := range got {
		if !want[role] {
			t.Errorf("scanRoles() returned unexpected role %q", role)
		}
	}
}

func TestScanRolesHandlesQuotedIdentifiers(t *testing.T) {
	dump := `ALTER TABLE public.orders OWNER TO "Weird-Name";`

	got := scanRoles(strings.NewReader(dump))
	if len(got) != 1 || got[0] != "Weird-Name" {
		t.Errorf("scanRoles() = %v, want [Weird-Name]", got)
	}
}

func TestScanRolesSkipsPublicAndPseudoRoles(t *testing.T) {
	dump := `
GRANT ALL ON SCHEMA public TO PUBLIC;
GRANT USAGE ON SCHEMA public TO postgres;
SET SESSION AUTHORIZATION CURRENT_USER;
`

	got := scanRoles(strings.NewReader(dump))
	if len(got) != 1 || got[0] != "postgres" {
		t.Errorf("scanRoles() = %v, want only [postgres] — PUBLIC and CURRENT_USER aren't roles to create", got)
	}
}

func TestScanRolesDeduplicates(t *testing.T) {
	dump := `
ALTER TABLE a OWNER TO app_user;
ALTER TABLE b OWNER TO app_user;
ALTER TABLE c OWNER TO app_user;
`

	got := scanRoles(strings.NewReader(dump))
	if len(got) != 1 || got[0] != "app_user" {
		t.Errorf("scanRoles() = %v, want [app_user] once", got)
	}
}

func TestScanRolesOnDumpWithNoOwnership(t *testing.T) {
	dump := "CREATE TABLE users (id int);\nINSERT INTO users VALUES (1);\n"

	if got := scanRoles(strings.NewReader(dump)); len(got) != 0 {
		t.Errorf("scanRoles() = %v, want none", got)
	}
}

func TestScanRolesIsDeterministic(t *testing.T) {
	dump := `
ALTER TABLE a OWNER TO zeta;
ALTER TABLE b OWNER TO alpha;
ALTER TABLE c OWNER TO mid;
`

	first := scanRoles(strings.NewReader(dump))
	second := scanRoles(strings.NewReader(dump))

	if len(first) != 3 {
		t.Fatalf("scanRoles() = %v, want 3 roles", first)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("scanRoles() order not stable: %v vs %v", first, second)
		}
	}
	if first[0] != "alpha" || first[2] != "zeta" {
		t.Errorf("scanRoles() = %v, want sorted order", first)
	}
}

func TestScanRolesHandlesLongDataLines(t *testing.T) {
	// A COPY block with a very long row shouldn't break the scanner.
	longRow := strings.Repeat("x", 200_000)
	dump := "ALTER TABLE t OWNER TO app_user;\nCOPY t (blob) FROM stdin;\n" + longRow + "\n\\.\n"

	got := scanRoles(strings.NewReader(dump))
	if len(got) != 1 || got[0] != "app_user" {
		t.Errorf("scanRoles() = %v, want [app_user] even with a huge data line", got)
	}
}
