// Copyright (c) 2018 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hybridbotanist

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gardener/gardener/pkg/operation"
	"github.com/gardener/gardener/pkg/operation/common"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
)

var chartPathMachines = filepath.Join(common.ChartPath, "seed-machines", "charts", "machines")

// DeployMachines asks the CloudBotanist to provide the specific configuration for MachineClasses and MachineDeployments.
// It deploys the machine specifications, waits until it is ready and cleans old specifications.
func (b *HybridBotanist) DeployMachines() error {
	machineClassKind, machineClassPlural, machineClassChartName := b.ShootCloudBotanist.GetMachineClassInfo()

	// Generate machine classes configuration and list of corresponding machine deployments.
	machineClassChartValues, machineDeployments, err := b.ShootCloudBotanist.GenerateMachineConfig()
	if err != nil {
		return fmt.Errorf("The CloudBotanist failed to generate the machine config: '%s'", err.Error())
	}

	// Deploy generated machine classes.
	values := map[string]interface{}{
		"machineClasses": machineClassChartValues,
	}
	if err := b.ApplyChartSeed(filepath.Join(common.ChartPath, "seed-machines", "charts", machineClassChartName), machineClassChartName, b.Shoot.SeedNamespace, values, nil); err != nil {
		return fmt.Errorf("Failed to deploy the generated machine classes: '%s'", err.Error())
	}

	// Generate machien deployment configuration based on previously computed list of deployments.
	machineDeploymentChartValues, err := b.generateMachineDeploymentConfig(machineDeployments, machineClassKind)
	if err != nil {
		return fmt.Errorf("Failed to generate the machine deployment config: '%s'", err.Error())
	}

	// Deploy generated machine deployments.
	if err := b.ApplyChartSeed(filepath.Join(chartPathMachines), "machines", b.Shoot.SeedNamespace, machineDeploymentChartValues, nil); err != nil {
		return fmt.Errorf("Failed to deploy the generated machine deployments: '%s'", err.Error())
	}

	// Wait until all generated machine deployments are healthy/available.
	if err := b.waitUntilMachineDeploymentsAvailable(machineDeployments); err != nil {
		return fmt.Errorf("Failed while waiting for all machine deployments to be ready: '%s'", err.Error())
	}

	// Delete all old machine deployments (i.e. those which were not previously computed by exist in the cluster).
	if err := b.cleanupMachineDeployments(machineDeployments); err != nil {
		return fmt.Errorf("Failed to cleanup the machine deployments: '%s'", err.Error())
	}

	// Delete all old machine classes (i.e. those which were not previously computed by exist in the cluster).
	usedSecrets, err := b.cleanupMachineClasses(machineClassPlural, machineDeployments)
	if err != nil {
		return fmt.Errorf("The CloudBotanist failed to cleanup the machine classes: '%s'", err.Error())
	}

	// Delete all old machine class secrets (i.e. those which were not previously computed by exist in the cluster).
	if err := b.cleanupMachineClassSecrets(usedSecrets); err != nil {
		return fmt.Errorf("The CloudBotanist failed to cleanup the orphaned machine class secrets: '%s'", err.Error())
	}

	return nil
}

// DestroyMachines deletes all existing MachineDeployments. As it won't trigger the drain of nodes it needs to label
// the existing machines. In case an errors occurs, it will return it.
func (b *HybridBotanist) DestroyMachines() error {
	var (
		machineList unstructured.Unstructured
		errorList   []error
		wg          sync.WaitGroup
	)

	if err := b.K8sSeedClient.MachineV1alpha1("GET", "machines", b.Shoot.SeedNamespace).Do().Into(&machineList); err != nil {
		return err
	}

	machineList.EachListItem(func(o runtime.Object) error {
		wg.Add(1)
		go func(obj *unstructured.Unstructured) {
			defer wg.Done()
			if err := b.labelMachine(obj); err != nil {
				errorList = append(errorList, err)
			}
		}(o.(*unstructured.Unstructured))
		return nil
	})
	wg.Wait()

	if len(errorList) > 0 {
		return fmt.Errorf("Labelling machines failed: %v", errorList)
	}

	var (
		_, machineClassPlural, _ = b.ShootCloudBotanist.GetMachineClassInfo()
		emptyMachineDeployments  = []operation.MachineDeployment{}
	)

	if err := b.cleanupMachineDeployments(emptyMachineDeployments); err != nil {
		return fmt.Errorf("Cleaning up machine deployments failed: %s", err.Error())
	}
	if _, err := b.cleanupMachineClasses(machineClassPlural, emptyMachineDeployments); err != nil {
		return fmt.Errorf("Cleaning up machine classes failed: %s", err.Error())
	}

	// Wait until all machine resources have been properly deleted.
	if err := b.waitUntilMachineResourcesDeleted(machineClassPlural); err != nil {
		return fmt.Errorf("Failed while waiting for all machine resources to be deleted: '%s'", err.Error())
	}

	return nil
}

