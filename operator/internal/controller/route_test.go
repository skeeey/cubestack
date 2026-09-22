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
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	aigwv1beta1 "github.com/envoyproxy/ai-gateway/api/v1beta1"
	egv1alpha1 "github.com/envoyproxy/gateway/api/v1alpha1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	aiv1alpha1 "github.com/suanova/cubestack/api/v1alpha1"
)

// notYetObservedClient models the manager's informer-cached client right after a
// write: an object this client created is persisted, but a Get for it still
// misses because the cache has not observed it yet. Reads of objects it did not
// create go through unchanged, so the spec exercises the create path only.
type notYetObservedClient struct {
	client.Client
	created map[client.ObjectKey]bool
}

func (c *notYetObservedClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if err := c.Client.Create(ctx, obj, opts...); err != nil {
		return err
	}
	c.created[client.ObjectKeyFromObject(obj)] = true
	return nil
}

func (c *notYetObservedClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if c.created[key] {
		return apierrors.NewNotFound(schema.GroupResource{Group: "aigateway.envoyproxy.io", Resource: "objects"}, key.Name)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// Gateway configuration shared by the checkRoute specs.
const (
	testGatewayName      = "platform"
	testGatewayNamespace = "cubestack-system"
	testCatalogHostname  = "ai.example.com"
	// testEngineModel is the engine's served model name the reconcile reads
	// from the ModelVersion and writes into the catalog entry as the
	// modelNameOverride of the backendRef.
	testEngineModel = "flash-engine"
)

func routeISVC(name string, publish bool) *aiv1alpha1.InferenceService {
	isvc := isvcForApply(name)
	isvc.Spec.Route = &aiv1alpha1.RouteSpec{Publish: publish, ModelName: "flash", TimeoutSeconds: ptrTo[int64](60)}
	return isvc
}

func routeProfile(name string) *aiv1alpha1.InferenceRuntimeProfile {
	return endpointProfile(name)
}

// routeReconciler returns the reconciler under test with the platform gateway
// and the model catalog configured.
func routeReconciler() *InferenceServiceReconciler {
	return &InferenceServiceReconciler{Client: k8sClient, Scheme: testScheme, GatewayName: testGatewayName, GatewayNamespace: testGatewayNamespace, CatalogHostname: testCatalogHostname, AgentRouterAvailable: true}
}

// aiAcceptedReason is the reason the Agent Router controller writes on the
// conditions it accepts: ai-gateway's newConditions writes a fixed
// "ReconciliationSucceeded" and puts the detail in the message.
const aiAcceptedReason = "ReconciliationSucceeded"

// acceptRoute marks the catalog objects accepted by the Agent Router
// controller for their current generation: envtest runs no Agent Router, so
// the specs write the conditions the controller would.
func acceptRoute(name string) {
	acceptAIGatewayRoute(name)
	acceptAIServiceBackend(name)
}

// acceptAIGatewayRoute writes the Accepted condition the Agent Router
// controller would set on the service's catalog entry (ObservedGeneration pins
// the status to the generation it was written for).
func acceptAIGatewayRoute(name string) {
	route := &aigwv1beta1.AIGatewayRoute{}
	Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name + "-route", Namespace: testNamespace}, route)).To(Succeed())
	route.Status.Conditions = []metav1.Condition{{
		Type: aigwv1beta1.ConditionTypeAccepted, Status: metav1.ConditionTrue, Reason: aiAcceptedReason,
		LastTransitionTime: metav1.Now(), ObservedGeneration: route.Generation,
	}}
	Expect(k8sClient.Status().Update(ctx, route)).To(Succeed())
}

// acceptAIServiceBackend writes the Accepted condition the Agent Router
// controller would set on the service's AIServiceBackend.
func acceptAIServiceBackend(name string) {
	sb := &aigwv1beta1.AIServiceBackend{}
	Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name + "-backend", Namespace: testNamespace}, sb)).To(Succeed())
	sb.Status.Conditions = []metav1.Condition{{
		Type: aigwv1beta1.ConditionTypeAccepted, Status: metav1.ConditionTrue, Reason: aiAcceptedReason,
		LastTransitionTime: metav1.Now(), ObservedGeneration: sb.Generation,
	}}
	Expect(k8sClient.Status().Update(ctx, sb)).To(Succeed())
}

