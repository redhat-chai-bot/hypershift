package hostedcluster

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/imageprovider"
	ignitionservercomponent "github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/v2/ignitionserver"
	ignitionserverproxycomponent "github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/v2/ignitionserver_proxy"
	hyperapi "github.com/openshift/hypershift/support/api"
	"github.com/openshift/hypershift/support/config"
	controlplanecomponent "github.com/openshift/hypershift/support/controlplane-component"

	routev1 "github.com/openshift/api/route/v1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestApplyPayloadDeploymentDefaults(t *testing.T) {
	t.Run("When adopting the legacy server Deployment, it should retain its selector and apply HA spread defaults", func(t *testing.T) {
		g := NewWithT(t)
		hcp := &hyperv1.HostedControlPlane{
			ObjectMeta: metav1.ObjectMeta{Namespace: "clusters-example", Name: "example", Annotations: map[string]string{}},
			Spec: hyperv1.HostedControlPlaneSpec{
				ControllerAvailabilityPolicy: hyperv1.HighlyAvailable,
				Platform:                     hyperv1.PlatformSpec{Type: hyperv1.AWSPlatform},
			},
		}
		current := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": payloadServerName}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": payloadServerName}}},
		}}
		desired := &appsv1.Deployment{Spec: payloadDeploymentSpec(payloadServerName, "quay.io/hypershift/operator:test", 3, []string{"/usr/bin/ignition-server", "ignition-payload-server"}, nil)}
		err := applyPayloadDeploymentDefaults(controlplanecomponent.ControlPlaneContext{
			Context:              t.Context(),
			HCP:                  hcp,
			ReleaseImageProvider: imageprovider.NewFromImages(nil),
		}, payloadServerName, payloadControllerDeploymentOptions{}, current, desired)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(desired.Spec.Selector).To(Equal(current.Spec.Selector))
		g.Expect(desired.Spec.Template.Labels).To(HaveKeyWithValue(config.NeedManagementKASAccessLabel, "true"))
		g.Expect(desired.Spec.Template.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution).NotTo(BeEmpty())
		g.Expect(desired.Spec.Strategy.RollingUpdate.MaxUnavailable.IntValue()).To(Equal(1))
		g.Expect(desired.Spec.Template.Spec.SecurityContext.RunAsUser).To(BeNil())
		g.Expect(desired.Spec.Template.Spec.SecurityContext.RunAsNonRoot).To(Equal(ptr.To(true)))
	})

	t.Run("When request-serving topology is dedicated, it should isolate the proxy through component defaults", func(t *testing.T) {
		g := NewWithT(t)
		hcp := &hyperv1.HostedControlPlane{
			ObjectMeta: metav1.ObjectMeta{Namespace: "clusters-example", Name: "example", Annotations: map[string]string{hyperv1.TopologyAnnotation: hyperv1.DedicatedRequestServingComponentsTopology}},
			Spec: hyperv1.HostedControlPlaneSpec{
				ControllerAvailabilityPolicy: hyperv1.HighlyAvailable,
				Platform:                     hyperv1.PlatformSpec{Type: hyperv1.AWSPlatform},
			},
		}
		desired := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](2),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": payloadProxyName}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": payloadProxyName}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "haproxy", Image: "quay.io/openshift/haproxy-router:test"}}}},
		}}
		err := applyPayloadDeploymentDefaults(controlplanecomponent.ControlPlaneContext{
			Context:              t.Context(),
			HCP:                  hcp,
			ReleaseImageProvider: imageprovider.NewFromImages(nil),
		}, payloadProxyName, ignitionserverproxycomponent.DeploymentOptions(), nil, desired)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(desired.Spec.Template.Labels).To(HaveKeyWithValue(hyperv1.RequestServingComponentLabel, "true"))
		g.Expect(desired.Spec.Template.Labels).NotTo(HaveKey(config.NeedManagementKASAccessLabel))
		g.Expect(desired.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution).NotTo(BeNil())
		g.Expect(desired.Spec.Template.Spec.AutomountServiceAccountToken).To(Equal(ptr.To(false)))
	})
}

