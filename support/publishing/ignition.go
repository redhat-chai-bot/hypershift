package publishing

import (
	"context"
	"fmt"
	"net"
	"strconv"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	cpomanifests "github.com/openshift/hypershift/pkg/manifests/cpo"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

func IgnitionRouterNodePortEndpoint(ctx context.Context, clusterClient client.Client, hc *hyperv1.HostedCluster, namespace, hostname string) (string, error) {
	if hc.Spec.Publishing.Type != hyperv1.ServicePublishingSingleIngressPoint || hc.Spec.Publishing.SingleIngressPoint.Exposure != hyperv1.IngressExposureNodePort {
		return hostname, nil
	}
	if host, _, err := net.SplitHostPort(hostname); err == nil {
		hostname = host
	}
	svc := cpomanifests.RouterPublicService(namespace)
	if err := clusterClient.Get(ctx, client.ObjectKeyFromObject(svc), svc); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("failed to get router NodePort service: %w", err)
	}
	if svc.Spec.Type != corev1.ServiceTypeNodePort {
		return "", nil
	}
	for _, port := range svc.Spec.Ports {
		if port.Name == "https" && port.NodePort > 0 && hostname != "" {
			return net.JoinHostPort(hostname, strconv.Itoa(int(port.NodePort))), nil
		}
	}
	return "", nil
}