var _ = Describe("checkRoute", func() {
	readyEndpoint := func(name string) *endpointCheck {
		return &endpointCheck{Internal: name + "-router.default.svc:8001", Role: testApplyRouterRole}
	}

	It("reports NotPublished and deletes the catalog objects when publish is false", func() {
		name := "route-off"
		Expect(k8sClient.Create(ctx, routeISVC(name, false))).To(Succeed())
		r := routeReconciler()
		// The catalog objects must be owned by the in-cluster isvc (the
		// ownerRef needs its UID), so they are built from the fetched object;
		// Publish is flipped on the local copy to model a service that was
		// published before publishing was turned off.
		isvc := mustGetISVC(ctx, name)
		isvc.Spec.Route.Publish = true
		Expect(k8sClient.Create(ctx, r.desiredBackend(isvc, routeProfile(name+"-prof"), 8001))).To(Succeed())
		Expect(k8sClient.Create(ctx, r.desiredAIServiceBackend(isvc))).To(Succeed())
		Expect(k8sClient.Create(ctx, r.desiredAIGatewayRoute(isvc, testEngineModel))).To(Succeed())

		check, err := r.checkRoute(ctx, mustGetISVC(ctx, name), routeProfile(name+"-prof"), readyEndpoint(name), testEngineModel)
		Expect(err).NotTo(HaveOccurred())
		Expect(check.Reason).To(Equal(RouteNotPublished))

		backend := &egv1alpha1.Backend{}
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKey{Name: name + "-endpoint", Namespace: testNamespace}, backend))).To(BeTrue())
		sb := &aigwv1beta1.AIServiceBackend{}
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKey{Name: name + "-backend", Namespace: testNamespace}, sb))).To(BeTrue())
		route := &aigwv1beta1.AIGatewayRoute{}
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKey{Name: name + "-route", Namespace: testNamespace}, route))).To(BeTrue())
	})

	It("creates the three catalog objects for a published service with a ready endpoint", func() {
		name := "route-on"
		Expect(k8sClient.Create(ctx, routeISVC(name, true))).To(Succeed())
		r := routeReconciler()
		// Persisted but not yet accepted by the Agent Router controller:
		// RouteReady must wait.
		check, err := r.checkRoute(ctx, mustGetISVC(ctx, name), routeProfile(name+"-prof"), readyEndpoint(name), testEngineModel)
		Expect(err).NotTo(HaveOccurred())
		Expect(check.Reason).To(Equal(RouteGatewayNotAccepted))

		backend := &egv1alpha1.Backend{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name + "-endpoint", Namespace: testNamespace}, backend)).To(Succeed())
		Expect(backend.Spec.Endpoints).To(Equal([]egv1alpha1.BackendEndpoint{{
			FQDN: &egv1alpha1.FQDNEndpoint{Hostname: name + "-router.default.svc.cluster.local", Port: 8001},
		}}))
		sb := &aigwv1beta1.AIServiceBackend{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name + "-backend", Namespace: testNamespace}, sb)).To(Succeed())
		Expect(string(sb.Spec.BackendRef.Name)).To(Equal(name + "-endpoint"))
		route := &aigwv1beta1.AIGatewayRoute{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name + "-route", Namespace: testNamespace}, route)).To(Succeed())
		Expect(route.Spec.Hostnames).To(Equal([]gatewayv1.Hostname{gatewayv1.Hostname(testCatalogHostname)}))
		Expect(route.Spec.Rules[0].Matches[0].Headers[0].Value).To(Equal("flash"))
		Expect(route.Spec.Rules[0].BackendRefs[0].ModelNameOverride).To(Equal(testEngineModel))

		// The Agent Router accepts both objects; the next check reports the
		// catalog entry ready.
		acceptRoute(name)
		check, err = r.checkRoute(ctx, mustGetISVC(ctx, name), routeProfile(name+"-prof"), readyEndpoint(name), testEngineModel)
		Expect(err).NotTo(HaveOccurred())
		Expect(check.Reason).To(BeEmpty())

		// The model-name uniqueness check scans every AIGatewayRoute in the
		// cluster, so this catalog entry must not leak into the later specs.
		Expect(k8sClient.Delete(ctx, route)).To(Succeed())
	})

	It("reports ModelNameConflict when another route claims the model name", func() {
		name := "route-conflict"
		Expect(k8sClient.Create(ctx, routeISVC(name, true))).To(Succeed())
		other := routeISVC("route-other", true)
		Expect(k8sClient.Create(ctx, other)).To(Succeed())
		r := routeReconciler()
		// Another service's catalog entry already claims the model name.
		existing := r.desiredAIGatewayRoute(other, testEngineModel)
		Expect(k8sClient.Create(ctx, existing)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, existing) }()

		check, err := r.checkRoute(ctx, mustGetISVC(ctx, name), routeProfile(name+"-prof"), readyEndpoint(name), testEngineModel)
		Expect(err).NotTo(HaveOccurred())
		Expect(check.Reason).To(Equal(RouteModelNameConflict))
		// The conflicting service publishes nothing of its own.
		route := &aigwv1beta1.AIGatewayRoute{}
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKey{Name: name + "-route", Namespace: testNamespace}, route))).To(BeTrue())
	})

	It("reports AgentRouterUnavailable when the cluster serves no Agent Router CRDs", func() {
		// The isvc is never persisted: the degrade check runs before any
		// cluster write.
		r := routeReconciler()
		r.AgentRouterAvailable = false
		check, err := r.checkRoute(ctx, routeISVC("isvc-degraded", true), routeProfile("p"), readyEndpoint("isvc-degraded"), testEngineModel)
		Expect(err).NotTo(HaveOccurred())
		Expect(check.Reason).To(Equal(RouteAgentRouterUnavailable))
	})

	It("reports EndpointNotReady without a ready endpoint", func() {
		name := "route-noep"
		Expect(k8sClient.Create(ctx, routeISVC(name, true))).To(Succeed())
		r := routeReconciler()
		check, err := r.checkRoute(ctx, mustGetISVC(ctx, name), routeProfile(name+"-prof"), &endpointCheck{Reason: "EndpointNotReady"}, testEngineModel)
		Expect(err).NotTo(HaveOccurred())
		Expect(check.Reason).To(Equal(EndpointNotReady))

		check, err = r.checkRoute(ctx, mustGetISVC(ctx, name), routeProfile(name+"-prof"), nil, testEngineModel)
		Expect(err).NotTo(HaveOccurred())
		Expect(check.Reason).To(Equal(EndpointNotReady))

		// Nothing is published without a reachable endpoint.
		route := &aigwv1beta1.AIGatewayRoute{}
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKey{Name: name + "-route", Namespace: testNamespace}, route))).To(BeTrue())
	})

	It("keeps the catalog objects when the endpoint goes unready", func() {
		name := "route-keep"
		Expect(k8sClient.Create(ctx, routeISVC(name, true))).To(Succeed())
		r := routeReconciler()
		_, err := r.checkRoute(ctx, mustGetISVC(ctx, name), routeProfile(name+"-prof"), readyEndpoint(name), testEngineModel)
		Expect(err).NotTo(HaveOccurred())
		// The endpoint goes unready: the catalog entry must survive (design:
		// route lifecycle follows the Service; gateway health checks drain).
		check, err := r.checkRoute(ctx, mustGetISVC(ctx, name), routeProfile(name+"-prof"), &endpointCheck{Reason: "EndpointNotReady"}, testEngineModel)
		Expect(err).NotTo(HaveOccurred())
		Expect(check.Reason).To(Equal(EndpointNotReady))
		route := &aigwv1beta1.AIGatewayRoute{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name + "-route", Namespace: testNamespace}, route)).To(Succeed())
		Expect(k8sClient.Delete(ctx, route)).To(Succeed())
	})

	It("reports GatewayNotAccepted until both AI objects are accepted", func() {
		name := "route-accept"
		Expect(k8sClient.Create(ctx, routeISVC(name, true))).To(Succeed())
		r := routeReconciler()
		check, err := r.checkRoute(ctx, mustGetISVC(ctx, name), routeProfile(name+"-prof"), readyEndpoint(name), testEngineModel)
		Expect(err).NotTo(HaveOccurred())
		Expect(check.Reason).To(Equal(RouteGatewayNotAccepted))

		// Only the entry is accepted: the backend is still pending.
		acceptAIGatewayRoute(name)
		check, err = r.checkRoute(ctx, mustGetISVC(ctx, name), routeProfile(name+"-prof"), readyEndpoint(name), testEngineModel)
		Expect(err).NotTo(HaveOccurred())
		Expect(check.Reason).To(Equal(RouteGatewayNotAccepted))

		// Both accepted: the catalog entry is live.
		acceptAIServiceBackend(name)
		check, err = r.checkRoute(ctx, mustGetISVC(ctx, name), routeProfile(name+"-prof"), readyEndpoint(name), testEngineModel)
		Expect(err).NotTo(HaveOccurred())
		Expect(check.Reason).To(BeEmpty())

		route := &aigwv1beta1.AIGatewayRoute{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name + "-route", Namespace: testNamespace}, route)).To(Succeed())
		Expect(k8sClient.Delete(ctx, route)).To(Succeed())
	})

	It("updates the AIGatewayRoute when the declared timeout changes", func() {
		// The model name must be unique in the cluster: the specs above leave
		// catalog entries behind.
		name := "route-update"
		isvc := isvcForApply(name)
		isvc.Spec.Route = &aiv1alpha1.RouteSpec{Publish: true, ModelName: "update-model", TimeoutSeconds: ptrTo[int64](60)}
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		r := routeReconciler()
		_, err := r.checkRoute(ctx, mustGetISVC(ctx, name), routeProfile(name+"-prof"), readyEndpoint(name), testEngineModel)
		Expect(err).NotTo(HaveOccurred())
		acceptRoute(name) // the Agent Router accepts; RouteReady reports "" below
		check, err := r.checkRoute(ctx, mustGetISVC(ctx, name), routeProfile(name+"-prof"), readyEndpoint(name), testEngineModel)
		Expect(err).NotTo(HaveOccurred())
		Expect(check.Reason).To(BeEmpty())

		route := &aigwv1beta1.AIGatewayRoute{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name + "-route", Namespace: testNamespace}, route)).To(Succeed())
		Expect(string(*route.Spec.Rules[0].Timeouts.Request)).To(Equal("60s"))
		oldRV := route.ResourceVersion

		// The in-cluster spec changes the timeout; the next check must update
		// the stored entry instead of leaving it stale.
		current := mustGetISVC(ctx, name)
		*current.Spec.Route.TimeoutSeconds = 30
		Expect(k8sClient.Update(ctx, current)).To(Succeed())

		// The update bumps the route generation; the acceptance check requires
		// fresh status (ObservedGeneration == the new generation), so the
		// Agent Router re-accepts before RouteReady returns.
		check, err = r.checkRoute(ctx, mustGetISVC(ctx, name), routeProfile(name+"-prof"), readyEndpoint(name), testEngineModel)
		Expect(err).NotTo(HaveOccurred())
		Expect(check.Reason).To(Equal(RouteGatewayNotAccepted))
		acceptRoute(name)
		check, err = r.checkRoute(ctx, mustGetISVC(ctx, name), routeProfile(name+"-prof"), readyEndpoint(name), testEngineModel)
		Expect(err).NotTo(HaveOccurred())
		Expect(check.Reason).To(BeEmpty())
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name + "-route", Namespace: testNamespace}, route)).To(Succeed())
		Expect(string(*route.Spec.Rules[0].Timeouts.Request)).To(Equal("30s"))
		Expect(route.ResourceVersion).NotTo(Equal(oldRV))
	})

	It("sees no drift in the objects a real API server stored", func() {
		// Pins the default set the *NeedsUpdate comparisons mirror against what
		// an actual API server writes: a default this list is missing makes
		// checkRoute rewrite the object on every pass — a write that runs the
		// optimistic-concurrency check against the Agent Router controller's
		// status writes and re-enqueues the service through the Owns() watch.
		name := "route-stored"
		isvc := isvcForApply(name)
		isvc.Spec.Route = &aiv1alpha1.RouteSpec{Publish: true, ModelName: "stored-model", TimeoutSeconds: ptrTo[int64](60)}
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		r := routeReconciler()
		_, err := r.checkRoute(ctx, mustGetISVC(ctx, name), routeProfile(name+"-prof"), readyEndpoint(name), testEngineModel)
		Expect(err).NotTo(HaveOccurred())

		stored := func(obj client.Object, name string) {
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: testNamespace}, obj)).To(Succeed())
		}
		backend := &egv1alpha1.Backend{}
		stored(backend, name+"-endpoint")
		sb := &aigwv1beta1.AIServiceBackend{}
		stored(sb, name+"-backend")
		route := &aigwv1beta1.AIGatewayRoute{}
		stored(route, name+"-route")

		// A second pass over the objects the API server stored must touch none
		// of them.
		_, err = r.checkRoute(ctx, mustGetISVC(ctx, name), routeProfile(name+"-prof"), readyEndpoint(name), testEngineModel)
		Expect(err).NotTo(HaveOccurred())
		after := &egv1alpha1.Backend{}
		stored(after, name+"-endpoint")
		Expect(after.ResourceVersion).To(Equal(backend.ResourceVersion))
		afterSB := &aigwv1beta1.AIServiceBackend{}
		stored(afterSB, name+"-backend")
		Expect(afterSB.ResourceVersion).To(Equal(sb.ResourceVersion))
		afterRoute := &aigwv1beta1.AIGatewayRoute{}
		stored(afterRoute, name+"-route")
		Expect(afterRoute.ResourceVersion).To(Equal(route.ResourceVersion))

		// Those comparisons are what decides whether that write is sent at
		// all: an object the API server stored must not read as drift, or every
		// pass writes — an update that runs the optimistic-concurrency check
		// against the Agent Router controller's status writes.
		current := mustGetISVC(ctx, name)
		Expect(aiGatewayRouteNeedsUpdate(afterRoute, r.desiredAIGatewayRoute(current, testEngineModel))).To(BeFalse())
		Expect(backendNeedsUpdate(after, r.desiredBackend(current, routeProfile(name+"-prof"), 8001))).To(BeFalse())
		Expect(aiServiceBackendNeedsUpdate(afterSB, r.desiredAIServiceBackend(current))).To(BeFalse())

		// A genuine change is still drift: the tolerance above must not degrade
		// into "never update", or the catalog objects would keep serving the
		// old endpoint or schema after the spec changed.
		Expect(backendNeedsUpdate(after, r.desiredBackend(current, routeProfile(name+"-prof"), 8002))).To(BeTrue())
		otherSchema := r.desiredAIServiceBackend(current)
		otherSchema.Spec.APISchema.Name = aigwv1beta1.APISchemaAWSBedrock
		Expect(aiServiceBackendNeedsUpdate(afterSB, otherSchema)).To(BeTrue())
	})

	It("does not report acceptance from a stale status after a spec update", func() {
		// The catalog objects are accepted for generation 1; a spec change
		// bumps the AIGatewayRoute's generation, and the pre-update status must
		// not report RouteReady until the Agent Router writes status for the
		// new generation.
		name := "route-stale"
		isvc := isvcForApply(name)
		isvc.Spec.Route = &aiv1alpha1.RouteSpec{Publish: true, ModelName: "stale-model", TimeoutSeconds: ptrTo[int64](60)}
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		r := routeReconciler()
		_, err := r.checkRoute(ctx, mustGetISVC(ctx, name), routeProfile(name+"-prof"), readyEndpoint(name), testEngineModel)
		Expect(err).NotTo(HaveOccurred())
		acceptRoute(name) // accepted for generation 1

		// The spec changes the timeout; the update bumps the AIGatewayRoute
		// generation but the stored status still carries generation 1.
		current := mustGetISVC(ctx, name)
		*current.Spec.Route.TimeoutSeconds = 30
		Expect(k8sClient.Update(ctx, current)).To(Succeed())
		check, err := r.checkRoute(ctx, mustGetISVC(ctx, name), routeProfile(name+"-prof"), readyEndpoint(name), testEngineModel)
		Expect(err).NotTo(HaveOccurred())
		Expect(check.Reason).To(Equal(RouteGatewayNotAccepted))

		// The Agent Router writes status for the new generation; RouteReady
		// returns.
		acceptRoute(name)
		check, err = r.checkRoute(ctx, mustGetISVC(ctx, name), routeProfile(name+"-prof"), readyEndpoint(name), testEngineModel)
		Expect(err).NotTo(HaveOccurred())
		Expect(check.Reason).To(BeEmpty())
	})

	It("deletes a legacy per-model HTTPRoute owned by the service", func() {
		// The retired publish primitive left <isvc>-route HTTPRoutes behind; one
		// still around would keep serving the old hostname with the old timeout
		// semantics. Turning publishing off must remove it too.
		name := "route-legacy"
		Expect(k8sClient.Create(ctx, routeISVC(name, false))).To(Succeed())
		r := routeReconciler()
		isvc := mustGetISVC(ctx, name)
		legacy := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: name + "-route", Namespace: testNamespace}}
		Expect(ctrl.SetControllerReference(isvc, legacy, testScheme)).To(Succeed())
		Expect(k8sClient.Create(ctx, legacy)).To(Succeed())

		check, err := r.checkRoute(ctx, mustGetISVC(ctx, name), routeProfile(name+"-prof"), readyEndpoint(name), testEngineModel)
		Expect(err).NotTo(HaveOccurred())
		Expect(check.Reason).To(Equal(RouteNotPublished))

		got := &gatewayv1.HTTPRoute{}
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKey{Name: name + "-route", Namespace: testNamespace}, got))).To(BeTrue())
	})

	It("deletes a legacy per-model HTTPRoute while the service is publishing", func() {
		// The cleanup runs on every pass, outside the publish branch: a legacy
		// HTTPRoute left behind would keep serving the old hostname with the old
		// timeout semantics even though the service now publishes through the
		// catalog — and it shares the <isvc>-route name with the catalog entry,
		// so only the kind tells the two apart.
		name := "route-legacy-pub"
		isvc := routeISVC(name, true)
		// Unique in the cluster: the catalog entry claims this model name.
		isvc.Spec.Route.ModelName = "legacy-pub-model"
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		r := routeReconciler()
		legacy := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: name + "-route", Namespace: testNamespace}}
		Expect(ctrl.SetControllerReference(mustGetISVC(ctx, name), legacy, testScheme)).To(Succeed())
		Expect(k8sClient.Create(ctx, legacy)).To(Succeed())

		check, err := r.checkRoute(ctx, mustGetISVC(ctx, name), routeProfile(name+"-prof"), readyEndpoint(name), testEngineModel)
		Expect(err).NotTo(HaveOccurred())
		// The pass took the publish path: the catalog entry exists and waits for
		// the Agent Router's acceptance.
		Expect(check.Reason).To(Equal(RouteGatewayNotAccepted))
		route := &aigwv1beta1.AIGatewayRoute{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name + "-route", Namespace: testNamespace}, route)).To(Succeed())

		// ...and the legacy HTTPRoute of the same name is removed nevertheless.
		got := &gatewayv1.HTTPRoute{}
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKey{Name: name + "-route", Namespace: testNamespace}, got))).To(BeTrue())
	})

	It("leaves the HTTPRoute the Agent Router derives from the catalog entry alone", func() {
		// The Agent Router's controller materialises an HTTPRoute named after the
		// AIGatewayRoute, in that route's namespace, controlled by it and carrying
		// its ai-gateway-generated annotation — the very <isvc>-route name this
		// cleanup looks at. That generated route is not the service's object:
		// deleting it would remove a live catalog route, and failing on it (as a
		// strict ownership check does) would wedge every reconcile of every
		// published service on a cluster that runs the Agent Router, publish=false
		// included.
		name := "route-generated"
		isvc := routeISVC(name, true)
		// Unique in the cluster: the catalog entry claims this model name.
		isvc.Spec.Route.ModelName = "generated-model"
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		r := routeReconciler()
		entry := r.desiredAIGatewayRoute(mustGetISVC(ctx, name), testEngineModel)
		Expect(k8sClient.Create(ctx, entry)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, entry) }()

		generated := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: name + "-route", Namespace: testNamespace}}
		Expect(ctrl.SetControllerReference(entry, generated, testScheme)).To(Succeed())
		Expect(k8sClient.Create(ctx, generated)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, generated) }()

		check, err := r.checkRoute(ctx, mustGetISVC(ctx, name), routeProfile(name+"-prof"), readyEndpoint(name), testEngineModel)
		Expect(err).NotTo(HaveOccurred())
		Expect(check.Reason).To(Equal(RouteGatewayNotAccepted))

		// ...and the generated route survived the pass, still the Agent Router's
		// object rather than this service's.
		got := &gatewayv1.HTTPRoute{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name + "-route", Namespace: testNamespace}, got)).To(Succeed())
		Expect(metav1.IsControlledBy(got, entry)).To(BeTrue())
	})

	It("reports GatewayNotAccepted when the catalog objects are not yet observed", func() {
		// The re-read after the apply goes through the client's cache: right after
		// the create the objects are persisted but the cache may not have observed
		// them yet, so a Get misses. That is a catalog entry waiting for the Agent
		// Router, not a failed pass.
		name := "route-unobserved"
		isvc := routeISVC(name, true)
		// Unique in the cluster: the catalog entry claims this model name.
		isvc.Spec.Route.ModelName = "unobserved-model"
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		r := routeReconciler()
		r.Client = &notYetObservedClient{Client: k8sClient, created: map[client.ObjectKey]bool{}}

		check, err := r.checkRoute(ctx, mustGetISVC(ctx, name), routeProfile(name+"-prof"), readyEndpoint(name), testEngineModel)
		Expect(err).NotTo(HaveOccurred())
		Expect(check.Reason).To(Equal(RouteGatewayNotAccepted))

		route := &aigwv1beta1.AIGatewayRoute{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name + "-route", Namespace: testNamespace}, route)).To(Succeed())
		Expect(k8sClient.Delete(ctx, route)).To(Succeed())
	})

	It("sets the RouteReady condition from the check", func() {
		conditions := []metav1.Condition{}
		setRouteReadyCondition(&conditions, &routeCheck{Reason: "NotPublished"})
		cond := meta.FindStatusCondition(conditions, aiv1alpha1.ConditionRouteReady)
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("NotPublished"))
	})
})

