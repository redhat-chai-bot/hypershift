package ignitionserver

import (
	"testing"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	component "github.com/openshift/hypershift/support/controlplane-component"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPreserveOnDisable(t *testing.T) {
	server := &ignitionServer{}
	hcp := &hyperv1.HostedControlPlane{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		hyperv1.DisableIgnitionServerAnnotation: hyperv1.IgnitionServerHandoffAnnotationValue,
	}}}
	if !server.PreserveOnDisable(component.WorkloadContext{HCP: hcp}) {
		t.Fatal("expected handoff resources to be preserved")
	}
	hcp.Annotations[hyperv1.DisableIgnitionServerAnnotation] = "true"
	if server.PreserveOnDisable(component.WorkloadContext{HCP: hcp}) {
		t.Fatal("expected historical disablement to retain delete behavior")
	}
}

func TestDisableAcknowledgementAnnotation(t *testing.T) {
	if got := (&ignitionServer{}).DisableAcknowledgementAnnotation(); got != hyperv1.IgnitionServerHandoffAcknowledgedAnnotation {
		t.Fatalf("unexpected acknowledgement annotation %q", got)
	}
}
