package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

const (
	// WatchedByLabel is the label added to deployments managed by this controller
	WatchedByLabel = "watched-by"
	// WatchedByValue is the value of the WatchedByLabel
	WatchedByValue = "deployment-watcher"

	// SharedBasePath is the base path for the shared volume
	SharedBasePath = "/shared/replicacount"
	// SharedVolumeName is the name of the shared volume added to deployments
	SharedVolumeName = "shared-replicacount"
	// SharedVolumeMountPath is the mount path inside the container
	SharedVolumeMountPath = "/shared/replicacount"
	// environment variable to add for container
	DeploymentName_Env = "DEPLOYMENT_NAME"
)

var (
	ContainerEnvironmentVariables = []string{"DEPLOYMENT_NAME"}
)

// flow for reconcile
// Informer Event
//
//	│
//	▼
//
// enqueueDeployment()  →  Queue (deduplicated by key)
//
//	│
//	▼
//
// reconcile(key)
//
//	├── Lister.Get() → NotFound  →  handleDelete()  →  rm /shared/replicacount/ns/name
//	├── no "watched-by" label   →  handleAdd()     →  patch label + volumeMount + write file
//	└── has label               →  handleUpdate()  →  overwrite /shared/replicacount/ns/name
func (dwc *DeploymentWatcherController) reconcile(key ReconcileKey) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		// Malformed key — don't retry
		klog.Errorf("Invalid key %q: %v", key, err)
		return nil
	}

	// Always read from the Lister (local cache), never the API server directly.
	// This is the current observed state of the deployment.
	dep, err := dwc.DeploymentLister.Deployments(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			// Deployment was deleted — clean up our side effects
			klog.Infof("Deployment %q not found — handling delete", key)
			return dwc.handleDelete(namespace, name)
		}
		return fmt.Errorf("fetching deployment %q from cache: %w", key, err)
	}
	// Distinguish ADD vs UPDATE by checking whether we already labelled this deployment.
	if _, hasLabel := dep.Labels[WatchedByLabel]; !hasLabel {
		klog.Infof("Deployment %q is new — handling add", key)
		return dwc.handleAdd(dep)
	}
	klog.Infof("Deployment %q already watched — handling update", key)
	return dwc.handleUpdate(dep)
}

// handleAdd is called the first time this controller sees a deployment.
// It:
//  1. Patches a "watched-by" label onto the deployment.
//  2. Adds a shared volume + volumeMount to every container.
//  3. Writes the initial replica count to /shared/replicacount/<namespace>/<name>.

func (dwc *DeploymentWatcherController) handleAdd(dep *v1.Deployment) error {

	// DeepCopy so we never mutate the shared cache object.
	depCopy := dep.DeepCopy()
	namespace := dep.Namespace
	name := dep.Name

	// 1. Add the "watched-by" label   [note : this label on deployment itself (update on deployment metadata) ]
	if depCopy.Labels == nil {
		depCopy.Labels = make(map[string]string)
	}
	depCopy.Labels[WatchedByLabel] = WatchedByValue

	// 2. Add shared volume if not already present
	// Note in kubernetes in case of
	// Deployment → ReplicaSet → Pods
	//  change | update on pod -> pod template -> dep.Spec.Template
	// like labels on template , containers , image , volume/volumemount  , env vars
	// kubernetes => New ReplicaSet → Rolling Update → new pods   (recreate (rolling update) in all pods )

	if !hasVolume(depCopy, SharedVolumeName) {
		depCopy.Spec.Template.Spec.Volumes = append(depCopy.Spec.Template.Spec.Volumes, corev1.Volume{
			Name: SharedVolumeName,
			VolumeSource: corev1.VolumeSource{
				// ref:
				//    volumes:
				//     - name: shared-replica-volume
				//       emptyDir: {}

				// EmptyDir is scoped to the Pod lifetime.
				// Replace with a HostPath or PVC if you need cross-pod persistence.
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		})
	}
	// 3. Add volumeMount to every container if not already present

	for i := range depCopy.Spec.Template.Spec.Containers {
		container := &depCopy.Spec.Template.Spec.Containers[i]

		if !hasEnvironmentVariable(container, DeploymentName_Env) {

			// ref:
			//		    containers:
			//		      - name: podcontainername
			//		        env:
			//	           - name: DEPLOYMENT_NAME
			//	             value: name
			// or         - name: DEPLOYMENT_NAME
			//              valuefrom
			//	               fieldRef:
			//	                fieldPath: metadata.labels['deployment']
			container.Env = append(container.Env, corev1.EnvVar{
				Name:  DeploymentName_Env,
				Value: name,
			})
		}
		if !hasVolumeMount(container, SharedVolumeName) {
			// 	ref :
			//    volumeMounts:
			//     - name: shared-replicacount-volume
			//       mountPath: /shared/replicacount
			container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
				Name:      SharedVolumeName,
				MountPath: SharedVolumeMountPath,
			})

		}
	}
	// 4. Persist the patched deployment to the API server
	ctx := context.Background()
	_, err := dwc.ClientSet.AppsV1().Deployments(namespace).Update(ctx, depCopy, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("[ADD] updating deployment %s/%s: %w", namespace, name, err)
	}
	klog.Infof("[ADD] Deployment %s/%s patched successfully", namespace, name)
	// 5. Write the initial replica count to the shared path

	replicas := int32(0)
	if depCopy.Spec.Replicas != nil {
		replicas = *depCopy.Spec.Replicas
	}
	if err := writeReplicaCount(namespace, name, replicas); err != nil {
		return fmt.Errorf("[ADD] writing replica count for %s/%s: %w", namespace, name, err)
	}
	return nil

}

