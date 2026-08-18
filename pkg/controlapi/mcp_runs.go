package controlapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/neuralmagic/nyann-bench/pkg/kube"
)

func (s *Server) planBenchmark(ctx context.Context, input benchmarkInput) (*plannedBenchmark, error) {
	scenario, err := s.parseBenchmarkScenario(ctx, input)
	if err != nil {
		return nil, err
	}
	if err := validateScenarioBounds(scenario); err != nil {
		return nil, err
	}
	target, ok := s.options.InferenceTargets[input.Target]
	if !ok || input.Target == "" {
		return nil, fmt.Errorf("target must name an operator-configured logical inference target")
	}
	if input.ResultLabel == "" || !resultLabelPattern.MatchString(input.ResultLabel) {
		return nil, fmt.Errorf("result_label must be a lowercase DNS-safe label of at most 63 characters")
	}
	if input.VDPWorkstream != "" && !attachmentPattern.MatchString(input.VDPWorkstream) {
		return nil, fmt.Errorf("vdp_workstream contains unsupported characters or is too long")
	}
	workers := input.Workers
	if workers == 0 {
		workers = 1
	}
	platform := input.Platform
	if platform == "" {
		platform = "kubernetes"
	}
	if !containsString(s.options.allowedPlatforms(), platform) {
		return nil, fmt.Errorf("platform %q is not allowed by the operator", platform)
	}
	if s.options.ResultPVC == "" || s.options.ResultRoot == "" {
		return nil, fmt.Errorf("durable result storage is not configured")
	}
	if err := validateScenarioDatasetPaths(scenario, s.options.DatasetRoot); err != nil {
		return nil, err
	}
	runnerIR, err := json.Marshal(scenario)
	if err != nil {
		return nil, fmt.Errorf("encoding runner scenario: %w", err)
	}
	if len(runnerIR) > mcpMaximumScenarioBytes {
		return nil, fmt.Errorf("compiled runner scenario exceeds the %d-byte service bound", mcpMaximumScenarioBytes)
	}
	canonical, _ := json.Marshal(input)
	digest := sha256.Sum256(canonical)
	runID := input.RunID
	if runID == "" {
		prefix := strings.NewReplacer(".", "-", "_", "-").Replace(input.ResultLabel)
		if len(prefix) > 40 {
			prefix = prefix[:40]
		}
		runID = fmt.Sprintf("nyann-%s-%s", strings.Trim(prefix, "-"), hex.EncodeToString(digest[:6]))
	}
	if len(validation.IsDNS1123Subdomain(runID)) > 0 || len(runID) > 63 {
		return nil, fmt.Errorf("run_id must be a DNS-safe Kubernetes name of at most 63 characters")
	}
	command := []string{"generate", "--target", target.URL, "--scenario-ir", string(runnerIR)}
	if target.Model != "" {
		command = append(command, "--model", target.Model)
	}
	mounts := []mountSpec{}
	if scenarioUsesDataset(scenario) {
		if s.options.DatasetPVC == "" || s.options.DatasetRoot == "" {
			return nil, fmt.Errorf("the scenario uses a dataset but dataset storage is not configured")
		}
		mounts = append(mounts, mountSpec{PVC: s.options.DatasetPVC, MountPath: s.options.DatasetRoot})
	}
	create := runSpec{Name: runID, Command: command, Workers: workers, Arch: input.Architecture, CPU: input.CPU, Memory: input.Memory, ActiveDeadlineSeconds: input.DeadlineSecond, Mounts: mounts, Results: &resultSpec{PVC: s.options.ResultPVC, MountPath: s.options.ResultRoot, Subdir: filepath.ToSlash(filepath.Join("mcp", input.ResultLabel))}}
	name, cfg, effectiveCommand, results, err := s.prepareRun(create)
	if err != nil {
		return nil, err
	}
	cfg.Platform = platform
	service, job, err := kube.RenderCoreResources(cfg, commandDefaultName(effectiveCommand), effectiveCommand)
	if err != nil {
		return nil, err
	}
	deadline, retention, err := s.resolveRuntimeLimits(create)
	if err != nil {
		return nil, err
	}
	job.Spec.ActiveDeadlineSeconds = &deadline
	job.Spec.TTLSecondsAfterFinished = &retention
	effective := effectiveScenario(scenario)
	effectiveJSON, err := json.Marshal(effective)
	if err != nil {
		return nil, fmt.Errorf("encoding effective scenario: %w", err)
	}
	if len(effectiveJSON) > mcpMaximumScenarioBytes {
		return nil, fmt.Errorf("effective scenario exceeds the %d-byte service bound", mcpMaximumScenarioBytes)
	}
	fingerprintJSON, _ := json.Marshal(struct {
		Command  []string
		Config   kube.KubeConfig
		Deadline int64
		TTL      int32
		Target   string
		Scenario json.RawMessage
	}{effectiveCommand, cfg, deadline, retention, input.Target, effectiveJSON})
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(fingerprintJSON))
	warnings := scenarioWarnings(scenario, workers, time.Duration(deadline)*time.Second)
	resources := resourcesFromJob(job, platform, cfg.Arch, workers, s.options.RunnerImage)
	plan := &plannedBenchmark{RunID: name, Target: plannedTarget{Name: input.Target, Model: target.Model}, EffectiveScenario: effective, Load: plannedLoad(scenario, workers), Resources: resources, Results: results, DeadlineSeconds: deadline, RetentionSeconds: retention, Warnings: warnings, config: cfg, command: effectiveCommand, service: service, job: job, fingerprint: fingerprint, resultLabel: input.ResultLabel, workstream: input.VDPWorkstream}
	if err := s.serverDryRun(ctx, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

func (s *Server) serverDryRun(ctx context.Context, plan *plannedBenchmark) error {
	suffix := "-dry-" + plan.fingerprint[:8]
	prefix := strings.TrimSuffix(plan.RunID, "-")
	if len(prefix)+len(suffix) > 63 {
		prefix = strings.TrimSuffix(prefix[:63-len(suffix)], "-")
	}
	dryRunName := prefix + suffix
	cfg := plan.config
	cfg.Name = dryRunName
	service, job, err := kube.RenderCoreResources(cfg, commandDefaultName(plan.command), plan.command)
	if err != nil {
		return fmt.Errorf("rendering Kubernetes dry-run resources: %w", err)
	}
	job.Spec.ActiveDeadlineSeconds = &plan.DeadlineSeconds
	job.Spec.TTLSecondsAfterFinished = &plan.RetentionSeconds
	metadata := map[string]string{requestAnnotation: plan.fingerprint, targetAnnotation: plan.Target.Name, resultLabelAnnotation: plan.resultLabel, mcpManagedAnnotation: "true"}
	decorate(&service.ObjectMeta, dryRunName, metadata)
	decorate(&job.ObjectMeta, dryRunName, metadata)
	decorate(&job.Spec.Template.ObjectMeta, dryRunName, nil)
	dryRun := metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}, FieldManager: "nyann-bench-mcp-plan"}
	if _, err := s.client.CoreV1().Services(s.namespace).Create(ctx, service, dryRun); err != nil {
		return fmt.Errorf("Kubernetes server-side dry-run rejected Service: %w", err)
	}
	if _, err := s.client.BatchV1().Jobs(s.namespace).Create(ctx, job, dryRun); err != nil {
		return fmt.Errorf("Kubernetes server-side dry-run rejected Job: %w", err)
	}
	return nil
}

