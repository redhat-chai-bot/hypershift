package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"sigs.k8s.io/yaml"
)

func requireLegacySource(manifest map[string]any) error {
	versions, found, err := unstructured.NestedSlice(manifest, "spec", "versions")
	if err != nil || !found {
		return fmt.Errorf("read CRD versions: found=%t: %v", found, err)
	}
	for _, version := range versions {
		versionMap, ok := version.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid CRD version")
		}
		schemaPath := []string{"schema", "openAPIV3Schema", "properties", "spec"}
		specSchema, found, err := unstructured.NestedMap(versionMap, schemaPath...)
		if err != nil || !found {
			return fmt.Errorf("read spec schema: found=%t: %v", found, err)
		}
		_, publishing, err := unstructured.NestedFieldNoCopy(specSchema, "properties", "publishing")
		if err != nil {
			return err
		}
		if publishing {
			continue
		}
		required, _, err := unstructured.NestedStringSlice(specSchema, "required")
		if err != nil {
			return err
		}
		hasServices := false
		for _, field := range required {
			if field == "services" {
				hasServices = true
			}
		}
		if !hasServices {
			required = append(required, "services")
		}
		if err := unstructured.SetNestedStringSlice(specSchema, required, "required"); err != nil {
			return err
		}
		validations, _, err := unstructured.NestedSlice(specSchema, "x-kubernetes-validations")
		if err != nil {
			return err
		}
		validations = append(validations, map[string]any{"rule": "has(self.services) && size(self.services) > 0", "message": "non-empty spec.services is required when ServicePublishingAPI is disabled"})
		if err := unstructured.SetNestedSlice(specSchema, validations, "x-kubernetes-validations"); err != nil {
			return err
		}
		if err := unstructured.SetNestedMap(versionMap, specSchema, schemaPath...); err != nil {
			return err
		}
	}
	return unstructured.SetNestedSlice(manifest, versions, "spec", "versions")
}

