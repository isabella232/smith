package app

import (
	"time"

	"github.com/atlassian/ctrl"
	"github.com/atlassian/smith"
	smith_v1 "github.com/atlassian/smith/pkg/apis/smith/v1"
	"github.com/atlassian/smith/pkg/client"
	smithClientset "github.com/atlassian/smith/pkg/client/clientset_generated/clientset"
	"github.com/atlassian/smith/pkg/client/smart"
	"github.com/atlassian/smith/pkg/controller/bundlec"
	"github.com/atlassian/smith/pkg/plugin"
	"github.com/atlassian/smith/pkg/specchecker"
	specchecker_builtin "github.com/atlassian/smith/pkg/specchecker/builtin"
	"github.com/atlassian/smith/pkg/statuschecker"
	statuschecker_builtin "github.com/atlassian/smith/pkg/statuschecker/builtin"
	"github.com/atlassian/smith/pkg/store"
	sc_v1b1 "github.com/kubernetes-sigs/service-catalog/pkg/apis/servicecatalog/v1beta1"
	scClientset "github.com/kubernetes-sigs/service-catalog/pkg/client/clientset_generated/clientset"
	sc_v1b1inf "github.com/kubernetes-sigs/service-catalog/pkg/client/informers_generated/externalversions/servicecatalog/v1beta1"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	apps_v1 "k8s.io/api/apps/v1"
	autoscaling_v2b1 "k8s.io/api/autoscaling/v2beta1"
	core_v1 "k8s.io/api/core/v1"
	net_v1b1 "k8s.io/api/networking/v1beta1"
	policy_v1 "k8s.io/api/policy/v1beta1"
	apiext_v1b1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	apiExtClientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apiext_v1b1inf "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions/apiextensions/v1beta1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	apps_v1inf "k8s.io/client-go/informers/apps/v1"
	autoscaling_v2b1inf "k8s.io/client-go/informers/autoscaling/v2beta1"
	core_v1inf "k8s.io/client-go/informers/core/v1"
	net_v1b1inf "k8s.io/client-go/informers/networking/v1beta1"
	policy_v1b1inf "k8s.io/client-go/informers/policy/v1beta1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
)

type BundleControllerConstructor struct {
	Plugins               []plugin.NewFunc
	ServiceCatalogSupport bool

	// To override things constructed by default. And for tests.
	SmithClient  smithClientset.Interface
	SCClient     scClientset.Interface
	APIExtClient apiExtClientset.Interface
	SmartClient  bundlec.SmartClient
}

func (c *BundleControllerConstructor) AddFlags(flagset ctrl.FlagSet) {
	flagset.BoolVar(&c.ServiceCatalogSupport, "bundle-service-catalog", true, "Service Catalog support in Bundle controller. Enabled by default.")
}

