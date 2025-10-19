package k8s

import (
    "context"
    "fmt"
    "log"
    "os"
    "path/filepath"
    "strconv"
    "strings"
    "sync"
    "time"

    corev1 "k8s.io/api/core/v1"
    appsv1 "k8s.io/api/apps/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/client-go/dynamic"
    "k8s.io/client-go/kubernetes"
    "k8s.io/client-go/rest"
    "k8s.io/client-go/tools/clientcmd"
    "k8s.io/client-go/kubernetes/scheme"
    "k8s.io/apimachinery/pkg/runtime"
)

type Client struct {
    clientset     *kubernetes.Clientset
    dynamicClient dynamic.Interface
    restClient    *rest.RESTClient
    cache         sync.Map // Cache for deployment metadata
}

type PodInfo struct {
    Name   string
    Status struct {
        Phase string
    }
}

func NewClient(kubeconfig string) *Client {
    var k8sCfg *rest.Config
    var err error

    // Try in-cluster configuration first
    k8sCfg, err = rest.InClusterConfig()
    if err != nil {
        // Fallback to kubeconfig file
        if kubeconfig == "" {
            kubeconfig = filepath.Join(filepath.Dir(os.Args[0]), ".kubeconfig")
        }
        k8sCfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
        if err != nil {
            fmt.Fprintf(os.Stderr, "Failed to load kubeconfig from %s: %v\n", kubeconfig, err)
            os.Exit(1)
        }
        log.Printf("Using kubeconfig from %s", kubeconfig)
    } else {
        log.Println("Using in-cluster service account configuration")
    }

    // Create Kubernetes clientset
    clientset, err := kubernetes.NewForConfig(k8sCfg)
    if err != nil {
        fmt.Fprintf(os.Stderr, "Failed to create Kubernetes clientset: %v\n", err)
        os.Exit(1)
    }

    // Create dynamic client
    dynamicClient, err := dynamic.NewForConfig(k8sCfg)
    if err != nil {
        fmt.Fprintf(os.Stderr, "Failed to create dynamic client: %v\n", err)
        os.Exit(1)
    }

    // Create REST client for core/v1 API group
    restClientCfg := *k8sCfg
    restClientCfg.APIPath = "/api"
    restClientCfg.GroupVersion = &corev1.SchemeGroupVersion
    restScheme := runtime.NewScheme()
    if err := corev1.AddToScheme(restScheme); err != nil {
        fmt.Fprintf(os.Stderr, "Failed to add core/v1 to scheme: %v\n", err)
        os.Exit(1)
    }
    restClientCfg.NegotiatedSerializer = scheme.Codecs.WithoutConversion()
    restClient, err := rest.RESTClientFor(&restClientCfg)
    if err != nil {
        fmt.Fprintf(os.Stderr, "Failed to create REST client: %v\n", err)
        os.Exit(1)
    }

    return &Client{
        clientset:     clientset,
        dynamicClient: dynamicClient,
        restClient:    restClient,
    }
}

func (c *Client) Client() *kubernetes.Clientset {
    return c.clientset
}

func (c *Client) DynamicClient() dynamic.Interface {
    return c.dynamicClient
}

func (c *Client) GetDeployment(ctx context.Context, service, namespace string) (*appsv1.Deployment, error) {
    cacheKey := fmt.Sprintf("deployment:%s:%s", namespace, service)
    if cached, found := c.cache.Load(cacheKey); found {
        if dep, ok := cached.(*appsv1.Deployment); ok {
            return dep, nil
        }
    }
    dep, err := c.clientset.AppsV1().Deployments(namespace).Get(ctx, service, metav1.GetOptions{})
    if err != nil {
        return nil, fmt.Errorf("failed to get deployment: %v", err)
    }
    c.cache.Store(cacheKey, dep)
    go func() {
        time.Sleep(1 * time.Minute)
        c.cache.Delete(cacheKey)
    }()
    return dep, nil
}

