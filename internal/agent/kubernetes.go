package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ghcall/internal/config"
	"ghcall/internal/pipeline"
)

// The projected ServiceAccount volume every in-cluster pod gets.
const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// maxJobNameLen is the Kubernetes limit for a Job's metadata.name (it must
// be a DNS-1123 label).
const maxJobNameLen = 63

// kubernetesLauncher creates one batch/v1 Job per failing-CI PR by POSTing
// to the in-cluster API server.
//
// This talks to the API server over plain net/http rather than client-go: it
// is a single POST against one stable API version, which keeps the module at
// three direct dependencies instead of the ~40 client-go pulls in — and
// leaves it testable with httptest. baseURL and tokenPath are fields for
// exactly that reason.
type kubernetesLauncher struct {
	cfg        config.AgentConfig
	baseURL    string
	namespace  string
	tokenPath  string
	httpClient *http.Client
}

// newKubernetesLauncherFunc is overridden in tests so Run can be exercised
// against an httptest API-server stub instead of a real cluster.
var newKubernetesLauncherFunc = newKubernetesLauncher

// newKubernetesLauncher builds a launcher from the in-cluster environment:
// the API server address from KUBERNETES_SERVICE_HOST/PORT, and the CA,
// token and default namespace from the projected ServiceAccount volume.
func newKubernetesLauncher(cfg config.AgentConfig) (launchFunc, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("agent launcher %q: not running in a cluster (KUBERNETES_SERVICE_HOST/PORT unset)", config.LauncherKubernetes)
	}

	namespace := cfg.Kubernetes.Namespace
	if namespace == "" {
		data, err := os.ReadFile(filepath.Join(serviceAccountDir, "namespace"))
		if err != nil {
			return nil, fmt.Errorf("agent launcher %q: reading own namespace: %w", config.LauncherKubernetes, err)
		}
		namespace = strings.TrimSpace(string(data))
	}

	caPEM, err := os.ReadFile(filepath.Join(serviceAccountDir, "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("agent launcher %q: reading cluster CA: %w", config.LauncherKubernetes, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("agent launcher %q: cluster CA contains no usable certificate", config.LauncherKubernetes)
	}

	l := &kubernetesLauncher{
		cfg:       cfg,
		baseURL:   "https://" + net.JoinHostPort(host, port),
		namespace: namespace,
		tokenPath: filepath.Join(serviceAccountDir, "token"),
		httpClient: &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
		},
	}
	return l.launch, nil
}

// launch creates the Job for one PR and returns as soon as the API server
// has accepted it — deliberately not waiting for the agent to finish. A
// blocking 25-minute run inside a CronJob with concurrencyPolicy: Forbid
// would starve the schedule, and the Job's own status is what to watch
// instead.
//
// A 409 AlreadyExists is the dedupe signal, not a failure: the Job name is
// derived from the repo and PR, so an identically-named Job means a run for
// this PR is still in flight or still within its ttlSecondsAfterFinished
// window. That keeps idempotency state in the API server rather than in
// ghcall's cache.
func (l *kubernetesLauncher) launch(ctx context.Context, target pipeline.FilterResult, env []string) (string, error) {
	name := jobName(target.Repo, target.PR.Number)

	body, err := json.Marshal(l.jobSpec(name, target, env))
	if err != nil {
		return "", fmt.Errorf("encoding job %s: %w", name, err)
	}

	token, err := os.ReadFile(l.tokenPath)
	if err != nil {
		return "", fmt.Errorf("reading service account token: %w", err)
	}

	url := fmt.Sprintf("%s/apis/batch/v1/namespaces/%s/jobs", l.baseURL, l.namespace)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := l.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("creating job %s: %w", name, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))

	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK:
		return name, nil
	case http.StatusConflict:
		return name + " (already in flight, skipped)", nil
	default:
		return "", fmt.Errorf("creating job %s: unexpected status %s: %s", name, resp.Status, strings.TrimSpace(string(respBody)))
	}
}

// Minimal batch/v1 Job types — only the fields ghcall actually sets, so the
// encoded object stays readable and the API server fills in the rest.
type k8sJob struct {
	APIVersion string      `json:"apiVersion"`
	Kind       string      `json:"kind"`
	Metadata   k8sMetadata `json:"metadata"`
	Spec       k8sJobSpec  `json:"spec"`
}

