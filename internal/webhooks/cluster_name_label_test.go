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
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
	. "github.com/onsi/gomega"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

func TestClusterNameLabelHandle(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clusterv1.AddToScheme(scheme)

	ownerWithLabel := newUnstructured("cluster.x-k8s.io/v1beta1", "Machine", "default", "owner-with-label", map[string]string{
		clusterv1.ClusterNameLabel: "owner-cluster",
	})
	otherOwnerWithLabel := newUnstructured("cluster.x-k8s.io/v1beta1", "MachineSet", "default", "other-owner-with-label", map[string]string{
		clusterv1.ClusterNameLabel: "other-cluster",
	})
	ownerWithoutLabel := newUnstructured("cluster.x-k8s.io/v1beta1", "Machine", "default", "owner-without-label", nil)

	tests := []struct {
		name          string
		object        *unstructured.Unstructured
		owners        []client.Object
		expectAllowed bool
		expectPatched bool
		expectLabel   string
		expectMessage string
	}{
		{
			name: "allows cluster with cluster name label",
			object: newUnstructured("cluster.x-k8s.io/v1beta1", "Cluster", "default", "cluster-with-label", map[string]string{
				clusterv1.ClusterNameLabel: "cluster-with-label",
			}),
			expectAllowed: true,
			expectLabel:   "cluster-with-label",
		},
		{
			name:          "denies cluster without cluster name label and owner",
			object:        newUnstructured("cluster.x-k8s.io/v1beta1", "Cluster", "default", "cluster-without-label", nil),
			expectAllowed: false,
			expectMessage: "cluster.x-k8s.io/cluster-name label must be set on the object or one of its owners",
		},
		{
			name: "copies label from controller owner",
			object: withOwnerReferences(newUnstructured("infrastructure.cluster.x-k8s.io/v1beta1", "InfraMachine", "default", "infra-machine", nil), []metav1.OwnerReference{
				ownerRef("cluster.x-k8s.io/v1beta1", "Machine", "owner-with-label", true),
			}),
			owners:        []client.Object{ownerWithLabel},
			expectAllowed: true,
			expectPatched: true,
			expectLabel:   "owner-cluster",
		},
		{
			name: "creates labels map when copying from owner",
			object: withOwnerReferences(newUnstructured("infrastructure.cluster.x-k8s.io/v1beta1", "InfraCluster", "default", "infra-cluster", nil), []metav1.OwnerReference{
				ownerRef("cluster.x-k8s.io/v1beta1", "Machine", "owner-with-label", true),
			}),
			owners:        []client.Object{ownerWithLabel},
			expectAllowed: true,
			expectPatched: true,
			expectLabel:   "owner-cluster",
		},
		{
			name: "does not overwrite existing label",
			object: withOwnerReferences(newUnstructured("infrastructure.cluster.x-k8s.io/v1beta1", "InfraMachine", "default", "infra-machine", map[string]string{
				clusterv1.ClusterNameLabel: "existing-cluster",
			}), []metav1.OwnerReference{
				ownerRef("cluster.x-k8s.io/v1beta1", "Machine", "owner-with-label", true),
			}),
			owners:        []client.Object{ownerWithLabel},
			expectAllowed: true,
			expectLabel:   "existing-cluster",
		},
		{
			name: "checks multiple owners",
			object: withOwnerReferences(newUnstructured("infrastructure.cluster.x-k8s.io/v1beta1", "InfraMachine", "default", "infra-machine", nil), []metav1.OwnerReference{
				ownerRef("cluster.x-k8s.io/v1beta1", "Machine", "owner-without-label", true),
				ownerRef("cluster.x-k8s.io/v1beta1", "MachineSet", "other-owner-with-label", false),
			}),
			owners:        []client.Object{ownerWithoutLabel, otherOwnerWithLabel},
			expectAllowed: true,
			expectPatched: true,
			expectLabel:   "other-cluster",
		},
		{
			name: "denies when owner lacks cluster name label",
			object: withOwnerReferences(newUnstructured("infrastructure.cluster.x-k8s.io/v1beta1", "InfraMachine", "default", "infra-machine", nil), []metav1.OwnerReference{
				ownerRef("cluster.x-k8s.io/v1beta1", "Machine", "owner-without-label", true),
			}),
			owners:        []client.Object{ownerWithoutLabel},
			expectAllowed: false,
			expectMessage: "cluster.x-k8s.io/cluster-name label must be set on the object or one of its owners",
		},
		{
			name: "denies when owner is not found",
			object: withOwnerReferences(newUnstructured("infrastructure.cluster.x-k8s.io/v1beta1", "InfraMachine", "default", "infra-machine", nil), []metav1.OwnerReference{
				ownerRef("cluster.x-k8s.io/v1beta1", "Machine", "missing-owner", true),
			}),
			expectAllowed: false,
			expectMessage: "cluster.x-k8s.io/cluster-name label must be set on the object or one of its owners",
		},
		{
			name: "not found owner does not block other owner",
			object: withOwnerReferences(newUnstructured("infrastructure.cluster.x-k8s.io/v1beta1", "InfraMachine", "default", "infra-machine", nil), []metav1.OwnerReference{
				ownerRef("cluster.x-k8s.io/v1beta1", "Machine", "missing-owner", true),
				ownerRef("cluster.x-k8s.io/v1beta1", "MachineSet", "other-owner-with-label", false),
			}),
			owners:        []client.Object{otherOwnerWithLabel},
			expectAllowed: true,
			expectPatched: true,
			expectLabel:   "other-cluster",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.owners...).Build()
			handler := &ClusterNameLabel{Client: fakeClient}

			raw := mustMarshal(t, tt.object)
			resp := handler.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
				UID:       uuid.NewUUID(),
				Kind:      metav1.GroupVersionKind(tt.object.GroupVersionKind()),
				Operation: admissionv1.Create,
				Object:    runtime.RawExtension{Raw: raw},
			}})

			g.Expect(resp.Allowed).To(Equal(tt.expectAllowed))
			if tt.expectMessage != "" {
				g.Expect(resp.Result.Message).To(Equal(tt.expectMessage))
			}

			if !tt.expectAllowed {
				return
			}

			mutatedRaw := raw
			if tt.expectPatched {
				g.Expect(resp.Patches).ToNot(BeEmpty())
				patchRaw, err := json.Marshal(resp.Patches)
				g.Expect(err).ToNot(HaveOccurred())
				patch, err := jsonpatch.DecodePatch(patchRaw)
				g.Expect(err).ToNot(HaveOccurred())
				mutatedRaw, err = patch.Apply(raw)
				g.Expect(err).ToNot(HaveOccurred())
			} else {
				g.Expect(resp.Patches).To(BeEmpty())
			}

			mutated := &unstructured.Unstructured{}
			g.Expect(json.Unmarshal(mutatedRaw, mutated)).To(Succeed())
			g.Expect(mutated.GetLabels()[clusterv1.ClusterNameLabel]).To(Equal(tt.expectLabel))
		})
	}
}

