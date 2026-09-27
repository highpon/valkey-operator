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
	_ "embed"
	"fmt"
	"os/exec"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/valkey-io/valkey-operator/test/utils"
)

// A CSI hostpath driver for the volume expansion specs, which need a driver
// that can expand volumes; kind's default local-path class cannot.
const (
	csiHostpathNamespace = "csi-hostpath"
	csiHostpathDriver    = "hostpath.csi.k8s.io"
	// The driver only provisions on the node carrying this label (see the
	// manifest), so clusters using its volumes must be pinned there too.
	csiHostpathNodeLabel = "valkey.io/e2e-csi-hostpath"

	expandableStorageClass = "csi-hostpath-expandable"
)

// csiHostpathManifest installs the driver and its StorageClass; see the
// file for what was trimmed from upstream and why.
//
//go:embed testdata/csi-hostpath-driver.yaml
var csiHostpathManifest string

// installCSIHostpathDriver labels a worker node for the driver, installs it
// and waits for it to run. Call it from a BeforeAll: it registers
// DeferCleanup steps right after each change, so the driver and the label are
// removed after the container even if a later step or spec fails.
func installCSIHostpathDriver() {
	By("labeling a worker node to host the CSI hostpath driver")
	cmd := exec.Command("kubectl", "get", "nodes", "-l", "!node-role.kubernetes.io/control-plane",
		"-o", "jsonpath={.items[*].metadata.name}")
	output, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to list worker nodes")
	workers := strings.Fields(output)
	Expect(workers).NotTo(BeEmpty(), "no worker node found")
	node := workers[0]
	cmd = exec.Command("kubectl", "label", "node", node, "--overwrite", csiHostpathNodeLabel+"=true")
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to label the driver node")
	DeferCleanup(func() {
		cmd := exec.Command("kubectl", "label", "node", node, csiHostpathNodeLabel+"-")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to remove the driver node label")
	})

	By("installing the CSI hostpath driver")
	cmd = exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(csiHostpathManifest)
	_, err = utils.Run(cmd)
	// Registered even if apply failed half-way, to remove whatever was created.
	DeferCleanup(func() {
		cmd := exec.Command("kubectl", "delete", "-f", "-",
			"--ignore-not-found=true", "--wait=true", "--timeout=120s")
		cmd.Stdin = strings.NewReader(csiHostpathManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to uninstall the CSI hostpath driver")
	})
	// Cleanups run in reverse order, so this wait runs before the uninstall:
	// with the driver gone, its PVs could no longer be deprovisioned and the
	// csi-hostpath namespace would hang. It is a separate step so that the
	// uninstall still runs if a test left volumes behind.
	DeferCleanup(func() {
		// The driver logs explain a stuck deprovisioning, and are gone once
		// the uninstall below runs, so collect them before failing.
		err := InterceptGomegaFailure(func() {
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pv", "-o",
					fmt.Sprintf("jsonpath={range .items[?(@.spec.csi.driver=='%s')]}{.metadata.name}{\"\\n\"}{end}", csiHostpathDriver))
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(utils.GetNonEmptyLines(output)).To(BeEmpty())
			}).Should(Succeed())
		})
		if err != nil {
			dumpCSIHostpathDriverLogs()
			Fail("CSI hostpath volumes were not released: " + err.Error())
		}
	})
	Expect(err).NotTo(HaveOccurred(), "Failed to install the CSI hostpath driver")

	cmd = exec.Command("kubectl", "rollout", "status", "statefulset/csi-hostpathplugin",
		"-n", csiHostpathNamespace, "--timeout=5m")
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "CSI hostpath driver did not become ready")
}

func dumpCSIHostpathDriverLogs() {
	cmd := exec.Command("kubectl", "logs", "-n", csiHostpathNamespace, "statefulset/csi-hostpathplugin",
		"--all-containers", "--tail=100")
	output, _ := utils.Run(cmd)
	_, _ = fmt.Fprintf(GinkgoWriter, "CSI hostpath driver logs:\n%s\n", output)
}
