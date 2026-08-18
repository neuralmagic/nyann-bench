package controlapi

import (
	"bufio"
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
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/neuralmagic/nyann-bench/pkg/analysis"
	"github.com/neuralmagic/nyann-bench/pkg/recorder"
	"github.com/neuralmagic/nyann-bench/pkg/statsutil"
)

func (s *Server) listArtifacts(ctx context.Context, run Run) ([]artifactMetadata, error) {
	ctx, cancel := context.WithTimeout(ctx, s.options.ArtifactProcessTimeout)
	defer cancel()
	dir, err := s.safeResultDirectory(run.Results)
	if err != nil {
		return nil, err
	}
	entries, err := readBoundedDirectory(dir, s.options.MaxArtifactFiles)
	if errors.Is(err, os.ErrNotExist) {
		return []artifactMetadata{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading benchmark artifacts: %w", err)
	}
	artifacts := make([]artifactMetadata, 0, len(entries))
	totalBytes := int64(0)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		matches := artifactPattern.FindStringSubmatch(entry.Name())
		if entry.IsDir() || matches == nil {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		file, err := os.Open(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("opening artifact metadata: %w", err)
		}
		hash := sha256.New()
		remaining := s.options.MaxArtifactBytes - totalBytes
		copied, copyErr := io.CopyBuffer(hash, &contextReader{ctx: ctx, reader: io.LimitReader(file, remaining+1)}, make([]byte, 128<<10))
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			return nil, fmt.Errorf("hashing artifact %s", entry.Name())
		}
		if copied > remaining {
			return nil, fmt.Errorf("artifact bytes exceed the configured %d-byte processing limit", s.options.MaxArtifactBytes)
		}
		totalBytes += copied
		kind := "prometheus"
		workerText := ""
		if matches[2] != "" {
			kind, workerText = "requests_jsonl", matches[2]
		} else if matches[3] != "" {
			kind, workerText = "timestamps", matches[3]
		} else if entry.Name() == "run-manifest.json" {
			kind = "run_manifest"
		}
		var worker *int
		if workerText != "" {
			value, _ := strconv.Atoi(workerText)
			worker = &value
		}
		artifacts = append(artifacts, artifactMetadata{Name: entry.Name(), Kind: kind, Worker: worker, SizeBytes: copied, SHA256: hex.EncodeToString(hash.Sum(nil)), ModifiedAt: info.ModTime().UTC()})
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Name < artifacts[j].Name })
	return artifacts, nil
}

