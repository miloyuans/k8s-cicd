package k8s

import (
    "context"
    "fmt"
    "log"
    "os"
    "strconv"
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

type PodInfo struct {
    Name   string
    Status struct {
        Phase string
    }
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

func (c *Client) CheckNewPodStatus(ctx context.Context, service, newImage, namespace string) (bool, error) {
    const maxAttempts = 10
    const interval = 5 * time.Second

    gvr := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
    for attempt := 1; attempt <= maxAttempts; attempt++ {
        pods, err := c.client.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{
            LabelSelector: fmt.Sprintf("app=%s", service),
        })
        if err != nil {
            return false, fmt.Errorf("failed to list pods: %v", err)
        }

        newPodsReady := true
        var errMsg strings.Builder
        for _, pod := range pods.Items {
            containers, _, _ := unstructured.NestedSlice(pod.Object, "spec", "containers")
            if len(containers) == 0 {
                continue
            }
            image, found, _ := unstructured.NestedString(containers[0].(map[string]interface{}), "image")
            if !found || image != newImage {
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
            phase, _, _ := unstructured.NestedString(pod.Object, "status", "phase")
            if phase == "" {
                phase = "Unknown"
            }
            if phase == "Failed" || phase == "CrashLoopBackOff" || phase == "Evicted" {
                newPodsReady = false
                errMsg.WriteString(fmt.Sprintf("Pod %s in error state: %s\n", pod.GetName(), phase))
            } else if phase != "Running" || !ready {
                newPodsReady = false
                errMsg.WriteString(fmt.Sprintf("Pod %s not ready (phase: %s, ready: %v)\n", pod.GetName(), phase, ready))
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

func (c *Client) GetPodEvents(ctx context.Context, podName, namespace string) string {
    eventsGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "events"}
    events, err := c.client.Resource(eventsGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
        FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=Pod", podName),
    })
    if err != nil {
        log.Printf("Failed to get pod events for %s in namespace %s: %v", podName, namespace, err)
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

func (c *Client) GetLatestStableRevision(ctx context.Context, service, namespace string) (int64, error) {
    gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}
    rsList, err := c.client.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{
        LabelSelector: fmt.Sprintf("app=%s", service),
    })
    if err != nil {
        log.Printf("Failed to list replicasets for service %s: %v", service, err)
        return 0, fmt.Errorf("failed to list replicasets: %v", err)
    }
    var latestStable int64
    for _, rs := range rsList.Items {
        revisionStr, found, _ := unstructured.NestedString(rs.Object, "metadata", "annotations", "deployment.kubernetes.io/revision")
        if !found {
            continue
        }
        revision, err := strconv.ParseInt(revisionStr, 10, 64)
        if err != nil {
            log.Printf("Failed to parse revision for replicaset %s: %v", rs.GetName(), err)
            continue
        }
        conditions, _, _ := unstructured.NestedSlice(rs.Object, "status", "conditions")
        isStable := false
        for _, cond := range conditions {
            c, ok := cond.(map[string]interface{})
            if ok && c["type"] == "Available" && c["status"] == "True" {
                isStable = true
                break
            }
        }
        if isStable && revision > latestStable {
            latestStable = revision
        }
    }
    if latestStable == 0 {
        log.Printf("No stable revision found for service %s", service)
        return 0, fmt.Errorf("no stable revision found")
    }
    return latestStable, nil
}

func (c *Client) RollbackDeployment(ctx context.Context, service, namespace string) error {
    stableRev, err := c.GetLatestStableRevision(ctx, service, namespace)
    if err != nil || stableRev == 0 {
        log.Printf("No stable revision found for service %s: %v", service, err)
        return fmt.Errorf("no stable revision found: %v", err)
    }
    gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
    rollback := &unstructured.Unstructured{
        Object: map[string]interface{}{
            "apiVersion": "apps/v1",
            "kind":       "DeploymentRollback",
            "name":       service,
            "rollbackTo": map[string]interface{}{
                "revision": stableRev,
            },
        },
    }
    _, err = c.client.Resource(gvr).Namespace(namespace).Create(ctx, rollback, metav1.CreateOptions{})
    if err != nil {
        log.Printf("Rollback failed for service %s to revision %d: %v", service, stableRev, err)
        return fmt.Errorf("rollback failed: %v", err)
    }
    log.Printf("Successfully initiated rollback for service %s to revision %d", service, stableRev)
    return nil
}

func (c *Client) GetPodLogs(ctx context.Context, podName, namespace string) string {
    gvr := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
    req := c.client.Resource(gvr).Namespace(namespace).SubResource("log").Param("tailLines", "50")
    logs, err := req.DoRaw(ctx)
    if err != nil {
        log.Printf("Failed to get logs for pod %s in namespace %s: %v", podName, namespace, err)
        return fmt.Sprintf("Failed to get logs for pod %s: %v", podName, err)
    }
    return string(logs)
}

func (c *Client) GetPodsForDeployment(ctx context.Context, service, namespace string) []PodInfo {
    gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}
    rsList, err := c.client.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{
        LabelSelector: fmt.Sprintf("app=%s", service),
    })
    if err != nil {
        log.Printf("Failed to list replicasets for service %s: %v", service, err)
        return nil
    }
    var latestRS *unstructured.Unstructured
    var latestRevision int64
    for _, rs := range rsList.Items {
        revisionStr, found, _ := unstructured.NestedString(rs.Object, "metadata", "annotations", "deployment.kubernetes.io/revision")
        if !found {
            continue
        }
        revision, err := strconv.ParseInt(revisionStr, 10, 64)
        if err != nil {
            log.Printf("Failed to parse revision for replicaset %s: %v", rs.GetName(), err)
            continue
        }
        if revision > latestRevision {
            latestRevision = revision
            latestRS = &rs
        }
    }
    if latestRS == nil {
        log.Printf("No replicasets found for service %s", service)
        return nil
    }
    podGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
    podList, err := c.client.Resource(podGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
        LabelSelector: fmt.Sprintf("app=%s", service),
    })
    if err != nil {
        log.Printf("Failed to list pods for service %s: %v", service, err)
        return nil
    }
    var pods []PodInfo
    for _, pod := range podList.Items {
        ownerRefs, _, _ := unstructured.NestedSlice(pod.Object, "metadata", "ownerReferences")
        for _, owner := range ownerRefs {
            ownerMap, ok := owner.(map[string]interface{})
            if ok && ownerMap["kind"] == "ReplicaSet" && ownerMap["name"] == latestRS.GetName() {
                phase, found, _ := unstructured.NestedString(pod.Object, "status", "phase")
                if !found {
                    phase = "Unknown"
                }
                pods = append(pods, PodInfo{
                    Name: pod.GetName(),
                    Status: struct{ Phase string }{Phase: phase},
                })
            }
        }
    }
    return pods
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