/*
Copyright 2024 IBM Corporation.

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

package appwrapper

import (
	"context"
	"fmt"
	"strconv"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	workloadv1beta2 "github.com/project-codeflare/appwrapper/api/v1beta2"
	"github.com/project-codeflare/appwrapper/internal/controller/awstatus"
	"github.com/project-codeflare/appwrapper/pkg/config"
	"github.com/project-codeflare/appwrapper/pkg/utils"
)

const (
	AppWrapperLabel     = "workload.codeflare.dev/appwrapper"
	AppWrapperFinalizer = "workload.codeflare.dev/finalizer"
	childJobQueueName   = "workload.codeflare.dev.admitted"
)

// AppWrapperReconciler reconciles an appwrapper
type AppWrapperReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Config *config.AppWrapperConfig
}

type podStatusSummary struct {
	expected  int32
	pending   int32
	running   int32
	succeeded int32
	failed    int32
}

type componentStatusSummary struct {
	expected int32
	deployed int32
}

// permission to fully control appwrappers
//+kubebuilder:rbac:groups=workload.codeflare.dev,resources=appwrappers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=workload.codeflare.dev,resources=appwrappers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=workload.codeflare.dev,resources=appwrappers/finalizers,verbs=update

// permission to edit wrapped resources: pods, services, jobs, podgroups, pytorchjobs, rayclusters

//+kubebuilder:rbac:groups="",resources=pods;services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=scheduling.sigs.k8s.io,resources=podgroups,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=scheduling.x-k8s.io,resources=podgroups,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=kubeflow.org,resources=pytorchjobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cluster.ray.io,resources=rayclusters,verbs=get;list;watch;create;update;patch;delete

// Reconcile reconciles an appwrapper
// Please see [aw-states] for documentation of this method.
//
// [aw-states]: https://project-codeflare.github.io/appwrapper/arch-controller/#framework-controller
//
//gocyclo:ignore
func (r *AppWrapperReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	aw := &workloadv1beta2.AppWrapper{}
	if err := r.Get(ctx, req.NamespacedName, aw); err != nil {
		return ctrl.Result{}, nil
	}

	// handle deletion first
	if !aw.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(aw, AppWrapperFinalizer) {
			statusUpdated := false
			if meta.IsStatusConditionTrue(aw.Status.Conditions, string(workloadv1beta2.ResourcesDeployed)) {
				if !r.deleteComponents(ctx, aw) {
					// one or more components are still terminating
					if aw.Status.Phase != workloadv1beta2.AppWrapperTerminating {
						// Set Phase for better UX, but ignore errors. We still want to requeue after 5 seconds (not immediately)
						aw.Status.Phase = workloadv1beta2.AppWrapperTerminating
						_ = r.Status().Update(ctx, aw)
					}
					return ctrl.Result{RequeueAfter: 5 * time.Second}, nil // check after a short while
				}
				meta.SetStatusCondition(&aw.Status.Conditions, metav1.Condition{
					Type:    string(workloadv1beta2.ResourcesDeployed),
					Status:  metav1.ConditionFalse,
					Reason:  string(workloadv1beta2.AppWrapperTerminating),
					Message: "Resources successfully deleted",
				})
				statusUpdated = true
			}

			if meta.IsStatusConditionTrue(aw.Status.Conditions, string(workloadv1beta2.QuotaReserved)) {
				meta.SetStatusCondition(&aw.Status.Conditions, metav1.Condition{
					Type:    string(workloadv1beta2.QuotaReserved),
					Status:  metav1.ConditionFalse,
					Reason:  string(workloadv1beta2.AppWrapperTerminating),
					Message: "No resources deployed",
				})
				statusUpdated = true
			}
			if statusUpdated {
				if err := r.Status().Update(ctx, aw); err != nil {
					return ctrl.Result{}, err
				}
			}

			if controllerutil.RemoveFinalizer(aw, AppWrapperFinalizer) {
				if err := r.Update(ctx, aw); err != nil {
					return ctrl.Result{}, err
				}
				log.FromContext(ctx).Info("Finalizer Deleted")
			}
		}
		return ctrl.Result{}, nil
	}

	switch aw.Status.Phase {

	case workloadv1beta2.AppWrapperEmpty: // initial state, inject finalizer
		if controllerutil.AddFinalizer(aw, AppWrapperFinalizer) {
			if err := r.Update(ctx, aw); err != nil {
				return ctrl.Result{}, err
			}
		}

		if err := awstatus.EnsureComponentStatusInitialized(ctx, aw); err != nil {
			return ctrl.Result{}, err
		}

		return r.updateStatus(ctx, aw, workloadv1beta2.AppWrapperSuspended)

	case workloadv1beta2.AppWrapperSuspended: // no components deployed
		if aw.Spec.Suspend {
			return ctrl.Result{}, nil // remain suspended
		}

		// begin deployment
		meta.SetStatusCondition(&aw.Status.Conditions, metav1.Condition{
			Type:    string(workloadv1beta2.QuotaReserved),
			Status:  metav1.ConditionTrue,
			Reason:  string(workloadv1beta2.AppWrapperResuming),
			Message: "Suspend is false",
		})
		meta.SetStatusCondition(&aw.Status.Conditions, metav1.Condition{
			Type:    string(workloadv1beta2.ResourcesDeployed),
			Status:  metav1.ConditionTrue,
			Reason:  string(workloadv1beta2.AppWrapperResuming),
			Message: "Suspend is false",
		})
		meta.SetStatusCondition(&aw.Status.Conditions, metav1.Condition{
			Type:    string(workloadv1beta2.PodsReady),
			Status:  metav1.ConditionFalse,
			Reason:  string(workloadv1beta2.AppWrapperResuming),
			Message: "Suspend is false",
		})
		meta.SetStatusCondition(&aw.Status.Conditions, metav1.Condition{
			Type:    string(workloadv1beta2.Unhealthy),
			Status:  metav1.ConditionFalse,
			Reason:  string(workloadv1beta2.AppWrapperResuming),
			Message: "Suspend is false",
		})
		return r.updateStatus(ctx, aw, workloadv1beta2.AppWrapperResuming)

	case workloadv1beta2.AppWrapperResuming: // deploying components
		if aw.Spec.Suspend {
			return r.updateStatus(ctx, aw, workloadv1beta2.AppWrapperSuspending) // abort deployment
		}
		err, fatal := r.createComponents(ctx, aw)
		if err != nil {
			meta.SetStatusCondition(&aw.Status.Conditions, metav1.Condition{
				Type:    string(workloadv1beta2.Unhealthy),
				Status:  metav1.ConditionTrue,
				Reason:  "CreateFailed",
				Message: fmt.Sprintf("error creating components: %v", err),
			})
			if fatal {
				return r.updateStatus(ctx, aw, workloadv1beta2.AppWrapperFailed) // abort on fatal error
			} else {
				return r.resetOrFail(ctx, aw)
			}
		}
		return r.updateStatus(ctx, aw, workloadv1beta2.AppWrapperRunning)

	case workloadv1beta2.AppWrapperRunning: // components deployed
		if aw.Spec.Suspend {
			return r.updateStatus(ctx, aw, workloadv1beta2.AppWrapperSuspending) // begin undeployment
		}

		// First, check the Component-level status of the workload
		compStatus, err := r.getComponentStatus(ctx, aw)
		if err != nil {
			return ctrl.Result{}, err
		}

		// Detect externally deleted components and transition to Failed with no GracePeriod or retry
		if compStatus.deployed != compStatus.expected {
			meta.SetStatusCondition(&aw.Status.Conditions, metav1.Condition{
				Type:    string(workloadv1beta2.Unhealthy),
				Status:  metav1.ConditionTrue,
				Reason:  "MissingComponent",
				Message: fmt.Sprintf("Only found %v deployed components, but was expecting %v", compStatus.deployed, compStatus.expected),
			})
			return r.updateStatus(ctx, aw, workloadv1beta2.AppWrapperFailed)
		}

		// Second, check the Pod-level status of the workload
		podStatus, err := r.getPodStatus(ctx, aw)
		if err != nil {
			return ctrl.Result{}, err
		}

		// Handle Success
		if podStatus.succeeded >= podStatus.expected && (podStatus.pending+podStatus.running+podStatus.failed == 0) {
			msg := fmt.Sprintf("%v pods succeeded and no running, pending, or failed pods", podStatus.succeeded)
			meta.SetStatusCondition(&aw.Status.Conditions, metav1.Condition{
				Type:    string(workloadv1beta2.QuotaReserved),
				Status:  metav1.ConditionFalse,
				Reason:  string(workloadv1beta2.AppWrapperSucceeded),
				Message: msg,
			})
			meta.SetStatusCondition(&aw.Status.Conditions, metav1.Condition{
				Type:    string(workloadv1beta2.ResourcesDeployed),
				Status:  metav1.ConditionTrue,
				Reason:  string(workloadv1beta2.AppWrapperSucceeded),
				Message: msg,
			})
			return r.updateStatus(ctx, aw, workloadv1beta2.AppWrapperSucceeded)
		}

		// Handle Failed Pods
		if podStatus.failed > 0 {
			meta.SetStatusCondition(&aw.Status.Conditions, metav1.Condition{
				Type:   string(workloadv1beta2.Unhealthy),
				Status: metav1.ConditionTrue,
				Reason: "FoundFailedPods",
				// Intentionally no detailed message with failed pod count, since changing the message resets the transition time
			})

			// Grace period to give the resource controller a chance to correct the failure
			whenDetected := meta.FindStatusCondition(aw.Status.Conditions, string(workloadv1beta2.Unhealthy)).LastTransitionTime
			gracePeriod := r.failureGraceDuration(ctx, aw)
			now := time.Now()
			deadline := whenDetected.Add(gracePeriod)
			if now.Before(deadline) {
				return ctrl.Result{RequeueAfter: deadline.Sub(now)}, r.Status().Update(ctx, aw)
			} else {
				return r.resetOrFail(ctx, aw)
			}
		}

		clearCondition(aw, workloadv1beta2.Unhealthy, "FoundNoFailedPods", "")

		if podStatus.running+podStatus.succeeded >= podStatus.expected {
			meta.SetStatusCondition(&aw.Status.Conditions, metav1.Condition{
				Type:    string(workloadv1beta2.PodsReady),
				Status:  metav1.ConditionTrue,
				Reason:  "SufficientPodsReady",
				Message: fmt.Sprintf("%v pods running; %v pods succeeded", podStatus.running, podStatus.succeeded),
			})
			return ctrl.Result{RequeueAfter: time.Minute}, r.Status().Update(ctx, aw)
		}

		// Not ready yet; either continue to wait or giveup if the warmup period has expired
		podDetailsMessage := fmt.Sprintf("%v pods pending; %v pods running; %v pods succeeded", podStatus.pending, podStatus.running, podStatus.succeeded)
		clearCondition(aw, workloadv1beta2.PodsReady, "InsufficientPodsReady", podDetailsMessage)
		whenDeployed := meta.FindStatusCondition(aw.Status.Conditions, string(workloadv1beta2.ResourcesDeployed)).LastTransitionTime
		var graceDuration time.Duration
		if podStatus.pending+podStatus.running+podStatus.succeeded >= podStatus.expected {
			graceDuration = r.warmupGraceDuration(ctx, aw)
		} else {
			graceDuration = r.admissionGraceDuration(ctx, aw)
		}
		if time.Now().Before(whenDeployed.Add(graceDuration)) {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, r.Status().Update(ctx, aw)
		} else {
			meta.SetStatusCondition(&aw.Status.Conditions, metav1.Condition{
				Type:    string(workloadv1beta2.Unhealthy),
				Status:  metav1.ConditionTrue,
				Reason:  "InsufficientPodsReady",
				Message: podDetailsMessage,
			})
			return r.resetOrFail(ctx, aw)
		}

	case workloadv1beta2.AppWrapperSuspending: // undeploying components
		// finish undeploying components irrespective of desired state (suspend bit)
		if meta.IsStatusConditionTrue(aw.Status.Conditions, string(workloadv1beta2.ResourcesDeployed)) {
			if !r.deleteComponents(ctx, aw) {
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			meta.SetStatusCondition(&aw.Status.Conditions, metav1.Condition{
				Type:    string(workloadv1beta2.ResourcesDeployed),
				Status:  metav1.ConditionFalse,
				Reason:  string(workloadv1beta2.AppWrapperSuspended),
				Message: "Suspend is true",
			})
		}
		meta.SetStatusCondition(&aw.Status.Conditions, metav1.Condition{
			Type:    string(workloadv1beta2.QuotaReserved),
			Status:  metav1.ConditionFalse,
			Reason:  string(workloadv1beta2.AppWrapperSuspended),
			Message: "Suspend is true",
		})
		clearCondition(aw, workloadv1beta2.PodsReady, string(workloadv1beta2.AppWrapperSuspended), "")
		clearCondition(aw, workloadv1beta2.Unhealthy, string(workloadv1beta2.AppWrapperSuspended), "")
		return r.updateStatus(ctx, aw, workloadv1beta2.AppWrapperSuspended)

	case workloadv1beta2.AppWrapperResetting:
		if aw.Spec.Suspend {
			return r.updateStatus(ctx, aw, workloadv1beta2.AppWrapperSuspending) // Suspending trumps Resetting
		}

		clearCondition(aw, workloadv1beta2.PodsReady, string(workloadv1beta2.AppWrapperResetting), "")
		if meta.IsStatusConditionTrue(aw.Status.Conditions, string(workloadv1beta2.ResourcesDeployed)) {
			if !r.deleteComponents(ctx, aw) {
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			meta.SetStatusCondition(&aw.Status.Conditions, metav1.Condition{
				Type:    string(workloadv1beta2.ResourcesDeployed),
				Status:  metav1.ConditionFalse,
				Reason:  string(workloadv1beta2.AppWrapperResetting),
				Message: "Resources deleted for resetting AppWrapper",
			})
		}

		// Pause before transitioning to Resuming to heuristically allow transient system problems to subside
		whenReset := meta.FindStatusCondition(aw.Status.Conditions, string(workloadv1beta2.Unhealthy)).LastTransitionTime
		pauseDuration := r.retryPauseDuration(ctx, aw)
		now := time.Now()
		deadline := whenReset.Add(pauseDuration)
		if now.Before(deadline) {
			return ctrl.Result{RequeueAfter: deadline.Sub(now)}, r.Status().Update(ctx, aw)
		}

		meta.SetStatusCondition(&aw.Status.Conditions, metav1.Condition{
			Type:    string(workloadv1beta2.ResourcesDeployed),
			Status:  metav1.ConditionTrue,
			Reason:  string(workloadv1beta2.AppWrapperResuming),
			Message: "Reset complete; resuming",
		})
		return r.updateStatus(ctx, aw, workloadv1beta2.AppWrapperResuming)

	case workloadv1beta2.AppWrapperFailed:
		// Support for debugging failed jobs.
		// When an appwrapper is annotated with a non-zero debugging delay,
		// we hold quota for the delay period and do not delete the resources of
		// a failed appwrapper unless Kueue preempts it by setting Suspend to true.
		deletionDelay := r.deletionOnFailureGraceDuration(ctx, aw)

		if deletionDelay > 0 && !aw.Spec.Suspend {
			meta.SetStatusCondition(&aw.Status.Conditions, metav1.Condition{
				Type:    string(workloadv1beta2.DeletingResources),
				Status:  metav1.ConditionFalse,
				Reason:  "DeletionPaused",
				Message: fmt.Sprintf("%v has value %v", workloadv1beta2.DeletionOnFailureGracePeriodAnnotation, deletionDelay),
			})
			whenDelayed := meta.FindStatusCondition(aw.Status.Conditions, string(workloadv1beta2.DeletingResources)).LastTransitionTime

			now := time.Now()
			deadline := whenDelayed.Add(deletionDelay)
			if now.Before(deadline) {
				return ctrl.Result{RequeueAfter: deadline.Sub(now)}, r.Status().Update(ctx, aw)
			}
		}

		if meta.IsStatusConditionTrue(aw.Status.Conditions, string(workloadv1beta2.ResourcesDeployed)) {
			if !r.deleteComponents(ctx, aw) {
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			msg := "Resources deleted for failed AppWrapper"
			if deletionDelay > 0 && aw.Spec.Suspend {
				msg = "Kueue forced resource deletion by suspending AppWrapper"
			}
			meta.SetStatusCondition(&aw.Status.Conditions, metav1.Condition{
				Type:    string(workloadv1beta2.ResourcesDeployed),
				Status:  metav1.ConditionFalse,
				Reason:  string(workloadv1beta2.AppWrapperFailed),
				Message: msg,
			})
		}
		meta.SetStatusCondition(&aw.Status.Conditions, metav1.Condition{
			Type:    string(workloadv1beta2.QuotaReserved),
			Status:  metav1.ConditionFalse,
			Reason:  string(workloadv1beta2.AppWrapperFailed),
			Message: "No resources deployed",
		})
		return ctrl.Result{}, r.Status().Update(ctx, aw)

	case workloadv1beta2.AppWrapperSucceeded:
		if meta.IsStatusConditionTrue(aw.Status.Conditions, string(workloadv1beta2.ResourcesDeployed)) {
			deletionDelay := r.timeToLiveAfterSucceededDuration(ctx, aw)
			whenSucceeded := meta.FindStatusCondition(aw.Status.Conditions, string(workloadv1beta2.ResourcesDeployed)).LastTransitionTime
			now := time.Now()
			deadline := whenSucceeded.Add(deletionDelay)
			if now.Before(deadline) {
				return ctrl.Result{RequeueAfter: deadline.Sub(now)}, r.Status().Update(ctx, aw)
			}

			if !r.deleteComponents(ctx, aw) {
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			meta.SetStatusCondition(&aw.Status.Conditions, metav1.Condition{
				Type:    string(workloadv1beta2.ResourcesDeployed),
				Status:  metav1.ConditionFalse,
				Reason:  string(workloadv1beta2.AppWrapperSucceeded),
				Message: fmt.Sprintf("Time to live after success of %v expired", deletionDelay),
			})
			return ctrl.Result{}, r.Status().Update(ctx, aw)
		}
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, nil
}

func (r *AppWrapperReconciler) updateStatus(ctx context.Context, aw *workloadv1beta2.AppWrapper, phase workloadv1beta2.AppWrapperPhase) (ctrl.Result, error) {
	aw.Status.Phase = phase
	if err := r.Status().Update(ctx, aw); err != nil {
		return ctrl.Result{}, err
	}
	log.FromContext(ctx).Info(string(phase), "phase", phase)
	return ctrl.Result{}, nil
}

func (r *AppWrapperReconciler) resetOrFail(ctx context.Context, aw *workloadv1beta2.AppWrapper) (ctrl.Result, error) {
	maxRetries := r.retryLimit(ctx, aw)
	if aw.Status.Retries < maxRetries {
		aw.Status.Retries += 1
		return r.updateStatus(ctx, aw, workloadv1beta2.AppWrapperResetting)
	} else {
		return r.updateStatus(ctx, aw, workloadv1beta2.AppWrapperFailed)
	}
}

func (r *AppWrapperReconciler) getPodStatus(ctx context.Context, aw *workloadv1beta2.AppWrapper) (*podStatusSummary, error) {
	pods := &v1.PodList{}
	if err := r.List(ctx, pods,
		client.InNamespace(aw.Namespace),
		client.MatchingLabels{AppWrapperLabel: aw.Name}); err != nil {
		return nil, err
	}
	summary := &podStatusSummary{expected: utils.ExpectedPodCount(aw)}

	for _, pod := range pods.Items {
		switch pod.Status.Phase {
		case v1.PodPending:
			summary.pending += 1
		case v1.PodRunning:
			summary.running += 1
		case v1.PodSucceeded:
			summary.succeeded += 1
		case v1.PodFailed:
			summary.failed += 1
		}
	}

	return summary, nil
}

func (r *AppWrapperReconciler) getComponentStatus(ctx context.Context, aw *workloadv1beta2.AppWrapper) (*componentStatusSummary, error) {
	summary := &componentStatusSummary{expected: int32(len(aw.Status.ComponentStatus))}

	for componentIdx := range aw.Status.ComponentStatus {
		cs := &aw.Status.ComponentStatus[componentIdx]
		obj := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{Kind: cs.Kind, APIVersion: cs.APIVersion}}
		if err := r.Get(ctx, types.NamespacedName{Name: cs.Name, Namespace: aw.Namespace}, obj); err == nil {
			summary.deployed += 1
		} else {
			if apierrors.IsNotFound(err) {
				meta.SetStatusCondition(&aw.Status.ComponentStatus[componentIdx].Conditions, metav1.Condition{
					Type:   string(workloadv1beta2.Unhealthy),
					Status: metav1.ConditionTrue,
					Reason: "ComponentNotFound",
				})
			} else {
				return nil, err
			}
		}
	}

	return summary, nil
}

func (r *AppWrapperReconciler) limitDuration(desired time.Duration) time.Duration {
	if desired < 0 {
		return 0 * time.Second
	} else if desired > r.Config.FaultTolerance.GracePeriodMaximum {
		return r.Config.FaultTolerance.GracePeriodMaximum
	} else {
		return desired
	}
}

func (r *AppWrapperReconciler) admissionGraceDuration(ctx context.Context, aw *workloadv1beta2.AppWrapper) time.Duration {
	if userPeriod, ok := aw.Annotations[workloadv1beta2.AdmissionGracePeriodDurationAnnotation]; ok {
		if duration, err := time.ParseDuration(userPeriod); err == nil {
			return r.limitDuration(duration)
		} else {
			log.FromContext(ctx).Error(err, "Malformed admission grace period annotation; using default", "annotation", userPeriod)
		}
	}
	return r.limitDuration(r.Config.FaultTolerance.AdmissionGracePeriod)
}

func (r *AppWrapperReconciler) warmupGraceDuration(ctx context.Context, aw *workloadv1beta2.AppWrapper) time.Duration {
	if userPeriod, ok := aw.Annotations[workloadv1beta2.WarmupGracePeriodDurationAnnotation]; ok {
		if duration, err := time.ParseDuration(userPeriod); err == nil {
			return r.limitDuration(duration)
		} else {
			log.FromContext(ctx).Error(err, "Malformed warmup grace period annotation; using default", "annotation", userPeriod)
		}
	}
	return r.limitDuration(r.Config.FaultTolerance.WarmupGracePeriod)
}

func (r *AppWrapperReconciler) failureGraceDuration(ctx context.Context, aw *workloadv1beta2.AppWrapper) time.Duration {
	if userPeriod, ok := aw.Annotations[workloadv1beta2.FailureGracePeriodDurationAnnotation]; ok {
		if duration, err := time.ParseDuration(userPeriod); err == nil {
			return r.limitDuration(duration)
		} else {
			log.FromContext(ctx).Error(err, "Malformed failure grace period annotation; using default", "annotation", userPeriod)
		}
	}
	return r.limitDuration(r.Config.FaultTolerance.FailureGracePeriod)
}

func (r *AppWrapperReconciler) retryLimit(ctx context.Context, aw *workloadv1beta2.AppWrapper) int32 {
	if userLimit, ok := aw.Annotations[workloadv1beta2.RetryLimitAnnotation]; ok {
		if limit, err := strconv.Atoi(userLimit); err == nil {
			return int32(limit)
		} else {
			log.FromContext(ctx).Error(err, "Malformed retry limit annotation; using default", "annotation", userLimit)
		}
	}
	return r.Config.FaultTolerance.RetryLimit
}

func (r *AppWrapperReconciler) retryPauseDuration(ctx context.Context, aw *workloadv1beta2.AppWrapper) time.Duration {
	if userPeriod, ok := aw.Annotations[workloadv1beta2.RetryPausePeriodDurationAnnotation]; ok {
		if duration, err := time.ParseDuration(userPeriod); err == nil {
			return r.limitDuration(duration)
		} else {
			log.FromContext(ctx).Error(err, "Malformed retry pause annotation; using default", "annotation", userPeriod)
		}
	}
	return r.limitDuration(r.Config.FaultTolerance.RetryPausePeriod)
}

func (r *AppWrapperReconciler) forcefulDeletionGraceDuration(ctx context.Context, aw *workloadv1beta2.AppWrapper) time.Duration {
	if userPeriod, ok := aw.Annotations[workloadv1beta2.ForcefulDeletionGracePeriodAnnotation]; ok {
		if duration, err := time.ParseDuration(userPeriod); err == nil {
			return r.limitDuration(duration)
		} else {
			log.FromContext(ctx).Error(err, "Malformed forceful deletion period annotation; using default", "annotation", userPeriod)
		}
	}
	return r.limitDuration(r.Config.FaultTolerance.ForcefulDeletionGracePeriod)
}

func (r *AppWrapperReconciler) deletionOnFailureGraceDuration(ctx context.Context, aw *workloadv1beta2.AppWrapper) time.Duration {
	if userPeriod, ok := aw.Annotations[workloadv1beta2.DeletionOnFailureGracePeriodAnnotation]; ok {
		if duration, err := time.ParseDuration(userPeriod); err == nil {
			return r.limitDuration(duration)
		} else {
			log.FromContext(ctx).Error(err, "Malformed deletion on failure grace period annotation; using default of 0", "annotation", userPeriod)
		}
	}
	return 0 * time.Second
}

func (r *AppWrapperReconciler) timeToLiveAfterSucceededDuration(ctx context.Context, aw *workloadv1beta2.AppWrapper) time.Duration {
	if userPeriod, ok := aw.Annotations[workloadv1beta2.SuccessTTLAnnotation]; ok {
		if duration, err := time.ParseDuration(userPeriod); err == nil {
			if duration > 0 && duration < r.Config.FaultTolerance.SuccessTTL {
				return duration
			}
		} else {
			log.FromContext(ctx).Error(err, "Malformed successTTL annotation; using default", "annotation", userPeriod)
		}
	}
	return r.Config.FaultTolerance.SuccessTTL
}

func clearCondition(aw *workloadv1beta2.AppWrapper, condition workloadv1beta2.AppWrapperCondition, reason string, message string) {
	if meta.IsStatusConditionTrue(aw.Status.Conditions, string(condition)) {
		meta.SetStatusCondition(&aw.Status.Conditions, metav1.Condition{
			Type:    string(condition),
			Status:  metav1.ConditionFalse,
			Reason:  reason,
			Message: message,
		})
	}
}

// podMapFunc maps pods to appwrappers
func (r *AppWrapperReconciler) podMapFunc(ctx context.Context, obj client.Object) []reconcile.Request {
	pod := obj.(*v1.Pod)
	if name, ok := pod.Labels[AppWrapperLabel]; ok {
		if pod.Status.Phase == v1.PodSucceeded {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: pod.Namespace, Name: name}}}
		}
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AppWrapperReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&workloadv1beta2.AppWrapper{}).
		Watches(&v1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.podMapFunc)).
		Named("AppWrapper").
		Complete(r)
}
