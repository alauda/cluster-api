/*
Copyright 2022 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"strings"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	runtimev1 "sigs.k8s.io/cluster-api/exp/runtime/api/v1alpha1"
	runtimeclient "sigs.k8s.io/cluster-api/internal/runtime/client"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
)

const (
	// tlsCAKey is used as a data key in Secret resources to store a CA certificate.
	tlsCAKey = "ca.crt"
)

// +kubebuilder:rbac:groups=runtime.cluster.x-k8s.io,resources=extensionconfigs;extensionconfigs/status,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

// Reconciler reconciles an ExtensionConfig object.
type Reconciler struct {
	Client        client.Client
	APIReader     client.Reader
	RuntimeClient runtimeclient.Client
	// WatchFilterValue is the label value used to filter events prior to reconciliation.
	WatchFilterValue string
}

func (r *Reconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, options controller.Options) error {
	err := ctrl.NewControllerManagedBy(mgr).
		For(&runtimev1.ExtensionConfig{}).
		Watches(
			&source.Kind{Type: &corev1.Secret{}},
			handler.EnqueueRequestsFromMapFunc(r.secretToExtensionConfig),
			builder.OnlyMetadata,
		).
		WithOptions(options).
		WithEventFilter(predicates.ResourceNotPausedAndHasFilterLabel(ctrl.LoggerFrom(ctx), r.WatchFilterValue)).
		Complete(r)
	if err != nil {
		return errors.Wrap(err, "failed setting up with a controller manager")
	}

	// warmupRunnable will attempt to sync the RuntimeSDK registry with existing ExtensionConfig objects to ensure extensions
	// are discovered before controllers begin reconciling.
	err = mgr.Add(&warmupRunnable{
		Client:        r.Client,
		APIReader:     r.APIReader,
		RuntimeClient: r.RuntimeClient,
	})
	if err != nil {
		return errors.Wrap(err, "failed adding warmupRunnable to controller manager")
	}
	return nil
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var errs []error
	log := ctrl.LoggerFrom(ctx)
	// Requeue events when the registry is not ready.
	if !r.RuntimeClient.IsReady() {
		return ctrl.Result{Requeue: true}, nil
	}

	extensionConfig := &runtimev1.ExtensionConfig{}
	err := r.Client.Get(ctx, req.NamespacedName, extensionConfig)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// ExtensionConfig not found. Remove from registry.
			// First need to add Namespace/Name to empty ExtensionConfig object.
			extensionConfig.Name = req.Name
			extensionConfig.Namespace = req.Namespace
			return r.reconcileDelete(ctx, extensionConfig)
		}
		// Error reading the object - requeue the request.
		return ctrl.Result{}, err
	}

	// Return early if the ExtensionConfig is paused.
	if annotations.HasPaused(extensionConfig) {
		log.Info("Reconciliation is paused for this object")
		return ctrl.Result{}, nil
	}

	// Handle deletion reconciliation loop.
	if !extensionConfig.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, extensionConfig)
	}

	// Copy to avoid modifying the original extensionConfig.
	original := extensionConfig.DeepCopy()

	// Inject CABundle from secret if annotation is set. Otherwise https calls may fail.
	if err := reconcileCABundle(ctx, r.Client, extensionConfig); err != nil {
		return ctrl.Result{}, err
	}

	// discoverExtensionConfig will return a discovered ExtensionConfig with the appropriate conditions.
	discoveredExtensionConfig, err := discoverExtensionConfig(ctx, r.RuntimeClient, extensionConfig)
	if err != nil {
		errs = append(errs, err)
	}

	// Always patch the ExtensionConfig as it may contain updates in conditions or clientConfig.caBundle.
	if err = patchExtensionConfig(ctx, r.Client, original, discoveredExtensionConfig); err != nil {
		errs = append(errs, err)
	}

	if len(errs) != 0 {
		return ctrl.Result{}, kerrors.NewAggregate(errs)
	}

	// Register the ExtensionConfig if it was found and patched without error.
	if err = r.RuntimeClient.Register(discoveredExtensionConfig); err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "failed to register ExtensionConfig %s/%s", extensionConfig.Namespace, extensionConfig.Name)
	}
	return ctrl.Result{}, nil
}

func patchExtensionConfig(ctx context.Context, client client.Client, original, modified *runtimev1.ExtensionConfig, options ...patch.Option) error {
	patchHelper, err := patch.NewHelper(original, client)
	if err != nil {
		return errors.Wrapf(err, "failed to create PatchHelper for ExtensionConfig %s/%s", modified.Namespace, modified.Name)
	}

	options = append(options, patch.WithOwnedConditions{Conditions: []clusterv1.ConditionType{
		runtimev1.RuntimeExtensionDiscoveredCondition,
	}})
	err = patchHelper.Patch(ctx, modified, options...)
	if err != nil {
		return errors.Wrapf(err, "failed to patch ExtensionConfig %s/%s", modified.Namespace, modified.Name)
	}
	return nil
}

// reconcileDelete will remove the ExtensionConfig from the registry on deletion of the object. Note this is a best
// effort deletion that may not catch all cases.
func (r *Reconciler) reconcileDelete(_ context.Context, extensionConfig *runtimev1.ExtensionConfig) (ctrl.Result, error) {
	if err := r.RuntimeClient.Unregister(extensionConfig); err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "failed to unregister ExtensionConfig %s/%s", extensionConfig.Namespace, extensionConfig.Name)
	}
	return ctrl.Result{}, nil
}

// secretToExtensionConfig maps a secret to ExtensionConfigs to reconcile them on updates of the secrets.
func (r *Reconciler) secretToExtensionConfig(secret client.Object) []reconcile.Request {
	result := []ctrl.Request{}

	extensionConfigs := runtimev1.ExtensionConfigList{}
	if err := r.Client.List(context.Background(), &extensionConfigs); err != nil {
		return nil
	}

	for _, ext := range extensionConfigs.Items {
		if secretNameRaw, ok := ext.GetAnnotations()[runtimev1.InjectCAFromSecretAnnotation]; ok {
			secretName := splitNamespacedName(secretNameRaw)
			// append all extensions to the result which refer the object as secret
			if secretName.Namespace == secret.GetNamespace() && secretName.Name == secret.GetName() {
				name := client.ObjectKey{Namespace: ext.GetNamespace(), Name: ext.GetName()}
				result = append(result, ctrl.Request{NamespacedName: name})
			}
		}
	}

	return result
}

// discoverExtensionConfig attempts to discover the Handlers for an ExtensionConfig.
// If discovery succeeds it returns the ExtensionConfig with Handlers updated in Status and an updated Condition.
// If discovery fails it returns the ExtensionConfig with no update to Handlers and a Failed Condition.
func discoverExtensionConfig(ctx context.Context, runtimeClient runtimeclient.Client, extensionConfig *runtimev1.ExtensionConfig) (*runtimev1.ExtensionConfig, error) {
	discoveredExtension, err := runtimeClient.Discover(ctx, extensionConfig.DeepCopy())
	if err != nil {
		modifiedExtensionConfig := extensionConfig.DeepCopy()
		conditions.MarkFalse(modifiedExtensionConfig, runtimev1.RuntimeExtensionDiscoveredCondition, runtimev1.DiscoveryFailedReason, clusterv1.ConditionSeverityError, "error in discovery: %v", err)
		return modifiedExtensionConfig, errors.Wrapf(err, "failed to discover ExtensionConfig %s/%s", extensionConfig.Namespace, extensionConfig.Name)
	}

	conditions.MarkTrue(discoveredExtension, runtimev1.RuntimeExtensionDiscoveredCondition)
	return discoveredExtension, nil
}

// reconcileCABundle reconciles the CA bundle for the ExtensionConfig.
// cert-manager code: pkg/controller/cainjector/sources.go certificateDataSource.
func reconcileCABundle(ctx context.Context, client client.Client, config *runtimev1.ExtensionConfig) error {
	secretNameRaw, ok := config.Annotations[runtimev1.InjectCAFromSecretAnnotation]
	if !ok {
		return nil
	}
	secretName := splitNamespacedName(secretNameRaw)

	if secretName.Namespace == "" || secretName.Name == "" {
		return errors.Errorf("secret name %q must be in the form namespace/name", secretNameRaw)
	}

	var secret corev1.Secret
	// Note: this is an expensive API call because secrets are explicitly not cached.
	if err := client.Get(ctx, secretName, &secret); err != nil {
		return errors.Wrapf(err, "failed to get secret %s", secretNameRaw)
	}

	caData, hasCAData := secret.Data[tlsCAKey]
	if !hasCAData {
		return errors.Errorf("secret %s does not contain a %s", secretNameRaw, tlsCAKey)
	}

	config.Spec.ClientConfig.CABundle = caData
	return nil
}

// splitNamespacedName turns the string form of a namespaced name
// (<namespace>/<name>) back into a types.NamespacedName.
func splitNamespacedName(nameStr string) types.NamespacedName {
	splitPoint := strings.IndexRune(nameStr, types.Separator)
	if splitPoint == -1 {
		return types.NamespacedName{Name: nameStr}
	}
	return types.NamespacedName{Namespace: nameStr[:splitPoint], Name: nameStr[splitPoint+1:]}
}