// handleUpdate is called on every subsequent reconcile for an already-watched deployment.
// It writes the current AvailableReplicas count to the shared path.
// This is idempotent — overwriting the same value is harmless.
func (dwc *DeploymentWatcherController) handleUpdate(dep *appsv1.Deployment) error {
	namespace := dep.Namespace
	name := dep.Name
	available := dep.Status.AvailableReplicas

	klog.Infof("[UPDATE] Deployment %s/%s: availableReplicas=%d", namespace, name, available)

	if err := writeReplicaCount(namespace, name, available); err != nil {
		return fmt.Errorf("[UPDATE] writing replica count for %s/%s: %w", namespace, name, err)
	}

	return nil
}

// handleDelete cleans up when a deployment is removed.
// It deletes the replica-count file from the shared path.
func (dwc *DeploymentWatcherController) handleDelete(namespace, name string) error {
	klog.Infof("[DELETE] Deployment %s/%s: removing shared state", namespace, name)

	filePath := replicaCountPath(namespace, name)
	err := os.Remove(filePath)
	if err != nil && !os.IsNotExist(err) {
		// IsNotExist is fine — maybe we never wrote it (e.g. controller restarted)
		return fmt.Errorf("[DELETE] removing file %q: %w", filePath, err)
	}

	klog.Infof("[DELETE] Cleaned up %q", filePath)

	// Optionally remove the per-namespace directory if now empty
	nsDir := filepath.Join(SharedBasePath, namespace)
	_ = os.Remove(nsDir) // ignore error — directory may still have other deployments

	return nil
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// hasVolume returns true if the pod spec already contains a volume with the given name.
// refernce :
// apiVersion: apps/v1
// kind: Deployment
// metadata:
//
//	name: ""
//
// spec:
// replicas: 1
// template:
//
//	metadata:
//	    labels:
//	      app: pod-detector
//	      deployment: pod-detector-deployment
//		  spec:
//		    containers:
//		      - name: pod-detector
//		        env:
//		          - name: POD_NAME
//		            valueFrom:
//		              fieldRef:
//		                fieldPath: metadata.name
//	           - name: DEPLOYMENT_NAME
//	             valueFrom:
//	               fieldRef:
//	                fieldPath: metadata.labels['deployment']
//		        volumeMounts:
//		          - name: shared-replicacount-volume
//		            mountPath: /shared
//		    volumes:
//		      - name: shared-replicacount-volume
//		        emptyDir: {}
func hasVolume(dep *v1.Deployment, volumeName string) bool {
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Name == volumeName {
			return true
		}
	}
	return false
}

func hasVolumeMount(c *corev1.Container, mountName string) bool {
	for _, vm := range c.VolumeMounts {
		if vm.Name == mountName {
			return true
		}
	}
	return false
}
func hasEnvironmentVariable(c *corev1.Container, environmentname string) bool {
	for _, env := range c.Env {
		if env.Name == environmentname {
			return true
		}
	}
	return false
}
func hasEnvironmentVariables(c *corev1.Container) []string {

	var missingEnvs []string
	existingEnvs := make(map[string]bool)
	for _, env := range c.Env {
		existingEnvs[env.Name] = true
	}

	for _, desiredenv := range ContainerEnvironmentVariables {
		if !existingEnvs[desiredenv] {
			missingEnvs = append(missingEnvs, desiredenv)
		}
	}
	return missingEnvs
}

// replicaCountPath returns the canonical file path for a deployment's replica count.
func replicaCountPath(namespace, name string) string {
	return filepath.Join(SharedBasePath, namespace, name)
}

// writeReplicaCount writes the replica count atomically using a temp-file + rename.
// This prevents readers from seeing a partial write.
func writeReplicaCount(namespace, name string, replicas int32) error {

	dir := filepath.Join(SharedBasePath, namespace)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating directory %q: %w", dir, err)
	}

	target := replicaCountPath(namespace, name)
	// Write to a temp file in the same directory so os.Rename is atomic
	// (same filesystem guaranteed).
	tmp := target + ".tmp"
	content := strconv.FormatInt(int64(replicas), 10)
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing temp file %q: %w", tmp, err)
	}

	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp) // best-effort cleanup
		return fmt.Errorf("renaming %q -> %q: %w", tmp, target, err)

	}
	klog.V(4).Infof("Wrote replicas=%d to %q", replicas, target)
	return nil
}

// ─── Patch helpers (alternative to full Update) ───────────────────────────────

// patchLabel applies a strategic merge patch to add the watched-by label
// without touching the rest of the spec. Use this if you want a lighter-weight
// label-only patch and handle volume/mount separately.
func (dwc *DeploymentWatcherController) patchLabel(dep *appsv1.Deployment) error {
	patch := fmt.Sprintf(
		`{"metadata":{"labels":{%q:%q}}}`,
		WatchedByLabel, WatchedByValue,
	)
	_, err := dwc.ClientSet.AppsV1().Deployments(dep.Namespace).Patch(
		context.Background(),
		dep.Name,
		types.StrategicMergePatchType,
		[]byte(patch),
		metav1.PatchOptions{},
	)
	return err
}
