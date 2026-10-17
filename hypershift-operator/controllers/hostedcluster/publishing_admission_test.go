//go:build envtest

package hostedcluster

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	hoFeaturegate "github.com/openshift/hypershift/hypershift-operator/featuregate"
	"github.com/openshift/hypershift/support/api"
	"github.com/openshift/hypershift/support/netutil"
	"github.com/openshift/hypershift/support/publishing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/stretchr/testify/require"
)

// publishingProjectionCases exercises every preset and endpoint spelling under both runtime gate
// states and returns the next case index.
func publishingProjectionCases(t *testing.T, apiClient client.Client, enabledSchema bool, caseIndex int) int {
	t.Helper()
	for _, gate := range []string{"DevPreviewNoUpgrade", "Default"} {
		if gate == "Default" {
			hoFeaturegate.ConfigureFeatureSet("")
		} else {
			hoFeaturegate.ConfigureFeatureSet(gate)
		}
		require.Equal(t, gate == "DevPreviewNoUpgrade", servicePublishingAPIEnabled())
		for _, endpoint := range []string{"api", "node01", "localhost", "node.example.com", "192.0.2.10", "2001:db8::10", "::ffff:192.0.2.10", "2001:db8:0:0:0:0:192.0.2.10", "bad_host", "999.999.999.999"} {
			for _, preset := range []hyperv1.ServicePublishingType{hyperv1.ServicePublishingNodePort, hyperv1.ServicePublishingSingleIngressPoint, hyperv1.ServicePublishingDedicatedAPIServerEndpoint} {
				if preset != hyperv1.ServicePublishingNodePort && endpoint != "api" && endpoint != "node01" && endpoint != "localhost" && endpoint != "node.example.com" && endpoint != "bad_host" {
					continue
				}
				caseIndex++
				t.Run(fmt.Sprintf("When %s projects %s with runtime gate %s, it should enforce the selected schema contract", preset, endpoint, gate), func(t *testing.T) {
					hc := publishingAdmissionCluster(t, fmt.Sprintf("projection-%d", caseIndex))
					hc.Spec.Publishing = hyperv1.ServicePublishing{Type: preset}
					switch preset {
					case hyperv1.ServicePublishingNodePort:
						hc.Spec.Publishing.NodePort.Address = endpoint
					case hyperv1.ServicePublishingSingleIngressPoint:
						hc.Spec.Publishing.SingleIngressPoint = hyperv1.SingleIngressPointPublishing{Exposure: hyperv1.IngressExposureLoadBalancer, LoadBalancer: &hyperv1.IngressLoadBalancerConfig{}, Services: []hyperv1.SingleIngressPointService{{Name: "APIServer", Hostname: endpoint}, {Name: "OAuthServer", Hostname: endpoint}}}
					case hyperv1.ServicePublishingDedicatedAPIServerEndpoint:
						hc.Spec.Publishing.DedicatedAPIServerEndpoint = &hyperv1.DedicatedAPIServerEndpointPublishing{Services: []hyperv1.DedicatedAPIServerEndpointService{{Name: "APIServer", Hostname: endpoint}, {Name: "OAuthServer", Hostname: endpoint}}}
					}
					invalid := endpoint == "bad_host" || endpoint == "999.999.999.999"
					if enabledSchema {
						err := apiClient.Create(t.Context(), hc)
						if invalid {
							require.Error(t, err)
						} else {
							require.NoError(t, err)
						}
					}
					hcp := &hyperv1.HostedControlPlane{ObjectMeta: metav1.ObjectMeta{Name: hc.Name, Namespace: hc.Namespace}}
					require.NoError(t, reconcileHostedControlPlane(hcp, hc, false, false, func() (map[string]string, error) { return nil, nil }, nil))
					require.Equal(t, hc.Spec.Publishing, hcp.Spec.Publishing)
					require.NotEmpty(t, hcp.Spec.Services)
					require.NoError(t, publishing.HCPTranslationError(hcp))
					err := apiClient.Create(t.Context(), hcp)
					legacyAccepted := endpoint == "localhost" && preset == hyperv1.ServicePublishingNodePort || endpoint == "node.example.com" || endpoint == "192.0.2.10" || endpoint == "2001:db8::10" || endpoint == "999.999.999.999"
					if enabledSchema && invalid {
						require.Error(t, err)
						return
					}
					if enabledSchema || legacyAccepted {
						require.NoError(t, err)
						stored := &hyperv1.HostedControlPlane{}
						require.NoError(t, apiClient.Get(t.Context(), client.ObjectKeyFromObject(hcp), stored))
						require.Equal(t, hcp.Spec.Services, stored.Spec.Services)
						if enabledSchema {
							require.Equal(t, hcp.Spec.Publishing, stored.Spec.Publishing)
							require.NoError(t, publishing.HCPTranslationError(stored))
						} else {
							require.Empty(t, stored.Spec.Publishing.Type)
						}
					} else {
						require.Error(t, err)
					}
				})
			}
		}
	}
	return caseIndex
}

