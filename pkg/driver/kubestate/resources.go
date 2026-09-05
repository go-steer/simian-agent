// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kubestate

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
)

// managedResource is one Kubernetes type this engine synthesizes, reduced to
// the two operations the driver performs on objects it may not have created in
// this process: find them again, and remove them.
type managedResource struct {
	plural string
	list   func(ctx context.Context, c kubernetes.Interface, ns, selector string) ([]metav1.ObjectMeta, error)
	del    func(ctx context.Context, c kubernetes.Interface, ns, name string) error
}

// managedResources are the types this engine creates.
//
// Every one of them is created inside an arena namespace, wears the managed
// label, and is deleted again on clear. Adding a type here is most of the cost
// of a fault kind that needs one: createObject dispatches on the Go type, and
// both Clear and ReapExpired walk this list, so the three cannot drift apart.
//
// A type also has to be granted in the arena Role — see pkg/arena. A create
// that would be Forbidden in-cluster passes every unit test, because the fake
// clientset does not do RBAC.
var managedResources = []managedResource{
	{
		plural: "deployments",
		list: func(ctx context.Context, c kubernetes.Interface, ns, selector string) ([]metav1.ObjectMeta, error) {
			l, err := c.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
			if err != nil {
				return nil, err
			}
			out := make([]metav1.ObjectMeta, len(l.Items))
			for i := range l.Items {
				out[i] = l.Items[i].ObjectMeta
			}
			return out, nil
		},
		del: func(ctx context.Context, c kubernetes.Interface, ns, name string) error {
			return c.AppsV1().Deployments(ns).Delete(ctx, name, metav1.DeleteOptions{})
		},
	},
	{
		plural: "jobs",
		list: func(ctx context.Context, c kubernetes.Interface, ns, selector string) ([]metav1.ObjectMeta, error) {
			l, err := c.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
			if err != nil {
				return nil, err
			}
			out := make([]metav1.ObjectMeta, len(l.Items))
			for i := range l.Items {
				out[i] = l.Items[i].ObjectMeta
			}
			return out, nil
		},
		del: func(ctx context.Context, c kubernetes.Interface, ns, name string) error {
			// Foreground, unlike everything else here. A Job deleted under the
			// default policy orphans its failed pods, and a namespace littered
			// with the previous scenario's dead pods poses a diagnosis the
			// next scenario did not intend.
			policy := metav1.DeletePropagationForeground
			return c.BatchV1().Jobs(ns).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy})
		},
	},
	{
		plural: "services",
		list: func(ctx context.Context, c kubernetes.Interface, ns, selector string) ([]metav1.ObjectMeta, error) {
			l, err := c.CoreV1().Services(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
			if err != nil {
				return nil, err
			}
			out := make([]metav1.ObjectMeta, len(l.Items))
			for i := range l.Items {
				out[i] = l.Items[i].ObjectMeta
			}
			return out, nil
		},
		del: func(ctx context.Context, c kubernetes.Interface, ns, name string) error {
			return c.CoreV1().Services(ns).Delete(ctx, name, metav1.DeleteOptions{})
		},
	},
	{
		plural: "persistentvolumeclaims",
		list: func(ctx context.Context, c kubernetes.Interface, ns, selector string) ([]metav1.ObjectMeta, error) {
			l, err := c.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
			if err != nil {
				return nil, err
			}
			out := make([]metav1.ObjectMeta, len(l.Items))
			for i := range l.Items {
				out[i] = l.Items[i].ObjectMeta
			}
			return out, nil
		},
		del: func(ctx context.Context, c kubernetes.Interface, ns, name string) error {
			return c.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, name, metav1.DeleteOptions{})
		},
	},
}

// createObject applies one object of a bundle.
//
// The type switch is why builders return concrete types rather than
// unstructured objects: a kind that synthesizes something this engine has no
// create path for fails in its own unit test rather than at apply time in a
// cluster.
func createObject(ctx context.Context, c kubernetes.Interface, ns string, obj runtime.Object) error {
	switch o := obj.(type) {
	case *appsv1.Deployment:
		_, err := c.AppsV1().Deployments(ns).Create(ctx, o, metav1.CreateOptions{})
		return err
	case *batchv1.Job:
		_, err := c.BatchV1().Jobs(ns).Create(ctx, o, metav1.CreateOptions{})
		return err
	case *corev1.Service:
		_, err := c.CoreV1().Services(ns).Create(ctx, o, metav1.CreateOptions{})
		return err
	case *corev1.PersistentVolumeClaim:
		_, err := c.CoreV1().PersistentVolumeClaims(ns).Create(ctx, o, metav1.CreateOptions{})
		return err
	default:
		return fmt.Errorf("no create path for %T", obj)
	}
}

// describeObject names an object's type the way an operator would, for the
// error a failed create returns. The Go type name leaks through %T as
// "*v1.Deployment", which names our vendored package rather than the thing
// that could not be created.
func describeObject(obj runtime.Object) string {
	switch obj.(type) {
	case *appsv1.Deployment:
		return "deployment"
	case *batchv1.Job:
		return "job"
	case *corev1.Service:
		return "service"
	case *corev1.PersistentVolumeClaim:
		return "persistentvolumeclaim"
	default:
		return fmt.Sprintf("%T", obj)
	}
}
