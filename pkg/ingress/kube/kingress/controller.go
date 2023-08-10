// Copyright (c) 2022 Alibaba Group Holding Ltd.
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

package kingress

import (
	"fmt"
	"github.com/alibaba/higress/pkg/ingress/kube/annotations"
	"path"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	kube "github.com/alibaba/higress/pkg/kube"
	"github.com/hashicorp/go-multierror"
	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/model/credentials"
	"istio.io/istio/pilot/pkg/util/sets"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/protocol"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/kube/controllers"

	"github.com/alibaba/higress/pkg/ingress/kube/common"
	"github.com/alibaba/higress/pkg/ingress/kube/kingress/resources"
	"github.com/alibaba/higress/pkg/ingress/kube/secret"
	. "github.com/alibaba/higress/pkg/ingress/log"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	kset "k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers/networking/v1beta1"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	ingress "knative.dev/networking/pkg/apis/networking/v1alpha1"
	networkingv1alpha1 "knative.dev/networking/pkg/client/listers/networking/v1alpha1"
)

var (
	_ common.KIngressController = &controller{}
)

type controller struct {
	queue                   workqueue.RateLimitingInterface
	virtualServiceHandlers  []model.EventHandler
	gatewayHandlers         []model.EventHandler
	destinationRuleHandlers []model.EventHandler
	envoyFilterHandlers     []model.EventHandler

	options common.Options

	mutex sync.RWMutex
	// key: namespace/name
	ingresses map[string]*ingress.Ingress

	ingressInformer cache.SharedInformer
	ingressLister   networkingv1alpha1.IngressLister
	serviceInformer cache.SharedInformer
	serviceLister   listerv1.ServiceLister
	// May be nil if ingress class is not supported in the cluster
	classes v1beta1.IngressClassInformer

	secretController secret.SecretController
}

// NewController creates a new Kubernetes controller
func NewController(localKubeClient, client kube.Client, options common.Options,
	secretController secret.SecretController) common.KIngressController {
	q := workqueue.NewRateLimitingQueue(workqueue.DefaultItemBasedRateLimiter())

	//var namespace string = "default"
	ingressInformer := client.KingressInformer().Networking().V1alpha1().Ingresses()
	serviceInformer := client.KubeInformer().Core().V1().Services()

	var classes v1beta1.IngressClassInformer
	if common.NetworkingIngressAvailable(client) {
		classes = client.KubeInformer().Networking().V1beta1().IngressClasses()
		_ = classes.Informer()
	} else {
		IngressLog.Infof("Skipping IngressClass, resource not supported for cluster %s", options.ClusterId)
	}

	c := &controller{
		options:          options,
		queue:            q,
		ingresses:        make(map[string]*ingress.Ingress),
		ingressInformer:  ingressInformer.Informer(),
		ingressLister:    ingressInformer.Lister(),
		classes:          classes,
		serviceInformer:  serviceInformer.Informer(),
		serviceLister:    serviceInformer.Lister(),
		secretController: secretController,
	}

	handler := controllers.LatestVersionHandlerFuncs(controllers.EnqueueForSelf(q))
	c.ingressInformer.AddEventHandler(handler)

	return c
}

func (c *controller) ServiceLister() listerv1.ServiceLister {
	return c.serviceLister
}

func (c *controller) SecretLister() listerv1.SecretLister {
	return c.secretController.Lister()
}

func (c *controller) Run(stop <-chan struct{}) {
	go c.secretController.Run(stop)

	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	if !cache.WaitForCacheSync(stop, c.HasSynced) {
		IngressLog.Errorf("Failed to sync ingress controller cache for cluster %s", c.options.ClusterId)
		return
	}
	go wait.Until(c.worker, time.Second, stop)
	<-stop
}

func (c *controller) worker() {
	for c.processNextWorkItem() {
	}
}

func (c *controller) processNextWorkItem() bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(key)
	ingressNamespacedName := key.(types.NamespacedName)
	if err := c.onEvent(ingressNamespacedName); err != nil {
		IngressLog.Errorf("error processing ingress item (%v) (retrying): %v, cluster: %s", key, err, c.options.ClusterId)
		c.queue.AddRateLimited(key)
	} else {
		c.queue.Forget(key)
	}
	return true
}

