package kube

import (
	"os"
	"strings"
	"testing"
)

func TestResolveImage(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"latest", "ghcr.io/neuralmagic/nyann-bench:latest"},
		{"pr-47", "ghcr.io/neuralmagic/nyann-bench:pr-47"},
		{"v1.2.3", "ghcr.io/neuralmagic/nyann-bench:v1.2.3"},
		{"myregistry.io/bench:v1", "myregistry.io/bench:v1"},
		{"ghcr.io/other/image:tag", "ghcr.io/other/image:tag"},
	}
	for _, tt := range tests {
		got := ResolveImage(tt.input)
		if got != tt.want {
			t.Errorf("ResolveImage(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestKubeConfigDefaults(t *testing.T) {
	cfg := KubeConfig{}
	cfg.applyDefaults("eval-gsm8k")

	if cfg.Name != "nyann-eval-gsm8k" {
		t.Errorf("Name = %q, want nyann-eval-gsm8k", cfg.Name)
	}
	// Namespace is intentionally empty when unset — kubectl uses kubeconfig default
	if cfg.Image != "latest" {
		t.Errorf("Image = %q, want latest", cfg.Image)
	}
	if cfg.Arch != "arm64" {
		t.Errorf("Arch = %q, want arm64", cfg.Arch)
	}
	if cfg.Workers != 1 {
		t.Errorf("Workers = %d, want 1", cfg.Workers)
	}
	if cfg.CPU != "4" {
		t.Errorf("CPU = %q, want 4", cfg.CPU)
	}
	if cfg.Memory != "8Gi" {
		t.Errorf("Memory = %q, want 8Gi", cfg.Memory)
	}
}

func TestKubeConfigNamePrefix(t *testing.T) {
	os.Setenv("NYANN_NAME_PREFIX", "tms")
	defer os.Unsetenv("NYANN_NAME_PREFIX")

	cfg := KubeConfig{}
	cfg.applyDefaults("eval-gsm8k")

	if cfg.Name != "tms-eval-gsm8k" {
		t.Errorf("Name = %q, want tms-eval-gsm8k", cfg.Name)
	}
}

func TestKubeConfigExplicitName(t *testing.T) {
	cfg := KubeConfig{Name: "my-job"}
	cfg.applyDefaults("eval-gsm8k")

	if cfg.Name != "my-job" {
		t.Errorf("Name = %q, want my-job", cfg.Name)
	}
}

func TestResolveVolumesPreset(t *testing.T) {
	cfg := KubeConfig{Volume: "lustre"}
	vols := cfg.resolveVolumes()

	if len(vols) != 1 {
		t.Fatalf("expected 1 volume, got %d", len(vols))
	}
	if vols[0].PVC != "lustre-pvc-vllm" {
		t.Errorf("PVC = %q, want lustre-pvc-vllm", vols[0].PVC)
	}
	if vols[0].MountPath != "/mnt/lustre" {
		t.Errorf("MountPath = %q, want /mnt/lustre", vols[0].MountPath)
	}
}

func TestResolveVolumesCustom(t *testing.T) {
	cfg := KubeConfig{
		Volumes: []VolumeSpec{{PVC: "my-pvc", MountPath: "/data"}},
	}
	vols := cfg.resolveVolumes()

	if len(vols) != 1 {
		t.Fatalf("expected 1 volume, got %d", len(vols))
	}
	if vols[0].PVC != "my-pvc" {
		t.Errorf("PVC = %q, want my-pvc", vols[0].PVC)
	}
}

func TestResolveVolumesCustomOverridesPreset(t *testing.T) {
	cfg := KubeConfig{
		Volume:  "lustre",
		Volumes: []VolumeSpec{{PVC: "my-pvc", MountPath: "/data"}},
	}
	vols := cfg.resolveVolumes()

	if len(vols) != 1 {
		t.Fatalf("expected 1 volume (custom overrides preset), got %d", len(vols))
	}
	if vols[0].PVC != "my-pvc" {
		t.Errorf("PVC = %q, want my-pvc (custom should override preset)", vols[0].PVC)
	}
}

func TestResolveVolumesNone(t *testing.T) {
	cfg := KubeConfig{}
	vols := cfg.resolveVolumes()
	if len(vols) != 0 {
		t.Errorf("expected 0 volumes, got %d", len(vols))
	}
}

func TestRenderYAML(t *testing.T) {
	yaml, err := RenderYAML(KubeConfig{
		Volume: "lustre",
	}, "eval-gsm8k", []string{"eval", "gsm8k", "--target", "http://vllm:8000/v1", "--gsm8k-path", "/mnt/lustre/test.jsonl"})
	if err != nil {
		t.Fatal(err)
	}

	checks := []string{
		"name: nyann-eval-gsm8k",
		"ghcr.io/neuralmagic/nyann-bench:latest",
		"completions: 1",
		"parallelism: 1",
		"restartPolicy: Never",
		"backoffLimit: 0",
		`"arm64"`,
		"lustre-pvc-vllm",
		"/mnt/lustre",
		`"eval"`,
		`"gsm8k"`,
		`"--target"`,
		`"http://vllm:8000/v1"`,
	}
	for _, check := range checks {
		if !strings.Contains(yaml, check) {
			t.Errorf("YAML missing %q", check)
		}
	}
}

func TestRenderYAMLNoVolumes(t *testing.T) {
	yaml, err := RenderYAML(KubeConfig{}, "eval-gsm8k", []string{"eval", "gsm8k"})
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(yaml, "volumeMounts") {
		t.Error("YAML should not contain volumeMounts when no volumes specified")
	}
	if strings.Contains(yaml, "persistentVolumeClaim") {
		t.Error("YAML should not contain PVC when no volumes specified")
	}
}

func TestRenderYAMLMultiWorker(t *testing.T) {
	yaml, err := RenderYAML(KubeConfig{Workers: 4}, "generate", []string{"generate"})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(yaml, "completions: 4") {
		t.Error("YAML should have completions: 4")
	}
	if !strings.Contains(yaml, "parallelism: 4") {
		t.Error("YAML should have parallelism: 4")
	}
}

func TestRenderYAMLCustomImage(t *testing.T) {
	yaml, err := RenderYAML(KubeConfig{Image: "pr-47"}, "eval-gsm8k", []string{"eval", "gsm8k"})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(yaml, "ghcr.io/neuralmagic/nyann-bench:pr-47") {
		t.Error("YAML should expand short image tag")
	}
}

func TestFlagsToConfig(t *testing.T) {
	f := Flags{
		Config: `{"namespace": "test-ns", "workers": 2}`,
		Image:  "pr-99",
		Volume: "lustre",
	}
	cfg, err := f.ToConfig()
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Namespace != "test-ns" {
		t.Errorf("Namespace = %q, want test-ns (from JSON)", cfg.Namespace)
	}
	if cfg.Workers != 2 {
		t.Errorf("Workers = %d, want 2 (from JSON)", cfg.Workers)
	}
	if cfg.Image != "pr-99" {
		t.Errorf("Image = %q, want pr-99 (dotted flag overrides)", cfg.Image)
	}
	if cfg.Volume != "lustre" {
		t.Errorf("Volume = %q, want lustre (from dotted flag)", cfg.Volume)
	}
}