// publishingCapabilityCases exercises the Agent capability-label contract and returns the next case index.
func publishingCapabilityCases(t *testing.T, apiClient client.Client, enabledSchema bool, caseIndex int) int {
	t.Helper()
	if enabledSchema {
		for _, gate := range []string{"DevPreviewNoUpgrade", "Default"} {
			if gate == "Default" {
				hoFeaturegate.ConfigureFeatureSet("")
			} else {
				hoFeaturegate.ConfigureFeatureSet(gate)
			}
			require.Equal(t, gate == "DevPreviewNoUpgrade", servicePublishingAPIEnabled())
			for _, exposure := range []hyperv1.IngressExposureType{hyperv1.IngressExposureNodePort, hyperv1.IngressExposurePlatformProvided, hyperv1.IngressExposureLoadBalancer} {
				for _, capability := range []string{"true", "false", ""} {
					caseIndex++
					t.Run(fmt.Sprintf("When Agent %s uses capability %q with gate %s, it should persist only supported actual parent output", exposure, capability, gate), func(t *testing.T) {
						hc := publishingAdmissionCluster(t, fmt.Sprintf("capability-%d", caseIndex))
						hc.Spec.Platform = hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform, Agent: &hyperv1.AgentPlatformSpec{AgentNamespace: "agents"}}
						hc.Spec.Publishing = hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingSingleIngressPoint, SingleIngressPoint: hyperv1.SingleIngressPointPublishing{Exposure: exposure, Services: []hyperv1.SingleIngressPointService{{Name: "APIServer", Hostname: "api"}}}}
						if exposure == hyperv1.IngressExposureNodePort {
							hc.Spec.Publishing.SingleIngressPoint.NodePort.Address = "::ffff:192.0.2.10"
						}
						if exposure == hyperv1.IngressExposureLoadBalancer {
							hc.Spec.Publishing.SingleIngressPoint.LoadBalancer = &hyperv1.IngressLoadBalancerConfig{}
						}
						require.NoError(t, apiClient.Create(t.Context(), hc))
						hcp := &hyperv1.HostedControlPlane{ObjectMeta: metav1.ObjectMeta{Name: hc.Name, Namespace: hc.Namespace}}
						_, err := controllerutil.CreateOrUpdate(t.Context(), apiClient, hcp, func() error {
							return reconcileHostedControlPlane(hcp, hc, false, false, func() (map[string]string, error) { return nil, nil }, map[string]string{controlPlaneOperatorSupportsRouterNodePortLabel: capability})
						})
						stored := &hyperv1.HostedControlPlane{}
						if capability != "true" && exposure != hyperv1.IngressExposureLoadBalancer {
							require.ErrorContains(t, err, controlPlaneOperatorSupportsRouterNodePortLabel)
							require.Error(t, apiClient.Get(t.Context(), client.ObjectKeyFromObject(hcp), stored))
							return
						}
						require.NoError(t, err)
						require.NoError(t, apiClient.Get(t.Context(), client.ObjectKeyFromObject(hcp), stored))
						require.Equal(t, hc.Spec.Publishing, stored.Spec.Publishing)
						require.NoError(t, publishing.HCPTranslationError(stored))
					})
				}
			}
		}
	}
	return caseIndex
}