type k8sMetadata struct {
	Name      string            `json:"name,omitempty"`
	Namespace string            `json:"namespace,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type k8sJobSpec struct {
	BackoffLimit            *int           `json:"backoffLimit,omitempty"`
	ActiveDeadlineSeconds   *int           `json:"activeDeadlineSeconds,omitempty"`
	TTLSecondsAfterFinished *int           `json:"ttlSecondsAfterFinished,omitempty"`
	Template                k8sPodTemplate `json:"template"`
}

type k8sPodTemplate struct {
	Metadata k8sMetadata `json:"metadata,omitempty"`
	Spec     k8sPodSpec  `json:"spec"`
}

type k8sPodSpec struct {
	RestartPolicy      string         `json:"restartPolicy"`
	ServiceAccountName string         `json:"serviceAccountName,omitempty"`
	ImagePullSecrets   []k8sLocalRef  `json:"imagePullSecrets,omitempty"`
	Containers         []k8sContainer `json:"containers"`
}

type k8sLocalRef struct {
	Name string `json:"name"`
}

type k8sContainer struct {
	Name      string         `json:"name"`
	Image     string         `json:"image"`
	Args      []string       `json:"args,omitempty"`
	Env       []k8sEnvVar    `json:"env,omitempty"`
	EnvFrom   []k8sEnvSource `json:"envFrom,omitempty"`
	Resources map[string]any `json:"resources,omitempty"`
}

type k8sEnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type k8sEnvSource struct {
	SecretRef k8sLocalRef `json:"secretRef"`
}

// jobSpec builds the Job object for one PR. backoffLimit is 0 because a
// failed autofix run should not be silently retried by the API server: the
// next ghcall run re-evaluates the PR's CI state and decides again.
func (l *kubernetesLauncher) jobSpec(name string, target pipeline.FilterResult, env []string) k8sJob {
	k := l.cfg.Kubernetes

	labels := map[string]string{
		"app.kubernetes.io/name":       "ghcall-autofix",
		"app.kubernetes.io/managed-by": "ghcall",
		"ghcall.dev/pr":                strconv.Itoa(target.PR.Number),
	}
	for key, v := range k.Labels {
		labels[key] = v
	}

	envVars := make([]k8sEnvVar, 0, len(env))
	for _, kv := range env {
		key, value, ok := strings.Cut(kv, "=")
		if !ok || key == "" {
			continue
		}
		envVars = append(envVars, k8sEnvVar{Name: key, Value: value})
	}

	envFrom := make([]k8sEnvSource, 0, len(k.EnvFromSecrets))
	for _, s := range k.EnvFromSecrets {
		envFrom = append(envFrom, k8sEnvSource{SecretRef: k8sLocalRef{Name: s}})
	}

	pullSecrets := make([]k8sLocalRef, 0, len(k.ImagePullSecrets))
	for _, s := range k.ImagePullSecrets {
		pullSecrets = append(pullSecrets, k8sLocalRef{Name: s})
	}

	backoffLimit := 0
	spec := k8sJobSpec{
		BackoffLimit: &backoffLimit,
		Template: k8sPodTemplate{
			Metadata: k8sMetadata{Labels: labels},
			Spec: k8sPodSpec{
				RestartPolicy:      "Never",
				ServiceAccountName: k.ServiceAccount,
				ImagePullSecrets:   pullSecrets,
				Containers: []k8sContainer{{
					Name:  "autofix",
					Image: l.cfg.Image,
					Args: []string{
						"--platform", "github",
						"--repo", target.Repo,
						"--pr", strconv.Itoa(target.PR.Number),
					},
					Env:       envVars,
					EnvFrom:   envFrom,
					Resources: k.Resources,
				}},
			},
		},
	}
	if l.cfg.PerRunTimeoutSec > 0 {
		deadline := l.cfg.PerRunTimeoutSec
		spec.ActiveDeadlineSeconds = &deadline
	}
	if k.JobTTLSeconds > 0 {
		ttl := k.JobTTLSeconds
		spec.TTLSecondsAfterFinished = &ttl
	}

	return k8sJob{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Metadata:   k8sMetadata{Name: name, Namespace: l.namespace, Labels: labels},
		Spec:       spec,
	}
}

// jobName derives a deterministic DNS-1123 name from the repo and PR number.
// Determinism is the whole dedupe mechanism: two ghcall runs that see the
// same red PR produce the same name, and the second one gets a 409.
func jobName(repo string, number int) string {
	full := "ghcall-autofix-" + sanitizeLabel(repo) + "-" + strconv.Itoa(number)
	if len(full) <= maxJobNameLen {
		return full
	}
	// Too long to keep whole; truncate and re-attach a digest of the full
	// name so distinct repos can't collide into one Job.
	sum := sha256.Sum256([]byte(full))
	digest := hex.EncodeToString(sum[:])[:8]
	head := strings.Trim(full[:maxJobNameLen-len(digest)-1], "-")
	return head + "-" + digest
}

// sanitizeLabel lowercases s and collapses every run of characters that are
// not [a-z0-9] into a single "-", which is what a DNS-1123 label allows.
func sanitizeLabel(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		case !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