func (s *Server) submitBenchmark(ctx context.Context, plan *plannedBenchmark) (Run, bool, error) {
	manifest, manifestExisted, err := s.persistRunManifest(plan)
	if err != nil {
		return Run{}, false, err
	}
	created := s.now().UTC()
	resultsJSON, _ := json.Marshal(plan.Results)
	scenarioJSON, _ := json.Marshal(plan.EffectiveScenario)
	metadata := map[string]string{createdAnnotation: created.Format(time.RFC3339Nano), resultsAnnotation: string(resultsJSON), requestAnnotation: plan.fingerprint, scenarioAnnotation: string(scenarioJSON), targetAnnotation: plan.Target.Name, resultLabelAnnotation: plan.resultLabel, platformAnnotation: plan.Resources.Platform, mcpManagedAnnotation: "true"}
	if plan.workstream != "" {
		metadata[workstreamAnnotation] = plan.workstream
	}
	decorate(&plan.service.ObjectMeta, plan.RunID, metadata)
	decorate(&plan.job.ObjectMeta, plan.RunID, metadata)
	decorate(&plan.job.Spec.Template.ObjectMeta, plan.RunID, nil)
	existing, err := s.client.BatchV1().Jobs(s.namespace).Get(ctx, plan.RunID, metav1.GetOptions{})
	if err == nil {
		if existing.Labels[managedLabel] != "true" || existing.Annotations[requestAnnotation] != plan.fingerprint {
			return Run{}, false, fmt.Errorf("run_id already exists with a different validated specification")
		}
		if err := s.upsertManagedService(ctx, plan.service, plan.fingerprint); err != nil {
			return Run{}, false, friendlyKubeError(err)
		}
		if err := s.attachServiceOwner(ctx, plan.RunID, existing, plan.fingerprint); err != nil {
			return Run{}, false, friendlyKubeError(err)
		}
		return runFromJob(existing), false, nil
	}
	if !apierrors.IsNotFound(err) {
		return Run{}, false, friendlyKubeError(err)
	}
	if manifestExisted {
		hasArtifacts, artifactErr := s.manifestHasBenchmarkArtifacts(manifest)
		if artifactErr != nil {
			return Run{}, false, artifactErr
		}
		if hasArtifacts {
			return runFromManifest(manifest, "archived"), false, nil
		}
	}
	if err := s.upsertManagedService(ctx, plan.service, plan.fingerprint); err != nil {
		return Run{}, false, friendlyKubeError(err)
	}
	job, err := s.client.BatchV1().Jobs(s.namespace).Create(ctx, plan.job, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			existing, getErr := s.client.BatchV1().Jobs(s.namespace).Get(ctx, plan.RunID, metav1.GetOptions{})
			if getErr == nil && existing.Labels[managedLabel] == "true" && existing.Annotations[requestAnnotation] == plan.fingerprint {
				if ownerErr := s.attachServiceOwner(ctx, plan.RunID, existing, plan.fingerprint); ownerErr != nil {
					return Run{}, false, friendlyKubeError(ownerErr)
				}
				return runFromJob(existing), false, nil
			}
		}
		// Keep the immutable Service for an identical concurrent winner or a
		// later retry; cross-resource deletion cannot be made atomic with Job
		// creation.
		return Run{}, false, friendlyKubeError(err)
	}
	if err := s.attachServiceOwner(ctx, plan.RunID, job, plan.fingerprint); err != nil {
		return Run{}, false, friendlyKubeError(err)
	}
	return runFromJob(job), true, nil
}

