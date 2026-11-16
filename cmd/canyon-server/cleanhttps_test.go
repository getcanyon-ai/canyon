package main

import "testing"

// A URL Canyon keeps for the deploy repo, or its pulls API, has a host of lower-case letters, digits, . and - alone,
// with an optional port (contracts/routes.md, the platform-repo row; #2473): the platform team's card hands the
// repo's URL to a kubectl command people paste into a shell, so no host carries what a shell reads.
func TestARepoURLsHostIsPlain(t *testing.T) {
	for _, s := range []string{"https://github.com/meridian/deploy", "https://git.meridian.example:8443/platform/deploy.git", "https://10.0.4.12/a/b"} {
		if _, ok := cleanHTTPS(s); !ok {
			t.Errorf("%s is refused", s)
		}
	}
	for _, s := range []string{"https://git$(id).example/a/b", "https://git;touch.example/a/b", "https://git'x.example/a/b", "https://git&x.example/a/b",
		"https://git!x.example/a/b", "https://git*x.example/a/b", "https://git(x).example/a/b", "https://git=x.example/a/b", "https://git+x.example/a/b",
		"https://git,x.example/a/b", "https://git.example:/a/b", "https://git.example:84a3/a/b", "https://[::1]/a/b", "https://GIT.example/a/b", "https://git_x.example/a/b"} {
		if _, ok := cleanHTTPS(s); ok {
			t.Errorf("%s is kept", s)
		}
	}
}
