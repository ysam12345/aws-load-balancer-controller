/*


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
	"fmt"
	"time"

	discv1 "k8s.io/api/discovery/v1"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/aws-load-balancer-controller/controllers/elbv2/eventhandlers"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/config"
	errmetrics "sigs.k8s.io/aws-load-balancer-controller/pkg/error"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/k8s"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/runtime"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/targetgroupbinding"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	"github.com/go-logr/logr"
	lbcmetrics "sigs.k8s.io/aws-load-balancer-controller/pkg/metrics/lbc"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	elbv2api "sigs.k8s.io/aws-load-balancer-controller/apis/elbv2/v1beta1"
	metricsutil "sigs.k8s.io/aws-load-balancer-controller/pkg/metrics/util"
)

const (
	targetGroupBindingFinalizer = "elbv2.k8s.aws/resources"
	controllerName              = "targetGroupBinding"
)

// NewTargetGroupBindingReconciler constructs new targetGroupBindingReconciler
func NewTargetGroupBindingReconciler(k8sClient client.Client, eventRecorder record.EventRecorder, finalizerManager k8s.FinalizerManager,
	tgbResourceManager targetgroupbinding.ResourceManager, config config.ControllerConfig, deferredTargetGroupBindingReconciler DeferredTargetGroupBindingReconciler,
	logger logr.Logger, metricsCollector lbcmetrics.MetricCollector, reconcileCounters *metricsutil.ReconcileCounters) *targetGroupBindingReconciler {

	return &targetGroupBindingReconciler{
		k8sClient:                            k8sClient,
		eventRecorder:                        eventRecorder,
		finalizerManager:                     finalizerManager,
		tgbResourceManager:                   tgbResourceManager,
		deferredTargetGroupBindingReconciler: deferredTargetGroupBindingReconciler,
		logger:                               logger,
		metricsCollector:                     metricsCollector,
		reconcileCounters:                    reconcileCounters,

		maxConcurrentReconciles:    config.TargetGroupBindingMaxConcurrentReconciles,
		maxExponentialBackoffDelay: config.TargetGroupBindingMaxExponentialBackoffDelay,
		enableEndpointSlices:       config.EnableEndpointSlices,
	}
}

// targetGroupBindingReconciler reconciles a TargetGroupBinding object
type targetGroupBindingReconciler struct {
	k8sClient                            client.Client
	eventRecorder                        record.EventRecorder
	finalizerManager                     k8s.FinalizerManager
	tgbResourceManager                   targetgroupbinding.ResourceManager
	deferredTargetGroupBindingReconciler DeferredTargetGroupBindingReconciler
	logger                               logr.Logger
	metricsCollector                     lbcmetrics.MetricCollector
	reconcileCounters                    *metricsutil.ReconcileCounters

	maxConcurrentReconciles    int
	maxExponentialBackoffDelay time.Duration
	enableEndpointSlices       bool
}

// +kubebuilder:rbac:groups=elbv2.k8s.aws,resources=targetgroupbindings,verbs=get;list;watch;update;patch;create;delete
// +kubebuilder:rbac:groups=elbv2.k8s.aws,resources=targetgroupbindings/status,verbs=update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/status,verbs=update;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=endpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;delete;create;update
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="discovery.k8s.io",resources=endpointslices,verbs=get;list;watch

func (r *targetGroupBindingReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	r.reconcileCounters.IncrementTGB(req.NamespacedName)
	r.logger.V(1).Info("Reconcile request", "name", req.Name)
	return runtime.HandleReconcileError(r.reconcile(ctx, req), r.logger)
}

func (r *targetGroupBindingReconciler) reconcile(ctx context.Context, req reconcile.Request) error {
	tgb := &elbv2api.TargetGroupBinding{}
	var err error
	fetchTargetGroupBindingFn := func() {
		err = r.k8sClient.Get(ctx, req.NamespacedName, tgb)
	}
	r.metricsCollector.ObserveControllerReconcileLatency(controllerName, "fetch_targetGroupBinding", fetchTargetGroupBindingFn)
	if err != nil {
		return client.IgnoreNotFound(err)
	}

	if !tgb.DeletionTimestamp.IsZero() {
		return r.cleanupTargetGroupBinding(ctx, tgb)
	}
	return r.reconcileTargetGroupBinding(ctx, tgb)
}

func (r *targetGroupBindingReconciler) reconcileTargetGroupBinding(ctx context.Context, tgb *elbv2api.TargetGroupBinding) error {
	var err error
	finalizerFn := func() {
		err = r.finalizerManager.AddFinalizers(ctx, tgb, targetGroupBindingFinalizer)
	}
	r.metricsCollector.ObserveControllerReconcileLatency(controllerName, "add_finalizers", finalizerFn)
	if err != nil {
		r.eventRecorder.Event(tgb, corev1.EventTypeWarning, k8s.TargetGroupBindingEventReasonFailedAddFinalizer, fmt.Sprintf("Failed add finalizer due to %v", err))
		return errmetrics.NewErrorWithMetrics(controllerName, "add_finalizers_error", err, r.metricsCollector)
	}

	var deferred bool
	tgbResourceFn := func() {
		deferred, err = r.tgbResourceManager.Reconcile(ctx, tgb)
	}
	r.metricsCollector.ObserveControllerReconcileLatency(controllerName, "reconcile_targetgroupbinding", tgbResourceFn)
	if err != nil {
		return errmetrics.NewErrorWithMetrics(controllerName, "reconcile_targetgroupbinding_error", err, r.metricsCollector)
	}

	if deferred {
		r.deferredTargetGroupBindingReconciler.Enqueue(tgb)
		return nil
	}

	updateTargetGroupBindingStatusFn := func() {
		err = r.updateTargetGroupBindingStatus(ctx, tgb)
	}
	r.metricsCollector.ObserveControllerReconcileLatency(controllerName, "update_status", updateTargetGroupBindingStatusFn)
	if err != nil {
		return errmetrics.NewErrorWithMetrics(controllerName, "update_status_error", err, r.metricsCollector)
	}

	r.eventRecorder.Event(tgb, corev1.EventTypeNormal, k8s.TargetGroupBindingEventReasonSuccessfullyReconciled, "Successfully reconciled")
	return nil
}

func (r *targetGroupBindingReconciler) cleanupTargetGroupBinding(ctx context.Context, tgb *elbv2api.TargetGroupBinding) error {
	if k8s.HasFinalizer(tgb, targetGroupBindingFinalizer) {
		if err := r.tgbResourceManager.Cleanup(ctx, tgb); err != nil {
			r.eventRecorder.Event(tgb, corev1.EventTypeWarning, k8s.TargetGroupBindingEventReasonFailedCleanup, fmt.Sprintf("Failed cleanup due to %v", err))
			return err
		}
		if err := r.finalizerManager.RemoveFinalizers(ctx, tgb, targetGroupBindingFinalizer); err != nil {
			r.eventRecorder.Event(tgb, corev1.EventTypeWarning, k8s.TargetGroupBindingEventReasonFailedRemoveFinalizer, fmt.Sprintf("Failed remove finalizer due to %v", err))
			return err
		}
	}
	return nil
}

func (r *targetGroupBindingReconciler) updateTargetGroupBindingStatus(ctx context.Context, tgb *elbv2api.TargetGroupBinding) error {
	if aws.ToInt64(tgb.Status.ObservedGeneration) == tgb.Generation {
		return nil
	}

	tgbOld := tgb.DeepCopy()

	tgb.Status.ObservedGeneration = aws.Int64(tgb.Generation)
	if err := r.k8sClient.Status().Patch(ctx, tgb, client.MergeFrom(tgbOld)); err != nil {
		return errors.Wrapf(err, "failed to update targetGroupBinding status: %v", k8s.NamespacedName(tgb))
	}

	return nil
}

func (r *targetGroupBindingReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	if err := r.setupIndexes(ctx, mgr.GetFieldIndexer()); err != nil {
		return err
	}

	svcEventHandler := eventhandlers.NewEnqueueRequestsForServiceEvent(r.k8sClient,
		r.logger.WithName("eventHandlers").WithName("service"))
	nodeEventsHandler := eventhandlers.NewEnqueueRequestsForNodeEvent(r.k8sClient,
		r.logger.WithName("eventHandlers").WithName("node"))

	var eventHandler handler.EventHandler
	var clientObj client.Object

	if r.enableEndpointSlices {
		clientObj = &discv1.EndpointSlice{}
		eventHandler = eventhandlers.NewEnqueueRequestsForEndpointSlicesEvent(r.k8sClient,
			r.logger.WithName("eventHandlers").WithName("endpointslices"))
	} else {
		clientObj = &corev1.Endpoints{}
		eventHandler = eventhandlers.NewEnqueueRequestsForEndpointsEvent(r.k8sClient,
			r.logger.WithName("eventHandlers").WithName("endpoints"))
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&elbv2api.TargetGroupBinding{}).
		Named(controllerName).
		Watches(&corev1.Service{}, svcEventHandler).
		Watches(clientObj, eventHandler).
		Watches(&corev1.Node{}, nodeEventsHandler).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: r.maxConcurrentReconciles,
			RateLimiter:             workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](5*time.Millisecond, r.maxExponentialBackoffDelay)}).
		Complete(r)
}

func (r *targetGroupBindingReconciler) setupIndexes(ctx context.Context, fieldIndexer client.FieldIndexer) error {
	if err := fieldIndexer.IndexField(ctx, &elbv2api.TargetGroupBinding{},
		targetgroupbinding.IndexKeyServiceRefName, targetgroupbinding.IndexFuncServiceRefName); err != nil {
		return err
	}
	return nil
}
