//go:build envtest

package envtest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	kyaml "sigs.k8s.io/yaml"
)

var _ = Describe("ServicePublishingAPI admission coverage", func() {
	for _, source := range suites {
		if !strings.Contains(source.Name, "publishing validation") {
			continue
		}
		kind := "HostedCluster"
		if strings.HasPrefix(source.Name, "HostedControlPlane") {
			kind = "HostedControlPlane"
		}
		source.Name = kind + " publishing matrix [OCPFeatureGate:ServicePublishingAPI] [Jira:HyperShift]"
		source.Tests = publishingAdmissionCases(kind)
		GenerateTestSuite(source)
	}
})

func publishingAdmissionCases(kind string) TestSpec {
	result := TestSpec{}
	add := func(name, policy, platform, expectedError string) {
		if !strings.Contains(name, "it should") {
			if expectedError == "" {
				name += ", it should accept it"
			} else {
				name += ", it should reject it"
			}
		}
		result.OnCreate = append(result.OnCreate, OnCreateTestSpec{
			Name:          "When " + name,
			Initial:       publishingResource(kind, policy, platform, false),
			ExpectedError: expectedError,
		})
	}
	dedicated := `{"type":"DedicatedAPIServerEndpoint"}`
	nodePort := `{"type":"NodePort","nodePort":{"address":"10.0.0.5"}}`
	publishingEndpointGrammarCases(add)
	ingress := publishingIngress("LoadBalancer", `"loadBalancer":{}`, `[{"name":"APIServer","hostname":"api.example.com"}]`)
	publishingUnionCases(add, ingress, nodePort)
	publishingPlatformCases(add, dedicated, ingress, nodePort)
	result.OnCreate = append(result.OnCreate,
		OnCreateTestSpec{
			Name:          "When legacy services are empty and publishing is absent, it should reject the missing source",
			Initial:       publishingSet(publishingResource(kind, "", "None", false), "spec.services", []interface{}{}),
			ExpectedError: "spec.publishing or non-empty spec.services is required",
		},
		OnCreateTestSpec{
			Name:    "When legacy private Routes omit external hostnames, it should preserve internal route admission",
			Initial: strings.ReplaceAll(publishingResource(kind, "", "AWSPrivate", true), `"route":{"hostname":"api.example.com"}`, `"route":{}`),
		},
	)
	for _, hostname := range []string{"api.example.com", "other.example.com"} {
		configuration := map[string]interface{}{
			"apiServer": map[string]interface{}{
				"servingCerts": map[string]interface{}{
					"namedCertificates": []interface{}{
						map[string]interface{}{
							"names":              []interface{}{hostname},
							"servingCertificate": map[string]interface{}{"name": "named-serving-cert"},
						},
					},
				},
			},
		}
		expectedError := ""
		if kind == "HostedCluster" && hostname == "api.example.com" {
			expectedError = "APIServer LoadBalancer hostname cannot be in namedCertificates"
		}
		result.OnCreate = append(result.OnCreate, OnCreateTestSpec{
			Name:          "When a dedicated API hostname is compared with named certificate " + hostname + ", it should enforce the HC certificate conflict rule",
			Initial:       publishingSet(publishingResource(kind, `{"type":"DedicatedAPIServerEndpoint","dedicatedAPIServerEndpoint":{"services":[{"name":"APIServer","hostname":"api.example.com"}]}}`, "None", false), "spec.configuration", configuration),
			ExpectedError: expectedError,
		})
		result.OnCreate = append(result.OnCreate, OnCreateTestSpec{
			Name:    "When an ingress Route hostname matches named certificate " + hostname + ", it should not apply a LoadBalancer restriction",
			Initial: publishingSet(publishingResource(kind, ingress, "None", false), "spec.configuration", configuration),
		})
	}
	publishingMigrationCases(&result, kind, dedicated, ingress)
	return result
}

