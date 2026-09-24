package ignitionpayload

import (
	"bufio"
	"context"
	coreerrors "errors"
	"fmt"
	"io"
	"sort"
	"strings"

	ignitionv1alpha1 "github.com/openshift/hypershift/api/hypershift/v1alpha1"
	"github.com/openshift/hypershift/support/ignitionconfig"
	"github.com/openshift/hypershift/support/statuspatching"
	supportutil "github.com/openshift/hypershift/support/util"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/ptr"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/google/uuid"
)

const (
	storeCleanupFinalizer    = "hypershift.openshift.io/ignition-payload-store-cleanup"
	PayloadConsumerFinalizer = "hypershift.openshift.io/ignition-payload-consumer"
)

// Renderer produces the final ignition bytes from validated, deterministic config.
type Renderer interface {
	Render(context.Context, *ignitionv1alpha1.IgnitionPayload, RenderInput) ([]byte, error)
}

// ReleaseResolver resolves a mutable release pullspec to an immutable image
// identity and the release version used by established NodePool rollout hashes.
type ReleaseResolver interface {
	ResolveRelease(context.Context, string, []byte) (ResolvedRelease, error)
}

type ResolvedRelease struct {
	Image    string
	Identity string
	Version  string
}

// RenderInput carries validated content and hashes into the existing MCO
// renderer. Keeping these values together prevents positional hash swaps.
type RenderInput struct {
	Config                    string
	PullSecretHash            string
	AdditionalTrustBundleHash string
	ReleaseImage              string
}

// Reconciler performs expensive rendering under manager leader election and stores
// finished bytes for independently scalable serving replicas.
type Reconciler struct {
	client.Client
	Renderer        Renderer
	ReleaseResolver ReleaseResolver
	Store           PayloadStore
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ignitionv1alpha1.IgnitionPayload{}).
		Watches(&corev1.ConfigMap{}, r.configMapToPayloads()).
		Watches(&corev1.Secret{}, r.secretToPayloads()).
		WithOptions(controllerOptions()).
		Complete(r)
}

func controllerOptions() controller.Options { return controller.Options{MaxConcurrentReconciles: 1} }

func (r *Reconciler) configMapToPayloads() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		payloads := &ignitionv1alpha1.IgnitionPayloadList{}
		if err := r.List(ctx, payloads, client.InNamespace(obj.GetNamespace())); err != nil {
			return nil
		}
		requests := make([]reconcile.Request, 0)
		for i := range payloads.Items {
			payload := &payloads.Items[i]
			if referencesConfigMap(payload, obj.GetName()) {
				requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(payload)})
			}
		}
		return requests
	})
}

func referencesConfigMap(payload *ignitionv1alpha1.IgnitionPayload, name string) bool {
	inputs := payload.Spec.RendererInputs
	if inputs.MachineConfigServerConfig.Name == name ||
		(inputs.CloudConfig != nil && inputs.CloudConfig.Name == name) ||
		(payload.Spec.AdditionalTrustBundle != nil && payload.Spec.AdditionalTrustBundle.Name == name) {
		return true
	}
	for _, ref := range append(append([]corev1.LocalObjectReference{}, payload.Spec.RolloutConfig...), payload.Spec.MgmtConfig...) {
		if ref.Name == name {
			return true
		}
	}
	return false
}

func (r *Reconciler) secretToPayloads() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		payloads := &ignitionv1alpha1.IgnitionPayloadList{}
		if err := r.List(ctx, payloads, client.InNamespace(obj.GetNamespace())); err != nil {
			return nil
		}
		requests := make([]reconcile.Request, 0)
		for i := range payloads.Items {
			if payloads.Items[i].Spec.PullSecretName == obj.GetName() {
				requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&payloads.Items[i])})
			}
		}
		return requests
	})
}

