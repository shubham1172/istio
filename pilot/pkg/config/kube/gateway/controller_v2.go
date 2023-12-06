package gateway

import (
	"fmt"
	"sync"

	"istio.io/istio/pilot/pkg/model"
	kubecontroller "istio.io/istio/pilot/pkg/serviceregistry/kube/controller"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/schema/collection"
	"istio.io/istio/pkg/config/schema/collections"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/config/schema/gvr"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/kube/kclient"
	istiolog "istio.io/istio/pkg/log"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8sv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

var logV2 = istiolog.RegisterScope("gateway_v2", "gateway-api controller v2")

var errUnsupportedOpV2 = fmt.Errorf("unsupported operation: the gateway config store is a read-only view")

type ControllerV2 struct {
	// Client for accessing Kubernetes
	client kube.Client

	queue                  controllers.Queue
	virtualServiceHandlers []model.EventHandler
	gatewayHandlers        []model.EventHandler

	// Processed Gateways
	gatewaysMMu sync.RWMutex
	gatewaysM   map[types.NamespacedName]*k8sv1beta1.Gateway

	// TODO: these are not used yet, but will be needed.
	// // Gateway API types reference namespaces directly
	// namespaces       kclient.Client[*corev1.Namespace]
	// namespaceHandler model.EventHandler

	// // Gateway-api types reference secrets directly
	// credentialsController credentials.MulticlusterController
	// secretHandler         model.EventHandler

	// TODO: Add more Gateway API resources.
	gatewayClasses kclient.Client[*k8sv1beta1.GatewayClass]
	gateways       kclient.Client[*k8sv1beta1.Gateway]
	httpRoutes     kclient.Client[*k8sv1beta1.HTTPRoute]

	// TODO: enable status reporting
	// // statusController controls the status working queue. Status will only be written if statusEnabled is true, which
	// // is only the case when we are the leader.
	// statusController *status.Controller
	// statusEnabled    *atomic.Bool

	waitForCRD func(class schema.GroupVersionResource, stop <-chan struct{}) bool
}

func NewControllerV2(
	kc kube.Client,
	options kubecontroller.Options,
	waitForCRD func(class schema.GroupVersionResource, stop <-chan struct{}) bool,
) model.ConfigStoreController {
	logV2.Warnf("NewControllerV2 called")

	gatewayClasses := kclient.New[*k8sv1beta1.GatewayClass](kc)
	gateways := kclient.NewFiltered[*k8sv1beta1.Gateway](kc, kclient.Filter{ObjectFilter: options.GetFilter()})
	httpRoutes := kclient.NewFiltered[*k8sv1beta1.HTTPRoute](kc, kclient.Filter{ObjectFilter: options.GetFilter()})

	c := &ControllerV2{
		client:         kc,
		gatewaysM:      make(map[types.NamespacedName]*k8sv1beta1.Gateway),
		gatewayClasses: gatewayClasses,
		gateways:       gateways,
		httpRoutes:     httpRoutes,
		// Disabled by default, we will enable only if we win the leader election
		waitForCRD: waitForCRD,
	}

	c.queue = controllers.NewQueue("gateway-api",
		controllers.WithReconciler(c.OnEvent),
		controllers.WithMaxAttempts(5))

	c.gateways.AddEventHandler(controllers.ObjectHandler(c.queue.AddObject))
	c.httpRoutes.AddEventHandler(controllers.FromEventHandler(func(o controllers.Event) {
		c.onHttpRouteEvent(o)
	}))

	return c
}

func (c *ControllerV2) onHttpRouteEvent(event controllers.Event) {
	logV2.Warnf("onHttpRouteEvent old: %+v, new: %+v", event.Old, event.New)

	// TODO, think more about this.
	// At this point, updates to HTTP routes trigger
	// updates to the Gateway.
	curHttpRoute := event.Latest().(*k8sv1beta1.HTTPRoute)

	// If curHttpRoute.Spec.ParentRefs contains a Gateway, we need to process it.
	if curHttpRoute.Spec.ParentRefs != nil {
		for _, ref := range curHttpRoute.Spec.ParentRefs {
			logV2.Warnf("HTTPRoute %s/%s has parent ref %s/%s with kind %s", curHttpRoute.Namespace, curHttpRoute.Name, ref.Namespace, ref.Name, ref.Kind)
			if ref.Kind == (*k8sv1beta1.Kind)(&gvk.Gateway.Kind) {
				gw := c.gateways.Get(string(ref.Name), string(*ref.Namespace))
				if gw != nil {
					c.queue.AddObject(gw)
				}
			}
		}
	}

}

// OnEvent always receives a Gateway item.
func (c *ControllerV2) OnEvent(item types.NamespacedName) error {
	logV2.Warnf("OnEvent %s/%s", item.Namespace, item.Name)

	event := model.EventUpdate
	gw := c.gateways.Get(item.Name, item.Namespace)
	if gw == nil {
		event = model.EventDelete
		c.gatewaysMMu.Lock()
		gw = c.gatewaysM[item]
		delete(c.gatewaysM, item)
		c.gatewaysMMu.Unlock()
		if gw == nil {
			// This is a delete event and we didn't have an existing known gateway, no action.
			return nil
		}
	}

	// If this is a delete event for a known gateway, needs processing.
	// If this an update event, we need to check if the gateway needs processing.
	if event != model.EventDelete && !c.shouldProcessGatewayUpdate(gw) {
		logV2.Warnf("Gateway %s/%s is not for this controller, no action", gw.Namespace, gw.Name)
		return nil
	}

	vsmetadata := config.Meta{
		Name:             item.Name + "-" + "virtualservice",
		Namespace:        item.Namespace,
		GroupVersionKind: gvk.VirtualService,
		// Set this label so that we do not compare configs and just push.
		Labels: map[string]string{constants.AlwaysPushLabel: "true"},
	}
	gatewaymetadata := config.Meta{
		Name:             item.Name + "-" + "gateway",
		Namespace:        item.Namespace,
		GroupVersionKind: gvk.Gateway,
		// Set this label so that we do not compare configs and just push.
		Labels: map[string]string{constants.AlwaysPushLabel: "true"},
	}

	// Trigger updates for Gateway and VirtualService
	// TODO: we could be smarter here and only trigger when real changes were found
	for _, f := range c.virtualServiceHandlers {
		f(config.Config{Meta: vsmetadata}, config.Config{Meta: vsmetadata}, event)
	}
	for _, f := range c.gatewayHandlers {
		f(config.Config{Meta: gatewaymetadata}, config.Config{Meta: gatewaymetadata}, event)
	}

	return nil
}

func (c *ControllerV2) shouldProcessGatewayUpdate(gw *k8sv1beta1.Gateway) bool {
	className := string(gw.Spec.GatewayClassName)

	// No gateway class found, this maybe meant for another controller, no action.
	if className == "" {
		logV2.Warnf("Gateway %s/%s has no gateway class, no action", gw.Namespace, gw.Name)
		return false
	}

	// TODO: perform other checks here, see conversion.go.
	gwc := c.gatewayClasses.Get(className, "")
	// Invalid gateway class, no action.
	return gwc != nil
}

func (c *ControllerV2) List(typ config.GroupVersionKind, namespace string) []config.Config {
	logV2.Warnf("List %s/%s", typ, namespace)
	if typ != gvk.Gateway && typ != gvk.VirtualService {
		return nil
	}

	out := make([]config.Config, 0)
	gateways := c.gateways.List("", labels.Everything())
	// httpRoutes := c.httpRoutes.List("", labels.Everything())

	for _, gw := range gateways {
		processGw := c.shouldProcessGatewayUpdate(gw)
		if !processGw {
			continue
		}

		// switch typ {
		// case gvk.Gateway:
		// 	out = append(out, ConvertGatewayIstioGateway(gw, httpRoutes))
		// case gvk.VirtualService:
		// 	out = append(out, ConvertGatewayVirtualService(gw, httpRoutes))
		// }
	}

	return out
}

// func ConvertGatewayVirtualService(gw *k8sv1.Gateway, httpRoutes []*k8sv1.HTTPRoute) config.Config {
// 	return nil
// }

// func ConvertGatewayIstioGateway(gw *k8sv1.Gateway, httpRoutes []*k8sv1.HTTPRoute) config.Config {
// 	return nil
// }

// RegisterEventHandler implements model.ConfigStoreController.
func (c *ControllerV2) RegisterEventHandler(kind config.GroupVersionKind, f model.EventHandler) {
	logV2.Warnf("RegisterEventHandler %s", kind)

	switch kind {
	case gvk.Gateway:
		c.gatewayHandlers = append(c.gatewayHandlers, f)
	case gvk.VirtualService:
		c.virtualServiceHandlers = append(c.virtualServiceHandlers, f)
	}
}

func (c *ControllerV2) Run(stop <-chan struct{}) {
	if c.waitForCRD(gvr.GatewayClass, stop) {
		c.client.RunAndWait(stop)
		kube.WaitForCacheSync("gatewayV2", stop, c.gatewayClasses.HasSynced, c.gateways.HasSynced, c.httpRoutes.HasSynced)
		c.queue.Run(stop)
		controllers.ShutdownAll(c.gatewayClasses, c.gateways, c.httpRoutes)
	}
}

func (*ControllerV2) Schemas() collection.Schemas {
	return collection.SchemasFor(
		collections.VirtualService,
		collections.Gateway,
	)
}

func (c *ControllerV2) HasSynced() bool {
	return c.queue.HasSynced()
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
