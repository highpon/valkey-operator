//go:build e2e
// +build e2e

/*
Copyright 2025 Valkey Contributors.

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

package e2e

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/test/utils"
)

// Helpers for the volume expansion specs. They all assume the topology that
// createExpansionTestCluster creates: one shard with one replica, so two pods,
// two ValkeyNodes and two PVCs.
const persistentClusterPods = 2

// createExpansionTestCluster creates a one shard, one replica cluster with
// initialVolumeSize volumes from the expandable StorageClass on the CSI
// hostpath driver's node.
func createExpansionTestCluster(name string) {
	// Leftovers of an interrupted run could have PVCs that already have the
	// size the specs are waiting for.
	deleteExpansionTestCluster(name)

	manifest := fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 1
  replicas: 1
  persistence:
    size: %s
    storageClassName: %s
  config:
    appendonly: "yes"
  scheduling:
    nodeSelector:
      %s: "true"
`, name, initialVolumeSize, expandableStorageClass, csiHostpathNodeLabel)
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to create the ValkeyCluster CR")

	By("waiting for the cluster to reach Ready")
	Eventually(func(g Gomega) { verifySingleShardClusterReady(g, name) }, 5*time.Minute).Should(Succeed())
}

// deleteExpansionTestCluster deletes the cluster, if any, and waits for its
// PVCs to go.
func deleteExpansionTestCluster(name string) {
	cmd := exec.Command("kubectl", "delete", "valkeycluster", name,
		"--ignore-not-found=true", "--wait=true", "--timeout=120s")
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to delete the ValkeyCluster")
	cmd = exec.Command("kubectl", "delete", "pvc", "-l", "valkey.io/cluster="+name,
		"--ignore-not-found=true", "--wait=true", "--timeout=120s")
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to delete the cluster's PVCs")
}

func verifySingleShardClusterReady(g Gomega, name string) {
	cr, err := utils.GetValkeyClusterStatus(name)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
	g.Expect(cr.Status.ReadyShards).To(Equal(int32(1)))
}

func patchPersistenceSize(name, size string) error {
	cmd := exec.Command("kubectl", "patch", "valkeycluster", name, "--type=merge",
		"-p", fmt.Sprintf(`{"spec":{"persistence":{"size":%q}}}`, size))
	_, err := utils.Run(cmd)
	return err
}

// managedVolume is a cluster's PVC together with its bound PV's capacity.
type managedVolume struct {
	PVC        string
	Phase      string
	Request    string
	Capacity   string
	PV         string
	PVCapacity string
}

// listManagedVolumes returns the cluster's PVCs, one per pod.
func listManagedVolumes(g Gomega, clusterName string) []managedVolume {
	cmd := exec.Command("kubectl", "get", "pvc", "-l", "valkey.io/cluster="+clusterName,
		"-o", "jsonpath={range .items[*]}{.metadata.name}:{.status.phase}:{.spec.resources.requests.storage}:{.status.capacity.storage}:{.spec.volumeName}{\"\\n\"}{end}")
	output, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred())
	lines := utils.GetNonEmptyLines(output)
	g.Expect(lines).To(HaveLen(persistentClusterPods))
	volumes := make([]managedVolume, 0, len(lines))
	for _, line := range lines {
		fields := strings.Split(line, ":")
		g.Expect(fields).To(HaveLen(5), "unexpected PVC line %q", line)
		v := managedVolume{PVC: fields[0], Phase: fields[1], Request: fields[2], Capacity: fields[3], PV: fields[4]}
		cmd := exec.Command("kubectl", "get", "pv", v.PV, "-o", "jsonpath={.spec.capacity.storage}")
		v.PVCapacity, err = utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		volumes = append(volumes, v)
	}
	return volumes
}

// verifyVolumeSizes asserts that every managed PVC is bound and requests
// size, and that both the PVC and its PV report size as their capacity.
func verifyVolumeSizes(g Gomega, clusterName, size string) {
	for _, v := range listManagedVolumes(g, clusterName) {
		g.Expect(v.Phase).To(Equal("Bound"), "PVC %s", v.PVC)
		g.Expect(v.Request).To(Equal(size), "PVC %s request", v.PVC)
		g.Expect(v.Capacity).To(Equal(size), "PVC %s capacity", v.PVC)
		g.Expect(v.PVCapacity).To(Equal(size), "PV %s capacity", v.PV)
	}
}

// sizeCondition returns the node's PersistentVolumeClaimSizeReady condition
// if it was computed for the node's current generation, nil otherwise.
func sizeCondition(node *valkeyiov1alpha1.ValkeyNode) *metav1.Condition {
	cond := utils.FindCondition(node.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionPersistentVolumeClaimSizeReady)
	if cond == nil || cond.ObservedGeneration != node.Generation {
		return nil
	}
	return cond
}

// verifySizeSatisfied asserts that every ValkeyNode requests size and
// reports PersistentVolumeClaimSizeReady=True for its current generation.
func verifySizeSatisfied(g Gomega, clusterName, size string) {
	nodes, err := utils.GetValkeyClusterNodes(clusterName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(nodes.Items).To(HaveLen(persistentClusterPods))
	for i := range nodes.Items {
		node := &nodes.Items[i]
		g.Expect(node.Spec.Persistence).NotTo(BeNil())
		g.Expect(node.Spec.Persistence.Size.String()).To(Equal(size), "ValkeyNode %s", node.Name)
		cond := sizeCondition(node)
		g.Expect(cond).NotTo(BeNil(), "ValkeyNode %s has no current size condition", node.Name)
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue), "ValkeyNode %s: %s", node.Name, cond.Message)
		g.Expect(cond.Reason).To(Equal(valkeyiov1alpha1.ValkeyNodeReasonPersistentVolumeClaimSizeSatisfied))
	}
}

// podIdentities maps each pod of the cluster to "uid:restartCount" of its
// server container, which changes if the pod is replaced or restarted.
func podIdentities(clusterName string) map[string]string {
	cmd := exec.Command("kubectl", "get", "pod", "-l", "valkey.io/cluster="+clusterName,
		"-o", "jsonpath={range .items[*]}{.metadata.name}={.metadata.uid}:{.status.containerStatuses[?(@.name=='server')].restartCount}{\"\\n\"}{end}")
	output, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
	identities := map[string]string{}
	for _, line := range utils.GetNonEmptyLines(output) {
		name, identity, _ := strings.Cut(line, "=")
		identities[name] = identity
	}
	Expect(identities).To(HaveLen(persistentClusterPods))
	return identities
}

// valkeyCLI runs a shell snippet of valkey-cli calls in the cluster's first
// pod. -c follows redirects, so it works whichever pod is primary.
func valkeyCLI(g Gomega, clusterName, script string) string {
	cmd := exec.Command("kubectl", "exec", fmt.Sprintf("valkey-%s-0-0-0", clusterName), "-c", "server", "--",
		"sh", "-c", "unset VALKEYCLI_AUTH REDISCLI_AUTH; "+script)
	output, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred())
	return output
}

func writeCanary(g Gomega, clusterName, key, value string) {
	g.Expect(valkeyCLI(g, clusterName, fmt.Sprintf("valkey-cli -c set %s %s", key, value))).To(ContainSubstring("OK"))
}

// verifyClusterServes asserts that the cluster is healthy, still holds the
// canary and accepts a new write.
func verifyClusterServes(g Gomega, clusterName, key, value string) {
	output := valkeyCLI(g, clusterName, fmt.Sprintf(
		"valkey-cli cluster info | head -1; valkey-cli -c get %s; valkey-cli -c set %s-after ok", key, key))
	g.Expect(output).To(ContainSubstring("cluster_state:ok"))
	g.Expect(output).To(ContainSubstring(value))
	g.Expect(output).To(ContainSubstring("OK"))
}
