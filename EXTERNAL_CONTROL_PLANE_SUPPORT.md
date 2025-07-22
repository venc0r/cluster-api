# External Control Plane Support for CAPD

This document describes the implementation of external control plane support in Cluster API Provider Docker (CAPD), specifically for integration with Kamaji as an external control plane provider.

## Overview

By default, CAPD creates and manages a HAProxy load balancer to provide a control plane endpoint for clusters using KubeadmControlPlane. However, when using external control plane providers like Kamaji, this load balancer is not needed as the external provider manages its own control plane endpoint.

## Implementation

The implementation adds support for external control plane providers by:

1. **Detection**: Detecting when a cluster uses an external control plane provider (e.g., `KamajiControlPlane`)
2. **Endpoint Retrieval**: Retrieving the control plane endpoint from the external provider
3. **Cluster Update**: Updating both the DockerCluster and Cluster resources with the external endpoint
4. **Load Balancer Skip**: Skipping HAProxy load balancer creation for external control planes

## Changes Made

### File Modified
- `test/infrastructure/docker/internal/controllers/backends/docker/dockercluster_backend.go`

### Key Functions Added

#### `shouldUseExternalControlPlane(cluster *clusterv1.Cluster) bool`
Checks if the cluster uses an external control plane provider by examining the `ControlPlaneRef.Kind` field.

#### `reconcileExternalControlPlane(ctx context.Context, cluster *clusterv1.Cluster, dockerCluster *infrav1.DevCluster) (ctrl.Result, error)`
Main function that handles external control plane reconciliation. Uses the external package helper to retrieve the control plane object with proper contract versioning.

#### `reconcileKamajiControlPlane(ctx context.Context, cluster *clusterv1.Cluster, dockerCluster *infrav1.DevCluster, kamajiControlPlane *unstructured.Unstructured) (ctrl.Result, error)`
Specific implementation for KamajiControlPlane integration that:
- Retrieves the associated TenantControlPlane resource
- Extracts the control plane endpoint from `status.kubeadmConfig.clusterConfiguration.controlPlaneEndpoint`
- Updates both DockerCluster and Cluster resources with the endpoint
- Marks the DockerCluster as ready without creating a load balancer

### Integration Flow

1. **ReconcileNormal** checks if the cluster uses an external control plane
2. If external control plane is detected, calls `reconcileExternalControlPlane`
3. For KamajiControlPlane specifically, calls `reconcileKamajiControlPlane`
4. The Kamaji handler retrieves the TenantControlPlane and extracts the endpoint
5. Both DockerCluster and Cluster resources are updated with the external endpoint
6. Load balancer creation is skipped, and the DockerCluster is marked as ready

### Kamaji Integration Details

The implementation specifically targets Kamaji's TenantControlPlane resource structure:
```yaml
status:
  kubeadmConfig:
    clusterConfiguration:
      controlPlaneEndpoint:
        host: "kamaji-control-plane-host"
        port: 6443
```

## Benefits

1. **Clean Separation**: CAPD handles infrastructure while Kamaji manages the control plane
2. **No Conflicts**: Avoids conflicting patches between CAPD and Kamaji on cluster resources
3. **Proper Endpoint Publishing**: Ensures worker nodes get the correct control plane endpoint for joining
4. **Extensible**: Framework can be extended to support other external control plane providers

## Usage

To use this feature:

1. Create a Cluster with `ControlPlaneRef.Kind` set to `"KamajiControlPlane"`
2. Ensure the KamajiControlPlane and associated TenantControlPlane resources exist
3. CAPD will automatically detect the external control plane and configure endpoints accordingly

## Testing

The implementation:
- ✅ Compiles successfully with `make manager-docker-infrastructure`
- ✅ Passes all existing tests with `make test-docker-infrastructure`
- Maintains backward compatibility with existing KubeadmControlPlane clusters

## Future Enhancements

- Support for additional external control plane providers
- Enhanced error handling and status reporting
- Unit tests specific to external control plane scenarios
- E2E testing with actual Kamaji deployments
