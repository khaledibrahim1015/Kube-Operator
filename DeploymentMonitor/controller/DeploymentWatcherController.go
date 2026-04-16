package controller

import (
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	appslisters "k8s.io/client-go/listers/apps/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const (
	// DefaultResyncPeriod is the default resync period for the informer
	DefaultResyncPeriod = 30 * time.Second
	// MaxRetries is the number of times a deployment will be retried before it is dropped
	MaxRetries = 5

	// ControllerName is used as the source for Kubernetes events.
	ControllerName = "deployment-watcher"
)

// The key type stored in the queue.
// Convention: use "namespace/name" strings for Kubernetes resources.
// This lets you deduplicate by resource identity.
type ReconcileKey = string

type DeploymentWatcherConfig struct {
	targetNamespace   string
	targetDeployments []string
}

type WorkQueueRateLimitConfig struct {
	baseDelayInMS    time.Duration
	maxDelayInSecond time.Duration
}

type DeploymentWatcherController struct {
	ClientSet kubernetes.Interface

	// Informer factory and informers
	InformerFactory    informers.SharedInformerFactory
	DeploymentInformer cache.SharedIndexInformer

	// lister: reads from cache, never calls API
	DeploymentLister appslisters.DeploymentLister
	DepSynced        cache.InformerSynced // func() bool — true when cache is ready

	Queue workqueue.RateLimitingInterface

	// config
	DeploymentMap map[string][]string // namespace -> [] deployment-names

	// state
	stopCh chan struct{}
}

// newEventRecorder creates an EventRecorder that tags events with ControllerName.
func newEventRecorder(clientset kubernetes.Interface) record.EventRecorder {
	eventBroadcaster := record.NewBroadcaster()
	// Send events to klog (visible in controller logs)
	eventBroadcaster.StartStructuredLogging(0)
	// Send events to the Kubernetes API (visible via kubectl describe)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{
		Interface: clientset.CoreV1().Events(""),
	})
	return eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: ControllerName})
}

// NewDeploymentWatcher creates a new DeploymentWatcherController
func NewDeploymentWatcher(clientset kubernetes.Interface, namespace string) *DeploymentWatcherController {
	return NewDeploymentWatcherResync(clientset, namespace, DefaultResyncPeriod)
}

// NewDeploymentWatcherResync creates a new DeploymentWatcherController with custom resync period
func NewDeploymentWatcherResync(clientset kubernetes.Interface, namespace string, resyncPeriod time.Duration) *DeploymentWatcherController {
	// Factory creates and manages informers centrally.
	// If another controller also needs Deployment informer, it gets the same one.
	// Create informer factory with namespace filter
	// SharedInformerFactory creates informers for you and handles sharing.
	// It also manages the resync period centrally.
	// defaultResync = how often to force a full re-list even if no events come
	InformerFactory := informers.NewSharedInformerFactoryWithOptions(
		clientset,
		resyncPeriod,
		informers.WithNamespace(namespace),
	)
	// Get deployment informer and lister
	// Get a Deployment informer from the factory
	// The factory ensures only ONE informer is created per resource type,
	// no matter how many controllers ask for it.
	// create controller with its   Reflector ListWatch - tells informer How to List and watch from api server  for this object
	depInformer := InformerFactory.Apps().V1().Deployments()
	deploymentInformer := depInformer.Informer()
	// Lister() returns a typed lister that reads from the informer's cache
	deploymentLister := depInformer.Lister()
	depSync := depInformer.Informer().HasSynced

	// DefaultControllerRateLimiter: 5ms base delay, 1000s max, max 10 per second
	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	controller := &DeploymentWatcherController{
		ClientSet:          clientset,
		InformerFactory:    InformerFactory,
		DeploymentInformer: deploymentInformer,
		DeploymentLister:   deploymentLister,
		DepSynced:          depSync,
		Queue:              queue,
	}

	controller.setupDefaultEventHandlers()
	return controller
}

