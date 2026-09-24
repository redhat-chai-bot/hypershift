package ignitionserverproxy

import (
	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	ignitionserverv2 "github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/v2/ignitionserver"
	component "github.com/openshift/hypershift/support/controlplane-component"
)

const (
	ComponentName = "ignition-server-proxy"
)

var _ component.ComponentOptions = &ignitionServerProxy{}

type ignitionServerProxy struct {
	defaultIngressDomain string
}

// DeploymentOptions exposes the component's topology and availability
// classification to reconcilers adopting its Deployment.
func DeploymentOptions() component.ComponentOptions {
	return &ignitionServerProxy{}
}

// IsRequestServing implements controlplanecomponent.ComponentOptions.
func (r *ignitionServerProxy) IsRequestServing() bool {
	return true
}

// MultiZoneSpread implements controlplanecomponent.ComponentOptions.
func (r *ignitionServerProxy) MultiZoneSpread() bool {
	return true
}

// NeedsManagementKASAccess implements controlplanecomponent.ComponentOptions.
func (r *ignitionServerProxy) NeedsManagementKASAccess() bool {
	return false
}

// PreserveOnDisable keeps the adopted proxy resources during the HO handoff.
func (r *ignitionServerProxy) PreserveOnDisable(cpContext component.WorkloadContext) bool {
	return cpContext.HCP.Annotations[hyperv1.DisableIgnitionServerAnnotation] == hyperv1.IgnitionServerHandoffAnnotationValue
}

func (r *ignitionServerProxy) DisableAcknowledgementAnnotation() string {
	return hyperv1.IgnitionServerProxyHandoffAcknowledgedAnnotation
}

func NewComponent(defaultIngressDomain string) component.ControlPlaneComponent {
	ignition := &ignitionServerProxy{
		defaultIngressDomain: defaultIngressDomain,
	}

	return component.NewDeploymentComponent(ComponentName, ignition).
		WithAdaptFunction(adaptDeployment).
		WithPredicate(predicate).
		WithManifestAdapter(
			"haproxy-config.yaml",
			component.WithAdaptFunction(adaptHAProxyConfig),
		).
		WithManifestAdapter(
			"service.yaml",
			component.WithAdaptFunction(adaptService),
		).
		WithDependencies(ignitionserverv2.ComponentName).
		Build()
}

func predicate(cpContext component.WorkloadContext) (bool, error) {
	_, disableIgnition := cpContext.HCP.Annotations[hyperv1.DisableIgnitionServerAnnotation]
	return !disableIgnition && cpContext.HCP.Spec.Platform.Type != hyperv1.IBMCloudPlatform, nil
}
