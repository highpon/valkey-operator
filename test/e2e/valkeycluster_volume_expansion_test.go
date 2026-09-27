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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/valkey-io/valkey-operator/test/utils"
)

const (
	initialVolumeSize  = "1Gi"
	expandedVolumeSize = "2Gi"
)

// Covers online expansion of the managed PVCs through a CSI driver that
// supports it: raising spec.persistence.size must grow every PVC and PV
// without restarting the pods, and the ValkeyNodes must report the new size
// as satisfied. hostpath volumes are plain directories, so this proves the
// Kubernetes side of an online expansion (PVC and PV capacity, no pod
// restart), not a filesystem growing inside the pod.
var _ = Describe("ValkeyCluster volume expansion", Ordered, Label("ValkeyCluster", "Persistence", "VolumeExpansion"), func() {
	const clusterName = "cluster-volume-expansion"
	const canaryKey = "expansion-canary"
	const canaryValue = "survives-online-expansion"

	var identitiesBefore map[string]string

	BeforeAll(func() {
		installCSIHostpathDriver()
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.CollectDebugInfo(namespace)
			dumpCSIHostpathDriverLogs()
		}
	})

	It("creates a persistent cluster on an expandable StorageClass", func() {
		createExpansionTestCluster(clusterName)

		By("verifying the PVCs are provisioned by the CSI driver at the initial size")
		Eventually(func(g Gomega) { verifyVolumeSizes(g, clusterName, initialVolumeSize) }).Should(Succeed())
		Eventually(func(g Gomega) { verifySizeSatisfied(g, clusterName, initialVolumeSize) }).Should(Succeed())

		By("writing a canary key")
		Eventually(func(g Gomega) { writeCanary(g, clusterName, canaryKey, canaryValue) }).Should(Succeed())
		identitiesBefore = podIdentities(clusterName)
	})

	It("expands the PVCs online when spec.persistence.size grows", func() {
		By("raising spec.persistence.size")
		Expect(patchPersistenceSize(clusterName, expandedVolumeSize)).To(Succeed(), "Failed to raise spec.persistence.size")

		By("waiting for every PVC and PV to reach the new size")
		Eventually(func(g Gomega) { verifyVolumeSizes(g, clusterName, expandedVolumeSize) }, 5*time.Minute).Should(Succeed())

		By("waiting for every ValkeyNode to report the new size as satisfied")
		Eventually(func(g Gomega) { verifySizeSatisfied(g, clusterName, expandedVolumeSize) }).Should(Succeed())

		// The cluster briefly reports UpdatingNodes while it hands the new
		// size to one ValkeyNode at a time, so it returns to Ready rather
		// than staying there throughout.
		By("verifying the cluster returns to Ready")
		Eventually(func(g Gomega) { verifySingleShardClusterReady(g, clusterName) }).Should(Succeed())

		By("verifying no pod was replaced or restarted by the expansion")
		Expect(podIdentities(clusterName)).To(Equal(identitiesBefore))

		By("verifying the data survived and the cluster still accepts writes")
		Eventually(func(g Gomega) { verifyClusterServes(g, clusterName, canaryKey, canaryValue) }).Should(Succeed())
	})

	// Runs before the driver cleanups registered in BeforeAll, which need the
	// cluster's volumes gone first.
	AfterAll(func() {
		deleteExpansionTestCluster(clusterName)
	})
})
