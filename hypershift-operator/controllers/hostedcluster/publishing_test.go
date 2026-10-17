package hostedcluster

import (
	"context"
	"errors"
	"testing"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	hoFeaturegate "github.com/openshift/hypershift/hypershift-operator/featuregate"
	cpomanifests "github.com/openshift/hypershift/pkg/manifests/cpo"
	"github.com/openshift/hypershift/support/publishing"
	"github.com/openshift/hypershift/support/util/fakeimagemetadataprovider"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/stretchr/testify/require"
)

func hostedClusterPublishingFixture(t *testing.T) *hyperv1.HostedCluster {
	t.Helper()
	hc := &hyperv1.HostedCluster{ObjectMeta: metav1.ObjectMeta{Name: "publishing", Namespace: "clusters", Generation: 4},
		Spec: hyperv1.HostedClusterSpec{Platform: hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform},
			Publishing: hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingSingleIngressPoint,
				SingleIngressPoint: hyperv1.SingleIngressPointPublishing{Exposure: hyperv1.IngressExposureNodePort,
					NodePort: hyperv1.IngressNodePortConfig{Address: "192.0.2.10"},
					Services: []hyperv1.SingleIngressPointService{{Name: "APIServer", Hostname: "api.hcp.example.com"}}}}}}
	return hc
}

func TestReconcileHostedControlPlanePublishing(t *testing.T) {
	t.Run("When a publishing HC requests NodePort on an old CPO, it should error without writing either field", func(t *testing.T) {
		hc := hostedClusterPublishingFixture(t)
		hcp := &hyperv1.HostedControlPlane{}
		require.ErrorContains(t, reconcileHostedControlPlanePublishing(hcp, hc, true, nil), controlPlaneOperatorSupportsRouterNodePortLabel)
		require.Empty(t, hcp.Spec.Publishing.Type)
		require.Nil(t, hcp.Spec.Services)
	})
	testCases := []struct {
		name            string
		legacy          bool
		enabled         bool
		unrepresentable bool
		invalid         bool
		wantPolicy      bool
	}{
		{name: "When a publishing HC targets a new CPO, it should write both fields without HC mutation", enabled: true, wantPolicy: true},
		{name: "When a publishing HC survives gate disablement, it should retain both fields", wantPolicy: true},
		{name: "When a legacy HC has the gate enabled, it should translate without HC mutation", legacy: true, enabled: true, wantPolicy: true},
		{name: "When the gate is disabled for a legacy HC, it should clear prior HCP publishing", legacy: true},
		{name: "When a legacy topology cannot be translated, it should copy legacy services and clear stale publishing", legacy: true, enabled: true, unrepresentable: true},
		{name: "When a publishing policy is invalid, it should return an error without writing HCP services", enabled: true, invalid: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			hc := hostedClusterPublishingFixture(t)
			services, err := publishing.ServicesForHC(hc)
			require.NoError(t, err)
			if testCase.legacy {
				hc.Spec.Publishing = hyperv1.ServicePublishing{}
				hc.Spec.Services = services
			}
			if testCase.unrepresentable {
				hc.Spec.Services = append(hc.Spec.Services, hyperv1.ServicePublishingStrategyMapping{Service: hyperv1.OIDC, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.None}})
			}
			if testCase.invalid {
				hc.Spec.Publishing.SingleIngressPoint.Services = nil
			}
			before := hc.DeepCopy()
			hcp := &hyperv1.HostedControlPlane{Spec: hyperv1.HostedControlPlaneSpec{Publishing: hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingNodePort}}}
			err = reconcileHostedControlPlanePublishing(hcp, hc, testCase.enabled, map[string]string{controlPlaneOperatorSupportsRouterNodePortLabel: "true"})
			if testCase.invalid {
				require.Error(t, err)
				require.Nil(t, hcp.Spec.Services)
			} else {
				require.NoError(t, err)
				require.Equal(t, testCase.wantPolicy, hcp.Spec.Publishing.Type != "")
				if testCase.legacy {
					require.Equal(t, hc.Spec.Services, hcp.Spec.Services)
				} else {
					require.Equal(t, hc.Spec.Publishing, hcp.Spec.Publishing)
					require.NoError(t, publishing.EquivalentServices(services, hcp.Spec.Services))
				}
				if testCase.wantPolicy {
					translated, err := publishing.ToServices(&hcp.Spec.Publishing, hc.Spec.Platform)
					require.NoError(t, err)
					require.NoError(t, publishing.EquivalentServices(hcp.Spec.Services, translated))
				}
				if testCase.legacy {
					hcp.Spec.Services[0].Route.Hostname = "changed.example.com"
				} else {
					hcp.Spec.Publishing.SingleIngressPoint.Services[0].Hostname = "changed.example.com"
				}
			}
			require.Equal(t, before, hc)
		})
	}
	t.Run("When a legacy HC migrates atomically to its translated policy, it should preserve legacy endpoints", func(t *testing.T) {
		hc := hostedClusterPublishingFixture(t)
		hc.Spec.Services, _ = publishing.ServicesForHC(hc)
		hc.Spec.Publishing = hyperv1.ServicePublishing{}
		hcp := &hyperv1.HostedControlPlane{}
		require.NoError(t, reconcileHostedControlPlanePublishing(hcp, hc, true, map[string]string{controlPlaneOperatorSupportsRouterNodePortLabel: "true"}))
		before := hcp.DeepCopy()
		hc.Spec.Services = nil
		hc.Spec.Publishing = *hcp.Spec.Publishing.DeepCopy()
		require.NoError(t, reconcileHostedControlPlanePublishing(hcp, hc, true, map[string]string{controlPlaneOperatorSupportsRouterNodePortLabel: "true"}))
		require.Equal(t, before.Spec, hcp.Spec)
	})
	t.Run("When an HCP policy changes, it should replace both translated fields", func(t *testing.T) {
		hc := hostedClusterPublishingFixture(t)
		hcp := &hyperv1.HostedControlPlane{}
		require.NoError(t, reconcileHostedControlPlanePublishing(hcp, hc, true, map[string]string{controlPlaneOperatorSupportsRouterNodePortLabel: "true"}))
		hc.Spec.Publishing.SingleIngressPoint.Services[0].Hostname = "api.new.example.com"
		require.NoError(t, reconcileHostedControlPlanePublishing(hcp, hc, true, map[string]string{controlPlaneOperatorSupportsRouterNodePortLabel: "true"}))
		require.Equal(t, "api.new.example.com", hcp.Spec.Services[0].Route.Hostname)
	})
}