func readBoundedDirectory(dir string, limit int) ([]os.DirEntry, error) {
	directory, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	entries, readErr := directory.ReadDir(limit + 1)
	closeErr := directory.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(entries) > limit {
		return nil, fmt.Errorf("artifact directory exceeds the bounded entry limit")
	}
	return entries, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func (s *Server) getBenchmarkReport(ctx context.Context, id string) (*benchmarkReport, error) {
	ctx, cancel := context.WithTimeout(ctx, s.options.ArtifactProcessTimeout)
	defer cancel()
	job, err := s.getManagedJob(ctx, id)
	var run Run
	var target, resultLabel, workstream, image string
	var effectiveScenarioJSON json.RawMessage
	if apierrors.IsNotFound(err) {
		manifest, manifestErr := s.loadRunManifest(id)
		if manifestErr != nil {
			if !errors.Is(manifestErr, os.ErrNotExist) {
				return nil, manifestErr
			}
			return nil, friendlyKubeError(err)
		}
		run = runFromManifest(manifest, "archived")
		target, resultLabel, workstream, image = manifest.Target.Name, manifest.ResultLabel, manifest.VDPWorkstream, manifest.Image
		effectiveScenarioJSON = manifest.EffectiveScenario
	} else if err != nil {
		return nil, friendlyKubeError(err)
	} else {
		run = runFromJob(job)
		target, resultLabel, workstream = job.Annotations[targetAnnotation], job.Annotations[resultLabelAnnotation], job.Annotations[workstreamAnnotation]
		if len(job.Spec.Template.Spec.Containers) > 0 {
			image = job.Spec.Template.Spec.Containers[0].Image
		}
		if value := job.Annotations[scenarioAnnotation]; json.Valid([]byte(value)) {
			effectiveScenarioJSON = json.RawMessage(value)
		}
	}
	artifacts, err := s.listArtifacts(ctx, run)
	if err != nil {
		return nil, err
	}
	report := &benchmarkReport{SchemaVersion: "nyann-bench-report-v1", Run: run, Target: target, ResultLabel: resultLabel, VDPWorkstream: workstream, Image: image, EffectiveScenario: effectiveScenarioJSON, Artifacts: artifacts, Warnings: []string{}}
	requestWorkers := map[int]bool{}
	timestampWorkers := map[int]bool{}
	for _, artifact := range artifacts {
		if artifact.Worker == nil {
			continue
		}
		if artifact.Kind == "requests_jsonl" {
			requestWorkers[*artifact.Worker] = true
		} else if artifact.Kind == "timestamps" {
			timestampWorkers[*artifact.Worker] = true
		}
	}
	for worker := 0; worker < int(run.Workers); worker++ {
		if requestWorkers[worker] && timestampWorkers[worker] {
			report.CompleteWorkers = append(report.CompleteWorkers, worker)
		} else {
			report.MissingWorkers = append(report.MissingWorkers, worker)
		}
	}
	report.WorkerComplete = len(report.MissingWorkers) == 0
	dir, err := s.safeResultDirectory(run.Results)
	if err != nil {
		return nil, err
	}
	if len(requestWorkers) > 0 {
		start, end, timeErr := s.loadMeasurementWindow(ctx, dir, artifacts)
		if timeErr == nil {
			report.MeasurementWindow = &measurementWindow{StartUnixSeconds: start, EndUnixSeconds: end}
		} else {
			report.Warnings = append(report.Warnings, "exact measurement window is unavailable: "+timeErr.Error())
		}
		summary, aggregateWarnings, aggregateErr := s.aggregateRequestArtifacts(ctx, dir, artifacts, start, end, timeErr == nil)
		if aggregateErr != nil {
			return nil, aggregateErr
		}
		report.Summary = summary
		report.Warnings = append(report.Warnings, aggregateWarnings...)
	} else {
		report.Warnings = append(report.Warnings, "no request artifacts are available yet")
	}
	if !report.WorkerComplete {
		report.Warnings = append(report.Warnings, "one or more Indexed Job partitions are incomplete")
	}
	return report, nil
}

func (s *Server) loadMeasurementWindow(ctx context.Context, dir string, artifacts []artifactMetadata) (float64, float64, error) {
	first := true
	var start, end float64
	for _, artifact := range artifacts {
		if artifact.Kind != "timestamps" {
			continue
		}
		if artifact.SizeBytes > int64(s.options.MaxRecordBytes) {
			return 0, 0, fmt.Errorf("timestamp artifact %s exceeds the record-size limit", artifact.Name)
		}
		file, err := os.Open(filepath.Join(dir, artifact.Name))
		if err != nil {
			return 0, 0, err
		}
		var timestamps recorder.Timestamps
		dec := json.NewDecoder(io.LimitReader(&contextReader{ctx: ctx, reader: file}, int64(s.options.MaxRecordBytes)+1))
		decodeErr := dec.Decode(&timestamps)
		closeErr := file.Close()
		if decodeErr != nil || closeErr != nil {
			return 0, 0, fmt.Errorf("reading timestamp artifact %s", artifact.Name)
		}
		if first {
			start, end, first = timestamps.RampupEndTime, timestamps.EndTime, false
		} else {
			if timestamps.RampupEndTime > start {
				start = timestamps.RampupEndTime
			}
			if timestamps.EndTime < end {
				end = timestamps.EndTime
			}
		}
	}
	if first || start <= 0 || end <= start {
		return 0, 0, fmt.Errorf("no valid common worker measurement window")
	}
	return start, end, nil
}

func (s *Server) aggregateRequestArtifacts(ctx context.Context, dir string, artifacts []artifactMetadata, start, end float64, useWindow bool) (*analysis.Summary, []string, error) {
	ttft := newBoundedSample(s.options.MaxLatencySamples)
	itl := newBoundedSample(s.options.MaxLatencySamples)
	e2e := newBoundedSample(s.options.MaxLatencySamples)
	// Store fixed-size identifiers so even a maximum-size JSONL string cannot
	// multiply the configured conversation-map memory bound.
	conversations := make(map[[sha256.Size]byte]int, s.options.MaxLatencySamples)
	conversationOverflow := false
	summary := &analysis.Summary{}
	firstRecord := true
	var minTime, maxTime float64
	var recordsSeen int64
	for _, artifact := range artifacts {
		if artifact.Kind != "requests_jsonl" {
			continue
		}
		file, err := os.Open(filepath.Join(dir, artifact.Name))
		if err != nil {
			return nil, nil, err
		}
		scanner := bufio.NewScanner(&contextReader{ctx: ctx, reader: file})
		scanner.Buffer(make([]byte, 64<<10), s.options.MaxRecordBytes)
		for scanner.Scan() {
			recordsSeen++
			if recordsSeen > s.options.MaxReportRecords {
				_ = file.Close()
				return nil, nil, fmt.Errorf("report exceeds the configured %d-record aggregation limit", s.options.MaxReportRecords)
			}
			var record recorder.Record
			if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
				_ = file.Close()
				return nil, nil, fmt.Errorf("decoding %s record %d: %w", artifact.Name, recordsSeen, err)
			}
			if useWindow && (record.StartTime < start || record.EndTime > end) {
				continue
			}
			summary.TotalRequests++
			if record.Status == "ok" {
				summary.SuccessRequests++
				summary.TotalOutputTokens += record.OutputTokens
				summary.TotalPromptTokens += record.PromptTokens
				ttft.Add(record.TTFT)
				e2e.Add(record.TotalLatencyMs)
				for _, value := range record.ITLs {
					itl.Add(value)
				}
			} else {
				summary.ErrorRequests++
			}
			if record.EvalCorrect != nil {
				summary.EvalTotal++
				if *record.EvalCorrect {
					summary.EvalCorrect++
				} else {
					summary.EvalIncorrect++
				}
			}
			conversationID := sha256.Sum256([]byte(record.ConversationID))
			if count, ok := conversations[conversationID]; ok {
				conversations[conversationID] = count + 1
			} else if len(conversations) < s.options.MaxLatencySamples {
				conversations[conversationID] = 1
			} else {
				conversationOverflow = true
			}
			if firstRecord {
				minTime, maxTime, firstRecord = record.StartTime, record.EndTime, false
			} else {
				if record.StartTime < minTime {
					minTime = record.StartTime
				}
				if record.EndTime > maxTime {
					maxTime = record.EndTime
				}
			}
		}
		scanErr := scanner.Err()
		closeErr := file.Close()
		if scanErr != nil {
			return nil, nil, fmt.Errorf("scanning %s: %w", artifact.Name, scanErr)
		}
		if closeErr != nil {
			return nil, nil, closeErr
		}
	}
	if useWindow {
		summary.TotalDurationS = end - start
	} else if !firstRecord {
		summary.TotalDurationS = maxTime - minTime
	}
	if summary.TotalDurationS > 0 {
		summary.RequestsPerSec = float64(summary.SuccessRequests) / summary.TotalDurationS
		summary.OutputTokensPerS = float64(summary.TotalOutputTokens) / summary.TotalDurationS
	}
	if summary.EvalTotal > 0 {
		summary.EvalAccuracy = float64(summary.EvalCorrect) / float64(summary.EvalTotal)
	}
	summary.TTFTMs, summary.ITLMs, summary.E2EMs = ttft.Stats(), itl.Stats(), e2e.Stats()
	summary.Conversations = len(conversations)
	turns := newBoundedSample(s.options.MaxLatencySamples)
	for _, count := range conversations {
		turns.Add(float64(count))
	}
	summary.TurnsPerConv = turns.Stats()
	warnings := []string{}
	if ttft.Sampled() || itl.Sampled() || e2e.Sampled() {
		warnings = append(warnings, fmt.Sprintf("latency statistics use a deterministic bounded sample of at most %d values", s.options.MaxLatencySamples))
	}
	if conversationOverflow {
		warnings = append(warnings, fmt.Sprintf("conversation statistics are bounded to %d distinct IDs", s.options.MaxLatencySamples))
	}
	return summary, warnings, nil
}

