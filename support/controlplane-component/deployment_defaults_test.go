package controlplanecomponent

import (
	"context"
	"testing"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/imageprovider"
	"github.com/openshift/hypershift/support/config"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/google/go-cmp/cmp"
)

type adoptedDeploymentOptions struct{}

func (adoptedDeploymentOptions) IsRequestServing() bool         { return false }
func (adoptedDeploymentOptions) MultiZoneSpread() bool          { return true }
func (adoptedDeploymentOptions) NeedsManagementKASAccess() bool { return true }

func TestApplyDeploymentDefaults(t *testing.T) {
	t.Run("When adopting a Deployment, it should preserve its immutable selector and existing resources", func(t *testing.T) {
		current := &appsv1.Deployment{
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "ignition-server"}},
				Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "ignition-server"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "ignition-server",
					Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("25m"),
					}},
				}}}},
			},
		}
		desired := &appsv1.Deployment{
			Spec: appsv1.DeploymentSpec{
				Replicas: ptr.To[int32](3),
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
					"app":                               "ignition-server",
					hyperv1.ControlPlaneComponentLabel:  "ignition-server",
					config.NeedManagementKASAccessLabel: "true",
				}},
				Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "ignition-server"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "ignition-server", Image: "quay.io/hypershift/operator:test"}}}},
			},
		}
		hcp := &hyperv1.HostedControlPlane{
			ObjectMeta: metav1.ObjectMeta{Namespace: "clusters-example", Name: "example", Annotations: map[string]string{}},
			Spec: hyperv1.HostedControlPlaneSpec{
				ControllerAvailabilityPolicy: hyperv1.HighlyAvailable,
				Platform:                     hyperv1.PlatformSpec{Type: hyperv1.AWSPlatform},
			},
		}
		err := ApplyDeploymentDefaults(ControlPlaneContext{
			Context:              context.Background(),
			HCP:                  hcp,
			ReleaseImageProvider: imageprovider.NewFromImages(nil),
		}, "ignition-server", adoptedDeploymentOptions{}, current, desired)
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(current.Spec.Selector, desired.Spec.Selector); diff != "" {
			t.Fatalf("immutable selector changed (-want +got):\n%s", diff)
		}
		if got := desired.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().String(); got != "25m" {
			t.Fatalf("existing resource request was not preserved: %s", got)
		}
		if desired.Spec.Template.Labels[hyperv1.ControlPlaneComponentLabel] != "ignition-server" {
			t.Fatalf("component defaults were not applied: %#v", desired.Spec.Template.Labels)
		}
		if desired.Spec.Template.Spec.Affinity == nil || desired.Spec.Template.Spec.Affinity.PodAntiAffinity == nil {
			t.Fatal("multi-zone component defaults were not applied")
		}
	})
}
