//go:build !noee

package runs_test

import (
	"github.com/getcanyon-ai/canyon/ee/patching"
	"github.com/getcanyon-ai/canyon/runs"
)

// In a build with Canyon Enterprise the tests' pipeline plans with its platform patching, as the release's does.
func init() { runs.SetTestPatcher(patching.New(nil)) }
