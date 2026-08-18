package controlapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/neuralmagic/nyann-bench/pkg/config"
	"github.com/neuralmagic/nyann-bench/pkg/kube"
)

const (
	managedLabel      = "nyann-bench.neuralmagic.com/managed"
	createdAnnotation = "nyann-bench.neuralmagic.com/created-at"
	resultsAnnotation = "nyann-bench.neuralmagic.com/results"
	requestAnnotation = "nyann-bench.neuralmagic.com/request-sha256"
)

type runSpec struct {
	Name                  string      `json:"name,omitempty"`
	Command               []string    `json:"command"`
	Workers               int         `json:"workers,omitempty"`
	Arch                  string      `json:"arch,omitempty"`
	CPU                   string      `json:"cpu,omitempty"`
	Memory                string      `json:"memory,omitempty"`
	ActiveDeadlineSeconds int64       `json:"active_deadline_seconds,omitempty"`
	Mounts                []mountSpec `json:"mounts,omitempty"`
	Results               *resultSpec `json:"results,omitempty"`
}

type mountSpec struct {
	PVC       string `json:"pvc"`
	MountPath string `json:"mount_path"`
}

// resultSpec describes storage, not a benchmark result format. nyann-bench
// continues to write its native requests_N.jsonl and timestamps_N.json files.
type resultSpec struct {
	PVC       string `json:"pvc"`
	MountPath string `json:"mount_path"`
	Subdir    string `json:"subdir,omitempty"`
}

type ResultMetadata struct {
	Durable bool   `json:"durable"`
	PVC     string `json:"pvc,omitempty"`
	Path    string `json:"path,omitempty"`
	URI     string `json:"uri,omitempty"`
	Format  string `json:"format,omitempty"`
}

type Run struct {
	ID                      string         `json:"id"`
	Status                  string         `json:"status"`
	Workers                 int32          `json:"workers"`
	Succeeded               int32          `json:"succeeded"`
	Failed                  int32          `json:"failed"`
	Active                  int32          `json:"active"`
	CreatedAt               time.Time      `json:"created_at"`
	StartedAt               *time.Time     `json:"started_at,omitempty"`
	EndedAt                 *time.Time     `json:"ended_at,omitempty"`
	Results                 ResultMetadata `json:"results"`
	ActiveDeadlineSeconds   int64          `json:"active_deadline_seconds"`
	TTLSecondsAfterFinished int32          `json:"ttl_seconds_after_finished"`
}

type Server struct {
	client               kubernetes.Interface
	namespace            string
	options              Options
	now                  func() time.Time
	starlarkCompileSlots chan struct{}
}

// InferenceTarget is an operator-owned logical destination. MCP clients select
// its configured map key; they never provide URLs or other network destinations.
type InferenceTarget struct {
	URL   string `json:"url"`
	Model string `json:"model,omitempty"`
}

type Options struct {
	Token                  string
	AllowedTargetHosts     []string
	AllowedTargetSuffixes  []string
	AllowedPVCs            []string
	RunnerImage            string
	MaxWorkers             int
	MaxCPU                 resource.Quantity
	MaxMemory              resource.Quantity
	DefaultActiveDeadline  time.Duration
	MaxActiveDeadline      time.Duration
	DefaultRetentionTTL    time.Duration
	MaxRetentionTTL        time.Duration
	InferenceTargets       map[string]InferenceTarget
	ResultPVC              string
	ResultRoot             string
	DatasetPVC             string
	DatasetRoot            string
	AllowedPlatforms       []string
	MaxArtifactFiles       int
	MaxArtifactBytes       int64
	MaxReportRecords       int64
	MaxLatencySamples      int
	MaxRecordBytes         int
	MaxIndexedRuns         int
	ArtifactProcessTimeout time.Duration
	StarlarkCompilerPath   string
	starlarkCompiler       func(context.Context, string) (*config.ScenarioConfig, error)
}