// publishingEndpointGrammarCases covers the address and hostname grammar of every endpoint field.
func publishingEndpointGrammarCases(add func(name, policy, platform, expectedError string)) {
	for _, address := range []string{"localhost", "node01", "node.example.com", "0.0.0.0", "255.255.255.255", "::", "2001:db8::1", "::ffff:192.0.2.10", "::FFFF:c000:20a", "0:0:0:0:0:ffff:192.0.2.10", "2001:db8:0:0:0:0:192.0.2.10", "999.999.999.999", "256.0.0.1", "192.0.2.01", "2001:db8:::1", "::ffff:999.999.999.999"} {
		expected := ""
		if address == "999.999.999.999" || address == "256.0.0.1" || address == "192.0.2.01" || address == "2001:db8:::1" || address == "::ffff:999.999.999.999" {
			expected = "address must be a valid hostname, IPv4, or IPv6 address"
		}
		add("direct NodePort validates address "+address, fmt.Sprintf(`{"type":"NodePort","nodePort":{"address":%q}}`, address), "None", expected)
		add("router NodePort validates address "+address, publishingIngress("NodePort", fmt.Sprintf(`"nodePort":{"address":%q}`, address), `[{"name":"APIServer","hostname":"api.example.com"}]`), "None", expected)
	}
	for _, hostname := range []string{"localhost", "api", "api.example.com", "API.Example.COM", strings.Repeat("a", 63) + ".example.com", "-api.example.com", "api-.example.com", strings.Repeat("a", 64) + ".example.com"} {
		invalid := strings.HasPrefix(hostname, "-") || hostname == "api-.example.com" || len(strings.Split(hostname, ".")[0]) > 63
		for _, service := range []string{"APIServer", "OAuthServer", "Konnectivity", "Ignition"} {
			expected := ""
			if invalid {
				expected = "hostname"
			}
			services := fmt.Sprintf(`[{"name":"APIServer","hostname":"api.example.com"},{"name":%q,"hostname":%q}]`, service, hostname)
			if service == "APIServer" {
				services = fmt.Sprintf(`[{"name":"APIServer","hostname":%q}]`, hostname)
			}
			add("ingress "+service+" validates hostname "+hostname, publishingIngress("LoadBalancer", `"loadBalancer":{}`, services), "None", expected)
		}
		for _, service := range []string{"APIServer", "OAuthServer"} {
			expected := ""
			if invalid {
				expected = "hostname"
			}
			add("dedicated "+service+" validates hostname "+hostname, fmt.Sprintf(`{"type":"DedicatedAPIServerEndpoint","dedicatedAPIServerEndpoint":{"services":[{"name":%q,"hostname":%q}]}}`, service, hostname), "None", expected)
		}
		expected := ""
		if invalid {
			expected = "hostname"
		}
		add("router validates hostname "+hostname, publishingIngress("LoadBalancer", fmt.Sprintf(`"loadBalancer":{"hostname":%q}`, hostname), `[{"name":"APIServer","hostname":"api.example.com"}]`), "None", expected)
	}
}

