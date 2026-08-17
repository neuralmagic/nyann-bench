package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/neuralmagic/nyann-bench/pkg/controlapi"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	listen := flag.String("listen", ":8080", "HTTP listen address")
	namespace := flag.String("namespace", "", "namespace in which benchmark Jobs are managed")
	tokenFile := flag.String("token-file", "/var/run/secrets/nyann-bench-api/token", "file containing the bearer token")
	allowedHosts := flag.String("allowed-target-hosts", "", "comma-separated exact vLLM or llm-d inference service hosts")
	allowedSuffixes := flag.String("allowed-target-suffixes", "", "comma-separated vLLM or llm-d inference service DNS suffixes")
	allowedPVCs := flag.String("allowed-pvcs", "", "comma-separated PVC names benchmark Jobs may mount")
	targetsFile := flag.String("targets-file", "", "JSON file mapping logical MCP target names to operator-owned inference destinations")
	resultPVC := flag.String("result-pvc", "", "PVC used for durable MCP benchmark results")
	resultRoot := flag.String("result-root", "", "absolute result path mounted in the API and benchmark Jobs")
	datasetPVC := flag.String("dataset-pvc", "", "PVC containing MCP benchmark datasets")
	datasetRoot := flag.String("dataset-root", "", "absolute dataset root mounted in benchmark Jobs")
	allowedPlatforms := flag.String("allowed-platforms", "kubernetes,openshift", "comma-separated platforms accepted by MCP plans")
	enableLegacyREST := flag.Bool("enable-legacy-rest", false, "enable the trusted command-vector /v1/runs compatibility API")
	runnerImage := flag.String("runner-image", "", "immutable nyann-bench image digest used for run Jobs")
	maxWorkers := flag.Int("max-workers", 16, "maximum workers per run")
	maxCPU := flag.String("max-cpu", "16", "maximum CPU per worker")
	maxMemory := flag.String("max-memory", "64Gi", "maximum memory per worker")
	defaultDeadline := flag.Duration("default-active-deadline", time.Hour, "default run active deadline")
	maxDeadline := flag.Duration("max-active-deadline", 6*time.Hour, "maximum run active deadline")
	defaultTTL := flag.Duration("default-retention-ttl", 24*time.Hour, "default completed Job retention")
	maxTTL := flag.Duration("max-retention-ttl", 7*24*time.Hour, "maximum completed Job retention")
	maxArtifactFiles := flag.Int("max-artifact-files", 512, "maximum files inspected in one result directory")
	maxArtifactBytes := flag.Int64("max-artifact-bytes", 64<<30, "maximum total artifact bytes hashed or aggregated per request")
	maxReportRecords := flag.Int64("max-report-records", 50_000_000, "maximum JSONL records aggregated per report")
	maxLatencySamples := flag.Int("max-latency-samples", 100_000, "bounded reservoir size for each latency distribution")
	maxRecordBytes := flag.Int("max-record-bytes", 8<<20, "maximum bytes in one JSONL request record")
	maxIndexedRuns := flag.Int("max-indexed-runs", 10_000, "maximum durable run manifests scanned by list_benchmarks")
	artifactProcessTimeout := flag.Duration("artifact-processing-timeout", 5*time.Minute, "maximum time spent hashing or aggregating artifacts per request")
	flag.Parse()
	if *namespace == "" {
		*namespace = currentNamespace()
	}
	if *namespace == "" {
		fatal("namespace is required (-namespace or POD_NAMESPACE)")
	}
	tokenBytes, err := os.ReadFile(*tokenFile)
	if err != nil {
		fatal("reading bearer token: %v", err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if len(token) < 32 {
		fatal("bearer token must contain at least 32 characters")
	}
	cpuLimit, err := resource.ParseQuantity(*maxCPU)
	if err != nil || cpuLimit.Sign() <= 0 {
		fatal("invalid -max-cpu")
	}
	memoryLimit, err := resource.ParseQuantity(*maxMemory)
	if err != nil || memoryLimit.Sign() <= 0 {
		fatal("invalid -max-memory")
	}
	targets, err := loadTargets(*targetsFile)
	if err != nil {
		fatal("loading logical inference targets: %v", err)
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		fatal("loading in-cluster Kubernetes config: %v", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		fatal("creating Kubernetes client: %v", err)
	}
	options := controlapi.Options{
		Token: token, AllowedTargetHosts: csv(*allowedHosts), AllowedTargetSuffixes: csv(*allowedSuffixes),
		AllowedPVCs: csv(*allowedPVCs), RunnerImage: *runnerImage, MaxWorkers: *maxWorkers, MaxCPU: cpuLimit, MaxMemory: memoryLimit,
		DefaultActiveDeadline: *defaultDeadline, MaxActiveDeadline: *maxDeadline,
		DefaultRetentionTTL: *defaultTTL, MaxRetentionTTL: *maxTTL,
		InferenceTargets: targets, ResultPVC: *resultPVC, ResultRoot: *resultRoot,
		DatasetPVC: *datasetPVC, DatasetRoot: *datasetRoot, AllowedPlatforms: csv(*allowedPlatforms),
		EnableLegacyREST: *enableLegacyREST,
		MaxArtifactFiles: *maxArtifactFiles, MaxArtifactBytes: *maxArtifactBytes,
		MaxReportRecords: *maxReportRecords, MaxLatencySamples: *maxLatencySamples, MaxRecordBytes: *maxRecordBytes,
		MaxIndexedRuns:         *maxIndexedRuns,
		ArtifactProcessTimeout: *artifactProcessTimeout,
	}
	if err := options.Validate(); err != nil {
		fatal("invalid API policy: %v", err)
	}
	server := &http.Server{
		Addr:              *listen,
		Handler:           controlapi.NewServer(client, *namespace, options).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	fmt.Fprintf(os.Stderr, "nyann-bench API listening on %s in namespace %s\n", *listen, *namespace)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fatal("serving API: %v", err)
	}
}

func loadTargets(filename string) (map[string]controlapi.InferenceTarget, error) {
	if strings.TrimSpace(filename) == "" {
		return map[string]controlapi.InferenceTarget{}, nil
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > 256<<10 {
		return nil, fmt.Errorf("targets file exceeds 256 KiB")
	}
	dec := json.NewDecoder(io.LimitReader(file, 256<<10))
	dec.DisallowUnknownFields()
	var targets map[string]controlapi.InferenceTarget
	if err := dec.Decode(&targets); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("targets file must contain exactly one JSON object")
	}
	return targets, nil
}

func csv(value string) []string {
	var result []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func currentNamespace() string {
	if value := strings.TrimSpace(os.Getenv("POD_NAMESPACE")); value != "" {
		return value
	}
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