func (s *Server) persistRunManifest(plan *plannedBenchmark) (durableRunManifest, bool, error) {
	scenarioJSON, err := json.Marshal(plan.EffectiveScenario)
	if err != nil {
		return durableRunManifest{}, false, err
	}
	desired := durableRunManifest{
		SchemaVersion: "nyann-bench-run-v1", RunID: plan.RunID, Fingerprint: plan.fingerprint,
		CreatedAt: s.now().UTC(), Target: plan.Target, ResultLabel: plan.resultLabel,
		VDPWorkstream: plan.workstream, EffectiveScenario: scenarioJSON, Resources: plan.Resources,
		Results: plan.Results, Workers: int32(plan.Resources.Workers), DeadlineSeconds: plan.DeadlineSeconds,
		RetentionSeconds: plan.RetentionSeconds, Image: plan.Resources.Image,
	}
	indexDir := filepath.Join(s.options.ResultRoot, ".nyann-bench", "runs")
	if err := ensureDirectoryBelowRoot(s.options.ResultRoot, indexDir); err != nil {
		return durableRunManifest{}, false, fmt.Errorf("creating durable run index: %w", err)
	}
	indexPath := filepath.Join(indexDir, plan.RunID+".json")
	manifest, created, err := createOrValidateManifest(indexPath, desired)
	if err != nil {
		return durableRunManifest{}, false, err
	}
	dir, err := s.safeResultDirectory(manifest.Results)
	if err != nil {
		if created {
			_ = os.Remove(indexPath)
		}
		return durableRunManifest{}, false, err
	}
	if err := ensureDirectoryBelowRoot(s.options.ResultRoot, dir); err != nil {
		if created {
			_ = os.Remove(indexPath)
		}
		return durableRunManifest{}, false, fmt.Errorf("creating run result directory: %w", err)
	}
	if _, _, err := createOrValidateManifest(filepath.Join(dir, "run-manifest.json"), manifest); err != nil {
		if created {
			_ = os.Remove(indexPath)
		}
		return durableRunManifest{}, false, err
	}
	return manifest, !created, nil
}

