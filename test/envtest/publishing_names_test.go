//go:build envtest

package envtest

import (
	"strings"
	"testing"

	"github.com/onsi/gomega"
)

func TestPublishingAdmissionCases(t *testing.T) {
	gomega.RegisterTestingT(t)
	for _, kind := range []string{"HostedCluster", "HostedControlPlane"} {
		cases := publishingAdmissionCases(kind)
		t.Logf("%s: %d create cases and %d update cases", kind, len(cases.OnCreate), len(cases.OnUpdate))
		for _, testCase := range cases.OnCreate {
			if !strings.HasPrefix(testCase.Name, "When ") || !strings.Contains(testCase.Name, "it should") {
				t.Errorf("%s case lacks condition/expected behavior: %q", kind, testCase.Name)
			}
		}
		for _, testCase := range cases.OnUpdate {
			if !strings.HasPrefix(testCase.Name, "When ") || !strings.Contains(testCase.Name, "it should") {
				t.Errorf("%s update case lacks condition/expected behavior: %q", kind, testCase.Name)
			}
		}
	}
}
