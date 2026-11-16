package events

import "cmp"

// failureWords is what a person reads for a failed run's reason (contracts/routes.md, *A failure reason
// in words*): the receipt and the GitHub check read it here; the CLI and the web keep their own copy.
var failureWords = map[string]string{
	"interrupted":  "it was cancelled, or another run of this app replaced it",
	"error":        "Canyon itself failed",
	"model":        "the reviewer's model failed",
	"build":        "the package did not build",
	"detect":       "no buildpack on this host recognised the package",
	"start":        "the app did not stay up",
	"no_room":      "the cluster had no room to run the build",
	"scan_no_room": "the cluster had no room to scan the package",
	"image_gone":   "the image production ran is no longer on this Canyon",
}

// stepWords is each step Canyon itself can fail in, as the reason's words name it.
var stepWords = map[string]string{
	"scan":      "scanning the package",
	"review":    "reviewing the package",
	"resources": "making the app's resources",
	"build":     "building the package",
	"deploy":    "starting the app",
}

// FailureWords is a failed run's reason in words, with the step it failed in when the run names one; a
// reason or a step the list does not name reads as itself.
func FailureWords(reason, step string) string {
	words, ok := failureWords[reason]
	if !ok {
		words = reason
	}
	if step != "" {
		words += " while " + cmp.Or(stepWords[step], step)
	}
	return words
}
