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
	"fmt"
	"strconv"
	"strings"

	aigwv1beta1 "github.com/envoyproxy/ai-gateway/api/v1beta1"
	egv1alpha1 "github.com/envoyproxy/gateway/api/v1alpha1"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	aiv1alpha1 "github.com/suanova/cubestack/api/v1alpha1"
)

// Reasons reported by checkRoute when the route is not published or cannot be
// published ("" means the route is published and accepted by the Agent Router).
const (
	RouteNotPublished           = "NotPublished"
	RouteModelNameConflict      = "ModelNameConflict"
	RouteGatewayNotConfigured   = "GatewayNotConfigured"
	RouteGatewayNotAccepted     = "GatewayNotAccepted"
	RouteAgentRouterUnavailable = "AgentRouterUnavailable"
)

// routeCheck reports the route-publish outcome.
type routeCheck struct {
	Reason string // NotPublished | ModelNameConflict | GatewayNotConfigured | EndpointNotReady | AgentRouterUnavailable | GatewayNotAccepted | ""
	Err    error  // API-level failure
}

// Kinds, API groups and the fixed fields of the Agent Router catalog objects a
// published service owns: the Envoy Gateway Backend naming the endpoint pool,
// the AIServiceBackend declaring the upstream schema and referencing that
// Backend, and the AIGatewayRoute carrying the catalog entry. The Backend type,
// weight, priority and modelsOwnedBy are spelled explicitly so the API server's
// defaults never read as drift.
const (
	envoyGatewayAPIGroup = "gateway.envoyproxy.io"
	egBackendKind        = "Backend"
	aiModelHeaderName    = "x-ai-eg-model"
	catalogOwnedBy       = "CubeStack"
)

// Names of the three catalog objects a published service owns.
func backendName(isvc *aiv1alpha1.InferenceService) string          { return isvc.Name + "-endpoint" }
func aiServiceBackendName(isvc *aiv1alpha1.InferenceService) string { return isvc.Name + "-backend" }
func aiGatewayRouteName(isvc *aiv1alpha1.InferenceService) string   { return isvc.Name + "-route" }

// routeLabels are the labels every catalog object carries.
func routeLabels(isvc *aiv1alpha1.InferenceService) map[string]string {
	return map[string]string{
		inferenceServiceLabelKey: isvc.Name,
		profileLabelKey:          isvc.Spec.ProfileRef,
		managedByLabelKey:        managedByValue,
	}
}

// desiredBackend builds the Envoy Gateway Backend an AIServiceBackend must
// reference: an FQDN endpoint pointing at the endpoint role's Service.
func (r *InferenceServiceReconciler) desiredBackend(isvc *aiv1alpha1.InferenceService, profile *aiv1alpha1.InferenceRuntimeProfile, port int32) *egv1alpha1.Backend {
	backend := &egv1alpha1.Backend{
		ObjectMeta: metav1.ObjectMeta{Name: backendName(isvc), Namespace: isvc.Namespace, Labels: routeLabels(isvc)},
		Spec: egv1alpha1.BackendSpec{
			Type: ptr(egv1alpha1.BackendTypeEndpoints),
			Endpoints: []egv1alpha1.BackendEndpoint{{
				FQDN: &egv1alpha1.FQDNEndpoint{
					Hostname: fmt.Sprintf("%s-%s.%s.svc.cluster.local", isvc.Name, profile.Spec.Endpoint.Role, isvc.Namespace),
					Port:     port,
				},
			}},
		},
	}
	_ = ctrl.SetControllerReference(isvc, backend, r.Scheme)
	return backend
}

// desiredAIServiceBackend builds the AIServiceBackend declaring the upstream
// schema (OpenAI: the model servers are vLLM) and referencing the Backend.
func (r *InferenceServiceReconciler) desiredAIServiceBackend(isvc *aiv1alpha1.InferenceService) *aigwv1beta1.AIServiceBackend {
	sb := &aigwv1beta1.AIServiceBackend{
		ObjectMeta: metav1.ObjectMeta{Name: aiServiceBackendName(isvc), Namespace: isvc.Namespace, Labels: routeLabels(isvc)},
		Spec: aigwv1beta1.AIServiceBackendSpec{
			APISchema: aigwv1beta1.VersionedAPISchema{Name: aigwv1beta1.APISchemaOpenAI},
			BackendRef: gatewayv1.BackendObjectReference{
				Group: ptr(gatewayv1.Group(envoyGatewayAPIGroup)),
				Kind:  ptr(gatewayv1.Kind(egBackendKind)),
				Name:  gatewayv1.ObjectName(backendName(isvc)),
			},
		},
	}
	_ = ctrl.SetControllerReference(isvc, sb, r.Scheme)
	return sb
}

