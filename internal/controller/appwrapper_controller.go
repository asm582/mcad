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
	"errors"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	mcadv1alpha1 "tardieu/mcad/api/v1alpha1"
)

// AppWrapperReconciler reconciles an AppWrapper object
type AppWrapperReconciler struct {
	client.Client
	Events          chan event.GenericEvent
	Scheme          *runtime.Scheme
	Cache           map[types.UID]*CachedAppWrapper // cache appWrapper updates to improve dispatch accuracy
	ClusterCapacity Weights                         // cluster capacity available to mcad
	NextSync        time.Time                       // when to refresh cluster capacity
}

const (
	namespaceLabel = "mcad.codeflare.dev/namespace" // owner namespace label for wrapped resources
	nameLabel      = "mcad.codeflare.dev/name"      // owner name label for wrapped resources
	uidLabel       = "mcad.codeflare.dev/uid"       // owner UID label for wrapped resources
	finalizer      = "mcad.codeflare.dev/finalizer" // finalizer name
	nvidiaGpu      = "nvidia.com/gpu"               // GPU resource name
	specNodeName   = ".spec.nodeName"               // key to index pods based on node placement
)

// PodCounts summarize the status of the pods associated with one AppWrapper
type PodCounts struct {
	Failed    int
	Other     int
	Running   int
	Succeeded int
}

// Cached AppWrapper status
type CachedAppWrapper struct {
	// AppWrapper phase
	Phase mcadv1alpha1.AppWrapperPhase

	// Number of condition (monotonically increasing, hence a good way to identify the most recent status)
	Conditions int

	// First conflict detected between our cache and reconciler cache or nil
	Conflict *time.Time
}

// We cache AppWrappers phases because the reconciler cache does not immediately reflect updates.
// A Get or List call soon after an Update or Status.Update call may not reflect the latest object.
// See: https://github.com/kubernetes-sigs/controller-runtime/issues/1622
// Therefore we need to maintain our own cache to make sure new dispatching decisions accurately account
// for recent dispatching decisions.
// The cache is populated on phase updates.
// The cache is only meant to be used for AppWrapper List calls when computing available resources.
// We use the number of conditions to confirm our cached version is more recent than the reconciler cache.
// We remove cache entries when removing finalizers. TODO: We should purge the cache from stale entries
// periodically in case a finalizer is deleted  outside of our control.
// When reconciling an AppWrapper, we proactively detect and abort on conflicts as
// there is no point working on a stale AppWrapper. We know etcd updates will fail.
// To defend against bugs in the cache implementation and egregious AppWrapper edits,
// we eventually give up on persistent conflicts and remove the AppWrapper phase from the cache.

//+kubebuilder:rbac:groups=*,resources=*,verbs=*

