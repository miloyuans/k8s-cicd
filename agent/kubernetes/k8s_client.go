// 文件: kubernetes/k8s_client.go
// 修改: SnapshotAndStoreImage 升级为：仅记录 Running 状态的 Pod 镜像作为旧版本快照
// 1. 获取所有 Pod
// 2. 过滤出 Running 状态的 Pod
// 3. 若无 Running Pod → 返回错误，任务失败
// 4. 若有多个 Running Pod，取第一个容器的镜像（假设一致）
// 5. 存储快照并返回旧 tag

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

	"k8s-cicd/agent/client"
	"k8s-cicd/agent/config"
	"k8s-cicd/agent/models"

	"github.com/fatih/color"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
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

// ExtractBaseImage 提取 registry/repo 部分（导出）
func ExtractBaseImage(image string) string {
	if idx := strings.LastIndex(image, ":"); idx != -1 {
		return image[:idx]
	}
	return image
}

// BuildNewImage 拼接新镜像（导出）
func BuildNewImage(currentImage, newTag string) string {
	base := ExtractBaseImage(currentImage)
	if base == "" {
		return newTag
	}
	return fmt.Sprintf("%s:%s", base, newTag)
}

// DiscoverServicesFromNamespace 发现命名空间中的服务（仅返回服务名，去重，不含 tag）
func (k *K8sClient) DiscoverServicesFromNamespace(namespace string) ([]string, error) {
	startTime := time.Now()
	ctx := context.Background()

	serviceSet := make(map[string]struct{})

	// 1. 获取 Deployments
	deployments, err := k.Clientset.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		logrus.Errorf("获取 Deployment 失败: %v", err)
	} else {
		for _, d := range deployments.Items {
			serviceSet[d.Name] = struct{}{}
		}
	}

	// 2. 获取 StatefulSets
	statefulSets, err := k.Clientset.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		logrus.Errorf("获取 StatefulSet 失败: %v", err)
	} else {
		for _, s := range statefulSets.Items {
			serviceSet[s.Name] = struct{}{}
		}
	}

	// 3. 获取 DaemonSets
	daemonSets, err := k.Clientset.AppsV1().DaemonSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		logrus.Errorf("获取 DaemonSet 失败: %v", err)
	} else {
		for _, ds := range daemonSets.Items {
			serviceSet[ds.Name] = struct{}{}
		}
	}

	services := make([]string, 0, len(serviceSet))
	for s := range serviceSet {
		services = append(services, s)
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "DiscoverServicesFromNamespace",
		"took":   time.Since(startTime),
		"data": logrus.Fields{
			"namespace": namespace,
			"count":     len(services),
		},
	}).Infof(color.GreenString("命名空间 [%s] 发现 %d 个服务"), namespace, len(services))
	return services, nil
}

// BuildPushRequest 构建推送请求（遍历环境映射发现服务）
func (k *K8sClient) BuildPushRequest(cfg *config.Config) (models.PushRequest, error) {
	startTime := time.Now()

	serviceSet := make(map[string]struct{})
	envSet := make(map[string]struct{})

	for env, namespace := range cfg.EnvMapping.Mappings {
		envSet[env] = struct{}{}
		services, err := k.DiscoverServicesFromNamespace(namespace)
		if err != nil {
			logrus.Errorf("发现服务失败 [%s]: %v", env, err)
			continue
		}
		for _, s := range services {
			serviceSet[s] = struct{}{}
		}
	}

	services := make([]string, 0, len(serviceSet))
	for s := range serviceSet {
		services = append(services, s)
	}
	envs := make([]string, 0, len(envSet))
	for e := range envSet {
		envs = append(envs, e)
	}

	req := models.PushRequest{
		Services:     services,
		Environments: envs,
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "BuildPushRequest",
		"took":   time.Since(startTime),
		"data": logrus.Fields{
			"service_count": len(services),
			"env_count":     len(envs),
		},
	}).Infof(color.GreenString("构建完成: %d 服务, %d 环境"), len(services), len(envs))
	return req, nil
}

// GetCurrentImage 获取当前镜像（从 Deployment/StatefulSet/DaemonSet 获取旧镜像）
func (k *K8sClient) GetCurrentImage(service, namespace string) (string, error) {
	ctx := context.Background()

	// 1. 尝试 Deployment
	deploy, err := k.Clientset.AppsV1().Deployments(namespace).Get(ctx, service, metav1.GetOptions{})
	if err == nil {
		if len(deploy.Spec.Template.Spec.Containers) == 0 {
			return "", fmt.Errorf("容器为空")
		}
		return deploy.Spec.Template.Spec.Containers[0].Image, nil
	}

	// 2. 尝试 StatefulSet
	sts, err := k.Clientset.AppsV1().StatefulSets(namespace).Get(ctx, service, metav1.GetOptions{})
	if err == nil {
		if len(sts.Spec.Template.Spec.Containers) == 0 {
			return "", fmt.Errorf("容器为空")
		}
		return sts.Spec.Template.Spec.Containers[0].Image, nil
	}

	// 3. 尝试 DaemonSet
	ds, err := k.Clientset.AppsV1().DaemonSets(namespace).Get(ctx, service, metav1.GetOptions{})
	if err == nil {
		if len(ds.Spec.Template.Spec.Containers) == 0 {
			return "", fmt.Errorf("容器为空")
		}
		return ds.Spec.Template.Spec.Containers[0].Image, nil
	}

	return "", fmt.Errorf("未找到工作负载: %s in %s", service, namespace)
}

// SnapshotAndStoreImage 获取当前镜像并存储快照到 Mongo（仅 Running Pod）
func (k *K8sClient) SnapshotAndStoreImage(service, namespace, taskID string, mongo *client.MongoClient) (string, error) {
	ctx := context.Background()
	labelSelector := fmt.Sprintf("app=%s", service)

	// 1. 获取所有 Pod
	pods, err := k.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return "", fmt.Errorf("查询 Pod 失败: %v", err)
	}

	// 2. 过滤 Running 状态的 Pod
	runningPods := []v1.Pod{}
	for _, pod := range pods.Items {
		if pod.Status.Phase == v1.PodRunning {
			runningPods = append(runningPods, pod)
		}
	}

	// 3. 无 Running Pod → 失败
	if len(runningPods) == 0 {
		return "", fmt.Errorf("当前服务无 Running 状态的 Pod，无法记录快照")
	}

	// 4. 取第一个 Running Pod 的镜像（假设一致）
	pod := runningPods[0]
	if len(pod.Spec.Containers) == 0 {
		return "", fmt.Errorf("Running Pod 无容器")
	}
	oldImage := pod.Spec.Containers[0].Image
	oldTag := ExtractTag(oldImage)

	// 5. 存储快照
	snapshot := &models.ImageSnapshot{
		Service:    service,
		Namespace:  namespace,
		Container:  "default",
		Image:      oldImage,
		Tag:        oldTag,
		RecordedAt: time.Now(),
		TaskID:     taskID,
	}

	if err := mongo.SnapshotImage(snapshot); err != nil {
		return oldTag, err
	}

	logrus.WithFields(logrus.Fields{
		"task_id": taskID,
		"service": service,
		"old_image": oldImage,
	}).Info("快照记录成功（基于 Running Pod）")
	return oldTag, nil
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
		newImage := BuildNewImage(oldImage, newVersion)
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
		newImage := BuildNewImage(oldImage, newVersion)
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
		newImage := BuildNewImage(oldImage, newVersion)
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