func DefaultOptions() Options {
	return Options{
		MaxWorkers:             16,
		MaxCPU:                 resource.MustParse("16"),
		MaxMemory:              resource.MustParse("64Gi"),
		DefaultActiveDeadline:  time.Hour,
		MaxActiveDeadline:      6 * time.Hour,
		DefaultRetentionTTL:    24 * time.Hour,
		MaxRetentionTTL:        7 * 24 * time.Hour,
		MaxArtifactFiles:       512,
		MaxArtifactBytes:       64 << 30,
		MaxReportRecords:       50_000_000,
		MaxLatencySamples:      100_000,
		MaxRecordBytes:         8 << 20,
		MaxIndexedRuns:         10_000,
		ArtifactProcessTimeout: 5 * time.Minute,
		StarlarkCompilerPath:   "/nyann-bench",
	}
}

func (o Options) Validate() error {
	if len(strings.TrimSpace(o.Token)) < 32 {
		return fmt.Errorf("bearer token must contain at least 32 characters")
	}
	if !validRunnerImage(o.RunnerImage) {
		return fmt.Errorf("runner image must be an immutable official sha256 digest")
	}
	if o.MaxWorkers < 1 {
		return fmt.Errorf("max workers must be positive")
	}
	if o.MaxCPU.Sign() <= 0 || o.MaxMemory.Sign() <= 0 {
		return fmt.Errorf("CPU and memory limits must be positive")
	}
	if o.DefaultActiveDeadline <= 0 || o.MaxActiveDeadline < o.DefaultActiveDeadline {
		return fmt.Errorf("active deadline limits are invalid")
	}
	if o.DefaultRetentionTTL <= 0 || o.MaxRetentionTTL < o.DefaultRetentionTTL || o.MaxRetentionTTL/time.Second > time.Duration(math.MaxInt32) {
		return fmt.Errorf("retention TTL limits are invalid")
	}
	for _, host := range o.AllowedTargetHosts {
		host = strings.ToLower(strings.TrimSpace(host))
		if strings.Contains(host, ".") || len(validation.IsDNS1123Label(host)) > 0 {
			return fmt.Errorf("allowed target host %q must be a single in-namespace Service name", host)
		}
	}
	for _, suffix := range o.AllowedTargetSuffixes {
		normalized := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(suffix)), ".")
		if !(strings.HasSuffix(normalized, ".svc") || strings.HasSuffix(normalized, ".svc.cluster.local")) || len(strings.Split(normalized, ".")) < 2 {
			return fmt.Errorf("allowed target suffix %q must be namespace-scoped Kubernetes Service DNS", suffix)
		}
	}
	for _, pvc := range o.AllowedPVCs {
		if len(validation.IsDNS1123Subdomain(strings.TrimSpace(pvc))) > 0 {
			return fmt.Errorf("allowed PVC %q is invalid", pvc)
		}
	}
	for name, target := range o.InferenceTargets {
		if len(validation.IsDNS1123Label(name)) > 0 {
			return fmt.Errorf("logical inference target %q must be a DNS label", name)
		}
		if _, _, err := validateCommand([]string{"generate", "--target", target.URL}, o); err != nil {
			return fmt.Errorf("logical inference target %q: %w", name, err)
		}
	}
	if (o.ResultPVC == "") != (o.ResultRoot == "") {
		return fmt.Errorf("result PVC and result root must be configured together")
	}
	if o.ResultRoot != "" {
		if !o.pvcConfigured(o.ResultPVC) || !validAbsoluteRoot(o.ResultRoot) {
			return fmt.Errorf("result storage must use an allowed PVC and clean absolute root")
		}
	}
	if (o.DatasetPVC == "") != (o.DatasetRoot == "") {
		return fmt.Errorf("dataset PVC and dataset root must be configured together")
	}
	if o.DatasetRoot != "" {
		if !o.pvcConfigured(o.DatasetPVC) || !validAbsoluteRoot(o.DatasetRoot) {
			return fmt.Errorf("dataset storage must use an allowed PVC and clean absolute root")
		}
	}
	for _, platform := range o.allowedPlatforms() {
		if platform != "kubernetes" && platform != "openshift" {
			return fmt.Errorf("unsupported allowed platform %q", platform)
		}
	}
	if o.MaxArtifactFiles < 1 || o.MaxArtifactBytes < 1 || o.MaxReportRecords < 1 || o.MaxLatencySamples < 1 || o.MaxRecordBytes < 1 || o.MaxIndexedRuns < 1 || o.ArtifactProcessTimeout <= 0 {
		return fmt.Errorf("artifact and report processing limits must be positive")
	}
	if !path.IsAbs(o.StarlarkCompilerPath) {
		return fmt.Errorf("Starlark compiler path must be absolute")
	}
	return nil
}

