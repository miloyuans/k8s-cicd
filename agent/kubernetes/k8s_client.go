package kubernetes

import (
	"context"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"k8s-cicd/agent/config"
	"k8s-cicd/agent/models"

	"github.com/fatih/color"
	"github.com/sirupsen/logrus"
)

// K8sClient Kubernetes 客户端
type K8sClient struct {
	Clientset *kubernetes.Clientset
	cfg       *config.DeployConfig
}

// NewK8sClient 创建 K8s 客户端
func NewK8sClient(k8sCfg *config.K8sAuthConfig, deployCfg *config.DeployConfig) (*K8sClient, error) {
	startTime := time.Now()

	var config *rest.Config
	var err error

	switch k8sCfg.AuthType {
	case "kubeconfig":
		config, err = clientcmd.BuildConfigFromFlags("", k8sCfg.Kubeconfig)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "NewK8sClient",
				"took":   time.Since(startTime),
			}).Errorf(color.RedString("kubeconfig 认证失败: %v"), err)
			return nil, fmt.Errorf("kubeconfig 认证失败: %v", err)
		}
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "NewK8sClient",
			"took":   time.Since(startTime),
		}).Infof(color.GreenString("使用 kubeconfig 认证成功"))
	case "serviceaccount":
		config, err = rest.InClusterConfig()
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "NewK8sClient",
				"took":   time.Since(startTime),
			}).Errorf(color.RedString("ServiceAccount 认证失败: %v"), err)
			return nil, fmt.Errorf("ServiceAccount 认证失败: %v", err)
		}
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "NewK8sClient",
			"took":   time.Since(startTime),
		}).Infof(color.GreenString("使用 ServiceAccount 认证成功"))
	default:
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "NewK8sClient",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("不支持的认证类型: %s"), k8sCfg.AuthType)
		return nil, fmt.Errorf("不支持的认证类型: %s", k8sCfg.AuthType)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "NewK8sClient",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("创建 K8s 客户端失败: %v"), err)
		return nil, fmt.Errorf("创建 K8s 客户端失败: %v", err)
	}

	client := &K8sClient{
		Clientset: clientset,
		cfg:       deployCfg,
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "NewK8sClient",
		"took":   time.Since(startTime),
	}).Info(color.GreenString("K8s 客户端创建成功"))
	return client, nil
}

// ExtractTag 提取镜像 tag
func ExtractTag(image string) string {
	if idx := strings.LastIndex(image, ":"); idx != -1 {
		return image[idx+1:]
	}
	return image
}

// extractBaseImage 提取 registry/repo 部分
func extractBaseImage(image string) string {
	if idx := strings.LastIndex(image, ":"); idx != -1 {
		return image[:idx]
	}
	return image
}

// buildNewImage 拼接新镜像
func buildNewImage(currentImage, newTag string) string {
	base := extractBaseImage(currentImage)
	if base == "" {
		return newTag
	}
	return fmt.Sprintf("%s:%s", base, newTag)
}

// DiscoverServicesFromNamespace 发现命名空间中的服务
func (k *K8sClient) DiscoverServicesFromNamespace(namespace string) ([]string, error) {
	startTime := time.Now()
	ctx := context.Background()

	services := []string{}

	// 1. 获取 Deployments
	deployments, err := k.Clientset.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		logrus.Errorf("获取 Deployment 失败: %v", err)
	} else {
		for _, d := range deployments.Items {
			for _, c := range d.Spec.Template.Spec.Containers {
				image := c.Image
				if image == "" {
					continue
				}
				serviceName := d.Name
				services = append(services, fmt.Sprintf("%s:%s", serviceName, ExtractTag(image)))
			}
		}
	}

	// 2. 获取 StatefulSets
	statefulSets, err := k.Clientset.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		logrus.Errorf("获取 StatefulSet 失败: %v", err)
	} else {
		for _, s := range statefulSets.Items {
			for _, c := range s.Spec.Template.Spec.Containers {
				image := c.Image
				if image == "" {
					continue
				}
				serviceName := s.Name
				services = append(services, fmt.Sprintf("%s:%s", serviceName, ExtractTag(image)))
			}
		}
	}

	// 3. 获取 DaemonSets
	daemonSets, err := k.Clientset.AppsV1().DaemonSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		logrus.Errorf("获取 DaemonSet 失败: %v", err)
	} else {
		for _, d := range daemonSets.Items {
			for _, c := range d.Spec.Template.Spec.Containers {
				image := c.Image
				if image == "" {
					continue
				}
				serviceName := d.Name
				services = append(services, fmt.Sprintf("%s:%s", serviceName, ExtractTag(image)))
			}
		}
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "DiscoverServicesFromNamespace",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("命名空间 [%s] 发现 %d 个服务"), namespace, len(services))
	return services, nil
}