// NewDeploymentWatcherResyncRateLimitQueue creates a controller with a custom rate limiter
func NewDeploymentWatcherResyncRateLimitQueue(clientset kubernetes.Interface, namespace string, resyncPeriod time.Duration, ratelimitconfig WorkQueueRateLimitConfig) *DeploymentWatcherController {
	InformerFactory := informers.NewSharedInformerFactoryWithOptions(
		clientset,
		resyncPeriod,
		informers.WithNamespace(namespace),
	)

	depInformer := InformerFactory.Apps().V1().Deployments()
	deploymentInformer := depInformer.Informer()
	deploymentLister := depInformer.Lister()
	depSync := depInformer.Informer().HasSynced

	rateLimiter := workqueue.NewItemExponentialFailureRateLimiter(
		ratelimitconfig.baseDelayInMS*time.Millisecond,
		ratelimitconfig.maxDelayInSecond*time.Second,
	)
	queue := workqueue.NewRateLimitingQueue(rateLimiter)
	controller := &DeploymentWatcherController{
		ClientSet:          clientset,
		InformerFactory:    InformerFactory,
		DeploymentInformer: deploymentInformer,
		DeploymentLister:   deploymentLister,
		DepSynced:          depSync,
		Queue:              queue,
	}

	controller.setupDefaultEventHandlers()
	return controller
}

// NewDeploymentWatcherResyncRateLimitQueue_V2 creates a fully-configured controller
func NewDeploymentWatcherResyncRateLimitQueue_V2(
	clientset kubernetes.Interface,
	resyncPeriod time.Duration,
	ratelimitconfig WorkQueueRateLimitConfig,
	depWatcherConfig DeploymentWatcherConfig,
) *DeploymentWatcherController {

	InformerFactory := informers.NewSharedInformerFactoryWithOptions(
		clientset,
		resyncPeriod,
		informers.WithNamespace(depWatcherConfig.targetNamespace),
	)

	depInformer := InformerFactory.Apps().V1().Deployments()
	deploymentInformer := depInformer.Informer()
	deploymentLister := depInformer.Lister()
	depSync := depInformer.Informer().HasSynced

	rateLimiter := workqueue.NewItemExponentialFailureRateLimiter(
		ratelimitconfig.baseDelayInMS*time.Millisecond,
		ratelimitconfig.maxDelayInSecond*time.Second,
	)
	queue := workqueue.NewRateLimitingQueue(rateLimiter)
	controller := &DeploymentWatcherController{
		ClientSet:          clientset,
		InformerFactory:    InformerFactory,
		DeploymentInformer: deploymentInformer,
		DeploymentLister:   deploymentLister,
		DepSynced:          depSync,
		Queue:              queue,
		DeploymentMap:      make(map[string][]string),
		stopCh:             make(chan struct{}),
	}

	if depWatcherConfig.targetNamespace != "" && len(depWatcherConfig.targetDeployments) > 0 {
		controller.DeploymentMap[depWatcherConfig.targetNamespace] = depWatcherConfig.targetDeployments
	}

	controller.setupDefaultEventHandlers()
	return controller
}

func (dwc *DeploymentWatcherController) setupDefaultEventHandlers() {
	dwc.DeploymentInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: dwc.enqueueDeployment,

		// IMPORTANT: Use DeletionHandlingMetaNamespaceKeyFunc for deletes.
		// When the informer processes a delete event, the object may be wrapped
		// in a cache.DeletedFinalStateUnknown tombstone (if the watch was
		// interrupted and the delete was missed). This helper unwraps it safely.
		DeleteFunc: func(obj interface{}) {
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err != nil {
				utilruntime.HandleError(fmt.Errorf("couldn't get key for deleted object %+v: %w", obj, err))
				return
			}
			dwc.filterAndEnqueue(key)
		},

		UpdateFunc: func(oldObj, newObj interface{}) {
			oldDep := oldObj.(*appsv1.Deployment)
			newDep := newObj.(*appsv1.Deployment)
			// Only reconcile on meaningful changes — skip no-op resync events
			if oldDep.Status.AvailableReplicas != newDep.Status.AvailableReplicas ||
				oldDep.ResourceVersion != newDep.ResourceVersion ||
				oldDep.Generation != newDep.Generation {
				dwc.enqueueDeployment(newObj)
			}
		},
	})
}

