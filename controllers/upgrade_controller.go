/*
Copyright 2022 NVIDIA CORPORATION & AFFILIATES

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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	gpuv1 "github.com/NVIDIA/gpu-operator/api/v1"
	"github.com/NVIDIA/k8s-operator-libs/pkg/consts"
	"github.com/NVIDIA/k8s-operator-libs/pkg/upgrade"
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// UpgradeReconciler reconciles Driver Daemon Sets for upgrade
type UpgradeReconciler struct {
	client.Client
	Log          logr.Logger
	Scheme       *runtime.Scheme
	StateManager *upgrade.ClusterUpgradeStateManager
}

const (
	plannedRequeueInterval = time.Minute * 2
	// DriverLabelKey indicates pod label key of the driver
	DriverLabelKey = "app"
	// DriverLabelValue indicates pod label value of the driver
	DriverLabelValue = "nvidia-driver-daemonset"
	// UpgradeSkipDrainLabel indicates label to skip drain
	UpgradeSkipDrainLabel = "xdxct.com/gpu.driver-upgrade-skip-drain"
)

//nolint
// +kubebuilder:rbac:groups=mellanox.com,resources=*,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=list
// +kubebuilder:rbac:groups=apps,resources=deployments;daemonsets;replicasets;statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *UpgradeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reqLogger := r.Log.WithValues("upgrade", req.NamespacedName)
	reqLogger.V(consts.LogLevelInfo).Info("Reconciling Upgrade")

	// Fetch the ClusterPolicy instance
	clusterPolicy := &gpuv1.ClusterPolicy{}
	err := r.Client.Get(context.TODO(), req.NamespacedName, clusterPolicy)
	if err != nil {
		reqLogger.V(consts.LogLevelError).Error(err, "Error getting ClusterPolicy object")
		if clusterPolicyCtrl.operatorMetrics != nil {
			clusterPolicyCtrl.operatorMetrics.reconciliationStatus.Set(reconciliationStatusClusterPolicyUnavailable)
		}
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	if clusterPolicy.Spec.Driver.UpgradePolicy == nil ||
		!clusterPolicy.Spec.Driver.UpgradePolicy.AutoUpgrade {
		reqLogger.V(consts.LogLevelInfo).Info("Advanced driver upgrade policy is disabled, cleaning up upgrade state and skipping reconciliation")
		// disable driver upgrade metrics
		if clusterPolicyCtrl.operatorMetrics != nil {
			clusterPolicyCtrl.operatorMetrics.driverAutoUpgradeEnabled.Set(driverAutoUpgradeDisabled)
		}
		return ctrl.Result{}, r.removeNodeUpgradeStateLabels(ctx)
	}
	// enable driver upgrade metrics
	if clusterPolicyCtrl.operatorMetrics != nil {
		clusterPolicyCtrl.operatorMetrics.driverAutoUpgradeEnabled.Set(driverAutoUpgradeEnabled)
	}

	driverLabelKey := DriverLabelKey
	driverLabelValue := DriverLabelValue
	state, err := r.StateManager.BuildState(ctx, clusterPolicyCtrl.operatorNamespace, map[string]string{driverLabelKey: driverLabelValue})
	if err != nil {
		r.Log.Error(err, "Failed to build cluster upgrade state")
		return ctrl.Result{}, err
	}

	reqLogger.Info("Propagate state to state manager")
	reqLogger.V(consts.LogLevelDebug).Info("Current cluster upgrade state", "state", state)

	totalNodes := r.StateManager.GetTotalManagedNodes(ctx, state)
	maxUnavailable := totalNodes
	if clusterPolicy.Spec.Driver.UpgradePolicy != nil && clusterPolicy.Spec.Driver.UpgradePolicy.MaxUnavailable != nil {
		maxUnavailable, err = intstr.GetScaledValueFromIntOrPercent(clusterPolicy.Spec.Driver.UpgradePolicy.MaxUnavailable, totalNodes, true)
		if err != nil {
			r.Log.Error(err, "Failed to compute maxUnavailable from the current total nodes")
			return ctrl.Result{}, err
		}
	}

	// log metrics with the current state
	if clusterPolicyCtrl.operatorMetrics != nil {
		clusterPolicyCtrl.operatorMetrics.upgradesInProgress.Set(float64(r.StateManager.GetUpgradesInProgress(ctx, state)))
		clusterPolicyCtrl.operatorMetrics.upgradesDone.Set(float64(r.StateManager.GetUpgradesDone(ctx, state)))
		clusterPolicyCtrl.operatorMetrics.upgradesAvailable.Set(float64(r.StateManager.GetUpgradesAvailable(ctx, state, clusterPolicy.Spec.Driver.UpgradePolicy.MaxParallelUpgrades, maxUnavailable)))
		clusterPolicyCtrl.operatorMetrics.upgradesFailed.Set(float64(r.StateManager.GetUpgradesFailed(ctx, state)))
		clusterPolicyCtrl.operatorMetrics.upgradesPending.Set(float64(r.StateManager.GetUpgradesPending(ctx, state)))
	}

	err = r.StateManager.ApplyState(ctx, state, clusterPolicy.Spec.Driver.UpgradePolicy)
	if err != nil {
		r.Log.Error(err, "Failed to apply cluster upgrade state")
		return ctrl.Result{}, err
	}

	// In some cases if node state changes fail to apply, upgrade process
	// might become stuck until the new reconcile loop is scheduled.
	// Since node/ds/clusterpolicy updates from outside of the upgrade flow
	// are not guaranteed, for safety reconcile loop should be requeued every few minutes.
	return ctrl.Result{Requeue: true, RequeueAfter: plannedRequeueInterval}, nil
}

// removeNodeUpgradeStateLabels loops over nodes in the cluster and removes "xdxct.com/gpu-driver-upgrade-state"
// It is used for cleanup when autoUpgrade feature gets disabled
func (r *UpgradeReconciler) removeNodeUpgradeStateLabels(ctx context.Context) error {
	r.Log.Info("Resetting node upgrade labels from all nodes")

	nodeList := &corev1.NodeList{}
	err := r.List(ctx, nodeList)
	if err != nil {
		r.Log.Error(err, "Failed to get node list to reset upgrade labels")
		return err
	}

	upgradeStateLabel := upgrade.GetUpgradeStateLabelKey()

	for i := range nodeList.Items {
		node := &nodeList.Items[i]
		_, present := node.Labels[upgradeStateLabel]
		if present {
			delete(node.Labels, upgradeStateLabel)
			err = r.Update(ctx, node)
			if err != nil {
				r.Log.V(consts.LogLevelError).Error(
					err, "Failed to reset upgrade state label from node", "node", node)
				return err
			}
		}
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
//
//nolint:dupl
func (r *UpgradeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Create a new controller
	c, err := controller.New("upgrade-controller", mgr, controller.Options{Reconciler: r, MaxConcurrentReconciles: 1, RateLimiter: workqueue.NewItemExponentialFailureRateLimiter(minDelayCR, maxDelayCR)})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource ClusterPolicy
	err = c.Watch(source.Kind(mgr.GetCache(), &gpuv1.ClusterPolicy{}), &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// Define a mapping from the Node object in the event to one or more
	// ClusterPolicy objects to Reconcile
	mapFn := func(ctx context.Context, a client.Object) []reconcile.Request {
		// find all the ClusterPolicy to trigger their reconciliation
		opts := []client.ListOption{} // Namespace = "" to list across all namespaces.
		list := &gpuv1.ClusterPolicyList{}

		err := mgr.GetClient().List(ctx, list, opts...)
		if err != nil {
			r.Log.Error(err, "Unable to list ClusterPolicies")
			return []reconcile.Request{}
		}

		cpToRec := []reconcile.Request{}

		for _, cp := range list.Items {
			cpToRec = append(cpToRec, reconcile.Request{NamespacedName: types.NamespacedName{
				Name:      cp.ObjectMeta.GetName(),
				Namespace: cp.ObjectMeta.GetNamespace(),
			}})
		}

		return cpToRec
	}

	// Watch for changes to node labels
	// TODO: only watch for changes to upgrade state label
	err = c.Watch(
		source.Kind(mgr.GetCache(), &corev1.Node{}),
		handler.EnqueueRequestsFromMapFunc(mapFn),
		predicate.LabelChangedPredicate{},
	)
	if err != nil {
		return err
	}

	// Watch for changes to Daemonsets and requeue the owner ClusterPolicy.
	// TODO: only watch for changes to driver Daemonset
	err = c.Watch(
		source.Kind(mgr.GetCache(), &appsv1.DaemonSet{}),
		handler.EnqueueRequestForOwner(mgr.GetScheme(), mgr.GetRESTMapper(), &gpuv1.ClusterPolicy{}, handler.OnlyControllerOwner()),
	)
	if err != nil {
		return err
	}

	return nil
}