func TestPublishingConfigurationCondition(t *testing.T) {
	testCases := []struct {
		name            string
		enabled         bool
		mismatch        bool
		missing         bool
		unrepresentable bool
		warning         bool
	}{
		{name: "When translation succeeds, it should report valid configuration", enabled: true},
		{name: "When translation fails, it should report a nonblocking observable warning", enabled: true, unrepresentable: true, warning: true},
		{name: "When HCP fields mismatch, it should report nonblocking legacy fallback", mismatch: true, warning: true},
		{name: "When the gate is disabled, it should not warn about legacy translation", unrepresentable: true},
		{name: "When no HCP exists, it should validate legacy translation without panicking", enabled: true, missing: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			hc := hostedClusterPublishingFixture(t)
			hcp := &hyperv1.HostedControlPlane{Spec: hyperv1.HostedControlPlaneSpec{Platform: hc.Spec.Platform}}
			require.NoError(t, reconcileHostedControlPlanePublishing(hcp, hc, true, map[string]string{controlPlaneOperatorSupportsRouterNodePortLabel: "true"}))
			hc.Spec.Services = hcp.Spec.Services
			hc.Spec.Publishing = hyperv1.ServicePublishing{}
			if testCase.unrepresentable {
				hc.Spec.Services = append(hc.Spec.Services, hyperv1.ServicePublishingStrategyMapping{Service: hyperv1.OIDC})
			}
			if testCase.mismatch {
				hcp.Spec.Publishing.SingleIngressPoint.Services[0].Hostname = "mismatch.example.com"
			}
			if testCase.missing {
				hcp = nil
			}
			condition := publishingConfigurationCondition(hc, hcp, testCase.enabled)
			require.Equal(t, metav1.ConditionTrue, condition.Status)
			require.Equal(t, string(hyperv1.ValidHostedClusterConfiguration), condition.Type)
			require.Equal(t, hc.Generation, condition.ObservedGeneration)
			if testCase.warning {
				require.Equal(t, "ServicePublishingTranslationWarning", condition.Reason)
				require.Contains(t, condition.Message, "authoritative legacy services")
			} else {
				require.Equal(t, hyperv1.AsExpectedReason, condition.Reason)
			}
		})
	}
}