// BuildPushRequest 构建推送请求
func (k *K8sClient) BuildPushRequest(cfg *config.Config) (models.PushRequest, error) {
	startTime := time.Now()
	serviceSet := make(map[string]struct{})
	envSet := make(map[string]struct{})

	for env, namespace := range cfg.EnvMapping.Mappings {
		nsServices, err := k.DiscoverServicesFromNamespace(namespace)
		if err != nil {
			logrus.Errorf("环境 [%s] 服务发现失败: %v", env, err)
			continue
		}

		envSet[env] = struct{}{}
		for _, serviceWithVersion := range nsServices {
			parts := strings.SplitN(serviceWithVersion, ":", 2)
			serviceName := parts[0]
			serviceSet[serviceName] = struct{}{}
		}
	}

	var services []string
	for s := range serviceSet {
		services = append(services, s)
	}
	var environments []string
	for e := range envSet {
		environments = append(environments, e)
	}

	if len(services) == 0 || len(environments) == 0 {
		return models.PushRequest{}, fmt.Errorf("services 或 environments 不能为空")
	}

	pushReq := models.PushRequest{
		Services:     services,
		Environments: environments,
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "BuildPushRequest",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("构建完成: %d 服务, %d 环境"), len(services), len(environments))
	return pushReq, nil
}

// CaptureImageSnapshot 捕获镜像快照
func (k *K8sClient) CaptureImageSnapshot(service, namespace string) (*models.ImageSnapshot, error) {
	startTime := time.Now()
	ctx := context.Background()

	snapshot := &models.ImageSnapshot{
		Namespace:  namespace,
		Service:    service,
		RecordedAt: time.Now(),
	}

	// 1. 尝试 Deployment
	deploy, err := k.Clientset.AppsV1().Deployments(namespace).Get(ctx, service, metav1.GetOptions{})
	if err == nil && len(deploy.Spec.Template.Spec.Containers) > 0 {
		c := deploy.Spec.Template.Spec.Containers[0]
		snapshot.Container = c.Name
		snapshot.Image = c.Image
		snapshot.Tag = ExtractTag(c.Image)
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "CaptureImageSnapshot",
			"took":   time.Since(startTime),
		}).Infof(color.GreenString("快照捕获成功 [Deployment]: %s -> %s"), service, c.Image)
		return snapshot, nil
	}

	// 2. 尝试 StatefulSet
	sts, err := k.Clientset.AppsV1().StatefulSets(namespace).Get(ctx, service, metav1.GetOptions{})
	if err == nil && len(sts.Spec.Template.Spec.Containers) > 0 {
		c := sts.Spec.Template.Spec.Containers[0]
		snapshot.Container = c.Name
		snapshot.Image = c.Image
  			snapshot.Tag = ExtractTag(c.Image)
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "CaptureImageSnapshot",
			"took":   time.Since(startTime),
		}).Infof(color.GreenString("快照捕获成功 [StatefulSet]: %s -> %s"), service, c.Image)
		return snapshot, nil
	}

	// 3. 尝试 DaemonSet
	ds, err := k.Clientset.AppsV1().DaemonSets(namespace).Get(ctx, service, metav1.GetOptions{})
	if err == nil && len(ds.Spec.Template.Spec.Containers) > 0 {
		c := ds.Spec.Template.Spec.Containers[0]
		snapshot.Container = c.Name
		snapshot.Image = c.Image
		snapshot.Tag = ExtractTag(c.Image)
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "CaptureImageSnapshot",
			"took":   time.Since(startTime),
		}).Infof(color.GreenString("快照捕获成功 [DaemonSet]: %s -> %s"), service, c.Image)
		return snapshot, nil
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "CaptureImageSnapshot",
		"took":   time.Since(startTime),
	}).Warnf(color.YellowString("未找到工作负载: %s in %s"), service, namespace)
	return nil, fmt.Errorf("未找到工作负载")
}