// desiredAIGatewayRoute builds the catalog entry: a rule matching the catalog
// model name — set by the Agent Router's ext_proc from the request body's
// "model" field and injected as the x-ai-eg-model header — routing to the
// service's backend with the engine's served model name (modelName) written
// back over it.
func (r *InferenceServiceReconciler) desiredAIGatewayRoute(isvc *aiv1alpha1.InferenceService, modelName string) *aigwv1beta1.AIGatewayRoute {
	timeout := int64(0)
	if isvc.Spec.Route.TimeoutSeconds != nil {
		timeout = *isvc.Spec.Route.TimeoutSeconds
	}
	idle := int64(300)
	if isvc.Spec.Route.IdleTimeoutSeconds != nil {
		idle = *isvc.Spec.Route.IdleTimeoutSeconds
	}
	route := &aigwv1beta1.AIGatewayRoute{
		ObjectMeta: metav1.ObjectMeta{Name: aiGatewayRouteName(isvc), Namespace: isvc.Namespace, Labels: routeLabels(isvc)},
		Spec: aigwv1beta1.AIGatewayRouteSpec{
			ParentRefs: []gatewayv1.ParentReference{{
				Name:      gatewayv1.ObjectName(r.GatewayName),
				Namespace: ptr(gatewayv1.Namespace(r.GatewayNamespace)),
			}},
			Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(r.CatalogHostname)},
			Rules: []aigwv1beta1.AIGatewayRouteRule{{
				Matches: []aigwv1beta1.AIGatewayRouteRuleMatch{{
					Headers: []gatewayv1.HTTPHeaderMatch{{
						Type:  ptr(gatewayv1.HeaderMatchExact),
						Name:  gatewayv1.HTTPHeaderName(aiModelHeaderName),
						Value: isvc.Spec.Route.ModelName,
					}},
				}},
				BackendRefs: []aigwv1beta1.AIGatewayRouteRuleBackendRef{{
					Name:              aiServiceBackendName(isvc),
					ModelNameOverride: modelName,
					Weight:            ptr(int32(1)),
					Priority:          ptr(uint32(0)),
				}},
				Timeouts:          &gatewayv1.HTTPRouteTimeouts{Request: ptr(gatewayv1.Duration(fmt.Sprintf("%ds", timeout)))},
				StreamIdleTimeout: ptr(gatewayv1.Duration(fmt.Sprintf("%ds", idle))),
				ModelsOwnedBy:     ptr(catalogOwnedBy),
			}},
		},
	}
	_ = ctrl.SetControllerReference(isvc, route, r.Scheme)
	return route
}

