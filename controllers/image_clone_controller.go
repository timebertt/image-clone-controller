/*
Copyright 2022 Tim Ebert.

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
	"strings"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// ImageCloneControllerName is the name of the image-clone-controller.
const ImageCloneControllerName = "image-clone"

// ImageCloneController reconciles Deployment and DaemonSet objects and copies images to the configured backup registry.
type ImageCloneController struct {
	client.Client
	Scheme *runtime.Scheme

	BackupRegistry string
}

//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;update;patch

// SetupWithManager sets up the controller with the Manager.
func (c *ImageCloneController) SetupWithManager(mgr ctrl.Manager) error {
	if err := ctrl.NewControllerManagedBy(mgr).
		Named(ImageCloneControllerName).
		For(&appsv1.Deployment{}, builder.WithPredicates(predicate.GenerationChangedPredicate{}, namespacePredicate)).
		Complete(reconcile.Func(c.ReconcileDeployment)); err != nil {
		return err
	}
	if err := ctrl.NewControllerManagedBy(mgr).
		Named(ImageCloneControllerName).
		For(&appsv1.DaemonSet{}, builder.WithPredicates(predicate.GenerationChangedPredicate{}, namespacePredicate)).
		Complete(reconcile.Func(c.ReconcileDaemonSet)); err != nil {
		return err
	}
	return nil
}

// RegistryNamespace is the namespace that our local registry is running in.
const RegistryNamespace = "registry"

var ignoredNamespaces = sets.NewString(
	metav1.NamespaceSystem,
	RegistryNamespace,
	"local-path-storage", // kind system component
)

// namespacePredicate ignores objects in system namespaces.
var namespacePredicate = predicate.NewPredicateFuncs(func(obj client.Object) bool {
	return !ignoredNamespaces.Has(obj.GetNamespace())
})

// ReconcileDeployment implements the reconciliation loop for Deployment objects.
func (c *ImageCloneController) ReconcileDeployment(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	deployment := &appsv1.Deployment{}
	if err := c.Get(ctx, req.NamespacedName, deployment); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Object is gone, stop reconciling")
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("error reading object: %w", err)
	}

	before := deployment.DeepCopy()
	if err := c.reconcilePodTemplate(log, &deployment.Spec.Template); err != nil {
		return ctrl.Result{}, err
	}

	// update deployment if reconciliation changed any images
	if !apiequality.Semantic.DeepEqual(before, deployment) {
		// use optimistic locking for patching the deployment, we should retry with exponential backoff if new containers or
		// images where added in the meantime
		return ctrl.Result{}, c.Patch(ctx, deployment, client.StrategicMergeFrom(before, client.MergeFromWithOptimisticLock{}))
	}

	return ctrl.Result{}, nil
}

// ReconcileDaemonSet implements the reconciliation loop for DaemonSet objects.
func (c *ImageCloneController) ReconcileDaemonSet(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	daemonSet := &appsv1.DaemonSet{}
	if err := c.Get(ctx, req.NamespacedName, daemonSet); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Object is gone, stop reconciling")
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("error reading object: %w", err)
	}

	before := daemonSet.DeepCopy()
	if err := c.reconcilePodTemplate(log, &daemonSet.Spec.Template); err != nil {
		return ctrl.Result{}, err
	}

	// update daemonSet if reconciliation changed any images
	if !apiequality.Semantic.DeepEqual(before, daemonSet) {
		// use optimistic locking for patching the daemonSet, we should retry with exponential backoff if new containers or
		// images where added in the meantime
		return ctrl.Result{}, c.Patch(ctx, daemonSet, client.StrategicMergeFrom(before, client.MergeFromWithOptimisticLock{}))
	}

	return ctrl.Result{}, nil
}

func (c *ImageCloneController) reconcilePodTemplate(log logr.Logger, template *corev1.PodTemplateSpec) error {
	for i, container := range template.Spec.Containers {
		containerLog := log.WithValues("container", container.Name, "image", container.Image)

		if strings.HasPrefix(container.Image, fmt.Sprintf("%s/", c.BackupRegistry)) {
			containerLog.V(1).Info("Container image is already specifying the backup registry")
			continue
		}

		containerLog.Info("Copying image to the backup registry")

		// TODO: perform cloning logic and overwrite image
		// template.Spec.Containers[i].Image = ...
		_ = i
	}

	return nil
}