func TestClusterNameLabelHandleWithInvalidOwnerAPIVersion(t *testing.T) {
	g := NewWithT(t)
	handler := &ClusterNameLabel{Client: fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()}
	obj := withOwnerReferences(newUnstructured("infrastructure.cluster.x-k8s.io/v1beta1", "InfraMachine", "default", "infra-machine", nil), []metav1.OwnerReference{
		{APIVersion: "invalid/api/version", Kind: "Machine", Name: "owner", Controller: ptr.To(true)},
	})

	resp := handler.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID:       uuid.NewUUID(),
		Kind:      metav1.GroupVersionKind(obj.GroupVersionKind()),
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: mustMarshal(t, obj)},
	}})

	g.Expect(resp.Allowed).To(BeFalse())
	g.Expect(resp.Result.Code).To(Equal(int32(500)))
	g.Expect(resp.Result.Message).To(ContainSubstring("failed to parse owner apiVersion"))
}

func newUnstructured(apiVersion, kind, namespace, name string, labels map[string]string) *unstructured.Unstructured {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		panic(err)
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gv.WithKind(kind))
	obj.SetNamespace(namespace)
	obj.SetName(name)
	obj.SetLabels(labels)
	return obj
}

func withOwnerReferences(obj *unstructured.Unstructured, ownerRefs []metav1.OwnerReference) *unstructured.Unstructured {
	obj.SetOwnerReferences(ownerRefs)
	return obj
}

func ownerRef(apiVersion, kind, name string, controller bool) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: apiVersion,
		Kind:       kind,
		Name:       name,
		Controller: &controller,
	}
}

func mustMarshal(t *testing.T, obj *unstructured.Unstructured) []byte {
	t.Helper()
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
