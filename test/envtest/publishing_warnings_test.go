//go:build envtest

package envtest

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/openshift/hypershift/cmd/install/assets"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	crdenvtest "sigs.k8s.io/controller-runtime/pkg/envtest"
)

type publishingWarningRecorder struct {
	lock     sync.Mutex
	warnings []string
}

func (recorder *publishingWarningRecorder) HandleWarningHeader(code int, agent, text string) {
	if code != 299 {
		return
	}
	recorder.lock.Lock()
	defer recorder.lock.Unlock()
	recorder.warnings = append(recorder.warnings, text)
}

func (recorder *publishingWarningRecorder) reset() {
	recorder.lock.Lock()
	defer recorder.lock.Unlock()
	recorder.warnings = nil
}

func (recorder *publishingWarningRecorder) hasLegacyWarning() bool {
	recorder.lock.Lock()
	defer recorder.lock.Unlock()
	for _, warning := range recorder.warnings {
		if strings.Contains(warning, "The field spec.services is deprecated; use spec.publishing instead") {
			return true
		}
	}
	return false
}

var _ = Describe("ServicePublishingAPI publishing warnings [OCPFeatureGate:ServicePublishingAPI]", Ordered, func() {
	var warningClient client.Client
	var recorder *publishingWarningRecorder
	var installed crdenvtest.CRDInstallOptions
	resource := func(kind, name, policy string, legacy bool) *unstructured.Unstructured {
		object := &unstructured.Unstructured{}
		Expect(json.Unmarshal([]byte(publishingResource(kind, policy, "None", legacy)), &object.Object)).To(Succeed())
		object.SetName(name)
		object.SetNamespace("publishing-warnings")
		return object
	}
	BeforeAll(func() {
		installed = crdenvtest.CRDInstallOptions{Paths: []string{
			filepath.Join(publishingAssetsDir(), "zz_generated.crd-manifests", "hostedclusters-Hypershift-DevPreviewNoUpgrade.crd.yaml"),
			filepath.Join(publishingAssetsDir(), "zz_generated.crd-manifests", "hostedcontrolplanes-Hypershift-DevPreviewNoUpgrade.crd.yaml"),
		}}
		_, err := crdenvtest.InstallCRDs(cfg, installed)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "publishing-warnings"}})).To(Succeed())
		recorder = &publishingWarningRecorder{}
		warningConfig := rest.CopyConfig(cfg)
		warningConfig.WarningHandler = recorder
		warningClient, err = client.New(warningConfig, client.Options{})
		Expect(err).NotTo(HaveOccurred())
		for iteration := 0; iteration < 2; iteration++ {
			policy := assets.HyperShiftDeprecationPolicy{}.Build()
			_, err := controllerutil.CreateOrUpdate(ctx, k8sClient, policy, func() error { policy.Spec = assets.HyperShiftDeprecationPolicy{}.Build().Spec; return nil })
			Expect(err).NotTo(HaveOccurred())
			binding := assets.HyperShiftDeprecationPolicyBinding{}.Build()
			_, err = controllerutil.CreateOrUpdate(ctx, k8sClient, binding, func() error { binding.Spec = assets.HyperShiftDeprecationPolicyBinding{}.Build().Spec; return nil })
			Expect(err).NotTo(HaveOccurred())
			Expect(binding.Spec.ValidationActions).To(Equal([]admissionv1.ValidationAction{admissionv1.Warn}))
		}
		Eventually(func() bool {
			recorder.reset()
			if err := warningClient.Create(ctx, resource("HostedCluster", "probe", "", true), client.DryRunAll); err != nil {
				return false
			}
			return recorder.hasLegacyWarning()
		}, 30*time.Second, 100*time.Millisecond).Should(BeTrue(), "the actual API server must activate its existing Warn-only policy before checking absence")
	})
	AfterAll(func() {
		Expect(k8sClient.Delete(ctx, assets.HyperShiftDeprecationPolicyBinding{}.Build())).To(Succeed())
		Expect(k8sClient.Delete(ctx, assets.HyperShiftDeprecationPolicy{}.Build())).To(Succeed())
		Expect(crdenvtest.UninstallCRDs(cfg, installed)).To(Succeed())
	})
	It("When legacy HC writes succeed, it should return HTTP warnings on create and update and stop warning after atomic adoption", func() {
		object := resource("HostedCluster", "legacy", "", true)
		recorder.reset()
		Expect(warningClient.Create(ctx, object)).To(Succeed())
		Expect(recorder.hasLegacyWarning()).To(BeTrue())
		object.SetAnnotations(map[string]string{"publishing-test": "update"})
		recorder.reset()
		Expect(warningClient.Update(ctx, object)).To(Succeed())
		Expect(recorder.hasLegacyWarning()).To(BeTrue())
		unstructured.RemoveNestedField(object.Object, "spec", "services")
		Expect(unstructured.SetNestedField(object.Object, map[string]interface{}{"type": "DedicatedAPIServerEndpoint"}, "spec", "publishing")).To(Succeed())
		recorder.reset()
		Expect(warningClient.Update(ctx, object)).To(Succeed())
		Expect(recorder.hasLegacyWarning()).To(BeFalse())
	})
	It("When publishing-only HC writes succeed, it should not return legacy HTTP warnings", func() {
		object := resource("HostedCluster", "publishing-only", `{"type":"DedicatedAPIServerEndpoint"}`, false)
		recorder.reset()
		Expect(warningClient.Create(ctx, object)).To(Succeed())
		Expect(recorder.hasLegacyWarning()).To(BeFalse())
		object.SetAnnotations(map[string]string{"publishing-test": "update"})
		recorder.reset()
		Expect(warningClient.Update(ctx, object)).To(Succeed())
		Expect(recorder.hasLegacyWarning()).To(BeFalse())
	})
	It("When an HCP has coexisting translated and legacy fields, it should allow writes and return the legacy HTTP warning", func() {
		object := resource("HostedControlPlane", "coexistence", `{"type":"DedicatedAPIServerEndpoint"}`, true)
		recorder.reset()
		Expect(warningClient.Create(ctx, object)).To(Succeed())
		Expect(recorder.hasLegacyWarning()).To(BeTrue())
		object.SetAnnotations(map[string]string{"publishing-test": "update"})
		recorder.reset()
		Expect(warningClient.Update(ctx, object)).To(Succeed())
		Expect(recorder.hasLegacyWarning()).To(BeTrue())
		unstructured.RemoveNestedField(object.Object, "spec", "services")
		recorder.reset()
		Expect(warningClient.Update(ctx, object)).To(Succeed())
		Expect(recorder.hasLegacyWarning()).To(BeFalse())
	})
})