func (c *controller) onEvent(namespacedName types.NamespacedName) error {
	event := model.EventUpdate
	ing, err := c.ingressLister.Ingresses(namespacedName.Namespace).Get(namespacedName.Name)
	if err != nil {
		if kerrors.IsNotFound(err) {
			event = model.EventDelete
			c.mutex.Lock()
			ing = c.ingresses[namespacedName.String()]
			delete(c.ingresses, namespacedName.String())
			c.mutex.Unlock()
		} else {
			return err
		}
	}

	// ingress deleted, and it is not processed before
	if ing == nil {
		return nil
	}

	// we should check need process only when event is not delete,
	// if it is delete event, and previously processed, we need to process too.
	if event != model.EventDelete {
		shouldProcess, err := c.shouldProcessIngressUpdate(ing)
		if err != nil {
			return err
		}
		if !shouldProcess {
			IngressLog.Infof("no need process, ingress %s", namespacedName)
			return nil
		}
	}

	drmetadata := config.Meta{
		Name:             ing.Name + "-" + "destinationrule",
		Namespace:        ing.Namespace,
		GroupVersionKind: gvk.DestinationRule,
		// Set this label so that we do not compare configs and just push.
		Labels: map[string]string{constants.AlwaysPushLabel: "true"},
	}
	vsmetadata := config.Meta{
		Name:             ing.Name + "-" + "virtualservice",
		Namespace:        ing.Namespace,
		GroupVersionKind: gvk.VirtualService,
		// Set this label so that we do not compare configs and just push.
		Labels: map[string]string{constants.AlwaysPushLabel: "true"},
	}
	efmetadata := config.Meta{
		Name:             ing.Name + "-" + "envoyfilter",
		Namespace:        ing.Namespace,
		GroupVersionKind: gvk.EnvoyFilter,
		// Set this label so that we do not compare configs and just push.
		Labels: map[string]string{constants.AlwaysPushLabel: "true"},
	}
	gatewaymetadata := config.Meta{
		Name:             ing.Name + "-" + "gateway",
		Namespace:        ing.Namespace,
		GroupVersionKind: gvk.Gateway,
		// Set this label so that we do not compare configs and just push.
		Labels: map[string]string{constants.AlwaysPushLabel: "true"},
	}

	for _, f := range c.destinationRuleHandlers {
		f(config.Config{Meta: drmetadata}, config.Config{Meta: drmetadata}, event)
	}

	for _, f := range c.virtualServiceHandlers {
		f(config.Config{Meta: vsmetadata}, config.Config{Meta: vsmetadata}, event)
	}

	for _, f := range c.envoyFilterHandlers {
		f(config.Config{Meta: efmetadata}, config.Config{Meta: efmetadata}, event)
	}

	for _, f := range c.gatewayHandlers {
		f(config.Config{Meta: gatewaymetadata}, config.Config{Meta: gatewaymetadata}, event)
	}

	return nil
}

func (c *controller) RegisterEventHandler(kind config.GroupVersionKind, f model.EventHandler) {
	switch kind {
	case gvk.VirtualService:
		c.virtualServiceHandlers = append(c.virtualServiceHandlers, f)
	case gvk.Gateway:
		c.gatewayHandlers = append(c.gatewayHandlers, f)
	case gvk.DestinationRule:
		c.destinationRuleHandlers = append(c.destinationRuleHandlers, f)
	case gvk.EnvoyFilter:
		c.envoyFilterHandlers = append(c.envoyFilterHandlers, f)
	}
}

func (c *controller) SetWatchErrorHandler(handler func(r *cache.Reflector, err error)) error {
	var errs error
	if err := c.serviceInformer.SetWatchErrorHandler(handler); err != nil {
		errs = multierror.Append(errs, err)
	}
	if err := c.ingressInformer.SetWatchErrorHandler(handler); err != nil {
		errs = multierror.Append(errs, err)
	}
	if err := c.secretController.Informer().SetWatchErrorHandler(handler); err != nil {
		errs = multierror.Append(errs, err)
	}
	if c.classes != nil {
		if err := c.classes.Informer().SetWatchErrorHandler(handler); err != nil {
			errs = multierror.Append(errs, err)
		}
	}
	return errs
}