func (o Options) pvcConfigured(name string) bool {
	for _, allowed := range o.AllowedPVCs {
		if strings.TrimSpace(allowed) == name {
			return true
		}
	}
	return false
}

func (o Options) allowedPlatforms() []string {
	if len(o.AllowedPlatforms) == 0 {
		return []string{"kubernetes", "openshift"}
	}
	return o.AllowedPlatforms
}

func validAbsoluteRoot(value string) bool {
	return strings.HasPrefix(value, "/") && path.Clean(value) == value && value != "/"
}

func NewServer(client kubernetes.Interface, namespace string, options Options) *Server {
	defaults := DefaultOptions()
	if options.MaxWorkers == 0 {
		options.MaxWorkers = defaults.MaxWorkers
	}
	if options.MaxCPU.IsZero() {
		options.MaxCPU = defaults.MaxCPU
	}
	if options.MaxMemory.IsZero() {
		options.MaxMemory = defaults.MaxMemory
	}
	if options.DefaultActiveDeadline == 0 {
		options.DefaultActiveDeadline = defaults.DefaultActiveDeadline
	}
	if options.MaxActiveDeadline == 0 {
		options.MaxActiveDeadline = defaults.MaxActiveDeadline
	}
	if options.DefaultRetentionTTL == 0 {
		options.DefaultRetentionTTL = defaults.DefaultRetentionTTL
	}
	if options.MaxRetentionTTL == 0 {
		options.MaxRetentionTTL = defaults.MaxRetentionTTL
	}
	if options.MaxArtifactFiles == 0 {
		options.MaxArtifactFiles = defaults.MaxArtifactFiles
	}
	if options.MaxArtifactBytes == 0 {
		options.MaxArtifactBytes = defaults.MaxArtifactBytes
	}
	if options.MaxReportRecords == 0 {
		options.MaxReportRecords = defaults.MaxReportRecords
	}
	if options.MaxLatencySamples == 0 {
		options.MaxLatencySamples = defaults.MaxLatencySamples
	}
	if options.MaxRecordBytes == 0 {
		options.MaxRecordBytes = defaults.MaxRecordBytes
	}
	if options.MaxIndexedRuns == 0 {
		options.MaxIndexedRuns = defaults.MaxIndexedRuns
	}
	if options.ArtifactProcessTimeout == 0 {
		options.ArtifactProcessTimeout = defaults.ArtifactProcessTimeout
	}
	if options.StarlarkCompilerPath == "" {
		options.StarlarkCompilerPath = defaults.StarlarkCompilerPath
	}
	return &Server{client: client, namespace: namespace, options: options, now: time.Now, starlarkCompileSlots: make(chan struct{}, 1)}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /readyz", s.ready)
	mux.Handle("/mcp", s.MCPHandler())
	return s.authenticate(mux)
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if s.options.Token == "" {
			writeError(w, http.StatusServiceUnavailable, "API authentication is not configured")
			return
		}
		if provided == r.Header.Get("Authorization") || subtle.ConstantTimeCompare([]byte(provided), []byte(s.options.Token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if _, err := s.client.BatchV1().Jobs(s.namespace).List(r.Context(), metav1.ListOptions{Limit: 1}); err != nil {
		writeError(w, http.StatusServiceUnavailable, "Kubernetes API is not ready")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) upsertManagedService(ctx context.Context, desired *corev1.Service, fingerprint string) error {
	existing, err := s.client.CoreV1().Services(s.namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, createErr := s.client.CoreV1().Services(s.namespace).Create(ctx, desired, metav1.CreateOptions{})
		if !apierrors.IsAlreadyExists(createErr) {
			return createErr
		}
		existing, err = s.client.CoreV1().Services(s.namespace).Get(ctx, desired.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
	}
	if err != nil {
		return err
	}
	if existing.Labels[managedLabel] != "true" {
		return apierrors.NewAlreadyExists(corev1.Resource("services"), desired.Name)
	}
	if existing.Annotations[requestAnnotation] == fingerprint {
		return nil
	}
	// A Service name and its request fingerprint are immutable together. In
	// particular, never let a concurrent different submission replace the
	// discovery Service underneath a Job that is about to win the create race.
	return apierrors.NewAlreadyExists(corev1.Resource("services"), desired.Name)
}

func (s *Server) attachServiceOwner(ctx context.Context, name string, job *batchv1.Job, fingerprint string) error {
	// Real Kubernetes Jobs always receive a UID. The fake client does not,
	// and there is no valid owner reference to attach in that case.
	if job.UID == "" {
		return nil
	}
	controller := true
	owner := metav1.OwnerReference{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: &controller}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		service, err := s.client.CoreV1().Services(s.namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if service.Labels[managedLabel] != "true" || service.Annotations[requestAnnotation] != fingerprint {
			return apierrors.NewConflict(corev1.Resource("services"), name, fmt.Errorf("service ownership fingerprint changed"))
		}
		if len(service.OwnerReferences) == 1 && service.OwnerReferences[0].UID == owner.UID {
			return nil
		}
		service.OwnerReferences = []metav1.OwnerReference{owner}
		_, err = s.client.CoreV1().Services(s.namespace).Update(ctx, service, metav1.UpdateOptions{})
		return err
	})
}

func (s *Server) deleteServiceIfFingerprint(ctx context.Context, name, fingerprint string) error {
	service, err := s.client.CoreV1().Services(s.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if service.Labels[managedLabel] != "true" || service.Annotations[requestAnnotation] != fingerprint {
		return nil
	}
	return s.client.CoreV1().Services(s.namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

func (s *Server) getManagedJob(ctx context.Context, id string) (*batchv1.Job, error) {
	if errs := validation.IsDNS1123Subdomain(id); len(errs) > 0 || len(id) > 63 {
		return nil, apierrors.NewNotFound(batchv1.Resource("jobs"), id)
	}
	job, err := s.client.BatchV1().Jobs(s.namespace).Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if job.Labels[managedLabel] != "true" {
		return nil, apierrors.NewNotFound(batchv1.Resource("jobs"), id)
	}
	return job, nil
}

func (s *Server) prepareRun(req runSpec) (string, kube.KubeConfig, []string, ResultMetadata, error) {
	name := req.Name
	if name == "" {
		var suffix [4]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return "", kube.KubeConfig{}, nil, ResultMetadata{}, fmt.Errorf("generating run ID: %w", err)
		}
		name = "nyann-" + s.now().UTC().Format("20060102-150405-") + hex.EncodeToString(suffix[:])
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 || len(name) > 63 {
		return "", kube.KubeConfig{}, nil, ResultMetadata{}, fmt.Errorf("name must be a DNS-safe Kubernetes name of at most 63 characters")
	}
	command, target, err := validateCommand(req.Command, s.options)
	if err != nil {
		return "", kube.KubeConfig{}, nil, ResultMetadata{}, err
	}
	_ = target // validated here; inference service ownership remains outside this API.
	workers := req.Workers
	if workers == 0 {
		workers = 1
	}
	if workers < 1 || workers > s.options.MaxWorkers {
		return "", kube.KubeConfig{}, nil, ResultMetadata{}, fmt.Errorf("workers must be between 1 and %d", s.options.MaxWorkers)
	}
	if req.Arch != "" && req.Arch != "amd64" && req.Arch != "arm64" {
		return "", kube.KubeConfig{}, nil, ResultMetadata{}, fmt.Errorf("arch must be amd64 or arm64")
	}
	if req.Arch == "" {
		req.Arch = "amd64"
	}
	for field, value := range map[string]string{"cpu": req.CPU, "memory": req.Memory} {
		if value != "" {
			quantity, err := resource.ParseQuantity(value)
			if err != nil || quantity.Sign() <= 0 {
				return "", kube.KubeConfig{}, nil, ResultMetadata{}, fmt.Errorf("%s must be a positive Kubernetes quantity", field)
			}
		}
	}
	cpu := req.CPU
	if cpu == "" {
		cpu = "4"
	}
	memory := req.Memory
	if memory == "" {
		memory = "8Gi"
	}
	cpuQuantity := resource.MustParse(cpu)
	if cpuQuantity.Cmp(s.options.MaxCPU) > 0 {
		return "", kube.KubeConfig{}, nil, ResultMetadata{}, fmt.Errorf("cpu exceeds the server maximum of %s", s.options.MaxCPU.String())
	}
	memoryQuantity := resource.MustParse(memory)
	if memoryQuantity.Cmp(s.options.MaxMemory) > 0 {
		return "", kube.KubeConfig{}, nil, ResultMetadata{}, fmt.Errorf("memory exceeds the server maximum of %s", s.options.MaxMemory.String())
	}
	if !validRunnerImage(s.options.RunnerImage) {
		return "", kube.KubeConfig{}, nil, ResultMetadata{}, fmt.Errorf("server runner image must be an immutable ghcr.io/neuralmagic/nyann-bench digest")
	}
	resultMeta := ResultMetadata{}
	volumes, err := s.validateMounts(req.Mounts)
	if err != nil {
		return "", kube.KubeConfig{}, nil, ResultMetadata{}, err
	}
	if req.Results != nil {
		if !s.pvcAllowed(req.Results.PVC) {
			return "", kube.KubeConfig{}, nil, ResultMetadata{}, fmt.Errorf("results PVC is not allowed by the operator")
		}
		resultPath, err := validateResults(*req.Results, name)
		if err != nil {
			return "", kube.KubeConfig{}, nil, ResultMetadata{}, err
		}
		command = append(command, "--output-dir", resultPath)
		found := false
		for _, volume := range volumes {
			if volume.MountPath == req.Results.MountPath && volume.PVC != req.Results.PVC {
				return "", kube.KubeConfig{}, nil, ResultMetadata{}, fmt.Errorf("results.mount_path conflicts with an existing mount")
			}
			if volume.PVC == req.Results.PVC && volume.MountPath == req.Results.MountPath {
				found = true
			}
		}
		if !found {
			volumes = append(volumes, kube.VolumeSpec{PVC: req.Results.PVC, MountPath: req.Results.MountPath})
		}
		resultMeta = ResultMetadata{
			Durable: true,
			PVC:     req.Results.PVC,
			Path:    resultPath,
			URI:     "pvc://" + req.Results.PVC + strings.TrimPrefix(resultPath, req.Results.MountPath),
			Format:  "nyann-bench-v1 (requests_N.jsonl, timestamps_N.json)",
		}
	}
	command = append(command, "--metrics", ":9090")
	if workers > 1 {
		command = append(command, "--workers", strconv.Itoa(workers))
	}
	cfg := kube.KubeConfig{
		Name: name, Namespace: s.namespace, Platform: "kubernetes", Image: s.options.RunnerImage,
		Arch: req.Arch, Workers: workers, CPU: req.CPU, Memory: req.Memory, Volumes: volumes,
		NetworkTuning: boolPointer(false), Restricted: true,
	}
	return name, cfg, command, resultMeta, nil
}

func validateCommand(input []string, options Options) ([]string, string, error) {
	if len(input) == 0 || len(input) > 128 {
		return nil, "", fmt.Errorf("command must contain between 1 and 128 arguments")
	}
	for _, arg := range input {
		if arg == "" || len(arg) > 64<<10 || strings.ContainsRune(arg, '\x00') {
			return nil, "", fmt.Errorf("command contains an invalid argument")
		}
	}
	validPrefix := input[0] == "generate" || (len(input) >= 2 && input[0] == "eval" && (input[1] == "gsm8k" || input[1] == "gpqa"))
	if !validPrefix {
		return nil, "", fmt.Errorf("command must start with generate, eval gsm8k, or eval gpqa")
	}
	var target string
	var configValue string
	var scenarioIRValue string
	for i, arg := range input {
		flag := strings.SplitN(arg, "=", 2)[0]
		if strings.HasPrefix(flag, "--kube") || flag == "--worker-id" || flag == "--workers" || flag == "--metrics" || flag == "--output-dir" {
			return nil, "", fmt.Errorf("%s is managed by the API and cannot be supplied", flag)
		}
		if flag == "--prometheus-url" {
			return nil, "", fmt.Errorf("--prometheus-url is not allowed by the API")
		}
		if arg == "--target" {
			if i+1 >= len(input) || target != "" {
				return nil, "", fmt.Errorf("command must contain exactly one --target")
			}
			target = input[i+1]
		} else if strings.HasPrefix(arg, "--target=") {
			if target != "" {
				return nil, "", fmt.Errorf("command must contain exactly one --target")
			}
			target = strings.TrimPrefix(arg, "--target=")
		} else if arg == "--config" {
			if i+1 >= len(input) || configValue != "" {
				return nil, "", fmt.Errorf("command may contain at most one --config with a value")
			}
			configValue = input[i+1]
		} else if strings.HasPrefix(arg, "--config=") {
			if configValue != "" {
				return nil, "", fmt.Errorf("command may contain at most one --config")
			}
			configValue = strings.TrimPrefix(arg, "--config=")
		} else if arg == "--scenario-ir" {
			if i+1 >= len(input) || scenarioIRValue != "" {
				return nil, "", fmt.Errorf("command may contain at most one --scenario-ir with a value")
			}
			scenarioIRValue = input[i+1]
		} else if strings.HasPrefix(arg, "--scenario-ir=") {
			if scenarioIRValue != "" {
				return nil, "", fmt.Errorf("command may contain at most one --scenario-ir")
			}
			scenarioIRValue = strings.TrimPrefix(arg, "--scenario-ir=")
		}
	}
	if configValue != "" && scenarioIRValue != "" {
		return nil, "", fmt.Errorf("command may contain only one of --config or --scenario-ir")
	}
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return nil, "", fmt.Errorf("--target must be an http(s) URL for an existing in-cluster vLLM or llm-d inference service")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if _, err := netip.ParseAddr(host); err == nil {
		return nil, "", fmt.Errorf("--target IP literals are not allowed")
	}
	if host == "localhost" || host == "kubernetes" || host == "kubernetes.default" || strings.HasPrefix(host, "kubernetes.default.svc") {
		return nil, "", fmt.Errorf("--target destination is reserved")
	}
	if !hostAllowed(host, options.AllowedTargetHosts, options.AllowedTargetSuffixes) {
		return nil, "", fmt.Errorf("--target host %q is not in the operator allowlist", host)
	}
	if configValue != "" {
		if err := validateInlineConfig(configValue); err != nil {
			return nil, "", err
		}
	}
	if scenarioIRValue != "" {
		scenario, err := config.ParseScenarioIR(scenarioIRValue)
		if err != nil {
			return nil, "", err
		}
		if err := validateScenarioBounds(scenario); err != nil {
			return nil, "", err
		}
		if err := validateScenarioDatasetPaths(scenario, options.DatasetRoot); err != nil {
			return nil, "", err
		}
	}
	return append([]string(nil), input...), target, nil
}

func hostAllowed(host string, exact, suffixes []string) bool {
	for _, allowed := range exact {
		allowed = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(allowed), "."))
		if !strings.Contains(allowed, ".") && host == allowed {
			return true
		}
	}
	for _, allowed := range suffixes {
		suffix := strings.ToLower(strings.TrimSpace(allowed))
		if suffix == "" {
			continue
		}
		if !strings.HasPrefix(suffix, ".") {
			suffix = "." + suffix
		}
		if !(strings.HasSuffix(suffix, ".svc") || strings.HasSuffix(suffix, ".svc.cluster.local")) {
			continue
		}
		if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
			return true
		}
	}
	return false
}

func validRunnerImage(image string) bool {
	const prefix = "ghcr.io/neuralmagic/nyann-bench@sha256:"
	if !strings.HasPrefix(image, prefix) {
		return false
	}
	digest := strings.TrimPrefix(image, prefix)
	if len(digest) != 64 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil && digest == strings.ToLower(digest)
}

func validateInlineConfig(value string) error {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "{") {
		return fmt.Errorf("API --config must be inline JSON; file and Starlark configs cannot be inspected for target overrides")
	}
	var decoded any
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return fmt.Errorf("invalid inline JSON --config: %w", err)
	}
	if containsAuxiliaryDestination(decoded) {
		return fmt.Errorf("--config may not contain target, URL, or Prometheus destination overrides")
	}
	return nil
}

func containsAuxiliaryDestination(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			key = strings.ToLower(strings.ReplaceAll(key, "-", "_"))
			if key == "target" || strings.Contains(key, "prometheus") || strings.HasSuffix(key, "_url") {
				return true
			}
			if containsAuxiliaryDestination(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if containsAuxiliaryDestination(child) {
				return true
			}
		}
	case string:
		lower := strings.ToLower(strings.TrimSpace(typed))
		if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
			return true
		}
	}
	return false
}

