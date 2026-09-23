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

	"github.com/google/uuid"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	storeCleanupFinalizer    = "hypershift.openshift.io/ignition-payload-store-cleanup"
	PayloadConsumerFinalizer = "hypershift.openshift.io/ignition-payload-consumer"
)

// Renderer produces the final ignition bytes from validated, deterministic config.
type Renderer interface {
	Render(context.Context, *ignitionv1alpha1.IgnitionPayload, string) ([]byte, error)
}

// Reconciler performs expensive rendering under manager leader election and stores
// finished bytes for independently scalable serving replicas.
type Reconciler struct {
	client.Client
	Renderer Renderer
	Store    PayloadStore
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ignitionv1alpha1.IgnitionPayload{}).
		Watches(&corev1.ConfigMap{}, r.configMapToPayloads()).
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
	if inputs.MachineConfigServerConfig.Name == name || (inputs.CloudConfig != nil && inputs.CloudConfig.Name == name) {
		return true
	}
	for _, ref := range append(append([]corev1.LocalObjectReference{}, payload.Spec.RolloutConfig...), payload.Spec.MgmtConfig...) {
		if ref.Name == name {
			return true
		}
	}
	return false
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

	config, identityHash, rolloutHash, err := r.resolveInput(ctx, payload)
	if err != nil {
		return ctrl.Result{}, r.setGenerated(ctx, payload, metav1.ConditionFalse, "InvalidInput", err.Error())
	}
	// A management-only input refresh must preserve the token advertised to
	// existing consumers. Replace the bytes behind current rather than creating
	// a third live token. This is the readiness-first migration contract.
	if current := payload.Status.CurrentRef(); current != nil && current.RolloutHash == rolloutHash {
		if current.ConfigHash != identityHash {
			if r.Renderer == nil {
				return ctrl.Result{}, r.setGenerated(ctx, payload, metav1.ConditionFalse, "RendererUnavailable", "payload renderer is not configured")
			}
			bytes, err := r.Renderer.Render(ctx, payload, config)
			if err != nil {
				return ctrl.Result{}, r.setGenerated(ctx, payload, metav1.ConditionFalse, "RenderFailed", err.Error())
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
	stored, err := r.findOrRender(ctx, payload, config, identityHash)
	if err != nil {
		if renderErr, ok := err.(*renderError); ok {
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

func (r *Reconciler) findOrRender(ctx context.Context, payload *ignitionv1alpha1.IgnitionPayload, config, identityHash string) (*StoredPayload, error) {
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
	bytes, err := r.Renderer.Render(ctx, payload, config)
	if err != nil {
		return nil, &renderError{reason: "RenderFailed", err: err}
	}
	stored = &StoredPayload{Token: uuid.NewString(), IdentityHash: identityHash, Payload: bytes}
	if err := r.Store.Put(ctx, payload, identityHash, stored.Token, stored.Payload); err != nil {
		return nil, fmt.Errorf("store generated payload: %w", err)
	}
	return stored, nil
}

func (r *Reconciler) resolveInput(ctx context.Context, payload *ignitionv1alpha1.IgnitionPayload) (string, string, string, error) {
	inputs := payload.Spec.RendererInputs
	if payload.Spec.PullSecretName == "" {
		return "", "", "", fmt.Errorf("pull secret name is required")
	}
	pullSecret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: payload.Namespace, Name: payload.Spec.PullSecretName}, pullSecret); err != nil {
		return "", "", "", fmt.Errorf("get pull secret %q: %w", payload.Spec.PullSecretName, err)
	}
	pullSecretBytes, ok := pullSecret.Data[corev1.DockerConfigJsonKey]
	if !ok {
		return "", "", "", fmt.Errorf("pull secret %q is missing %q", payload.Spec.PullSecretName, corev1.DockerConfigJsonKey)
	}
	pullSecretHash := supportutil.HashSimple(string(pullSecretBytes))
	mcs := &corev1.ConfigMap{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: payload.Namespace, Name: inputs.MachineConfigServerConfig.Name}, mcs); err != nil {
		return "", "", "", fmt.Errorf("get machine-config-server config: %w", err)
	}
	if mcs.Data["configuration-hash"] != inputs.MachineConfigServerConfigHash {
		return "", "", "", fmt.Errorf("machine-config-server configmap is out of date, waiting for update %s != %s", mcs.Data["configuration-hash"], inputs.MachineConfigServerConfigHash)
	}
	cloudHash := ""
	if inputs.CloudConfig != nil {
		cloud := &corev1.ConfigMap{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: payload.Namespace, Name: inputs.CloudConfig.Name}, cloud); err != nil {
			return "", "", "", fmt.Errorf("get cloud config: %w", err)
		}
		cloudHash = supportutil.HashConfigMapData(cloud.Data)
		if cloudHash != inputs.CloudConfigHash {
			return "", "", "", fmt.Errorf("cloud config %s/%s hash mismatch (expected %s, got %s), waiting for update", cloud.Namespace, cloud.Name, inputs.CloudConfigHash, cloudHash)
		}
	}
	rolloutItems, err := r.gatherRolloutItems(ctx, payload.Namespace, payload.Spec.RolloutConfig)
	if err != nil {
		return "", "", "", err
	}
	managementConfig, err := r.gatherManagement(ctx, payload.Namespace, payload.Spec.MgmtConfig)
	if err != nil {
		return "", "", "", err
	}
	rolloutConfig := joinConfig(rolloutItems)
	allConfig := joinConfig(append(rolloutItems, managementConfig...))
	// Pull-secret bytes affect image lookup and generated bytes, but rotating a
	// Secret in place must not roll every NodePool. Keep its content in the
	// identity hash only; changing the selected Secret name remains a deliberate
	// rollout input below.
	identity := supportutil.HashSimple(strings.Join([]string{allConfig, payload.Spec.ReleaseImage, payload.Spec.PullSecretName, pullSecretHash, payload.Spec.OSStream, inputs.ManagementGlobalConfig, inputs.MachineConfigServerConfigHash, cloudHash}, "\x00"))
	rollout := supportutil.HashSimple(strings.Join([]string{rolloutConfig, payload.Spec.ReleaseImage, payload.Spec.PullSecretName, payload.Spec.OSStream, payload.Spec.RolloutGlobalConfig}, "\x00"))
	return allConfig, identity, rollout, nil
}

func (r *Reconciler) gatherRollout(ctx context.Context, namespace string, refs []corev1.LocalObjectReference) (string, error) {
	items, err := r.gatherRolloutItems(ctx, namespace, refs)
	if err != nil {
		return "", err
	}
	return joinConfig(items), nil
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
	// Once the consumer reports the old generation drained, retirement is
	// independent of whether a new rollout is happening in this reconciliation.
	if previous := payload.Status.PreviousRef(); previous != nil && ptr.Deref(payload.Spec.RetiredGeneration, -1) >= ptr.Deref(previous.Generation, -1) {
		if err := r.Store.Delete(ctx, payload, previous.Token); err != nil {
			return err
		}
	}
	return statuspatching.PatchStatus(ctx, r.Client, payload, func() error {
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
			// A rollout cannot replace an unretired previous generation. Keeping
			// only two generation references makes the bounded handoff explicit:
			// consumers must acknowledge the previous generation before advancing.
			if previous := payload.Status.PreviousRef(); previous != nil && ptr.Deref(payload.Spec.RetiredGeneration, -1) < ptr.Deref(previous.Generation, -1) {
				return fmt.Errorf("wait for consumer retirement acknowledgement for generation %d before creating another rollout", ptr.Deref(previous.Generation, -1))
			}
			payload.Status.SetPrevious(current.DeepCopy())
		}
		payload.Status.SetCurrent(&ignitionv1alpha1.PayloadReference{ConfigHash: stored.IdentityHash, RolloutHash: rolloutHash, Token: stored.Token, Generation: ptr.To(nextGeneration)})
		meta.SetStatusCondition(&payload.Status.Conditions, metav1.Condition{Type: ignitionv1alpha1.IgnitionReachedCondition, Status: metav1.ConditionFalse, Reason: "WaitingForIgnition", Message: "Waiting for a node to fetch the current payload", ObservedGeneration: payload.Generation, LastTransitionTime: metav1.Now()})
		return nil
	})
}

func (r *Reconciler) setGenerated(ctx context.Context, payload *ignitionv1alpha1.IgnitionPayload, status metav1.ConditionStatus, reason, message string) error {
	return statuspatching.PatchStatus(ctx, r.Client, payload, func() error {
		meta.SetStatusCondition(&payload.Status.Conditions, metav1.Condition{Type: ignitionv1alpha1.PayloadGeneratedCondition, Status: status, Reason: reason, Message: message, ObservedGeneration: payload.Generation, LastTransitionTime: metav1.Now()})
		return nil
	})
}