var _ = Describe("desired catalog objects", func() {
	profile := func() *aiv1alpha1.InferenceRuntimeProfile {
		p := routeProfile("catalog-profile")
		p.Spec.Endpoint.Role = "vllm"
		return p
	}
	// expectCatalogLabels pins the label map of every catalog object literally —
	// the three keys with the values the fixture's isvc carries — rather than
	// through routeLabels, which would restate the code under test.
	expectCatalogLabels := func(labels map[string]string) {
		Expect(labels).To(HaveLen(3))
		Expect(labels).To(HaveKeyWithValue("ai.cubestack.io/inference-service", "catalog-isvc"))
		Expect(labels).To(HaveKeyWithValue("ai.cubestack.io/profile", "prof"))
		Expect(labels).To(HaveKeyWithValue("ai.cubestack.io/managed-by", "inference-Controller"))
	}

	It("renders the Backend pointing at the endpoint Service FQDN", func() {
		isvc := routeISVC("catalog-isvc", true)
		backend := routeReconciler().desiredBackend(isvc, profile(), 8000)

		Expect(backend.Name).To(Equal("catalog-isvc-endpoint"))
		Expect(backend.Namespace).To(Equal(testNamespace))
		expectCatalogLabels(backend.Labels)
		Expect(backend.Spec.Type).To(HaveValue(Equal(egv1alpha1.BackendTypeEndpoints)))
		Expect(backend.Spec.Endpoints).To(Equal([]egv1alpha1.BackendEndpoint{{
			FQDN: &egv1alpha1.FQDNEndpoint{Hostname: "catalog-isvc-vllm.default.svc.cluster.local", Port: 8000},
		}}))
		Expect(metav1.IsControlledBy(backend, isvc)).To(BeTrue())
	})

	It("renders the AIServiceBackend with the OpenAI schema and the Backend ref", func() {
		isvc := routeISVC("catalog-isvc", true)
		sb := routeReconciler().desiredAIServiceBackend(isvc)

		Expect(sb.Name).To(Equal("catalog-isvc-backend"))
		expectCatalogLabels(sb.Labels)
		Expect(sb.Spec.APISchema.Name).To(Equal(aigwv1beta1.APISchemaOpenAI))
		Expect(sb.Spec.BackendRef.Group).To(HaveValue(Equal(gatewayv1.Group(envoyGatewayAPIGroup))))
		Expect(sb.Spec.BackendRef.Kind).To(HaveValue(Equal(gatewayv1.Kind(egBackendKind))))
		Expect(string(sb.Spec.BackendRef.Name)).To(Equal("catalog-isvc-endpoint"))
		Expect(metav1.IsControlledBy(sb, isvc)).To(BeTrue())
	})

	It("renders the AIGatewayRoute as a catalog entry of the shared hostname", func() {
		isvc := routeISVC("catalog-isvc", true)
		route := routeReconciler().desiredAIGatewayRoute(isvc, "qwen38-27b")

		Expect(route.Name).To(Equal("catalog-isvc-route"))
		expectCatalogLabels(route.Labels)
		Expect(route.Spec.Hostnames).To(Equal([]gatewayv1.Hostname{gatewayv1.Hostname(testCatalogHostname)}))
		Expect(route.Spec.ParentRefs).To(HaveLen(1))
		Expect(string(route.Spec.ParentRefs[0].Name)).To(Equal(testGatewayName))
		Expect(route.Spec.ParentRefs[0].Namespace).To(HaveValue(Equal(gatewayv1.Namespace(testGatewayNamespace))))

		rule := route.Spec.Rules[0]
		Expect(rule.Matches[0].Headers).To(Equal([]gatewayv1.HTTPHeaderMatch{{
			Type:  ptrTo(gatewayv1.HeaderMatchExact),
			Name:  gatewayv1.HTTPHeaderName(aiModelHeaderName),
			Value: "flash", // the route.modelName routeISVC declares
		}}))
		Expect(rule.BackendRefs[0].Name).To(Equal("catalog-isvc-backend"))
		Expect(rule.BackendRefs[0].ModelNameOverride).To(Equal("qwen38-27b"))
		Expect(rule.BackendRefs[0].Weight).To(HaveValue(Equal(int32(1))))
		Expect(rule.BackendRefs[0].Priority).To(HaveValue(Equal(uint32(0))))
		Expect(rule.Timeouts.Request).To(HaveValue(Equal(gatewayv1.Duration("60s"))))
		Expect(rule.StreamIdleTimeout).To(HaveValue(Equal(gatewayv1.Duration("300s"))))
		Expect(rule.ModelsOwnedBy).To(HaveValue(Equal(catalogOwnedBy)))
		Expect(metav1.IsControlledBy(route, isvc)).To(BeTrue())
	})

	It("honours the declared timeouts", func() {
		isvc := routeISVC("catalog-isvc", true)
		isvc.Spec.Route.TimeoutSeconds = ptrTo(int64(3600))
		isvc.Spec.Route.IdleTimeoutSeconds = ptrTo(int64(120))
		rule := routeReconciler().desiredAIGatewayRoute(isvc, "m").Spec.Rules[0]

		Expect(rule.Timeouts.Request).To(HaveValue(Equal(gatewayv1.Duration("3600s"))))
		Expect(rule.StreamIdleTimeout).To(HaveValue(Equal(gatewayv1.Duration("120s"))))
	})
})