func createOrValidateManifest(filename string, desired durableRunManifest) (durableRunManifest, bool, error) {
	data, err := json.MarshalIndent(desired, "", "  ")
	if err != nil {
		return durableRunManifest{}, false, err
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(filepath.Dir(filename), ".run-manifest-*")
	if err != nil {
		return durableRunManifest{}, false, fmt.Errorf("creating durable run manifest temporary file: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o440); err != nil {
		_ = temp.Close()
		return durableRunManifest{}, false, err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return durableRunManifest{}, false, err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return durableRunManifest{}, false, err
	}
	if err := temp.Close(); err != nil {
		return durableRunManifest{}, false, err
	}
	// Linking a fully-written inode gives create-if-absent semantics without
	// exposing a partial manifest to concurrent submitters.
	linkErr := os.Link(tempName, filename)
	if linkErr == nil {
		return desired, true, nil
	}
	if !errors.Is(linkErr, os.ErrExist) {
		return durableRunManifest{}, false, fmt.Errorf("creating durable run manifest: %w", linkErr)
	}
	existing, err := readManifestFile(filename)
	if err != nil {
		return durableRunManifest{}, false, err
	}
	if existing.RunID != desired.RunID || existing.Fingerprint != desired.Fingerprint || existing.Results.Path != desired.Results.Path {
		return durableRunManifest{}, false, fmt.Errorf("run_id already has durable provenance for a different validated specification")
	}
	return existing, false, nil
}

func readManifestFile(filename string) (durableRunManifest, error) {
	var manifest durableRunManifest
	file, err := os.Open(filename)
	if err != nil {
		return manifest, err
	}
	defer file.Close()
	dec := json.NewDecoder(io.LimitReader(file, mcpMaximumRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&manifest); err != nil {
		return manifest, fmt.Errorf("reading durable run manifest: %w", err)
	}
	if manifest.SchemaVersion != "nyann-bench-run-v1" || manifest.RunID == "" || manifest.Fingerprint == "" {
		return manifest, fmt.Errorf("durable run manifest is invalid")
	}
	if err := ensureEOF(dec); err != nil {
		return manifest, err
	}
	return manifest, nil
}

func ensureDirectoryBelowRoot(root, dir string) error {
	root = filepath.Clean(root)
	dir = filepath.Clean(dir)
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path escapes configured root")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return err
	}
	current := root
	if rel == "." {
		return nil
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o750); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, statErr = os.Lstat(current)
		}
		if statErr != nil {
			return statErr
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path component %s is not a real directory", current)
		}
	}
	return nil
}

func (s *Server) loadRunManifest(id string) (durableRunManifest, error) {
	if len(validation.IsDNS1123Subdomain(id)) > 0 || len(id) > 63 {
		return durableRunManifest{}, os.ErrNotExist
	}
	return readManifestFile(filepath.Join(s.options.ResultRoot, ".nyann-bench", "runs", id+".json"))
}

func (s *Server) manifestHasBenchmarkArtifacts(manifest durableRunManifest) (bool, error) {
	dir, err := s.safeResultDirectory(manifest.Results)
	if err != nil {
		return false, err
	}
	entries, err := readBoundedDirectory(dir, s.options.MaxArtifactFiles)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.Name() != "run-manifest.json" && artifactPattern.MatchString(entry.Name()) {
			return true, nil
		}
	}
	return false, nil
}

