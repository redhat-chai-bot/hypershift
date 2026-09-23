package v1alpha1

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestIgnitionPayloadRendererInputsSerialization(t *testing.T) {
	t.Run("When renderer inputs are serialized, it should preserve owned hashes and optional cloud references", func(t *testing.T) {
		payload := IgnitionPayload{Spec: IgnitionPayloadSpec{RendererInputs: IgnitionPayloadRendererInputs{
			MachineConfigServerConfigRef:  corev1.LocalObjectReference{Name: "machine-config-server"},
			MachineConfigServerConfigHash: "hosted-cluster-hash",
			CloudConfigRef:                &corev1.LocalObjectReference{Name: "azure-cloud-config"},
			CloudConfigHash:               "cloud-config-hash",
			ManagementGlobalConfig:        "full-management-config",
		}}}
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		var roundTrip IgnitionPayload
		if err := json.Unmarshal(data, &roundTrip); err != nil {
			t.Fatal(err)
		}
		inputs := roundTrip.Spec.RendererInputs
		if inputs.MachineConfigServerConfigRef.Name != "machine-config-server" || inputs.MachineConfigServerConfigHash != "hosted-cluster-hash" || inputs.CloudConfigRef == nil || inputs.CloudConfigRef.Name != "azure-cloud-config" || inputs.CloudConfigHash != "cloud-config-hash" || inputs.ManagementGlobalConfig != "full-management-config" {
			t.Fatalf("renderer input round trip lost data: %#v", inputs)
		}
	})
}