// Reconcile one AppWrapper or dispatch next AppWrapper
// Normal reconciliations "namespace/name" implement all phase transitions except for Queued->Dispatching
// Queued->Dispatching transitions happen as part of a special "*/*" reconciliation
// In a "*/*" invocation, we first decide which AppWrapper to dispatch then make the transition
func (r *AppWrapperReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// log.Info("Reconcile")

	// handle dispatching first

	// req == "*/*", find the next AppWrapper to dispatch and dispatch this AppWrapper
	if req.Name == "*" {
		appWrapper, last, err := r.dispatchNext(ctx) // last == is last appWrapper in queue?
		if err != nil {
			return ctrl.Result{}, err
		}
		if appWrapper == nil { // no appWrapper eligible for dispatch
			return ctrl.Result{RequeueAfter: dispatchDelay}, nil // retry to dispatch later
		}
		// abort and requeue reconciliation if reconciler cache is stale
		if err := r.checkCache(appWrapper); err != nil {
			return ctrl.Result{}, err
		}
		if appWrapper.Status.Phase != mcadv1alpha1.Queued {
			// this check should be redundant but better be defensive
			return ctrl.Result{}, errors.New("not queued")
		}
		// set dispatching timestamp and status
		appWrapper.Status.LastDispatchTime = metav1.Now()
		if _, err := r.updateStatus(ctx, appWrapper, mcadv1alpha1.Dispatching); err != nil {
			return ctrl.Result{}, err
		}
		if last {
			return ctrl.Result{RequeueAfter: dispatchDelay}, nil // retry to dispatch later
		}
		return ctrl.Result{Requeue: true}, nil // requeue to continue to dispatch queued appWrappers
	}

	// normal reconciliation starts here

	appWrapper := &mcadv1alpha1.AppWrapper{}

	// get deep copy of AppWrapper object in reconciler cache
	if err := r.Get(ctx, req.NamespacedName, appWrapper); err != nil {
		// no such AppWrapper, nothing to reconcile, not an error
		return ctrl.Result{}, nil
	}

	// abort and requeue reconciliation if reconciler cache is stale
	if err := r.checkCache(appWrapper); err != nil {
		return ctrl.Result{}, err
	}

	// first handle deletion
	if !appWrapper.DeletionTimestamp.IsZero() {
		// delete wrapped resources
		if r.deleteResources(ctx, appWrapper) != 0 {
			return ctrl.Result{RequeueAfter: deletionDelay}, nil // requeue reconciliation
		}
		// remove finalizer
		if controllerutil.RemoveFinalizer(appWrapper, finalizer) {
			if err := r.Update(ctx, appWrapper); err != nil {
				return ctrl.Result{}, err
			}
		}
		log.Info("Deleted")
		delete(r.Cache, appWrapper.UID) // remove appWrapper from cache
		if isActivePhase(appWrapper.Status.Phase) {
			r.triggerDispatchNext() // cluster may have more available capacity
		}
		return ctrl.Result{}, nil
	}

	// handle all other phases including the default empty phase

	switch appWrapper.Status.Phase {
	case mcadv1alpha1.Succeeded, mcadv1alpha1.Failed:
		// nothing to reconcile
		return ctrl.Result{}, nil

	case mcadv1alpha1.Queued:
		// this AppWrapper may not be the head of the queue, trigger dispatch in queue order
		r.triggerDispatchNext()
		return ctrl.Result{}, nil

	case mcadv1alpha1.Requeuing:
		// delete wrapped resources
		if r.deleteResources(ctx, appWrapper) != 0 {
			if isSlowDeletion(appWrapper) {
				// give up requeuing and fail instead
				return r.updateStatus(ctx, appWrapper, mcadv1alpha1.Failed)
			} else {
				return ctrl.Result{RequeueAfter: deletionDelay}, nil // requeue reconciliation
			}
		}
		// update status to queued
		return r.updateStatus(ctx, appWrapper, mcadv1alpha1.Queued)

	case mcadv1alpha1.Dispatching:
		// dispatching is taking too long?
		if isSlowCreation(appWrapper) {
			// set requeuing or failed status
			return r.requeueOrFail(ctx, appWrapper)
		}
		// create wrapped resources
		objects, err := r.parseResources(appWrapper)
		if err != nil {
			log.Error(err, "Resource parsing error during creation")
			return r.updateStatus(ctx, appWrapper, mcadv1alpha1.Failed)
		}
		// create wrapped resources
		if err := r.createResources(ctx, objects); err != nil {
			return ctrl.Result{}, err
		}
		// set running status only after successfully requesting the creation of all resources
		return r.updateStatus(ctx, appWrapper, mcadv1alpha1.Running)

	case mcadv1alpha1.Running:
		// check AppWrapper health
		counts, err := r.monitorPods(ctx, appWrapper)
		if err != nil {
			return ctrl.Result{}, err
		}
		slow := isSlowCreation(appWrapper)
		if counts.Failed > 0 || slow && (counts.Other > 0 || counts.Running < int(appWrapper.Spec.MinPods)) {
			// set requeuing or failed status
			return r.requeueOrFail(ctx, appWrapper)
		}
		if appWrapper.Spec.MinPods > 0 && counts.Succeeded >= int(appWrapper.Spec.MinPods) && counts.Running == 0 && counts.Other == 0 {
			// set succeeded status
			return r.updateStatus(ctx, appWrapper, mcadv1alpha1.Succeeded)
		}
		if !slow {
			return ctrl.Result{RequeueAfter: creationDelay}, nil // check again soon
		}
		return ctrl.Result{}, nil // only check again on pod change

	default: // empty phase
		// add finalizer
		if controllerutil.AddFinalizer(appWrapper, finalizer) {
			if err := r.Update(ctx, appWrapper); err != nil {
				return ctrl.Result{}, err
			}
		}
		// set queued status only after adding finalizer
		return r.updateStatus(ctx, appWrapper, mcadv1alpha1.Queued)
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *AppWrapperReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// index pods with nodeName key
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &v1.Pod{}, specNodeName, func(obj client.Object) []string {
		pod := obj.(*v1.Pod)
		return []string{pod.Spec.NodeName}
	}); err != nil {
		return err
	}
	// watch AppWrapper pods in addition to AppWrappers so we can react to pod failures and other pod events
	// watch r.Events channel, which we use to trigger dispatchNext
	return ctrl.NewControllerManagedBy(mgr).
		For(&mcadv1alpha1.AppWrapper{}).
		WatchesRawSource(&source.Channel{Source: r.Events}, &handler.EnqueueRequestForObject{}).
		Watches(&v1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.podMapFunc)).
		Complete(r)
}

