package shardpkg

import (
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) { os.Exit(m.Run()) }

func TestA(t *testing.T) {}

// TestAB has TestA's name as a prefix: only an anchored -run keeps the two apart.
func TestAB(t *testing.T) { t.Run("sub", func(t *testing.T) {}) }

func TestFails(t *testing.T) {
	if os.Getenv("TESTSEL_FAIL") != "" {
		t.Fatal("asked to fail")
	}
}

func ExampleA() {
	fmt.Println(A())
	// Output: a
}

func FuzzA(f *testing.F) {
	f.Add(1)
	f.Fuzz(func(t *testing.T, n int) {})
}

func BenchmarkA(b *testing.B) {}