// publishingUnionCases covers the discriminator, the union arms and the NodePort specific rules.
func publishingUnionCases(add func(name, policy, platform, expectedError string), ingress, nodePort string) {
	add("a dedicated API preset has an empty arm, it should accept it", `{"type":"DedicatedAPIServerEndpoint","dedicatedAPIServerEndpoint":{}}`, "None", "")
	// Both optional service lists are explicitly MinItems=0, so an empty list has to be admitted
	// and means the same as omitting the field. Only the required SingleIngressPoint list, which
	// is MinItems=1, rejects an empty list (asserted further down).
	add("a dedicated API preset has an empty service list, it should accept it", `{"type":"DedicatedAPIServerEndpoint","dedicatedAPIServerEndpoint":{"services":[]}}`, "None", "")
	add("direct NodePort has an empty service list, it should accept it", `{"type":"NodePort","nodePort":{"address":"10.0.0.5","services":[]}}`, "None", "")
	add("the preset is unknown, it should reject it", `{"type":"Custom"}`, "None", "spec.publishing.type")
	add("the discriminator is absent, it should reject it", `{}`, "None", "spec.publishing.type")
	add("SingleIngressPoint omits its arm, it should reject it", `{"type":"SingleIngressPoint"}`, "None", "singleIngressPoint must match")
	add("NodePort omits its arm, it should reject it", `{"type":"NodePort"}`, "None", "nodePort must match")
	add("SingleIngressPoint selects the NodePort arm, it should reject it", `{"type":"SingleIngressPoint","nodePort":{"address":"10.0.0.5"}}`, "None", "must match the publishing type")
	add("a dedicated API preset sets the ingress arm, it should reject it", strings.Replace(ingress, "SingleIngressPoint", "DedicatedAPIServerEndpoint", 1), "None", "singleIngressPoint must match")
	add("NodePort sets the dedicated API arm, it should reject it", `{"type":"NodePort","nodePort":{"address":"10.0.0.5"},"dedicatedAPIServerEndpoint":{}}`, "None", "dedicatedAPIServerEndpoint is forbidden")
	add("a LoadBalancer exposure omits its config, it should reject it", publishingIngress("LoadBalancer", "", `[{"name":"APIServer","hostname":"api.example.com"}]`), "None", "loadBalancer must match")
	add("LoadBalancer exposure selects NodePort config, it should reject it", publishingIngress("LoadBalancer", `"nodePort":{"address":"10.0.0.5"}`, `[{"name":"APIServer","hostname":"api.example.com"}]`), "None", "must match ingress exposure")
	add("NodePort exposure omits its config, it should reject it", publishingIngress("NodePort", "", `[{"name":"APIServer","hostname":"api.example.com"}]`), "None", "nodePort must match ingress exposure")
	add("PlatformProvided sets a LoadBalancer arm, it should reject it", publishingIngress("PlatformProvided", `"loadBalancer":{}`, `[{"name":"APIServer","hostname":"api.example.com"}]`), "None", "loadBalancer must match")
	add("ingress exposure is unknown, it should reject it", publishingIngress("External", "", `[{"name":"APIServer","hostname":"api.example.com"}]`), "None", "spec.publishing.singleIngressPoint.exposure")
	add("SingleIngressPoint uses an empty LoadBalancer config, it should accept that config", ingress, "None", "")
	add("SingleIngressPoint has an empty service list, it should reject it", publishingIngress("LoadBalancer", `"loadBalancer":{}`, `[]`), "None", "spec.publishing.singleIngressPoint.services")
	add("SingleIngressPoint omits its service list, it should reject it", `{"type":"SingleIngressPoint","singleIngressPoint":{"exposure":"LoadBalancer","loadBalancer":{}}}`, "None", "spec.publishing.singleIngressPoint.services")
	add("a service omits its name, it should reject it", publishingIngress("LoadBalancer", `"loadBalancer":{}`, `[{"hostname":"api.example.com"}]`), "None", "name")
	add("SingleIngressPoint omits APIServer, it should reject it", publishingIngress("LoadBalancer", `"loadBalancer":{}`, `[{"name":"OAuthServer"}]`), "None", "requires an APIServer entry with a hostname")
	add("APIServer omits its hostname, it should reject it", publishingIngress("LoadBalancer", `"loadBalancer":{}`, `[{"name":"APIServer"}]`), "None", "requires an APIServer entry with a hostname")
	add("APIServer has an empty hostname, it should reject it", publishingIngress("LoadBalancer", `"loadBalancer":{}`, `[{"name":"APIServer","hostname":""}]`), "None", "spec.publishing.singleIngressPoint.services[0].hostname")
	add("a hostname is malformed, it should reject it", strings.Replace(ingress, "api.example.com", "not a hostname", 1), "None", "spec.publishing.singleIngressPoint.services[0].hostname")
	add("a router LoadBalancer hostname is malformed, it should reject it", publishingIngress("LoadBalancer", `"loadBalancer":{"hostname":"not a hostname"}`, `[{"name":"APIServer","hostname":"api.example.com"}]`), "None", "spec.publishing.singleIngressPoint.loadBalancer.hostname")
	add("a dedicated API hostname is empty, it should reject it", `{"type":"DedicatedAPIServerEndpoint","dedicatedAPIServerEndpoint":{"services":[{"name":"APIServer","hostname":""}]}}`, "None", "spec.publishing.dedicatedAPIServerEndpoint.services[0].hostname")
	maxHostname := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 57) + ".com"
	for _, hostname := range []string{maxHostname, "a" + maxHostname} {
		expected := ""
		if len(hostname) > 253 {
			expected = "spec.publishing.singleIngressPoint.services[0].hostname"
		}
		add(fmt.Sprintf("an ingress service hostname has %d characters, it should enforce the length limit", len(hostname)), strings.Replace(ingress, "api.example.com", hostname, 1), "None", expected)
		if expected != "" {
			expected = "spec.publishing.dedicatedAPIServerEndpoint.services[0].hostname"
		}
		add(fmt.Sprintf("a dedicated API hostname has %d characters, it should enforce the length limit", len(hostname)), fmt.Sprintf(`{"type":"DedicatedAPIServerEndpoint","dedicatedAPIServerEndpoint":{"services":[{"name":"APIServer","hostname":%q}]}}`, hostname), "None", expected)
		if expected != "" {
			expected = "spec.publishing.singleIngressPoint.loadBalancer.hostname"
		}
		add(fmt.Sprintf("a router hostname has %d characters, it should enforce the length limit", len(hostname)), publishingIngress("LoadBalancer", fmt.Sprintf(`"loadBalancer":{"hostname":%q}`, hostname), `[{"name":"APIServer","hostname":"api.example.com"}]`), "None", expected)
	}
	add("an ingress service name is unknown, it should reject it", publishingIngress("LoadBalancer", `"loadBalancer":{}`, `[{"name":"APIServer","hostname":"api.example.com"},{"name":"Router"}]`), "None", "spec.publishing.singleIngressPoint.services[1].name")
	add("ingress has duplicate service names, it should reject it", publishingIngress("LoadBalancer", `"loadBalancer":{}`, `[{"name":"APIServer","hostname":"api.example.com"},{"name":"APIServer","hostname":"other.example.com"}]`), "None", "Duplicate value")
	for _, service := range []string{"Konnectivity", "Ignition"} {
		add("a dedicated API preset configures "+service+", it should reject the unreachable override", fmt.Sprintf(`{"type":"DedicatedAPIServerEndpoint","dedicatedAPIServerEndpoint":{"services":[{"name":%q,"hostname":"service.example.com"}]}}`, service), "None", "spec.publishing.dedicatedAPIServerEndpoint.services[0].name")
	}
	add("a dedicated API preset has duplicate services, it should reject it", `{"type":"DedicatedAPIServerEndpoint","dedicatedAPIServerEndpoint":{"services":[{"name":"OAuthServer"},{"name":"OAuthServer"}]}}`, "None", "Duplicate value")
	add("NodePort has duplicate services, it should reject it", `{"type":"NodePort","nodePort":{"address":"10.0.0.5","services":[{"name":"APIServer"},{"name":"APIServer"}]}}`, "None", "Duplicate value")
	add("NodePort has an unknown service, it should reject it", `{"type":"NodePort","nodePort":{"address":"10.0.0.5","services":[{"name":"Router"}]}}`, "None", "spec.publishing.nodePort.services[0].name")
	add("NodePort assigns the same explicit port twice, it should reject it", `{"type":"NodePort","nodePort":{"address":"10.0.0.5","services":[{"name":"APIServer","port":30443},{"name":"OAuthServer","port":30443}]}}`, "None", "explicit port values must be unique")
	add("NodePort omits all port overrides, it should allow dynamic allocation", nodePort, "None", "")
	add("NodePort omits its shared address, it should reject it", `{"type":"NodePort","nodePort":{}}`, "None", "spec.publishing.nodePort.address")
	add("the router NodePort omits its address, it should reject it", publishingIngress("NodePort", `"nodePort":{}`, `[{"name":"APIServer","hostname":"api.example.com"}]`), "None", "spec.publishing.singleIngressPoint.nodePort.address")
	add("NodePort lists services without ports, it should allow dynamic allocation", `{"type":"NodePort","nodePort":{"address":"10.0.0.5","services":[{"name":"APIServer"},{"name":"OAuthServer"}]}}`, "None", "")
	for _, address := range []string{"node.example.com", "localhost", "10.0.0.5", "2001:db8::1", "::1"} {
		add("the NodePort address is "+address+", it should accept it", fmt.Sprintf(`{"type":"NodePort","nodePort":{"address":%q}}`, address), "None", "")
		add("the router NodePort address is "+address+", it should accept it", publishingIngress("NodePort", fmt.Sprintf(`"nodePort":{"address":%q}`, address), `[{"name":"APIServer","hostname":"api.example.com"}]`), "None", "")
	}
	for _, address := range []string{"", "not an address", "https://node.example.com", "2001:db8::gg"} {
		add("the NodePort address is invalid "+fmt.Sprintf("%q", address)+", it should reject it", fmt.Sprintf(`{"type":"NodePort","nodePort":{"address":%q}}`, address), "None", "spec.publishing.nodePort.address")
		add("the router NodePort address is invalid "+fmt.Sprintf("%q", address)+", it should reject it", publishingIngress("NodePort", fmt.Sprintf(`"nodePort":{"address":%q}`, address), `[{"name":"APIServer","hostname":"api.example.com"}]`), "None", "spec.publishing.singleIngressPoint.nodePort.address")
	}
	for _, port := range []int{0, 29999, 30000, 32767, 32768} {
		expected := ""
		if port < 30000 || port > 32767 {
			expected = "spec.publishing.nodePort.services[0].port"
		}
		add(fmt.Sprintf("a service NodePort is %d, it should enforce the range", port), fmt.Sprintf(`{"type":"NodePort","nodePort":{"address":"10.0.0.5","services":[{"name":"APIServer","port":%d}]}}`, port), "None", expected)
		if expected != "" {
			expected = "spec.publishing.singleIngressPoint.nodePort.port"
		}
		add(fmt.Sprintf("the router NodePort is %d, it should enforce the range", port), publishingIngress("NodePort", fmt.Sprintf(`"nodePort":{"address":"10.0.0.5","port":%d}`, port), `[{"name":"APIServer","hostname":"api.example.com"}]`), "None", expected)
	}
}