func (c *BundleControllerConstructor) New(config *ctrl.Config, cctx *ctrl.Context) (*ctrl.Constructed, error) {
	// Plugins
	pluginContainers, err := c.loadPlugins()
	if err != nil {
		return nil, err
	}
	for pluginName := range pluginContainers {
		config.Logger.Sugar().Infof("Loaded plugin: %q", pluginName)
	}
	scheme, err := FullScheme(c.ServiceCatalogSupport)
	if err != nil {
		return nil, err
	}

	// Clients
	smithClient := c.SmithClient
	if smithClient == nil {
		smithClient, err = smithClientset.NewForConfig(config.RestConfig)
		if err != nil {
			return nil, err
		}
	}
	scClient := c.SCClient
	if scClient == nil {
		scClient, err = scClientset.NewForConfig(config.RestConfig)
		if err != nil {
			return nil, err
		}
	}
	apiExtClient := c.APIExtClient
	if apiExtClient == nil {
		apiExtClient, err = apiExtClientset.NewForConfig(config.RestConfig)
		if err != nil {
			return nil, err
		}
	}
	smartClient := c.SmartClient
	if smartClient == nil {
		rm := restmapper.NewDeferredDiscoveryRESTMapper(
			&smart.CachedDiscoveryClient{
				DiscoveryInterface: config.MainClient.Discovery(),
			},
		)
		dynamicClient, err := dynamic.NewForConfig(config.RestConfig) // nolint: vetshadow
		if err != nil {
			return nil, err
		}
		smartClient = &smart.DynamicClient{
			DynamicClient: dynamicClient,
			RESTMapper:    rm,
		}
	}

	// Informers
	bundleInf, err := smithInformer(config, cctx, smithClient, smith_v1.BundleGVK, client.BundleInformer)
	if err != nil {
		return nil, err
	}
	crdGVK := apiext_v1b1.SchemeGroupVersion.WithKind("CustomResourceDefinition")
	crdInf, err := apiExtensionsInformer(config, cctx, apiExtClient,
		crdGVK,
		apiext_v1b1inf.NewCustomResourceDefinitionInformer)
	if err != nil {
		return nil, err
	}
	crdStore, err := store.NewCrd(crdInf)
	if err != nil {
		return nil, err
	}

	var catalog *store.Catalog
	if c.ServiceCatalogSupport {
		catalog, err = svcCatalog(config, cctx, scClient)
		if err != nil {
			return nil, err
		}
	}

	// Multi store
	multiStore := store.NewMulti()

	bs, err := store.NewBundle(bundleInf, multiStore, pluginContainers)
	if err != nil {
		return nil, err
	}

	// Add resource informers to Multi store (not ServiceClass/Plan informers, ...)
	resourceInfs, err := c.resourceInformers(config, cctx, scClient)
	if err != nil {
		return nil, err
	}
	resourceInfs[crdGVK] = crdInf
	resourceInfs[smith_v1.BundleGVK] = bundleInf
	for gvk, inf := range resourceInfs {
		if err = multiStore.AddInformer(gvk, inf); err != nil {
			return nil, errors.Errorf("failed to add informer for %s", gvk)
		}
	}

	// Metrics
	bundleTransitionCounter := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: config.AppName,
			Name:      "bundle_transitions_total",
			Help:      "Records the number of times a Bundle transitions into a new condition",
		},
		[]string{"namespace", "name", "type", "reason"},
	)
	bundleResourceTransitionCounter := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: config.AppName,
			Name:      "bundle_resource_transitions_total",
			Help:      "Records the number of times a Bundle transitions into a new condition",
		},
		[]string{"namespace", "name", "resource", "type", "reason"},
	)

	allMetrics := []prometheus.Collector{bundleTransitionCounter, bundleResourceTransitionCounter}
	for _, metric := range allMetrics {
		err = config.Registry.Register(metric)
		if err != nil {
			return nil, errors.WithStack(err)
		}
	}

	// Status Checker
	readyTypes := []map[schema.GroupKind]statuschecker.ObjectStatusChecker{statuschecker_builtin.MainKnownTypes}
	if c.ServiceCatalogSupport {
		readyTypes = append(readyTypes, statuschecker_builtin.ServiceCatalogKnownTypes)
	}
	rc, err := statuschecker.New(crdStore, readyTypes...)
	if err != nil {
		return nil, err
	}

	// Spec checker
	checkTypes := []map[schema.GroupKind]specchecker.ObjectProcessor{specchecker_builtin.MainKnownTypes}
	if c.ServiceCatalogSupport {
		checkTypes = append(checkTypes, specchecker_builtin.ServiceCatalogKnownTypes)
	}
	specChecker := specchecker.New(multiStore, checkTypes...)

	// Event Recorder
	broadcaster := record.NewBroadcaster()
	recorder := broadcaster.NewRecorder(scheme, core_v1.EventSource{Component: smith.Smith})

	// Controller
	cntrlr := &bundlec.Controller{
		Logger:                          config.Logger,
		ReadyForWork:                    cctx.ReadyForWork,
		MainClient:                      config.MainClient,
		BundleClient:                    smithClient.SmithV1(),
		BundleStore:                     bs,
		SmartClient:                     smartClient,
		Rc:                              rc,
		Store:                           multiStore,
		SpecChecker:                     specChecker,
		WorkQueue:                       cctx.WorkQueue,
		CrdResyncPeriod:                 config.ResyncPeriod,
		Namespace:                       config.Namespace,
		PluginContainers:                pluginContainers,
		Scheme:                          scheme,
		Catalog:                         catalog,
		BundleTransitionCounter:         bundleTransitionCounter,
		BundleResourceTransitionCounter: bundleResourceTransitionCounter,

		Broadcaster: broadcaster,
		Recorder:    recorder,
	}
	err = cntrlr.Prepare(crdInf, resourceInfs)
	if err != nil {
		return nil, err
	}

	return &ctrl.Constructed{
		Interface: cntrlr,
	}, nil
}