func allowPublishingProjection(manifest map[string]any) error {
	kind, _, err := unstructured.NestedString(manifest, "spec", "names", "kind")
	if err != nil || kind != "HostedControlPlane" {
		return err
	}
	versions, found, err := unstructured.NestedSlice(manifest, "spec", "versions")
	if err != nil || !found {
		return fmt.Errorf("read HCP versions: found=%t: %v", found, err)
	}
	for _, version := range versions {
		versionMap, ok := version.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid HCP version")
		}
		schemaPath := []string{"schema", "openAPIV3Schema", "properties", "spec"}
		specSchema, found, err := unstructured.NestedMap(versionMap, schemaPath...)
		if err != nil || !found {
			return fmt.Errorf("read HCP spec schema: found=%t: %v", found, err)
		}
		if _, enabled, err := unstructured.NestedFieldNoCopy(specSchema, "properties", "publishing"); err != nil {
			return err
		} else if !enabled {
			continue
		}
		parentRules, _, err := unstructured.NestedSlice(specSchema, "x-kubernetes-validations")
		if err != nil {
			return err
		}
		for _, endpoint := range []struct {
			arm, field     string
			publishingPath []string
		}{
			{arm: "nodePort", field: "address", publishingPath: []string{"properties", "publishing", "properties", "nodePort", "properties", "address"}},
			{arm: "loadBalancer", field: "hostname", publishingPath: []string{"properties", "publishing", "properties", "dedicatedAPIServerEndpoint", "properties", "services", "items", "properties", "hostname"}},
			{arm: "route", field: "hostname", publishingPath: []string{"properties", "publishing", "properties", "dedicatedAPIServerEndpoint", "properties", "services", "items", "properties", "hostname"}},
		} {
			legacyPath := []string{"properties", "services", "items", "properties", "servicePublishingStrategy", "properties", endpoint.arm, "properties", endpoint.field, "x-kubernetes-validations"}
			legacyRules, found, err := unstructured.NestedSlice(specSchema, legacyPath...)
			if err != nil || !found || len(legacyRules) == 0 {
				return fmt.Errorf("read legacy %s.%s validation: found=%t: %v", endpoint.arm, endpoint.field, found, err)
			}
			publishingRule, err := publishingEndpointRule(specSchema, endpoint.publishingPath)
			if err != nil {
				return err
			}
			if strings.Contains(publishingRule, "oldSelf") {
				return fmt.Errorf("union publishing %s.%s rule: transition rules are not supported", endpoint.arm, endpoint.field)
			}
			for _, validation := range legacyRules {
				legacyValidation, ok := validation.(map[string]any)
				if !ok {
					return fmt.Errorf("invalid legacy endpoint validation")
				}
				legacyRule, found, err := unstructured.NestedString(legacyValidation, "rule")
				if err != nil || !found {
					return fmt.Errorf("read legacy endpoint rule: found=%t: %v", found, err)
				}
				message, _, err := unstructured.NestedString(legacyValidation, "message")
				if err != nil {
					return err
				}
				if strings.Contains(legacyRule, "oldSelf") {
					return fmt.Errorf("lift legacy %s.%s rule: transition rules are not supported", endpoint.arm, endpoint.field)
				}
				strategy := "service.servicePublishingStrategy." + endpoint.arm
				value := strategy + "." + endpoint.field
				guard := "has(self.publishing) || !has(self.services) || self.services.all(service, !has(" + strategy + ") || !has(" + value + ") || (" + strings.ReplaceAll(legacyRule, "self", value) + "))"
				// Lifting a leaf rule to spec scope widens the scope Kubernetes
				// correlates for CEL validation ratcheting: the original leaf error
				// was ratcheted while the atomic spec.services array was unchanged,
				// but a spec-scoped error is only ratcheted while the whole spec is
				// unchanged. Restore the original granularity by exempting objects
				// that were already legacy-only and whose entire spec.services array
				// is unchanged, so historical data cannot block unrelated spec
				// updates. optionalOldSelf keeps the guard enforced on create, where
				// a plain transition rule would be skipped. Kubernetes only honours
				// optionalOldSelf while the CRDValidationRatcheting feature is
				// enabled, so on a cluster that disables it these guards are skipped
				// on create and the unioned leaf rules are the only legacy check.
				// That configuration also has no ratcheting, so it cannot hit the
				// regression this exception exists to avoid; the feature is on by
				// default from Kubernetes 1.30 and locked on from 1.33.
				ratchet := "(oldSelf.hasValue() && !has(oldSelf.value().publishing) && has(oldSelf.value().services) && oldSelf.value().services == self.services)"
				parentRules = append(parentRules, map[string]any{
					"rule":            guard + " || " + ratchet,
					"message":         "legacy services without publishing: " + message,
					"optionalOldSelf": true,
				})
				legacyValidation["rule"] = "(" + legacyRule + ") || (" + publishingRule + ")"
			}
			if err := unstructured.SetNestedSlice(specSchema, legacyRules, legacyPath...); err != nil {
				return err
			}
		}
		if err := unstructured.SetNestedSlice(specSchema, parentRules, "x-kubernetes-validations"); err != nil {
			return err
		}
		if err := unstructured.SetNestedMap(versionMap, specSchema, schemaPath...); err != nil {
			return err
		}
	}
	return unstructured.SetNestedSlice(manifest, versions, "spec", "versions")
}

// keyedValidation pairs a spec validation with the rule text it is sorted by.
type keyedValidation struct {
	rule       string
	validation any
}

