package kube

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"text/template"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const defaultRegistry = "ghcr.io/neuralmagic/nyann-bench"

var volumePresets = map[string]VolumeSpec{
	"lustre": {PVC: "lustre-pvc-vllm", MountPath: "/mnt/lustre"},
}

type KubeConfig struct {
	Name      string       `json:"name,omitempty"`
	Context   string       `json:"context,omitempty"`
	Namespace string       `json:"namespace,omitempty"`
	Platform  string       `json:"platform,omitempty"`
	Image     string       `json:"image,omitempty"`
	Arch      string       `json:"arch,omitempty"`
	Workers   int          `json:"workers,omitempty"`
	Volume    string       `json:"volume,omitempty"`
	Volumes   []VolumeSpec `json:"volumes,omitempty"`
	CPU       string       `json:"cpu,omitempty"`
	Memory    string       `json:"memory,omitempty"`
}

type VolumeSpec struct {
	PVC       string `json:"pvc"`
	MountPath string `json:"mountPath"`
}

// Flags holds the raw flag values registered on a cobra command.
type Flags struct {
	Enabled   bool
	Config    string
	Name      string
	Context   string
	Namespace string
	Platform  string
	Image     string
	Arch      string
	Volume    string
	CPU       string
	Memory    string
}

// RegisterFlags adds --kube and --kube.* flags to a cobra command.
func RegisterFlags(cmd *cobra.Command, f *Flags) {
	cmd.Flags().BoolVar(&f.Enabled, "kube", false, "Deploy to Kubernetes instead of running locally")
	cmd.Flags().StringVar(&f.Config, "kube.config", "", "Kubernetes deploy config as JSON")
	cmd.Flags().StringVar(&f.Name, "kube.name", "", "Job name (auto-generated if omitted)")
	cmd.Flags().StringVar(&f.Context, "kube.context", "", "Exact kube context to use")
	cmd.Flags().StringVar(&f.Namespace, "kube.namespace", "", "Kubernetes namespace (default: from kubeconfig)")
	cmd.Flags().StringVar(&f.Platform, "kube.platform", "", "Cluster platform: kubernetes or openshift")
	cmd.Flags().StringVar(&f.Image, "kube.image", "", "Container image tag or full ref (default: latest)")
	cmd.Flags().StringVar(&f.Arch, "kube.arch", "", "Node architecture (default: arm64)")
	cmd.Flags().StringVar(&f.Volume, "kube.volume", "", "Volume preset (e.g. lustre)")
	cmd.Flags().StringVar(&f.CPU, "kube.cpu", "", "CPU limit (default: 4)")
	cmd.Flags().StringVar(&f.Memory, "kube.memory", "", "Memory limit (default: 8Gi)")
}

// IsEnabled returns true if any --kube* flag was set.
func (f *Flags) IsEnabled(cmd *cobra.Command) bool {
	for _, name := range []string{"kube", "kube.config", "kube.name", "kube.context", "kube.namespace", "kube.platform", "kube.image", "kube.arch", "kube.volume", "kube.cpu", "kube.memory"} {
		if cmd.Flags().Changed(name) {
			return true
		}
	}
	return false
}

// ToConfig parses the JSON from --kube.config (if any) and overlays --kube.* flags.
func (f *Flags) ToConfig() (KubeConfig, error) {
	var cfg KubeConfig
	if f.Config != "" {
		if err := json.Unmarshal([]byte(f.Config), &cfg); err != nil {
			return cfg, fmt.Errorf("parsing --kube.config JSON: %w", err)
		}
	}
	if f.Name != "" {
		cfg.Name = f.Name
	}
	if f.Context != "" {
		cfg.Context = f.Context
	}
	if f.Namespace != "" {
		cfg.Namespace = f.Namespace
	}
	if f.Platform != "" {
		cfg.Platform = f.Platform
	}
	if f.Image != "" {
		cfg.Image = f.Image
	}
	if f.Arch != "" {
		cfg.Arch = f.Arch
	}
	if f.Volume != "" {
		cfg.Volume = f.Volume
	}
	if f.CPU != "" {
		cfg.CPU = f.CPU
	}
	if f.Memory != "" {
		cfg.Memory = f.Memory
	}
	if cfg.Platform != "" && cfg.Platform != "kubernetes" && cfg.Platform != "openshift" {
		return cfg, fmt.Errorf("unsupported kube platform %q (options: kubernetes, openshift)", cfg.Platform)
	}
	return cfg, nil
}

func (cfg *KubeConfig) applyDefaults(defaultName string) {
	if cfg.Name == "" {
		if prefix, ok := os.LookupEnv("NYANN_NAME_PREFIX"); ok {
			cfg.Name = prefix + "-" + defaultName
		} else {
			cfg.Name = "nyann-" + defaultName
		}
	}
	// Namespace intentionally left empty if not set — kubectl will use the kubeconfig default
	if cfg.Image == "" {
		cfg.Image = "latest"
	}
	if cfg.Platform == "" {
		cfg.Platform = "kubernetes"
	}
	if cfg.Arch == "" {
		cfg.Arch = "arm64"
	}
	if cfg.Workers == 0 {
		cfg.Workers = 1
	}
	if cfg.CPU == "" {
		cfg.CPU = "4"
	}
	if cfg.Memory == "" {
		cfg.Memory = "8Gi"
	}
}