func TestValidateRouterNodePortCapability(t *testing.T) {
	testCases := []struct {
		name      string
		label     string
		exposure  hyperv1.IngressExposureType
		legacy    bool
		wantError bool
		platform  hyperv1.PlatformType
	}{
		{name: "When a CPO lacks the router NodePort capability, it should return an error not fallback", exposure: hyperv1.IngressExposureNodePort, wantError: true},
		{name: "When a CPO disables the router NodePort capability, it should return an error", label: "false", exposure: hyperv1.IngressExposureNodePort, wantError: true},
		{name: "When a CPO supports router NodePort, it should accept the topology", label: "true", exposure: hyperv1.IngressExposureNodePort},
		{name: "When a CPO is old but ingress is LoadBalancer, it should allow back-translated services", exposure: hyperv1.IngressExposureLoadBalancer},
		{name: "When Agent ingress is PlatformProvided on an old CPO, it should reject unsafe ownership translation", exposure: hyperv1.IngressExposurePlatformProvided, wantError: true},
		{name: "When Agent ingress is PlatformProvided on a capable CPO, it should preserve management ingress", exposure: hyperv1.IngressExposurePlatformProvided, label: "true"},
		{name: "When a HC uses legacy services, it should not require router NodePort", legacy: true},
		{name: "When an old IBM CPO uses PlatformProvided, it should retain the existing platform path", exposure: hyperv1.IngressExposurePlatformProvided, platform: hyperv1.IBMCloudPlatform},
		{name: "When an old ARO CPO uses PlatformProvided, it should retain shared ingress", exposure: hyperv1.IngressExposurePlatformProvided, platform: hyperv1.AzurePlatform},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			hc := hostedClusterPublishingFixture(t)
			hc.Spec.Publishing.SingleIngressPoint.Exposure = testCase.exposure
			if testCase.platform != "" {
				hc.Spec.Platform.Type = testCase.platform
			}
			if testCase.legacy {
				hc.Spec.Publishing = hyperv1.ServicePublishing{}
			}
			err := validateRouterNodePortCapability(hc, map[string]string{controlPlaneOperatorSupportsRouterNodePortLabel: testCase.label})
			if testCase.wantError {
				require.ErrorContains(t, err, controlPlaneOperatorSupportsRouterNodePortLabel)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestIgnitionRouterNodePortEndpoint(t *testing.T) {
	testCases := []struct {
		name       string
		port       int32
		portName   string
		hostname   string
		exposure   hyperv1.IngressExposureType
		missing    bool
		fetchError bool
		staleLB    bool
		want       string
	}{
		{name: "When HTTPS NodePort is allocated, it should use the route hostname and allocated port", port: 31443, portName: "https", hostname: "ignition.hcp.example.com", exposure: hyperv1.IngressExposureNodePort, want: "ignition.hcp.example.com:31443"},
		{name: "When status already has a port, it should refresh without duplicating it", port: 31443, portName: "https", hostname: "ignition.hcp.example.com:30443", exposure: hyperv1.IngressExposureNodePort, want: "ignition.hcp.example.com:31443"},
		{name: "When router service is missing, it should wait without advertising port 443", missing: true, hostname: "ignition.hcp.example.com", exposure: hyperv1.IngressExposureNodePort},
		{name: "When HTTPS NodePort is unallocated, it should wait", portName: "https", hostname: "ignition.hcp.example.com", exposure: hyperv1.IngressExposureNodePort},
		{name: "When a stale LoadBalancer still has its backing NodePort, it should wait", port: 31443, portName: "https", hostname: "ignition.hcp.example.com", exposure: hyperv1.IngressExposureNodePort, staleLB: true},
		{name: "When only HTTP NodePort is allocated, it should wait", port: 30080, portName: "http", hostname: "ignition.hcp.example.com", exposure: hyperv1.IngressExposureNodePort},
		{name: "When the route hostname is empty, it should wait", port: 31443, portName: "https", exposure: hyperv1.IngressExposureNodePort},
		{name: "When ingress is LoadBalancer, it should leave the hostname unchanged", missing: true, hostname: "ignition.hcp.example.com", exposure: hyperv1.IngressExposureLoadBalancer, want: "ignition.hcp.example.com"},
		{name: "When the router service cannot be read, it should return the API error", fetchError: true, exposure: hyperv1.IngressExposureNodePort},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			hc := hostedClusterPublishingFixture(t)
			hc.Spec.Publishing.SingleIngressPoint.Exposure = testCase.exposure
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			objects := []client.Object{}
			if !testCase.missing {
				svc := cpomanifests.RouterPublicService("clusters-publishing")
				svc.Spec.Type = corev1.ServiceTypeNodePort
				if testCase.staleLB {
					svc.Spec.Type = corev1.ServiceTypeLoadBalancer
				}
				svc.Spec.Ports = []corev1.ServicePort{{Name: testCase.portName, NodePort: testCase.port}}
				objects = append(objects, svc)
			}
			builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...)
			if testCase.fetchError {
				builder.WithInterceptorFuncs(interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return errors.New("service read failed")
				}})
			}
			reconciler := &HostedClusterReconciler{Client: builder.Build()}
			endpoint, err := reconciler.ignitionRouterNodePortEndpoint(context.Background(), hc, "clusters-publishing", testCase.hostname)
			if testCase.fetchError {
				require.ErrorContains(t, err, "service read failed")
				return
			}
			require.NoError(t, err)
			require.Equal(t, testCase.want, endpoint)
		})
	}
}