func TestPayloadDeploymentSpec(t *testing.T) {
	t.Run("When the control plane is single replica, it should collapse payload workloads to one replica", func(t *testing.T) {
		hcp := &hyperv1.HostedControlPlane{Spec: hyperv1.HostedControlPlaneSpec{ControllerAvailabilityPolicy: hyperv1.SingleReplica}}
		if got := payloadReplicas(hcp, 3); got != 1 {
			t.Fatalf("expected one replica, got %d", got)
		}
	})

	t.Run("When creating the payload controller, it should use two leader-elected replicas", func(t *testing.T) {
		g := NewWithT(t)
		spec := payloadDeploymentSpec(payloadControllerName, "quay.io/hypershift/operator:test", 2, []string{"/usr/bin/ignition-server", "ignition-payload-controller"}, nil)
		g.Expect(*spec.Replicas).To(Equal(int32(2)))
		g.Expect(spec.Template.Spec.ServiceAccountName).To(Equal(payloadControllerName))
		g.Expect(spec.Template.Spec.Containers[0].Command).To(Equal([]string{"/usr/bin/ignition-server", "ignition-payload-controller"}))
		g.Expect(spec.Template.Spec.Containers[0].ReadinessProbe.HTTPGet.Path).To(Equal("/readyz"))
	})

	t.Run("When creating the serving tier, it should use three stateless replicas with a TLS readiness probe", func(t *testing.T) {
		g := NewWithT(t)
		spec := payloadDeploymentSpec(payloadServerName, "quay.io/hypershift/operator:test", 3, []string{"/usr/bin/ignition-server", "ignition-payload-server"}, nil)
		g.Expect(*spec.Replicas).To(Equal(int32(3)))
		g.Expect(spec.Template.Spec.Containers[0].ReadinessProbe).ToNot(BeNil())
		g.Expect(spec.Template.Spec.Containers[0].ReadinessProbe.HTTPGet.Path).To(Equal("/readyz"))
		g.Expect(spec.Template.Spec.Containers[0].ReadinessProbe.HTTPGet.Scheme).To(Equal(corev1.URISchemeHTTPS))
	})

	t.Run("When creating the public proxy, it should use its own service account and trust the ignition CA", func(t *testing.T) {
		g := NewWithT(t)
		spec := payloadProxyDeploymentSpec("quay.io/openshift/haproxy-router:test", 2)
		g.Expect(spec.Template.Spec.ServiceAccountName).To(Equal(payloadProxyName))
		g.Expect(spec.Template.Spec.Containers[0].Image).To(Equal("quay.io/openshift/haproxy-router:test"))
		g.Expect(spec.Template.Spec.Volumes).To(ContainElement(WithTransform(func(v corev1.Volume) string { return v.Name }, Equal("root-ca"))))
	})
}

func TestIgnitionPayloadRules(t *testing.T) {
	t.Run("When configuring the renderer role, it should grant lease and payload-store write access", func(t *testing.T) {
		g := NewWithT(t)
		rules := ignitionPayloadRules(true)
		var leaseWrite, secretWrite bool
		for _, rule := range rules {
			if len(rule.APIGroups) > 0 && rule.APIGroups[0] == "coordination.k8s.io" {
				leaseWrite = true
			}
			if len(rule.Resources) > 0 && rule.Resources[0] == "secrets" && len(rule.Verbs) > 3 {
				secretWrite = true
			}
		}
		g.Expect(leaseWrite).To(BeTrue())
		g.Expect(secretWrite).To(BeTrue())
	})

	t.Run("When configuring disjoint writers, it should forbid server status update and controller-wide status ownership", func(t *testing.T) {
		for _, controller := range []bool{false, true} {
			for _, rule := range ignitionPayloadRules(controller) {
				for _, resource := range rule.Resources {
					if resource != "ignitionpayloads/status" {
						continue
					}
					for _, verb := range rule.Verbs {
						if verb == "update" || verb == "*" {
							t.Fatalf("status rule grants forbidden verb %q: %#v", verb, rule)
						}
					}
				}
			}
		}
	})
}