type boundedSample struct {
	values []float64
	seen   uint64
	limit  uint64
}

func newBoundedSample(limit int) *boundedSample {
	return &boundedSample{values: make([]float64, 0, limit), limit: uint64(limit)}
}

func (s *boundedSample) Add(value float64) {
	s.seen++
	if uint64(len(s.values)) < s.limit {
		s.values = append(s.values, value)
		return
	}
	// Deterministic reservoir sampling keeps restart reports reproducible.
	hash := s.seen + 0x9e3779b97f4a7c15
	hash = (hash ^ (hash >> 30)) * 0xbf58476d1ce4e5b9
	hash = (hash ^ (hash >> 27)) * 0x94d049bb133111eb
	hash ^= hash >> 31
	index := hash % s.seen
	if index < s.limit {
		s.values[index] = value
	}
}

func (s *boundedSample) Sampled() bool { return s.seen > s.limit }

func (s *boundedSample) Stats() analysis.LatencyStats {
	if len(s.values) == 0 {
		return analysis.LatencyStats{}
	}
	values := append([]float64(nil), s.values...)
	sort.Float64s(values)
	sum := 0.0
	for _, value := range values {
		sum += value
	}
	return analysis.LatencyStats{Mean: sum / float64(len(values)), P10: statsutil.Percentile(values, 0.10), P50: statsutil.Percentile(values, 0.50), P90: statsutil.Percentile(values, 0.90), P95: statsutil.Percentile(values, 0.95), P99: statsutil.Percentile(values, 0.99), Min: values[0], Max: values[len(values)-1]}
}