func (c *controller) HasSynced() bool {
	return c.ingressInformer.HasSynced() && c.serviceInformer.HasSynced() &&
		(c.classes == nil || c.classes.Informer().HasSynced()) &&
		c.secretController.HasSynced()
}

func (c *controller) List() []config.Config {
	c.mutex.RLock()
	out := make([]config.Config, 0, len(c.ingresses))
	c.mutex.RUnlock()
	for _, raw := range c.ingressInformer.GetStore().List() {
		ing, ok := raw.(*ingress.Ingress)
		if !ok {
			continue
		}

		if should, err := c.shouldProcessIngress(ing); !should || err != nil {
			continue
		}

		copiedConfig := ing.DeepCopy()
		//setDefaultMSEIngressOptionalField(copiedConfig)

		outConfig := config.Config{
			Meta: config.Meta{
				Name:              copiedConfig.Name,
				Namespace:         copiedConfig.Namespace,
				Annotations:       common.CreateOrUpdateAnnotations(copiedConfig.Annotations, c.options),
				Labels:            copiedConfig.Labels,
				CreationTimestamp: copiedConfig.CreationTimestamp.Time,
			},
			Spec: copiedConfig.Spec,
		}

		out = append(out, outConfig)
	}

	common.RecordIngressNumber(c.options.ClusterId, len(out))
	return out
}

func extractTLSSecretName(host string, tls []ingress.IngressTLS) string {
	if len(tls) == 0 {
		return ""
	}

	for _, t := range tls {
		match := false
		for _, h := range t.Hosts {
			if h == host {
				match = true
			}
		}

		if match {
			return t.SecretName
		}
	}

	return ""
}