func runFromManifest(manifest durableRunManifest, status string) Run {
	return Run{ID: manifest.RunID, Status: status, Workers: manifest.Workers, CreatedAt: manifest.CreatedAt, Results: manifest.Results, ActiveDeadlineSeconds: manifest.DeadlineSeconds, TTLSecondsAfterFinished: manifest.RetentionSeconds}
}

func (s *Server) listBenchmarksMCP(ctx context.Context, input benchmarkListInput) (any, error) {
	if input.Limit == 0 {
		input.Limit = 50
	}
	if input.Limit < 1 || input.Limit > 100 {
		return nil, fmt.Errorf("limit must be between 1 and 100")
	}
	if input.Status != "" && !containsString([]string{"pending", "running", "succeeded", "failed", "archived"}, input.Status) {
		return nil, fmt.Errorf("status filter is invalid")
	}
	jobs, err := s.client.BatchV1().Jobs(s.namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.Set{managedLabel: "true"}.String()})
	if err != nil {
		return nil, friendlyKubeError(err)
	}
	runs := make([]Run, 0, len(jobs.Items))
	activeIDs := make(map[string]bool, len(jobs.Items))
	for i := range jobs.Items {
		activeIDs[jobs.Items[i].Name] = true
		run := runFromJob(&jobs.Items[i])
		if input.Status != "" && run.Status != input.Status {
			continue
		}
		if input.Label != "" && jobs.Items[i].Annotations[resultLabelAnnotation] != input.Label {
			continue
		}
		runs = append(runs, run)
	}
	manifestDir := filepath.Join(s.options.ResultRoot, ".nyann-bench", "runs")
	var entries []os.DirEntry
	indexDir, openErr := os.Open(manifestDir)
	if openErr == nil {
		entries, err = indexDir.ReadDir(s.options.MaxIndexedRuns + 1)
		closeErr := indexDir.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("reading durable run index: %w", err)
		}
		if closeErr != nil {
			return nil, closeErr
		}
	} else if !errors.Is(openErr, os.ErrNotExist) {
		return nil, fmt.Errorf("opening durable run index: %w", openErr)
	}
	if len(entries) > s.options.MaxIndexedRuns {
		return nil, fmt.Errorf("durable run index exceeds the configured entry limit")
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if activeIDs[id] {
			continue
		}
		manifest, manifestErr := s.loadRunManifest(id)
		if manifestErr != nil {
			return nil, manifestErr
		}
		run := runFromManifest(manifest, "archived")
		if input.Status != "" && input.Status != run.Status {
			continue
		}
		if input.Label != "" && manifest.ResultLabel != input.Label {
			continue
		}
		runs = append(runs, run)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].CreatedAt.After(runs[j].CreatedAt) })
	total := len(runs)
	if len(runs) > input.Limit {
		runs = runs[:input.Limit]
	}
	return map[string]any{"runs": runs, "returned": len(runs), "total": total, "truncated": len(runs) < total}, nil
}

func (s *Server) cancelBenchmarkMCP(ctx context.Context, id string) (any, error) {
	job, err := s.getManagedJob(ctx, id)
	if apierrors.IsNotFound(err) {
		return map[string]any{"run_id": id, "canceled": true, "already_absent": true}, nil
	}
	if err != nil {
		return nil, friendlyKubeError(err)
	}
	if status := jobStatus(job); status == "succeeded" || status == "failed" {
		return nil, fmt.Errorf("terminal benchmark runs cannot be canceled")
	}
	propagation := metav1.DeletePropagationBackground
	if err := s.client.BatchV1().Jobs(s.namespace).Delete(ctx, id, metav1.DeleteOptions{PropagationPolicy: &propagation}); err != nil && !apierrors.IsNotFound(err) {
		return nil, friendlyKubeError(err)
	}
	if err := s.deleteServiceIfFingerprint(ctx, id, job.Annotations[requestAnnotation]); err != nil && !apierrors.IsNotFound(err) {
		return nil, friendlyKubeError(err)
	}
	return map[string]any{"run_id": id, "canceled": true, "already_absent": false}, nil
}