// publishingPlatformCases covers the per-platform preset restrictions and the OAuth exposure override.
func publishingPlatformCases(add func(name, policy, platform, expectedError string), dedicated, ingress, nodePort string) {
	for _, platform := range []string{"AWS", "Azure", "GCP", "PowerVS"} {
		add(platform+" selects NodePort, it should reject the cloud topology", nodePort, platform, "this cloud platform requires")
		add(platform+" selects dedicated API publishing, it should accept it", dedicated, platform, "")
		add(platform+" selects ingress publishing, it should accept it", ingress, platform, "")
		add(platform+" exposes the ingress router as NodePort, it should reject unreachable node exposure", publishingIngress("NodePort", `"nodePort":{"address":"10.0.0.5"}`, `[{"name":"APIServer","hostname":"api.example.com"}]`), platform, "SingleIngressPoint NodePort exposure requires")
	}
	for _, platform := range []string{"Agent", "KubeVirt", "None"} {
		add(platform+" selects NodePort, it should accept it", nodePort, platform, "")
		add(platform+" selects dedicated API publishing, it should accept it", dedicated, platform, "")
		for _, exposure := range []string{"LoadBalancer", "NodePort", "PlatformProvided"} {
			config := ""
			if exposure == "LoadBalancer" {
				config = `"loadBalancer":{}`
			}
			if exposure == "NodePort" {
				config = `"nodePort":{"address":"10.0.0.5"}`
			}
			add(platform+" selects SingleIngressPoint with "+exposure+", it should accept it", publishingIngress(exposure, config, `[{"name":"APIServer","hostname":"api.example.com"}]`), platform, "")
		}
	}
	add("OpenStack selects dedicated API publishing, it should accept it", dedicated, "OpenStack", "")
	add("OpenStack selects ingress publishing, it should reject it", ingress, "OpenStack", "OpenStack requires DedicatedAPIServerEndpoint")
	add("IBMCloud selects NodePort, it should preserve the legacy topology", nodePort, "IBMCloud", "")
	add("IBMCloud selects platform-provided ingress, it should accept it", publishingIngress("PlatformProvided", "", `[{"name":"APIServer","hostname":"api.example.com"}]`), "IBMCloud", "")
	add("IBMCloud selects LoadBalancer ingress, it should reject it", ingress, "IBMCloud", "IBMCloud requires NodePort or PlatformProvided")
	add("IBMCloud selects dedicated API publishing, it should reject it", dedicated, "IBMCloud", "IBMCloud requires NodePort or PlatformProvided")
	add("AWS Private selects dedicated API publishing, it should reject it", dedicated, "AWSPrivate", "AWS Private requires SingleIngressPoint")
	add("AWS Private selects ingress publishing, it should accept private hostname overrides", publishingIngress("LoadBalancer", `"loadBalancer":{}`, `[{"name":"APIServer","hostname":"api.example.com"},{"name":"Konnectivity","hostname":"konnectivity.example.com"},{"name":"Ignition","hostname":"ignition.example.com"}]`), "AWSPrivate", "")
	add("GCP Private omits a dedicated API hostname, it should preserve current CLI output", dedicated, "GCPPrivate", "")
	add("Azure Private selects dedicated API publishing, it should reject it", dedicated, "AzurePrivate", "Azure Private requires SingleIngressPoint")
	add("ARO selects dedicated API publishing, it should reject it", dedicated, "ARO", "ARO HCP requires SingleIngressPoint")
	add("ARO selects PlatformProvided ingress, it should preserve shared ingress", publishingIngress("PlatformProvided", "", `[{"name":"APIServer","hostname":"api.example.com"}]`), "ARO", "")
	for _, preset := range []string{"SingleIngressPoint", "DedicatedAPIServerEndpoint"} {
		policy := `{"type":"DedicatedAPIServerEndpoint","dedicatedAPIServerEndpoint":{"services":[{"name":"OAuthServer","exposure":"DedicatedLoadBalancer"}]}}`
		if preset == "SingleIngressPoint" {
			policy = publishingIngress("LoadBalancer", `"loadBalancer":{}`, `[{"name":"APIServer","hostname":"api.example.com"},{"name":"OAuthServer","exposure":"DedicatedLoadBalancer"}]`)
		}
		add(preset+" uses an OAuth dedicated LoadBalancer on Azure self-managed, it should accept it", policy, "Azure", "")
		add(preset+" uses an OAuth dedicated LoadBalancer on AWS, it should reject it", policy, "AWS", "OAuth dedicated LoadBalancer requires Azure self-managed")
		add(preset+" uses an OAuth dedicated LoadBalancer on ARO, it should reject it", policy, "ARO", "OAuth dedicated LoadBalancer requires Azure self-managed")
		invalidOverride := strings.ReplaceAll(policy, `"OAuthServer"`, `"APIServer"`)
		if preset == "SingleIngressPoint" {
			invalidOverride = publishingIngress("LoadBalancer", `"loadBalancer":{}`, `[{"name":"APIServer","hostname":"api.example.com","exposure":"DedicatedLoadBalancer"}]`)
		}
		add(preset+" uses an exposure override for APIServer, it should reject it", invalidOverride, "Azure", "exposure override is only valid for OAuthServer")
	}
	add("Azure Private promotes OAuth onto its dedicated LoadBalancer, it should accept it", publishingIngress("LoadBalancer", `"loadBalancer":{}`, `[{"name":"APIServer","hostname":"api.example.com"},{"name":"OAuthServer","exposure":"DedicatedLoadBalancer"}]`), "AzurePrivate", "")
	add("an exposure override value is unknown, it should reject it", publishingIngress("LoadBalancer", `"loadBalancer":{}`, `[{"name":"APIServer","hostname":"api.example.com"},{"name":"OAuthServer","exposure":"NodePort"}]`), "Azure", "spec.publishing.singleIngressPoint.services[1].exposure")
}