// publishingLegacyAddressCases exercises the legacy NodePort address grammar and the atomic
// migration path, and returns the next case index.
func publishingLegacyAddressCases(t *testing.T, apiClient client.Client, enabledSchema bool, caseIndex int) int {
	t.Helper()
	for _, endpoint := range []string{"node01", "localhost", "node.example.com", "192.0.2.10", "2001:db8::10", "::ffff:c000:20a", "::ffff:192.0.2.10", "2001:db8:0:0:0:0:192.0.2.10", "bad_host", "999.999.999.999"} {
		caseIndex++
		t.Run("When legacy services use "+endpoint+", it should retain the original address contract", func(t *testing.T) {
			hc := publishingAdmissionCluster(t, fmt.Sprintf("legacy-%d", caseIndex))
			hc.Spec.Services = publishingAdmissionLegacyServices(endpoint)
			hcp := &hyperv1.HostedControlPlane{ObjectMeta: metav1.ObjectMeta{Name: hc.Name, Namespace: hc.Namespace}}
			hoFeaturegate.ConfigureFeatureSet("")
			require.False(t, servicePublishingAPIEnabled())
			require.NoError(t, reconcileHostedControlPlane(hcp, hc, false, false, func() (map[string]string, error) { return nil, nil }, nil))
			accepted := endpoint == "localhost" || endpoint == "node.example.com" || endpoint == "192.0.2.10" || endpoint == "2001:db8::10" || endpoint == "::ffff:c000:20a" || endpoint == "999.999.999.999"
			for _, object := range []client.Object{hc, hcp} {
				err := apiClient.Create(t.Context(), object)
				if accepted {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
			}
		})
	}
	if enabledSchema {
		t.Run("When an HC atomically adopts publishing, it should persist equivalent mutable HCP output and retain legacy mismatch authority", func(t *testing.T) {
			hoFeaturegate.ConfigureFeatureSet("DevPreviewNoUpgrade")
			hc := publishingAdmissionCluster(t, "migration")
			hc.Spec.Services = publishingAdmissionLegacyServices("node.example.com")
			require.NoError(t, apiClient.Create(t.Context(), hc))
			hc.Spec.Services = nil
			hc.Spec.Publishing = hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingNodePort, NodePort: hyperv1.NodePortPublishing{Address: "node01"}}
			require.NoError(t, apiClient.Update(t.Context(), hc))
			hcp := &hyperv1.HostedControlPlane{ObjectMeta: metav1.ObjectMeta{Name: "migration", Namespace: hc.Namespace}}
			require.NoError(t, reconcileHostedControlPlane(hcp, hc, false, false, func() (map[string]string, error) { return nil, nil }, nil))
			require.NoError(t, apiClient.Create(t.Context(), hcp))
			require.NoError(t, apiClient.Get(t.Context(), client.ObjectKeyFromObject(hcp), hcp))
			require.NoError(t, publishing.HCPTranslationError(hcp))
			invalid := hcp.DeepCopy()
			invalid.Spec.Services[0].NodePort.Address = "bad_host"
			require.Error(t, apiClient.Update(t.Context(), invalid))
			hcp.Spec.Services[0].NodePort.Address = "legacy.example.com"
			require.NoError(t, apiClient.Update(t.Context(), hcp))
			require.NoError(t, apiClient.Get(t.Context(), client.ObjectKeyFromObject(hcp), hcp))
			require.Error(t, publishing.HCPTranslationError(hcp))
			require.Nil(t, netutil.PublishingForHCP(hcp))
			services, err := publishing.ServicesForHCP(hcp)
			require.NoError(t, err)
			require.Equal(t, "legacy.example.com", services[0].NodePort.Address)
			hcp.Spec.Publishing.NodePort.Address = "mutable.example.com"
			require.NoError(t, apiClient.Update(t.Context(), hcp))
			hc.Spec.Publishing.NodePort.Address = "changed.example.com"
			require.Error(t, apiClient.Update(t.Context(), hc))
			hcp.Spec.Publishing = hyperv1.ServicePublishing{}
			require.Error(t, apiClient.Update(t.Context(), hcp))
		})
	}
	return caseIndex
}