// ResolveImage expands a short tag to a full image ref.
func ResolveImage(image string) string {
	if strings.Contains(image, "/") {
		return image
	}
	return defaultRegistry + ":" + image
}

func (cfg *KubeConfig) resolveVolumes() []VolumeSpec {
	if len(cfg.Volumes) > 0 {
		return cfg.Volumes
	}
	if cfg.Volume != "" {
		if v, ok := volumePresets[cfg.Volume]; ok {
			return []VolumeSpec{v}
		}
		slog.Warn("Unknown volume preset, ignoring", "volume", cfg.Volume)
	}
	return nil
}

type deployParams struct {
	Name      string
	Namespace string
	Image     string
	Arch      string
	Workers   int
	CPU       string
	Memory    string
	OpenShift bool
	Args      []string
	Volumes   []VolumeSpec
}

var funcMap = template.FuncMap{
	"yamlEscape": func(s string) string {
		s = strings.ReplaceAll(s, `\`, `\\`)
		s = strings.ReplaceAll(s, `"`, `\"`)
		return s
	},
}

var jobTemplate = template.Must(template.New("job").Funcs(funcMap).Parse(`apiVersion: v1
kind: Service
metadata:
  name: {{ .Name }}
spec:
  clusterIP: None
  selector:
    job-name: {{ .Name }}
---
apiVersion: batch/v1
kind: Job
metadata:
  name: {{ .Name }}
  labels:
    app: {{ .Name }}
spec:
  backoffLimit: 0
  completions: {{ .Workers }}
  parallelism: {{ .Workers }}
  completionMode: Indexed
  template:
    metadata:
      labels:
        app: {{ .Name }}
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "9090"
    spec:
      restartPolicy: Never
      subdomain: {{ .Name }}
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
            - matchExpressions:
              - key: kubernetes.io/arch
                operator: In
                values: ["{{ .Arch }}"]
{{ if .OpenShift }}
      securityContext:
        seccompProfile:
          type: RuntimeDefault
{{ else }}
      initContainers:
        - name: sysctl
          image: public.ecr.aws/docker/library/busybox:1.36
          command: ["sh", "-c"]
          args:
            - |
              sysctl -w net.ipv4.ip_local_port_range='1024 65535'
              sysctl -w net.ipv4.tcp_tw_reuse=1
          securityContext:
            privileged: true
{{ end }}
      containers:
        - name: nyann-bench
          image: {{ .Image }}
          imagePullPolicy: Always
          resources:
            requests:
              cpu: "{{ .CPU }}"
              memory: "{{ .Memory }}"
            limits:
              cpu: "{{ .CPU }}"
              memory: "{{ .Memory }}"
{{ if .OpenShift }}
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
            runAsNonRoot: true
{{ end }}
          ports:
            - name: metrics
              containerPort: 9090
              protocol: TCP
            - name: barrier
              containerPort: 8080
              protocol: TCP
          env:
            - name: BARRIER_ADDR
              value: "{{ .Name }}-0.{{ .Name }}"
          args:
{{- range .Args }}
            - "{{ . | yamlEscape }}"
{{- end }}
{{- if .Volumes }}
          volumeMounts:
{{- range .Volumes }}
            - name: {{ .VolumeName }}
              mountPath: {{ .MountPath }}
{{- end }}
      volumes:
{{- range .Volumes }}
        - name: {{ .VolumeName }}
          persistentVolumeClaim:
            claimName: {{ .PVC }}
{{- end }}
{{- end }}
{{ if .OpenShift }}
---
apiVersion: monitoring.coreos.com/v1
kind: PodMonitor
metadata:
  name: {{ .Name }}
spec:
  selector:
    matchLabels:
      app: {{ .Name }}
  podMetricsEndpoints:
    - port: metrics
      path: /metrics
      interval: 15s
  podTargetLabels:
    - app
    - job-name
{{ end }}
`))

type templateVolume struct {
	VolumeName string
	PVC        string
	MountPath  string
}

type templateParams struct {
	Name      string
	Arch      string
	Image     string
	Workers   int
	CPU       string
	Memory    string
	OpenShift bool
	Args      []string
	Volumes   []templateVolume
}

func renderYAML(p deployParams) (string, error) {
	var vols []templateVolume
	for i, v := range p.Volumes {
		name := fmt.Sprintf("vol-%d", i)
		if v.PVC == "lustre-pvc-vllm" {
			name = "lustre"
		}
		vols = append(vols, templateVolume{
			VolumeName: name,
			PVC:        v.PVC,
			MountPath:  v.MountPath,
		})
	}
	tp := templateParams{
		Name:      p.Name,
		Arch:      p.Arch,
		Image:     p.Image,
		Workers:   p.Workers,
		CPU:       p.CPU,
		Memory:    p.Memory,
		OpenShift: p.OpenShift,
		Args:      p.Args,
		Volumes:   vols,
	}
	var buf bytes.Buffer
	if err := jobTemplate.Execute(&buf, tp); err != nil {
		return "", fmt.Errorf("rendering Job YAML: %w", err)
	}
	return buf.String(), nil
}

func kubectl(context, namespace string, args ...string) error {
	var fullArgs []string
	if context != "" {
		fullArgs = append(fullArgs, "--context", context)
	}
	if namespace != "" {
		fullArgs = append(fullArgs, "-n", namespace)
	}
	fullArgs = append(fullArgs, args...)
	cmd := exec.Command("kubectl", fullArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Deploy generates and applies a Kubernetes Job for the given command args.
// defaultName is used for auto-naming (e.g. "eval-gsm8k").
func Deploy(cfg KubeConfig, defaultName string, args []string) error {
	cfg.applyDefaults(defaultName)

	image := ResolveImage(cfg.Image)
	volumes := cfg.resolveVolumes()

	slog.Info("Deploying to Kubernetes",
		"name", cfg.Name,
		"namespace", cfg.Namespace,
		"image", image,
		"workers", cfg.Workers,
		"volumes", len(volumes))

	yaml, err := renderYAML(deployParams{
		Name:      cfg.Name,
		Namespace: cfg.Namespace,
		Image:     image,
		Arch:      cfg.Arch,
		Workers:   cfg.Workers,
		CPU:       cfg.CPU,
		Memory:    cfg.Memory,
		OpenShift: cfg.Platform == "openshift",
		Args:      args,
		Volumes:   volumes,
	})
	if err != nil {
		return err
	}

	baseArgs := []string{}
	if cfg.Context != "" {
		baseArgs = append(baseArgs, "--context", cfg.Context)
	}
	if cfg.Namespace != "" {
		baseArgs = append(baseArgs, "-n", cfg.Namespace)
	}
	dryRunArgs := append(append([]string{}, baseArgs...), "apply", "--dry-run=server", "-f", "-")
	dryRunCmd := exec.Command("kubectl", dryRunArgs...)
	dryRunCmd.Stdin = strings.NewReader(yaml)
	dryRunCmd.Stdout = os.Stdout
	dryRunCmd.Stderr = os.Stderr
	if err := dryRunCmd.Run(); err != nil {
		return fmt.Errorf("kubectl server-side dry run: %w", err)
	}

	// Only replace an existing deployment after the new manifest validates.
	_ = kubectl(cfg.Context, cfg.Namespace, "delete", "job", cfg.Name, "--ignore-not-found=true")
	_ = kubectl(cfg.Context, cfg.Namespace, "delete", "service", cfg.Name, "--ignore-not-found=true")
	if cfg.Platform == "openshift" {
		_ = kubectl(cfg.Context, cfg.Namespace, "delete", "podmonitor", cfg.Name, "--ignore-not-found=true")
	}

	applyArgs := append(baseArgs, "apply", "-f", "-")
	applyCmd := exec.Command("kubectl", applyArgs...)
	applyCmd.Stdin = strings.NewReader(yaml)
	applyCmd.Stdout = os.Stdout
	applyCmd.Stderr = os.Stderr
	if err := applyCmd.Run(); err != nil {
		return fmt.Errorf("kubectl apply: %w", err)
	}

	var ctxFlag, nsFlag string
	if cfg.Context != "" {
		ctxFlag = "--context " + cfg.Context + " "
	}
	if cfg.Namespace != "" {
		nsFlag = "-n " + cfg.Namespace + " "
	}
	fmt.Fprintf(os.Stderr, "\nDeployed. Follow with:\n  kubectl %s%slogs -l app=%s -c nyann-bench --tail=50 -f\n", ctxFlag, nsFlag, cfg.Name)
	return nil
}

// CollectArgs reconstructs the CLI args from a cobra command, skipping --kube* flags.
// prefix is prepended (e.g. ["eval", "gsm8k"]).
func CollectArgs(cmd *cobra.Command, prefix []string) []string {
	args := append([]string{}, prefix...)
	cmd.Flags().Visit(func(f *pflag.Flag) {
		if strings.HasPrefix(f.Name, "kube") {
			return
		}
		args = append(args, "--"+f.Name, f.Value.String())
	})
	return args
}

// RenderYAML generates the Job + Service YAML without applying it (for testing/dry-run).
func RenderYAML(cfg KubeConfig, defaultName string, args []string) (string, error) {
	cfg.applyDefaults(defaultName)
	image := ResolveImage(cfg.Image)
	volumes := cfg.resolveVolumes()
	return renderYAML(deployParams{
		Name:      cfg.Name,
		Namespace: cfg.Namespace,
		Image:     image,
		Arch:      cfg.Arch,
		Workers:   cfg.Workers,
		CPU:       cfg.CPU,
		Memory:    cfg.Memory,
		OpenShift: cfg.Platform == "openshift",
		Args:      args,
		Volumes:   volumes,
	})
}