func publishingMigrationCases(result *TestSpec, kind, dedicated, ingress string) {
	legacy := publishingResource(kind, "", "None", true)
	policy := publishingResource(kind, dedicated, "None", false)
	dual := publishingResource(kind, dedicated, "None", true)
	add := func(name, initial, updated, expectedError string) {
		expected := ""
		if expectedError == "" {
			expected = fmt.Sprintf("apiVersion: hypershift.openshift.io/v1beta1\nkind: %s\n", kind)
		}
		result.OnUpdate = append(result.OnUpdate, OnUpdateTestSpec{Name: "When " + name, Initial: initial, Updated: updated, ExpectedError: expectedError, Expected: expected})
	}
	if kind == "HostedCluster" {
		result.OnCreate = append(result.OnCreate, OnCreateTestSpec{Name: "When both publishing sources are present, it should reject them", Initial: dual, ExpectedError: "mutually exclusive"})
		add("legacy services are removed while publishing is set, it should permit atomic migration", legacy, policy, "")
		add("legacy services are emptied while publishing is set, it should permit atomic migration", legacy, publishingSet(policy, "spec.services", []interface{}{}), "")
		add("legacy services are removed without publishing, it should reject the missing source", legacy, publishingResource(kind, "", "None", false), "spec.publishing or non-empty spec.services is required")
		add("legacy services are emptied without publishing, it should reject the missing source", legacy, publishingSet(publishingResource(kind, "", "None", false), "spec.services", []interface{}{}), "spec.publishing or non-empty spec.services is required")
		add("legacy services change without migration, it should reject the change", legacy, strings.Replace(legacy, `"type":"LoadBalancer"`, `"type":"NodePort","nodePort":{"address":"10.0.0.5"}`, 1), "Services is immutable")
		add("publishing is removed after migration, it should forbid reversal to services", policy, legacy, "spec.publishing cannot be removed")
		add("the publishing type changes, it should reject the immutable policy", policy, publishingResource(kind, ingress, "None", false), "spec.publishing is immutable")
		add("a publishing hostname changes, it should reject the immutable policy", publishingResource(kind, ingress, "None", false), publishingResource(kind, strings.Replace(ingress, "api.example.com", "other.example.com", 1), "None", false), "spec.publishing is immutable")
		initialNodePort := publishingResource(kind, `{"type":"NodePort","nodePort":{"address":"10.0.0.5","services":[{"name":"APIServer","port":30443}]}}`, "None", false)
		add("a publishing NodePort changes, it should reject the immutable policy", initialNodePort, strings.Replace(initialNodePort, "30443", "30444", 1), "spec.publishing is immutable")
		add("a publishing address changes, it should reject the immutable policy", initialNodePort, strings.Replace(initialNodePort, "10.0.0.5", "10.0.0.6", 1), "spec.publishing is immutable")
		add("the ingress exposure changes, it should reject the immutable policy", publishingResource(kind, ingress, "None", false), publishingResource(kind, publishingIngress("NodePort", `"nodePort":{"address":"10.0.0.5"}`, `[{"name":"APIServer","hostname":"api.example.com"}]`), "None", false), "spec.publishing is immutable")
		add("publishing is removed without restoring services, it should reject removal", policy, publishingResource(kind, "", "None", false), "spec.publishing cannot be removed")
		ibmLegacy := publishingResource(kind, "", "IBMCloud", true)
		add("IBMCloud legacy services change, it should preserve their mutability", ibmLegacy, strings.Replace(ibmLegacy, "api.example.com", "other.example.com", 1), "")
		ibmPolicy := publishingIngress("PlatformProvided", "", `[{"name":"APIServer","hostname":"api.example.com"}]`)
		add("IBMCloud publishing changes, it should enforce whole-policy immutability", publishingResource(kind, ibmPolicy, "IBMCloud", false), publishingResource(kind, strings.Replace(ibmPolicy, "api.example.com", "other.example.com", 1), "IBMCloud", false), "spec.publishing is immutable")
	} else {
		result.OnCreate = append(result.OnCreate, OnCreateTestSpec{Name: "When both publishing sources are present, it should accept coexistence", Initial: dual})
		add("the operator adds translated publishing, it should retain legacy services", legacy, dual, "")
		add("translated publishing changes, it should allow operator updates", dual, publishingResource(kind, ingress, "None", true), "")
		add("translation is removed on fallback, it should retain legacy services", dual, legacy, "")
		add("a translated publishing hostname changes, it should remain mutable", publishingResource(kind, ingress, "None", true), publishingResource(kind, strings.Replace(ingress, "api.example.com", "other.example.com", 1), "None", true), "")
	}
	add("publishing is unchanged during a metadata update, it should accept the update", policy, publishingSet(policy, "metadata.labels.publishing-test", "unchanged"), "")
}

