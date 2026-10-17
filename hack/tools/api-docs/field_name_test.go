package main

import (
	"testing"

	"k8s.io/gengo/types"
)

func TestFieldName(t *testing.T) {
	for _, testCase := range []struct {
		name string
		tag  string
		want string
	}{
		{"When a tag has no options, it should preserve the field name", `json:"publishing"`, "publishing"},
		{"When publishing uses omitzero, it should omit the tag option", `json:"publishing,omitzero"`, "publishing"},
		{"When a union arm uses omitempty, it should omit the tag option", `json:"loadBalancer,omitempty"`, "loadBalancer"},
		{"When a union arm uses omitzero, it should omit the tag option", `json:"singleIngressPoint,omitzero"`, "singleIngressPoint"},
		{"When options are combined, it should use only the literal field name", `json:"nodePort,omitempty,omitzero"`, "nodePort"},
		{"When options are reordered, it should use only the literal field name", `json:"nodePort,omitzero,omitempty"`, "nodePort"},
		{"When a member is inline, it should retain the member name", `json:",inline"`, "ObjectMeta"},
		{"When no JSON tag is present, it should retain the member name", "", "ObjectMeta"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := fieldName(types.Member{Name: "ObjectMeta", Tags: testCase.tag}); got != testCase.want {
				t.Fatalf("got %q, want %q", got, testCase.want)
			}
		})
	}
}
