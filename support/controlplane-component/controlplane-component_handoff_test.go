package controlplanecomponent

import (
	"context"
	"testing"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	hyperapi "github.com/openshift/hypershift/support/api"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type handoffComponentOptions struct{}

func (handoffComponentOptions) IsRequestServing() bool                 { return true }
func (handoffComponentOptions) MultiZoneSpread() bool                  { return false }
func (handoffComponentOptions) NeedsManagementKASAccess() bool         { return false }
func (handoffComponentOptions) PreserveOnDisable(WorkloadContext) bool { return true }
func (handoffComponentOptions) DisableAcknowledgementAnnotation() string {
	return hyperv1.IgnitionServerHandoffAcknowledgedAnnotation
}

func TestControlPlaneWorkloadReconcileAcknowledgesPreservedDisable(t *testing.T) {
	hcp := &hyperv1.HostedControlPlane{ObjectMeta: metav1.ObjectMeta{Namespace: "clusters-example", Name: "example"}}
	c := fake.NewClientBuilder().WithScheme(hyperapi.Scheme).WithObjects(hcp).Build()
	workload := &controlPlaneWorkload[*appsv1.Deployment]{
		ComponentOptions: handoffComponentOptions{},
		predicate:        func(WorkloadContext) (bool, error) { return false, nil },
	}

	if err := workload.Reconcile(ControlPlaneContext{Context: context.Background(), Client: c, HCP: hcp}); err != nil {
		t.Fatal(err)
	}
	live := &hyperv1.HostedControlPlane{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(hcp), live); err != nil {
		t.Fatal(err)
	}
	if live.Annotations[hyperv1.IgnitionServerHandoffAcknowledgedAnnotation] != "true" {
		t.Fatalf("preserved component did not acknowledge disable: %#v", live.Annotations)
	}
}