// UpdateWorkloadImage 更新镜像
func (k *K8sClient) UpdateWorkloadImage(service, namespace, newVersion string) error {
	startTime := time.Now()
	ctx := context.Background()

	// 1. 尝试 Deployment
	deploy, err := k.Clientset.AppsV1().Deployments(namespace).Get(ctx, service, metav1.GetOptions{})
	if err == nil {
		if len(deploy.Spec.Template.Spec.Containers) == 0 {
			return fmt.Errorf("容器为空")
		}
		oldImage := deploy.Spec.Template.Spec.Containers[0].Image
		newImage := buildNewImage(oldImage, newVersion)
		deploy.Spec.Template.Spec.Containers[0].Image = newImage
		_, err = k.Clientset.AppsV1().Deployments(namespace).Update(ctx, deploy, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "UpdateWorkloadImage",
			"took":   time.Since(startTime),
		}).Infof(color.GreenString("镜像更新成功 [Deployment]: %s -> %s"), oldImage, newImage)
		return nil
	}

	// 2. 尝试 StatefulSet
	sts, err := k.Clientset.AppsV1().StatefulSets(namespace).Get(ctx, service, metav1.GetOptions{})
	if err == nil {
		if len(sts.Spec.Template.Spec.Containers) == 0 {
			return fmt.Errorf("容器为空")
		}
		oldImage := sts.Spec.Template.Spec.Containers[0].Image
		newImage := buildNewImage(oldImage, newVersion)
		sts.Spec.Template.Spec.Containers[0].Image = newImage
		_, err = k.Clientset.AppsV1().StatefulSets(namespace).Update(ctx, sts, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "UpdateWorkloadImage",
			"took":   time.Since(startTime),
		}).Infof(color.GreenString("镜像更新成功 [StatefulSet]: %s -> %s"), oldImage, newImage)
		return nil
	}

	// 3. 尝试 DaemonSet
	ds, err := k.Clientset.AppsV1().DaemonSets(namespace).Get(ctx, service, metav1.GetOptions{})
	if err == nil {
		if len(ds.Spec.Template.Spec.Containers) == 0 {
			return fmt.Errorf("容器为空")
		}
		oldImage := ds.Spec.Template.Spec.Containers[0].Image
		newImage := buildNewImage(oldImage, newVersion)
		ds.Spec.Template.Spec.Containers[0].Image = newImage
		_, err = k.Clientset.AppsV1().DaemonSets(namespace).Update(ctx, ds, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "UpdateWorkloadImage",
			"took":   time.Since(startTime),
		}).Infof(color.GreenString("镜像更新成功 [DaemonSet]: %s -> %s"), oldImage, newImage)
		return nil
	}

	return fmt.Errorf("未找到工作负载: %s in %s", service, namespace)
}

// WaitForRolloutComplete 等待 rollout 完成
func (k *K8sClient) WaitForRolloutComplete(service, namespace string, timeout time.Duration) error {
	startTime := time.Now()
	ctx := context.Background()

	pollInterval := k.cfg.PollInterval
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		// 1. 检查 Deployment
		deploy, err := k.Clientset.AppsV1().Deployments(namespace).Get(ctx, service, metav1.GetOptions{})
		if err == nil {
			if deploy.Status.ObservedGeneration >= deploy.Generation &&
				deploy.Status.Replicas == deploy.Status.ReadyReplicas {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "WaitForRolloutComplete",
					"took":   time.Since(startTime),
				}).Infof(color.GreenString("Rollout 完成 [Deployment]: %s"), service)
				return nil
			}
		}

		// 2. 检查 StatefulSet
		sts, err := k.Clientset.AppsV1().StatefulSets(namespace).Get(ctx, service, metav1.GetOptions{})
		if err == nil {
			if sts.Status.ObservedGeneration >= sts.Generation &&
				sts.Status.Replicas == sts.Status.ReadyReplicas {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "WaitForRolloutComplete",
					"took":   time.Since(startTime),
				}).Infof(color.GreenString("Rollout 完成 [StatefulSet]: %s"), service)
				return nil
			}
		}

		time.Sleep(pollInterval)
	}

	return fmt.Errorf("rollout 超时: %s in %s", service, namespace)
}

// RollbackWithSnapshot 使用快照回滚
func (k *K8sClient) RollbackWithSnapshot(service, namespace string, snapshot *models.ImageSnapshot) error {
	if snapshot == nil || snapshot.Image == "" {
		return fmt.Errorf("无效快照")
	}
	return k.UpdateWorkloadImage(service, namespace, snapshot.Tag)
}