// RefreshMachineClassSecrets updates all existing machine class secrets to reflect the latest
// cloud provider credentials.
func (b *HybridBotanist) RefreshMachineClassSecrets() error {
	secretList, err := b.listMachineClassSecrets()
	if err != nil {
		return err
	}

	// Refresh all secrets by updating the cloud provider credentials to the latest known values.
	for _, secret := range secretList.Items {
		var newSecret = secret

		newSecret.Data = b.ShootCloudBotanist.GenerateMachineClassSecretData()
		newSecret.Data["userData"] = secret.Data["userData"]

		if _, err := b.K8sSeedClient.UpdateSecretObject(&newSecret); err != nil {
			return err
		}
	}

	return nil
}

// generateMachineDeploymentConfig generates the configuration values for the machine deployment Helm chart. It
// does that based on the provided list of to-be-deployed <machineDeployments>.
func (b *HybridBotanist) generateMachineDeploymentConfig(machineDeployments []operation.MachineDeployment, classKind string) (map[string]interface{}, error) {
	var values = []map[string]interface{}{}

	for _, deployment := range machineDeployments {
		values = append(values, map[string]interface{}{
			"name":            deployment.Name,
			"replicas":        deployment.Replicas,
			"minReadySeconds": 500,
			"rollingUpdate": map[string]interface{}{
				"maxSurge":       1,
				"maxUnavailable": 1,
			},
			"labels": map[string]interface{}{
				"name": deployment.Name,
			},
			"class": map[string]interface{}{
				"kind": classKind,
				"name": deployment.ClassName,
			},
		})
	}

	return map[string]interface{}{
		"machineDeployments": values,
	}, nil
}

// labelMachine labels a machine object to be forcefully deleted.
func (b *HybridBotanist) labelMachine(obj *unstructured.Unstructured) error {
	var (
		labels      = obj.GetLabels()
		machineName = obj.GetName()
	)

	if val, ok := labels["force-deletion"]; ok && val == "True" {
		return nil
	}

	labels["force-deletion"] = "True"
	obj.SetLabels(labels)

	body, err := json.Marshal(obj.UnstructuredContent())
	if err != nil {
		return fmt.Errorf("Marshalling machine %s object failed: %s", machineName, err.Error())
	}

	return b.K8sSeedClient.MachineV1alpha1("PUT", "machines", b.Shoot.SeedNamespace).Name(machineName).Body(body).Do().Error()
}

// waitUntilMachineDeploymentsAvailable waits for a maximum of 30 minutes until all the desired <machineDeployments>
// were marked as healthy/available by the machine-controller-manager. It polls the status every 10 seconds.
func (b *HybridBotanist) waitUntilMachineDeploymentsAvailable(machineDeployments []operation.MachineDeployment) error {
	var (
		numReady   int64
		numDesired int64
	)
	return wait.Poll(5*time.Second, 1800*time.Second, func() (bool, error) {
		numReady, numDesired = 0, 0
		var machineDeploymentList unstructured.Unstructured

		if err := b.K8sSeedClient.MachineV1alpha1("GET", "machinedeployments", b.Shoot.SeedNamespace).Do().Into(&machineDeploymentList); err != nil {
			return false, err
		}

		if err := machineDeploymentList.EachListItem(func(o runtime.Object) error {
			for _, machineDeployment := range machineDeployments {
				var (
					obj                             = o.(*unstructured.Unstructured)
					deploymentName                  = obj.GetName()
					deploymentDesiredReplicas, _, _ = unstructured.NestedInt64(obj.UnstructuredContent(), "spec", "replicas")
					deploymentReadyReplicas, _, _   = unstructured.NestedInt64(obj.UnstructuredContent(), "status", "readyReplicas")
				)

				if machineDeployment.Name == deploymentName {
					numDesired += deploymentDesiredReplicas
					numReady += deploymentReadyReplicas
				}
			}
			return nil
		}); err != nil {
			return false, err
		}

		b.Logger.Infof("Waiting until all machines are healthy/ready (%d/%d OK)...", numReady, numDesired)
		if numReady >= numDesired {
			return true, nil
		}
		return false, nil
	})
}