func (s *Server) safeResultDirectory(results ResultMetadata) (string, error) {
	if !results.Durable || results.Path == "" || s.options.ResultRoot == "" {
		return "", fmt.Errorf("durable result artifacts are unavailable")
	}
	root, err := filepath.Abs(s.options.ResultRoot)
	if err != nil {
		return "", err
	}
	dir, err := filepath.Abs(results.Path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return "", fmt.Errorf("result path escapes the configured result root")
	}
	if _, statErr := os.Stat(dir); statErr == nil {
		realRoot, rootErr := filepath.EvalSymlinks(root)
		realDir, dirErr := filepath.EvalSymlinks(dir)
		if rootErr != nil || dirErr != nil {
			return "", fmt.Errorf("resolving durable result path")
		}
		realRel, relErr := filepath.Rel(realRoot, realDir)
		if relErr != nil || realRel == "." || realRel == ".." || strings.HasPrefix(realRel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("result path resolves outside the configured result root")
		}
	}
	return dir, nil
}

func benchmarkDetails(job *batchv1.Job) map[string]any {
	details := map[string]any{
		"run":          runFromJob(job),
		"target":       job.Annotations[targetAnnotation],
		"result_label": job.Annotations[resultLabelAnnotation],
		"platform":     job.Annotations[platformAnnotation],
	}
	if value := job.Annotations[scenarioAnnotation]; json.Valid([]byte(value)) {
		details["effective_scenario"] = json.RawMessage(value)
	}
	if value := job.Annotations[workstreamAnnotation]; value != "" {
		details["vdp_workstream"] = value
	}
	if len(job.Spec.Template.Spec.Containers) > 0 {
		details["image"] = job.Spec.Template.Spec.Containers[0].Image
	}
	return details
}

func benchmarkDetailsFromManifest(manifest durableRunManifest) map[string]any {
	details := map[string]any{
		"run":                runFromManifest(manifest, "archived"),
		"target":             manifest.Target.Name,
		"result_label":       manifest.ResultLabel,
		"platform":           manifest.Resources.Platform,
		"effective_scenario": manifest.EffectiveScenario,
		"image":              manifest.Image,
	}
	if manifest.VDPWorkstream != "" {
		details["vdp_workstream"] = manifest.VDPWorkstream
	}
	return details
}

func decodeBenchmarkRef(raw json.RawMessage) (benchmarkRef, error) {
	var ref benchmarkRef
	if err := decodeStrict(raw, &ref); err != nil {
		return ref, err
	}
	if len(validation.IsDNS1123Subdomain(ref.RunID)) > 0 || len(ref.RunID) > 63 {
		return ref, fmt.Errorf("run_id is invalid")
	}
	return ref, nil
}
