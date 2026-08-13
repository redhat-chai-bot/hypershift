package core

import (
	"fmt"
	"testing"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	clientgotesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestIsResourceRegistered(t *testing.T) {
	dummyGroup := "dummy.group.io"
	dummyVersion := "v2beta3"
	dummyKind := "machinedeployment"

	fakeDiscoveryClient := &fakediscovery.FakeDiscovery{
		Fake: &clientgotesting.Fake{
			Resources: []*metav1.APIResourceList{
				{
					GroupVersion: fmt.Sprintf("%s/%s", dummyGroup, dummyVersion),
					APIResources: []metav1.APIResource{
						{
							Kind: dummyKind,
						},
					},
				},
			},
		},
	}

	tests := []struct {
		name        string
		gvk         schema.GroupVersionKind
		expected    bool
		expectError bool
	}{
		{
			name:        "group version not found",
			gvk:         schema.GroupVersionKind{Group: "non.existing.group.io", Version: dummyVersion, Kind: dummyKind},
			expected:    false,
			expectError: false,
		},
		{
			name:        "group version found but kind not found",
			gvk:         schema.GroupVersionKind{Group: dummyGroup, Version: dummyVersion, Kind: "non-existing-kind"},
			expected:    false,
			expectError: false,
		},
		{
			name:        "group version kind found",
			gvk:         schema.GroupVersionKind{Group: dummyGroup, Version: dummyVersion, Kind: dummyKind},
			expected:    true,
			expectError: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := isResourceRegistered(fakeDiscoveryClient, test.gvk)
			if result != test.expected {
				t.Errorf("expected %v, got %v", test.expected, result)
			}
			if (err != nil) != test.expectError {
				t.Errorf("expected error: %v, got error: %v", test.expectError, err)
			}
		})
	}
}

