/*
Copyright 2023 Red Hat, Inc.

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
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/klog/v2"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/controllers/external"
	"sigs.k8s.io/cluster-api/controllers/remote"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/collections"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/go-logr/logr"
	openshiftclusterv1 "github.com/openshift/cluster-api-provider-openshift/api/cluster/v1alpha1"
)

const (
	openshiftControlPlaneFinalizer     = "openshiftcontrolplane.cluster.openshift.io/finalizer"
	openshiftBootstrapMachineLabelName = "cluster.openshift.io/bootstrap"
	controlPlaneManagerName            = "openshift-control-plane-controller"
	requeueAfter                       = 30 * time.Second
)

// OpenShiftControlPlaneReconciler reconciles a OpenShiftControlPlane object.
type OpenShiftControlPlaneReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Tracker is used to access the remote cluster and verify that the cluster bootstrap has completed.
	Tracker *remote.ClusterCacheTracker
}

// SetupWithManager sets up the controller with the Manager.
func (r *OpenShiftControlPlaneReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := ctrl.NewControllerManagedBy(mgr).
		For(&openshiftclusterv1.OpenShiftControlPlane{}).
		Owns(&clusterv1.Machine{}).
		Complete(r); err != nil {
		return fmt.Errorf("failed to create controller: %w", err)
	}

	if r.Tracker == nil {
		cacheLogger := log.Log.WithName("openshift-control-plane-controller").WithName("remote-cache")
		tracker, err := remote.NewClusterCacheTracker(mgr, remote.ClusterCacheTrackerOptions{
			Log:            &cacheLogger,
			ControllerName: "remote-cache",
		})
		if err != nil {
			return fmt.Errorf("failed to create remote cluster cache tracker: %w", err)
		}
		r.Tracker = tracker
	}

	return nil
}

//+kubebuilder:rbac:groups=cluster.openshift.io,resources=openshiftcontrolplanes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cluster.openshift.io,resources=openshiftcontrolplanes/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=cluster.openshift.io,resources=openshiftcontrolplanes/finalizers,verbs=update

// Reconcile reconciles a OpenShiftControlPlane object.
func (r *OpenShiftControlPlaneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, rerr error) {
	logger := log.FromContext(ctx)

	// Fetch the OpenShiftControlPlane instance.
	ocp := &openshiftclusterv1.OpenShiftControlPlane{}
	if err := r.Client.Get(ctx, req.NamespacedName, ocp); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Fetch the Cluster.
	cluster, err := util.GetOwnerCluster(ctx, r.Client, ocp.ObjectMeta)
	if err != nil {
		logger.Error(err, "Failed to retrieve owner Cluster from the API Server")
		return ctrl.Result{}, fmt.Errorf("failed to retrieve owner Cluster from the API Server: %w", err)
	}
	if cluster == nil {
		logger.Info("Cluster Controller has not yet set OwnerRef")
		return ctrl.Result{}, nil
	}

	logger = logger.WithValues("Cluster", klog.KObj(cluster))
	ctx = ctrl.LoggerInto(ctx, logger)

	if annotations.IsPaused(cluster, ocp) {
		logger.Info("Reconciliation is paused for this object")
		return ctrl.Result{}, nil
	}

	// Initialize the patch helper.
	patchHelper, err := patch.NewHelper(ocp, r.Client)
	if err != nil {
		logger.Error(err, "Failed to configure the patch helper")
		return ctrl.Result{Requeue: true}, nil
	}

	// Attempt to Patch the OpenShiftControlPlane object and status after each reconciliation if no error occurs.
	defer func() {
		// always update the readyCondition; the summary is represented using the "1 of x completed" notation.
		conditions.SetSummary(ocp,
			conditions.WithConditions(
			// TODO: add conditions
			),
		)

		// Patch ObservedGeneration only if the reconciliation completed successfully
		patchOpts := []patch.Option{}
		if rerr == nil {
			patchOpts = append(patchOpts, patch.WithStatusObservedGeneration{})
		}

		if err := patchHelper.Patch(ctx, ocp, patchOpts...); err != nil {
			rerr = kerrors.NewAggregate([]error{rerr, fmt.Errorf("failed to patch %s: %w", klog.KObj(ocp), err)})
		}
	}()

	if err := reconcileExternalTemplateReference(ctx, r.Client, cluster, ocp.Spec.MachineTemplate.InfrastructureRef); err != nil {
		logger.Error(err, "Failed to reconcile external template reference")
		return ctrl.Result{}, fmt.Errorf("failed to reconcile external template reference: %w", err)
	}

	res, err := r.reconcile(ctx, logger, cluster, ocp)
	if err != nil {
		logger.Error(err, "Failed to reconcile")
		return ctrl.Result{}, fmt.Errorf("failed to reconcile: %w", err)
	}

	return res, nil
}

// reconcile handles OpenShiftControlPlane reconciliation.
func (r *OpenShiftControlPlaneReconciler) reconcile(ctx context.Context, logger logr.Logger, cluster *clusterv1.Cluster, ocp *openshiftclusterv1.OpenShiftControlPlane) (ctrl.Result, error) {
	if !cluster.Status.InfrastructureReady {
		logger.Info("Cluster infrastructure is not ready, waiting")
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	controlPlaneMachines, err := collections.GetFilteredMachinesForCluster(ctx, r.Client, cluster, collections.ControlPlaneMachines(cluster.Name))
	if err != nil {
		logger.Error(err, "Failed to retrieve control plane machines for cluster")
		return ctrl.Result{}, fmt.Errorf("failed to retrieve control plane machines for cluster: %w", err)
	}

	if ocp.Status.Initialized {
		return r.reconcileControlPlaneReady(ctx, logger, cluster, ocp, controlPlaneMachines)
	} else {
		return r.reconcileInitControlPlane(ctx, logger, cluster, ocp, controlPlaneMachines)
	}
}

// reconcileControlPlaneReady handles the control plane ready phase of the OpenShiftControlPlane reconciliation.
func (r OpenShiftControlPlaneReconciler) reconcileControlPlaneReady(ctx context.Context, logger logr.Logger, cluster *clusterv1.Cluster, ocp *openshiftclusterv1.OpenShiftControlPlane, controlPlaneMachines collections.Machines) (ctrl.Result, error) {
	remoteClient, err := r.Tracker.GetClient(ctx, types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace})
	if err != nil {
		// We expect the API to be available by this point so should be able to get a client.
		logger.Error(err, "Failed to get remote cluster client, waiting")
		return ctrl.Result{}, fmt.Errorf("failed to get remote cluster client: %w", err)
	}

	if err := remoteClient.Get(ctx, types.NamespacedName{Name: "bootstrap", Namespace: "kube-system"}, &corev1.ConfigMap{}); err != nil && !apierrors.IsNotFound(err) {
		logger.Error(err, "Failed to get bootstrap config map")
		return ctrl.Result{}, fmt.Errorf("failed to get bootstrap config map: %w", err)
	} else if apierrors.IsNotFound(err) {
		logger.Info("Cluster bootstrap not yet complete, waiting")
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	bootstrapMachines := controlPlaneMachines.Filter(bootstrapMachinesFilter(cluster.Name))
	if bootstrapMachines.Len() > 0 {
		bootstrapMachine := bootstrapMachines.UnsortedList()[0]
		if bootstrapMachine.DeletionTimestamp == nil {
			logger.Info("Deleting bootstrap machine")
			if err := r.Client.Delete(ctx, bootstrapMachine); err != nil {
				logger.Error(err, "Failed to delete bootstrap machine")
				return ctrl.Result{}, fmt.Errorf("failed to delete bootstrap machine: %w", err)
			}
		}
	}

	logger.Info("Control plane is ready")
	ocp.Status.Ready = true

	return ctrl.Result{}, nil
}

// reconcileInitControlPlane handles the initialisation control plane phase of the OpenShiftControlPlane reconciliation.
func (r OpenShiftControlPlaneReconciler) reconcileInitControlPlane(ctx context.Context, logger logr.Logger, cluster *clusterv1.Cluster, ocp *openshiftclusterv1.OpenShiftControlPlane, controlPlaneMachines collections.Machines) (ctrl.Result, error) {
	bootstrapMachines := controlPlaneMachines.Filter(bootstrapMachinesFilter(cluster.Name))

	switch {
	case controlPlaneMachines.Len() == 0:
		// No bootstrap or control plane machines exist, create the bootstrap machine.
		return r.reconcileInitBootstrapMachine(ctx, logger, cluster, ocp)
	case bootstrapMachines.Len() == 1 && controlPlaneMachines.Len() < 4:
		return r.reconcileInitControlPlaneMachines(ctx, logger, cluster, ocp, bootstrapMachines)
	case bootstrapMachines.Len() == 0 && controlPlaneMachines.Len() > 0:
		// Somehow a control plane machine exists, but the bootstrap machine has not been created.
		// Something has gone wrong with the cluster bootstrap.
		logger.Error(nil, "No bootstrap machine exists for cluster, aborting")
		// Don't return an error here. This needs manual intervention.
		return ctrl.Result{}, nil
	case bootstrapMachines.Len() > 1:
		logger.Error(nil, "More than one bootstrap machine exists for cluster, aborting")
		// Don't return an error here. This needs manual intervention.
		return ctrl.Result{}, nil
	default:
		return r.reconcileWaitForControlPlaneInitialized(logger, ocp, controlPlaneMachines)
	}
}

// reconcileInitBootstrapMachine handles the initialisation of the bootstrap machine phase of the OpenShiftControlPlane reconciliation.
// This should be called after the cluster infrastructure is initialised but before any other machines are created.
func (r OpenShiftControlPlaneReconciler) reconcileInitBootstrapMachine(ctx context.Context, logger logr.Logger, cluster *clusterv1.Cluster, ocp *openshiftclusterv1.OpenShiftControlPlane) (ctrl.Result, error) {
	bootstrapLabels := map[string]string{
		openshiftBootstrapMachineLabelName: "",
	}

	logger.Info("Creating bootstrap machine")
	if err := createControlPlaneMachine(ctx, r.Client, cluster, ocp, "bootstrap", bootstrapLabels, ocp.Spec.BootstrapMachineTemplate.InfrastructureRef); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create bootstrap machine: %w", err)
	}

	return ctrl.Result{}, nil
}

// reconcileInitControlPlaneMachines handles the initialisation of the control plane machines phase of the OpenShiftControlPlane reconciliation.
func (r OpenShiftControlPlaneReconciler) reconcileInitControlPlaneMachines(ctx context.Context, logger logr.Logger, cluster *clusterv1.Cluster, ocp *openshiftclusterv1.OpenShiftControlPlane, bootstrapMachines collections.Machines) (ctrl.Result, error) {
	if !bootstrapMachines.UnsortedList()[0].Status.InfrastructureReady {
		logger.Info("Bootstrap machine infrastructure is not ready, waiting")
		return ctrl.Result{}, nil
	}

	_, err := r.Tracker.GetClient(ctx, types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace})
	if err != nil {
		logger.Error(nil, "Failed to get remote cluster client, bootstrap API is not ready, waiting")
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	logger.Info("Creating control plane machines")
	for i := 0; i < 3; i++ {
		// OpenShift expets the machines to be called master-0, master-1 and master-2.
		name := fmt.Sprintf("master-%d", i)
		if err := createControlPlaneMachine(ctx, r.Client, cluster, ocp, name, nil, ocp.Spec.MachineTemplate.InfrastructureRef); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create control-plane machine %q: %w", name, err)
		}
	}

	return ctrl.Result{}, nil
}

// reconcileWaitForControlPlaneInitialized waits for the first Machine to have a node in the workload cluster.
// Once this happens, we can assume that the remaining control plane machines will follow suit.
// Note the bootstrap node will never get a node so we need not to exclude it while iterating.
func (r OpenShiftControlPlaneReconciler) reconcileWaitForControlPlaneInitialized(logger logr.Logger, ocp *openshiftclusterv1.OpenShiftControlPlane, controlPlaneMachines collections.Machines) (ctrl.Result, error) {
	for _, machine := range controlPlaneMachines.UnsortedList() {
		if machine.Status.NodeRef != nil {
			logger.Info("First control plane machine has a node, marking control plane as initialized")
			ocp.Status.Initialized = true
			return ctrl.Result{}, nil
		}
	}

	logger.Info("Waiting for first control plane machine to have a node")
	return ctrl.Result{}, nil
}

// bootstrapMachinesFilter returns a filter function that matches bootstrap machines for a given cluster.
func bootstrapMachinesFilter(clusterName string) func(machine *clusterv1.Machine) bool {
	selector := bootstrapSelectorForCluster(clusterName)
	return func(machine *clusterv1.Machine) bool {
		if machine == nil {
			return false
		}
		return selector.Matches(labels.Set(machine.Labels))
	}
}

// bootstrapSelectorForCluster returns the label selector necessary to get control plane machines for a given cluster.
func bootstrapSelectorForCluster(clusterName string) labels.Selector {
	must := func(r *labels.Requirement, err error) labels.Requirement {
		if err != nil {
			panic(err)
		}
		return *r
	}
	return labels.NewSelector().Add(
		must(labels.NewRequirement(clusterv1.ClusterNameLabel, selection.Equals, []string{clusterName})),
		must(labels.NewRequirement(clusterv1.MachineControlPlaneLabel, selection.Exists, []string{})),
		must(labels.NewRequirement(openshiftBootstrapMachineLabelName, selection.Exists, []string{})),
	)
}

// reconcileExternalTemplateReference ensures that the external template reference is owned by the cluster.
// We use this for the MachineTemplate within the OpenShiftControlPlane spec.
// This is copied from the Cluster API MachineSet controller and tweaked slightly to fit our use case.
func reconcileExternalTemplateReference(ctx context.Context, c client.Client, cluster *clusterv1.Cluster, ref openshiftclusterv1.InfrastructureReference) error {
	if !strings.HasSuffix(ref.Kind, clusterv1.TemplateSuffix) {
		return nil
	}

	// CAPI should not be doing this!
	// if err := utilconversion.UpdateReferenceAPIContract(ctx, c, ref); err != nil {
	// 	return err
	// }

	// CAPI uses a util for this but it uses the wrong object reference type
	obj, err := getUnstructuredFor(ctx, c, cluster, ref)
	if err != nil {
		return err
	}

	patchHelper, err := patch.NewHelper(obj, c)
	if err != nil {
		return fmt.Errorf("failed to create patch helper: %w", err)
	}

	obj.SetOwnerReferences(util.EnsureOwnerRef(obj.GetOwnerReferences(), metav1.OwnerReference{
		APIVersion: clusterv1.GroupVersion.String(),
		Kind:       "Cluster",
		Name:       cluster.Name,
		UID:        cluster.UID,
	}))

	if err := patchHelper.Patch(ctx, obj); err != nil {
		return fmt.Errorf("failed to patch %s %s/%s: %w", ref.Kind, cluster.Namespace, ref.Name, err)
	}

	return nil
}

func getUnstructuredFor(ctx context.Context, c client.Client, cluster *clusterv1.Cluster, ref openshiftclusterv1.InfrastructureReference) (*unstructured.Unstructured, error) {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion(ref.APIVersion)
	obj.SetKind(ref.Kind)

	if err := c.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: ref.Name}, obj); err != nil {
		return nil, fmt.Errorf("failed to retrieve %s %s/%s: %w", ref.Kind, cluster.Namespace, ref.Name, err)
	}

	return obj, nil
}

func createControlPlaneMachine(ctx context.Context, c client.Client, cluster *clusterv1.Cluster, ocp *openshiftclusterv1.OpenShiftControlPlane, name string, additonalLabels map[string]string, infrastructureRef openshiftclusterv1.InfrastructureReference) error {
	bootstrapReference, err := getOrCreateFromTemplate(ctx, c, cluster, getBootstrapConfigReference(cluster, name), &corev1.ObjectReference{}, getUnstructuredBootstrapConfigTemplate())
	if err != nil {
		return fmt.Errorf("failed to get or create bootstrap config reference: %w", err)
	}

	infraStructureTemplate, err := getUnstructuredFor(ctx, c, cluster, infrastructureRef)
	if err != nil {
		return fmt.Errorf("failed to get infrastructure template: %w", err)
	}

	infrastructureReference := &corev1.ObjectReference{
		APIVersion: infraStructureTemplate.GetAPIVersion(),
		Kind:       strings.TrimSuffix(infraStructureTemplate.GetKind(), clusterv1.TemplateSuffix),
		Name:       clusterMachineName(cluster, name),
		Namespace:  cluster.Namespace,
	}

	infrastructureReference, err = getOrCreateFromTemplate(ctx, c, cluster, infrastructureReference, objectRefFrom(ocp.Spec.MachineTemplate.InfrastructureRef), infraStructureTemplate)
	if err != nil {
		return fmt.Errorf("failed to get or create infrastructure reference: %w", err)
	}

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterMachineName(cluster, name),
			Namespace: cluster.Namespace,
		},
	}

	existingMachine := &clusterv1.Machine{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: machine.Namespace, Name: machine.Name}, existingMachine); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get machine: %w", err)
	}

	// If we managed to fetch a machine, the UID should now be set.
	// If no machine exists, it will be empty.
	if existingMachine.GetUID() != "" {
		machine = existingMachine
	}

	machine.Spec.ClusterName = cluster.Name
	machine.Spec.InfrastructureRef = *infrastructureReference
	machine.Spec.Bootstrap.ConfigRef = bootstrapReference
	machine.Spec.NodeDrainTimeout = ocp.Spec.MachineTemplate.NodeDeletionTimeout.DeepCopy()
	machine.Spec.NodeVolumeDetachTimeout = ocp.Spec.MachineTemplate.NodeDeletionTimeout.DeepCopy()
	machine.Spec.NodeDeletionTimeout = ocp.Spec.MachineTemplate.NodeDeletionTimeout.DeepCopy()

	if machine.Labels == nil {
		machine.Labels = map[string]string{}
	}

	for k, v := range ocp.Spec.MachineTemplate.ObjectMeta.Labels {
		machine.Labels[k] = v
	}
	for k, v := range ocp.Spec.MachineTemplate.ObjectMeta.Annotations {
		machine.Annotations[k] = v
	}

	for k, v := range additonalLabels {
		machine.Labels[k] = v
	}

	machine.Labels[clusterv1.MachineControlPlaneLabel] = ""
	machine.Labels[clusterv1.MachineControlPlaneNameLabel] = ocp.Name

	machine.SetOwnerReferences(util.EnsureOwnerRef(machine.GetOwnerReferences(), *metav1.NewControllerRef(ocp, openshiftclusterv1.GroupVersion.WithKind("OpenShiftControlPlane"))))

	if machine.GetUID() != "" {
		if err := c.Update(ctx, machine); err != nil {
			return fmt.Errorf("failed to update machine: %w", err)
		}
	} else {
		if err := c.Create(ctx, machine); err != nil {
			return fmt.Errorf("failed to create machine: %w", err)
		}
	}

	return nil
}

func getOrCreateFromTemplate(ctx context.Context, c client.Client, cluster *clusterv1.Cluster, objectRef, templateRef *corev1.ObjectReference, template *unstructured.Unstructured) (*corev1.ObjectReference, error) {
	obj, err := external.Get(ctx, c, objectRef, cluster.Namespace)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("failed to get %s %s/%s: %w", objectRef.Kind, cluster.Namespace, objectRef.Name, err)
	}
	if obj != nil {
		return external.GetObjectReference(obj), nil
	}

	// The object doesn't exist, so we need to create it.
	templateInput := &external.GenerateTemplateInput{
		Template:    template,
		TemplateRef: templateRef,
		Namespace:   cluster.Namespace,
		ClusterName: cluster.Name,
	}

	obj, err = external.GenerateTemplate(templateInput)
	if err != nil {
		return nil, fmt.Errorf("failed to generate %s %s/%s from template: %w", objectRef.Kind, cluster.Namespace, objectRef.Name, err)
	}

	// The name for control plane machine and related obejcts are specified rather than random.
	obj.SetName(objectRef.Name)

	if err := c.Create(ctx, obj); err != nil {
		return nil, fmt.Errorf("failed to create %s %s/%s: %w", objectRef.Kind, cluster.Namespace, objectRef.Name, err)
	}

	return external.GetObjectReference(obj), nil
}

func getBootstrapConfigReference(cluster *clusterv1.Cluster, machineName string) *corev1.ObjectReference {
	return &corev1.ObjectReference{
		APIVersion: openshiftclusterv1.GroupVersion.String(),
		Kind:       "OpenShiftBootstrapConfig",
		Name:       clusterMachineName(cluster, machineName),
		Namespace:  cluster.Namespace,
	}
}

func getUnstructuredBootstrapConfigTemplate() *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion(openshiftclusterv1.GroupVersion.String())
	obj.SetKind("OpenShiftBootstrapConfig")

	if err := unstructured.SetNestedField(obj.Object, map[string]interface{}{}, "spec", "template"); err != nil {
		panic(err)
	}

	return obj
}

func objectRefFrom(ref openshiftclusterv1.InfrastructureReference) *corev1.ObjectReference {
	return &corev1.ObjectReference{
		APIVersion: ref.APIVersion,
		Kind:       ref.Kind,
		Name:       ref.Name,
		Namespace:  ref.Namespace,
	}
}

func clusterMachineName(cluster *clusterv1.Cluster, name string) string {
	return fmt.Sprintf("%s-%s", cluster.Name, name)
}