var _ = Describe("routeParentsAccepted", func() {
	// parentsAcceptedBy renders the status.parents a gateway writes when it
	// accepts the route, observed for the given generation (0 = a gateway that
	// did not report one).
	parentsAcceptedBy := func(observed int64) []gatewayv1.RouteParentStatus {
		return gatewayRouteParents(gatewayv1.ParentReference{
			Name:      gatewayv1.ObjectName(testGatewayName),
			Namespace: ptrTo(gatewayv1.Namespace(testGatewayNamespace)),
		}, observed, true, "", "")
	}

	It("accepts a status written for the current generation", func() {
		Expect(routeParentsAccepted(parentsAcceptedBy(3), 3, testGatewayName, testGatewayNamespace)).To(BeTrue())
	})

	It("rejects a status written for an earlier generation", func() {
		Expect(routeParentsAccepted(parentsAcceptedBy(2), 3, testGatewayName, testGatewayNamespace)).To(BeFalse())
	})

	It("accepts a status that leaves observedGeneration unset", func() {
		// observedGeneration is optional in the Gateway API schema, so a gateway
		// may omit it. Treating that as stale would withhold every environment's
		// endpoints on such a cluster; the tolerance is deliberate.
		Expect(routeParentsAccepted(parentsAcceptedBy(0), 3, testGatewayName, testGatewayNamespace)).To(BeTrue())
	})
})

