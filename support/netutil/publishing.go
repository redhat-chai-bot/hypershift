package netutil

import (
	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/publishing"

	"k8s.io/klog/v2"
)

func PublishingForHCP(hcp *hyperv1.HostedControlPlane) *hyperv1.ServicePublishing {
	if hcp.Spec.Publishing.Type == "" {
		return nil
	}
	services, err := publishing.ToServices(&hcp.Spec.Publishing, hcp.Spec.Platform)
	if err == nil && len(hcp.Spec.Services) > 0 {
		err = publishing.EquivalentServices(hcp.Spec.Services, services)
	}
	if err != nil {
		klog.Warningf("Ignoring publishing policy for HostedControlPlane %s/%s and retaining authoritative legacy services: %v", hcp.Namespace, hcp.Name, err)
		return nil
	}
	return &hcp.Spec.Publishing
}

func HCPRouterExposure(hcp *hyperv1.HostedControlPlane) hyperv1.IngressExposureType {
	policy := PublishingForHCP(hcp)
	if policy == nil || policy.Type != hyperv1.ServicePublishingSingleIngressPoint {
		return ""
	}
	return policy.SingleIngressPoint.Exposure
}

func HCPRouterNodePort(hcp *hyperv1.HostedControlPlane) *hyperv1.IngressNodePortConfig {
	if HCPRouterExposure(hcp) != hyperv1.IngressExposureNodePort {
		return nil
	}
	return &hcp.Spec.Publishing.SingleIngressPoint.NodePort
}
