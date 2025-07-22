/*
Copyright 2024 The Kubernetes Authors.

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

// Package docker implements docker backends for DevClusters and DevMachines.
package docker

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/test/infrastructure/container"
	infrav1 "sigs.k8s.io/cluster-api/test/infrastructure/docker/api/v1beta1"
	"sigs.k8s.io/cluster-api/test/infrastructure/docker/internal/docker"
	"sigs.k8s.io/cluster-api/util/conditions"
	v1beta2conditions "sigs.k8s.io/cluster-api/util/conditions/v1beta2"
	"sigs.k8s.io/cluster-api/util/patch"
)

// ClusterBackEndReconciler reconciles a DockerCluster object.
type ClusterBackEndReconciler struct {
	client.Client
	ContainerRuntime container.Runtime
}

// ReconcileNormal handle docker backend for DevCluster not yet deleted.
func (r *ClusterBackEndReconciler) ReconcileNormal(ctx context.Context, cluster *clusterv1.Cluster, dockerCluster *infrav1.DevCluster) (ctrl.Result, error) {
	if dockerCluster.Spec.Backend.Docker == nil {
		return ctrl.Result{}, errors.New("DockerBackendReconciler can't be called for DevCluster without a Docker backend")
	}

	// Check if this cluster uses an external control plane (e.g., Kamaji)
	isExternalCP, externalEndpoint, err := r.checkExternalControlPlane(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to check for external control plane")
	}

	// If we have an external control plane, we still need to create the load balancer
	// but configure it to point to the external endpoint instead of local control plane nodes
	log := ctrl.LoggerFrom(ctx)
	if isExternalCP {
		log.Info("Using external control plane", "endpoint", externalEndpoint)
	} else {
		log.Info("Using standard kubeadm control plane")
	}

	// Support FailureDomains
	// In cloud providers this would likely look up which failure domains are supported and set the status appropriately.
	// In the case of Docker, failure domains don't mean much so we simply copy the Spec into the Status.
	dockerCluster.Status.FailureDomains = dockerCluster.Spec.Backend.Docker.FailureDomains

	// Create a helper for managing a docker container hosting the loadbalancer.
	externalLoadBalancer, err := docker.NewLoadBalancer(ctx, cluster,
		dockerCluster.Spec.Backend.Docker.LoadBalancer.ImageRepository,
		dockerCluster.Spec.Backend.Docker.LoadBalancer.ImageTag,
		strconv.Itoa(dockerCluster.Spec.ControlPlaneEndpoint.Port))
	if err != nil {
		conditions.MarkFalse(dockerCluster, infrav1.LoadBalancerAvailableCondition, infrav1.LoadBalancerProvisioningFailedReason, clusterv1.ConditionSeverityWarning, err.Error())
		v1beta2conditions.Set(dockerCluster, metav1.Condition{
			Type:    infrav1.DevClusterDockerLoadBalancerAvailableV1Beta2Condition,
			Status:  metav1.ConditionFalse,
			Reason:  infrav1.DevClusterDockerLoadBalancerNotAvailableV1Beta2Reason,
			Message: fmt.Sprintf("Failed to create helper for managing the externalLoadBalancer: %v", err),
		})
		return ctrl.Result{}, errors.Wrapf(err, "failed to create helper for managing the externalLoadBalancer")
	}

	// Create the docker container hosting the load balancer.
	if err := externalLoadBalancer.Create(ctx); err != nil {
		conditions.MarkFalse(dockerCluster, infrav1.LoadBalancerAvailableCondition, infrav1.LoadBalancerProvisioningFailedReason, clusterv1.ConditionSeverityWarning, err.Error())
		v1beta2conditions.Set(dockerCluster, metav1.Condition{
			Type:    infrav1.DevClusterDockerLoadBalancerAvailableV1Beta2Condition,
			Status:  metav1.ConditionFalse,
			Reason:  infrav1.DevClusterDockerLoadBalancerNotAvailableV1Beta2Reason,
			Message: fmt.Sprintf("Failed to create load balancer: %v", err),
		})
		return ctrl.Result{}, errors.Wrap(err, "failed to create load balancer")
	}

	// Set APIEndpoints with the load balancer IP so the Cluster API Cluster Controller can pull it
	lbIP, err := externalLoadBalancer.IP(ctx)
	if err != nil {
		conditions.MarkFalse(dockerCluster, infrav1.LoadBalancerAvailableCondition, infrav1.LoadBalancerProvisioningFailedReason, clusterv1.ConditionSeverityWarning, err.Error())
		v1beta2conditions.Set(dockerCluster, metav1.Condition{
			Type:    infrav1.DevClusterDockerLoadBalancerAvailableV1Beta2Condition,
			Status:  metav1.ConditionFalse,
			Reason:  infrav1.DevClusterDockerLoadBalancerNotAvailableV1Beta2Reason,
			Message: fmt.Sprintf("Failed to get ip for the load balancer: %v", err),
		})
		return ctrl.Result{}, errors.Wrap(err, "failed to get ip for the load balancer")
	}

	if dockerCluster.Spec.ControlPlaneEndpoint.Host == "" {
		// Surface the control plane endpoint
		// Note: the control plane port is already set by the user or defaulted by the dockerCluster webhook.
		dockerCluster.Spec.ControlPlaneEndpoint.Host = lbIP
	}

	// Mark the dockerCluster ready
	dockerCluster.Status.Ready = true
	conditions.MarkTrue(dockerCluster, infrav1.LoadBalancerAvailableCondition)
	v1beta2conditions.Set(dockerCluster, metav1.Condition{
		Type:   infrav1.DevClusterDockerLoadBalancerAvailableV1Beta2Condition,
		Status: metav1.ConditionTrue,
		Reason: infrav1.DevClusterDockerLoadBalancerAvailableV1Beta2Reason,
	})

	return ctrl.Result{}, nil
}

// checkExternalControlPlane checks if the cluster uses an external control plane like Kamaji.
func (r *ClusterBackEndReconciler) checkExternalControlPlane(ctx context.Context, cluster *clusterv1.Cluster) (bool, string, error) {
	log := ctrl.LoggerFrom(ctx)

	// Check if the cluster has a control plane reference
	if cluster.Spec.ControlPlaneRef == nil {
		return false, "", nil
	}

	// Check if it's a Kamaji control plane
	if cluster.Spec.ControlPlaneRef.Kind == "KamajiControlPlane" {
		log.Info("Detected Kamaji external control plane", "controlPlaneRef", cluster.Spec.ControlPlaneRef)

		// Get the KamajiControlPlane object
		kamajiCP := &unstructured.Unstructured{}
		kamajiCP.SetAPIVersion(cluster.Spec.ControlPlaneRef.APIVersion)
		kamajiCP.SetKind(cluster.Spec.ControlPlaneRef.Kind)

		key := client.ObjectKey{
			Namespace: cluster.Spec.ControlPlaneRef.Namespace,
			Name:      cluster.Spec.ControlPlaneRef.Name,
		}

		if err := r.Get(ctx, key, kamajiCP); err != nil {
			return false, "", errors.Wrapf(err, "failed to get KamajiControlPlane %s", key)
		}

		// In Kamaji, the TenantControlPlane has the same name and namespace as the KamajiControlPlane
		tcpName := cluster.Spec.ControlPlaneRef.Name
		tcpNamespace := cluster.Spec.ControlPlaneRef.Namespace
		if tcpNamespace == "" {
			tcpNamespace = cluster.Namespace // default to cluster namespace
		}

		// Get the TenantControlPlane object
		tcp := &unstructured.Unstructured{}
		tcp.SetAPIVersion("kamaji.clastix.io/v1alpha1")
		tcp.SetKind("TenantControlPlane")

		tcpKey := client.ObjectKey{
			Namespace: tcpNamespace,
			Name:      tcpName,
		}

		if err := r.Get(ctx, tcpKey, tcp); err != nil {
			return false, "", errors.Wrapf(err, "failed to get TenantControlPlane %s", tcpKey)
		}

		// Extract the control plane endpoint
		endpoint, found, err := unstructured.NestedString(tcp.Object, "status", "controlPlaneEndpoint")
		if err != nil || !found {
			return false, "", errors.New("failed to find controlPlaneEndpoint in TenantControlPlane status")
		}

		log.Info("Found Kamaji control plane endpoint", "endpoint", endpoint)
		return true, endpoint, nil
	}

	// Check for other external control plane types here if needed

	return false, "", nil
}

// ReconcileDelete handle docker backend for delete DevMachines.
func (r *ClusterBackEndReconciler) ReconcileDelete(ctx context.Context, cluster *clusterv1.Cluster, dockerCluster *infrav1.DevCluster) (ctrl.Result, error) {
	if dockerCluster.Spec.Backend.Docker == nil {
		return ctrl.Result{}, errors.New("DockerBackendReconciler can't be called for DevClusters without a Docker backend")
	}

	// Create a helper for managing a docker container hosting the loadbalancer.
	externalLoadBalancer, err := docker.NewLoadBalancer(ctx, cluster,
		dockerCluster.Spec.Backend.Docker.LoadBalancer.ImageRepository,
		dockerCluster.Spec.Backend.Docker.LoadBalancer.ImageTag,
		strconv.Itoa(dockerCluster.Spec.ControlPlaneEndpoint.Port))
	if err != nil {
		conditions.MarkFalse(dockerCluster, infrav1.LoadBalancerAvailableCondition, infrav1.LoadBalancerProvisioningFailedReason, clusterv1.ConditionSeverityWarning, err.Error())
		v1beta2conditions.Set(dockerCluster, metav1.Condition{
			Type:    infrav1.DevClusterDockerLoadBalancerAvailableV1Beta2Condition,
			Status:  metav1.ConditionFalse,
			Reason:  infrav1.DevClusterDockerLoadBalancerNotAvailableV1Beta2Reason,
			Message: fmt.Sprintf("Failed to create helper for managing the externalLoadBalancer: %v", err),
		})

		return ctrl.Result{}, errors.Wrapf(err, "failed to create helper for managing the externalLoadBalancer")
	}

	// Set the LoadBalancerAvailableCondition reporting delete is started, and requeue in order to make
	// this visible to the users.
	if conditions.GetReason(dockerCluster, infrav1.LoadBalancerAvailableCondition) != clusterv1.DeletingReason {
		conditions.MarkFalse(dockerCluster, infrav1.LoadBalancerAvailableCondition, clusterv1.DeletingReason, clusterv1.ConditionSeverityInfo, "")
		v1beta2conditions.Set(dockerCluster, metav1.Condition{
			Type:   infrav1.DevClusterDockerLoadBalancerAvailableV1Beta2Condition,
			Status: metav1.ConditionFalse,
			Reason: infrav1.DevClusterDockerLoadBalancerDeletingV1Beta2Reason,
		})
		return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
	}

	// Delete the docker container hosting the load balancer
	if err := externalLoadBalancer.Delete(ctx); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to delete load balancer")
	}

	// Cluster is deleted so remove the finalizer.
	controllerutil.RemoveFinalizer(dockerCluster, infrav1.ClusterFinalizer)

	return ctrl.Result{}, nil
}

// PatchDevCluster patch a DevCluster.
func (r *ClusterBackEndReconciler) PatchDevCluster(ctx context.Context, patchHelper *patch.Helper, dockerCluster *infrav1.DevCluster) error {
	if dockerCluster.Spec.Backend.Docker == nil {
		return errors.New("DockerBackendReconciler can't be called for DevClusters without a Docker backend")
	}

	// Always update the readyCondition by summarizing the state of other conditions.
	// A step counter is added to represent progress during the provisioning process (instead we are hiding it during the deletion process).
	conditions.SetSummary(dockerCluster,
		conditions.WithConditions(
			infrav1.LoadBalancerAvailableCondition,
		),
		conditions.WithStepCounterIf(dockerCluster.ObjectMeta.DeletionTimestamp.IsZero()),
	)
	if err := v1beta2conditions.SetSummaryCondition(dockerCluster, dockerCluster, infrav1.DevClusterReadyV1Beta2Condition,
		v1beta2conditions.ForConditionTypes{
			infrav1.DevClusterDockerLoadBalancerAvailableV1Beta2Condition,
		},
		// Using a custom merge strategy to override reasons applied during merge.
		v1beta2conditions.CustomMergeStrategy{
			MergeStrategy: v1beta2conditions.DefaultMergeStrategy(
				// Use custom reasons.
				v1beta2conditions.ComputeReasonFunc(v1beta2conditions.GetDefaultComputeMergeReasonFunc(
					infrav1.DevClusterNotReadyV1Beta2Reason,
					infrav1.DevClusterReadyUnknownV1Beta2Reason,
					infrav1.DevClusterReadyV1Beta2Reason,
				)),
			),
		},
	); err != nil {
		return errors.Wrapf(err, "failed to set %s condition", infrav1.DevClusterReadyV1Beta2Condition)
	}

	// Patch the object, ignoring conflicts on the conditions owned by this controller.
	return patchHelper.Patch(
		ctx,
		dockerCluster,
		patch.WithOwnedConditions{Conditions: []clusterv1.ConditionType{
			clusterv1.ReadyCondition,
			infrav1.LoadBalancerAvailableCondition,
		}},
		patch.WithOwnedV1Beta2Conditions{Conditions: []string{
			clusterv1.PausedV1Beta2Condition,
			infrav1.DevClusterReadyV1Beta2Condition,
			infrav1.DevClusterDockerLoadBalancerAvailableV1Beta2Condition,
		}},
	)
}
