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
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
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
	Recorder record.EventRecorder

	BackupRegistry name.Registry
	PodNamespace   string
}

//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// SetupWithManager sets up the controller with the Manager.
func (c *ImageCloneController) SetupWithManager(mgr ctrl.Manager) error {
	if c.PodNamespace != "" {
		// ignore the namespace that this controller is running in
		ignoredNamespaces.Insert(c.PodNamespace)
	}

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
		c.Recorder.Event(deployment, corev1.EventTypeWarning, "FailedCopyingImages", err.Error())
		return ctrl.Result{}, err
	}

	// update deployment if reconciliation changed any images
	if !apiequality.Semantic.DeepEqual(before, deployment) {
		// use optimistic locking for patching the deployment, we should retry with exponential backoff if new containers or
		// images were added in the meantime
		log.Info("Patching images in Deployment")
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
		c.Recorder.Event(daemonSet, corev1.EventTypeWarning, "FailedCopyingImages", err.Error())
		return ctrl.Result{}, err
	}

	// update daemonSet if reconciliation changed any images
	if !apiequality.Semantic.DeepEqual(before, daemonSet) {
		// use optimistic locking for patching the daemonSet, we should retry with exponential backoff if new containers or
		// images were added in the meantime
		log.Info("Patching images in DaemonSet")
		return ctrl.Result{}, c.Patch(ctx, daemonSet, client.StrategicMergeFrom(before, client.MergeFromWithOptimisticLock{}))
	}

	return ctrl.Result{}, nil
}

// reconcilePodTemplate copies all images in the given PodTemplate to our backup registry if they don't reference the
// backup registry already. It updates the PodTemplate to reference the copied images.
func (c *ImageCloneController) reconcilePodTemplate(log logr.Logger, template *corev1.PodTemplateSpec) error {
	for i, container := range template.Spec.Containers {
		containerLog := log.WithValues("container", container.Name, "image", container.Image)

		srcImg, err := name.ParseReference(container.Image)
		if err != nil {
			return fmt.Errorf("failed parsing image %q: %w", container.Image, err)
		}

		if srcImg.Context().Registry == c.BackupRegistry {
			containerLog.V(1).Info("Container image is already specifying the backup registry")
			continue
		}

		dstImg, err := toDestinationImage(srcImg, c.BackupRegistry)
		if err != nil {
			return fmt.Errorf("failed rewriting image %q: %w", srcImg.Name(), err)
		}

		containerLog = containerLog.WithValues("destination", dstImg.Name())
		containerLog.Info("Copying image to the backup registry")

		if err := crane.Copy(srcImg.Name(), dstImg.Name()); err != nil {
			return fmt.Errorf("error copying image %q to %q: %w", srcImg.Name(), dstImg.Name(), err)
		}

		containerLog.Info("Finished copying image")
		template.Spec.Containers[i].Image = dstImg.Name()
	}

	return nil
}

// registryReplacer replaces . and : with _ in registry names to be used as a prefix in rewritten repository names.
var registryReplacer = strings.NewReplacer(".", "_", ":", "_")

// toDestinationImage rewrites source image references to corresponding tags in our backup registry, e.g.:
// nginx                                        -> <dstRegistry>/index_docker_io/library/nginx:latest
// nginx:1.23                                   -> <dstRegistry>/index_docker_io/library/nginx:1.23
// nginx@sha256:33cef...                        -> <dstRegistry>/index_docker_io/library/nginx:sha256_33cef...
// grafana/grafana:main                         -> <dstRegistry>/index_docker_io/grafana/grafana:main
// ghcr.io/timebertt/speedtest-exporter:v0.1.0  -> <dstRegistry>/ghcr_io/timebertt/speedtest-exporter:v0.1.0
func toDestinationImage(srcImg name.Reference, dstRegistry name.Registry) (name.Tag, error) {
	var (
		newRepository = registryReplacer.Replace(srcImg.Context().Registry.RegistryStr()) + "/" + srcImg.Context().RepositoryStr()
		newTag        = srcImg.Identifier()
	)

	if digest, ok := srcImg.(name.Digest); ok {
		// if image is identified via digest instead of tag, rewrite digest to tag
		// (need to replace the : separator, as it is not a valid tag character)
		newTag = strings.ReplaceAll(digest.DigestStr(), ":", "_")
	}

	return name.NewTag(fmt.Sprintf("%s/%s:%s", dstRegistry.RegistryStr(), newRepository, newTag))
}