func (c *controller) ConvertGateway(convertOptions *common.ConvertOptions, wrapper *common.WrapperConfig) error {
	if convertOptions == nil {
		return fmt.Errorf("convertOptions is nil")
	}
	if wrapper == nil {
		return fmt.Errorf("wrapperConfig is nil")
	}

	// Ignore canary config.
	if wrapper.AnnotationsConfig.IsCanary() {
		return nil
	}
	cfg := wrapper.Config
	kingressv1alpha1, ok := cfg.Spec.(ingress.IngressSpec)

	if !ok {
		common.IncrementInvalidIngress(c.options.ClusterId, common.Unknown)
		return fmt.Errorf("convert type is invalid in cluster %s", c.options.ClusterId)
	}
	if len(kingressv1alpha1.Rules) == 0 {
		common.IncrementInvalidIngress(c.options.ClusterId, common.EmptyRule)
		return fmt.Errorf("invalid ingress rule %s:%s in cluster %s, `rules` must be specified", cfg.Namespace, cfg.Name, c.options.ClusterId)
	}

	//ruleHost: Kingress的rule可以对多条Host生效，此处多做一遍
	// 当Knative开启AutoTLS时 需要校验
	for _, rule := range kingressv1alpha1.Rules {
		for _, ruleHost := range rule.Hosts {
			cleanHost := common.CleanHost(ruleHost)
			// Need create builder for every rule.
			domainBuilder := &common.IngressDomainBuilder{
				ClusterId: c.options.ClusterId,
				Protocol:  common.HTTP,
				Host:      ruleHost,
				Ingress:   cfg,
				Event:     common.Normal,
			}

			// Extract the previous gateway and builder
			wrapperGateway, exist := convertOptions.Gateways[ruleHost]
			preDomainBuilder, _ := convertOptions.IngressDomainCache.Valid[ruleHost]
			if !exist {
				wrapperGateway = &common.WrapperGateway{
					Gateway:       &networking.Gateway{},
					WrapperConfig: wrapper,
					ClusterId:     c.options.ClusterId,
					Host:          ruleHost,
				}
				if c.options.GatewaySelectorKey != "" {
					wrapperGateway.Gateway.Selector = map[string]string{c.options.GatewaySelectorKey: c.options.GatewaySelectorValue}
				}

				wrapperGateway.Gateway.Servers = append(wrapperGateway.Gateway.Servers, &networking.Server{
					Port: &networking.Port{
						Number:   80,
						Protocol: string(protocol.HTTP),
						Name:     common.CreateConvertedName("http-80-ingress", c.options.ClusterId, cfg.Namespace, cfg.Name, cleanHost),
					},
					Hosts: []string{ruleHost},
				})

				// Add new gateway, builder
				convertOptions.Gateways[ruleHost] = wrapperGateway
				convertOptions.IngressDomainCache.Valid[ruleHost] = domainBuilder
			} else {
				// Fallback to get downstream tls from current ingress.
				if wrapperGateway.WrapperConfig.AnnotationsConfig.DownstreamTLS == nil {
					wrapperGateway.WrapperConfig.AnnotationsConfig.DownstreamTLS = wrapper.AnnotationsConfig.DownstreamTLS
				}
			}

			// There are no tls settings, so just skip.
			if len(kingressv1alpha1.TLS) == 0 {
				continue
			}

			// Get tls secret matching the rule host
			secretName := extractTLSSecretName(ruleHost, kingressv1alpha1.TLS)
			if secretName == "" {
				// There no matching secret, so just skip.
				continue
			}

			domainBuilder.Protocol = common.HTTPS
			domainBuilder.SecretName = path.Join(c.options.ClusterId, cfg.Namespace, secretName)

			// There is a matching secret and the gateway has already a tls secret.
			// We should report the duplicated tls secret event.
			if wrapperGateway.IsHTTPS() {
				domainBuilder.Event = common.DuplicatedTls
				domainBuilder.PreIngress = preDomainBuilder.Ingress
				convertOptions.IngressDomainCache.Invalid = append(convertOptions.IngressDomainCache.Invalid,
					domainBuilder.Build())
				continue
			}

			// Append https server
			wrapperGateway.Gateway.Servers = append(wrapperGateway.Gateway.Servers, &networking.Server{
				Port: &networking.Port{
					Number:   443,
					Protocol: string(protocol.HTTPS),
					Name:     common.CreateConvertedName("https-443-ingress", c.options.ClusterId, cfg.Namespace, cfg.Name, cleanHost),
				},
				Hosts: []string{ruleHost},
				Tls: &networking.ServerTLSSettings{
					Mode:           networking.ServerTLSSettings_SIMPLE,
					CredentialName: credentials.ToKubernetesIngressResource(c.options.RawClusterId, cfg.Namespace, secretName),
				},
			})

			// Update domain builder
			convertOptions.IngressDomainCache.Valid[ruleHost] = domainBuilder
		}

	}

	return nil
}