// publishingLegacyHostnameCases exercises the legacy Route and LoadBalancer hostname grammar.
func publishingLegacyHostnameCases(t *testing.T, apiClient client.Client, caseIndex int) {
	t.Helper()
	for _, strategyType := range []hyperv1.PublishingStrategyType{hyperv1.Route, hyperv1.LoadBalancer} {
		for _, hostname := range []string{"api", "localhost", "api.example.com", "-legacy.example.com", "bad_host"} {
			caseIndex++
			t.Run(fmt.Sprintf("When legacy %s uses %s, it should retain the original hostname contract", strategyType, hostname), func(t *testing.T) {
				hc := publishingAdmissionCluster(t, fmt.Sprintf("legacy-host-%d", caseIndex))
				hc.Spec.Services = publishingAdmissionLegacyServices("node.example.com")
				strategy := hyperv1.ServicePublishingStrategy{Type: strategyType}
				if strategyType == hyperv1.Route {
					strategy.Route = &hyperv1.RoutePublishingStrategy{Hostname: hostname}
				} else {
					strategy.LoadBalancer = &hyperv1.LoadBalancerPublishingStrategy{Hostname: hostname}
				}
				hc.Spec.Services[0].ServicePublishingStrategy = strategy
				hoFeaturegate.ConfigureFeatureSet("")
				require.False(t, servicePublishingAPIEnabled())
				hcp := &hyperv1.HostedControlPlane{ObjectMeta: metav1.ObjectMeta{Name: hc.Name, Namespace: hc.Namespace}}
				require.NoError(t, reconcileHostedControlPlane(hcp, hc, false, false, func() (map[string]string, error) { return nil, nil }, nil))
				for _, object := range []client.Object{hc, hcp} {
					err := apiClient.Create(t.Context(), object)
					if hostname == "api.example.com" || hostname == "-legacy.example.com" {
						require.NoError(t, err)
					} else {
						require.Error(t, err)
					}
				}
			})
		}
	}
}

func publishingAdmissionCluster(t *testing.T, name string) *hyperv1.HostedCluster {
	t.Helper()
	hc := &hyperv1.HostedCluster{}
	require.NoError(t, yaml.Unmarshal([]byte(`spec:
  release:
    image: quay.io/openshift-release-dev/ocp-release:4.21.10-x86_64
  pullSecret:
    name: pull-secret
  sshKey:
    name: ssh-key
  issuerURL: https://issuer.example.com
  infraID: publishing-admission
  dns:
    baseDomain: example.com
  platform:
    type: None
  etcd:
    managementType: Managed
    managed:
      storage:
        type: PersistentVolume
        persistentVolume:
          size: 8Gi
`), hc))
	hc.Name, hc.Namespace = name, "publishing-admission"
	return hc
}

// publishingAdmissionCRDPath locates a generated feature-set CRD manifest.
func publishingAdmissionCRDPath(resource, schemaSet string) string {
	return filepath.Join("..", "..", "..", "cmd", "install", "assets", "crds", "hypershift-operator", "zz_generated.crd-manifests", resource+"-Hypershift-"+schemaSet+".crd.yaml")
}

func publishingAdmissionLegacyServices(address string) []hyperv1.ServicePublishingStrategyMapping {
	services := []hyperv1.ServicePublishingStrategyMapping{}
	for _, name := range []hyperv1.ServiceType{hyperv1.APIServer, hyperv1.OAuthServer, hyperv1.Konnectivity, hyperv1.Ignition} {
		services = append(services, hyperv1.ServicePublishingStrategyMapping{Service: name, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.NodePort, NodePort: &hyperv1.NodePortPublishingStrategy{Address: address}}})
	}
	return services
}