// checkRoute applies the publish decision: publish=false removes the catalog
// objects (a valid state reported as NotPublished); publish=true creates the
// three catalog objects of the model catalog entry and reports acceptance from
// the two Agent Router objects. A cluster without the Agent Router CRDs
// degrades to AgentRouterUnavailable instead of failing.
func (r *InferenceServiceReconciler) checkRoute(ctx context.Context, isvc *aiv1alpha1.InferenceService, profile *aiv1alpha1.InferenceRuntimeProfile, endpoint *endpointCheck, modelName string) (*routeCheck, error) {
	check := &routeCheck{}
	// The retired per-model hostname HTTPRoute is removed on every pass: an
	// object left behind would keep serving the old hostname with the old
	// timeout semantics.
	if err := r.deleteLegacyHTTPRoute(ctx, isvc); err != nil {
		return routeErr(check, err)
	}
	if isvc.Spec.Route == nil || !isvc.Spec.Route.Publish {
		if err := r.deleteCatalogObjects(ctx, isvc); err != nil {
			return routeErr(check, err)
		}
		check.Reason = RouteNotPublished
		return check, nil
	}
	if !r.AgentRouterAvailable {
		check.Reason = RouteAgentRouterUnavailable
		return check, nil
	}
	if endpoint == nil || endpoint.Internal == "" {
		check.Reason = EndpointNotReady
		return check, nil
	}
	if r.CatalogHostname == "" || r.GatewayName == "" {
		check.Reason = RouteGatewayNotConfigured
		return check, nil
	}
	taken, err := r.catalogModelNameTaken(ctx, isvc)
	if err != nil {
		return routeErr(check, err)
	}
	if taken {
		check.Reason = RouteModelNameConflict
		return check, nil
	}
	port, err := endpointPort(endpoint.Internal)
	if err != nil {
		check.Reason = EndpointNotReady
		return check, nil
	}
	backend := r.desiredBackend(isvc, profile, port)
	if err := applyOwned(ctx, r.Client, isvc, backend, backendNeedsUpdate); err != nil {
		return routeErr(check, err)
	}
	sb := r.desiredAIServiceBackend(isvc)
	if err := applyOwned(ctx, r.Client, isvc, sb, aiServiceBackendNeedsUpdate); err != nil {
		return routeErr(check, err)
	}
	desired := r.desiredAIGatewayRoute(isvc, modelName)
	if err := applyOwned(ctx, r.Client, isvc, desired, aiGatewayRouteNeedsUpdate); err != nil {
		return routeErr(check, err)
	}
	// Re-fetch after a possible update: the acceptance check must not run
	// against a status written for a previous generation.
	fresh := &aigwv1beta1.AIGatewayRoute{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(desired), fresh); err != nil {
		return routeErrAfterApply(check, err)
	}
	freshSB := &aigwv1beta1.AIServiceBackend{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(sb), freshSB); err != nil {
		return routeErrAfterApply(check, err)
	}
	return aiRouteAcceptance(check, fresh, freshSB), nil
}

