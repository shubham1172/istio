package gateway

import (
	"fmt"
	"sync"
	"time"

	"go.uber.org/atomic"
	"istio.io/istio/pilot/pkg/credentials"
	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/model/kstatus"
	kubecontroller "istio.io/istio/pilot/pkg/serviceregistry/kube/controller"
	"istio.io/istio/pilot/pkg/status"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/labels"
	"istio.io/istio/pkg/config/schema/collection"
	"istio.io/istio/pkg/config/schema/collections"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/config/schema/gvr"
	"istio.io/istio/pkg/config/schema/kind"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/kube/kclient"
	istiolog "istio.io/istio/pkg/log"
	"istio.io/istio/pkg/util/sets"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8sv1 "sigs.k8s.io/gateway-api/apis/v1"
)

var logV2 = istiolog.RegisterScope("gateway_v2", "gateway-api controller v2")

var errUnsupportedOpV2 = fmt.Errorf("unsupported operation: the gateway config store is a read-only view")

type ControllerV2 struct {
	// client for accessing Kubernetes
	client kube.Client

	queue                  controllers.Queue
	virtualServiceHandlers []model.EventHandler
	gatewayHandlers        []model.EventHandler

	// state is our computed Istio resources. Access is guarded by stateMu.
	// This is updated from Reconcile().
	state   IstioResources
	stateMu sync.RWMutex

	// Gateway-api types reference namespace labels directly, so we need access to these
	namespaces       kclient.Client[*corev1.Namespace]
	namespaceHandler model.EventHandler

	// Gateway-api types reference secrets directly, so we need access to these
	credentialsController credentials.MulticlusterController
	secretHandler         model.EventHandler

	// TODO: Add more Gateway API resources.
	gatewayClasses kclient.Client[*k8sv1.GatewayClass]
	gateways       kclient.Client[*k8sv1.Gateway]
	httpRoutes     kclient.Client[*k8sv1.HTTPRoute]

	// statusController controls the status working queue. Status will only be written if statusEnabled is true, which
	// is only the case when we are the leader.
	statusController *status.Controller
	statusEnabled    *atomic.Bool

	waitForCRD func(class schema.GroupVersionResource, stop <-chan struct{}) bool
}

func NewControllerV2(
	kc kube.Client,
	options kubecontroller.Options,
	credsController credentials.MulticlusterController,
	waitForCRD func(class schema.GroupVersionResource, stop <-chan struct{}) bool,
) model.ConfigStoreController {
	var ctl *status.Controller

	namespaces := kclient.New[*corev1.Namespace](kc)
	gatewayClasses := kclient.New[*k8sv1.GatewayClass](kc)
	gateways := kclient.NewFiltered[*k8sv1.Gateway](kc, kclient.Filter{ObjectFilter: options.GetFilter()})
	httpRoutes := kclient.NewFiltered[*k8sv1.HTTPRoute](kc, kclient.Filter{ObjectFilter: options.GetFilter()})

	c := &ControllerV2{
		client:                kc,
		namespaces:            namespaces,
		credentialsController: credsController,
		gatewayClasses:        gatewayClasses,
		gateways:              gateways,
		httpRoutes:            httpRoutes,
		statusController:      ctl,
		// Disabled by default, we will enable only if we win the leader election
		statusEnabled: atomic.NewBool(false),
		waitForCRD:    waitForCRD,
	}

	c.queue = controllers.NewQueue("gateway-api",
		controllers.WithReconciler(c.OnEvent),
		controllers.WithMaxAttempts(25))

	c.gatewayClasses.AddEventHandler(controllers.ObjectHandler(c.queue.AddObject))
	c.gateways.AddEventHandler(controllers.ObjectHandler(c.queue.AddObject))
	c.httpRoutes.AddEventHandler(controllers.ObjectHandler(c.queue.AddObject))

	c.namespaces.AddEventHandler(controllers.EventHandler[*corev1.Namespace]{
		UpdateFunc: func(oldNs, newNs *corev1.Namespace) {
			if options.DiscoveryNamespacesFilter != nil && !options.DiscoveryNamespacesFilter.Filter(newNs) {
				return
			}
			if !labels.Instance(oldNs.Labels).Equals(newNs.Labels) {
				c.namespaceEvent(oldNs, newNs)
			}
		},
	})

	if credsController != nil {
		credsController.AddSecretHandler(c.secretEvent)
	}

	return c
}

func (c *ControllerV2) OnEvent(item types.NamespacedName) error {
	t0 := time.Now()
	defer func() {
		logV2.Debugf("reconcile complete in %v", time.Since(t0))
	}()

	// gatewayClass := c.gatewayClasses.List(metav1.NamespaceAll)
	// gateway := c.cache.List(gvk.KubernetesGateway, metav1.NamespaceAll)
	// httpRoute := c.cache.List(gvk.HTTPRoute, metav1.NamespaceAll)

	event := model.EventUpdate

	output := convertResources(input)

	// Handle all status updates
	c.queueStatusUpdates(input)

	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.state = output
	return nil
}

func (c *ControllerV2) queueStatusUpdates(r GatewayResources) {
	c.handleStatusUpdates(r.GatewayClass)
	c.handleStatusUpdates(r.Gateway)
	c.handleStatusUpdates(r.HTTPRoute)
	// TODO: Add more Gateway API resources.
	// c.handleStatusUpdates(r.GRPCRoute)
	// c.handleStatusUpdates(r.TCPRoute)
	// c.handleStatusUpdates(r.TLSRoute)
}