// waitUntilMachineResourcesDeleted waits for a maximum of 30 minutes until all machine resoures have been properly
// deleted by the machine-controller-manager. It polls the status every 10 seconds.
func (b *HybridBotanist) waitUntilMachineResourcesDeleted(classKind string) error {
	var (
		resources         = []string{classKind, "machinedeployments", "machinesets", "machines"}
		numberOfResources = map[string]int{}
	)

	for _, resource := range resources {
		numberOfResources[resource] = -1
	}

	return wait.Poll(5*time.Second, 1800*time.Second, func() (bool, error) {
		for _, resource := range resources {
			if numberOfResources[resource] == 0 {
				continue
			}

			var list unstructured.Unstructured
			if err := b.K8sSeedClient.MachineV1alpha1("GET", resource, b.Shoot.SeedNamespace).Do().Into(&list); err != nil {
				return false, err
			}

			if field, ok := list.Object["items"]; ok {
				if items, ok := field.([]interface{}); ok {
					numberOfResources[resource] = len(items)
				}
			}
		}

		msg := ""
		for resource, count := range numberOfResources {
			if numberOfResources[resource] != 0 {
				msg += fmt.Sprintf("%d %s, ", count, resource)
			}
		}

		if msg != "" {
			b.Logger.Infof("Waiting until the following machine resources have been deleted: %s", strings.TrimSuffix(msg, ", "))
			return false, nil
		}
		return true, nil
	})
}

// cleanupMachineClasses deletes all machine classes which are not part of the provided list <machineDeployments>.
// It also computes a list of used secrets which contain the credentials and the cloud configuration. The list is
// returned in order that its items can be deleted by the HelperBotanist.
func (b *HybridBotanist) cleanupMachineClasses(machineClassPlural string, machineDeployments []operation.MachineDeployment) (sets.String, error) {
	var (
		machineClassList unstructured.Unstructured
		usedSecrets      = sets.NewString()
	)

	if err := b.K8sSeedClient.MachineV1alpha1("GET", machineClassPlural, b.Shoot.SeedNamespace).Do().Into(&machineClassList); err != nil {
		return nil, err
	}

	if err := machineClassList.EachListItem(func(o runtime.Object) error {
		var (
			obj                                  = o.(*unstructured.Unstructured)
			className                            = obj.GetName()
			secretRefName, secretRefNameFound, _ = unstructured.NestedString(obj.UnstructuredContent(), "spec", "secretRef", "name")
		)

		if !secretRefNameFound {
			return fmt.Errorf("could not find secret reference in class %s", className)
		}

		usedSecrets.Insert(secretRefName)
		if !operation.ClassContainedInMachineDeploymentList(className, machineDeployments) {
			return b.K8sSeedClient.MachineV1alpha1("DELETE", machineClassPlural, b.Shoot.SeedNamespace).Name(className).Do().Error()
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return usedSecrets, nil
}

// cleanupMachineDeployments deletes all machine deployments which are not part of the provided list
// <machineDeployments>.
func (b *HybridBotanist) cleanupMachineDeployments(machineDeployments []operation.MachineDeployment) error {
	var machineDeploymentList unstructured.Unstructured

	if err := b.K8sSeedClient.MachineV1alpha1("GET", "machinedeployments", b.Shoot.SeedNamespace).Do().Into(&machineDeploymentList); err != nil {
		return err
	}

	return machineDeploymentList.EachListItem(func(o runtime.Object) error {
		var (
			obj                    = o.(*unstructured.Unstructured)
			existingDeploymentName = obj.GetName()
		)

		if !operation.NameContainedInMachineDeploymentList(existingDeploymentName, machineDeployments) {
			return b.K8sSeedClient.MachineV1alpha1("DELETE", "machinedeployments", b.Shoot.SeedNamespace).Name(existingDeploymentName).Do().Error()
		}
		return nil
	})
}

func (b *HybridBotanist) listMachineClassSecrets() (*corev1.SecretList, error) {
	return b.K8sSeedClient.ListSecrets(b.Shoot.SeedNamespace, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=machineclass", common.GardenPurpose),
	})
}

// cleanupMachineClassSecrets deletes all unused machine class secrets (i.e., those which are not part
// of the provided list <usedSecrets>.
func (b *HybridBotanist) cleanupMachineClassSecrets(usedSecrets sets.String) error {
	secretList, err := b.listMachineClassSecrets()
	if err != nil {
		return err
	}

	// Cleanup all secrets which were used for machine classes that do not exist anymore.
	for _, secret := range secretList.Items {
		if !usedSecrets.Has(secret.Name) {
			if err := b.K8sSeedClient.DeleteSecret(secret.Namespace, secret.Name); err != nil {
				return err
			}
		}
	}

	return nil
}
