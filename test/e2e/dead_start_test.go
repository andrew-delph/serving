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

// TestInitScalePositive tests setting of annotation initialScale to greater than 0 on
// the revision level.
func TestDeadStart(t *testing.T) {
	t.Parallel()

	clients := Setup(t)

	svcName := test.ObjectNameForTest(t)
	names := test.ResourceNames{
		Config:  svcName,
		Route:   svcName,
		Service: svcName,
		// TrafficTarget: svcName,
		Image: "deadstart",
		// Image: test.HelloWorld,
	}
	test.EnsureTearDown(t, clients, &names)

	const initialScale = 1

	_, err := v1test.CreateService(t, clients, names, rtesting.WithConfigAnnotations(map[string]string{
		autoscaling.MinScaleAnnotationKey: "1",
	}), rtesting.WithRevisionTimeoutSeconds(5))
	if err != nil {
		t.Fatalf("Failed to create initial Service: %v: %v", names.Service, err)
	}

	t.Log("Waiting for Service to transition to Ready.", "service", names.Service)
	err = v1test.WaitForServiceState(clients.ServingClient, names.Service,
		func(s *v1.Service) (bool, error) {
			ss := s.Status
			return ss.ObservedGeneration == s.Generation &&
				ss.GetCondition("Ready").IsUnknown(), nil
		}, "RevisionMissing")
	if err != nil {
		t.Fatal("Error obtaining Revision's name", err)
	}

	configurationSelector := fmt.Sprintf("%s=%s", serving.ConfigurationLabelKey, names.Config)

	t.Log("wait for updated revision")
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

	t.Logf("Waiting for Configuration %q to transition to RESTART with %d number of pods.", names.Config, 1)
	if err := v1test.WaitForConfigurationState(clients.ServingClient, names.Config, func(s *v1.Configuration) (b bool, e error) {
		pods := clients.KubeClient.CoreV1().Pods(test.ServingFlags.TestNamespace)
		podList, err := pods.List(context.Background(), metav1.ListOptions{
			LabelSelector: configurationSelector + fmt.Sprintf(",%s=%s", "serving.knative.dev/configurationGeneration", "1"),
			// Include both running and terminating pods, because we will scale down from
			// initial scale immediately if there's no traffic coming in.
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
	}, "ConfigurationIsReadyWithinitialScale"); err != nil {
		t.Fatal("Configuration does not have the desired number of pods running:", err)
	}

	_, err = v1test.UpdateService(t, clients, names, rtesting.WithServiceImage(pkgtest.ImagePath(test.HelloWorld)))
	if err != nil {
		t.Fatalf("err : %v", err)
	}

	t.Log("wait for updated revision")
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

	t.Log("Waiting for new revision to become ready")
	if err := v1test.WaitForRevisionState(
		clients.ServingClient, names.Revision, v1test.IsRevisionReady, "RevisionIsReady",
	); err != nil {
		t.Fatalf("The Revision %q did not become ready: %v", names.Revision, err)
	}

	t.Log("Service should reflect new revision created and ready in status.")
	if err := v1test.WaitForConfigurationState(clients.ServingClient, names.Config, func(s *v1.Configuration) (b bool, e error) {
		pods := clients.KubeClient.CoreV1().Pods(test.ServingFlags.TestNamespace)
		podList, err := pods.List(context.Background(), metav1.ListOptions{
			LabelSelector: configurationSelector + fmt.Sprintf(",%s=%s", "serving.knative.dev/configurationGeneration", "1"),
			// Include both running and terminating pods, because we will scale down from
			// initial scale immediately if there's no traffic coming in.
			FieldSelector: "status.phase!=Pending",
		})
		if err != nil {
			return false, err
		}
		gotPods := len(podList.Items)
		// if pods dont exits return.
		if gotPods > 0 {
			return false, nil
		} else {
			return true, nil
		}
	}, "ConfigurationIsReadyWithinitialScale"); err != nil {
		t.Fatal("Configuration does not have the desired number of pods running:", err)
	}

}