// Map labelled pods to corresponding AppWrappers
func (r *AppWrapperReconciler) podMapFunc(ctx context.Context, obj client.Object) []reconcile.Request {
	pod := obj.(*v1.Pod)
	if namespace, ok := pod.Labels[namespaceLabel]; ok {
		if name, ok := pod.Labels[nameLabel]; ok {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}}
		}
	}
	return nil
}

// Update AppWrapper status
func (r *AppWrapperReconciler) updateStatus(ctx context.Context, appWrapper *mcadv1alpha1.AppWrapper, phase mcadv1alpha1.AppWrapperPhase) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	now := metav1.Now()
	activeBefore := isActivePhase(appWrapper.Status.Phase)
	if phase == mcadv1alpha1.Dispatching {
		now = appWrapper.Status.LastDispatchTime // ensure condition timestamp is consistent with status field
	} else if phase == mcadv1alpha1.Requeuing {
		now = appWrapper.Status.LastRequeuingTime // ensure condition timestamp is consistent with status field
	}
	// log transition as condition
	condition := mcadv1alpha1.AppWrapperCondition{LastTransitionTime: now, Reason: string(phase)}
	appWrapper.Status.Conditions = append(appWrapper.Status.Conditions, condition)
	appWrapper.Status.Phase = phase
	if err := r.Status().Update(ctx, appWrapper); err != nil {
		return ctrl.Result{}, err // etcd update failed, abort and requeue reconciliation
	}
	log.Info(string(phase))
	// cache AppWrapper status
	r.Cache[appWrapper.UID] = &CachedAppWrapper{Phase: appWrapper.Status.Phase, Conditions: len(appWrapper.Status.Conditions)}
	activeAfter := isActivePhase(phase)
	if activeBefore && !activeAfter {
		r.triggerDispatchNext() // cluster may have more available capacity
	}
	return ctrl.Result{}, nil
}

// Set requeuing or failed status depending on retry count
func (r *AppWrapperReconciler) requeueOrFail(ctx context.Context, appWrapper *mcadv1alpha1.AppWrapper) (ctrl.Result, error) {
	if appWrapper.Status.Requeued < appWrapper.Spec.MaxRetries {
		appWrapper.Status.Requeued += 1
		appWrapper.Status.LastRequeuingTime = metav1.Now()
		return r.updateStatus(ctx, appWrapper, mcadv1alpha1.Requeuing)
	}
	return r.updateStatus(ctx, appWrapper, mcadv1alpha1.Failed)
}

// Trigger dispatch
func (r *AppWrapperReconciler) triggerDispatchNext() {
	select {
	case r.Events <- event.GenericEvent{Object: &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Namespace: "*", Name: "*"}}}:
	default:
	}
}

// Check whether our cache and reconciler cache appear to be in sync
func (r *AppWrapperReconciler) checkCache(appWrapper *mcadv1alpha1.AppWrapper) error {
	if cached, ok := r.Cache[appWrapper.UID]; ok {
		// check number of conditions
		if cached.Conditions > len(appWrapper.Status.Conditions) {
			// reconciler cache appears to be behind
			if cached.Conflict != nil {
				if time.Now().After(cached.Conflict.Add(cacheConflictTimeout)) {
					// this has been going on for a while, assume something is wrong with our cache
					delete(r.Cache, appWrapper.UID)
					return errors.New("persistent cache conflict") // force redo
				}
			} else {
				now := time.Now()
				cached.Conflict = &now // remember when conflict started
			}
			return errors.New("stale reconciler cache") // force redo
		}
		if cached.Conditions < len(appWrapper.Status.Conditions) || cached.Phase != appWrapper.Status.Phase {
			// something is wrong with our cache
			delete(r.Cache, appWrapper.UID)
			return errors.New("stale phase cache") // force redo
		}
		// caches appear to be in sync
		cached.Conflict = nil // clear conflict timestamp
	}
	return nil
}
