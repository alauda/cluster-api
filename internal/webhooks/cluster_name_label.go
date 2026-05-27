/*
Copyright 2026 The Kubernetes Authors.

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

package webhooks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

const clusterNameLabelWebhookPath = "/mutate-cluster-x-k8s-io-v1beta1-cluster-name-label"

// ClusterNameLabel enforces the cluster name label for explicitly configured resources.
type ClusterNameLabel struct {
	Client client.Reader
}

func (h *ClusterNameLabel) SetupWebhookWithManager(mgr ctrl.Manager) error {
	if h.Client == nil {
		h.Client = mgr.GetAPIReader()
	}

	mgr.GetWebhookServer().Register(clusterNameLabelWebhookPath, &webhook.Admission{Handler: h})
	return nil
}

// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;machines;machinesets;machinedeployments;machinehealthchecks,verbs=get
// +kubebuilder:rbac:groups=exp.cluster.x-k8s.io,resources=machinepools,verbs=get
// +kubebuilder:webhook:verbs=create;update,path=/mutate-cluster-x-k8s-io-v1beta1-cluster-name-label,mutating=true,failurePolicy=fail,matchPolicy=Equivalent,groups=cluster.x-k8s.io,resources=clusters,versions=v1beta1,name=cluster-name-label.cluster.cluster.x-k8s.io,sideEffects=None,admissionReviewVersions=v1;v1beta1

var _ admission.Handler = &ClusterNameLabel{}

func (webhook *ClusterNameLabel) Handle(ctx context.Context, req admission.Request) admission.Response {
	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal(req.Object.Raw, obj); err != nil {
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("failed to decode object: %w", err))
	}

	if clusterName := obj.GetLabels()[clusterv1.ClusterNameLabel]; clusterName != "" {
		return admission.Allowed("")
	}

	clusterName, err := webhook.clusterNameFromOwner(ctx, obj.GetNamespace(), obj.GetOwnerReferences())
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	if clusterName == "" {
		return admission.Denied(fmt.Sprintf("%s label must be set on the object or one of its owners", clusterv1.ClusterNameLabel))
	}

	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[clusterv1.ClusterNameLabel] = clusterName
	obj.SetLabels(labels)

	mutatedRaw, err := json.Marshal(obj)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("failed to encode mutated object: %w", err))
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, mutatedRaw)
}

func (webhook *ClusterNameLabel) clusterNameFromOwner(ctx context.Context, namespace string, ownerRefs []metav1.OwnerReference) (string, error) {
	for _, ownerRef := range sortedOwnerReferences(ownerRefs) {
		gv, err := schema.ParseGroupVersion(ownerRef.APIVersion)
		if err != nil {
			return "", fmt.Errorf("failed to parse owner apiVersion %q: %w", ownerRef.APIVersion, err)
		}

		owner := &unstructured.Unstructured{}
		owner.SetGroupVersionKind(gv.WithKind(ownerRef.Kind))
		if err := webhook.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ownerRef.Name}, owner); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return "", fmt.Errorf("failed to get owner %s %s/%s: %w", ownerRef.Kind, namespace, ownerRef.Name, err)
		}

		if clusterName := owner.GetLabels()[clusterv1.ClusterNameLabel]; clusterName != "" {
			return clusterName, nil
		}
	}
	return "", nil
}

func sortedOwnerReferences(ownerRefs []metav1.OwnerReference) []metav1.OwnerReference {
	if len(ownerRefs) <= 1 {
		return ownerRefs
	}

	controllerOwners := make([]metav1.OwnerReference, 0, 1)
	otherOwners := make([]metav1.OwnerReference, 0, len(ownerRefs))
	for _, ownerRef := range ownerRefs {
		if ownerRef.Controller != nil && *ownerRef.Controller {
			controllerOwners = append(controllerOwners, ownerRef)
			continue
		}
		otherOwners = append(otherOwners, ownerRef)
	}

	return append(controllerOwners, otherOwners...)
}
