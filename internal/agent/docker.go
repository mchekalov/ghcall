package agent

import (
	"bytes"
	"context"
	"os/exec"
	"strconv"

	"ghcall/internal/config"
	"ghcall/internal/pipeline"
)

// execCommandContext is overridden in tests so the docker launcher can be
// exercised without a real docker binary or containers.
var execCommandContext = exec.CommandContext

// newDockerLauncher returns a launchFunc that shells out one `docker run`
// per PR and blocks until the container exits (or the context's per-run
// timeout cuts it off), returning its combined stdout+stderr.
//
// env is forwarded as repeated `-e NAME=VALUE` flags.
func newDockerLauncher(cfg config.AgentConfig) launchFunc {
	dockerBin := cfg.DockerBin
	if dockerBin == "" {
		dockerBin = "docker"
	}

	return func(ctx context.Context, target pipeline.FilterResult, env []string) (string, error) {
		args := make([]string, 0, 8+2*len(env))
		args = append(args, "run", "--rm")
		for _, kv := range env {
			args = append(args, "-e", kv)
		}
		args = append(args, cfg.Image,
			"--platform", "github",
			"--repo", target.Repo,
			"--pr", strconv.Itoa(target.PR.Number))

		cmd := execCommandContext(ctx, dockerBin, args...)
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		err := cmd.Run()
		return buf.String(), err
	}
}