var _ = Describe("routeParentsTo", func() {
	// refTo builds a parentRef with group and kind spelled out, as the API server
	// stores them; "" leaves the field unset.
	refTo := func(group gatewayv1.Group, kind gatewayv1.Kind, name, namespace string) gatewayv1.ParentReference {
		ref := gatewayv1.ParentReference{
			Group: ptrTo(group),
			Kind:  ptrTo(kind),
			Name:  gatewayv1.ObjectName(name),
		}
		if namespace != "" {
			ref.Namespace = ptrTo(gatewayv1.Namespace(namespace))
		}
		return ref
	}
	gateway := func(name, namespace string) gatewayv1.ParentReference {
		return refTo(gatewayAPIGroup, gatewayKind, name, namespace)
	}

	It("matches the configured Gateway", func() {
		refs := []gatewayv1.ParentReference{gateway(testGatewayName, testGatewayNamespace)}
		Expect(routeParentsTo(refs, testNamespace, testGatewayName, testGatewayNamespace)).To(BeTrue())
	})

	It("applies the API's defaults to an unset namespace, group and kind", func() {
		// name alone means a Gateway in the Gateway API group, in the route's own
		// namespace — the defaults the API server fills in on write.
		refs := []gatewayv1.ParentReference{{Name: gatewayv1.ObjectName(testGatewayName)}}
		Expect(routeParentsTo(refs, testGatewayNamespace, testGatewayName, testGatewayNamespace)).To(BeTrue())
	})

	It("ignores a parent in another namespace", func() {
		refs := []gatewayv1.ParentReference{gateway(testGatewayName, "somewhere-else")}
		Expect(routeParentsTo(refs, testNamespace, testGatewayName, testGatewayNamespace)).To(BeFalse())
	})

	It("ignores a parent that is not a Gateway", func() {
		// A same-name, same-namespace Service, as a mesh route may parent to: it
		// has no listener on our Gateway, so its route must not reserve a port.
		svc := refTo("", serviceKind, testGatewayName, testGatewayNamespace)
		Expect(routeParentsTo([]gatewayv1.ParentReference{svc}, testNamespace, testGatewayName, testGatewayNamespace)).To(BeFalse())

		// A Gateway of the same name in another API group is likewise not ours.
		foreign := refTo("example.com", gatewayKind, testGatewayName, testGatewayNamespace)
		Expect(routeParentsTo([]gatewayv1.ParentReference{foreign}, testNamespace, testGatewayName, testGatewayNamespace)).To(BeFalse())
	})
})

