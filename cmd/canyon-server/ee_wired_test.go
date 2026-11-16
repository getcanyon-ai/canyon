//go:build !noee

package main

import (
	"reflect"
	"testing"
)

// The server built with Canyon Enterprise, the released one, sets every hook (design/ARCHITECTURE.md §8): a hook left
// nil would answer a licensed install licence_required.
func TestTheReleaseSetsEveryHook(t *testing.T) {
	h, err := wireEnterprise(func(string) string { return "" }, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := reflect.ValueOf(h)
	for i := range v.NumField() {
		if v.Field(i).IsNil() {
			t.Errorf("the release's %s hook is nil: its wiring file (ee_<feature>.go) sets none", v.Type().Field(i).Name)
		}
	}
}