func (c *Client) GetNewImage(service, version, namespace string) (string, error) {
    dep, err := c.GetDeployment(context.Background(), service, namespace)
    if err != nil {
        return "", err
    }

    if len(dep.Spec.Template.Spec.Containers) == 0 {
        return "", fmt.Errorf("no containers found in deployment")
    }

    currentImage := dep.Spec.Template.Spec.Containers[0].Image
    parts := strings.Split(currentImage, ":")
    if len(parts) != 2 {
        return "", fmt.Errorf("invalid image format: %s", currentImage)
    }
    baseImage := parts[0]
    newImage := fmt.Sprintf("%s:%s", baseImage, version)

    return newImage, nil
}

func (c *Client) UpdateDeployment(ctx context.Context, service, newImage, namespace string) (bool, string, string) {
    const maxRetries = 3
    var dep *appsv1.Deployment
    var err error
    for attempt := 1; attempt <= maxRetries; attempt++ {
        dep, err = c.GetDeployment(ctx, service, namespace)
        if err == nil {
            break
        }
        log.Printf("Failed to get deployment for service %s (attempt %d/%d): %v", service, attempt, maxRetries, err)
        if attempt < maxRetries {
            time.Sleep(2 * time.Second)
        }
    }
    if err != nil {
        return false, fmt.Sprintf("Failed to get deployment after %d attempts: %v", maxRetries, err), ""
    }

    if len(dep.Spec.Template.Spec.Containers) == 0 {
        return false, "No containers found", ""
    }

    oldImage := dep.Spec.Template.Spec.Containers[0].Image
    dep.Spec.Template.Spec.Containers[0].Image = newImage
    if dep.Annotations == nil {
        dep.Annotations = make(map[string]string)
    }
    dep.Annotations["cicd/previous-image"] = oldImage

    for attempt := 1; attempt <= maxRetries; attempt++ {
        _, err = c.clientset.AppsV1().Deployments(namespace).Update(ctx, dep, metav1.UpdateOptions{})
        if err == nil {
            break
        }
        log.Printf("Failed to update deployment for service %s (attempt %d/%d): %v", service, attempt, maxRetries, err)
        if attempt < maxRetries {
            time.Sleep(2 * time.Second)
        }
    }
    if err != nil {
        return false, fmt.Sprintf("Failed to update deployment after %d attempts: %v", maxRetries, err), oldImage
    }

    return true, "", oldImage
}

func (c *Client) CheckNewPodStatus(ctx context.Context, service, newImage, namespace string) (bool, error) {
    watcher, err := c.clientset.CoreV1().Pods(namespace).Watch(ctx, metav1.ListOptions{
        LabelSelector: fmt.Sprintf("app=%s", service),
    })
    if err != nil {
        return false, fmt.Errorf("failed to watch pods: %v", err)
    }
    defer watcher.Stop()

    timeout := time.After(2 * time.Minute)
    for {
        select {
        case event, ok := <-watcher.ResultChan():
            if !ok {
                return false, fmt.Errorf("watch channel closed")
            }
            pod, ok := event.Object.(*corev1.Pod)
            if !ok || len(pod.Spec.Containers) == 0 || pod.Spec.Containers[0].Image != newImage {
                continue
            }
            ready := false
            for _, cond := range pod.Status.Conditions {
                if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
                    ready = true
                    break
                }
            }
            if pod.Status.Phase == corev1.PodRunning && ready {
                return true, nil
            }
            if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == "CrashLoopBackOff" || pod.Status.Phase == "Evicted" {
                return false, fmt.Errorf("pod %s in error state: %s", pod.Name, pod.Status.Phase)
            }
        case <-timeout:
            return false, fmt.Errorf("timeout waiting for pods to become ready")
        case <-ctx.Done():
            return false, fmt.Errorf("context cancelled: %v", ctx.Err())
        }
    }
}