func validateResults(spec resultSpec, runName string) (string, error) {
	if errs := validation.IsDNS1123Subdomain(spec.PVC); len(errs) > 0 {
		return "", fmt.Errorf("results.pvc must be a valid PVC name")
	}
	if !strings.HasPrefix(spec.MountPath, "/") || path.Clean(spec.MountPath) != spec.MountPath || spec.MountPath == "/" {
		return "", fmt.Errorf("results.mount_path must be a clean absolute path other than /")
	}
	subdir := spec.Subdir
	if subdir == "" {
		subdir = "runs"
	}
	if path.IsAbs(subdir) || path.Clean(subdir) != subdir || subdir == "." || strings.HasPrefix(subdir, "../") {
		return "", fmt.Errorf("results.subdir must be a clean relative path")
	}
	return path.Join(spec.MountPath, subdir, runName), nil
}

func (s *Server) validateMounts(specs []mountSpec) ([]kube.VolumeSpec, error) {
	if len(specs) > 8 {
		return nil, fmt.Errorf("mounts may contain at most 8 entries")
	}
	volumes := make([]kube.VolumeSpec, 0, len(specs))
	seenPaths := map[string]bool{}
	for _, spec := range specs {
		if !s.pvcAllowed(spec.PVC) {
			return nil, fmt.Errorf("mount PVC %q is not allowed by the operator", spec.PVC)
		}
		if errs := validation.IsDNS1123Subdomain(spec.PVC); len(errs) > 0 {
			return nil, fmt.Errorf("mounts.pvc must be a valid PVC name")
		}
		if !strings.HasPrefix(spec.MountPath, "/") || path.Clean(spec.MountPath) != spec.MountPath || spec.MountPath == "/" {
			return nil, fmt.Errorf("mounts.mount_path must be a clean absolute path other than /")
		}
		if seenPaths[spec.MountPath] {
			return nil, fmt.Errorf("mount paths must be unique")
		}
		seenPaths[spec.MountPath] = true
		volumes = append(volumes, kube.VolumeSpec{PVC: spec.PVC, MountPath: spec.MountPath})
	}
	return volumes, nil
}

