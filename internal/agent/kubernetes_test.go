package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"ghcall/internal/config"
	"ghcall/internal/pipeline"
	"ghcall/internal/vcs"
)

// newTestLauncher wires a kubernetesLauncher at an httptest API-server stub,
// with a token read from a temp file the way the projected ServiceAccount
// volume would provide it.
func newTestLauncher(t *testing.T, cfg config.AgentConfig, srv *httptest.Server) *kubernetesLauncher {
	t.Helper()
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("sa-token\n"), 0o600); err != nil {
		t.Fatalf("writing fake token: %v", err)
	}
	return &kubernetesLauncher{
		cfg:        cfg,
		baseURL:    srv.URL,
		namespace:  "ghcall",
		tokenPath:  tokenPath,
		httpClient: srv.Client(),
	}
}

func testAgentConfig() config.AgentConfig {
	return config.AgentConfig{
		Image:            "autofix:latest",
		Launcher:         config.LauncherKubernetes,
		PerRunTimeoutSec: 1500,
		Kubernetes: config.KubernetesConfig{
			ServiceAccount:   "ghcall-agent",
			EnvFromSecrets:   []string{"ghcall-agent-creds"},
			ImagePullSecrets: []string{"artifactory"},
			JobTTLSeconds:    3600,
			Labels:           map[string]string{"team": "platform"},
			Resources:        map[string]any{"limits": map[string]any{"cpu": "2"}},
		},
	}
}

func TestKubernetesLaunch_CreatesJob(t *testing.T) {
	var (
		gotPath, gotAuth, gotMethod string
		gotJob                      k8sJob
	)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotJob); err != nil {
			t.Errorf("decoding posted job: %v (body %s)", err, body)
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"kind":"Job"}`))
	}))
	defer srv.Close()

	l := newTestLauncher(t, testAgentConfig(), srv)
	target := pipeline.FilterResult{Provider: vcs.GitHub, Repo: "mchekalov/ghcall", PR: pipeline.PullRequestResult{Number: 42, CIState: "FAILURE"}}

	out, err := l.launch(context.Background(), target, []string{"GITHUB_TOKEN=tok"})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if want := "ghcall-autofix-mchekalov-ghcall-42"; out != want {
		t.Errorf("output %q, want the created job name %q", out, want)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method %s, want POST", gotMethod)
	}
	if want := "/apis/batch/v1/namespaces/ghcall/jobs"; gotPath != want {
		t.Errorf("path %q, want %q", gotPath, want)
	}
	if want := "Bearer sa-token"; gotAuth != want {
		t.Errorf("Authorization %q, want %q (token trimmed of its trailing newline)", gotAuth, want)
	}

	if gotJob.APIVersion != "batch/v1" || gotJob.Kind != "Job" {
		t.Errorf("got %s/%s, want batch/v1 Job", gotJob.APIVersion, gotJob.Kind)
	}
	if gotJob.Metadata.Name != "ghcall-autofix-mchekalov-ghcall-42" {
		t.Errorf("job name %q", gotJob.Metadata.Name)
	}
	if gotJob.Metadata.Namespace != "ghcall" {
		t.Errorf("job namespace %q, want ghcall", gotJob.Metadata.Namespace)
	}
	if gotJob.Metadata.Labels["team"] != "platform" {
		t.Errorf("configured labels not merged in: %v", gotJob.Metadata.Labels)
	}
	if gotJob.Metadata.Labels["ghcall.dev/pr"] != "42" {
		t.Errorf("pr label %v", gotJob.Metadata.Labels)
	}

	spec := gotJob.Spec
	if spec.BackoffLimit == nil || *spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit %v, want 0 (ghcall re-decides on the next run, the API server must not retry)", spec.BackoffLimit)
	}
	if spec.ActiveDeadlineSeconds == nil || *spec.ActiveDeadlineSeconds != 1500 {
		t.Errorf("activeDeadlineSeconds %v, want per_run_timeout_seconds (1500)", spec.ActiveDeadlineSeconds)
	}
	if spec.TTLSecondsAfterFinished == nil || *spec.TTLSecondsAfterFinished != 3600 {
		t.Errorf("ttlSecondsAfterFinished %v, want job_ttl_seconds (3600)", spec.TTLSecondsAfterFinished)
	}

	pod := spec.Template.Spec
	if pod.RestartPolicy != "Never" {
		t.Errorf("restartPolicy %q, want Never", pod.RestartPolicy)
	}
	if pod.ServiceAccountName != "ghcall-agent" {
		t.Errorf("serviceAccountName %q", pod.ServiceAccountName)
	}
	if len(pod.ImagePullSecrets) != 1 || pod.ImagePullSecrets[0].Name != "artifactory" {
		t.Errorf("imagePullSecrets %+v", pod.ImagePullSecrets)
	}
	if len(pod.Containers) != 1 {
		t.Fatalf("got %d containers, want 1", len(pod.Containers))
	}
	ctr := pod.Containers[0]
	if ctr.Image != "autofix:latest" {
		t.Errorf("image %q", ctr.Image)
	}
	wantArgs := []string{"--platform", "github", "--repo", "mchekalov/ghcall", "--pr", "42"}
	if strings.Join(ctr.Args, " ") != strings.Join(wantArgs, " ") {
		t.Errorf("args %v, want %v", ctr.Args, wantArgs)
	}
	if len(ctr.Env) != 1 || ctr.Env[0].Name != "GITHUB_TOKEN" || ctr.Env[0].Value != "tok" {
		t.Errorf("env %+v, want GITHUB_TOKEN=tok", ctr.Env)
	}
	if len(ctr.EnvFrom) != 1 || ctr.EnvFrom[0].SecretRef.Name != "ghcall-agent-creds" {
		t.Errorf("envFrom %+v", ctr.EnvFrom)
	}
	if ctr.Resources == nil {
		t.Errorf("resources not passed through: %+v", ctr.Resources)
	}
}

// TestKubernetesLaunch_ConflictIsSkipNotError covers the dedupe path: the
// deterministic job name means a second run while the first is still in
// flight (or within its TTL) gets a 409, which must not be reported as a
// failure.
func TestKubernetesLaunch_ConflictIsSkipNotError(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"kind":"Status","reason":"AlreadyExists"}`))
	}))
	defer srv.Close()

	l := newTestLauncher(t, testAgentConfig(), srv)
	target := pipeline.FilterResult{Provider: vcs.GitHub, Repo: "o/r", PR: pipeline.PullRequestResult{Number: 7, CIState: "FAILURE"}}

	out, err := l.launch(context.Background(), target, nil)
	if err != nil {
		t.Fatalf("409 AlreadyExists must be a skip, not an error: %v", err)
	}
	if !strings.Contains(out, "ghcall-autofix-o-r-7") || !strings.Contains(out, "already in flight") {
		t.Errorf("output %q should name the job and say it was skipped", out)
	}
}