func TestPrepareIgnitionPayloadHandoff(t *testing.T) {
	ctx := context.Background()
	hc := &hyperv1.HostedCluster{ObjectMeta: metav1.ObjectMeta{Namespace: "clusters", Name: "example"}}
	hcp := &hyperv1.HostedControlPlane{
		ObjectMeta: metav1.ObjectMeta{Namespace: "clusters-example", Name: "example"},
		Spec:       hyperv1.HostedControlPlaneSpec{Platform: hyperv1.PlatformSpec{Type: hyperv1.AWSPlatform}},
	}
	c := fake.NewClientBuilder().WithScheme(hyperapi.Scheme).WithObjects(hc).Build()
	r := &HostedClusterReconciler{Client: c}

	t.Run("When handoff has not been requested, it should persist the request and wait", func(t *testing.T) {
		prepared, err := r.prepareIgnitionPayloadHandoff(ctx, hc, hcp)
		if err != nil {
			t.Fatal(err)
		}
		if prepared {
			t.Fatal("handoff was prepared before CPO acknowledgement")
		}
		live := &hyperv1.HostedCluster{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(hc), live); err != nil {
			t.Fatal(err)
		}
		if live.Annotations[hyperv1.DisableIgnitionServerAnnotation] != payloadCutoverAnnotationValue {
			t.Fatalf("handoff request was not persisted: %#v", live.Annotations)
		}
		hc = live
	})

	t.Run("When only the server acknowledged, it should wait for the proxy", func(t *testing.T) {
		hcp.Annotations = map[string]string{
			hyperv1.DisableIgnitionServerAnnotation:             payloadCutoverAnnotationValue,
			hyperv1.IgnitionServerHandoffAcknowledgedAnnotation: "true",
		}
		prepared, err := r.prepareIgnitionPayloadHandoff(ctx, hc, hcp)
		if err != nil {
			t.Fatal(err)
		}
		if prepared {
			t.Fatal("handoff was prepared before proxy acknowledgement")
		}
	})

	t.Run("When both CPO components acknowledged, it should allow HO reconciliation", func(t *testing.T) {
		hcp.Annotations[hyperv1.IgnitionServerProxyHandoffAcknowledgedAnnotation] = "true"
		prepared, err := r.prepareIgnitionPayloadHandoff(ctx, hc, hcp)
		if err != nil {
			t.Fatal(err)
		}
		if !prepared {
			t.Fatal("handoff did not become prepared after both acknowledgements")
		}
	})
}

func TestPrepareIgnitionPayloadHandoffClearsStaleAcknowledgements(t *testing.T) {
	hc := &hyperv1.HostedCluster{ObjectMeta: metav1.ObjectMeta{Namespace: "clusters", Name: "example"}}
	hcp := &hyperv1.HostedControlPlane{ObjectMeta: metav1.ObjectMeta{
		Namespace: "clusters-example",
		Name:      "example",
		Annotations: map[string]string{
			hyperv1.IgnitionServerHandoffAcknowledgedAnnotation:      "true",
			hyperv1.IgnitionServerProxyHandoffAcknowledgedAnnotation: "true",
		},
	}}
	c := fake.NewClientBuilder().WithScheme(hyperapi.Scheme).WithObjects(hc, hcp).Build()
	r := &HostedClusterReconciler{Client: c}

	t.Run("When a previous handoff left acknowledgements, it should clear them before requesting another handoff", func(t *testing.T) {
		prepared, err := r.prepareIgnitionPayloadHandoff(t.Context(), hc, hcp)
		if err != nil {
			t.Fatal(err)
		}
		if prepared {
			t.Fatal("handoff was prepared from stale acknowledgements")
		}
		live := &hyperv1.HostedControlPlane{}
		if err := c.Get(t.Context(), client.ObjectKeyFromObject(hcp), live); err != nil {
			t.Fatal(err)
		}
		if live.Annotations[hyperv1.IgnitionServerHandoffAcknowledgedAnnotation] != "" || live.Annotations[hyperv1.IgnitionServerProxyHandoffAcknowledgedAnnotation] != "" {
			t.Fatalf("stale acknowledgements remain: %#v", live.Annotations)
		}
	})
}

