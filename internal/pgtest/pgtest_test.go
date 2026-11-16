package pgtest

import (
	"strings"
	"testing"
)

func TestASchemaNameFitsPostgresAndTwoLongNamesDoNotCollide(t *testing.T) {
	short := schemaName("TestShort")
	if short != "t_testshort" {
		t.Fatalf("a short name is itself: %q", short)
	}
	if sub := schemaName("TestA/a_row_whose_run-config_does_not_read"); sub != "t_testa_a_row_whose_run_config_does_not_read" {
		t.Fatalf("a subtest's name is an identifier: %q", sub)
	}
	long := strings.Repeat("A", 70)
	a, b := schemaName(long+"X"), schemaName(long+"Y")
	if len(a) > 63 || len(b) > 63 || a == b || !strings.HasPrefix(a, "t_aaaa") {
		t.Fatalf("long names: %q %q", a, b)
	}
	if schemaName(long+"X") != a {
		t.Fatal("the name is stable")
	}
}
