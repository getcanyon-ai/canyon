//go:build race

package b

import (
	"testing"

	"example.com/racegraph/a"
)

func TestA(t *testing.T) { a.A() }
