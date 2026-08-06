package kube

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// RenderCoreResources builds the same Service and Indexed Job used by the CLI
// deploy path. Control-plane callers should use this instead of maintaining a
// second Kubernetes representation of nyann-bench runs.
func RenderCoreResources(cfg KubeConfig, defaultName string, args []string) (*corev1.Service, *batchv1.Job, error) {
	manifest, err := RenderYAML(cfg, defaultName, args)
	if err != nil {
		return nil, nil, err
	}

	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewBufferString(manifest), 4096)
	var service *corev1.Service
	var job *batchv1.Job
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			if err == io.EOF {
				break
			}
			return nil, nil, fmt.Errorf("decoding rendered manifest: %w", err)
		}
		if len(raw) == 0 {
			continue
		}
		var meta struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			return nil, nil, fmt.Errorf("decoding rendered object metadata: %w", err)
		}
		switch meta.Kind {
		case "Service":
			service = &corev1.Service{}
			if err := json.Unmarshal(raw, service); err != nil {
				return nil, nil, fmt.Errorf("decoding rendered Service: %w", err)
			}
		case "Job":
			job = &batchv1.Job{}
			if err := json.Unmarshal(raw, job); err != nil {
				return nil, nil, fmt.Errorf("decoding rendered Job: %w", err)
			}
		}
	}
	if service == nil || job == nil {
		return nil, nil, fmt.Errorf("rendered manifest did not contain both a Service and Job")
	}
	return service, job, nil
}