func TestReconcileHostedControlPlaneAdmission(t *testing.T) {
	previousFeatureSet := hoFeaturegate.FeatureSet()
	t.Cleanup(func() { hoFeaturegate.ConfigureFeatureSet(string(previousFeatureSet)) })
	for _, schemaSet := range []string{"DevPreviewNoUpgrade", "CustomNoUpgrade", "Default", "TechPreviewNoUpgrade", "OKD"} {
		t.Run("When the schema is "+schemaSet+", it should preserve projection and legacy admission contracts", func(t *testing.T) {
			paths := []string{}
			for _, resource := range []string{"hostedclusters", "hostedcontrolplanes"} {
				paths = append(paths, publishingAdmissionCRDPath(resource, schemaSet))
			}
			testEnvironment := &envtest.Environment{CRDInstallOptions: envtest.CRDInstallOptions{Paths: paths, ErrorIfPathMissing: true}}
			config, err := testEnvironment.Start()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, testEnvironment.Stop()) })
			apiClient, err := client.New(config, client.Options{Scheme: api.Scheme})
			require.NoError(t, err)
			discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
			require.NoError(t, err)
			version, err := discoveryClient.ServerVersion()
			require.NoError(t, err)
			t.Logf("Actual HO parent output admission: %s schema=%s", version.GitVersion, schemaSet)
			require.NoError(t, apiClient.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "publishing-admission"}}))
			enabledSchema := schemaSet == "DevPreviewNoUpgrade" || schemaSet == "CustomNoUpgrade"
			caseIndex := publishingProjectionCases(t, apiClient, enabledSchema, 0)
			caseIndex = publishingCapabilityCases(t, apiClient, enabledSchema, caseIndex)
			caseIndex = publishingLegacyAddressCases(t, apiClient, enabledSchema, caseIndex)
			publishingLegacyHostnameCases(t, apiClient, caseIndex)
		})
	}
}

// publishingRatchetSuffix is the spec-scope exception the generator appends to
// each lifted legacy endpoint guard so historical legacy-only data keeps the
// original atomic spec.services ratcheting granularity. hack/tools/publishing-source
// is the source of truth; the generator rejects legacy rules that already use
// oldSelf, so this token appears at most once per lifted guard.
const publishingRatchetSuffix = " || (oldSelf.hasValue() && !has(oldSelf.value().publishing)"

// publishingRatchetSchema loads the generated enabled HCP CRD and returns the
// requested variant of it. "repaired" is the generated schema as-is, "original"
// is the pre-repair shape with the lifted guards reverted to unconditional
// spec rules, and "permissive" drops both the lifted guards and the legacy leaf
// endpoint rules so historical data can be seeded as an earlier schema allowed.
func publishingRatchetSchema(t *testing.T, schemaSet, variant string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	data, err := os.ReadFile(publishingAdmissionCRDPath("hostedcontrolplanes", schemaSet))
	require.NoError(t, err)
	crd := &apiextensionsv1.CustomResourceDefinition{}
	require.NoError(t, yaml.Unmarshal(data, crd))
	for i := range crd.Spec.Versions {
		lifted := 0
		specSchema, found := crd.Spec.Versions[i].Schema.OpenAPIV3Schema.Properties["spec"]
		require.True(t, found)
		rules := []apiextensionsv1.ValidationRule{}
		for _, rule := range specSchema.XValidations {
			index := strings.Index(rule.Rule, publishingRatchetSuffix)
			if index < 0 {
				rules = append(rules, rule)
				continue
			}
			lifted++
			switch variant {
			case "repaired":
				rules = append(rules, rule)
			case "original":
				rule.Rule, rule.OptionalOldSelf = rule.Rule[:index], nil
				rules = append(rules, rule)
			case "permissive":
				// Drop the lifted guards entirely.
			default:
				t.Fatalf("unknown schema variant %q", variant)
			}
		}
		specSchema.XValidations = rules
		if variant == "permissive" {
			services := specSchema.Properties["services"].Items.Schema.Properties["servicePublishingStrategy"].Properties
			for arm, field := range map[string]string{"nodePort": "address", "loadBalancer": "hostname", "route": "hostname"} {
				endpoint := services[arm].Properties[field]
				endpoint.XValidations, endpoint.Pattern = nil, ""
				services[arm].Properties[field] = endpoint
			}
		}
		crd.Spec.Versions[i].Schema.OpenAPIV3Schema.Properties["spec"] = specSchema
		require.Equal(t, 3, lifted, "expected the three lifted legacy endpoint guards in %s/%s", schemaSet, crd.Spec.Versions[i].Name)
	}
	return crd
}

// publishingRatchetHCP returns the actual HO parent output for a legacy-only
// HostedCluster using the given endpoint, which is what an older permissive
// schema would have persisted.
func publishingRatchetHCP(t *testing.T, name, endpoint string) *hyperv1.HostedControlPlane {
	t.Helper()
	hc := publishingAdmissionCluster(t, name)
	hc.Spec.Services = publishingAdmissionLegacyServices(endpoint)
	hcp := &hyperv1.HostedControlPlane{ObjectMeta: metav1.ObjectMeta{Name: hc.Name, Namespace: hc.Namespace}}
	require.NoError(t, reconcileHostedControlPlane(hcp, hc, false, false, func() (map[string]string, error) { return nil, nil }, nil))
	require.Empty(t, hcp.Spec.Publishing.Type)
	return hcp
}

