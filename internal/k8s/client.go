package k8s

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"k8s-cicd/internal/config"
	"k8s-cicd/internal/storage"
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

func NewClient(cfg *config.Config) *Client {
	kubeconfig := cfg.KubeConfigPath
	if kubeconfig == "" {
		kubeconfig = clientcmd.RecommendedHomeFile
	}
	k8sCfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		k8sCfg, err = rest.InClusterConfig()
		if err != nil {
			log.Fatalf("Failed to get kube config: %v", err)
		}
	}
	client, err := dynamic.NewForConfig(k8sCfg)
	if err != nil {
		log.Fatalf("Failed to create k8s client: %v", err)
	}
	return &Client{client: client}
}

func (c *Client) GetNewImage(service, version string, namespace string) (string, error) {
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
	currentImage, _, _ := unstructured.NestedString(container, "image")
	if currentImage == "" {
		return "", fmt.Errorf("no image found in deployment")
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
	oldImage, _, _ := unstructured.NestedString(container, "image")

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
		message, _, _ := unstructured.NestedString(ev.Object, "message")
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

	podName := pods.Items[0].GetName()
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
			name, _, _ := unstructured.NestedString(e, "name")
			value, _, _ := unstructured.NestedString(e, "value")
			if name != "" && value != "" {
				result[name] = value
			}
		}
	}
	return result
}

func (c *Client) SendTelegramNotification(cfg *config.Config, result *storage.DeployResult) {
	token, ok := cfg.TelegramBots[result.Request.Service]
	if !ok {
		log.Printf("No bot for service: %s", result.Request.Service)
		return
	}

	chatID, ok := cfg.TelegramChats[result.Request.Service]
	if !ok {
		log.Printf("No chat for service: %s", result.Request.Service)
		return
	}

	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Printf("Failed to create bot: %v", err)
		return
	}

	var md strings.Builder
	if result.Success {
		md.WriteString(fmt.Sprintf(`
## 🚀 **Deployment Success**

**Service**: *%s*
**Environment**: *%s*
**New Version**: *%s*
**Old Image**: *%s*

✅ Deployment completed successfully!

---
**Deployed at**: %s
`, result.Request.Service, result.Request.Env, result.Request.Version, result.OldImage,
			result.Request.Timestamp.Format("2006-01-02 15:04:05")))
	} else {
		md.WriteString(fmt.Sprintf(`
## ❌ **Deployment Failed**

**Service**: *%s*
**Environment**: *%s*
**Version**: *%s*
**Error**: *%s*

### 🔍 **Diagnostics**

**Events**:
%s

**Environment Variables**:
`, result.Request.Service, result.Request.Env, result.Request.Version, result.ErrorMsg, result.Events))

		for k, v := range result.Envs {
			md.WriteString(fmt.Sprintf("• `%s`: %s\n", k, v))
		}

		md.WriteString(fmt.Sprintf(`
**Logs**: %s

⚠️ **Rollback completed**
---
**Failed at**: %s
`, result.Logs, result.Request.Timestamp.Format("2006-01-02 15:04:05")))
	}

	msg := tgbotapi.NewMessage(chatID, md.String())
	msg.ParseMode = "Markdown"
	_, err = bot.Send(msg)
	if err != nil {
		log.Printf("Failed to send telegram message: %v", err)
	}
}