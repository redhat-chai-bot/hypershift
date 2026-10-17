package hostedcluster

import (
	"context"
	"fmt"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	hoFeaturegate "github.com/openshift/hypershift/hypershift-operator/featuregate"
	"github.com/openshift/hypershift/support/imageregistry"
	"github.com/openshift/hypershift/support/publishing"
	hyperutil "github.com/openshift/hypershift/support/util"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const controlPlaneOperatorSupportsRouterNodePortLabel = "io.openshift.hypershift.control-plane-operator-supports-router-nodeport"

func reconcileHostedControlPlanePublishing(hcp *hyperv1.HostedControlPlane, hc *hyperv1.HostedCluster, enabled bool, labels map[string]string) error {
	if err := validateRouterNodePortCapability(hc, labels); err != nil {
		return err
	}
	services, err := publishing.ServicesForHC(hc)
	if err != nil {
		return fmt.Errorf("invalid publishing policy: %w", err)
	}
	policy := hyperv1.ServicePublishing{}
	if hc.Spec.Publishing.Type != "" {
		policy = *hc.Spec.Publishing.DeepCopy()
	} else if enabled {
		translated, translationErr := publishing.FromServices(services, hc.Spec.Platform)
		if translationErr == nil {
			policy = *translated
		}
	}
	hcp.Spec.Services = services
	hcp.Spec.Publishing = policy
	return nil
}

func publishingConfigurationCondition(hc *hyperv1.HostedCluster, hcp *hyperv1.HostedControlPlane, enabled bool) metav1.Condition {
	condition := metav1.Condition{
		Type: string(hyperv1.ValidHostedClusterConfiguration), ObservedGeneration: hc.Generation,
		Status: metav1.ConditionTrue, Reason: hyperv1.AsExpectedReason, Message: "Configuration passes validation",
	}
	var diagnostic error
	if hc.Spec.Publishing.Type == "" && enabled {
		_, diagnostic = publishing.FromServices(hc.Spec.Services, hc.Spec.Platform)
	}
	if diagnostic == nil && hcp != nil {
		diagnostic = publishing.HCPTranslationError(hcp)
	}
	if diagnostic != nil {
		condition.Reason = "ServicePublishingTranslationWarning"
		condition.Message = fmt.Sprintf("Continuing with authoritative legacy services: %v", diagnostic)
	}
	return condition
}

func validateRouterNodePortCapability(hc *hyperv1.HostedCluster, labels map[string]string) error {
	if hc.Spec.Publishing.Type == hyperv1.ServicePublishingSingleIngressPoint && hc.Spec.Publishing.SingleIngressPoint.Exposure == hyperv1.IngressExposureNodePort && labels[controlPlaneOperatorSupportsRouterNodePortLabel] != "true" {
		return fmt.Errorf("SingleIngressPoint NodePort requires a control plane operator image with %s=true", controlPlaneOperatorSupportsRouterNodePortLabel)
	}
	if hc.Spec.Platform.Type == hyperv1.AgentPlatform && hc.Spec.Publishing.Type == hyperv1.ServicePublishingSingleIngressPoint && hc.Spec.Publishing.SingleIngressPoint.Exposure == hyperv1.IngressExposurePlatformProvided && labels[controlPlaneOperatorSupportsRouterNodePortLabel] != "true" {
		return fmt.Errorf("SingleIngressPoint PlatformProvided on the Agent platform requires a publishing-aware control plane operator image with %s=true to preserve management ingress ownership", controlPlaneOperatorSupportsRouterNodePortLabel)
	}
	return nil
}

func (r *HostedClusterReconciler) validateRouterNodePortImageCapability(ctx context.Context, hc *hyperv1.HostedCluster) error {
	if hc.Spec.Publishing.Type != hyperv1.ServicePublishingSingleIngressPoint || (hc.Spec.Publishing.SingleIngressPoint.Exposure != hyperv1.IngressExposureNodePort && (hc.Spec.Platform.Type != hyperv1.AgentPlatform || hc.Spec.Publishing.SingleIngressPoint.Exposure != hyperv1.IngressExposurePlatformProvided)) {
		return nil
	}
	pullSecret, err := hyperutil.GetPullSecretBytes(ctx, r.Client, hc)
	if err != nil {
		return fmt.Errorf("cannot verify router NodePort capability: %w", err)
	}
	image, err := imageregistry.GetControlPlaneOperatorImage(ctx, hc, r.RegistryProvider.GetReleaseProvider(), r.HypershiftOperatorImage, pullSecret)
	if err != nil {
		return fmt.Errorf("cannot resolve router NodePort control plane operator: %w", err)
	}
	labels, err := imageregistry.GetControlPlaneOperatorImageLabels(ctx, hc, image, pullSecret, r.RegistryProvider.GetMetadataProvider())
	if err != nil {
		return fmt.Errorf("cannot verify router NodePort image labels: %w", err)
	}
	return validateRouterNodePortCapability(hc, labels)
}

func (r *HostedClusterReconciler) ignitionRouterNodePortEndpoint(ctx context.Context, hc *hyperv1.HostedCluster, namespace, hostname string) (string, error) {
	return publishing.IgnitionRouterNodePortEndpoint(ctx, r.Client, hc, namespace, hostname)
}

func servicePublishingAPIEnabled() bool {
	return hoFeaturegate.Gate().Enabled(hoFeaturegate.ServicePublishingAPI)
}
