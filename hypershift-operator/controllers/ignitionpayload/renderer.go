package ignitionpayload

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	ignitionv1alpha1 "github.com/openshift/hypershift/api/hypershift/v1alpha1"
	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	ignitioncontrollers "github.com/openshift/hypershift/ignition-server/controllers"
	hyperapi "github.com/openshift/hypershift/support/api"
	"github.com/openshift/hypershift/support/releaseinfo"
	supportutil "github.com/openshift/hypershift/support/util"

	configv1 "github.com/openshift/api/config/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// MCORenderer invokes the existing release-matched MCO pipeline. It is used only
// by the leader-elected PayloadController; serving replicas never construct one.
type MCORenderer struct {
	Client           client.Client
	ReleaseProvider  releaseinfo.ProviderWithOpenShiftImageRegistryOverrides
	MetadataProvider *supportutil.RegistryClientImageMetadataProvider
	WorkDir          string
}

func (r *MCORenderer) Render(ctx context.Context, request *ignitionv1alpha1.IgnitionPayload, input RenderInput) ([]byte, error) {
	if r.ReleaseProvider == nil || r.MetadataProvider == nil {
		return nil, fmt.Errorf("release and image metadata providers are required")
	}
	workDir := r.WorkDir
	if workDir == "" {
		workDir = os.TempDir()
	}
	featureGateManifest, cleanup, err := r.featureGateManifest(ctx, request.Namespace, workDir)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	imageFileCache, err := ignitioncontrollers.NewImageFileCache(workDir)
	if err != nil {
		return nil, err
	}
	provider := &ignitioncontrollers.LocalIgnitionProvider{
		Client:                r.Client,
		ReleaseProvider:       r.ReleaseProvider,
		CloudProvider:         cloudProvider(request),
		Namespace:             request.Namespace,
		PullSecretName:        request.Spec.PullSecretName,
		WorkDir:               workDir,
		FeatureGateManifest:   featureGateManifest,
		ImageFileCache:        imageFileCache,
		ImageMetadataProvider: r.MetadataProvider,
	}
	return provider.GetPayload(ctx, input.ReleaseImage, input.Config, input.PullSecretHash, input.AdditionalTrustBundleHash, request.Spec.RendererInputs.MachineConfigServerConfigHash, request.Spec.OSStream, request.Spec.RendererInputs.CloudConfigHash)
}

func (r *MCORenderer) ResolveRelease(ctx context.Context, image string, pullSecret []byte) (ResolvedRelease, error) {
	if r.ReleaseProvider == nil || r.MetadataProvider == nil {
		return ResolvedRelease{}, fmt.Errorf("release and image metadata providers are required")
	}
	digest, ref, err := r.MetadataProvider.GetDigest(ctx, image, pullSecret)
	if err != nil {
		return ResolvedRelease{}, err
	}
	resolvedImage := ref.String()
	release, err := r.ReleaseProvider.Lookup(ctx, resolvedImage, pullSecret)
	if err != nil {
		return ResolvedRelease{}, err
	}
	return ResolvedRelease{Image: resolvedImage, Identity: digest.String(), Version: release.Version()}, nil
}

func cloudProvider(request *ignitionv1alpha1.IgnitionPayload) hyperv1.PlatformType {
	if request.Spec.RendererInputs.CloudConfig == nil {
		return ""
	}
	switch request.Spec.RendererInputs.CloudConfig.Name {
	case "azure-cloud-config":
		return hyperv1.AzurePlatform
	case "openstack-cloud-config":
		return hyperv1.OpenStackPlatform
	default:
		return ""
	}
}

func (r *MCORenderer) featureGateManifest(ctx context.Context, namespace, workDir string) (string, func(), error) {
	hcps := &hyperv1.HostedControlPlaneList{}
	if err := r.Client.List(ctx, hcps, client.InNamespace(namespace)); err != nil {
		return "", nil, fmt.Errorf("list HostedControlPlanes for feature gate rendering: %w", err)
	}
	if len(hcps.Items) != 1 {
		return "", nil, fmt.Errorf("expected one HostedControlPlane in namespace %q, found %d", namespace, len(hcps.Items))
	}
	featureGate := &configv1.FeatureGate{TypeMeta: metav1.TypeMeta{APIVersion: configv1.GroupVersion.String(), Kind: "FeatureGate"}, ObjectMeta: metav1.ObjectMeta{Name: "cluster"}}
	if hcps.Items[0].Spec.Configuration != nil && hcps.Items[0].Spec.Configuration.FeatureGate != nil {
		featureGate.Spec = *hcps.Items[0].Spec.Configuration.FeatureGate
	}
	bytes, err := hyperapi.CompatibleYAMLEncode(featureGate, hyperapi.YamlSerializer)
	if err != nil {
		return "", nil, fmt.Errorf("serialize feature gate: %w", err)
	}
	dir, err := os.MkdirTemp(workDir, "ignition-feature-gate-")
	if err != nil {
		return "", nil, err
	}
	path := filepath.Join(dir, "99_feature-gate.yaml")
	if err := os.WriteFile(path, bytes, 0600); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	return path, func() { _ = os.RemoveAll(dir) }, nil
}
