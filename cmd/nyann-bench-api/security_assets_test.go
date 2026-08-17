package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
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

func TestDeploymentRBACIsNamespaceScopedAndMinimal(t *testing.T) {
	manifest, err := os.ReadFile("../../deploy/api.yaml")
	if err != nil {
		t.Fatal(err)
	}
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	var role *rbacv1.Role
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatal(err)
		}
		var meta struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			t.Fatal(err)
		}
		if meta.Kind == "Role" {
			role = &rbacv1.Role{}
			if err := json.Unmarshal(raw, role); err != nil {
				t.Fatal(err)
			}
		}
	}
	if role == nil {
		t.Fatal("namespace-scoped Role is missing")
	}
	want := []rbacv1.PolicyRule{
		{APIGroups: []string{"batch"}, Resources: []string{"jobs"}, Verbs: []string{"create", "delete", "get", "list"}},
		{APIGroups: []string{""}, Resources: []string{"services"}, Verbs: []string{"create", "delete", "get", "update"}},
		{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}},
		{APIGroups: []string{""}, Resources: []string{"pods/log"}, Verbs: []string{"get"}},
	}
	if !reflect.DeepEqual(role.Rules, want) {
		t.Fatalf("RBAC grew beyond the reviewed minimum:\n got: %#v\nwant: %#v", role.Rules, want)
	}
	if _, err := os.Stat("../../deploy/networkpolicy.example.yaml"); err != nil {
		t.Fatalf("NetworkPolicy guidance is missing: %v", err)
	}
}