func publishingIngress(exposure, config, services string) string {
	if config != "" {
		config += ","
	}
	return fmt.Sprintf(`{"type":"SingleIngressPoint","singleIngressPoint":{"exposure":%q,%s"services":%s}}`, exposure, config, services)
}

func publishingResource(kind, policy, platform string, legacy bool) string {
	spec := map[string]interface{}{
		"dns":        map[string]interface{}{"baseDomain": "example.com"},
		"pullSecret": map[string]interface{}{"name": "pull-secret"},
		"platform":   publishingPlatform(platform),
	}
	image := "quay.io/openshift-release-dev/ocp-release:4.21.10-x86_64"
	if kind == "HostedCluster" {
		spec["release"] = map[string]interface{}{"image": image}
	} else {
		spec["releaseImage"] = image
		spec["issuerURL"] = "https://issuer.example.com"
		spec["sshKey"] = map[string]interface{}{"name": "ssh-key"}
		spec["infraID"] = "publishing-test"
		spec["etcd"] = map[string]interface{}{"managementType": "Managed", "managed": map[string]interface{}{"storage": map[string]interface{}{"type": "PersistentVolume", "persistentVolume": map[string]interface{}{"size": "8Gi"}}}}
	}
	if policy != "" {
		publishing := map[string]interface{}{}
		Expect(json.Unmarshal([]byte(policy), &publishing)).To(Succeed())
		spec["publishing"] = publishing
	}
	if legacy {
		services := []interface{}{}
		for _, name := range []string{"APIServer", "OAuthServer", "Konnectivity", "Ignition"} {
			strategy := map[string]interface{}{"type": "Route", "route": map[string]interface{}{"hostname": "api.example.com"}}
			if name == "APIServer" && platform != "IBMCloud" {
				strategy = map[string]interface{}{"type": "LoadBalancer", "loadBalancer": map[string]interface{}{}}
			}
			if platform == "IBMCloud" && name == "Ignition" {
				continue
			}
			services = append(services, map[string]interface{}{"service": name, "servicePublishingStrategy": strategy})
		}
		spec["services"] = services
	}
	serialized, err := json.Marshal(map[string]interface{}{"apiVersion": "hypershift.openshift.io/v1beta1", "kind": kind, "spec": spec})
	Expect(err).NotTo(HaveOccurred())
	return string(serialized)
}

