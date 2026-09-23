package hostedcluster

import (
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
)

func TestPayloadDeploymentSpec(t *testing.T) {
	t.Run("When creating the payload controller, it should use two leader-elected replicas", func(t *testing.T) {
		g := NewWithT(t)
		spec := payloadDeploymentSpec(payloadControllerName, "quay.io/hypershift/operator:test", 2, []string{"/usr/bin/ignition-server", "ignition-payload-controller"}, nil)
		g.Expect(*spec.Replicas).To(Equal(int32(2)))
		g.Expect(spec.Template.Spec.ServiceAccountName).To(Equal(payloadControllerName))
		g.Expect(spec.Template.Spec.Containers[0].Command).To(Equal([]string{"/usr/bin/ignition-server", "ignition-payload-controller"}))
	})

	t.Run("When creating the serving tier, it should use three stateless replicas with a TLS readiness probe", func(t *testing.T) {
		g := NewWithT(t)
		spec := payloadDeploymentSpec(payloadServerName, "quay.io/hypershift/operator:test", 3, []string{"/usr/bin/ignition-server", "ignition-payload-server"}, nil)
		g.Expect(*spec.Replicas).To(Equal(int32(3)))
		g.Expect(spec.Template.Spec.Containers[0].ReadinessProbe).ToNot(BeNil())
		g.Expect(spec.Template.Spec.Containers[0].ReadinessProbe.HTTPGet.Scheme).To(Equal(corev1.URISchemeHTTPS))
	})

	t.Run("When creating the public proxy, it should use its own service account and trust the ignition CA", func(t *testing.T) {
		g := NewWithT(t)
		spec := payloadProxyDeploymentSpec("quay.io/openshift/haproxy-router:test")
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
}