// applyOwned creates the desired object, or updates it in place when a
// controller-owned field drifted. An identical object is left untouched:
// updating it would bump the resourceVersion and re-enqueue the service
// through the Owns() watch in an unbounded loop.
func applyOwned[T client.Object](ctx context.Context, c client.Client, owner *aiv1alpha1.InferenceService, desired T, needsUpdate func(existing, desired T) bool) error {
	existing, ok := desired.DeepCopyObject().(T)
	if !ok {
		return fmt.Errorf("deep copy of %T is not the same type", desired)
	}
	err := c.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if apierrors.IsNotFound(err) {
		return c.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if err := ensureOwned(existing, owner.UID); err != nil {
		return err
	}
	if needsUpdate(existing, desired) {
		desired.SetResourceVersion(existing.GetResourceVersion())
		return c.Update(ctx, desired)
	}
	return nil
}

// backendNeedsUpdate reports whether the controller-owned fields of an existing
// Backend differ from the desired ones: the labels and the Spec. Server-side
// fields (owner references, status, the defaults the API server fills into the
// spec) never count as drift. An identical object is left untouched: updating it
// would bump the resourceVersion and re-enqueue the service through the Owns()
// watch in an unbounded loop.
func backendNeedsUpdate(existing, desired *egv1alpha1.Backend) bool {
	return !apiequality.Semantic.DeepEqual(existing.Labels, desired.Labels) ||
		!apiequality.Semantic.DeepEqual(existing.Spec, desired.Spec)
}

// aiServiceBackendNeedsUpdate reports whether the controller-owned fields of an
// existing AIServiceBackend differ from the desired ones (the labels and the
// Spec), with the same no-drift rule as backendNeedsUpdate.
func aiServiceBackendNeedsUpdate(existing, desired *aigwv1beta1.AIServiceBackend) bool {
	return !apiequality.Semantic.DeepEqual(existing.Labels, desired.Labels) ||
		!apiequality.Semantic.DeepEqual(existing.Spec, desired.Spec)
}

// aiGatewayRouteNeedsUpdate reports whether the controller-owned fields of an
// existing AIGatewayRoute differ from the desired ones: the labels and the
// Spec. The parentRef group and kind are compared as the API server stores them
// — the CRD defaults them to the Gateway API group and the Gateway kind — so
// the server's own defaults never read as drift.
func aiGatewayRouteNeedsUpdate(existing, desired *aigwv1beta1.AIGatewayRoute) bool {
	return !apiequality.Semantic.DeepEqual(existing.Labels, desired.Labels) ||
		!apiequality.Semantic.DeepEqual(existing.Spec, aiGatewayRouteSpecWithDefaults(desired.Spec))
}

// aiGatewayRouteSpecWithDefaults spells out the spec as the API server stores
// it: the AIGatewayRoute CRD defaults each parentRef's group and kind (the
// Gateway API group and the Gateway kind), so a stored route differs from the
// spec the controller builds. A comparison that counts those as drift makes
// checkRoute issue an update on every reconcile — a write that runs the
// optimistic-concurrency check against the Agent Router controller's status
// writes and re-enqueues the service through the Owns() watch. Everything else
// the controller writes is spelled explicitly in desiredAIGatewayRoute.
func aiGatewayRouteSpecWithDefaults(spec aigwv1beta1.AIGatewayRouteSpec) aigwv1beta1.AIGatewayRouteSpec {
	out := *spec.DeepCopy()
	for i := range out.ParentRefs {
		if out.ParentRefs[i].Group == nil {
			out.ParentRefs[i].Group = ptr(gatewayv1.Group(gatewayAPIGroup))
		}
		if out.ParentRefs[i].Kind == nil {
			out.ParentRefs[i].Kind = ptr(gatewayv1.Kind(gatewayKind))
		}
	}
	return out
}

// catalogModelNameTaken reports whether another published service already
// claims this service's catalog model name (the x-ai-eg-model match value of
// another service's route; this service's own route is excluded by owner).
func (r *InferenceServiceReconciler) catalogModelNameTaken(ctx context.Context, isvc *aiv1alpha1.InferenceService) (bool, error) {
	list := &aigwv1beta1.AIGatewayRouteList{}
	if err := r.List(ctx, list); err != nil {
		return false, err
	}
	for i := range list.Items {
		item := &list.Items[i]
		if owner := metav1.GetControllerOf(item); owner != nil && owner.UID == isvc.UID {
			continue
		}
		for _, rule := range item.Spec.Rules {
			for _, match := range rule.Matches {
				for _, header := range match.Headers {
					if header.Name == aiModelHeaderName && header.Value == isvc.Spec.Route.ModelName {
						return true, nil
					}
				}
			}
		}
	}
	return false, nil
}

// aiRouteAcceptance reports the catalog objects' acceptance: RouteReady
// requires both the AIGatewayRoute and its AIServiceBackend to be Accepted for
// the current generation — a route persisted but not accepted by the Agent
// Router controller is reported as GatewayNotAccepted.
func aiRouteAcceptance(check *routeCheck, route *aigwv1beta1.AIGatewayRoute, backend *aigwv1beta1.AIServiceBackend) *routeCheck {
	if aiObjectAccepted(route.Status.Conditions, route.Generation) && aiObjectAccepted(backend.Status.Conditions, backend.Generation) {
		return check // Reason stays "" — published and accepted.
	}
	check.Reason = RouteGatewayNotAccepted
	return check
}

// aiObjectAccepted reports whether the Agent Router controller accepted the
// object at the given generation. A condition pinned to an older generation is
// stale; an unset observedGeneration counts as current (the schema leaves it
// optional).
//
// The zero tolerance is deliberate: the real Agent Router writes no
// observedGeneration, so in production a stale condition cannot be told from a
// current one this way, while the envtest fixtures do write one (the Agent
// Router runs no controller there). The staleness guarantee the specs assert is
// therefore stricter than production's — it holds for any controller that
// reports observedGeneration, and degrades to the accepted set for one that
// does not.
func aiObjectAccepted(conditions []metav1.Condition, generation int64) bool {
	cond := meta.FindStatusCondition(conditions, aigwv1beta1.ConditionTypeAccepted)
	if cond == nil {
		return false
	}
	if cond.ObservedGeneration != 0 && cond.ObservedGeneration != generation {
		return false
	}
	return cond.Status == metav1.ConditionTrue
}

// deleteCatalogObjects removes the three objects a published service owns.
func (r *InferenceServiceReconciler) deleteCatalogObjects(ctx context.Context, isvc *aiv1alpha1.InferenceService) error {
	for _, obj := range []client.Object{
		&aigwv1beta1.AIGatewayRoute{ObjectMeta: metav1.ObjectMeta{Name: aiGatewayRouteName(isvc), Namespace: isvc.Namespace}},
		&aigwv1beta1.AIServiceBackend{ObjectMeta: metav1.ObjectMeta{Name: aiServiceBackendName(isvc), Namespace: isvc.Namespace}},
		&egv1alpha1.Backend{ObjectMeta: metav1.ObjectMeta{Name: backendName(isvc), Namespace: isvc.Namespace}},
	} {
		if err := r.deleteOwned(ctx, isvc, obj); err != nil {
			return err
		}
	}
	return nil
}

// deleteLegacyHTTPRoute removes the per-model hostname HTTPRoute the previous
// publish primitive created, when it is still around and owned by the service.
//
// Its name and namespace are also the ones the Agent Router's controller gives
// the HTTPRoute it materialises from this service's AIGatewayRoute: that route
// is controlled by the AIGatewayRoute, not by the service, so a foreign object
// under this name must be left to its own controller. Treating it as a conflict
// (as the catalog cleanup's strict ownership check does) would fail every
// reconcile of every published service on a cluster that runs the Agent Router.
func (r *InferenceServiceReconciler) deleteLegacyHTTPRoute(ctx context.Context, isvc *aiv1alpha1.InferenceService) error {
	legacy := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: isvc.Name + "-route", Namespace: isvc.Namespace}}
	err := r.Get(ctx, client.ObjectKeyFromObject(legacy), legacy)
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil // absent, or a cluster serving no HTTPRoute CRD
	}
	if err != nil {
		return err
	}
	if owner := metav1.GetControllerOf(legacy); owner == nil || owner.UID != isvc.UID {
		return nil
	}
	if err := r.Delete(ctx, legacy); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// deleteOwned deletes a named object when it exists and is controlled by the