func (c *controller) ConvertHTTPRoute(convertOptions *common.ConvertOptions, wrapper *common.WrapperConfig) error {
	if convertOptions == nil {
		return fmt.Errorf("convertOptions is nil")
	}
	if wrapper == nil {
		return fmt.Errorf("wrapperConfig is nil")
	}

	cfg := wrapper.Config
	KingressV1, ok := cfg.Spec.(ingress.IngressSpec)
	if !ok {
		common.IncrementInvalidIngress(c.options.ClusterId, common.Unknown)
		return fmt.Errorf("convert type is invalid in cluster %s", c.options.ClusterId)
	}
	if len(KingressV1.Rules) == 0 {
		common.IncrementInvalidIngress(c.options.ClusterId, common.EmptyRule)
		return fmt.Errorf("invalid ingress rule %s:%s in cluster %s, `rules` must be specified", cfg.Namespace, cfg.Name, c.options.ClusterId)
	}
	convertOptions.HasDefaultBackend = false
	// In one ingress, we will limit the rule conflict.
	// When the host, pathType, path of two rule are same, we think there is a conflict event.
	definedRules := sets.NewSet()

	var (
		// But in across ingresses case, we will restrict this limit.
		// When the {host, path, headers, method, params} of two rule in different ingress are same, we think there is a conflict event.
		tempRuleKey []string
	)

	for _, rule := range KingressV1.Rules {
		for _, rulehost := range rule.Hosts {
			if rule.HTTP == nil || len(rule.HTTP.Paths) == 0 {
				IngressLog.Warnf("invalid ingress rule %s:%s for host %q in cluster %s, no paths defined", cfg.Namespace, cfg.Name, rulehost, c.options.ClusterId)
				continue
			}
			wrapperVS, exist := convertOptions.VirtualServices[rulehost]
			if !exist {
				wrapperVS = &common.WrapperVirtualService{
					VirtualService: &networking.VirtualService{
						Hosts: []string{rulehost},
					},
					WrapperConfig: wrapper,
				}
				convertOptions.VirtualServices[rulehost] = wrapperVS
			}
			wrapperHttpRoutes := make([]*common.WrapperHTTPRoute, 0, len(rule.HTTP.Paths))
			for _, httpPath := range rule.HTTP.Paths {
				wrapperHttpRoute := &common.WrapperHTTPRoute{
					HTTPRoute:     &networking.HTTPRoute{},
					WrapperConfig: wrapper,
					Host:          rulehost,
					ClusterId:     c.options.ClusterId,
				}

				var pathType common.PathType
				originPath := httpPath.Path
				pathType = common.Prefix
				wrapperHttpRoute.OriginPath = originPath
				wrapperHttpRoute.OriginPathType = pathType
				wrapperHttpRoute.HTTPRoute = resources.MakeVirtualServiceRoute(transformHosts(rulehost), &httpPath)
				wrapperHttpRoute.HTTPRoute.Name = common.GenerateUniqueRouteName(c.options.SystemNamespace, wrapperHttpRoute)
				ingressRouteBuilder := convertOptions.IngressRouteCache.New(wrapperHttpRoute)
				//fmt.Print(wrapperHttpRoute.WrapperConfig.Config.Namespace, c.options.SystemNamespace, "11111111111111")
				hostAndPath := wrapperHttpRoute.PathFormat()
				key := createRuleKey(cfg.Annotations, hostAndPath)
				wrapperHttpRoute.RuleKey = key
				if WrapPreIngress, exist := convertOptions.Route2Ingress[key]; exist {
					ingressRouteBuilder.PreIngress = WrapPreIngress.Config
					ingressRouteBuilder.Event = common.DuplicatedRoute
				}
				tempRuleKey = append(tempRuleKey, key)

				// Two duplicated rules in the same ingress.
				if ingressRouteBuilder.Event == common.Normal {
					pathFormat := wrapperHttpRoute.PathFormat()
					if definedRules.Contains(pathFormat) {
						ingressRouteBuilder.PreIngress = cfg
						ingressRouteBuilder.Event = common.DuplicatedRoute
					}
					definedRules.Insert(pathFormat)
				}

				// backend service check
				var event common.Event
				destinationConfig := wrapper.AnnotationsConfig.Destination
				event = c.IngressRouteBuilderServicesCheck(&httpPath, cfg.Namespace, ingressRouteBuilder, destinationConfig)

				if destinationConfig != nil {
					wrapperHttpRoute.WeightTotal = int32(destinationConfig.WeightSum)
				}

				if ingressRouteBuilder.Event != common.Normal {
					event = ingressRouteBuilder.Event
				}

				if event != common.Normal {
					common.IncrementInvalidIngress(c.options.ClusterId, event)
					ingressRouteBuilder.Event = event
				} else {
					wrapperHttpRoutes = append(wrapperHttpRoutes, wrapperHttpRoute)
				}
				convertOptions.IngressRouteCache.Add(ingressRouteBuilder)
			}

			for idx, item := range tempRuleKey {
				if val, exist := convertOptions.Route2Ingress[item]; !exist || strings.Compare(val.RuleKey, tempRuleKey[idx]) != 0 {
					convertOptions.Route2Ingress[item] = &common.WrapperConfigWithRuleKey{
						Config:  cfg,
						RuleKey: tempRuleKey[idx],
					}
				}
			}

			old, f := convertOptions.HTTPRoutes[rulehost]
			if f {
				old = append(old, wrapperHttpRoutes...)
				convertOptions.HTTPRoutes[rulehost] = old
			} else {
				convertOptions.HTTPRoutes[rulehost] = wrapperHttpRoutes
			}

			// Sort, exact -> prefix -> regex
			routes := convertOptions.HTTPRoutes[rulehost]
			IngressLog.Debugf("routes of host %s is %v", rulehost, routes)
			common.SortHTTPRoutes(routes)
			for i, route := range routes {
				fmt.Print("number", i, "----HTTPRoute-----", route.HTTPRoute, "\n")
			}

		}

	}
	return nil
}
func (c *controller) IngressRouteBuilderServicesCheck(httppath *ingress.HTTPIngressPath, namespace string,
	builder *common.IngressRouteBuilder, config *annotations.DestinationConfig) common.Event {

	//builder.PortName弃用，knative ingress中每条HTTPIngressPath配置了不止一条backend。
	//builder.PortName = backend.ServicePort.StrVal
	if httppath.Splits == nil {
		return common.InvalidBackendService
	}
	for _, split := range httppath.Splits {
		//删除此处MCPDestination的限制，不知道KnativeServing可否配到注册配置中心上。
		if split.ServiceName == "" {
			return common.InvalidBackendService
		}
		new1 := model.BackendService{
			Namespace: namespace,
			Name:      split.ServiceName,
			Port:      uint32(split.ServicePort.IntValue()),
			Weight:    int32(split.Percent),
		}
		builder.ServiceList = append(builder.ServiceList, new1)
	}
	return common.Normal
}

