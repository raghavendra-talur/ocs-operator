package storagecluster

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/openshift/ocs-operator/controllers/defaults"
	utils "github.com/openshift/ocs-operator/controllers/util"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ocsv1 "github.com/openshift/ocs-operator/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (r *StorageClusterReconciler) getStorageClusterEligibleNodes(sc *ocsv1.StorageCluster) (nodes *corev1.NodeList, err error) {
	nodes = &corev1.NodeList{}
	var selector labels.Selector

	labelSelector := &metav1.LabelSelector{
		MatchLabels: map[string]string{defaults.NodeAffinityKey: ""},
	}
	if sc.Spec.LabelSelector != nil {
		labelSelector = sc.Spec.LabelSelector
	}

	selector, err = metav1.LabelSelectorAsSelector(labelSelector)
	if err != nil {
		return nodes, err
	}
	err = r.Client.List(context.TODO(), nodes, MatchingLabelsSelector{Selector: selector})

	return nodes, err
}

type failureDomain struct {
	Type   string
	Key    string
	Values []string
}

// determineFailureDomain determines the appropriate Ceph failure domain type,
// key and values based on the storage cluster's topology map
func (r *StorageClusterReconciler) determineFailureDomain(sc *ocsv1.StorageCluster) (failureDomain failureDomain, err error) {

	minNodes := getMinimumNodes(sc)

	nodes, err := r.getStorageClusterEligibleNodes(sc)
	if err != nil {
		return failureDomain, fmt.Errorf("Failed to get storage cluster eligible nodes: %v", err)
	}

	if sc.Status.NodeTopologies == nil || sc.Status.NodeTopologies.Labels == nil {
		err := r.reconcileNodeTopologyMap(sc, minNodes, nodes)
		if err != nil {
			return failureDomain, fmt.Errorf("Failed to reconcile node topology: %v", err)
		}
	}

	filterDeprecatedLabels(sc.Status.NodeTopologies)

	if sc.Status.FailureDomain != "" {
		failureDomain.Type = sc.Status.FailureDomain
		failureDomain.Key, failureDomain.Values = sc.Status.NodeTopologies.GetKeyValues(failureDomain.Type)
		return failureDomain, nil
	}

	if sc.Spec.FlexibleScaling {
		failureDomain.Type = "host"
		failureDomain.Key, failureDomain.Values = sc.Status.NodeTopologies.GetKeyValues(failureDomain.Type)
		return failureDomain, nil
	}

	for label, labelValues := range sc.Status.NodeTopologies.Labels {
		if strings.Contains(label, "zone") {
			if (len(labelValues) >= 2 && arbiterEnabled(sc)) || (len(labelValues) >= 3) {
				failureDomain.Type = "zone"
				failureDomain.Key, failureDomain.Values = sc.Status.NodeTopologies.GetKeyValues(failureDomain.Type)
				return failureDomain, nil
			}
		}
	}

	// Default to rack failure domain if no other failure domain available
	err = r.ensureNodeRacks(nodes, minNodes, sc.Status.NodeTopologies)
	if err != nil {
		return failureDomain, fmt.Errorf("Unable to assign rack labels: %v", err)
	}
	failureDomain.Type = "rack"
	failureDomain.Key, failureDomain.Values = sc.Status.NodeTopologies.GetKeyValues(failureDomain.Type)
	return failureDomain, nil
}

// determinePlacementRack sorts the list of known racks in alphabetical order,
// counts the number of Nodes in each rack, then returns the first rack with
// the fewest number of Nodes. If there are fewer than three racks, define new
// racks so that there are at least three. It also ensures that only racks with
// either no nodes or nodes in the same AZ are considered valid racks.
func determinePlacementRack(
	nodes *corev1.NodeList, node corev1.Node,
	minRacks int, nodeRacks *ocsv1.NodeTopologyMap) string {

	rackList := []string{}

	if len(nodeRacks.Labels) < minRacks {
		for i := len(nodeRacks.Labels); i < minRacks; i++ {
			for j := 0; j <= i; j++ {
				newRack := fmt.Sprintf("rack%d", j)
				if _, ok := nodeRacks.Labels[newRack]; !ok {
					nodeRacks.Labels[newRack] = ocsv1.TopologyLabelValues{}
					break
				}
			}
		}
	}

	targetAZ := ""
	for label, value := range node.Labels {
		for _, key := range validTopologyLabelKeys {
			if strings.Contains(label, key) && strings.Contains(label, "zone") {
				targetAZ = value
				break
			}
		}
		if targetAZ != "" {
			break
		}
	}

	if len(targetAZ) > 0 {
		for rack := range nodeRacks.Labels {
			nodeNames := nodeRacks.Labels[rack]
			if len(nodeNames) == 0 {
				rackList = append(rackList, rack)
				continue
			}

			validRack := false
			for _, nodeName := range nodeNames {
				for _, n := range nodes.Items {
					if n.Name == nodeName {
						for label, value := range n.Labels {
							for _, key := range validTopologyLabelKeys {
								if strings.Contains(label, key) && strings.Contains(label, "zone") && value == targetAZ {
									validRack = true
									break
								}
							}
							if validRack {
								break
							}
						}
						break
					}
				}
				if validRack {
					break
				}
			}
			if validRack {
				rackList = append(rackList, rack)
			}
		}
	} else {
		for rack := range nodeRacks.Labels {
			rackList = append(rackList, rack)
		}
	}

	sort.Strings(rackList)
	rack := rackList[0]

	for _, r := range rackList {
		if len(nodeRacks.Labels[r]) < len(nodeRacks.Labels[rack]) {
			rack = r
		}
	}

	return rack
}