// enqueueDeployment extracts the key and delegates to filterAndEnqueue.
// enqueueDeployment converts an object to a key and adds it to the queue.
// Using a key instead of the object itself means:
// - By the time the worker runs, it fetches CURRENT state from the Lister
// - Multiple events for the same object collapse into one queue entry
func (dwc *DeploymentWatcherController) enqueueDeployment(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %w", obj, err))
		return
	}
	dwc.filterAndEnqueue(key)
}

// filterAndEnqueue checks the DeploymentMap and adds the key to the queue
// only if it belongs to a watched namespace+deployment pair.
// If DeploymentMap is empty, all deployments are enqueued (watch-all mode).
func (dwc *DeploymentWatcherController) filterAndEnqueue(key string) {
	// If no filter is configured, enqueue everything
	if len(dwc.DeploymentMap) == 0 {
		dwc.Queue.Add(key)
		return
	}

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't split key %q: %w", key, err))
		return
	}

	targetDeploymentList, exists := dwc.DeploymentMap[namespace]
	if !exists {
		return // namespace not watched
	}

	for _, targetDep := range targetDeploymentList {
		if name == targetDep {
			dwc.Queue.Add(key)
			return
		}
	}
}

// Run starts the controller.
func (dwc *DeploymentWatcherController) Run(workers int) error {
	defer dwc.Cleanup()

	// Start all informers managed by the factory
	dwc.InformerFactory.Start(dwc.stopCh)

	// Block until the informer cache is fully populated.
	// Lister reads before this point may return empty/stale results.
	if ok := cache.WaitForCacheSync(dwc.stopCh, dwc.DepSynced); !ok {
		return fmt.Errorf("timed out waiting for caches to sync")
	}

	for i := 0; i < workers; i++ {
		go wait.Until(dwc.runWorker, time.Second, dwc.stopCh)
	}

	klog.Info("Controller started successfully")
	// Block until context is cancelled
	<-dwc.stopCh
	return nil
}

// Stop signals the controller to shut down gracefully.
func (dwc *DeploymentWatcherController) Stop() {
	select {
	case <-dwc.stopCh:
		// already closed
	default:
		close(dwc.stopCh)
	}
}

func (dwc *DeploymentWatcherController) Cleanup() {
	dwc.Queue.ShutDown()
}

// runWorker processes items from the queue sequentially.
func (dwc *DeploymentWatcherController) runWorker() {
	for dwc.processNextItem() {
		// processNextItem returns false only when queue is shut down
	}
}

func (dwc *DeploymentWatcherController) processNextItem() bool {
	// Get blocks until an item is available or the queue is shut down
	// Get() blocks until something is available
	// Also returns false if queue is shut down
	item, quit := dwc.Queue.Get()
	if quit {
		return false
	}
	// MUST call Done after Get — otherwise the item stays in "processing"
	// and the same key can never be re-queued
	defer dwc.Queue.Done(item)

	key, ok := item.(ReconcileKey)
	if !ok {
		// Malformed item — discard immediately, do not retry
		dwc.Queue.Forget(item)
		return true
	}
	// reconcile is where your actual logic lives.

	err := dwc.reconcile(key)
	if err == nil {
		dwc.Queue.Forget(item)
		return true
	}

	if dwc.Queue.NumRequeues(item) < MaxRetries {
		klog.Errorf("Error reconciling %q (will retry): %v", key, err)
		dwc.Queue.AddRateLimited(item)
		return true
	}

	// Max retries exceeded — drop the item
	klog.Errorf("Dropping %q after %d retries: %v", key, MaxRetries, err)
	dwc.Queue.Forget(item)
	utilruntime.HandleError(err)
	return true
}