func (c *controller) ConvertTrafficPolicy(convertOptions *common.ConvertOptions, wrapper *common.WrapperConfig) error {
	if convertOptions == nil {
		return fmt.Errorf("convertOptions is nil")
	}
	if wrapper == nil {
		return fmt.Errorf("wrapperConfig is nil")
	}

	if !wrapper.AnnotationsConfig.NeedTrafficPolicy() {
		return nil
	}

	return nil
}

func resolveNamedPort(backend *ingress.IngressBackend, namespace string, serviceLister listerv1.ServiceLister) (int32, error) {
	if backend == nil {
		return 0, fmt.Errorf("ingressBackend is nil")
	}

	svc, err := serviceLister.Services(namespace).Get(backend.ServiceName)
	if err != nil {
		return 0, err
	}
	for _, port := range svc.Spec.Ports {
		if port.Name == backend.ServicePort.StrVal {
			return port.Port, nil
		}
	}
	return 0, common.ErrNotFound
}

/*  这里的操作就是根据backend产生RouteDestination。 这个在MakeVirturlServiceRoute中已完成。
func (c *controller) backendToRouteDestination(backend *ingress.IngressBackend, namespace string,
	builder *common.IngressRouteBuilder, config *annotations.DestinationConfig) ([]*networking.HTTPRouteDestination, common.Event) {
}
*/

// Kingress 定义IngressClass资源类型检查在annotation里面，此处做IngressClass的校验
const (
	// ClassAnnotationKey points to the annotation for the class of this resource.
	ClassAnnotationKey    = "networking.knative.dev/ingress.class"
	IstioIngressClassName = "istio.ingress.networking.knative.dev"
)

func (c *controller) shouldProcessIngressWithClass(ing *ingress.Ingress) bool {
	if classValue, found := ing.GetAnnotations()[ClassAnnotationKey]; !found || classValue != IstioIngressClassName {
		IngressLog.Debugf("Ingress class %s does not match knative IngressCLassName %s.", ClassAnnotationKey, classValue+"!="+IstioIngressClassName)
		return false
	}
	return true
}

func (c *controller) shouldProcessIngress(i *ingress.Ingress) (bool, error) {
	//check namespace
	if c.shouldProcessIngressWithClass(i) {
		switch c.options.WatchNamespace {
		case "":
			return true, nil
		default:
			return c.options.WatchNamespace == i.Namespace, nil
		}
	}
	return false, nil

}

