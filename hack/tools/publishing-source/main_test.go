package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestRequireLegacySource(t *testing.T) {
	for _, publishing := range []bool{true, false} {
		t.Run("When publishing availability differs, it should require legacy services only without the feature", func(t *testing.T) {
			properties := map[string]any{"services": map[string]any{"type": "array"}}
			if publishing {
				properties["publishing"] = map[string]any{"type": "object"}
			}
			schema := map[string]any{"properties": properties}
			version := map[string]any{"schema": map[string]any{"openAPIV3Schema": map[string]any{"properties": map[string]any{"spec": schema}}}}
			manifest := map[string]any{"spec": map[string]any{"versions": []any{version}}}
			if err := requireLegacySource(manifest); err != nil {
				t.Fatal(err)
			}
			versions, _, _ := unstructured.NestedSlice(manifest, "spec", "versions")
			updated := versions[0].(map[string]any)
			required, _, err := unstructured.NestedStringSlice(updated, "schema", "openAPIV3Schema", "properties", "spec", "required")
			if err != nil {
				t.Fatal(err)
			}
			if publishing && len(required) != 0 || !publishing && (len(required) != 1 || required[0] != "services") {
				t.Fatalf("unexpected required services: %v", required)
			}
		})
	}
	if err := requireLegacySource(map[string]any{}); err == nil {
		t.Fatal("missing schema must fail generation")
	}
}

func TestRun(t *testing.T) {
	t.Run("When stable CRDs omit publishing, it should generate legacy source requirements without changing unrelated files", func(t *testing.T) {
		directory := t.TempDir()
		manifest := "apiVersion: apiextensions.k8s.io/v1\nspec:\n  versions:\n  - schema:\n      openAPIV3Schema:\n        properties:\n          spec:\n            properties:\n              services:\n                type: array\n"
		path := filepath.Join(directory, "hostedclusters-Hypershift-Default.crd.yaml")
		if err := os.WriteFile(path, []byte(manifest), 0600); err != nil {
			t.Fatal(err)
		}
		unrelated := filepath.Join(directory, "nodepools-Hypershift-Default.crd.yaml")
		if err := os.WriteFile(unrelated, []byte("unchanged"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := run(directory); err != nil {
			t.Fatal(err)
		}
		generated, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(generated), "non-empty spec.services is required") {
			t.Fatal("legacy source requirement was not generated")
		}
		unchanged, err := os.ReadFile(unrelated)
		if err != nil || string(unchanged) != "unchanged" {
			t.Fatalf("unrelated manifest changed: %v", err)
		}
	})
	t.Run("When the directory is absent, it should fail generation", func(t *testing.T) {
		if err := run(filepath.Join(t.TempDir(), "absent")); err == nil {
			t.Fatal("missing manifest directory must fail generation")
		}
	})
	t.Run("When a CRD is malformed, it should fail rather than emit an incomplete schema", func(t *testing.T) {
		directory := t.TempDir()
		if err := os.WriteFile(filepath.Join(directory, "hostedclusters-Hypershift-Default.crd.yaml"), []byte("apiVersion: apiextensions.k8s.io/v1"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := run(directory); err == nil {
			t.Fatal("malformed CRD must fail generation")
		}
	})
}

func TestCanonicaliseSpecValidations(t *testing.T) {
	newManifest := func(rules ...string) map[string]any {
		validations := make([]any, 0, len(rules))
		for _, rule := range rules {
			validations = append(validations, map[string]any{"rule": rule, "message": "message for " + rule})
		}
		spec := map[string]any{"x-kubernetes-validations": validations}
		version := map[string]any{"schema": map[string]any{"openAPIV3Schema": map[string]any{"properties": map[string]any{"spec": spec}}}}
		return map[string]any{"spec": map[string]any{"versions": []any{version}}}
	}
	readRules := func(t *testing.T, manifest map[string]any) []string {
		t.Helper()
		versions, _, _ := unstructured.NestedSlice(manifest, "spec", "versions")
		validations, found, err := unstructured.NestedSlice(versions[0].(map[string]any), "schema", "openAPIV3Schema", "properties", "spec", "x-kubernetes-validations")
		if err != nil || !found {
			t.Fatalf("read validations: found=%t: %v", found, err)
		}
		rules := make([]string, 0, len(validations))
		for _, validation := range validations {
			rule, _, _ := unstructured.NestedString(validation.(map[string]any), "rule")
			rules = append(rules, rule)
		}
		return rules
	}

	t.Run("When the generator emits rules in different orders, it should produce one canonical order", func(t *testing.T) {
		first := newManifest("c", "a", "b")
		second := newManifest("b", "c", "a")
		for _, manifest := range []map[string]any{first, second} {
			if err := canonicaliseSpecValidations(manifest); err != nil {
				t.Fatal(err)
			}
		}
		want := []string{"a", "b", "c"}
		for name, got := range map[string][]string{"first": readRules(t, first), "second": readRules(t, second)} {
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("%s order is %v, want %v", name, got, want)
			}
		}
	})

	t.Run("When it runs twice, it should keep the messages attached to their rules", func(t *testing.T) {
		manifest := newManifest("b", "a")
		for range 2 {
			if err := canonicaliseSpecValidations(manifest); err != nil {
				t.Fatal(err)
			}
		}
		versions, _, _ := unstructured.NestedSlice(manifest, "spec", "versions")
		validations, _, _ := unstructured.NestedSlice(versions[0].(map[string]any), "schema", "openAPIV3Schema", "properties", "spec", "x-kubernetes-validations")
		for _, validation := range validations {
			rule, _, _ := unstructured.NestedString(validation.(map[string]any), "rule")
			message, _, _ := unstructured.NestedString(validation.(map[string]any), "message")
			if message != "message for "+rule {
				t.Fatalf("rule %q carries message %q", rule, message)
			}
		}
	})

	t.Run("When a validation has no rule, it should fail generation", func(t *testing.T) {
		manifest := newManifest("a", "b")
		versions, _, _ := unstructured.NestedSlice(manifest, "spec", "versions")
		validations, _, _ := unstructured.NestedSlice(versions[0].(map[string]any), "schema", "openAPIV3Schema", "properties", "spec", "x-kubernetes-validations")
		delete(validations[1].(map[string]any), "rule")
		_ = unstructured.SetNestedSlice(versions[0].(map[string]any), validations, "schema", "openAPIV3Schema", "properties", "spec", "x-kubernetes-validations")
		_ = unstructured.SetNestedSlice(manifest, versions, "spec", "versions")
		if err := canonicaliseSpecValidations(manifest); err == nil {
			t.Fatal("a validation without a rule must fail generation")
		}
	})
}