func TestHostedControlPlaneLegacyServicesRatcheting(t *testing.T) {
	previousFeatureSet := hoFeaturegate.FeatureSet()
	t.Cleanup(func() { hoFeaturegate.ConfigureFeatureSet(string(previousFeatureSet)) })
	hoFeaturegate.ConfigureFeatureSet("")
	require.False(t, servicePublishingAPIEnabled())
	// Historical endpoints an earlier permissive schema accepted and the current
	// legacy grammar rejects, across all three lifted endpoint arms.
	historical := []struct {
		name    string
		address string
		mutate  func(*hyperv1.HostedControlPlane)
	}{
		{name: "nodePort address node01", address: "node01"},
		{name: "nodePort address bad_host", address: "bad_host"},
		{name: "route hostname bad_host", address: "node01", mutate: func(hcp *hyperv1.HostedControlPlane) {
			hcp.Spec.Services[0].ServicePublishingStrategy = hyperv1.ServicePublishingStrategy{Type: hyperv1.Route, Route: &hyperv1.RoutePublishingStrategy{Hostname: "bad_host"}}
		}},
		{name: "loadBalancer hostname bad_host", address: "node01", mutate: func(hcp *hyperv1.HostedControlPlane) {
			hcp.Spec.Services[0].ServicePublishingStrategy = hyperv1.ServicePublishingStrategy{Type: hyperv1.LoadBalancer, LoadBalancer: &hyperv1.LoadBalancerPublishingStrategy{Hostname: "bad_host"}}
		}},
	}
	for _, schemaSet := range []string{"DevPreviewNoUpgrade", "CustomNoUpgrade"} {
		for _, variant := range []string{"original", "repaired"} {
			behavior := "it should reject unrelated spec updates to historical legacy services"
			if variant == "repaired" {
				behavior = "it should admit unrelated spec updates to historical legacy services"
			}
			t.Run("When the "+variant+" "+schemaSet+" schema replaces a permissive predecessor, "+behavior, func(t *testing.T) {
				testEnvironment := &envtest.Environment{CRDs: []*apiextensionsv1.CustomResourceDefinition{publishingRatchetSchema(t, schemaSet, "permissive")}}
				config, err := testEnvironment.Start()
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, testEnvironment.Stop()) })
				apiClient, err := client.New(config, client.Options{Scheme: api.Scheme})
				require.NoError(t, err)
				discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
				require.NoError(t, err)
				version, err := discoveryClient.ServerVersion()
				require.NoError(t, err)
				t.Logf("Legacy services ratcheting: %s schema=%s variant=%s", version.GitVersion, schemaSet, variant)
				require.NoError(t, apiClient.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "publishing-admission"}}))

				// Seed historical legacy-only data under the permissive predecessor.
				for index, historic := range historical {
					hcp := publishingRatchetHCP(t, fmt.Sprintf("historic-%d", index), historic.address)
					if historic.mutate != nil {
						historic.mutate(hcp)
					}
					require.NoError(t, apiClient.Create(t.Context(), hcp), historic.name)
				}

				// Replace the permissive predecessor with the variant under test.
				replacement := publishingRatchetSchema(t, schemaSet, variant)
				installed := &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: replacement.Name}}
				_, err = controllerutil.CreateOrUpdate(t.Context(), apiClient, installed, func() error {
					installed.Spec = replacement.Spec
					return nil
				})
				require.NoError(t, err)
				probe := 0
				require.NoError(t, wait.PollUntilContextTimeout(t.Context(), 100*time.Millisecond, time.Minute, true, func(ctx context.Context) (bool, error) {
					probe++
					hcp := publishingRatchetHCP(t, fmt.Sprintf("schema-probe-%d", probe), "bad_host")
					err := apiClient.Create(ctx, hcp)
					// Only an Invalid response proves the replacement schema is live. Any other
					// error (old schema still served, conflict, transient connection failure)
					// means keep polling; a successful create means the probe object has to go.
					if apierrors.IsInvalid(err) {
						return true, nil
					}
					if err == nil {
						return false, apiClient.Delete(ctx, hcp)
					}
					t.Logf("schema probe %d create retried: %v", probe, err)
					return false, nil
				}), "replacement schema never rejected a new invalid legacy endpoint")

				// An unrelated spec update with spec.services unchanged must keep the
				// original leaf-scoped ratcheting: the repaired schema admits it, the
				// pre-repair spec-scope guards reject it. This is API-FINAL1-1.
				for index, historic := range historical {
					t.Run("When an unrelated spec field changes for historical "+historic.name+", it should preserve the previous ratcheting", func(t *testing.T) {
						stored := &hyperv1.HostedControlPlane{}
						require.NoError(t, apiClient.Get(t.Context(), client.ObjectKey{Namespace: "publishing-admission", Name: fmt.Sprintf("historic-%d", index)}, stored))
						before := stored.DeepCopy().Spec.Services
						stored.Spec.ReleaseImage = "quay.io/openshift-release-dev/ocp-release:4.21.11-x86_64"
						err := apiClient.Update(t.Context(), stored)
						if variant != "repaired" {
							require.Error(t, err, "pre-repair guards must reproduce the reported regression")
							return
						}
						require.NoError(t, err)
						require.NoError(t, apiClient.Get(t.Context(), client.ObjectKeyFromObject(stored), stored))
						require.Equal(t, "quay.io/openshift-release-dev/ocp-release:4.21.11-x86_64", stored.Spec.ReleaseImage)
						require.Equal(t, before, stored.Spec.Services, "grandfathered services must be preserved verbatim")
					})
				}
				if variant != "repaired" {
					return
				}

				// Negative controls: the exception must not grandfather anything else.
				t.Run("When a historical services array changes, it should reject the whole atomic array", func(t *testing.T) {
					stored := &hyperv1.HostedControlPlane{}
					require.NoError(t, apiClient.Get(t.Context(), client.ObjectKey{Namespace: "publishing-admission", Name: "historic-0"}, stored))
					stored.Spec.Services = append(stored.Spec.Services, hyperv1.ServicePublishingStrategyMapping{Service: hyperv1.OVNSbDb, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.NodePort, NodePort: &hyperv1.NodePortPublishingStrategy{Address: "node.example.com"}}})
					require.Error(t, apiClient.Update(t.Context(), stored))
				})
				t.Run("When a new invalid legacy endpoint is introduced, it should reject the create", func(t *testing.T) {
					require.Error(t, apiClient.Create(t.Context(), publishingRatchetHCP(t, "new-invalid", "node01")))
				})
				t.Run("When publishing is removed leaving legacy-invalid projected values, it should reject the update", func(t *testing.T) {
					hoFeaturegate.ConfigureFeatureSet("DevPreviewNoUpgrade")
					t.Cleanup(func() { hoFeaturegate.ConfigureFeatureSet("") })
					require.True(t, servicePublishingAPIEnabled())
					hc := publishingAdmissionCluster(t, "publishing-removal")
					hc.Spec.Publishing = hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingNodePort, NodePort: hyperv1.NodePortPublishing{Address: "node01"}}
					hcp := &hyperv1.HostedControlPlane{ObjectMeta: metav1.ObjectMeta{Name: hc.Name, Namespace: hc.Namespace}}
					require.NoError(t, reconcileHostedControlPlane(hcp, hc, false, false, func() (map[string]string, error) { return nil, nil }, nil))
					require.NoError(t, apiClient.Create(t.Context(), hcp))
					require.NoError(t, apiClient.Get(t.Context(), client.ObjectKeyFromObject(hcp), hcp))
					hcp.Spec.Publishing = hyperv1.ServicePublishing{}
					require.Error(t, apiClient.Update(t.Context(), hcp))
				})
				t.Run("When only metadata changes on historical data, it should remain admitted", func(t *testing.T) {
					stored := &hyperv1.HostedControlPlane{}
					require.NoError(t, apiClient.Get(t.Context(), client.ObjectKey{Namespace: "publishing-admission", Name: "historic-1"}, stored))
					stored.Labels = map[string]string{"publishing.hypershift.openshift.io/ratchet": "true"}
					require.NoError(t, apiClient.Update(t.Context(), stored))
				})
			})
		}
	}
}