func (c *Client) GetPodEvents(ctx context.Context, service, namespace string) string {
    const maxRetries = 3
    var eventsStr strings.Builder
    for attempt := 1; attempt <= maxRetries; attempt++ {
        events, err := c.clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
            FieldSelector: fmt.Sprintf("involvedObject.kind=Pod,reason!=Scheduled"),
        })
        if err != nil {
            log.Printf("Failed to get pod events for service %s in namespace %s (attempt %d/%d): %v", service, namespace, attempt, maxRetries, err)
            if attempt == maxRetries {
                return fmt.Sprintf("Failed to get pod events after %d attempts: %v", maxRetries, err)
            }
            time.Sleep(2 * time.Second)
            continue
        }
        for _, ev := range events.Items {
            if strings.Contains(ev.InvolvedObject.Name, service) {
                eventsStr.WriteString(fmt.Sprintf("• %s\n", ev.Message))
            }
        }
        if eventsStr.Len() == 0 {
            return "No relevant events found"
        }
        return eventsStr.String()
    }
    return "No relevant events found"
}

func (c *Client) GetLatestStableRevision(ctx context.Context, service, namespace string) (int64, error) {
    const maxRetries = 3
    var rsList *appsv1.ReplicaSetList
    var err error
    for attempt := 1; attempt <= maxRetries; attempt++ {
        rsList, err = c.clientset.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{
            LabelSelector: fmt.Sprintf("app=%s", service),
        })
        if err == nil {
            break
        }
        log.Printf("Failed to list replicasets for service %s (attempt %d/%d): %v", service, attempt, maxRetries, err)
        if attempt < maxRetries {
            time.Sleep(2 * time.Second)
        }
    }
    if err != nil {
        return 0, fmt.Errorf("failed to list replicasets after %d attempts: %v", maxRetries, err)
    }
    var latestStable int64
    for _, rs := range rsList.Items {
        revisionStr, ok := rs.Annotations["deployment.kubernetes.io/revision"]
        if !ok {
            continue
        }
        revision, err := strconv.ParseInt(revisionStr, 10, 64)
        if err != nil {
            log.Printf("Failed to parse revision for replicaset %s: %v", rs.Name, err)
            continue
        }
        isStable := rs.Status.Replicas == rs.Status.ReadyReplicas && rs.Status.ReadyReplicas > 0
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
    const maxRetries = 3
    var dep *appsv1.Deployment
    var err error
    for attempt := 1; attempt <= maxRetries; attempt++ {
        dep, err = c.GetDeployment(ctx, service, namespace)
        if err == nil {
            break
        }
        log.Printf("Failed to get deployment for rollback for service %s (attempt %d/%d): %v", service, attempt, maxRetries, err)
        if attempt < maxRetries {
            time.Sleep(2 * time.Second)
        }
    }
    if err != nil {
        return fmt.Errorf("failed to get deployment for rollback after %d attempts: %v", maxRetries, err)
    }

    stableRev, err := c.GetLatestStableRevision(ctx, service, namespace)
    if err != nil || stableRev == 0 {
        log.Printf("No stable revision found for service %s: %v", service, err)
        if oldImage, ok := dep.Annotations["cicd/previous-image"]; ok && oldImage != "" {
            dep.Spec.Template.Spec.Containers[0].Image = oldImage
        } else {
            return fmt.Errorf("no stable revision or previous image found")
        }
    } else {
        if dep.Spec.Template.Annotations == nil {
            dep.Spec.Template.Annotations = make(map[string]string)
        }
        dep.Spec.Template.Annotations["deployment.kubernetes.io/revision"] = fmt.Sprintf("%d", stableRev)
    }

    for attempt := 1; attempt <= maxRetries; attempt++ {
        _, err = c.clientset.AppsV1().Deployments(namespace).Update(ctx, dep, metav1.UpdateOptions{})
        if err == nil {
            break
        }
        log.Printf("Rollback failed for service %s to revision %d (attempt %d/%d): %v", service, stableRev, attempt, maxRetries, err)
        if attempt < maxRetries {
            time.Sleep(2 * time.Second)
        }
    }
    if err != nil {
        return fmt.Errorf("rollback failed after %d attempts: %v", maxRetries, err)
    }
    log.Printf("Successfully initiated rollback for service %s to revision %d", service, stableRev)
    return nil
}

