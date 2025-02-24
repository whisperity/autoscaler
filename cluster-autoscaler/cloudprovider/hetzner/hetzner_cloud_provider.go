/*
Copyright 2019 The Kubernetes Authors.

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

package hetzner

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	"k8s.io/autoscaler/cluster-autoscaler/config"
	"k8s.io/autoscaler/cluster-autoscaler/utils/errors"
	"k8s.io/autoscaler/cluster-autoscaler/utils/gpu"
	"k8s.io/klog/v2"
)

var _ cloudprovider.CloudProvider = (*HetznerCloudProvider)(nil)

const (
	// GPULabel is the label added to nodes with GPU resource.
	GPULabel                   = hcloudLabelNamespace + "/gpu-node"
	providerIDPrefix           = "hcloud://"
	nodeGroupLabel             = hcloudLabelNamespace + "/node-group"
	hcloudLabelNamespace       = "hcloud"
	serverCreateTimeoutDefault = 5 * time.Minute
	serverRegisterTimeout      = 10 * time.Minute
	defaultPodAmountsLimit     = 110
)

// HetznerCloudProvider implements CloudProvider interface.
type HetznerCloudProvider struct {
	manager         *hetznerManager
	resourceLimiter *cloudprovider.ResourceLimiter
}

// Name returns name of the cloud provider.
func (d *HetznerCloudProvider) Name() string {
	return cloudprovider.HetznerProviderName
}

// NodeGroups returns all node groups configured for this cloud provider.
func (d *HetznerCloudProvider) NodeGroups() []cloudprovider.NodeGroup {
	groups := make([]cloudprovider.NodeGroup, 0, len(d.manager.nodeGroups))
	for groupId := range d.manager.nodeGroups {
		groups = append(groups, d.manager.nodeGroups[groupId])
	}
	return groups
}

// NodeGroupForNode returns the node group for the given node, nil if the node
// should not be processed by cluster autoscaler, or non-nil error if such
// occurred. Must be implemented.
func (d *HetznerCloudProvider) NodeGroupForNode(node *apiv1.Node) (cloudprovider.NodeGroup, error) {
	server, err := d.manager.serverForNode(node)
	if err != nil {
		return nil, fmt.Errorf("failed to check if server %s exists error: %v", node.Spec.ProviderID, err)
	}

	var groupId string
	if server == nil {
		klog.V(3).Infof("failed to find hcloud server for node %s", node.Name)
		nodeGroupId, exists := node.Labels[nodeGroupLabel]
		if !exists {
			return nil, nil
		}
		groupId = nodeGroupId
	} else {
		serverGroupId, exists := server.Labels[nodeGroupLabel]
		groupId = serverGroupId
		if !exists {
			return nil, nil
		}
	}

	group, exists := d.manager.nodeGroups[groupId]
	if !exists {
		return nil, nil
	}

	return group, nil
}

// HasInstance returns whether a given node has a corresponding instance in this cloud provider
func (d *HetznerCloudProvider) HasInstance(node *apiv1.Node) (bool, error) {
	return true, cloudprovider.ErrNotImplemented
}

// Pricing returns pricing model for this cloud provider or error if not
// available. Implementation optional.
func (d *HetznerCloudProvider) Pricing() (cloudprovider.PricingModel, errors.AutoscalerError) {
	return nil, cloudprovider.ErrNotImplemented
}

// GetAvailableMachineTypes get all machine types that can be requested from
// the cloud provider. Implementation optional.
func (d *HetznerCloudProvider) GetAvailableMachineTypes() ([]string, error) {
	serverTypes, err := d.manager.cachedServerType.getAllServerTypes()
	if err != nil {
		return nil, err
	}

	types := make([]string, len(serverTypes))
	for _, server := range serverTypes {
		types = append(types, server.Name)
	}

	return types, nil
}

// NewNodeGroup builds a theoretical node group based on the node definition
// provided. The node group is not automatically created on the cloud provider
// side. The node group is not returned by NodeGroups() until it is created.
// Implementation optional.
func (d *HetznerCloudProvider) NewNodeGroup(
	machineType string,
	labels map[string]string,
	systemLabels map[string]string,
	taints []apiv1.Taint,
	extraResources map[string]resource.Quantity,
) (cloudprovider.NodeGroup, error) {
	return nil, cloudprovider.ErrNotImplemented
}

// GetResourceLimiter returns struct containing limits (max, min) for
// resources (cores, memory etc.).
func (d *HetznerCloudProvider) GetResourceLimiter() (*cloudprovider.ResourceLimiter, error) {
	return d.resourceLimiter, nil
}

// GPULabel returns the label added to nodes with GPU resource.
func (d *HetznerCloudProvider) GPULabel() string {
	return GPULabel
}

// GetAvailableGPUTypes return all available GPU types cloud provider supports.
func (d *HetznerCloudProvider) GetAvailableGPUTypes() map[string]struct{} {
	return nil
}

// GetNodeGpuConfig returns the label, type and resource name for the GPU added to node. If node doesn't have
// any GPUs, it returns nil.
func (d *HetznerCloudProvider) GetNodeGpuConfig(node *apiv1.Node) *cloudprovider.GpuConfig {
	return gpu.GetNodeGPUFromCloudProvider(d, node)
}

// Cleanup cleans up open resources before the cloud provider is destroyed,
// i.e. go routines etc.
func (d *HetznerCloudProvider) Cleanup() error {
	return nil
}

// Refresh is called before every main loop and can be used to dynamically
// update cloud provider state. In particular the list of node groups returned
// by NodeGroups() can change as a result of CloudProvider.Refresh().
func (d *HetznerCloudProvider) Refresh() error {
	for _, group := range d.manager.nodeGroups {
		group.resetTargetSize(0)
	}
	return nil
}

// Check if any defined placement groups could potentially have more than the maximum allowed number of nodes
func getLargePlacementGroups(nodeGroups map[string]*hetznerNodeGroup, threshold int) []int64 {
	placementGroupTotals := make(map[int64]int)

	// Calculate totals for each placement group
	for _, nodeGroup := range nodeGroups {
		if nodeGroup.placementGroup == nil || nodeGroup.placementGroup.ID == 0 {
			continue
		}

		placementGroupID := nodeGroup.placementGroup.ID
		placementGroupTotals[placementGroupID] += nodeGroup.maxSize
	}

	// Collect placement groups with total maxSize > threshold
	var largePlacementGroupIDs []int64
	for id, totalMaxSize := range placementGroupTotals {
		if totalMaxSize > threshold {
			largePlacementGroupIDs = append(largePlacementGroupIDs, id)
		}
	}

	return largePlacementGroupIDs
}

// BuildHetzner builds the Hetzner cloud provider.
func BuildHetzner(_ config.AutoscalingOptions, do cloudprovider.NodeGroupDiscoveryOptions, rl *cloudprovider.ResourceLimiter) cloudprovider.CloudProvider {
	manager, err := newManager()
	if err != nil {
		klog.Fatalf("Failed to create Hetzner manager: %v", err)
	}

	provider, err := newHetznerCloudProvider(manager, rl)
	if err != nil {
		klog.Fatalf("Failed to create Hetzner cloud provider: %v", err)
	}

	if manager.clusterConfig.IsUsingNewFormat && len(manager.clusterConfig.NodeConfigs) == 0 {
		klog.Fatalf("No cluster config present provider: %v", err)
	}

	validNodePoolName := regexp.MustCompile(`^[a-z0-9A-Z]+[a-z0-9A-Z\-\.\_]*[a-z0-9A-Z]+$|^[a-z0-9A-Z]{1}$`)
	clusterUpdateLock := sync.Mutex{}
	for _, nodegroupSpec := range do.NodeGroupSpecs {
		spec, err := createNodePoolSpec(nodegroupSpec)
		if err != nil {
			klog.Fatalf("Failed to parse pool spec `%s` provider: %v", nodegroupSpec, err)
		}

		validNodePoolName.MatchString(spec.name)
		servers, err := manager.allServers(spec.name)
		if err != nil {
			klog.Fatalf("Failed to get servers for for node pool %s error: %v", nodegroupSpec, err)
		}

		if manager.clusterConfig.IsUsingNewFormat {
			_, ok := manager.clusterConfig.NodeConfigs[spec.name]
			if !ok {
				klog.Fatalf("No node config present for node group id `%s` error: %v", spec.name, err)
			}
		}

		manager.nodeGroups[spec.name] = &hetznerNodeGroup{
			manager:            manager,
			id:                 spec.name,
			minSize:            spec.minSize,
			maxSize:            spec.maxSize,
			instanceType:       strings.ToLower(spec.instanceType),
			region:             strings.ToLower(spec.region),
			targetSize:         len(servers),
			clusterUpdateMutex: &clusterUpdateLock,
		}

		// If a placement group was specified, check with the API to see if it exists
		if manager.clusterConfig.IsUsingNewFormat {

			placementGroupRef := manager.clusterConfig.NodeConfigs[spec.name].PlacementGroup

			if placementGroupRef == "" {
				continue
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			placementGroup, _, err := manager.client.PlacementGroup.Get(ctx, placementGroupRef)

			// Check if an error occurred
			if err != nil {
				if err == context.DeadlineExceeded {
					klog.Fatalf("Timed out checking if placement group `%s` exists.", placementGroupRef)
				} else {
					klog.Fatalf("Failed to verify if placement group `%s` exists error: %v", placementGroupRef, err)
				}
			}

			// If the placement group exists, add it to the node group config
			if placementGroup != nil {
				manager.nodeGroups[spec.name].placementGroup = placementGroup
			} else {
				klog.Fatalf("The requested placement group `%s` does not appear to exist.", placementGroupRef)
			}
		}
	}

	// Get placement groups with total maxSize over the maximum allowed
	maxPlacementGroupSize := 10

	largePlacementGroups := getLargePlacementGroups(manager.nodeGroups, maxPlacementGroupSize)

	// Fail if we have placement groups over the max size
	if len(largePlacementGroups) > 0 {

		// Gather placement group names
		var placementGroupIDs string
		for i, placementGroupID := range largePlacementGroups {
			if i > 0 {
				placementGroupIDs += ", "
			}
			placementGroupIDs += strconv.FormatInt(placementGroupID, 10)
		}

		klog.Fatalf("The following placement groups have a potential size over the allowed maximum of %d: %s.", maxPlacementGroupSize, placementGroupIDs)
	}

	return provider
}

func createNodePoolSpec(groupSpec string) (*hetznerNodeGroupSpec, error) {
	tokens := strings.SplitN(groupSpec, ":", 5)
	if len(tokens) != 5 {
		return nil, fmt.Errorf("expected format `<min-servers>:<max-servers>:<machine-type>:<region>:<name>` got %s", groupSpec)
	}

	definition := hetznerNodeGroupSpec{
		instanceType: tokens[2],
		region:       tokens[3],
		name:         tokens[4],
	}
	if size, err := strconv.Atoi(tokens[0]); err == nil {
		definition.minSize = size
	} else {
		return nil, fmt.Errorf("failed to set min size: %s, expected integer", tokens[0])
	}

	if size, err := strconv.Atoi(tokens[1]); err == nil {
		definition.maxSize = size
	} else {
		return nil, fmt.Errorf("failed to set max size: %s, expected integer", tokens[1])
	}

	return &definition, nil
}

func newHetznerCloudProvider(manager *hetznerManager, rl *cloudprovider.ResourceLimiter) (*HetznerCloudProvider, error) {
	return &HetznerCloudProvider{
		manager:         manager,
		resourceLimiter: rl,
	}, nil
}
