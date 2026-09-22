/*
Copyright 2026.

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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	aigwv1beta1 "github.com/envoyproxy/ai-gateway/api/v1beta1"
	egv1alpha1 "github.com/envoyproxy/gateway/api/v1alpha1"
)

var _ = Describe("Agent Router fixtures", func() {
	It("stores an AIGatewayRoute through the API server", func() {
		route := &aigwv1beta1.AIGatewayRoute{
			ObjectMeta: metav1.ObjectMeta{Name: "fixture-route", Namespace: testNamespace},
			Spec: aigwv1beta1.AIGatewayRouteSpec{
				Rules: []aigwv1beta1.AIGatewayRouteRule{{BackendRefs: []aigwv1beta1.AIGatewayRouteRuleBackendRef{{Name: "somewhere"}}}},
			},
		}
		Expect(k8sClient.Create(ctx, route)).To(Succeed())
		// The fixture must deliver the server-written defaults the publish
		// path's drift comparison relies on (weight 1, priority 0).
		stored := &aigwv1beta1.AIGatewayRoute{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: route.Name, Namespace: testNamespace}, stored)).To(Succeed())
		Expect(stored.Spec.Rules[0].BackendRefs[0].Weight).To(HaveValue(Equal(int32(1))))
		Expect(stored.Spec.Rules[0].BackendRefs[0].Priority).To(HaveValue(Equal(uint32(0))))
		Expect(k8sClient.Delete(ctx, route)).To(Succeed())
	})

	It("stores an Envoy Gateway Backend through the API server", func() {
		backend := &egv1alpha1.Backend{
			ObjectMeta: metav1.ObjectMeta{Name: "fixture-backend", Namespace: testNamespace},
			Spec: egv1alpha1.BackendSpec{
				Endpoints: []egv1alpha1.BackendEndpoint{{
					FQDN: &egv1alpha1.FQDNEndpoint{Hostname: "example.default.svc.cluster.local", Port: 8000},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, backend)).To(Succeed())
		// The type default is server-written, like the AIGatewayRoute counters
		// above.
		stored := &egv1alpha1.Backend{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: backend.Name, Namespace: testNamespace}, stored)).To(Succeed())
		Expect(stored.Spec.Type).To(HaveValue(Equal(egv1alpha1.BackendTypeEndpoints)))
		Expect(k8sClient.Delete(ctx, backend)).To(Succeed())
	})
})

var _ = Describe("AgentRouterAvailable", func() {
	It("reports false when the mapper serves none of the three kinds", func() {
		mapper := meta.NewDefaultRESTMapper(nil)
		Expect(AgentRouterAvailable(mapper)).To(BeFalse())
	})

	// All three kinds are gated together: the guarded setup below registers
	// Owns/Watches for each of them, so a cluster serving only some of the
	// Agent Router kinds would abort controller startup.
	It("reports false when the mapper serves only the AIGatewayRoute kind", func() {
		mapper := meta.NewDefaultRESTMapper(nil)
		mapper.Add(aigwv1beta1.SchemeGroupVersion.WithKind("AIGatewayRoute"), meta.RESTScopeNamespace)
		Expect(AgentRouterAvailable(mapper)).To(BeFalse())
	})

	It("reports false when the mapper serves only the Envoy Gateway Backend kind", func() {
		mapper := meta.NewDefaultRESTMapper(nil)
		mapper.Add(egv1alpha1.SchemeGroupVersion.WithKind("Backend"), meta.RESTScopeNamespace)
		Expect(AgentRouterAvailable(mapper)).To(BeFalse())
	})

	// A partial ai-gateway install, or an upstream release that moves
	// AIServiceBackend out of v1beta1: the two kinds that gate the route and
	// its backend are served, the third is not.
	It("reports false when the mapper serves two of the three kinds", func() {
		mapper := meta.NewDefaultRESTMapper(nil)
		mapper.Add(aigwv1beta1.SchemeGroupVersion.WithKind("AIGatewayRoute"), meta.RESTScopeNamespace)
		mapper.Add(egv1alpha1.SchemeGroupVersion.WithKind("Backend"), meta.RESTScopeNamespace)
		Expect(AgentRouterAvailable(mapper)).To(BeFalse())
	})

	It("reports true when the mapper serves all three kinds", func() {
		mapper := meta.NewDefaultRESTMapper(nil)
		mapper.Add(aigwv1beta1.SchemeGroupVersion.WithKind("AIGatewayRoute"), meta.RESTScopeNamespace)
		mapper.Add(aigwv1beta1.SchemeGroupVersion.WithKind("AIServiceBackend"), meta.RESTScopeNamespace)
		mapper.Add(egv1alpha1.SchemeGroupVersion.WithKind("Backend"), meta.RESTScopeNamespace)
		Expect(AgentRouterAvailable(mapper)).To(BeTrue())
	})
})