// canonicaliseSpecValidations sorts the spec-scope x-kubernetes-validations of
// every CRD version by rule text, which is a total order because a schema cannot
// carry the same rule twice.
//
// controller-tools builds the schema marker list by ranging over a map of marker
// names and then orders it with an unstable sort.Slice on apply priority alone
// (vendor/sigs.k8s.io/controller-tools/pkg/crd/schema.go). Go's sort.Slice keeps
// the input order only while the slice has at most twelve elements, so a scope
// that carries more validations than that emits them in a different order on
// every generator run. HostedClusterSpec and HostedControlPlaneSpec cross that
// threshold once the ServicePublishingAPI rules are present, which would make
// `make verify-ci` (and its verify-git-clean step) fail at random. Sorting here
// removes the ordering freedom instead of leaving generated output unstable;
// CEL rule order is not semantically significant.
func canonicaliseSpecValidations(manifest map[string]any) error {
	versions, found, err := unstructured.NestedSlice(manifest, "spec", "versions")
	if err != nil || !found {
		return fmt.Errorf("read CRD versions: found=%t: %v", found, err)
	}
	for _, version := range versions {
		versionMap, ok := version.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid CRD version")
		}
		schemaPath := []string{"schema", "openAPIV3Schema", "properties", "spec"}
		specSchema, found, err := unstructured.NestedMap(versionMap, schemaPath...)
		if err != nil || !found {
			return fmt.Errorf("read spec schema: found=%t: %v", found, err)
		}
		validations, found, err := unstructured.NestedSlice(specSchema, "x-kubernetes-validations")
		if err != nil {
			return err
		}
		if !found || len(validations) < 2 {
			continue
		}
		keyed := make([]keyedValidation, 0, len(validations))
		for _, validation := range validations {
			validationMap, ok := validation.(map[string]any)
			if !ok {
				return fmt.Errorf("invalid spec validation")
			}
			rule, found, err := unstructured.NestedString(validationMap, "rule")
			if err != nil || !found || rule == "" {
				return fmt.Errorf("read spec validation rule: found=%t: %v", found, err)
			}
			keyed = append(keyed, keyedValidation{rule: rule, validation: validation})
		}
		sort.SliceStable(keyed, func(i, j int) bool { return keyed[i].rule < keyed[j].rule })
		for i, entry := range keyed {
			validations[i] = entry.validation
		}
		if err := unstructured.SetNestedSlice(specSchema, validations, "x-kubernetes-validations"); err != nil {
			return err
		}
		if err := unstructured.SetNestedMap(versionMap, specSchema, schemaPath...); err != nil {
			return err
		}
	}
	return unstructured.SetNestedSlice(manifest, versions, "spec", "versions")
}

func publishingEndpointRule(specSchema map[string]any, path []string) (string, error) {
	endpoint, found, err := unstructured.NestedMap(specSchema, path...)
	if err != nil || !found {
		return "", fmt.Errorf("read publishing endpoint schema: found=%t: %v", found, err)
	}
	pattern, found, err := unstructured.NestedString(endpoint, "pattern")
	if err != nil {
		return "", err
	}
	if found && pattern != "" {
		return "self.matches(" + strconv.Quote(pattern) + ")", nil
	}
	validations, found, err := unstructured.NestedSlice(endpoint, "x-kubernetes-validations")
	if err != nil || !found || len(validations) != 1 {
		return "", fmt.Errorf("read publishing endpoint validation: found=%t: %v", found, err)
	}
	validation, ok := validations[0].(map[string]any)
	if !ok {
		return "", fmt.Errorf("invalid publishing endpoint validation")
	}
	rule, found, err := unstructured.NestedString(validation, "rule")
	if err != nil || !found || rule == "" {
		return "", fmt.Errorf("read publishing endpoint rule: found=%t: %v", found, err)
	}
	return rule, nil
}

func run(directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "hostedclusters-") && !strings.HasPrefix(entry.Name(), "hostedcontrolplanes-") {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		manifest := map[string]any{}
		if err := yaml.Unmarshal(data, &manifest); err != nil {
			return err
		}
		if err := requireLegacySource(manifest); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if err := allowPublishingProjection(manifest); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if err := canonicaliseSpecValidations(manifest); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		data, err = yaml.Marshal(manifest)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, data, 0644); err != nil {
			return err
		}
	}
	return nil
}

// runCanonicalise sorts the spec-scope validations of the per-feature-gate
// partial schemas, which are generated before the merged CRDs this tool
// otherwise rewrites. They need the same treatment because they are committed
// artifacts that `make verify-ci` regenerates and compares.
func runCanonicalise(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Ext(path) != ".yaml" {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		manifest := map[string]any{}
		if err := yaml.Unmarshal(data, &manifest); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		kind, _, err := unstructured.NestedString(manifest, "spec", "names", "kind")
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if kind != "HostedCluster" && kind != "HostedControlPlane" {
			return nil
		}
		if err := canonicaliseSpecValidations(manifest); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		data, err = yaml.Marshal(manifest)
		if err != nil {
			return err
		}
		return os.WriteFile(path, data, 0644)
	})
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == "--canonicalise" {
		if err := runCanonicalise(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: publishing-source CRD_DIRECTORY | publishing-source --canonicalise PARTIAL_SCHEMA_DIRECTORY")
		os.Exit(1)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
