package hostedcluster

import (
	"testing"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/publishing"

	configv1 "github.com/openshift/api/config/v1"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
)

func TestPublishingVirtLauncherPolicyEquivalence(t *testing.T) {
	network := &configv1.Network{Spec: configv1.NetworkSpec{ClusterNetwork: []configv1.ClusterNetworkEntry{{CIDR: "10.128.0.0/14"}, {CIDR: "fd01::/48"}}, ServiceNetwork: []string{"172.30.0.0/16", "fd02::/112"}}}
	for _, external := range []bool{false, true} {
		for _, address := range []string{"192.0.2.10", "2001:db8::10"} {
			for _, preset := range []string{"NodePort", "NodePortRouter", "LoadBalancerRouter"} {
				t.Run("When "+preset+" publishes "+address+", it should preserve virt-launcher egress across adoption", func(t *testing.T) {
					hc := hostedClusterPublishingFixture(t)
					hc.Spec.InfraID = "test-infra"
					hc.Spec.Platform = hyperv1.PlatformSpec{Type: hyperv1.KubevirtPlatform, Kubevirt: &hyperv1.KubevirtPlatformSpec{}}
					switch preset {
					case "NodePort":
						hc.Spec.Publishing = hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingNodePort, NodePort: hyperv1.NodePortPublishing{Address: address}}
					case "NodePortRouter":
						hc.Spec.Publishing.SingleIngressPoint.NodePort.Address = address
					default:
						hc.Spec.Publishing.SingleIngressPoint.Exposure = hyperv1.IngressExposureLoadBalancer
						hc.Spec.Publishing.SingleIngressPoint.NodePort = hyperv1.IngressNodePortConfig{}
						hc.Spec.Publishing.SingleIngressPoint.LoadBalancer = &hyperv1.IngressLoadBalancerConfig{}
					}
					services, err := publishing.ServicesForHC(hc)
					require.NoError(t, err)
					legacy := hc.DeepCopy()
					legacy.Spec.Services = services
					legacy.Spec.Publishing = hyperv1.ServicePublishing{}
					newPolicy, oldPolicy := &networkingv1.NetworkPolicy{}, &networkingv1.NetworkPolicy{}
					reconcile := reconcileVirtLauncherNetworkPolicy
					if external {
						reconcile = reconcileVirtLauncherNetworkPolicyExternalInfra
					}
					require.NoError(t, reconcile(logr.Discard(), oldPolicy, legacy, network))
					require.NoError(t, reconcile(logr.Discard(), newPolicy, hc, network))
					require.Equal(t, oldPolicy.Spec, newPolicy.Spec)
					require.NotEmpty(t, newPolicy.Spec.Egress)
					blocks := map[string]int{}
					routerPeers := 0
					for _, rule := range newPolicy.Spec.Egress {
						for _, peer := range rule.To {
							if peer.IPBlock != nil {
								blocks[peer.IPBlock.CIDR]++
								if peer.IPBlock.CIDR == "0.0.0.0/0" {
									require.ElementsMatch(t, []string{"10.128.0.0/14", "172.30.0.0/16"}, peer.IPBlock.Except)
								}
								if peer.IPBlock.CIDR == "::/0" {
									require.ElementsMatch(t, []string{"fd01::/48", "fd02::/112"}, peer.IPBlock.Except)
								}
							}
							if peer.PodSelector != nil && peer.PodSelector.MatchLabels["app"] == "private-router" {
								require.Equal(t, &metav1.LabelSelector{MatchLabels: map[string]string{"app": "private-router"}}, peer.PodSelector)
								require.Nil(t, peer.NamespaceSelector, "the centralized router peer must remain in the policy's own control-plane namespace")
								routerPeers++
							}
						}
					}
					prefix := address + "/32"
					if address == "2001:db8::10" {
						prefix = address + "/128"
					}
					if preset == "NodePort" {
						require.Equal(t, 1, blocks[prefix], "all four translated NodePort services must share one endpoint exception")
						require.Len(t, blocks, 3)
					} else {
						require.Zero(t, blocks[prefix], "router Route services must not manufacture direct NodePort exceptions")
						require.Len(t, blocks, 2)
					}
					require.Equal(t, 1, blocks["0.0.0.0/0"])
					require.Equal(t, 1, blocks["::/0"])
					if !external && preset != "NodePort" {
						require.Equal(t, 1, routerPeers)
					} else {
						require.Zero(t, routerPeers)
					}
					hc.Spec.Publishing.Type = "Invalid"
					require.Error(t, reconcile(logr.Discard(), newPolicy, hc, network))
				})
			}
		}
	}
}
