package k8s

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type Client struct {
	client dynamic.Interface
}

func NewClient(kubeconfig string) *Client {
	if kubeconfig == "" {
		kubeconfig = clientcmd.RecommendedHomeFile
	}
	k8sCfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		k8sCfg, err = rest.InClusterConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to get kube config: %v\n", err)
			os.Exit(1)
		}
	}
	client, err := dynamic.NewForConfig(k8sCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create k8s client: %v\n", err)
		os.Exit(1)
	}
	return &Client{client: client}
}

func (c *Client) Client() dynamic.Interface {
	return c.client
}

func (c *Client) GetNewImage(service, version, namespace string) (string, error) {
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	dep, err := c.client.Resource(gvr).Namespace(namespace).Get(context.Background(), service, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get deployment: %v", err)
	}

	containers, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
	if len(containers) == 0 {
		return "", fmt.Errorf("no containers found in deployment")
	}

	container := containers[0].(map[string]interface{})
	currentImage, found, err := unstructured.NestedString(container, "image")
	if err != nil || !found {
		return "", fmt.Errorf("no image found in deployment: %v", err)
	}

	parts := strings.Split(currentImage, ":")
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid image format: %s", currentImage)
	}
	baseImage := parts[0]
	newImage := fmt.Sprintf("%s:%s", baseImage, version)

	return newImage, nil
}

func (c *Client) UpdateDeployment(ctx context.Context, service, newImage, namespace string) (bool, string, string) {
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	dep, err := c.client.Resource(gvr).Namespace(namespace).Get(ctx, service, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Sprintf("Failed to get deployment: %v", err), ""
	}

	containers, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
	if len(containers) == 0 {
		return false, "No containers found", ""
	}

	container := containers[0].(map[string]interface{})
	oldImage, found, err := unstructured.NestedString(container, "image")
	if err != nil || !found {
		return false, fmt.Sprintf("No image found in container: %v", err), ""
	}

	unstructured.SetNestedField(container, newImage, "image")
	unstructured.SetNestedSlice(dep.Object, containers, "spec", "template", "spec", "containers")

	_, err = c.client.Resource(gvr).Namespace(namespace).Update(ctx, dep, metav1.UpdateOptions{})
	if err != nil {
		return false, fmt.Sprintf("Failed to update deployment: %v", err), oldImage
	}

	return true, "", oldImage
}

func (c *Client) WaitForRollout(ctx context.Context, service, namespace string) bool {
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	watcher, err := c.client.Resource(gvr).Namespace(namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", service),
	})
	if err != nil {
		return false
	}
	defer watcher.Stop()

	for {
		select {
		case event, more := <-watcher.ResultChan():
			if !more {
				return false
			}
			if event.Type == watch.Modified {
				dep := event.Object.(*unstructured.Unstructured)
				cond, _, _ := unstructured.NestedSlice(dep.Object, "status", "conditions")
				for _, c := range cond {
					if m, ok := c.(map[string]interface{}); ok {
						if typ, _ := m["type"].(string); typ == "Available" && m["status"].(string) == "True" {
							return true
						}
					}
				}
			}
		case <-ctx.Done():
			return false
		}
	}
}

func (c *Client) RestoreDeployment(ctx context.Context, service, oldImage, namespace string) {
	if oldImage == "" {
		return
	}
	c.UpdateDeployment(ctx, service, oldImage, namespace)
}

func (c *Client) GetDeploymentDiagnostics(ctx context.Context, service, namespace string) (string, string, map[string]string) {
	events := c.getEvents(ctx, service, namespace)
	logs := c.getPodLogs(ctx, service, namespace)
	envs := c.getDeploymentEnvs(ctx, service, namespace)
	return events, logs, envs
}

func (c *Client) getEvents(ctx context.Context, service, namespace string) string {
	eventsGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "events"}
	events, err := c.client.Resource(eventsGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s", service),
	})
	if err != nil {
		return fmt.Sprintf("Failed to get events: %v", err)
	}

	var eventsStr strings.Builder
	for _, ev := range events.Items {
		message, found, err := unstructured.NestedString(ev.Object, "message")
		if err != nil || !found {
			continue
		}
		eventsStr.WriteString(fmt.Sprintf("• %s\n", message))
	}
	return eventsStr.String()
}

func (c *Client) getPodLogs(ctx context.Context, service, namespace string) string {
	podsGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
	pods, err := c.client.Resource(podsGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", service),
	})
	if err != nil || len(pods.Items) == 0 {
		return "No pods found or error getting pods"
	}

	podName, found, err := unstructured.NestedString(pods.Items[0].Object, "metadata", "name")
	if err != nil || !found {
		return "Failed to get pod name"
	}
	return fmt.Sprintf("Pod: %s - Logs retrieval requires REST client", podName)
}