// shouldProcessIngressUpdate checks whether we should renotify registered handlers about an update event
func (c *controller) shouldProcessIngressUpdate(ing *ingress.Ingress) (bool, error) {
	shouldProcess, err := c.shouldProcessIngress(ing)
	if err != nil {
		return false, err
	}

	namespacedName := ing.Namespace + "/" + ing.Name
	if shouldProcess {
		// record processed ingress
		c.mutex.Lock()
		preConfig, exist := c.ingresses[namespacedName]
		c.ingresses[namespacedName] = ing
		c.mutex.Unlock()

		// We only care about annotations, labels and spec.
		if exist {
			if !reflect.DeepEqual(preConfig.Annotations, ing.Annotations) {
				IngressLog.Debugf("Annotations of ingress %s changed, should process.", namespacedName)
				return true, nil
			}
			if !reflect.DeepEqual(preConfig.Labels, ing.Labels) {
				IngressLog.Debugf("Labels of ingress %s changed, should process.", namespacedName)
				return true, nil
			}
			if !reflect.DeepEqual(preConfig.Spec, ing.Spec) {
				IngressLog.Debugf("Spec of ingress %s changed, should process.", namespacedName)
				return true, nil
			}

			return false, nil
		}
		IngressLog.Debugf("First receive relative ingress %s, should process.", namespacedName)
		return true, nil
	}

	c.mutex.Lock()
	_, preProcessed := c.ingresses[namespacedName]
	// previous processed but should not currently, delete it
	if preProcessed && !shouldProcess {
		delete(c.ingresses, namespacedName)
	}
	c.mutex.Unlock()

	return preProcessed, nil
}

func shouldReconcileTLS(ing *ingress.Ingress) bool {
	return isIngressPublic(ing) && len(ing.Spec.TLS) > 0
}

func shouldReconcileHTTPServer(ing *ingress.Ingress) bool {
	// We will create a Ingress specific HTTPServer when
	// 1. auto TLS is enabled as in this case users want us to fully handle the TLS/HTTP behavior,
	// 2. HTTPOption is set to Redirected as we don't have default HTTP server supporting HTTP redirection.
	return isIngressPublic(ing) && (ing.Spec.HTTPOption == ingress.HTTPOptionRedirected || len(ing.Spec.TLS) > 0)
}

func isIngressPublic(ing *ingress.Ingress) bool {
	for _, rule := range ing.Spec.Rules {
		if rule.Visibility == ingress.IngressVisibilityExternalIP {
			return true
		}
	}
	return false
}

//use MakeMatch from net-istio
/*
func (c *controller) generateHttpMatches(pathType common.PathType, path string, wrapperVS *common.WrapperVirtualService) []*networking.HTTPMatchRequest {
}*/

// setDefaultMSEIngressOptionalField sets a default value for optional fields when is not defined.
/*
func setDefaultMSEIngressOptionalField(ing *ingress.Ingress) {

}*/

// createRuleKey according to the pathType, path, methods, headers, params of rules
func createRuleKey(annots map[string]string, hostAndPath string) string {
	var (
		headers [][2]string
		params  [][2]string
		sb      strings.Builder
	)

	sep := "\n\n"

	// path
	sb.WriteString(hostAndPath)
	sb.WriteString(sep)

	// methods
	if str, ok := annots[annotations.HigressAnnotationsPrefix+"/"+annotations.MatchMethod]; ok {
		sb.WriteString(str)
	}
	sb.WriteString(sep)

	start := len(annotations.HigressAnnotationsPrefix) + 1 // example: higress.io/exact-match-header-key: value
	// headers && params
	for k, val := range annots {
		if idx := strings.Index(k, annotations.MatchHeader); idx != -1 {
			key := k[start:idx] + k[idx+len(annotations.MatchHeader)+1:]
			headers = append(headers, [2]string{key, val})
		}
		if idx := strings.Index(k, annotations.MatchQuery); idx != -1 {
			key := k[start:idx] + k[idx+len(annotations.MatchQuery)+1:]
			params = append(params, [2]string{key, val})
		}
	}
	sort.SliceStable(headers, func(i, j int) bool {
		return headers[i][0] < headers[j][0]
	})
	sort.SliceStable(params, func(i, j int) bool {
		return params[i][0] < params[j][0]
	})
	for idx := range headers {
		if idx != 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(headers[idx][0])
		sb.WriteByte('\t')
		sb.WriteString(headers[idx][1])
	}
	sb.WriteString(sep)
	for idx := range params {
		if idx != 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(params[idx][0])
		sb.WriteByte('\t')
		sb.WriteString(params[idx][1])
	}
	sb.WriteString(sep)

	return sb.String()
}

func transformHosts(host string) kset.String {
	hosts := []string{host}
	out := kset.NewString()
	out.Insert(hosts...)
	return out
}
