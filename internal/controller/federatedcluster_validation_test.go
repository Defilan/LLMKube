/*
Copyright 2025.

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

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	federationv1alpha1 "github.com/defilantech/llmkube/api/federation/v1alpha1"
)

// There is no admission webhook for FederatedCluster, so the CRD schema is the
// only guard on the heartbeat cadence. These Its pin that bound at the API
// server, which is what an operator actually hits.
var _ = Describe("FederatedCluster admission validation", func() {
	var (
		ctx  context.Context
		name string
	)

	BeforeEach(func() {
		ctx = context.Background()
		name = "fc-heartbeat-validation"
	})

	AfterEach(func() {
		fc := &federationv1alpha1.FederatedCluster{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, fc); err == nil {
			_ = k8sClient.Delete(ctx, fc)
		}
	})

	It("rejects a heartbeat interval below the edge push cadence", func() {
		fc := &federationv1alpha1.FederatedCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: federationv1alpha1.FederatedClusterSpec{
				HeartbeatIntervalSeconds: 5,
			},
		}

		err := k8sClient.Create(ctx, fc)
		Expect(err).To(HaveOccurred())
		// A CRD schema violation is Invalid (422), not merely any error: a bare
		// non-nil check would pass on an unrelated failure.
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "err = %v", err)
		Expect(err.Error()).To(ContainSubstring("30"))
	})

	It("accepts the cadence floor exactly", func() {
		fc := &federationv1alpha1.FederatedCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: federationv1alpha1.FederatedClusterSpec{
				HeartbeatIntervalSeconds: 30,
			},
		}

		Expect(k8sClient.Create(ctx, fc)).To(Succeed())
	})
})
