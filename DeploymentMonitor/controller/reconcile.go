package controller

// utilerrors "k8s.io/apimachinery/pkg/api/errors"

// // add -> new deployment  -> create label , moutpath , voulume mount    path shared /shared/replicacount
// // update -> update replica count /shared/replicacount
// // delete ->
// // The core idea:
// // use a shared volume + file-based signaling pattern
// // where each watched deployment writes its replica count
// // to /shared/replicacount/<namespace>/<deployment-name>

// // full flow
// // Informer Event
// //
// //	│
// //	▼
// //
// // enqueueDeployment()  →  Queue (deduplicated by key)
// //
// //	│
// //	▼
// //
// // reconcile(key)
// //
// //	│
// //	├── Lister.Get() → NotFound  →  handleDelete()  →  rm /shared/replicacount/ns/name
// //	│
// //	├── no "watched-by" label   →  handleAdd()     →  patch label + volumeMount + write file
// //	│
// //	└── has label               →  handleUpdate()  →  overwrite /shared/replicacount/ns/name
// func (dwc *DeploymentWatcherController) reconcile(key ReconcileKey) error {

// 	namespace, name, err := cache.SplitMetaNamespaceKey(key)

// 	if err != nil {
// 		return fmt.Errorf("invalid key: %w", err)
// 	}

// 	// Fetch current state from cache (never the API server directly)
// 	// Fetch from LOCAL CACHE via Lister — zero API calls

// 	dep, err := dwc.DeploymentLister.Deployments(namespace).Get(name)
// 	if err != nil {
// 		if utilerrors.IsNotFound(err) {
// 			// DELETE path
// 			dwc.handleDelete(namespace, name)
// 		}
// 		return fmt.Errorf("fetching deployment %s: %w", key, err)
// 	}
// 	// ADD or UPDATE path — distinguish by checking your label
// 	if _, hasLabel := dep.Labels["watched-by"]; !hasLabel {
// 		return dwc.handleAdd(dep)
// 	}
// 	return dwc.handleUpdate(dep)
// }