func (c *Client) GetPodLogs(ctx context.Context, podName, namespace string) string {
    const maxRetries = 3
    for attempt := 1; attempt <= maxRetries; attempt++ {
        logs, err := c.restClient.Get().
            Namespace(namespace).
            Resource("pods").
            Name(podName).
            SubResource("log").
            Param("tailLines", "50").
            DoRaw(ctx)
        if err != nil {
            log.Printf("Failed to get logs for pod %s in namespace %s (attempt %d/%d): %v", podName, namespace, attempt, maxRetries, err)
            if attempt == maxRetries {
                return fmt.Sprintf("Failed to get logs for pod %s after %d attempts: %v", podName, maxRetries, err)
            }
            time.Sleep(2 * time.Second)
            continue
        }
        return string(logs)
    }
    return "Failed to get logs after retries"
}

func (c *Client) GetPodsForDeployment(ctx context.Context, service, namespace string) []PodInfo {
    const maxRetries = 3
    for attempt := 1; attempt <= maxRetries; attempt++ {
        rsList, err := c.clientset.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{
            LabelSelector: fmt.Sprintf("app=%s", service),
        })
        if err != nil {
            log.Printf("Failed to list replicasets for service %s (attempt %d/%d): %v", service, attempt, maxRetries, err)
            if attempt == maxRetries {
                return nil
            }
            time.Sleep(2 * time.Second)
            continue
        }
        var latestRS *appsv1.ReplicaSet
        var latestRevision int64
        for _, rs := range rsList.Items {
            revisionStr, ok := rs.Annotations["deployment.kubernetes.io/revision"]
            if !ok {
                continue
            }
            revision, err := strconv.ParseInt(revisionStr, 10, 64)
            if err != nil {
                log.Printf("Failed to parse revision for replicaset %s: %v", rs.Name, err)
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
        podList, err := c.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
            LabelSelector: fmt.Sprintf("app=%s", service),
        })
        if err != nil {
            log.Printf("Failed to list pods for service %s (attempt %d/%d): %v", service, attempt, maxRetries, err)
            if attempt == maxRetries {
                return nil
            }
            time.Sleep(2 * time.Second)
            continue
        }
        var pods []PodInfo
        for _, pod := range podList.Items {
            for _, owner := range pod.OwnerReferences {
                if owner.Kind == "ReplicaSet" && owner.Name == latestRS.Name {
                    phase := string(pod.Status.Phase)
                    if phase == "" {
                        phase = "Unknown"
                    }
                    pods = append(pods, PodInfo{
                        Name:   pod.Name,
                        Status: struct{ Phase string }{Phase: phase},
                    })
                }
            }
        }
        return pods
    }
    return nil
}

func (c *Client) RestartDeployment(ctx context.Context, service, namespace string) error {
    const maxRetries = 3
    var dep *appsv1.Deployment
    var err error
    for attempt := 1; attempt <= maxRetries; attempt++ {
        dep, err = c.GetDeployment(ctx, service, namespace)
        if err == nil {
            break
        }
        log.Printf("Failed to get deployment for restart for service %s (attempt %d/%d): %v", service, attempt, maxRetries, err)
        if attempt < maxRetries {
            time.Sleep(2 * time.Second)
        }
    }
    if err != nil {
        return fmt.Errorf("failed to get deployment for restart after %d attempts: %v", maxRetries, err)
    }

    if dep.Spec.Template.Annotations == nil {
        dep.Spec.Template.Annotations = make(map[string]string)
    }
    dep.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().Format(time.RFC3339)

    for attempt := 1; attempt <= maxRetries; attempt++ {
        _, err = c.clientset.AppsV1().Deployments(namespace).Update(ctx, dep, metav1.UpdateOptions{})
        if err == nil {
            break
        }
        log.Printf("Failed to restart deployment for service %s (attempt %d/%d): %v", service, attempt, maxRetries, err)
        if attempt < maxRetries {
            time.Sleep(2 * time.Second)
        }
    }
    if err != nil {
        return fmt.Errorf("failed to restart deployment after %d attempts: %v", maxRetries, err)
    }
    return nil
}