func (r *Reconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	payload := &ignitionv1alpha1.IgnitionPayload{}
	if err := r.Get(ctx, request.NamespacedName, payload); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !payload.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(payload, storeCleanupFinalizer) {
			return ctrl.Result{}, nil
		}
		entries, err := r.Store.ListByOwner(ctx, payload)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("list payloads for cleanup: %w", err)
		}
		for _, entry := range entries {
			if err := r.Store.Delete(ctx, payload, entry.Token); err != nil {
				return ctrl.Result{}, fmt.Errorf("delete stored payload %q: %w", entry.Token, err)
			}
		}
		controllerutil.RemoveFinalizer(payload, storeCleanupFinalizer)
		return ctrl.Result{}, r.Update(ctx, payload)
	}
	if !controllerutil.ContainsFinalizer(payload, storeCleanupFinalizer) {
		controllerutil.AddFinalizer(payload, storeCleanupFinalizer)
		if err := r.Update(ctx, payload); err != nil {
			return ctrl.Result{}, err
		}
	}

	input, identityHash, rolloutHash, err := r.resolveInput(ctx, payload)
	if err != nil {
		return ctrl.Result{}, r.setGenerated(ctx, payload, metav1.ConditionFalse, "InvalidInput", err.Error())
	}
	if err := r.sweepOrphans(ctx, payload, identityHash); err != nil {
		return ctrl.Result{}, err
	}
	// A management-only input refresh must preserve the token advertised to
	// existing consumers. Replace the bytes behind current rather than creating
	// a third live token. This is the readiness-first migration contract.
	if current := payload.Status.CurrentRef(); current != nil && current.RolloutHash == rolloutHash {
		if current.ConfigHash != identityHash {
			if r.Renderer == nil {
				return ctrl.Result{}, r.setGenerated(ctx, payload, metav1.ConditionFalse, "RendererUnavailable", "payload renderer is not configured")
			}
			bytes, err := r.Renderer.Render(ctx, payload, input)
			if err != nil {
				return ctrl.Result{}, r.setGenerated(ctx, payload, metav1.ConditionFalse, "RenderFailed", err.Error())
			}
			if err := r.validateGeneration(ctx, payload); err != nil {
				return ctrl.Result{}, err
			}
			if err := r.Store.Put(ctx, payload, identityHash, current.Token, bytes); err != nil {
				return ctrl.Result{}, fmt.Errorf("refresh current payload: %w", err)
			}
		}
		stored := &StoredPayload{Token: current.Token, IdentityHash: identityHash}
		if err := r.updateCurrent(ctx, payload, stored, rolloutHash); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.setGenerated(ctx, payload, metav1.ConditionTrue, "PayloadGenerated", "Payload generated successfully")
	}
	stored, err := r.findOrRender(ctx, payload, input, identityHash)
	if err != nil {
		var renderErr *renderError
		if coreerrors.As(err, &renderErr) {
			return ctrl.Result{}, r.setGenerated(ctx, payload, metav1.ConditionFalse, renderErr.reason, renderErr.Error())
		}
		return ctrl.Result{}, err
	}
	if err := r.updateCurrent(ctx, payload, stored, rolloutHash); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.setGenerated(ctx, payload, metav1.ConditionTrue, "PayloadGenerated", "Payload generated successfully")
}

type renderError struct {
	reason string
	err    error
}

func (e *renderError) Error() string { return e.err.Error() }

func (r *Reconciler) findOrRender(ctx context.Context, payload *ignitionv1alpha1.IgnitionPayload, input RenderInput, identityHash string) (*StoredPayload, error) {
	stored, err := r.Store.FindByIdentity(ctx, payload, identityHash)
	if err != nil {
		return nil, fmt.Errorf("look up payload identity: %w", err)
	}
	if stored != nil {
		return stored, nil
	}
	if r.Renderer == nil {
		return nil, &renderError{reason: "RendererUnavailable", err: fmt.Errorf("payload renderer is not configured")}
	}
	bytes, err := r.Renderer.Render(ctx, payload, input)
	if err != nil {
		return nil, &renderError{reason: "RenderFailed", err: err}
	}
	stored = &StoredPayload{Token: uuid.NewString(), IdentityHash: identityHash, Payload: bytes}
	if err := r.validateGeneration(ctx, payload); err != nil {
		return nil, err
	}
	if err := r.Store.Put(ctx, payload, identityHash, stored.Token, stored.Payload); err != nil {
		return nil, fmt.Errorf("store generated payload: %w", err)
	}
	return stored, nil
}