func TestKubernetesLaunch_ErrorStatus(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"kind":"Status","reason":"Forbidden","message":"jobs is forbidden"}`))
	}))
	defer srv.Close()

	l := newTestLauncher(t, testAgentConfig(), srv)
	target := pipeline.FilterResult{Provider: vcs.GitHub, Repo: "o/r", PR: pipeline.PullRequestResult{Number: 7, CIState: "FAILURE"}}

	if _, err := l.launch(context.Background(), target, nil); err == nil {
		t.Fatal("expected an error for 403 Forbidden")
	} else if !strings.Contains(err.Error(), "jobs is forbidden") {
		t.Errorf("error %q should carry the API server's message", err)
	}
}

// TestRun_KubernetesLauncherEndToEnd drives Run itself (not just launch)
// through the kubernetes launcher, proving the launcher swap in
// launcherFor is wired up and the worker pool is launcher-agnostic.
func TestRun_KubernetesLauncherEndToEnd(t *testing.T) {
	var mu sync.Mutex
	var created []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var job k8sJob
		json.NewDecoder(r.Body).Decode(&job)
		mu.Lock()
		created = append(created, job.Metadata.Name)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	cfg := testAgentConfig()
	cfg.MaxConcurrentRuns = 2
	cfg.PerRunTimeoutSec = 5
	l := newTestLauncher(t, cfg, srv)

	results := []pipeline.FilterResult{
		{Provider: vcs.GitHub, Repo: "o/a", PR: pipeline.PullRequestResult{Number: 1, CIState: "FAILURE"}},
		{Provider: vcs.GitHub, Repo: "o/b", PR: pipeline.PullRequestResult{Number: 2, CIState: "ERROR"}},
		{Provider: vcs.GitHub, Repo: "o/c", PR: pipeline.PullRequestResult{Number: 3, CIState: "SUCCESS"}}, // not a candidate
	}

	// Run resolves the launcher from cfg; point that resolution at the stub.
	orig := newKubernetesLauncherFunc
	defer func() { newKubernetesLauncherFunc = orig }()
	newKubernetesLauncherFunc = func(config.AgentConfig) (launchFunc, error) { return l.launch, nil }

	out := Run(context.Background(), results, cfg, []string{"GITHUB_TOKEN=tok"})

	if len(out) != 2 {
		t.Fatalf("got %d results, want 2 (only failing-CI PRs): %+v", len(out), out)
	}
	for _, r := range out {
		if r.Err != nil {
			t.Errorf("%s#%d: %v", r.Repo, r.Number, r.Err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(created) != 2 {
		t.Fatalf("created %v, want 2 jobs", created)
	}
}

func TestJobName(t *testing.T) {
	longRepo := strings.Repeat("averylongorgname", 3) + "/repo"

	cases := []struct {
		repo   string
		number int
		want   string
	}{
		{"mchekalov/ghcall", 42, "ghcall-autofix-mchekalov-ghcall-42"},
		{"Owner/Repo.Name_x", 7, "ghcall-autofix-owner-repo-name-x-7"},
		{"group/subgroup/project", 3, "ghcall-autofix-group-subgroup-project-3"},
	}
	for _, c := range cases {
		if got := jobName(c.repo, c.number); got != c.want {
			t.Errorf("jobName(%q, %d) = %q, want %q", c.repo, c.number, got, c.want)
		}
	}

	long := jobName(longRepo, 1)
	if len(long) > maxJobNameLen {
		t.Errorf("jobName for a long repo is %d chars (%q), want <= %d", len(long), long, maxJobNameLen)
	}
	if long == jobName(longRepo+"x", 1) {
		t.Error("two different long repo names truncated to the same job name; the digest suffix should keep them apart")
	}
	if jobName(longRepo, 1) != long {
		t.Error("jobName must be deterministic — it is the whole dedupe mechanism")
	}
}
