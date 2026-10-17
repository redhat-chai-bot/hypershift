package main

import (
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
)

func TestPublishingMarkerOptionalEquivalence(t *testing.T) {
	environment, err := cel.NewEnv(cel.Variable("self", cel.DynType), cel.OptionalTypes())
	if err != nil {
		t.Fatal(err)
	}
	markerPattern := regexp.MustCompile(`rule="([^"]+)",message="([^"]+)"`)
	optionalPattern := regexp.MustCompile(`\(!([^ ]+)\.hasValue\(\) \|\| ([^ ]+)\.value\(\) != '([^']+)'\)`)
	for _, filename := range []string{"hostedcluster_types.go", "hosted_controlplane.go"} {
		t.Run(filename, func(t *testing.T) {
			source, err := os.ReadFile(filepath.Join("..", "..", "..", "api", "hypershift", "v1beta1", filename))
			if err != nil {
				t.Fatal(err)
			}
			formatted, err := format.Source(source)
			if err != nil || string(formatted) != string(source) {
				t.Fatalf("API markers must remain stable under the tools module formatter: %v", err)
			}
			markerCount := 0
			for _, line := range strings.Split(string(source), "\n") {
				if !strings.Contains(line, "FeatureGateAwareXValidation:featureGate=ServicePublishingAPI") || !strings.Contains(line, ".hasValue()") {
					continue
				}
				markerCount++
				marker := markerPattern.FindStringSubmatch(line)
				optional := optionalPattern.FindStringSubmatch(line)
				if len(marker) != 3 || len(optional) != 4 || optional[1] != optional[2] {
					t.Fatalf("unexpected optional marker representation: %s", line)
				}
				previousRule := strings.Replace(marker[1], optional[0], optional[1]+".orValue('') != '"+optional[3]+"'", 1)
				currentProgram := compilePublishingRule(t, environment, marker[1])
				previousProgram := compilePublishingRule(t, environment, previousRule)
				optionalProgram := compilePublishingRule(t, environment, optional[0])
				propertyPath := strings.Split(strings.TrimPrefix(optional[1], "self.platform.?"), ".")
				for _, state := range []string{"parentAbsent", "fieldAbsent", "empty", "matching", "nonmatching"} {
					for _, platformType := range []string{"AWS", "Azure", "GCP", "Agent", "IBMCloud"} {
						for _, publishingType := range []string{"SingleIngressPoint", "DedicatedAPIServerEndpoint", "NodePort", "absent"} {
							for _, exposure := range []string{"LoadBalancer", "absent"} {
								name := fmt.Sprintf("%s/%s/%s/%s/%s", marker[2], state, platformType, publishingType, exposure)
								t.Run(name, func(t *testing.T) {
									platform := map[string]any{"type": platformType}
									if state != "parentAbsent" {
										parent := platform
										for _, property := range propertyPath[:len(propertyPath)-1] {
											nested := map[string]any{}
											parent[property] = nested
											parent = nested
										}
										if state != "fieldAbsent" {
											value := ""
											if state == "matching" {
												value = optional[3]
											} else if state == "nonmatching" {
												value = "Other"
											}
											parent[propertyPath[len(propertyPath)-1]] = value
										}
									}
									service := map[string]any{"name": "OAuthServer"}
									if exposure != "absent" {
										service["exposure"] = exposure
									}
									self := map[string]any{"platform": platform}
									if publishingType != "absent" {
										self["publishing"] = map[string]any{
											"type":                       publishingType,
											"singleIngressPoint":         map[string]any{"services": []any{service}},
											"dedicatedAPIServerEndpoint": map[string]any{"services": []any{service}},
										}
									}
									bindings := map[string]any{"self": self}
									current := evaluatePublishingRule(t, currentProgram, bindings)
									previous := evaluatePublishingRule(t, previousProgram, bindings)
									if current != previous {
										t.Fatalf("marker truth value changed: current=%v previous=%v", current, previous)
									}
									if actual := evaluatePublishingRule(t, optionalProgram, bindings); actual != (state != "matching") {
										t.Fatalf("optional comparison changed absent/empty/matching/nonmatching behavior: %v", actual)
									}
								})
							}
						}
					}
				}
			}
			if markerCount != 5 {
				t.Fatalf("expected five corrected markers, got %d", markerCount)
			}
		})
	}
}

func compilePublishingRule(t *testing.T, environment *cel.Env, rule string) cel.Program {
	t.Helper()
	expression, issues := environment.Compile(rule)
	if issues.Err() != nil {
		t.Fatalf("compile %s: %v", rule, issues.Err())
	}
	program, err := environment.Program(expression)
	if err != nil {
		t.Fatal(err)
	}
	return program
}

func evaluatePublishingRule(t *testing.T, program cel.Program, bindings map[string]any) bool {
	t.Helper()
	result, _, err := program.Eval(bindings)
	if err != nil {
		t.Fatal(err)
	}
	value, ok := result.Value().(bool)
	if !ok {
		t.Fatalf("expected bool, got %v", result)
	}
	return value
}