func (s *Server) pvcAllowed(name string) bool {
	for _, allowed := range s.options.AllowedPVCs {
		if name == strings.TrimSpace(allowed) {
			return true
		}
	}
	return false
}

func (s *Server) resolveRuntimeLimits(req runSpec) (int64, int32, error) {
	active := time.Duration(req.ActiveDeadlineSeconds) * time.Second
	if active == 0 {
		active = s.options.DefaultActiveDeadline
	}
	if active < time.Second || active > s.options.MaxActiveDeadline {
		return 0, 0, fmt.Errorf("active_deadline_seconds must be between 1 and %d", int64(s.options.MaxActiveDeadline/time.Second))
	}
	ttl := s.options.DefaultRetentionTTL
	if ttl < time.Second || ttl > s.options.MaxRetentionTTL {
		return 0, 0, fmt.Errorf("ttl_seconds_after_finished must be between 1 and %d", int64(s.options.MaxRetentionTTL/time.Second))
	}
	return int64(active / time.Second), int32(ttl / time.Second), nil
}

func boolPointer(value bool) *bool { return &value }

func decorate(meta *metav1.ObjectMeta, name string, annotations map[string]string) {
	if meta.Labels == nil {
		meta.Labels = map[string]string{}
	}
	meta.Labels[managedLabel] = "true"
	meta.Labels["app.kubernetes.io/name"] = "nyann-bench"
	meta.Labels["app.kubernetes.io/instance"] = name
	if annotations != nil {
		if meta.Annotations == nil {
			meta.Annotations = map[string]string{}
		}
		for key, value := range annotations {
			meta.Annotations[key] = value
		}
	}
}