func publishingSet(resource, path string, value interface{}) string {
	object := map[string]interface{}{}
	Expect(json.Unmarshal([]byte(resource), &object)).To(Succeed())
	Expect(unstructured.SetNestedField(object, value, strings.Split(path, ".")...)).To(Succeed())
	serialized, err := json.Marshal(object)
	Expect(err).NotTo(HaveOccurred())
	return string(serialized)
}

func publishingPlatform(platform string) map[string]interface{} {
	basePlatform := strings.TrimSuffix(platform, "Private")
	if basePlatform == "Azure" || basePlatform == "ARO" || basePlatform == "GCP" || basePlatform == "AWS" {
		filename := "stable.hostedclusters.azure.testsuite.yaml"
		fixtureName := "Azure German cloud should be selectable"
		if basePlatform == "AWS" {
			filename = "stable.hostedclusters.aws.testsuite.yaml"
			fixtureName = "when resourceTags have valid key and value with overridePolicy Allow it should pass"
		}
		if basePlatform == "ARO" {
			fixtureName = "When managed Azure uses Route for OAuthServer it should pass"
		}
		if platform == "AzurePrivate" {
			fixtureName = "When Azure PrivateLink type is set with privateLink config it should pass"
		}
		if basePlatform == "GCP" {
			filename = "techpreview.hostedclusters.gcp.testsuite.yaml"
			fixtureName = "When GCP project ID has invalid format it should fail"
		}
		raw, err := os.ReadFile(filepath.Join(publishingAssetsDir(), "tests", "hostedclusters.hypershift.openshift.io", filename))
		Expect(err).NotTo(HaveOccurred())
		fixture := SuiteSpec{}
		Expect(kyaml.Unmarshal(raw, &fixture)).To(Succeed())
		for _, test := range fixture.Tests.OnCreate {
			if test.Name != fixtureName {
				continue
			}
			object := map[string]interface{}{}
			Expect(kyaml.Unmarshal([]byte(test.Initial), &object)).To(Succeed())
			result, found, err := unstructured.NestedMap(object, "spec", "platform")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			if basePlatform == "GCP" {
				Expect(unstructured.SetNestedField(result, "my-project-123", "gcp", "project")).To(Succeed())
			}
			if strings.HasSuffix(platform, "Private") {
				field := "endpointAccess"
				if basePlatform == "Azure" {
					field = "topology"
				}
				Expect(unstructured.SetNestedField(result, "Private", strings.ToLower(basePlatform), field)).To(Succeed())
			}
			return result
		}
		Fail("publishing platform fixture not found: " + fixtureName)
	}
	result := map[string]interface{}{"type": basePlatform}
	return result
}

func publishingAssetsDir() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..", "cmd", "install", "assets", "crds", "hypershift-operator")
}

func TestLoadCRDFromFilePublishingSchema(t *testing.T) {
	for _, resource := range []string{"hostedclusters", "hostedcontrolplanes"} {
		for _, featureSet := range []string{"Default", "TechPreviewNoUpgrade", "DevPreviewNoUpgrade", "CustomNoUpgrade"} {
			t.Run("When "+resource+" uses "+featureSet+", it should expose only enabled publishing schema", func(t *testing.T) {
				crd, err := loadCRDFromFile(filepath.Join(publishingAssetsDir(), "zz_generated.crd-manifests", resource+"-Hypershift-"+featureSet+".crd.yaml"))
				if err != nil {
					t.Fatal(err)
				}
				for _, version := range crd.Spec.Versions {
					spec := version.Schema.OpenAPIV3Schema.Properties["spec"]
					publishing, found := spec.Properties["publishing"]
					if found != (featureSet == "DevPreviewNoUpgrade" || featureSet == "CustomNoUpgrade") {
						t.Fatalf("publishing schema presence = %v for %s", found, featureSet)
					}
					if !found {
						for _, validation := range spec.XValidations {
							if strings.Contains(validation.Rule, ".publishing") {
								t.Errorf("disabled gate retains publishing CEL: %s", validation.Rule)
							}
						}
						continue
					}
					if len(publishing.Properties["type"].Enum) != 3 {
						t.Error("publishing must advertise exactly three topology presets")
					}
					assertPublishingServiceArms(t, publishing)
					assertPublishingNodePortEndpoints(t, publishing)
				}
			})
		}
	}
}