func generateStrategicPatch(oldObj, newObj interface{}) (client.Patch, error) {
	oldJSON, err := json.Marshal(oldObj)
	if err != nil {
		return nil, err
	}

	newJSON, err := json.Marshal(newObj)
	if err != nil {
		return nil, err
	}

	patch, err := strategicpatch.CreateTwoWayMergePatch(oldJSON, newJSON, oldObj)
	if err != nil {
		return nil, err
	}

	return client.RawPatch(types.StrategicMergePatchType, patch), nil
}

// ensureNodeRacks iterates through the list of storage nodes and ensures
// all nodes have a rack topology label.
func (r *StorageClusterReconciler) ensureNodeRacks(
	nodes *corev1.NodeList, minRacks int,
	topologyMap *ocsv1.NodeTopologyMap) error {

	nodeRacks := ocsv1.NewNodeTopologyMap()

	for _, node := range nodes.Items {
		labels := node.Labels
		for label, value := range labels {
			if strings.Contains(label, "rack") {
				if !nodeRacks.Contains(value, node.Name) {
					nodeRacks.Add(value, node.Name)
				}
			}
		}

	}

	for _, node := range nodes.Items {
		hasRack := false

		for _, nodeNames := range nodeRacks.Labels {
			for _, nodeName := range nodeNames {
				if nodeName == node.Name {
					hasRack = true
					break
				}
			}
			if hasRack {
				break
			}
		}

		if !hasRack {
			rack := determinePlacementRack(nodes, node, minRacks, nodeRacks)
			nodeRacks.Add(rack, node.Name)
			if !topologyMap.Contains(defaults.RackTopologyKey, rack) {
				r.Log.Info("Adding rack label from node", "Node", node.Name, "Label", defaults.RackTopologyKey, "Value", rack)
				topologyMap.Add(defaults.RackTopologyKey, rack)
			}

			r.Log.Info("Labeling node with rack label", "Node", node.Name, "Label", defaults.RackTopologyKey, "Value", rack)
			newNode := node.DeepCopy()
			newNode.Labels[defaults.RackTopologyKey] = rack
			patch, err := generateStrategicPatch(node, newNode)
			if err != nil {
				return err
			}
			err = r.Client.Patch(context.TODO(), &node, patch)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// reconcileNodeTopologyMap builds the map of all topology labels on all nodes
// in the storage cluster
func (r *StorageClusterReconciler) reconcileNodeTopologyMap(sc *ocsv1.StorageCluster, minNodes int, nodes *corev1.NodeList) error {

	if sc.Status.NodeTopologies == nil || sc.Status.NodeTopologies.Labels == nil {
		sc.Status.NodeTopologies = ocsv1.NewNodeTopologyMap()
	}
	topologyMap := sc.Status.NodeTopologies

	r.nodeCount = len(nodes.Items)

	if r.nodeCount < minNodes {
		return fmt.Errorf("Not enough nodes found: Expected %d, found %d", minNodes, r.nodeCount)
	}

	for _, node := range nodes.Items {
		labels := node.Labels
		for label, value := range labels {
			for _, key := range validTopologyLabelKeys {
				if strings.Contains(label, key) {
					if !topologyMap.Contains(label, value) {
						r.Log.Info("Adding topology label from node", "Node", node.Name, "Label", label, "Value", value)
						topologyMap.Add(label, value)
					}
				}
			}

		}

	}

	return nil
}

// filterDeprecatedLabels will remove the old labels from the TopologyMap if the list of values completely match with the list of values of the new label.
func filterDeprecatedLabels(topologyMap *ocsv1.NodeTopologyMap) {

	sort.Strings(topologyMap.Labels["failure-domain.beta.kubernetes.io/zone"])
	sort.Strings(topologyMap.Labels["failure-domain.kubernetes.io/zone"])
	sort.Strings(topologyMap.Labels["topology.kubernetes.io/zone"])
	sort.Strings(topologyMap.Labels["failure-domain.beta.kubernetes.io/region"])
	sort.Strings(topologyMap.Labels["failure-domain.kubernetes.io/region"])
	sort.Strings(topologyMap.Labels["topology.kubernetes.io/region"])

	if utils.CompareStringSlices(topologyMap.Labels["failure-domain.beta.kubernetes.io/zone"], topologyMap.Labels["topology.kubernetes.io/zone"]) {
		delete(topologyMap.Labels, "failure-domain.beta.kubernetes.io/zone")
	}
	if utils.CompareStringSlices(topologyMap.Labels["failure-domain.beta.kubernetes.io/region"], topologyMap.Labels["topology.kubernetes.io/region"]) {
		delete(topologyMap.Labels, "failure-domain.beta.kubernetes.io/region")
	}

	if utils.CompareStringSlices(topologyMap.Labels["failure-domain.kubernetes.io/zone"], topologyMap.Labels["topology.kubernetes.io/zone"]) {
		delete(topologyMap.Labels, "failure-domain.kubernetes.io/zone")
	}
	if utils.CompareStringSlices(topologyMap.Labels["failure-domain.kubernetes.io/region"], topologyMap.Labels["topology.kubernetes.io/region"]) {
		delete(topologyMap.Labels, "failure-domain.kubernetes.io/region")
	}

}