func TestPlatformSpecificResources(t *testing.T) {
	// allPlatformResourceTypes collects every resource type string returned by
	// the default (fallback) case so we can verify it against known platforms.
	allPlatformResourceTypes := resourceTypeSet(platformSpecificResources(""))

	tests := []struct {
		name                string
		platformType        hyperv1.PlatformType
		wantContains        []string
		wantNotContains     []string
		wantMatchesFallback bool
	}{
		{
			name:         "When platform is AWS, it should return only AWS resources",
			platformType: hyperv1.AWSPlatform,
			wantContains: []string{
				"awsmachine.infrastructure.cluster.x-k8s.io",
				"awsmachinetemplate.infrastructure.cluster.x-k8s.io",
				"awscluster.infrastructure.cluster.x-k8s.io",
				"awsendpointservice.hypershift.openshift.io",
			},
			wantNotContains: []string{
				"azurecluster.infrastructure.cluster.x-k8s.io",
				"openstackcluster.infrastructure.cluster.x-k8s.io",
				"agentmachine.capi-provider.agent-install.openshift.io",
				"kubevirtmachine.infrastructure.cluster.x-k8s.io",
			},
		},
		{
			name:         "When platform is Azure, it should return only Azure resources",
			platformType: hyperv1.AzurePlatform,
			wantContains: []string{
				"azurecluster.infrastructure.cluster.x-k8s.io",
				"azuremachine.infrastructure.cluster.x-k8s.io",
				"azuremachinetemplate.infrastructure.cluster.x-k8s.io",
			},
			wantNotContains: []string{
				"awsmachine.infrastructure.cluster.x-k8s.io",
				"openstackcluster.infrastructure.cluster.x-k8s.io",
				"agentmachine.capi-provider.agent-install.openshift.io",
				"kubevirtmachine.infrastructure.cluster.x-k8s.io",
			},
		},
		{
			name:         "When platform is OpenStack, it should return only OpenStack resources",
			platformType: hyperv1.OpenStackPlatform,
			wantContains: []string{
				"openstackserver.infrastructure.cluster.x-k8s.io",
				"openstackcluster.infrastructure.cluster.x-k8s.io",
				"openstackmachine.infrastructure.cluster.x-k8s.io",
				"openstackmachinetemplate.infrastructure.cluster.x-k8s.io",
				"image.openstack.k-orc.cloud",
			},
			wantNotContains: []string{
				"awsmachine.infrastructure.cluster.x-k8s.io",
				"azurecluster.infrastructure.cluster.x-k8s.io",
				"agentmachine.capi-provider.agent-install.openshift.io",
				"kubevirtmachine.infrastructure.cluster.x-k8s.io",
			},
		},
		{
			name:         "When platform is Agent, it should return only Agent resources",
			platformType: hyperv1.AgentPlatform,
			wantContains: []string{
				"agentmachine.capi-provider.agent-install.openshift.io",
				"agentmachinetemplate.capi-provider.agent-install.openshift.io",
				"agentcluster.capi-provider.agent-install.openshift.io",
			},
			wantNotContains: []string{
				"awsmachine.infrastructure.cluster.x-k8s.io",
				"azurecluster.infrastructure.cluster.x-k8s.io",
				"openstackcluster.infrastructure.cluster.x-k8s.io",
				"kubevirtmachine.infrastructure.cluster.x-k8s.io",
			},
		},
		{
			name:         "When platform is KubeVirt, it should return only KubeVirt resources",
			platformType: hyperv1.KubevirtPlatform,
			wantContains: []string{
				"kubevirtmachine.infrastructure.cluster.x-k8s.io",
				"kubevirtmachinetemplate.infrastructure.cluster.x-k8s.io",
				"kubevirtcluster.infrastructure.cluster.x-k8s.io",
			},
			wantNotContains: []string{
				"awsmachine.infrastructure.cluster.x-k8s.io",
				"azurecluster.infrastructure.cluster.x-k8s.io",
				"openstackcluster.infrastructure.cluster.x-k8s.io",
				"agentmachine.capi-provider.agent-install.openshift.io",
			},
		},
		{
			name:         "When platform is None, it should return no platform-specific resources",
			platformType: hyperv1.NonePlatform,
			wantContains: []string{},
			wantNotContains: []string{
				"awsmachine.infrastructure.cluster.x-k8s.io",
				"azurecluster.infrastructure.cluster.x-k8s.io",
				"openstackcluster.infrastructure.cluster.x-k8s.io",
				"agentmachine.capi-provider.agent-install.openshift.io",
				"kubevirtmachine.infrastructure.cluster.x-k8s.io",
			},
		},
		{
			name:                "When platform type is empty, it should return all platform resources as fallback",
			platformType:        "",
			wantMatchesFallback: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resources := platformSpecificResources(test.platformType)
			types := resourceTypeSet(resources)

			if test.wantMatchesFallback {
				if len(types) != len(allPlatformResourceTypes) {
					t.Errorf("expected fallback to return %d resource types, got %d", len(allPlatformResourceTypes), len(types))
				}
				for rt := range allPlatformResourceTypes {
					if !types[rt] {
						t.Errorf("expected fallback to include %q", rt)
					}
				}
				return
			}

			for _, want := range test.wantContains {
				if !types[want] {
					t.Errorf("expected resources to contain %q, got types: %v", want, typeNames(types))
				}
			}
			for _, unwanted := range test.wantNotContains {
				if types[unwanted] {
					t.Errorf("expected resources NOT to contain %q, got types: %v", unwanted, typeNames(types))
				}
			}
		})
	}
}

// resourceTypeSet converts a slice of client.Object to a set of resource type strings.
func resourceTypeSet(objs []client.Object) map[string]bool {
	result := make(map[string]bool, len(objs))
	for _, obj := range objs {
		result[objectType(obj)] = true
	}
	return result
}

// typeNames extracts the keys from a resource type set for error messages.
func typeNames(m map[string]bool) []string {
	result := make([]string, 0, len(m))
	for k := range m {
		result = append(result, k)
	}
	return result
}
