package executor

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"time"
)

// ownBuilderDockerfile makes canyon-builder with Canyon's run image and its two build variables its own
// (builder.own.Dockerfile; contracts/executor.md, Every app builds on Canyon's build image; #177).
//
//go:embed builder.own.Dockerfile
var ownBuilderDockerfile string

// ownBuilder is canyon-builder as a build with no build values for either of the executor's own two build variables
// uses it: built FROM builderImage, under a tag the hash of builderImage's tag and this file make, so a change to
// either is a new tag. pack takes its run image from its metadata and its two variables from its own build
// environment, so it is handed neither --run-image nor --env for them, and builds on it as it is: without an
// ephemeral copy, which takes about 85 s a build to save on Docker's containerd image store.
var ownBuilder = &scanImage{dockerfile: ownBuilderDockerfile, deadline: time.Hour, args: ownBuilderArgs,
	repo: "canyon-builder:" + fmt.Sprintf("%x", sha256.Sum256([]byte(builderImage.repo+"\n"+ownBuilderDockerfile)))[:12]}

// ownBuilderArgs builds builderImage first, then names it and its metadata with runTag as its one run image.
func ownBuilderArgs(ctx context.Context, docker string) ([]string, error) {
	if _, err := builderImage.built(ctx, docker); err != nil {
		return nil, err
	}
	out, err := child(ctx, docker, "image", "inspect", "--format", `{{index .Config.Labels "io.buildpacks.builder.metadata"}}`, builderImage.repo).Output()
	if err != nil {
		return nil, fmt.Errorf("%s's builder metadata: %w", builderImage.repo, err)
	}
	metadata, err := ownRunImage(out)
	if err != nil {
		return nil, fmt.Errorf("%s's builder metadata: %w", builderImage.repo, err)
	}
	return []string{"--build-arg", "BASE=" + builderImage.repo, "--build-arg", "METADATA=" + string(metadata)}, nil
}

// ownRunImage is a builder's io.buildpacks.builder.metadata with runTag its one run image, under both keys pack
// 0.40.3 reads it from (images, then stack.runImage), mirrors none. Every other field keeps its value.
func ownRunImage(label []byte) ([]byte, error) {
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(label, &metadata); err != nil {
		return nil, err
	}
	var stack map[string]json.RawMessage
	if raw, ok := metadata["stack"]; ok {
		if err := json.Unmarshal(raw, &stack); err != nil {
			return nil, fmt.Errorf("its stack: %w", err)
		}
	}
	if stack == nil {
		stack = map[string]json.RawMessage{}
	}
	run, err := json.Marshal(map[string]any{"image": runTag, "mirrors": []string{}})
	if err != nil {
		return nil, err
	}
	stack["runImage"] = run
	if metadata["stack"], err = json.Marshal(stack); err != nil {
		return nil, err
	}
	metadata["images"] = json.RawMessage("[" + string(run) + "]")
	return json.Marshal(metadata)
}
