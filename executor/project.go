package executor

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// OwnProject is the compose project of the container this executor runs in, as Docker labels it
// (com.docker.compose.project), read once at start: "" when it runs outside a container, or in one compose didn't
// start, or Docker can't say. Docker names a container's hostname after its ID unless told otherwise, so the hostname
// finds it (contracts/executor.md, What an app's things are labelled; #178).
func (e *Executor) OwnProject(ctx context.Context) string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return ""
	}
	out, err := child(ctx, e.docker(), "container", "inspect", "--format", `{{index .Config.Labels "com.docker.compose.project"}}`, host).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// projectTag gives an image the executor made for an app, tagged canyon/<app>:<tag>, a second name in its project,
// <project>/<app>:<tag>: pack can't label the image it builds, and a down that has removed every labelled thing finds
// its project's images by that name (contracts/executor.md; #178). With no project, nothing.
func (e *Executor) projectTag(ctx context.Context, tag string) error {
	if e.Project == "" {
		return nil
	}
	name := e.Project + "/" + strings.TrimPrefix(tag, "canyon/")
	if out, err := child(ctx, e.docker(), "tag", tag, name).CombinedOutput(); err != nil {
		return fmt.Errorf("docker tag %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// projectLabel is the canyon.project label what the executor makes for an app carries: its containers, the holder of
// its floor, its network and its build caches. None when the executor has no project.
func (e *Executor) projectLabel() []string {
	if e.Project == "" {
		return nil
	}
	return []string{"--label", "canyon.project=" + e.Project}
}