// service. A missing object and an unserved kind both count as already gone.
func (r *InferenceServiceReconciler) deleteOwned(ctx context.Context, isvc *aiv1alpha1.InferenceService, obj client.Object) error {
	err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj)
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := ensureOwned(obj, isvc.UID); err != nil {
		return err
	}
	if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// routeParentsAccepted reports whether parents carries an entry for the named
// Gateway reporting Accepted=True and ResolvedRefs=True for the CURRENT
// generation: a condition whose ObservedGeneration is set but does not match the
// route's generation is stale (the gateway has not processed the latest spec
// yet). Without a matching entry the gateway has not processed the route.
//
// A condition with an unset (zero) ObservedGeneration counts as current. The
// Gateway API schema leaves it optional on these conditions, so a gateway may
// simply not report it; reading those conditions as absent would withhold every
// environment's endpoints on such a cluster — a permanent false negative bought
// to close a transient one.
//
// It takes the parents rather than a route so it applies to every route kind:
// HTTPRoute and TCPRoute are distinct types sharing only this status shape.
func routeParentsAccepted(parents []gatewayv1.RouteParentStatus, generation int64, gatewayName, gatewayNamespace string) bool {
	parent := routeParentFor(parents, gatewayName, gatewayNamespace)
	if parent == nil {
		return false
	}
	var accepted, resolved bool
	for _, cond := range parent.Conditions {
		if cond.ObservedGeneration != 0 && cond.ObservedGeneration != generation {
			continue // stale status from a previous generation
		}
		switch cond.Type {
		case string(gatewayv1.RouteConditionAccepted):
			accepted = cond.Status == metav1.ConditionTrue
		case string(gatewayv1.RouteConditionResolvedRefs):
			resolved = cond.Status == metav1.ConditionTrue
		}
	}
	return accepted && resolved
}

// routeParentFor returns the status.parents entry belonging to the named
// Gateway, matched by parentRef name and — when set — namespace. It is nil when
// the gateway has not reported on the route.
func routeParentFor(parents []gatewayv1.RouteParentStatus, gatewayName, gatewayNamespace string) *gatewayv1.RouteParentStatus {
	for i := range parents {
		if parents[i].ParentRef.Name != gatewayv1.ObjectName(gatewayName) {
			continue
		}
		if parents[i].ParentRef.Namespace != nil && string(*parents[i].ParentRef.Namespace) != gatewayNamespace {
			continue
		}
		return &parents[i]
	}
	return nil
}