func (c *ControllerV2) handleStatusUpdates(configs []config.Config) {
	if c.statusController == nil || !c.statusEnabled.Load() {
		return
	}
	for _, cfg := range configs {
		ws := cfg.Status.(*kstatus.WrappedStatus)
		if ws.Dirty {
			res := status.ResourceFromModelConfig(cfg)
			c.statusController.EnqueueStatusUpdateResource(ws.Unwrap(), res)
		}
	}
}

func (c *ControllerV2) SetStatusWrite(enabled bool, statusManager *status.Manager) {
	c.statusEnabled.Store(enabled)
	if enabled && features.EnableGatewayAPIGatewayClassControllerV2 && statusManager != nil {
		c.statusController = statusManager.CreateGenericController(func(status any, context any) status.GenerationProvider {
			return &gatewayGeneration{context}
		})
	} else {
		c.statusController = nil
	}
}

func (c *ControllerV2) List(typ config.GroupVersionKind, namespace string) []config.Config {
	if typ != gvk.Gateway && typ != gvk.VirtualService {
		return nil
	}

	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	switch typ {
	case gvk.Gateway:
		return filterNamespace(c.state.Gateway, namespace)
	case gvk.VirtualService:
		return filterNamespace(c.state.VirtualService, namespace)
	default:
		return nil
	}
}

// RegisterEventHandler implements model.ConfigStoreController.
func (c *ControllerV2) RegisterEventHandler(kind config.GroupVersionKind, f model.EventHandler) {
	switch kind {
	case gvk.Namespace:
		c.namespaceHandler = f
	case gvk.Secret:
		c.secretHandler = f
	case gvk.Gateway:
		c.gatewayHandlers = append(c.gatewayHandlers, f)
	case gvk.VirtualService:
		c.virtualServiceHandlers = append(c.virtualServiceHandlers, f)
	}
}

func (c *ControllerV2) Run(stop <-chan struct{}) {
	if features.EnableGatewayAPIGatewayClassControllerV2 {
		if c.waitForCRD(gvr.GatewayClass, stop) {
			c.client.RunAndWait(stop)
			kube.WaitForCacheSync("gatewayV2", stop, c.gatewayClasses.HasSynced, c.gateways.HasSynced, c.httpRoutes.HasSynced)
			c.queue.Run(stop)
			controllers.ShutdownAll(c.gatewayClasses, c.gateways, c.httpRoutes)
		}
	}
}

func (*ControllerV2) Schemas() collection.Schemas {
	return collection.SchemasFor(
		collections.VirtualService,
		collections.Gateway,
	)
}

func (c *ControllerV2) HasSynced() bool {
	return c.queue.HasSynced() && c.namespaces.HasSynced()
}

func (*ControllerV2) Get(typ config.GroupVersionKind, name string, namespace string) *config.Config {
	return nil
}

func (c *ControllerV2) Create(config config.Config) (revision string, err error) {
	return "", errUnsupportedOpV2
}

func (c *ControllerV2) Update(config config.Config) (newRevision string, err error) {
	return "", errUnsupportedOpV2
}

func (c *ControllerV2) UpdateStatus(config config.Config) (newRevision string, err error) {
	return "", errUnsupportedOpV2
}

func (c *ControllerV2) Patch(orig config.Config, patchFn config.PatchFunc) (string, error) {
	return "", errUnsupportedOpV2
}

func (c *ControllerV2) Delete(typ config.GroupVersionKind, name, namespace string, _ *string) error {
	return errUnsupportedOpV2
}

// namespaceEvent handles a namespace add/update. Gateway's can select routes by label, so we need to handle
// when the labels change.
// Note: we don't handle delete as a delete would also clean up any relevant gateway-api types which will
// trigger its own event.
func (c *ControllerV2) namespaceEvent(oldNs, newNs *corev1.Namespace) {
	// First, find all the label keys on the old/new namespace. We include NamespaceNameLabel
	// since we have special logic to always allow this on namespace.
	touchedNamespaceLabels := sets.New(NamespaceNameLabel)
	touchedNamespaceLabels.InsertAll(getLabelKeys(oldNs)...)
	touchedNamespaceLabels.InsertAll(getLabelKeys(newNs)...)

	// Next, we find all keys our Gateways actually reference.
	c.stateMu.RLock()
	intersection := touchedNamespaceLabels.Intersection(c.state.ReferencedNamespaceKeys)
	c.stateMu.RUnlock()

	// If there was any overlap, then a relevant namespace label may have changed, and we trigger a
	// push. A more exact check could actually determine if the label selection result actually changed.
	// However, this is a much simpler approach that is likely to scale well enough for now.
	if !intersection.IsEmpty() && c.namespaceHandler != nil {
		logV2.Debugf("namespace labels changed, triggering namespace handler: %v", intersection.UnsortedList())
		c.namespaceHandler(config.Config{}, config.Config{}, model.EventUpdate)
	}
}

func (c *ControllerV2) secretEvent(name, namespace string) {
	var impactedConfigs []model.ConfigKey
	c.stateMu.RLock()
	impactedConfigs = c.state.ResourceReferences[model.ConfigKey{
		Kind:      kind.Secret,
		Namespace: namespace,
		Name:      name,
	}]
	c.stateMu.RUnlock()
	if len(impactedConfigs) > 0 {
		logV2.Debugf("secret %s/%s changed, triggering secret handler", namespace, name)
		for _, cfg := range impactedConfigs {
			gw := config.Config{
				Meta: config.Meta{
					GroupVersionKind: gvk.KubernetesGateway,
					Namespace:        cfg.Namespace,
					Name:             cfg.Name,
				},
			}
			c.secretHandler(gw, gw, model.EventUpdate)
		}
	}
}
