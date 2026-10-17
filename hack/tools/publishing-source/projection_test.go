package main

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
)

func projectionSchemaFixture(kind string, enabled bool) map[string]any {
	leaf := func(rule string) map[string]any {
		return map[string]any{"type": "string", "x-kubernetes-validations": []any{map[string]any{"rule": rule, "message": "endpoint validation"}}}
	}
	strategyProperties := map[string]any{}
	for _, arm := range []string{"nodePort", "route", "loadBalancer"} {
		field := "hostname"
		if arm == "nodePort" {
			field = "address"
		}
		strategyProperties[arm] = map[string]any{"properties": map[string]any{field: leaf("self == 'legacy'")}}
	}
	properties := map[string]any{"services": map[string]any{"items": map[string]any{"properties": map[string]any{"servicePublishingStrategy": map[string]any{"properties": strategyProperties}}}}}
	if enabled {
		properties["publishing"] = map[string]any{"properties": map[string]any{
			"nodePort":                   map[string]any{"properties": map[string]any{"address": leaf("self == 'new'")}},
			"dedicatedAPIServerEndpoint": map[string]any{"properties": map[string]any{"services": map[string]any{"items": map[string]any{"properties": map[string]any{"hostname": leaf("self == 'new'")}}}}},
		}}
	}
	return map[string]any{"spec": map[string]any{"names": map[string]any{"kind": kind}, "versions": []any{map[string]any{"schema": map[string]any{"openAPIV3Schema": map[string]any{"properties": map[string]any{"spec": map[string]any{"properties": properties}}}}}}}}
}