func (r *Reconciler) resolveInput(ctx context.Context, payload *ignitionv1alpha1.IgnitionPayload) (RenderInput, string, string, error) {
	inputs := payload.Spec.RendererInputs
	if payload.Spec.PullSecretName == "" {
		return RenderInput{}, "", "", fmt.Errorf("pull secret name is required")
	}
	pullSecret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: payload.Namespace, Name: payload.Spec.PullSecretName}, pullSecret); err != nil {
		return RenderInput{}, "", "", fmt.Errorf("get pull secret %q: %w", payload.Spec.PullSecretName, err)
	}
	pullSecretBytes, ok := pullSecret.Data[corev1.DockerConfigJsonKey]
	if !ok {
		return RenderInput{}, "", "", fmt.Errorf("pull secret %q is missing %q", payload.Spec.PullSecretName, corev1.DockerConfigJsonKey)
	}
	pullSecretHash := supportutil.HashSimple(string(pullSecretBytes))
	resolved := ResolvedRelease{Image: payload.Spec.ReleaseImage, Identity: payload.Spec.ReleaseImage, Version: payload.Spec.ReleaseImage}
	if r.ReleaseResolver != nil {
		var err error
		resolved, err = r.ReleaseResolver.ResolveRelease(ctx, payload.Spec.ReleaseImage, pullSecretBytes)
		if err != nil {
			return RenderInput{}, "", "", fmt.Errorf("resolve release image %q: %w", payload.Spec.ReleaseImage, err)
		}
		if resolved.Image == "" || resolved.Identity == "" || resolved.Version == "" {
			return RenderInput{}, "", "", fmt.Errorf("resolved release image, identity, and version are required")
		}
	}
	mcs := &corev1.ConfigMap{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: payload.Namespace, Name: inputs.MachineConfigServerConfig.Name}, mcs); err != nil {
		return RenderInput{}, "", "", fmt.Errorf("get machine-config-server config: %w", err)
	}
	if mcs.Data["configuration-hash"] != inputs.MachineConfigServerConfigHash {
		return RenderInput{}, "", "", fmt.Errorf("machine-config-server configmap is out of date, waiting for update %s != %s", mcs.Data["configuration-hash"], inputs.MachineConfigServerConfigHash)
	}
	mcsContentHash := supportutil.HashConfigMapData(mcs.Data)
	additionalTrustBundleHash := ""
	additionalTrustBundleName := ""
	if payload.Spec.AdditionalTrustBundle != nil {
		additionalTrustBundleName = payload.Spec.AdditionalTrustBundle.Name
		bundle := &corev1.ConfigMap{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: payload.Namespace, Name: additionalTrustBundleName}, bundle); err != nil {
			return RenderInput{}, "", "", fmt.Errorf("get additional trust bundle: %w", err)
		}
		additionalTrustBundleHash = supportutil.HashSimple(bundle.Data["ca-bundle.crt"])
	}
	cloudHash := ""
	if inputs.CloudConfig != nil {
		cloud := &corev1.ConfigMap{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: payload.Namespace, Name: inputs.CloudConfig.Name}, cloud); err != nil {
			return RenderInput{}, "", "", fmt.Errorf("get cloud config: %w", err)
		}
		cloudHash = supportutil.HashConfigMapData(cloud.Data)
		if cloudHash != inputs.CloudConfigHash {
			return RenderInput{}, "", "", fmt.Errorf("cloud config %s/%s hash mismatch (expected %s, got %s), waiting for update", cloud.Namespace, cloud.Name, inputs.CloudConfigHash, cloudHash)
		}
	}
	rolloutItems, err := r.gatherRolloutItems(ctx, payload.Namespace, payload.Spec.RolloutConfig)
	if err != nil {
		return RenderInput{}, "", "", err
	}
	managementConfig, err := r.gatherManagement(ctx, payload.Namespace, payload.Spec.MgmtConfig)
	if err != nil {
		return RenderInput{}, "", "", err
	}
	rolloutConfig := joinConfig(rolloutItems)
	allItems := append(append([]string{}, rolloutItems...), managementConfig...)
	allConfig := joinConfig(allItems)
	// Pull-secret bytes affect image lookup and generated bytes, but rotating a
	// Secret in place must not roll every NodePool. Keep its content in the
	// identity hash only; changing the selected Secret name remains a deliberate
	// rollout input below.
	cloudConfigName := ""
	if inputs.CloudConfig != nil {
		cloudConfigName = inputs.CloudConfig.Name
	}
	identity := supportutil.HashSimple(strings.Join([]string{allConfig, inputs.ManagementGlobalConfig, resolved.Identity, payload.Spec.PullSecretName, pullSecretHash, additionalTrustBundleName, additionalTrustBundleHash, payload.Spec.OSStream, payload.Spec.RolloutGlobalConfig, inputs.MachineConfigServerConfig.Name, inputs.MachineConfigServerConfigHash, mcsContentHash, cloudConfigName, cloudHash}, "\x00"))
	rollout := supportutil.HashSimple(strings.Join([]string{rolloutConfig, resolved.Version, payload.Spec.PullSecretName, additionalTrustBundleName, payload.Spec.OSStream, payload.Spec.RolloutGlobalConfig}, "\x00"))
	return RenderInput{Config: allConfig, PullSecretHash: pullSecretHash, AdditionalTrustBundleHash: additionalTrustBundleHash, ReleaseImage: resolved.Image}, identity, rollout, nil
}