func (c *BundleControllerConstructor) Describe() ctrl.Descriptor {
	return ctrl.Descriptor{
		Gvk: smith_v1.BundleGVK,
	}
}

func (c *BundleControllerConstructor) loadPlugins() (map[smith_v1.PluginName]plugin.Container, error) {
	pluginContainers := make(map[smith_v1.PluginName]plugin.Container, len(c.Plugins))
	for _, p := range c.Plugins {
		pluginContainer, err := plugin.NewContainer(p)
		if err != nil {
			return nil, err
		}
		description := pluginContainer.Plugin.Describe()
		if _, ok := pluginContainers[description.Name]; ok {
			return nil, errors.Errorf("plugins with same name found %q", description.Name)
		}
		pluginContainers[description.Name] = pluginContainer
	}
	return pluginContainers, nil
}

func (c *BundleControllerConstructor) resourceInformers(config *ctrl.Config, cctx *ctrl.Context, scClient scClientset.Interface) (map[schema.GroupVersionKind]cache.SharedIndexInformer, error) {
	coreInfs := map[schema.GroupVersionKind]func(kubernetes.Interface, string, time.Duration, cache.Indexers) cache.SharedIndexInformer{
		// Core API types
		net_v1b1.SchemeGroupVersion.WithKind("Ingress"):                         net_v1b1inf.NewIngressInformer,
		core_v1.SchemeGroupVersion.WithKind("Service"):                          core_v1inf.NewServiceInformer,
		core_v1.SchemeGroupVersion.WithKind("ConfigMap"):                        core_v1inf.NewConfigMapInformer,
		core_v1.SchemeGroupVersion.WithKind("Secret"):                           core_v1inf.NewSecretInformer,
		core_v1.SchemeGroupVersion.WithKind("ServiceAccount"):                   core_v1inf.NewServiceAccountInformer,
		apps_v1.SchemeGroupVersion.WithKind("Deployment"):                       apps_v1inf.NewDeploymentInformer,
		autoscaling_v2b1.SchemeGroupVersion.WithKind("HorizontalPodAutoscaler"): autoscaling_v2b1inf.NewHorizontalPodAutoscalerInformer,
		policy_v1.SchemeGroupVersion.WithKind("PodDisruptionBudget"):            policy_v1b1inf.NewPodDisruptionBudgetInformer,
	}
	infs := make(map[schema.GroupVersionKind]cache.SharedIndexInformer, len(coreInfs)+2)
	for gvk, coreInf := range coreInfs {
		inf, err := cctx.MainInformer(config, gvk, coreInf)
		if err != nil {
			return nil, err
		}
		infs[gvk] = inf
	}

	// Service Catalog types
	if c.ServiceCatalogSupport {
		scInfs := map[schema.GroupVersionKind]func(scClientset.Interface, string, time.Duration, cache.Indexers) cache.SharedIndexInformer{
			// Service Catalog types
			sc_v1b1.SchemeGroupVersion.WithKind("ServiceBinding"):  sc_v1b1inf.NewServiceBindingInformer,
			sc_v1b1.SchemeGroupVersion.WithKind("ServiceInstance"): sc_v1b1inf.NewServiceInstanceInformer,
		}
		for gvk, scInf := range scInfs {
			inf, err := svcCatInformer(config, cctx, scClient, gvk, scInf)
			if err != nil {
				return nil, err
			}
			infs[gvk] = inf
		}
	}

	return infs, nil
}