// routeParentsTo reports whether a route's spec.parentRefs attach it to the
// named Gateway — the question routeParentFor answers for the parents status
// reports. The API's defaults apply to the fields left unset: a parentRef
// without a namespace means the route's own namespace, and one without a group
// or kind means a Gateway in the Gateway API group. A ref that names another
// resource — a Service of the same name, as a mesh route may parent to — is a
// different parent however it is spelled, and holds no listener here.
func routeParentsTo(refs []gatewayv1.ParentReference, routeNamespace, gatewayName, gatewayNamespace string) bool {
	for _, ref := range refs {
		group, kind := gatewayAPIGroup, gatewayKind
		if ref.Group != nil {
			group = string(*ref.Group)
		}
		if ref.Kind != nil {
			kind = string(*ref.Kind)
		}
		if group != gatewayAPIGroup || kind != gatewayKind {
			continue
		}
		if ref.Name != gatewayv1.ObjectName(gatewayName) {
			continue
		}
		namespace := routeNamespace
		if ref.Namespace != nil {
			namespace = string(*ref.Namespace)
		}
		if namespace == gatewayNamespace {
			return true
		}
	}
	return false
}

// listenerSetParentsToGateway reports whether a ListenerSet contributes its
// listeners to the named Gateway — the same question routeParentsTo answers,
// asked of the single parentRef a ListenerSet carries. It delegates rather than
// restating the rule so that a ListenerSet and a route cannot disagree about
// which Gateway they are attached to; only the ref type differs.
func listenerSetParentsToGateway(ls *gatewayv1.ListenerSet, gatewayName, gatewayNamespace string) bool {
	return routeParentsTo([]gatewayv1.ParentReference{{
		Group:     ls.Spec.ParentRef.Group,
		Kind:      ls.Spec.ParentRef.Kind,
		Name:      ls.Spec.ParentRef.Name,
		Namespace: ls.Spec.ParentRef.Namespace,
	}}, ls.Namespace, gatewayName, gatewayNamespace)
}

// endpointPort extracts the port from the reachable internal endpoint
// "<svc>.<ns>.svc:<port>" reported by checkEndpoint.
func endpointPort(internal string) (int32, error) {
	port, err := strconv.Atoi(internal[strings.LastIndex(internal, ":")+1:])
	if err != nil {
		return 0, err
	}
	return int32(port), nil
}

// routeErr maps an error from a route API call: a missing gateway-api CRD
// (NoMatchError) degrades gracefully to GatewayNotConfigured; any other
// failure is a hard error.
func routeErr(check *routeCheck, err error) (*routeCheck, error) {
	if meta.IsNoMatchError(err) {
		check.Reason = RouteGatewayNotConfigured
		return check, nil
	}
	check.Err = err
	return check, err
}

// routeErrAfterApply maps an error from the re-read that follows the apply: it
// reads through the client's cache, so right after a create it can miss an
// object the API server already holds — the cache has not observed it yet. An
// object that exists but cannot be read carries no acceptance status, which is
// GatewayNotAccepted rather than a failure of the pass.
func routeErrAfterApply(check *routeCheck, err error) (*routeCheck, error) {
	if apierrors.IsNotFound(err) {
		check.Reason = RouteGatewayNotAccepted
		return check, nil
	}
	return routeErr(check, err)
}

// setRouteReadyCondition sets the RouteReady condition from the check:
// publish=false is a valid state reported as True/NotPublished (the service
// simply did not request a public route); any other non-empty reason is a
// failure reported as False.
func setRouteReadyCondition(conditions *[]metav1.Condition, check *routeCheck) {
	if check.Reason == "" {
		meta.SetStatusCondition(conditions, metav1.Condition{
			Type:    aiv1alpha1.ConditionRouteReady,
			Status:  metav1.ConditionTrue,
			Reason:  "RouteReady",
			Message: "The public route is published to the gateway",
		})
		return
	}
	status := metav1.ConditionFalse
	message := fmt.Sprintf("The public route could not be published: %s", check.Reason)
	if check.Reason == RouteNotPublished {
		status = metav1.ConditionTrue
		message = "No public route requested: the service is not published"
	}
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:    aiv1alpha1.ConditionRouteReady,
		Status:  status,
		Reason:  check.Reason,
		Message: message,
	})
}