func (r *Reconciler) validateGeneration(ctx context.Context, payload *ignitionv1alpha1.IgnitionPayload) error {
	live := &ignitionv1alpha1.IgnitionPayload{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(payload), live); err != nil {
		return fmt.Errorf("validate ignition payload generation: %w", err)
	}
	if live.Generation != payload.Generation {
		return fmt.Errorf("ignition payload spec changed from generation %d to %d while rendering", payload.Generation, live.Generation)
	}
	return nil
}

func (r *Reconciler) sweepOrphans(ctx context.Context, payload *ignitionv1alpha1.IgnitionPayload, inFlightIdentity string) error {
	entries, err := r.Store.ListByOwner(ctx, payload)
	if err != nil {
		return fmt.Errorf("list payloads for orphan sweep: %w", err)
	}
	keep := map[string]struct{}{inFlightIdentity: {}}
	if current := payload.Status.CurrentRef(); current != nil {
		keep[current.ConfigHash] = struct{}{}
	}
	if previous := payload.Status.PreviousRef(); previous != nil {
		keep[previous.ConfigHash] = struct{}{}
	}
	for _, entry := range entries {
		if _, ok := keep[entry.IdentityHash]; ok {
			continue
		}
		if err := r.Store.Delete(ctx, payload, entry.Token); err != nil {
			return fmt.Errorf("delete orphaned payload %q: %w", entry.Token, err)
		}
	}
	return nil
}

func (r *Reconciler) gatherRolloutItems(ctx context.Context, namespace string, refs []corev1.LocalObjectReference) ([]string, error) {
	items := make([]string, 0)
	for _, ref := range refs {
		config := &corev1.ConfigMap{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, config); err != nil {
			return nil, fmt.Errorf("get configmap %q: %w", ref.Name, err)
		}
		value, ok := config.Data["config"]
		if !ok {
			return nil, fmt.Errorf("configmap %q is missing config", ref.Name)
		}
		reader := yaml.NewYAMLReader(bufio.NewReader(strings.NewReader(value)))
		for {
			document, err := reader.Read()
			if err != nil && !coreerrors.Is(err, io.EOF) {
				return nil, fmt.Errorf("configmap %q contains invalid yaml: %w", ref.Name, err)
			}
			if len(document) > 0 && strings.TrimSpace(string(document)) != "" {
				normalized, err := ignitionconfig.Normalize(document)
				if err != nil {
					return nil, fmt.Errorf("configmap %q yaml document failed validation: %w", ref.Name, err)
				}
				items = append(items, string(normalized))
			}
			if coreerrors.Is(err, io.EOF) {
				break
			}
		}
	}
	return items, nil
}