func TestServicePublishingAPIEnabled(t *testing.T) {
	require.Equal(t, hoFeaturegate.Gate().Enabled(hoFeaturegate.ServicePublishingAPI), servicePublishingAPIEnabled())
}

func TestValidateRouterNodePortImageCapability(t *testing.T) {
	testCases := []struct {
		name             string
		labels           string
		missingSecret    bool
		metadataFailure  bool
		legacy           bool
		wantError        string
		platformProvided bool
	}{
		{name: "When image labels advertise router NodePort, it should pass status validation", labels: controlPlaneOperatorSupportsRouterNodePortLabel + "=true"},
		{name: "When an old CPO lacks router NodePort, it should fail status validation", labels: "version=4.21", wantError: controlPlaneOperatorSupportsRouterNodePortLabel},
		{name: "When the pull secret is missing, it should fail capability verification", missingSecret: true, wantError: "cannot verify router NodePort capability"},
		{name: "When image metadata is unavailable, it should fail capability verification", metadataFailure: true, wantError: "cannot verify router NodePort image labels"},
		{name: "When a HC has no publishing policy, it should not require image metadata", missingSecret: true, legacy: true},
		{name: "When Agent PlatformProvided uses an unlabeled image, it should reject unsafe translation", platformProvided: true, labels: "version=4.21", wantError: controlPlaneOperatorSupportsRouterNodePortLabel},
		{name: "When Agent PlatformProvided uses a false capability label, it should reject unsafe translation", platformProvided: true, labels: controlPlaneOperatorSupportsRouterNodePortLabel + "=false", wantError: controlPlaneOperatorSupportsRouterNodePortLabel},
		{name: "When Agent PlatformProvided uses a capable image, it should retain management ingress", platformProvided: true, labels: controlPlaneOperatorSupportsRouterNodePortLabel + "=true"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			hc := hostedClusterPublishingFixture(t)
			hc.Spec.PullSecret.Name = "pull-secret"
			if testCase.platformProvided {
				hc.Spec.Publishing.SingleIngressPoint.Exposure = hyperv1.IngressExposurePlatformProvided
				hc.Spec.Publishing.SingleIngressPoint.NodePort = hyperv1.IngressNodePortConfig{}
			}
			hc.Annotations = map[string]string{hyperv1.ControlPlaneOperatorImageAnnotation: "quay.io/hypershift/hypershift-operator:latest"}
			if testCase.labels != "" {
				hc.Annotations[hyperv1.ControlPlaneOperatorImageLabelsAnnotation] = testCase.labels
			}
			if testCase.legacy {
				hc.Spec.Publishing = hyperv1.ServicePublishing{}
			}
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			objects := []client.Object{}
			if !testCase.missingSecret {
				objects = append(objects, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "pull-secret", Namespace: hc.Namespace}, Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte("{}")}})
			}
			registryProvider := fakeReleaseProvider{}
			if testCase.metadataFailure {
				registryProvider.metadataProvider = fakeimagemetadataprovider.FakeRegistryClientImageMetadataProvider{Err: errors.New("metadata unavailable")}
			}
			reconciler := &HostedClusterReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(), RegistryProvider: registryProvider}
			err := reconciler.validateRouterNodePortImageCapability(context.Background(), hc)
			if testCase.wantError != "" {
				require.ErrorContains(t, err, testCase.wantError)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