var _ = Describe("listenerSetParentsToGateway", func() {
	// A ListenerSet carries one parentRef rather than a list, and its namespace
	// defaults to the ListenerSet's own — the same rule routeParentsTo applies to
	// a route's parentRefs, which is why the two share it.
	set := func(namespace string, mut func(*gatewayv1.ParentGatewayReference)) *gatewayv1.ListenerSet {
		ls := &gatewayv1.ListenerSet{
			ObjectMeta: metav1.ObjectMeta{Name: "de-l4-l4", Namespace: testNamespace},
			Spec: gatewayv1.ListenerSetSpec{
				ParentRef: gatewayv1.ParentGatewayReference{
					Group: ptrTo(gatewayv1.Group(gatewayAPIGroup)),
					Kind:  ptrTo(gatewayv1.Kind(gatewayKind)),
					Name:  gatewayv1.ObjectName(testGatewayName),
				},
			},
		}
		if namespace != "" {
			ls.Spec.ParentRef.Namespace = ptrTo(gatewayv1.Namespace(namespace))
		}
		if mut != nil {
			mut(&ls.Spec.ParentRef)
		}
		return ls
	}

	It("matches a ListenerSet contributing to the configured Gateway", func() {
		Expect(listenerSetParentsToGateway(set(testGatewayNamespace, nil), testGatewayName, testGatewayNamespace)).To(BeTrue())
	})

	It("applies the API's defaults to an unset namespace, group and kind", func() {
		ls := set("", func(ref *gatewayv1.ParentGatewayReference) {
			ref.Group, ref.Kind = nil, nil
		})
		Expect(listenerSetParentsToGateway(ls, testGatewayName, testNamespace)).To(BeTrue())
	})

	It("ignores a ListenerSet on another Gateway", func() {
		Expect(listenerSetParentsToGateway(set(testGatewayNamespace, func(ref *gatewayv1.ParentGatewayReference) {
			ref.Name = "other-gw"
		}), testGatewayName, testGatewayNamespace)).To(BeFalse())
	})

	It("ignores a ListenerSet in another namespace", func() {
		Expect(listenerSetParentsToGateway(set("somewhere-else", nil), testGatewayName, testGatewayNamespace)).To(BeFalse())
	})
})