func runFromJob(job *batchv1.Job) Run {
	workers := int32(1)
	if job.Spec.Completions != nil {
		workers = *job.Spec.Completions
	}
	run := Run{ID: job.Name, Status: jobStatus(job), Workers: workers, Succeeded: job.Status.Succeeded, Failed: job.Status.Failed, Active: job.Status.Active}
	if job.Spec.ActiveDeadlineSeconds != nil {
		run.ActiveDeadlineSeconds = *job.Spec.ActiveDeadlineSeconds
	}
	if job.Spec.TTLSecondsAfterFinished != nil {
		run.TTLSecondsAfterFinished = *job.Spec.TTLSecondsAfterFinished
	}
	_ = json.Unmarshal([]byte(job.Annotations[resultsAnnotation]), &run.Results)
	if value := job.Annotations[createdAnnotation]; value != "" {
		run.CreatedAt, _ = time.Parse(time.RFC3339Nano, value)
	} else {
		run.CreatedAt = job.CreationTimestamp.Time
	}
	if job.Status.StartTime != nil {
		value := job.Status.StartTime.Time
		run.StartedAt = &value
	}
	if job.Status.CompletionTime != nil {
		value := job.Status.CompletionTime.Time
		run.EndedAt = &value
	}
	return run
}

func jobStatus(job *batchv1.Job) string {
	for _, condition := range job.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		switch condition.Type {
		case batchv1.JobComplete:
			return "succeeded"
		case batchv1.JobFailed:
			return "failed"
		}
	}
	if job.Status.Active > 0 {
		return "running"
	}
	return "pending"
}

func commandDefaultName(command []string) string {
	if command[0] == "eval" {
		return "eval-" + command[1]
	}
	return command[0]
}

func ensureEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("request body must contain exactly one JSON object")
	}
	return nil
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
