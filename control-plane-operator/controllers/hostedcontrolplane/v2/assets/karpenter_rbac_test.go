package assets

import (
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
)

func TestKarpenterIgnitionPayloadRBAC(t *testing.T) {
	role := &rbacv1.Role{}
	if _, _, err := LoadManifestInto("karpenter-operator", "role.yaml", role); err != nil {
		t.Fatal(err)
	}
	for _, rule := range role.Rules {
		for _, resource := range rule.Resources {
			if resource == "ignitionpayloads/status" {
				t.Fatalf("Karpenter consumer must not own IgnitionPayload status: %#v", rule)
			}
		}
	}
}