func TestPerTestRuntimeInfoPublishingFeatureGate(t *testing.T) {
	for _, resource := range []string{"hostedclusters", "hostedcontrolplanes"} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("When %s requires ServicePublishingAPI=%t, it should select matching schemas", resource, enabled), func(t *testing.T) {
				gate := "ServicePublishingAPI"
				if !enabled {
					gate = "-" + gate
				}
				info, err := perTestRuntimeInfo(filepath.Join(publishingAssetsDir(), "tests", resource+".hypershift.openshift.io"), resource+".hypershift.openshift.io", []string{gate})
				if err != nil {
					t.Fatal(err)
				}
				if len(info.CRDFilenames) == 0 {
					t.Fatal("no matching CRDs selected")
				}
				for _, filename := range info.CRDFilenames {
					crd, err := loadCRDFromFile(filename)
					if err != nil {
						t.Fatal(err)
					}
					for _, version := range crd.Spec.Versions {
						_, found := version.Schema.OpenAPIV3Schema.Properties["spec"].Properties["publishing"]
						if found != enabled {
							t.Errorf("%s publishing schema presence = %t, gate selection = %t", filename, found, enabled)
						}
					}
				}
			})
		}
	}
}

// assertPublishingServiceArms checks the list semantics, name enums and hostname grammar each
// publishing arm exposes.
func assertPublishingServiceArms(t *testing.T, publishing apiextensionsv1.JSONSchemaProps) {
	t.Helper()
	for _, arm := range []string{"singleIngressPoint", "dedicatedAPIServerEndpoint", "nodePort"} {
		services := publishing.Properties[arm].Properties["services"]
		if services.XListType == nil || *services.XListType != "map" || len(services.XListMapKeys) != 1 || services.XListMapKeys[0] != "name" {
			t.Errorf("%s services must be a map list keyed by name", arm)
		}
		maxItems := int64(4)
		if arm == "dedicatedAPIServerEndpoint" {
			maxItems = 2
		}
		if services.MaxItems == nil || *services.MaxItems != maxItems {
			t.Errorf("%s services must be capped at %d", arm, maxItems)
		}
		if services.Items == nil || services.Items.Schema == nil || len(services.Items.Schema.Properties["name"].Enum) != int(maxItems) {
			t.Fatalf("%s service names must be a closed preset-specific enum", arm)
		}
		if arm == "singleIngressPoint" && (services.MinItems == nil || *services.MinItems != 1) {
			t.Error("SingleIngressPoint services must have at least one entry")
		}
		serviceName := services.Items.Schema.Properties["name"]
		if serviceName.MaxLength == nil || *serviceName.MaxLength != 16 {
			t.Errorf("%s service names must enforce their maximum length", arm)
		}
		if arm != "nodePort" {
			hostname := services.Items.Schema.Properties["hostname"]
			if hostname.MinLength == nil || *hostname.MinLength != 1 || hostname.MaxLength == nil || *hostname.MaxLength != 253 || len(hostname.XValidations) != 1 {
				t.Errorf("%s hostnames must enforce length and format", arm)
			}
			exposure := services.Items.Schema.Properties["exposure"]
			if len(exposure.Enum) != 1 || string(exposure.Enum[0].Raw) != `"DedicatedLoadBalancer"` || exposure.MaxLength == nil || *exposure.MaxLength != 24 {
				t.Errorf("%s exposure must be limited to DedicatedLoadBalancer", arm)
			}
		}
	}
}

// assertPublishingNodePortEndpoints checks the address and port bounds of both NodePort endpoints.
func assertPublishingNodePortEndpoints(t *testing.T, publishing apiextensionsv1.JSONSchemaProps) {
	t.Helper()
	for _, endpoint := range []string{"nodePort", "routerNodePort"} {
		config := publishing.Properties["nodePort"]
		port := config.Properties["services"].Items.Schema.Properties["port"]
		if endpoint == "routerNodePort" {
			config = publishing.Properties["singleIngressPoint"].Properties["nodePort"]
			port = config.Properties["port"]
		}
		address := config.Properties["address"]
		if address.MinLength == nil || *address.MinLength != 1 || address.MaxLength == nil || *address.MaxLength != 253 || len(address.XValidations) == 0 {
			t.Errorf("%s address must enforce length and format", endpoint)
		}
		if port.Minimum == nil || *port.Minimum != 30000 || port.Maximum == nil || *port.Maximum != 32767 {
			t.Errorf("%s ports must enforce the NodePort range", endpoint)
		}
	}
}
