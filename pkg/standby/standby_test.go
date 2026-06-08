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

package standby

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type errorReader struct{}

func (errorReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("test error")
}

func (errorReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("test error")
}

type fakeDetector struct {
	standby bool
	err     error
}

func (d fakeDetector) IsStandby(context.Context) (bool, error) {
	return d.standby, d.err
}

type countingReconciler struct {
	calls int
	res   reconcile.Result
	err   error
}

func (r *countingReconciler) Reconcile(context.Context, reconcile.Request) (reconcile.Result, error) {
	r.calls++
	return r.res, r.err
}

func TestConfigMapDetectorIsStandby(t *testing.T) {
	g := NewWithT(t)
	oldNamespace := ConfigMapNamespace
	ConfigMapNamespace = "cpaas-system"
	defer func() { ConfigMapNamespace = oldNamespace }()

	scheme := runtime.NewScheme()
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())

	standbyClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ConfigMapNamespace,
			Name:      ConfigMapName,
		},
	}).Build()
	isStandby, err := NewConfigMapDetector(standbyClient).IsStandby(context.Background())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(isStandby).To(BeTrue())

	activeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	isStandby, err = NewConfigMapDetector(activeClient).IsStandby(context.Background())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(isStandby).To(BeFalse())
}

func TestConfigMapDetectorReturnsGetError(t *testing.T) {
	g := NewWithT(t)

	isStandby, err := NewConfigMapDetector(errorReader{}).IsStandby(context.Background())
	g.Expect(err).To(HaveOccurred())
	g.Expect(isStandby).To(BeFalse())
}

func TestWrapReconciler(t *testing.T) {
	t.Run("standby skips inner reconciler", func(t *testing.T) {
		g := NewWithT(t)
		inner := &countingReconciler{}

		res, err := WrapReconciler(fakeDetector{standby: true}, "test", inner).Reconcile(context.Background(), reconcile.Request{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res).To(Equal(reconcile.Result{RequeueAfter: RequeueAfter}))
		g.Expect(inner.calls).To(Equal(0))
	})

	t.Run("active calls inner reconciler", func(t *testing.T) {
		g := NewWithT(t)
		inner := &countingReconciler{res: reconcile.Result{Requeue: true}}

		res, err := WrapReconciler(fakeDetector{}, "test", inner).Reconcile(context.Background(), reconcile.Request{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res).To(Equal(reconcile.Result{Requeue: true}))
		g.Expect(inner.calls).To(Equal(1))
	})

	t.Run("detector error skips inner reconciler", func(t *testing.T) {
		g := NewWithT(t)
		inner := &countingReconciler{}
		detectorErr := apierrors.NewNotFound(schema.GroupResource{Group: "test", Resource: "detectors"}, "test")

		_, err := WrapReconciler(fakeDetector{err: detectorErr}, "test", inner).Reconcile(context.Background(), reconcile.Request{})
		g.Expect(err).To(MatchError(detectorErr))
		g.Expect(inner.calls).To(Equal(0))
	})
}

func TestWrapClusterReconciler(t *testing.T) {
	t.Run("standby allows global cluster", func(t *testing.T) {
		g := NewWithT(t)
		inner := &countingReconciler{res: reconcile.Result{Requeue: true}}

		res, err := WrapClusterReconciler(fakeDetector{standby: true}, "cluster", inner).Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: GlobalClusterName}})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res).To(Equal(reconcile.Result{Requeue: true}))
		g.Expect(inner.calls).To(Equal(1))
	})

	t.Run("standby skips business cluster", func(t *testing.T) {
		g := NewWithT(t)
		inner := &countingReconciler{}

		res, err := WrapClusterReconciler(fakeDetector{standby: true}, "cluster", inner).Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "business"}})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res).To(Equal(reconcile.Result{RequeueAfter: RequeueAfter}))
		g.Expect(inner.calls).To(Equal(0))
	})

	t.Run("active calls inner reconciler", func(t *testing.T) {
		g := NewWithT(t)
		inner := &countingReconciler{}

		_, err := WrapClusterReconciler(fakeDetector{}, "cluster", inner).Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "business"}})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(inner.calls).To(Equal(1))
	})
}

func TestWrapClusterNamedReconciler(t *testing.T) {
	scheme := runtime.NewScheme()
	NewWithT(t).Expect(corev1.AddToScheme(scheme)).To(Succeed())

	newObject := func() client.Object { return &corev1.ConfigMap{} }
	getClusterName := func(obj client.Object) string { return obj.(*corev1.ConfigMap).Data["clusterName"] }

	t.Run("standby allows object from global cluster", func(t *testing.T) {
		g := NewWithT(t)
		reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespaceDefault, Name: "cm"},
			Data:       map[string]string{"clusterName": GlobalClusterName},
		}).Build()
		inner := &countingReconciler{}

		_, err := WrapClusterNamedReconciler(fakeDetector{standby: true}, reader, "test", newObject, getClusterName, inner).Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: metav1.NamespaceDefault, Name: "cm"}})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(inner.calls).To(Equal(1))
	})

	t.Run("standby skips object from business cluster", func(t *testing.T) {
		g := NewWithT(t)
		reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespaceDefault, Name: "cm"},
			Data:       map[string]string{"clusterName": "business"},
		}).Build()
		inner := &countingReconciler{}

		res, err := WrapClusterNamedReconciler(fakeDetector{standby: true}, reader, "test", newObject, getClusterName, inner).Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: metav1.NamespaceDefault, Name: "cm"}})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res).To(Equal(reconcile.Result{RequeueAfter: RequeueAfter}))
		g.Expect(inner.calls).To(Equal(0))
	})

	t.Run("standby lets not found object reach inner reconciler", func(t *testing.T) {
		g := NewWithT(t)
		reader := fake.NewClientBuilder().WithScheme(scheme).Build()
		inner := &countingReconciler{}

		_, err := WrapClusterNamedReconciler(fakeDetector{standby: true}, reader, "test", newObject, getClusterName, inner).Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: metav1.NamespaceDefault, Name: "missing"}})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(inner.calls).To(Equal(1))
	})

	t.Run("detector error skips inner reconciler", func(t *testing.T) {
		g := NewWithT(t)
		reader := fake.NewClientBuilder().WithScheme(scheme).Build()
		inner := &countingReconciler{}
		detectorErr := errors.New("detector error")

		_, err := WrapClusterNamedReconciler(fakeDetector{err: detectorErr}, reader, "test", newObject, getClusterName, inner).Reconcile(context.Background(), reconcile.Request{})
		g.Expect(err).To(MatchError(detectorErr))
		g.Expect(inner.calls).To(Equal(0))
	})
}