func TestServiceHasReadyEndpoint(t *testing.T) {
	const (
		namespace = "clusters-example"
		name      = "ignition-server"
	)

	t.Run("When the EndpointSlice has no ready endpoint, it should keep the replacement gated", func(t *testing.T) {
		service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
		slice := &discoveryv1.EndpointSlice{
			ObjectMeta:  metav1.ObjectMeta{Namespace: namespace, Name: name + "-abc", Labels: map[string]string{discoveryv1.LabelServiceName: name}},
			AddressType: discoveryv1.AddressTypeIPv4,
			Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(false)}}},
		}
		r := &HostedClusterReconciler{Client: fake.NewClientBuilder().WithScheme(hyperapi.Scheme).WithObjects(service, slice).Build()}

		if err := r.serviceHasReadyEndpoint(t.Context(), namespace, name); err == nil {
			t.Fatal("expected an unready EndpointSlice to keep cutover gated")
		}
	})

	t.Run("When the EndpointSlice has a ready endpoint, it should allow replacement cutover", func(t *testing.T) {
		service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
		slice := &discoveryv1.EndpointSlice{
			ObjectMeta:  metav1.ObjectMeta{Namespace: namespace, Name: name + "-abc", Labels: map[string]string{discoveryv1.LabelServiceName: name}},
			AddressType: discoveryv1.AddressTypeIPv4,
			Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}}},
		}
		r := &HostedClusterReconciler{Client: fake.NewClientBuilder().WithScheme(hyperapi.Scheme).WithObjects(service, slice).Build()}

		if err := r.serviceHasReadyEndpoint(t.Context(), namespace, name); err != nil {
			t.Fatalf("expected a ready EndpointSlice to allow cutover: %v", err)
		}
	})
}

func TestServiceHasCurrentReadyEndpoints(t *testing.T) {
	const namespace = "clusters-example"
	desiredContainer := corev1.Container{Name: payloadServerName, Image: "hypershift:new", Command: []string{"ignition-server", "ignition-payload-server"}}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: payloadServerName},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To[int32](1), Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{desiredContainer}}}},
	}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: payloadServerName}}
	newPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "new"}, Spec: corev1.PodSpec{Containers: []corev1.Container{desiredContainer}}}
	oldPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "old"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: payloadServerName, Image: "release-cpo:old", Command: []string{"ignition-server"}}}}}

	t.Run("When an old revision remains a ready endpoint, it should keep CR token cutover gated", func(t *testing.T) {
		slice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "mixed", Labels: map[string]string{discoveryv1.LabelServiceName: payloadServerName}}, AddressType: discoveryv1.AddressTypeIPv4, Endpoints: []discoveryv1.Endpoint{
			{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}, TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: newPod.Name}},
			{Addresses: []string{"10.0.0.2"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}, TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: oldPod.Name}},
		}}
		r := &HostedClusterReconciler{Client: fake.NewClientBuilder().WithScheme(hyperapi.Scheme).WithObjects(deployment, service, newPod, oldPod, slice).Build()}
		if err := r.serviceHasCurrentReadyEndpoints(t.Context(), namespace, payloadServerName, payloadServerName); err == nil {
			t.Fatal("mixed serving revisions unexpectedly passed cutover")
		}
	})

	t.Run("When every ready endpoint runs the new revision, it should allow CR token cutover", func(t *testing.T) {
		slice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "current", Labels: map[string]string{discoveryv1.LabelServiceName: payloadServerName}}, AddressType: discoveryv1.AddressTypeIPv4, Endpoints: []discoveryv1.Endpoint{
			{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}, TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: newPod.Name}},
		}}
		r := &HostedClusterReconciler{Client: fake.NewClientBuilder().WithScheme(hyperapi.Scheme).WithObjects(deployment, service, newPod, slice).Build()}
		if err := r.serviceHasCurrentReadyEndpoints(t.Context(), namespace, payloadServerName, payloadServerName); err != nil {
			t.Fatalf("current serving revision remained gated: %v", err)
		}
	})
}