func FullScheme(serviceCatalog bool) (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	var sb runtime.SchemeBuilder
	sb.Register(smith_v1.SchemeBuilder...)
	sb.Register(net_v1b1.SchemeBuilder...)
	sb.Register(core_v1.SchemeBuilder...)
	sb.Register(apps_v1.SchemeBuilder...)
	sb.Register(apiext_v1b1.SchemeBuilder...)
	sb.Register(autoscaling_v2b1.SchemeBuilder...)
	sb.Register(policy_v1.SchemeBuilder...)
	if serviceCatalog {
		sb.Register(sc_v1b1.SchemeBuilder...)
	}
	if err := sb.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return scheme, nil
}

func smithInformer(config *ctrl.Config, cctx *ctrl.Context, smithClient smithClientset.Interface, gvk schema.GroupVersionKind, f func(smithClientset.Interface, string, time.Duration) cache.SharedIndexInformer) (cache.SharedIndexInformer, error) {
	inf := cctx.Informers[gvk]
	if inf == nil {
		inf = f(smithClient, config.Namespace, config.ResyncPeriod)
		err := cctx.RegisterInformer(gvk, inf)
		if err != nil {
			return nil, err
		}
	}
	return inf, nil
}

func apiExtensionsInformer(config *ctrl.Config, cctx *ctrl.Context, apiExtClient apiExtClientset.Interface, gvk schema.GroupVersionKind, f func(apiExtClientset.Interface, time.Duration, cache.Indexers) cache.SharedIndexInformer) (cache.SharedIndexInformer, error) {
	inf := cctx.Informers[gvk]
	if inf == nil {
		inf = f(apiExtClient, config.ResyncPeriod, cache.Indexers{})
		err := cctx.RegisterInformer(gvk, inf)
		if err != nil {
			return nil, err
		}
	}
	return inf, nil
}

func svcCatClusterInformer(config *ctrl.Config, cctx *ctrl.Context, scClient scClientset.Interface, gvk schema.GroupVersionKind, f func(scClientset.Interface, time.Duration, cache.Indexers) cache.SharedIndexInformer) (cache.SharedIndexInformer, error) {
	inf := cctx.Informers[gvk]
	if inf == nil {
		inf = f(scClient, config.ResyncPeriod, cache.Indexers{})
		err := cctx.RegisterInformer(gvk, inf)
		if err != nil {
			return nil, err
		}
	}
	return inf, nil
}

func svcCatInformer(config *ctrl.Config, cctx *ctrl.Context, scClient scClientset.Interface, gvk schema.GroupVersionKind, f func(scClientset.Interface, string, time.Duration, cache.Indexers) cache.SharedIndexInformer) (cache.SharedIndexInformer, error) {
	inf := cctx.Informers[gvk]
	if inf == nil {
		inf = f(scClient, config.Namespace, config.ResyncPeriod, cache.Indexers{})
		err := cctx.RegisterInformer(gvk, inf)
		if err != nil {
			return nil, err
		}
	}
	return inf, nil
}

func svcCatalog(config *ctrl.Config, cctx *ctrl.Context, scClient scClientset.Interface) (*store.Catalog, error) {
	serviceClassInf, err := svcCatClusterInformer(config, cctx, scClient,
		sc_v1b1.SchemeGroupVersion.WithKind("ClusterServiceClass"),
		sc_v1b1inf.NewClusterServiceClassInformer)
	if err != nil {
		return nil, err
	}
	servicePlanInf, err := svcCatClusterInformer(config, cctx, scClient,
		sc_v1b1.SchemeGroupVersion.WithKind("ClusterServicePlan"),
		sc_v1b1inf.NewClusterServicePlanInformer)
	if err != nil {
		return nil, err
	}
	return store.NewCatalog(serviceClassInf, servicePlanInf)
}
