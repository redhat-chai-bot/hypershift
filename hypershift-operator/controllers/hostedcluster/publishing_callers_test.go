package hostedcluster

import (
	"testing"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/api/util/ipnet"
	hoFeaturegate "github.com/openshift/hypershift/hypershift-operator/featuregate"
	"github.com/openshift/hypershift/support/api"
	"github.com/openshift/hypershift/support/capabilities"
	fakecapabilities "github.com/openshift/hypershift/support/capabilities/fake"
	"github.com/openshift/hypershift/support/publishing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/stretchr/testify/require"
)

func TestValidateConfigAndClusterCapabilitiesPublishing(t *testing.T) {
	for _, testCase := range []struct {
		name             string
		imageLabel       string
		noRoutes         bool
		malformed        bool
		address          string
		emptyAddress     bool
		want             string
		platformProvided bool
	}{
		{name: "When the CPO supports router NodePort, it should validate publishing at the parent", imageLabel: "true"},
		{name: "When the CPO label is absent, it should reject router NodePort at the parent", want: controlPlaneOperatorSupportsRouterNodePortLabel},
		{name: "When the CPO label is false, it should reject router NodePort at the parent", imageLabel: "false", want: controlPlaneOperatorSupportsRouterNodePortLabel},
		{name: "When Routes are unavailable, it should reject the translated Route at the parent", imageLabel: "true", noRoutes: true, want: "cluster does not support Routes"},
		{name: "When publishing is malformed, it should report the policy error at the parent", imageLabel: "true", malformed: true, want: "unsupported"},
		{name: "When direct NodePort overlaps the service network, it should reject the address at the parent", address: "172.30.0.10", want: "Nodeport IP is within the service network"},
		{name: "When direct NodePort has no address, it should reject publishing at the parent", emptyAddress: true, want: "address"},
		{name: "When direct NodePort uses a separate IPv4 address, it should validate it at the parent", address: "192.0.2.10"},
		{name: "When direct NodePort uses a separate IPv6 address, it should validate it at the parent", address: "2001:db8::10"},
		{name: "When Agent PlatformProvided targets an unlabeled CPO, it should reject unsafe ownership at the parent", platformProvided: true, want: controlPlaneOperatorSupportsRouterNodePortLabel},
		{name: "When Agent PlatformProvided targets a false capability label, it should reject unsafe ownership at the parent", platformProvided: true, imageLabel: "false", want: controlPlaneOperatorSupportsRouterNodePortLabel},
		{name: "When Agent PlatformProvided targets a capable CPO, it should validate at the parent", platformProvided: true, imageLabel: "true"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			hc := hostedClusterPublishingFixture(t)
			hc.Spec.Platform = hyperv1.PlatformSpec{Type: hyperv1.NonePlatform}
			if testCase.platformProvided {
				hc.Spec.Platform.Type = hyperv1.AgentPlatform
				hc.Spec.Platform.Agent = &hyperv1.AgentPlatformSpec{AgentNamespace: "agents"}
				hc.Spec.Publishing.SingleIngressPoint.Exposure = hyperv1.IngressExposurePlatformProvided
				hc.Spec.Publishing.SingleIngressPoint.NodePort = hyperv1.IngressNodePortConfig{}
			}
			hc.Spec.Networking.ClusterNetwork = []hyperv1.ClusterNetworkEntry{{CIDR: *ipnet.MustParseCIDR("10.128.0.0/14")}}
			hc.Spec.Networking.ServiceNetwork = []hyperv1.ServiceNetworkEntry{{CIDR: *ipnet.MustParseCIDR("172.30.0.0/16")}}
			hc.Spec.PullSecret.Name = "pull-secret"
			hc.Annotations = map[string]string{hyperv1.ControlPlaneOperatorImageAnnotation: "quay.io/hypershift/hypershift-operator:latest", hyperv1.ControlPlaneOperatorImageLabelsAnnotation: "version=4.21"}
			if testCase.imageLabel != "" {
				hc.Annotations[hyperv1.ControlPlaneOperatorImageLabelsAnnotation] += "," + controlPlaneOperatorSupportsRouterNodePortLabel + "=" + testCase.imageLabel
			}
			if testCase.address != "" || testCase.emptyAddress {
				hc.Spec.Publishing = hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingNodePort, NodePort: hyperv1.NodePortPublishing{Address: testCase.address}}
			}
			if testCase.malformed {
				hc.Spec.Publishing.Type = "Invalid"
			}
			clusterClient := fake.NewClientBuilder().WithScheme(api.Scheme).WithObjects(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "pull-secret", Namespace: hc.Namespace}, Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte("{}")}}, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "agents"}}).Build()
			reconciler := &HostedClusterReconciler{Client: clusterClient, RegistryProvider: fakeReleaseProvider{}, ManagementClusterCapabilities: &fakecapabilities.FakeSupportAllCapabilities{}}
			if testCase.noRoutes {
				reconciler.ManagementClusterCapabilities = fakecapabilities.NewSupportAllExcept(capabilities.CapabilityRoute)
			}
			err := reconciler.validateConfigAndClusterCapabilities(t.Context(), hc)
			if testCase.want != "" {
				require.ErrorContains(t, err, testCase.want)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestReconcileHostedControlPlaneLegacyPublishingGateMatrix(t *testing.T) {
	previous := hoFeaturegate.FeatureSet()
	t.Cleanup(func() { hoFeaturegate.ConfigureFeatureSet(string(previous)) })
	for _, featureSet := range []string{"Default", "DevPreviewNoUpgrade"} {
		for _, dualHC := range []bool{false, true} {
			t.Run("When "+featureSet+" copies legacy publishing, it should preserve authority and gate translation", func(t *testing.T) {
				hoFeaturegate.ConfigureFeatureSet(featureSet)
				hc := hostedClusterPublishingFixture(t)
				hc.Spec.Publishing.SingleIngressPoint.Exposure = hyperv1.IngressExposureLoadBalancer
				hc.Spec.Publishing.SingleIngressPoint.NodePort = hyperv1.IngressNodePortConfig{}
				hc.Spec.Publishing.SingleIngressPoint.LoadBalancer = &hyperv1.IngressLoadBalancerConfig{}
				services, err := publishing.ServicesForHC(hc)
				require.NoError(t, err)
				hc.Spec.Services = services
				if !dualHC {
					hc.Spec.Publishing = hyperv1.ServicePublishing{}
				}
				before := hc.DeepCopy()
				hcp := &hyperv1.HostedControlPlane{}
				require.NoError(t, reconcileHostedControlPlane(hcp, hc, false, false, func() (map[string]string, error) { return nil, nil }))
				require.Equal(t, before, hc)
				require.Equal(t, services, hcp.Spec.Services)
				if featureSet == "DevPreviewNoUpgrade" || dualHC {
					require.NotEmpty(t, hcp.Spec.Publishing.Type)
				} else {
					require.Empty(t, hcp.Spec.Publishing.Type)
				}
			})
		}
	}
}

func TestPublishingParentControlPlaneCopy(t *testing.T) {
	for _, platform := range []hyperv1.PlatformSpec{
		{Type: hyperv1.IBMCloudPlatform},
		{Type: hyperv1.AzurePlatform, Azure: &hyperv1.AzurePlatformSpec{Topology: hyperv1.AzureTopologyPublic, AzureAuthenticationConfig: hyperv1.AzureAuthenticationConfiguration{AzureAuthenticationConfigType: hyperv1.AzureAuthenticationTypeManagedIdentities}}},
	} {
		t.Run("When an old CPO uses "+string(platform.Type)+" PlatformProvided, it should retain its existing ingress topology", func(t *testing.T) {
			hc := hostedClusterPublishingFixture(t)
			hc.Spec.Platform = platform
			hc.Spec.Publishing.SingleIngressPoint.Exposure = hyperv1.IngressExposurePlatformProvided
			hc.Spec.Publishing.SingleIngressPoint.NodePort = hyperv1.IngressNodePortConfig{}
			hcp := &hyperv1.HostedControlPlane{ObjectMeta: metav1.ObjectMeta{Name: "hcp", Namespace: "clusters-publishing"}}
			clusterClient := fake.NewClientBuilder().WithScheme(api.Scheme).WithObjects(hcp).Build()
			_, err := controllerutil.CreateOrUpdate(t.Context(), clusterClient, hcp, func() error {
				return reconcileHostedControlPlane(hcp, hc, false, false, func() (map[string]string, error) { return nil, nil }, nil)
			})
			require.NoError(t, err)
			stored := &hyperv1.HostedControlPlane{}
			require.NoError(t, clusterClient.Get(t.Context(), client.ObjectKeyFromObject(hcp), stored))
			require.Equal(t, hc.Spec.Publishing, stored.Spec.Publishing)
			services, err := publishing.ServicesForHC(hc)
			require.NoError(t, err)
			require.Equal(t, services, stored.Spec.Services)
		})
	}
	previousFeatureSet := hoFeaturegate.FeatureSet()
	t.Cleanup(func() { hoFeaturegate.ConfigureFeatureSet(string(previousFeatureSet)) })
	for _, featureSet := range []string{"DevPreviewNoUpgrade", "Default"} {
		hoFeaturegate.ConfigureFeatureSet(featureSet)
		for _, capability := range []string{"true", "false", ""} {
			for _, exposure := range []hyperv1.IngressExposureType{hyperv1.IngressExposureNodePort, hyperv1.IngressExposurePlatformProvided, hyperv1.IngressExposureLoadBalancer} {
				t.Run("When "+featureSet+" targets CPO capability "+capability+" for "+string(exposure)+", it should atomically apply only supported publishing", func(t *testing.T) {
					hc := hostedClusterPublishingFixture(t)
					hc.Spec.Publishing.SingleIngressPoint.Exposure = exposure
					if exposure != hyperv1.IngressExposureNodePort {
						hc.Spec.Publishing.SingleIngressPoint.NodePort = hyperv1.IngressNodePortConfig{}
					}
					if exposure == hyperv1.IngressExposureLoadBalancer {
						hc.Spec.Publishing.SingleIngressPoint.LoadBalancer = &hyperv1.IngressLoadBalancerConfig{}
					}
					hcp := &hyperv1.HostedControlPlane{}
					hcp.Name, hcp.Namespace = "hcp", "clusters-publishing"
					clusterClient := fake.NewClientBuilder().WithScheme(api.Scheme).WithObjects(hcp).Build()
					_, err := controllerutil.CreateOrUpdate(t.Context(), clusterClient, hcp, func() error {
						return reconcileHostedControlPlane(hcp, hc, false, false, func() (map[string]string, error) { return nil, nil }, map[string]string{controlPlaneOperatorSupportsRouterNodePortLabel: capability})
					})
					stored := &hyperv1.HostedControlPlane{}
					require.NoError(t, clusterClient.Get(t.Context(), client.ObjectKeyFromObject(hcp), stored))
					if capability != "true" && exposure != hyperv1.IngressExposureLoadBalancer {
						require.ErrorContains(t, err, controlPlaneOperatorSupportsRouterNodePortLabel)
						require.Empty(t, stored.Spec.Publishing.Type)
						require.Empty(t, stored.Spec.Services)
						return
					}
					require.NoError(t, err)
					require.Equal(t, hc.Spec.Publishing, stored.Spec.Publishing)
					services, err := publishing.ServicesForHC(hc)
					require.NoError(t, err)
					require.Equal(t, services, stored.Spec.Services)
				})
			}
		}
	}
}

func TestPublishingParentNodePortRange(t *testing.T) {
	for _, port := range []int32{0, 29999, 30000, 32767, 32768} {
		t.Run("When direct publishing selects a port, it should validate the parent range", func(t *testing.T) {
			hc := hostedClusterPublishingFixture(t)
			hc.Spec.Publishing = hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingNodePort, NodePort: hyperv1.NodePortPublishing{Address: "192.0.2.10", Services: []hyperv1.NodePortPublishedService{{Name: "APIServer", Port: port}}}}
			if port == 0 {
				hc.Spec.Publishing.NodePort.Services[0].Port = 0
			}
			require.Equal(t, port != 0 && (port < 30000 || port > 32767), len(validateNodePortPortRange(hc)) > 0)
		})
	}
	hc := hostedClusterPublishingFixture(t)
	hc.Spec.Publishing.Type = "Invalid"
	require.NotEmpty(t, validateNodePortPortRange(hc))
}
