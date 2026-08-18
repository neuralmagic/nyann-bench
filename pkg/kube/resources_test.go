package kube

import "testing"

func TestRenderCoreResourcesUsesIndexedCPUJob(t *testing.T) {
	service, job, err := RenderCoreResources(KubeConfig{
		Name:    "bench-one",
		Workers: 3,
		CPU:     "2",
		Memory:  "4Gi",
	}, "generate", []string{"generate", "--target", "http://model/v1", "--config", `{}`})
	if err != nil {
		t.Fatal(err)
	}
	if service.Name != "bench-one" || job.Name != "bench-one" {
		t.Fatalf("resource names = %q/%q", service.Name, job.Name)
	}
	if job.Spec.CompletionMode == nil || *job.Spec.CompletionMode != "Indexed" {
		t.Fatalf("completion mode = %v", job.Spec.CompletionMode)
	}
	if got := *job.Spec.Completions; got != 3 {
		t.Fatalf("completions = %d", got)
	}
	container := job.Spec.Template.Spec.Containers[0]
	if _, exists := container.Resources.Requests["nvidia.com/gpu"]; exists {
		t.Fatal("benchmark Job must not request GPUs")
	}
	if _, exists := job.Labels["kueue.x-k8s.io/queue-name"]; exists {
		t.Fatal("benchmark Job must not carry a Kueue queue label")
	}
	if job.Spec.Suspend != nil && *job.Spec.Suspend {
		t.Fatal("benchmark Job must not be suspended")
	}
	if job.Spec.Template.Spec.AutomountServiceAccountToken == nil || *job.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Fatal("benchmark Job must not receive a Kubernetes service account token")
	}
}

func TestRenderCoreResourcesCanDisablePrivilegedNetworkTuning(t *testing.T) {
	disabled := false
	_, job, err := RenderCoreResources(KubeConfig{
		Name: "api-run", NetworkTuning: &disabled, Restricted: true,
	}, "generate", []string{"generate", "--target", "http://model/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(job.Spec.Template.Spec.InitContainers) != 0 {
		t.Fatal("network tuning disabled Job contains an init container")
	}
	for _, container := range job.Spec.Template.Spec.Containers {
		if container.SecurityContext != nil && container.SecurityContext.Privileged != nil && *container.SecurityContext.Privileged {
			t.Fatal("network tuning disabled Job contains a privileged container")
		}
		if container.SecurityContext == nil || container.SecurityContext.AllowPrivilegeEscalation == nil || *container.SecurityContext.AllowPrivilegeEscalation {
			t.Fatal("restricted Job must disable privilege escalation")
		}
	}
	if job.Spec.Template.Spec.AutomountServiceAccountToken == nil || *job.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Fatal("Job must disable service account token automount")
	}
}
