/*
Copyright 2023.

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

package controller

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/strings/slices"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	mcadv1alpha1 "tardieu/mcad/api/v1alpha1"
)

// AppWrapperReconciler reconciles a AppWrapper object
type AppWrapperReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const label = "mcad.my.domain/appwrapper"
const finalizer = "mcad.my.domain/finalizer"

type PodCounts struct {
	Failed    int
	Other     int
	Running   int
	Succeeded int
}

//+kubebuilder:rbac:groups=mcad.my.domain,resources=appwrappers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=mcad.my.domain,resources=appwrappers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=mcad.my.domain,resources=appwrappers/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the AppWrapper object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.15.0/pkg/reconcile
func (r *AppWrapperReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	log.Info("Reconcile")

	aw := &mcadv1alpha1.AppWrapper{}

	if err := r.Get(ctx, req.NamespacedName, aw); err != nil {
		// no such appwrapper, nothing to reconcile, not an error
		return ctrl.Result{}, nil
	}

	// deletion requested
	if !aw.ObjectMeta.DeletionTimestamp.IsZero() && aw.Status.Phase != "Terminating" {
		return ctrl.Result{}, r.updateStatus(ctx, aw, "Terminating")
	}

	switch aw.Status.Phase {
	case "Completed", "Failed":
		// nothing to reconcile
		return ctrl.Result{}, nil

	case "Terminating":
		// delete wrapped resources
		if r.deleteResources(ctx, aw) != 0 {
			if slowDeletion(aw) {
				log.Error(nil, "Resource deletion timeout")
			} else {
				return ctrl.Result{RequeueAfter: time.Minute}, nil // requeue
			}
		}
		// remove finalizer
		if controllerutil.RemoveFinalizer(aw, finalizer) {
			if err := r.Update(ctx, aw); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil

	case "Requeuing":
		// delete wrapped resources
		if r.deleteResources(ctx, aw) != 0 {
			if slowDeletion(aw) {
				log.Error(nil, "Resource deletion timeout")
			} else {
				return ctrl.Result{RequeueAfter: time.Minute}, nil // requeue
			}
		}
		// update status to queued
		return ctrl.Result{}, r.updateStatus(ctx, aw, "Queued")

	case "Queued":
		// check if appwrapper fits available resources
		shouldDispatch, err := r.shouldDispatch(ctx, aw)
		if err != nil {
			return ctrl.Result{}, err
		}
		if shouldDispatch {
			// set dispatching status
			aw.Status.LastDispatchTime = metav1.Now()
			return ctrl.Result{}, r.updateStatus(ctx, aw, "Dispatching")
		}
		// if not, retry after a delay
		return ctrl.Result{RequeueAfter: time.Minute}, nil

	case "Dispatching":
		// dispatching is taking too long?
		if slowDispatch(aw) {
			// set requeuing or failed status
			if aw.Status.Requeued < aw.Spec.MaxRetries {
				aw.Status.Requeued += 1
				return ctrl.Result{}, r.updateStatus(ctx, aw, "Requeuing")
			}
			return ctrl.Result{}, r.updateStatus(ctx, aw, "Failed")
		}
		// create wrapped resources
		objects, err := r.parseResources(aw)
		if err != nil {
			log.Error(err, "Resource parsing error during creation")
			return ctrl.Result{}, r.updateStatus(ctx, aw, "Failed")
		}
		// create wrapped resources
		if err := r.createResources(ctx, objects); err != nil {
			return ctrl.Result{}, err
		}
		// set running status only after successfully requesting the creation of all resources
		return ctrl.Result{}, r.updateStatus(ctx, aw, "Running")

	case "Running":
		// check appwrapper health
		counts, err := r.monitorPods(ctx, aw)
		if err != nil {
			return ctrl.Result{}, err
		}
		slow := slowDispatch(aw)
		if counts.Failed > 0 || slow && (counts.Other > 0 || counts.Running < int(aw.Spec.Pods)) {
			// set requeuing or failed status
			if aw.Status.Requeued < aw.Spec.MaxRetries {
				aw.Status.Requeued += 1
				return ctrl.Result{}, r.updateStatus(ctx, aw, "Requeuing")
			}
			return ctrl.Result{}, r.updateStatus(ctx, aw, "Failed")
		}
		if counts.Succeeded >= int(aw.Spec.Pods) && counts.Running == 0 && counts.Other == 0 {
			// set completed status
			return ctrl.Result{}, r.updateStatus(ctx, aw, "Completed")
		}
		if !slow {
			return ctrl.Result{RequeueAfter: time.Minute}, nil // check soon
		}
		return ctrl.Result{}, nil

	default: // empty phase
		// add finalizer
		if controllerutil.AddFinalizer(aw, finalizer) {
			if err := r.Update(ctx, aw); err != nil {
				return ctrl.Result{}, err
			}
		}
		// set queued status only after adding finalizer
		return ctrl.Result{}, r.updateStatus(ctx, aw, "Queued")
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *AppWrapperReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &v1.Pod{}, ".spec.nodeName", func(obj client.Object) []string {
		pod := obj.(*v1.Pod)
		return []string{pod.Spec.NodeName}
	}); err != nil {
		return err
	}

	// watch pods in addition to appwrappers
	return ctrl.NewControllerManagedBy(mgr).
		For(&mcadv1alpha1.AppWrapper{}).
		WatchesMetadata(&v1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.podMapFunc)).
		Complete(r)
}

// Map labelled pods to appwrappers
func (r *AppWrapperReconciler) podMapFunc(ctx context.Context, obj client.Object) []reconcile.Request {
	pod := obj.(*metav1.PartialObjectMetadata)
	if aw, ok := pod.ObjectMeta.Labels[label]; ok {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: pod.Namespace, Name: aw}}}
	}
	return nil
}

// Update appwrapper status
func (r *AppWrapperReconciler) updateStatus(ctx context.Context, aw *mcadv1alpha1.AppWrapper, phase string) error {
	log := log.FromContext(ctx)
	now := metav1.Now()
	if phase == "Dispatching" {
		now = aw.Status.LastDispatchTime // ensure timestamps are consistent
	}
	condition := mcadv1alpha1.AppWrapperCondition{LastTransitionTime: now, Reason: phase}
	aw.Status.Conditions = append(aw.Status.Conditions, condition)
	aw.Status.Phase = phase
	if err := r.Status().Update(ctx, aw); err != nil {
		return err
	}
	log.Info(phase)
	return nil
}

// Test if appwrapper fits available resources
func (r *AppWrapperReconciler) shouldDispatch(ctx context.Context, aw *mcadv1alpha1.AppWrapper) (bool, error) {
	gpus := 0 // available gpus
	// add available gpus for each schedulable node
	nodes := &v1.NodeList{}
	if err := r.List(ctx, nodes, client.UnsafeDisableDeepCopy); err != nil {
		return false, err
	}
	for _, node := range nodes.Items {
		// skip unschedulable nodes
		if node.Spec.Unschedulable {
			continue
		}
		// add allocatable gpus
		g := node.Status.Allocatable["nvidia.com/gpu"]
		gpus += int(g.Value())
		// subtract gpus used by non-appwrapper, non-terminated pods on this node
		fieldSelector, err := fields.ParseSelector(".spec.nodeName=" + node.Name)
		if err != nil {
			return false, err
		}
		pods := &v1.PodList{}
		if err := r.List(ctx, pods, client.UnsafeDisableDeepCopy,
			client.MatchingFieldsSelector{Selector: fieldSelector}); err != nil {
			return false, err
		}
		for _, pod := range pods.Items {
			if _, ok := pod.GetLabels()[label]; !ok && pod.Status.Phase != v1.PodFailed && pod.Status.Phase != v1.PodSucceeded {
				for _, container := range pod.Spec.Containers {
					g := container.Resources.Requests["nvidia.com/gpu"]
					gpus -= int(g.Value())
				}
			}
		}
	}
	// subtract gpus used by non-preemptable appwrappers
	aws := &mcadv1alpha1.AppWrapperList{}
	if err := r.List(ctx, aws, client.UnsafeDisableDeepCopy); err != nil {
		return false, err
	}
	for _, a := range aws.Items {
		if a.UID != aw.UID {
			if (slices.Contains([]string{"Dispatching", "Running", "Terminating", "Failed", "Requeuing"}, a.Status.Phase)) &&
				a.Spec.Priority >= aw.Spec.Priority {
				gpus -= gpuRequest(&a)
			}
		}
	}
	return gpuRequest(aw) <= gpus, nil
}

// Count gpu requested by appwrapper
func gpuRequest(aw *mcadv1alpha1.AppWrapper) int {
	gpus := 0
	for _, resource := range aw.Spec.Resources {
		g := resource.Requests["nvidia.com/gpu"]
		gpus += int(resource.Replicas) * int(g.Value())
	}
	return gpus
}

// Monitor appwrapper pods
func (r *AppWrapperReconciler) monitorPods(ctx context.Context, aw *mcadv1alpha1.AppWrapper) (*PodCounts, error) {
	// list matching pods
	pods := &v1.PodList{}
	if err := r.List(ctx, pods, client.UnsafeDisableDeepCopy,
		client.InNamespace(aw.ObjectMeta.Namespace),
		client.MatchingLabels{label: aw.ObjectMeta.Name}); err != nil {
		return nil, err
	}
	counts := &PodCounts{}
	for _, pod := range pods.Items {
		switch pod.Status.Phase {
		case "Succeeded":
			counts.Succeeded += 1
		case "Running":
			counts.Running += 1
		case "Failed":
			counts.Failed += 1
		default:
			counts.Other += 1
		}
	}
	return counts, nil
}

// Parse raw resource into client object
func (r *AppWrapperReconciler) parseResource(aw *mcadv1alpha1.AppWrapper, raw []byte) (client.Object, error) {
	into, _, err := unstructured.UnstructuredJSONScheme.Decode(raw, nil, nil)
	if err != nil {
		return nil, err
	}
	obj := into.(client.Object)
	namespaced, err := r.IsObjectNamespaced(obj)
	if err != nil {
		return nil, err
	}
	if namespaced && obj.GetNamespace() == "" {
		obj.SetNamespace(aw.ObjectMeta.Namespace) // use appwrapper namespace as default
	}
	obj.SetLabels(map[string]string{label: aw.ObjectMeta.Name}) // add appwrapper label
	return obj, nil
}

// Parse raw resources
func (r *AppWrapperReconciler) parseResources(aw *mcadv1alpha1.AppWrapper) ([]client.Object, error) {
	objects := make([]client.Object, len(aw.Spec.Resources))
	var err error
	for i, resource := range aw.Spec.Resources {
		objects[i], err = r.parseResource(aw, resource.Template.Raw)
		if err != nil {
			return nil, err
		}
	}
	return objects, err
}

// Create wrapped resources
func (r *AppWrapperReconciler) createResources(ctx context.Context, objects []client.Object) error {
	for _, obj := range objects {
		if err := r.Create(ctx, obj); err != nil {
			if !errors.IsAlreadyExists(err) { // ignore existing resources
				return err
			}
		}
	}
	return nil
}

// Delete wrapped resources, returning count of pending deletions
func (r *AppWrapperReconciler) deleteResources(ctx context.Context, aw *mcadv1alpha1.AppWrapper) int {
	log := log.FromContext(ctx)
	count := 0
	for _, resource := range aw.Spec.Resources {
		obj, err := r.parseResource(aw, resource.Template.Raw)
		if err != nil {
			log.Error(err, "Resource parsing error during deletion")
			continue
		}
		background := metav1.DeletePropagationBackground
		if err := r.Delete(ctx, obj, &client.DeleteOptions{PropagationPolicy: &background}); err != nil {
			if errors.IsNotFound(err) {
				continue // ignore missing resources
			}
			log.Error(err, "Resource deletion error")
		}
		count += 1
	}
	return count
}

func slowDispatch(aw *mcadv1alpha1.AppWrapper) bool {
	return metav1.Now().After(aw.Status.LastDispatchTime.Add(2 * time.Minute))
}

func slowDeletion(aw *mcadv1alpha1.AppWrapper) bool {
	return metav1.Now().After(aw.ObjectMeta.DeletionTimestamp.Add(2 * time.Minute))
}