func (r *Reconciler) gatherManagement(ctx context.Context, namespace string, refs []corev1.LocalObjectReference) ([]string, error) {
	items := make([]string, 0, len(refs))
	for _, ref := range refs {
		config := &corev1.ConfigMap{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, config); err != nil {
			return nil, fmt.Errorf("get management configmap %q: %w", ref.Name, err)
		}
		value, ok := config.Data["config"]
		if !ok {
			return nil, fmt.Errorf("management configmap %q is missing config", ref.Name)
		}
		items = append(items, value)
	}
	return items, nil
}

func joinConfig(items []string) string {
	sort.Strings(items)
	return strings.Join(items, "\n---\n")
}

func (r *Reconciler) updateCurrent(ctx context.Context, payload *ignitionv1alpha1.IgnitionPayload, stored *StoredPayload, rolloutHash string) error {
	expectedGeneration := payload.Generation
	// Stop advertising a retired token before deleting its backing Secret. If
	// the status patch fails, keeping an extra Secret is safe; advertising a
	// token whose Secret was already deleted is not.
	retiredToken := ""
	evictedToken := ""
	if previous := payload.Status.PreviousRef(); previous != nil && ptr.Deref(payload.Spec.RetiredGeneration, -1) >= ptr.Deref(previous.Generation, -1) {
		retiredToken = previous.Token
	}
	if err := statuspatching.PatchStatus(ctx, r.Client, payload, func() error {
		if payload.Generation != expectedGeneration {
			return fmt.Errorf("ignition payload spec changed from generation %d to %d while rendering", expectedGeneration, payload.Generation)
		}
		current := payload.Status.CurrentRef()
		if previous := payload.Status.PreviousRef(); previous != nil && ptr.Deref(payload.Spec.RetiredGeneration, -1) >= ptr.Deref(previous.Generation, -1) {
			payload.Status.SetPrevious(nil)
		}
		if current != nil && current.RolloutHash == rolloutHash {
			current.ConfigHash = stored.IdentityHash
			payload.Status.SetCurrent(current)
			return nil
		}
		nextGeneration := int64(0)
		if current != nil {
			nextGeneration = ptr.Deref(current.Generation, -1) + 1
			if previous := payload.Status.PreviousRef(); previous != nil {
				evictedToken = previous.Token
			}
			payload.Status.SetPrevious(current.DeepCopy())
		}
		payload.Status.SetCurrent(&ignitionv1alpha1.PayloadReference{ConfigHash: stored.IdentityHash, RolloutHash: rolloutHash, Token: stored.Token, Generation: ptr.To(nextGeneration)})
		meta.SetStatusCondition(&payload.Status.Conditions, metav1.Condition{Type: ignitionv1alpha1.IgnitionReachedCondition, Status: metav1.ConditionFalse, Reason: "WaitingForIgnition", Message: "Waiting for a node to fetch the current payload", ObservedGeneration: payload.Generation, LastTransitionTime: metav1.Now()})
		return nil
	}); err != nil {
		return err
	}
	if retiredToken != "" && retiredToken != stored.Token {
		if err := r.Store.Delete(ctx, payload, retiredToken); err != nil {
			return fmt.Errorf("delete retired payload %q: %w", retiredToken, err)
		}
	}
	if evictedToken != "" && evictedToken != retiredToken && evictedToken != stored.Token {
		if err := r.Store.Delete(ctx, payload, evictedToken); err != nil {
			return fmt.Errorf("delete evicted payload %q: %w", evictedToken, err)
		}
	}
	return nil
}

func (r *Reconciler) setGenerated(ctx context.Context, payload *ignitionv1alpha1.IgnitionPayload, status metav1.ConditionStatus, reason, message string) error {
	return statuspatching.PatchStatus(ctx, r.Client, payload, func() error {
		meta.SetStatusCondition(&payload.Status.Conditions, metav1.Condition{Type: ignitionv1alpha1.PayloadGeneratedCondition, Status: status, Reason: reason, Message: message, ObservedGeneration: payload.Generation, LastTransitionTime: metav1.Now()})
		return nil
	})
}
