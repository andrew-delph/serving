//go:build e2e
// +build e2e

/*
Copyright 2020 The Knative Authors

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
	"context"
	"fmt"
	"strconv"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	pkgtest "knative.dev/pkg/test"
	"knative.dev/serving/pkg/apis/autoscaling"
	"knative.dev/serving/pkg/apis/serving"
	v1 "knative.dev/serving/pkg/apis/serving/v1"
	rtesting "knative.dev/serving/pkg/testing/v1"
	v1options "knative.dev/serving/pkg/testing/v1"
	"knative.dev/serving/test"
	v1test "knative.dev/serving/test/v1"
)

// withConfigImage sets the container image to be the provided string.
func withConfigImage(img string) v1options.ServiceOption {
	return func(svc *v1.Service) {
		svc.Spec.Template.Spec.PodSpec.Containers[0].Image = img
	}
}

func generationLabelSelector(config string, generation int) string {
	return fmt.Sprintf("%s=%s,%s=%s", serving.ConfigurationLabelKey, config, "serving.knative.dev/configurationGeneration", strconv.Itoa(generation))
}

// This test case creates a service which can never reach a ready state.
// The service is then udpated with a healthy image and is verified that
// the healthy revision is ready and the unhealhy revision is scaled to zero.
func TestDeadStartToHealthy(t *testing.T) {
	t.Parallel()

	clients := Setup(t)

	svcName := test.ObjectNameForTest(t)
	names := test.ResourceNames{
		Config:  svcName,
		Service: svcName,
		Image:   test.DeadStart,
	}
	test.EnsureTearDown(t, clients, &names)

	const initialScale = 3
	// Setup Initial Service with failing deadstart image.
	_, err := v1test.CreateService(t, clients, names,
		rtesting.WithConfigAnnotations(map[string]string{
			autoscaling.MinScaleAnnotationKey: strconv.Itoa(initialScale),
		}),
		rtesting.WithRevisionTimeoutSeconds(5),                                                         // All scale to zero quickly.
		rtesting.WithConfigAnnotations(map[string]string{serving.ProgressDeadlineAnnotationKey: "1h"}), // ProgressDeadline is very long.
	)
	if err != nil {
		t.Fatalf("Failed to create Service %q: %v", names.Service, err)
	}

	t.Logf("Waiting for Service %q to reconile.", names.Service)
	err = v1test.WaitForServiceState(clients.ServingClient, names.Service,
		func(s *v1.Service) (bool, error) {
			ss := s.Status
			return ss.ObservedGeneration == s.Generation, nil
		}, "ServiceIsCreated")
	if err != nil {
		t.Fatal("Error obtaining updated Revision", err)
	}

	t.Logf("Obtaining latest revision for Configuration %q.", names.Config)
	err = v1test.WaitForConfigurationState(clients.ServingClient, names.Config, func(c *v1.Configuration) (bool, error) {
		if c.Status.LatestCreatedRevisionName != names.Revision {
			names.Revision = c.Status.LatestCreatedRevisionName
			return true, nil
		}
		return false, nil
	}, "ConfigurationUpdatedWithRevision")
	if err != nil {
		t.Fatal("Error obtaining Revision's name", err)
	}

	t.Logf("Waiting for Configuration %q pods to be restarting.", names.Config)
	if err := v1test.WaitForConfigurationState(clients.ServingClient, names.Config, func(s *v1.Configuration) (b bool, e error) {
		pods := clients.KubeClient.CoreV1().Pods(test.ServingFlags.TestNamespace)
		podList, err := pods.List(context.Background(), metav1.ListOptions{
			LabelSelector: generationLabelSelector(names.Config, 1),
			FieldSelector: "status.phase!=Pending",
		})

		if err != nil {
			return false, err
		}
		gotPods := len(podList.Items)
		// if pods dont exits return.
		if gotPods < initialScale {
			return false, nil
		}
		// verify the pods are restarting.
		for i := range podList.Items {
			conds := podList.Items[i].Status.ContainerStatuses
			for j := range conds {
				if conds[j].RestartCount > 0 {
					return true, nil
				}
			}
		}
		return false, nil
	}, "ConfigurationIsRestarting"); err != nil {
		t.Fatal("Configuration does not have the desired number of pods running:", err)
	}

	t.Logf("Update service %q with working image.", names.Service)
	_, err = v1test.UpdateService(t, clients, names, rtesting.WithServiceImage(pkgtest.ImagePath(test.HelloWorld)))
	if err != nil {
		t.Fatal("Error updating Service", err)
	}

	t.Logf("Waiting for Configuration %q pods to reconcile.", names.Config)
	err = v1test.WaitForConfigurationState(clients.ServingClient, names.Config, func(c *v1.Configuration) (bool, error) {
		if c.Status.LatestCreatedRevisionName != names.Revision {
			names.Revision = c.Status.LatestCreatedRevisionName
			return true, nil
		}
		return false, nil
	}, "ConfigurationUpdatedWithRevision")
	if err != nil {
		t.Fatal("Error obtaining LatestCreatedRevisionName", err)
	}

	t.Logf("Waiting for Revision %q pods to become ready.", names.Revision)
	if err := v1test.WaitForRevisionState(
		clients.ServingClient, names.Revision, v1test.IsRevisionReady, "RevisionIsReady",
	); err != nil {
		t.Fatalf("The Revision %q did not become ready: %v", names.Revision, err)
	}

	t.Logf("Waiting first generation of Config %q to scale to zero.", names.Config)
	if err := v1test.WaitForConfigurationState(clients.ServingClient, names.Config, func(s *v1.Configuration) (b bool, e error) {
		pods := clients.KubeClient.CoreV1().Pods(test.ServingFlags.TestNamespace)
		podList, err := pods.List(context.Background(), metav1.ListOptions{
			LabelSelector: generationLabelSelector(names.Config, 1),
			FieldSelector: "status.phase!=Pending",
		})
		if err != nil {
			return false, err
		}
		// If 0 pods exists, resolve.
		gotPods := len(podList.Items)
		return gotPods == 0, nil
	}, "ConfigurationIsScaledToZero"); err != nil {
		t.Fatal("Configuration did not scale to zero:", err)
	}

}

// This test case updates a healthy service with an image that can never reach a ready state.
// The healthy revision remains Ready and the DeadStart revision doesnt not scale down until ProgressDeadline is reached.
func TestDeadStartFromHealthy(t *testing.T) {
	const initialScale = 3
	var firstRevision string
	var secondRevision string

	t.Parallel()

	clients := Setup(t)

	svcName := test.ObjectNameForTest(t)
	names := test.ResourceNames{
		Config:  svcName,
		Service: svcName,
		Image:   test.HelloWorld,
	}
	test.EnsureTearDown(t, clients, &names)

	// Setup Initial Service with helloworld image.
	_, err := v1test.CreateService(t, clients, names,
		rtesting.WithConfigAnnotations(map[string]string{
			autoscaling.MinScaleAnnotationKey: strconv.Itoa(initialScale),
		}),
		rtesting.WithRevisionTimeoutSeconds(5),                                                         // Allow scale to zero quickly.
		rtesting.WithConfigAnnotations(map[string]string{serving.ProgressDeadlineAnnotationKey: "1h"}), // ProgressDeadline is very long.
	)
	if err != nil {
		t.Fatalf("Failed to create Service %q: %v", names.Service, err)
	}

	t.Logf("Waiting for Service %q to reconile.", names.Service)
	err = v1test.WaitForServiceState(clients.ServingClient, names.Service,
		func(s *v1.Service) (bool, error) {
			ss := s.Status
			return ss.ObservedGeneration == s.Generation, nil
		}, "ServiceIsCreated")
	if err != nil {
		t.Fatal("Error obtaining updated Revision", err)
	}

	t.Logf("Waiting for Configuration %q pods to reconcile.", names.Config)
	err = v1test.WaitForConfigurationState(clients.ServingClient, names.Config, func(c *v1.Configuration) (bool, error) {
		if c.Status.LatestCreatedRevisionName != names.Revision {
			names.Revision = c.Status.LatestCreatedRevisionName
			firstRevision = c.Status.LatestCreatedRevisionName
			return true, nil
		}
		return false, nil
	}, "ConfigurationUpdatedWithRevision")
	if err != nil {
		t.Fatal("Error obtaining LatestCreatedRevisionName", err)
	}

	t.Logf("Waiting for Revision %q pods to become ready.", names.Revision)
	if err := v1test.WaitForRevisionState(
		clients.ServingClient, names.Revision, v1test.IsRevisionReady, "RevisionIsReady",
	); err != nil {
		t.Fatalf("The Revision %q did not become ready: %v", names.Revision, err)
	}

	t.Logf("Update service %q with deadstart image.", names.Service)
	_, err = v1test.UpdateService(t, clients, names, rtesting.WithServiceImage(pkgtest.ImagePath(test.DeadStart)))
	if err != nil {
		t.Fatal("Error updating Service", err)
	}

	t.Logf("Waiting for Configuration %q pods to reconcile.", names.Config)
	err = v1test.WaitForConfigurationState(clients.ServingClient, names.Config, func(c *v1.Configuration) (bool, error) {
		if c.Status.LatestCreatedRevisionName != names.Revision {
			names.Revision = c.Status.LatestCreatedRevisionName
			secondRevision = c.Status.LatestCreatedRevisionName
			return true, nil
		}
		return false, nil
	}, "ConfigurationUpdatedWithRevision")
	if err != nil {
		t.Fatal("Error obtaining LatestCreatedRevisionName", err)
	}

	t.Logf("Waiting for Configuration %q pods to be restarting.", names.Config)
	if err := v1test.WaitForConfigurationState(clients.ServingClient, names.Config, func(s *v1.Configuration) (b bool, e error) {
		pods := clients.KubeClient.CoreV1().Pods(test.ServingFlags.TestNamespace)
		podList, err := pods.List(context.Background(), metav1.ListOptions{
			LabelSelector: generationLabelSelector(names.Config, 2),
			FieldSelector: "status.phase!=Pending",
		})

		if err != nil {
			return false, err
		}
		gotPods := len(podList.Items)
		// if pods dont exits return.
		if gotPods < initialScale {
			return false, nil
		}
		// verify the pods are restarting.
		for i := range podList.Items {
			conds := podList.Items[i].Status.ContainerStatuses
			for j := range conds {
				if conds[j].RestartCount > 2 {
					return true, nil
				}
			}
		}
		return false, nil
	}, "ConfigurationIsRestarting"); err != nil {
		t.Fatal("Configuration does not have the desired number of pods running:", err)
	}

	// Verify that the first revision is still the Ready revision.
	t.Logf("Waiting for Configuration %q pods to reconcile.", names.Config)
	err = v1test.WaitForConfigurationState(clients.ServingClient, names.Config, func(c *v1.Configuration) (bool, error) {
		if firstRevision == c.Status.LatestReadyRevisionName && secondRevision == c.Status.LatestCreatedRevisionName {
			return true, nil
		}
		return false, nil
	}, "ConfigurationWaitingToBecomeReady")
	if err != nil {
		t.Fatal("Error obtaining LatestCreatedRevisionName", err)
	}
}