func TestAllowPublishingProjection(t *testing.T) {
	for _, kind := range []string{"HostedCluster", "HostedControlPlane"} {
		for _, enabled := range []bool{false, true} {
			t.Run("When "+kind+" publishing schema enabled is "+strconv.FormatBool(enabled)+", it should expand only enabled HCP projections", func(t *testing.T) {
				manifest := projectionSchemaFixture(kind, enabled)
				before := projectionSchemaFixture(kind, enabled)
				if err := allowPublishingProjection(manifest); err != nil {
					t.Fatal(err)
				}
				if kind != "HostedControlPlane" || !enabled {
					if !reflect.DeepEqual(before, manifest) {
						t.Fatal("unrelated or disabled schema changed")
					}
					return
				}
				versions, _, _ := unstructured.NestedSlice(manifest, "spec", "versions")
				schema, _, _ := unstructured.NestedMap(versions[0].(map[string]any), "schema", "openAPIV3Schema", "properties", "spec")
				parentRules, _, _ := unstructured.NestedSlice(schema, "x-kubernetes-validations")
				if len(parentRules) != 3 {
					t.Fatalf("expected all three legacy endpoint guards, got %d", len(parentRules))
				}
				environment, err := cel.NewEnv(cel.Variable("self", cel.DynType), cel.Variable("oldSelf", cel.DynType), cel.OptionalTypes())
				if err != nil {
					t.Fatal(err)
				}
				for _, rule := range parentRules {
					if optionalOldSelf, found, err := unstructured.NestedBool(rule.(map[string]any), "optionalOldSelf"); err != nil || !found || !optionalOldSelf {
						t.Fatalf("lifted guards must set optionalOldSelf so they still apply on create: found=%t: %v", found, err)
					}
				}
				for _, arm := range []string{"nodePort", "route", "loadBalancer"} {
					field := "hostname"
					if arm == "nodePort" {
						field = "address"
					}
					leafRules, _, _ := unstructured.NestedSlice(schema, "properties", "services", "items", "properties", "servicePublishingStrategy", "properties", arm, "properties", field, "x-kubernetes-validations")
					leafRule := leafRules[0].(map[string]any)["rule"].(string)
					leafProgram := compilePublishingRule(t, environment, leafRule)
					var parentRule string
					for _, rule := range parentRules {
						candidate := rule.(map[string]any)["rule"].(string)
						if strings.Contains(candidate, "servicePublishingStrategy."+arm) {
							parentRule = candidate
						}
					}
					parentProgram := compilePublishingRule(t, environment, parentRule)
					for _, value := range []string{"legacy", "new", "invalid"} {
						if evaluatePublishingRule(t, leafProgram, map[string]any{"self": value}) != (value != "invalid") {
							t.Fatalf("leaf must accept only old or publishing grammar: %s", value)
						}
						for _, publishing := range []bool{true, false} {
							services := []any{map[string]any{"servicePublishingStrategy": map[string]any{arm: map[string]any{field: value}}}}
							self := map[string]any{"services": services}
							if publishing {
								self["publishing"] = map[string]any{}
							}
							guarded := publishing || value == "legacy"
							// Only an already legacy-only predecessor whose entire atomic
							// services array is unchanged keeps the original leaf-scoped
							// ratcheting; every other transition re-applies the guard.
							for _, ratchet := range []struct {
								name    string
								oldSpec map[string]any
								want    bool
							}{
								{name: "create", oldSpec: nil, want: guarded},
								{name: "legacy-only predecessor with unchanged services", oldSpec: map[string]any{"services": services}, want: true},
								{name: "legacy-only predecessor with changed services", oldSpec: map[string]any{"services": []any{map[string]any{"servicePublishingStrategy": map[string]any{arm: map[string]any{field: "other"}}}}}, want: guarded},
								{name: "legacy-only predecessor without services", oldSpec: map[string]any{}, want: guarded},
								{name: "publishing predecessor with unchanged services", oldSpec: map[string]any{"publishing": map[string]any{}, "services": services}, want: guarded},
							} {
								oldSelf := types.OptionalNone
								if ratchet.oldSpec != nil {
									oldSelf = types.OptionalOf(types.DefaultTypeAdapter.NativeToValue(ratchet.oldSpec))
								}
								if evaluatePublishingRule(t, parentProgram, map[string]any{"self": self, "oldSelf": oldSelf}) != ratchet.want {
									t.Fatalf("legacy-only guard changed: arm=%s value=%s publishing=%t transition=%s", arm, value, publishing, ratchet.name)
								}
							}
						}
					}
					for _, self := range []map[string]any{{}, {"services": []any{map[string]any{"servicePublishingStrategy": map[string]any{}}}}} {
						if !evaluatePublishingRule(t, parentProgram, map[string]any{"self": self, "oldSelf": types.OptionalNone}) {
							t.Fatal("omitted legacy endpoints must remain optional")
						}
					}
				}
			})
		}
	}
	t.Run("When an enabled HCP endpoint schema is missing, it should fail rather than weaken validation", func(t *testing.T) {
		manifest := projectionSchemaFixture("HostedControlPlane", true)
		versions, _, _ := unstructured.NestedSlice(manifest, "spec", "versions")
		version := versions[0].(map[string]any)
		unstructured.RemoveNestedField(version, "schema", "openAPIV3Schema", "properties", "spec", "properties", "publishing", "properties", "nodePort")
		if err := unstructured.SetNestedSlice(manifest, versions, "spec", "versions"); err != nil {
			t.Fatal(err)
		}
		if err := allowPublishingProjection(manifest); err == nil {
			t.Fatal("missing schema must fail generation")
		}
	})
	t.Run("When a legacy endpoint rule is a transition rule, it should fail rather than corrupt the lifted guard", func(t *testing.T) {
		manifest := projectionSchemaFixture("HostedControlPlane", true)
		versions, _, _ := unstructured.NestedSlice(manifest, "spec", "versions")
		version := versions[0].(map[string]any)
		legacyPath := []string{"schema", "openAPIV3Schema", "properties", "spec", "properties", "services", "items", "properties", "servicePublishingStrategy", "properties", "nodePort", "properties", "address", "x-kubernetes-validations"}
		if err := unstructured.SetNestedSlice(version, []any{map[string]any{"rule": "self == oldSelf", "message": "endpoint validation"}}, legacyPath...); err != nil {
			t.Fatal(err)
		}
		if err := unstructured.SetNestedSlice(manifest, versions, "spec", "versions"); err != nil {
			t.Fatal(err)
		}
		err := allowPublishingProjection(manifest)
		if err == nil || !strings.Contains(err.Error(), "transition rules are not supported") {
			t.Fatalf("transition rules must fail generation: %v", err)
		}
	})
}

func TestPublishingEndpointRule(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		schema map[string]any
		want   string
	}{
		{name: "When hostname validation is a pattern, it should preserve the generated grammar", schema: map[string]any{"endpoint": map[string]any{"pattern": `^[a-z]+\.example$`}}, want: `self.matches("^[a-z]+\\.example$")`},
		{name: "When address validation is CEL, it should preserve the generated rule", schema: map[string]any{"endpoint": map[string]any{"x-kubernetes-validations": []any{map[string]any{"rule": "self == 'address'"}}}}, want: "self == 'address'"},
		{name: "When endpoint schema is absent, it should fail generation", schema: map[string]any{}},
		{name: "When endpoint validation is absent, it should fail generation", schema: map[string]any{"endpoint": map[string]any{}}},
		{name: "When endpoint validation is malformed, it should fail generation", schema: map[string]any{"endpoint": map[string]any{"x-kubernetes-validations": []any{"invalid"}}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			rule, err := publishingEndpointRule(testCase.schema, []string{"endpoint"})
			if testCase.want == "" {
				if err == nil {
					t.Fatal("missing or malformed endpoint must fail generation")
				}
				return
			}
			if err != nil || rule != testCase.want {
				t.Fatalf("unexpected endpoint rule %q: %v", rule, err)
			}
		})
	}
}