func (c *Client) getDeploymentEnvs(ctx context.Context, service, namespace string) map[string]string {
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	dep, err := c.client.Resource(gvr).Namespace(namespace).Get(ctx, service, metav1.GetOptions{})
	if err != nil {
		return nil
	}

	containers, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
	if len(containers) == 0 {
		return nil
	}

	container := containers[0].(map[string]interface{})
	envs, _, _ := unstructured.NestedSlice(container, "env")

	result := make(map[string]string)
	for _, env := range envs {
		if e, ok := env.(map[string]interface{}); ok {
			name, nameFound, nameErr := unstructured.NestedString(e, "name")
			value, valueFound, valueErr := unstructured.NestedString(e, "value")
			if nameErr == nil && valueErr == nil && nameFound && valueFound && name != "" && value != "" {
				result[name] = value
			}
		}
	}
	return result
}

func (c *Client) CheckNewPodStatus(ctx context.Context, service, newImage, namespace string) (bool, error) {
	const maxAttempts = 10
	const interval = 5 * time.Second

	var errMsg strings.Builder
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		podsGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
		pods, err := c.client.Resource(podsGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app=%s", service),
		})
		if err != nil {
			return false, fmt.Errorf("failed to list pods: %v", err)
		}

		var newPodsReady bool = true
		errMsg.Reset()
		for _, pod := range pods.Items {
			containers, _, _ := unstructured.NestedSlice(pod.Object, "spec", "containers")
			if len(containers) == 0 {
				continue
			}
			podImage, found, err := unstructured.NestedString(containers[0].(map[string]interface{}), "image")
			if err != nil || !found {
				continue
			}
			if newImage != "" && podImage != newImage {
				continue // Only check pods with the new image
			}

			phase, found, err := unstructured.NestedString(pod.Object, "status", "phase")
			if err != nil || !found {
				newPodsReady = false
				errMsg.WriteString(fmt.Sprintf("Pod %s: failed to get status phase: %v\n", pod.GetName(), err))
				continue
			}
			if phase != "Running" {
				newPodsReady = false
				errMsg.WriteString(fmt.Sprintf("Pod %s phase: %s (not Running)\n", pod.GetName(), phase))
				if phase == "Pending" || strings.Contains(phase, "Error") || strings.Contains(phase, "Crash") {
					errMsg.WriteString(c.getPodEvents(ctx, pod.GetName(), namespace))
				}
				continue
			}

			conditions, _, _ := unstructured.NestedSlice(pod.Object, "status", "conditions")
			ready := false
			for _, cond := range conditions {
				c, ok := cond.(map[string]interface{})
				if ok && c["type"] == "Ready" && c["status"] == "True" {
					ready = true
					break
				}
			}
			if !ready {
				newPodsReady = false
				errMsg.WriteString(fmt.Sprintf("Pod %s not ready\n", pod.GetName()))
			}
		}

		if newPodsReady && errMsg.Len() == 0 {
			return true, nil
		}
		if attempt < maxAttempts {
			select {
			case <-ctx.Done():
				return false, fmt.Errorf("context cancelled: %v", ctx.Err())
			case <-time.After(interval):
			}
		}
	}
	return false, fmt.Errorf("new pods not ready after %d attempts: %s", maxAttempts, errMsg.String())
}

func (c *Client) getPodEvents(ctx context.Context, podName, namespace string) string {
	eventsGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "events"}
	events, err := c.client.Resource(eventsGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=Pod", podName),
	})
	if err != nil {
		return fmt.Sprintf("Failed to get pod events: %v", err)
	}

	var eventsStr strings.Builder
	for _, ev := range events.Items {
		message, found, err := unstructured.NestedString(ev.Object, "message")
		if err != nil || !found {
			continue
		}
		eventsStr.WriteString(fmt.Sprintf("• %s\n", message))
	}
	if eventsStr.Len() == 0 {
		return "No events found"
	}
	return eventsStr.String()
}

func (c *Client) RollbackDeployment(ctx context.Context, service, namespace string) error {
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	dep, err := c.client.Resource(gvr).Namespace(namespace).Get(ctx, service, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get deployment: %v", err)
	}
	currentRevision, found, err := unstructured.NestedInt64(dep.Object, "status", "observedGeneration")
	if err != nil || !found {
		return fmt.Errorf("failed to get current revision: %v", err)
	}

	rollback := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "DeploymentRollback",
			"name":       service,
			"rollbackTo": map[string]interface{}{
				"revision": currentRevision - 1, // Roll back to the last stable revision
			},
		},
	}
	_, err = c.client.Resource(gvr).Namespace(namespace).Create(ctx, rollback, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("rollback failed: %v", err)
	}
	return nil
}

func (c *Client) RestartDeployment(ctx context.Context, service, namespace string) error {
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	dep, err := c.client.Resource(gvr).Namespace(namespace).Get(ctx, service, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get deployment for restart: %v", err)
	}

	annotations, _, _ := unstructured.NestedMap(dep.Object, "spec", "template", "metadata", "annotations")
	if annotations == nil {
		annotations = make(map[string]interface{})
	}
	annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().Format(time.RFC3339)
	unstructured.SetNestedMap(dep.Object, annotations, "spec", "template", "metadata", "annotations")

	_, err = c.client.Resource(gvr).Namespace(namespace).Update(ctx, dep, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to restart deployment: %v", err)
	}
	return nil
}