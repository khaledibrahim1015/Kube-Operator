package controller

import (
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	appslisters "k8s.io/client-go/listers/apps/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const (
	// DefaultResyncPeriod is the default resync period for the informer
	DefaultResyncPeriod = 30 * time.Second
	// MaxRetries is the number of times a deployment will be retried before it is dropped
	MaxRetries = 5
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
	Queue            workqueue.RateLimitingInterface

	// config
	DeploymentMap map[string][]string // namespace -> [] deployment-names

	//  state
	stopCh chan struct{}
}

// NewDeploymentWatcher creates a new DeploymentWatcherController
func NewDeploymentWatcher(clientset kubernetes.Interface, namespace string) *DeploymentWatcherController {

	return NewDeploymentWatcherResync(clientset, namespace, DefaultResyncPeriod)
}

// NewDeploymentWatcherWithResync creates a new DeploymentWatcherController with custom resync period
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

// NewDeploymentWatcherWithResync creates a new DeploymentWatcherController with custom resync period
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

	// Create rate limiting queue
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

// NewDeploymentWatcherWithResync creates a new DeploymentWatcherController with custom resync period
func NewDeploymentWatcherResyncRateLimitQueue_V2(clientset kubernetes.Interface, resyncPeriod time.Duration,
	ratelimitconfig WorkQueueRateLimitConfig,
	depWatacherconfig DeploymentWatcherConfig,
) *DeploymentWatcherController {

	InformerFactory := informers.NewSharedInformerFactoryWithOptions(
		clientset,
		resyncPeriod,
		informers.WithNamespace(depWatacherconfig.targetNamespace),
	)

	depInformer := InformerFactory.Apps().V1().Deployments()
	deploymentInformer := depInformer.Informer()
	deploymentLister := depInformer.Lister()
	depSync := depInformer.Informer().HasSynced

	// Create rate limiting queue
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
	if depWatacherconfig.targetNamespace != "" && len(depWatacherconfig.targetDeployments) > 0 {
		controller.DeploymentMap[depWatacherconfig.targetNamespace] = depWatacherconfig.targetDeployments

	}
	controller.setupDefaultEventHandlers()
	return controller
}

func (dwc *DeploymentWatcherController) setupDefaultEventHandlers() {
	dwc.DeploymentInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: dwc.enqueueDeployment,
		DeleteFunc: func(obj interface{}) {
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				dwc.Queue.Add(key)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldDep := oldObj.(*appsv1.Deployment)
			newDep := newObj.(*appsv1.Deployment)
			if oldDep.Status.AvailableReplicas != newDep.Status.AvailableReplicas ||
				oldDep.ResourceVersion != newDep.ResourceVersion ||
				oldDep.Generation != newDep.Generation {
				dwc.enqueueDeployment(newObj)
			}
		},
	})
}

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

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't split key for object %+v: %w", obj, err))
		return
	}

	targetDeploymentList, exists := dwc.DeploymentMap[namespace]
	if !exists {
		// Deployment namespace not in watch list - skip
		return
	}

	// Check if this specific deployment is in the target list
	for _, targetDep := range targetDeploymentList {
		if name == targetDep {
			expectedKey := fmt.Sprintf("%s/%s", namespace, targetDep)
			if key == expectedKey {
				dwc.Queue.Add(key)
			}
		}
	}

}

// Run starts the controller.
func (dwc *DeploymentWatcherController) Run(workers int) error {

	defer dwc.Cleanup()

	// start informer
	dwc.InformerFactory.Start(dwc.stopCh)

	// CRITICAL: wait for the informer cache to be populated
	// Until HasSynced returns true, the Lister will return stale/empty data.
	// This is equivalent to waiting for the initial LIST to complete.
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
func (dwc *DeploymentWatcherController) Cleanup() {
	dwc.Queue.ShutDown()
	close(dwc.stopCh)
}

// runWorker is the worker loop. It processes one item at a time.
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
	// type asseration for stored key "namespace/name"
	key, ok := item.(ReconcileKey)
	if !ok {
		dwc.Queue.Forget(item)
		return true
	}

	// reconcile is where your actual logic lives.
	err := dwc.reconcile(key)

	if err == nil {
		// Success: clear any backoff/retry state for this key
		dwc.Queue.Forget(item)
		return true
	}
	if dwc.Queue.NumRequeues(item) < MaxRetries {
		dwc.Queue.AddRateLimited(item)
		return true
	}

	// max retries exceed - drop it
	dwc.Queue.Forget(item)
	return true

}
