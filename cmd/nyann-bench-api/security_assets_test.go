package main

import (
	"os"
	"strings"
	"testing"
)

func TestContainerAndDeploymentUseNumericNonRootIdentity(t *testing.T) {
	dockerfile, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dockerfile), "USER 65532:65532") {
		t.Fatal("scratch image must declare a numeric non-root USER")
	}
	manifest, err := os.ReadFile("../../deploy/api.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(manifest)
	for _, required := range []string{
		"runAsNonRoot: true", "runAsUser: 65532", "runAsGroup: 65532",
		"secretName: nyann-bench-api-auth", "@sha256:REPLACE_WITH_IMAGE_DIGEST",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("deployment missing %q", required)
		}
	}
	if strings.Contains(text, ":latest") {
		t.Fatal("deployment must not use a mutable :latest image")
	}
}