func TestIBMIgnitionPublishingParity(t *testing.T) {
	const namespace = "clusters-example"
	hcp := &hyperv1.HostedControlPlane{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "example", UID: types.UID("hcp-uid")},
		Spec: hyperv1.HostedControlPlaneSpec{
			Platform: hyperv1.PlatformSpec{Type: hyperv1.IBMCloudPlatform},
			Services: []hyperv1.ServicePublishingStrategyMapping{{Service: hyperv1.Ignition, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.NodePort, NodePort: &hyperv1.NodePortPublishingStrategy{Port: 32001}}}},
		},
	}

	t.Run("When IBM publishing changes from Route to NodePort, it should delete the obsolete owned Route", func(t *testing.T) {
		route := &routev1.Route{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: payloadServerName, OwnerReferences: []metav1.OwnerReference{{APIVersion: hyperv1.GroupVersion.String(), Kind: "HostedControlPlane", Name: hcp.Name, UID: hcp.UID, Controller: ptr.To(true)}}}}
		c := fake.NewClientBuilder().WithScheme(hyperapi.Scheme).WithObjects(hcp, route).Build()
		r := &HostedClusterReconciler{Client: c}
		if err := r.reconcilePayloadRoute(t.Context(), hcp, nil); err != nil {
			t.Fatal(err)
		}
		if err := c.Get(t.Context(), client.ObjectKeyFromObject(route), &routev1.Route{}); err == nil {
			t.Fatal("obsolete Route still exists")
		}
	})

	t.Run("When IBM Route adoption finds a NodePort Service, it should preserve the allocated port", func(t *testing.T) {
		hcpRoute := hcp.DeepCopy()
		hcpRoute.Spec.Services[0].ServicePublishingStrategy = hyperv1.ServicePublishingStrategy{Type: hyperv1.Route}
		existing := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: payloadServerName}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort, Ports: []corev1.ServicePort{{Port: 443, NodePort: 32001}}}}
		c := fake.NewClientBuilder().WithScheme(hyperapi.Scheme).WithObjects(existing).Build()
		desired := existing.DeepCopy()
		desired.Spec.Type = corev1.ServiceTypeClusterIP
		desired.Spec.Ports[0].NodePort = 0
		if err := ignitionservercomponent.AdaptService(controlplanecomponent.ControlPlaneContext{Context: t.Context(), Client: c, HCP: hcpRoute}, desired); err != nil {
			t.Fatal(err)
		}
		if desired.Spec.Type != corev1.ServiceTypeNodePort || desired.Spec.Ports[0].NodePort != 32001 {
			t.Fatalf("existing IBM NodePort allocation was not preserved: %#v", desired.Spec)
		}
	})
}

func TestReconcileIgnitionPayloadCutoverPersistsReadiness(t *testing.T) {
	const namespace = "clusters-example"
	hcp := &hyperv1.HostedControlPlane{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "example"}, Spec: hyperv1.HostedControlPlaneSpec{
		Platform: hyperv1.PlatformSpec{Type: hyperv1.IBMCloudPlatform},
		Services: []hyperv1.ServicePublishingStrategyMapping{{Service: hyperv1.Ignition, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.NodePort, NodePort: &hyperv1.NodePortPublishingStrategy{Port: 32001}}}},
	}}
	deployment := func(name string, container corev1.Container) *appsv1.Deployment {
		return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}, Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](1), Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{container}}}}, Status: appsv1.DeploymentStatus{ObservedGeneration: 0, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}}
	}
	controller := deployment(payloadControllerName, corev1.Container{Name: payloadControllerName, Image: "operator:new", Command: []string{"ignition-server", "ignition-payload-controller"}})
	serverContainer := corev1.Container{Name: payloadServerName, Image: "operator:new", Command: []string{"ignition-server", "ignition-payload-server"}}
	serverDeployment := deployment(payloadServerName, serverContainer)
	serverPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "server-new"}, Spec: corev1.PodSpec{Containers: []corev1.Container{serverContainer}}}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: payloadServerName}}
	slice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "server", Labels: map[string]string{discoveryv1.LabelServiceName: payloadServerName}}, AddressType: discoveryv1.AddressTypeIPv4, Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}, TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: serverPod.Name}}}}
	cert := func(name string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}, Data: map[string][]byte{corev1.TLSCertKey: []byte("cert"), corev1.TLSPrivateKeyKey: []byte("key")}}
	}
	c := fake.NewClientBuilder().WithScheme(hyperapi.Scheme).WithObjects(hcp, controller, serverDeployment, serverPod, service, slice, cert("ignition-server-ca-cert"), cert("ignition-server-serving-cert")).Build()
	r := &HostedClusterReconciler{Client: c}
	ready, err := r.reconcileIgnitionPayloadCutover(t.Context(), hcp)
	if err != nil || !ready {
		t.Fatalf("cutover did not become ready: ready=%t err=%v", ready, err)
	}
	live := &hyperv1.HostedControlPlane{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(hcp), live); err != nil {
		t.Fatal(err)
	}
	if live.Annotations[hyperv1.IgnitionPayloadHandoffReadyAnnotation] != "true" {
		t.Fatalf("durable handoff readiness was not persisted: %#v", live.Annotations)
	}
}
