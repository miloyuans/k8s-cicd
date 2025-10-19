package k8s

import (
    "context"
    "fmt"
    "log"
    "os"
    "path/filepath"
    "strconv"
    "strings"
    "time"

    corev1 "k8s.io/api/core/v1"
    appsv1 "k8s.io/api/apps/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/client-go/kubernetes"
    "k8s.io/client-go/rest"
    "k8s.io/client-go/tools/clientcmd"
)

type Client struct {
    clientset  *kubernetes.Clientset
    restClient *rest.RESTClient // For log retrieval
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
            // Default to .kubeconfig in the same directory as the code
            kubeconfig = filepath.Join(filepath.Dir(os.Args[0]), ".kubeconfig")
        }
        k8sCfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
        if err != nil {
            // If kubeconfig file doesn't exist or is invalid, log and exit
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

    // Create REST client for logs
    restClient, err := rest.RESTClientFor(k8sCfg)
    if err != nil {
        fmt.Fprintf(os.Stderr, "Failed to create REST client: %v\n", err)
        os.Exit(1)
    }

    return &Client{
        clientset:  clientset,
        restClient: restClient,
    }
}

func (c *Client) Client() *kubernetes.Clientset {
    return c.clientset
}

func (c *Client) GetNewImage(service, version, namespace string) (string, error) {
    dep, err := c.clientset.AppsV1().Deployments(namespace).Get(context.Background(), service, metav1.GetOptions{})
    if err != nil {
        return "", fmt.Errorf("failed to get deployment: %v", err)
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
    dep, err := c.clientset.AppsV1().Deployments(namespace).Get(ctx, service, metav1.GetOptions{})
    if err != nil {
        return false, fmt.Sprintf("Failed to get deployment: %v", err), ""
    }

    if len(dep.Spec.Template.Spec.Containers) == 0 {
        return false, "No containers found", ""
    }

    oldImage := dep.Spec.Template.Spec.Containers[0].Image
    dep.Spec.Template.Spec.Containers[0].Image = newImage

    _, err = c.clientset.AppsV1().Deployments(namespace).Update(ctx, dep, metav1.UpdateOptions{})
    if err != nil {
        return false, fmt.Sprintf("Failed to update deployment: %v", err), oldImage
    }

    return true, "", oldImage
}

func (c *Client) CheckNewPodStatus(ctx context.Context, service, newImage, namespace string) (bool, error) {
    const maxAttempts = 10
    const interval = 5 * time.Second

    var errMsg strings.Builder
    for attempt := 1; attempt <= maxAttempts; attempt++ {
        pods, err := c.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
            LabelSelector: fmt.Sprintf("app=%s", service),
        })
        if err != nil {
            return false, fmt.Errorf("failed to list pods: %v", err)
        }

        newPodsReady := true
        for _, pod := range pods.Items {
            if len(pod.Spec.Containers) == 0 || pod.Spec.Containers[0].Image != newImage {
                continue
            }
            ready := false
            for _, cond := range pod.Status.Conditions {
                if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
                    ready = true
                    break
                }
            }
            phase := pod.Status.Phase
            if phase == corev1.PodFailed || phase == "CrashLoopBackOff" || phase == "Evicted" {
                newPodsReady = false
                errMsg.WriteString(fmt.Sprintf("Pod %s in error state: %s\n", pod.Name, phase))
            } else if phase != corev1.PodRunning || !ready {
                newPodsReady = false
                errMsg.WriteString(fmt.Sprintf("Pod %s not ready (phase: %s, ready: %v)\n", pod.Name, phase, ready))
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

func (c *Client) GetPodEvents(ctx context.Context, service, namespace string) string {
    events, err := c.clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
        FieldSelector: fmt.Sprintf("involvedObject.kind=Pod,reason!=Scheduled"),
    })
    if err != nil {
        log.Printf("Failed to get pod events for service %s in namespace %s: %v", service, namespace, err)
        return fmt.Sprintf("Failed to get pod events: %v", err)
    }

    var eventsStr strings.Builder
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

func (c *Client) GetLatestStableRevision(ctx context.Context, service, namespace string) (int64, error) {
    rsList, err := c.clientset.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{
        LabelSelector: fmt.Sprintf("app=%s", service),
    })
    if err != nil {
        log.Printf("Failed to list replicasets for service %s: %v", service, err)
        return 0, fmt.Errorf("failed to list replicasets: %v", err)
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
    stableRev, err := c.GetLatestStableRevision(ctx, service, namespace)
    if err != nil || stableRev == 0 {
        log.Printf("No stable revision found for service %s: %v", service, err)
        return fmt.Errorf("no stable revision found: %v", err)
    }

    dep, err := c.clientset.AppsV1().Deployments(namespace).Get(ctx, service, metav1.GetOptions{})
    if err != nil {
        return fmt.Errorf("failed to get deployment for rollback: %v", err)
    }

    if dep.Spec.Template.Annotations == nil {
        dep.Spec.Template.Annotations = make(map[string]string)
    }
    dep.Spec.Template.Annotations["deployment.kubernetes.io/revision"] = fmt.Sprintf("%d", stableRev)

    _, err = c.clientset.AppsV1().Deployments(namespace).Update(ctx, dep, metav1.UpdateOptions{})
    if err != nil {
        log.Printf("Rollback failed for service %s to revision %d: %v", service, stableRev, err)
        return fmt.Errorf("rollback failed: %v", err)
    }
    log.Printf("Successfully initiated rollback for service %s to revision %d", service, stableRev)
    return nil
}

func (c *Client) GetPodLogs(ctx context.Context, podName, namespace string) string {
    logs, err := c.restClient.Get().
        Namespace(namespace).
        Resource("pods").
        Name(podName).
        SubResource("log").
        Param("tailLines", "50").
        DoRaw(ctx)
    if err != nil {
        log.Printf("Failed to get logs for pod %s in namespace %s: %v", podName, namespace, err)
        return fmt.Sprintf("Failed to get logs for pod %s: %v", podName, err)
    }
    return string(logs)
}

func (c *Client) GetPodsForDeployment(ctx context.Context, service, namespace string) []PodInfo {
    rsList, err := c.clientset.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{
        LabelSelector: fmt.Sprintf("app=%s", service),
    })
    if err != nil {
        log.Printf("Failed to list replicasets for service %s: %v", service, err)
        return nil
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
        log.Printf("Failed to list pods for service %s: %v", service, err)
        return nil
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
                    Name: pod.Name,
                    Status: struct{ Phase string }{Phase: phase},
                })
            }
        }
    }
    return pods
}

func (c *Client) RestartDeployment(ctx context.Context, service, namespace string) error {
    dep, err := c.clientset.AppsV1().Deployments(namespace).Get(ctx, service, metav1.GetOptions{})
    if err != nil {
        return fmt.Errorf("failed to get deployment for restart: %v", err)
    }

    if dep.Spec.Template.Annotations == nil {
        dep.Spec.Template.Annotations = make(map[string]string)
    }
    dep.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().Format(time.RFC3339)

    _, err = c.clientset.AppsV1().Deployments(namespace).Update(ctx, dep, metav1.UpdateOptions{})
    if err != nil {
        return fmt.Errorf("failed to restart deployment: %v", err)
    }
    return nil
}