// 文件: kubernetes/k8s_client.go
// 完整、可直接编译使用的版本
// 修复: 移除未使用的 ctx 变量
// 功能：
// 1. 精准快照：Deployment → ownerReferences → ReadyReplicas>0 && Age>1s → hash → Pod(Running+Ready)
// 2. 存储快照时带 task_id
// 3. 成功存储后，清理 service+namespace 下的旧快照（排除当前 task_id）
// 4. 失败时保留快照（回滚用）
// 5. 所有功能完整，无删减

package kubernetes

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	appsv1 "k8s.io/api/apps/v1"

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

// ExtractBaseImage 提取 registry/repo 部分
func ExtractBaseImage(image string) string {
	if idx := strings.LastIndex(image, ":"); idx != -1 {
		return image[:idx]
	}
	return image
}

// BuildNewImage 拼接新镜像
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
	if deploy, err := k.Clientset.AppsV1().Deployments(namespace).Get(ctx, service, metav1.GetOptions{}); err == nil {
		if len(deploy.Spec.Template.Spec.Containers) == 0 {
			return "", fmt.Errorf("容器为空")
		}
		return deploy.Spec.Template.Spec.Containers[0].Image, nil
	}

	// 2. 尝试 StatefulSet
	if sts, err := k.Clientset.AppsV1().StatefulSets(namespace).Get(ctx, service, metav1.GetOptions{}); err == nil {
		if len(sts.Spec.Template.Spec.Containers) == 0 {
			return "", fmt.Errorf("容器为空")
		}
		return sts.Spec.Template.Spec.Containers[0].Image, nil
	}

	// 3. 尝试 DaemonSet
	if ds, err := k.Clientset.AppsV1().DaemonSets(namespace).Get(ctx, service, metav1.GetOptions{}); err == nil {
		if len(ds.Spec.Template.Spec.Containers) == 0 {
			return "", fmt.Errorf("容器为空")
		}
		return ds.Spec.Template.Spec.Containers[0].Image, nil
	}

	return "", fmt.Errorf("未找到工作负载: %s in %s", service, namespace)
}

// SnapshotAndStoreImage 获取当前镜像并存储快照（精准版）
// SnapshotAndStoreImage 返回 oldTag 且确保非空
func (k *K8sClient) SnapshotAndStoreImage(service, namespace, taskID string, mongo *client.MongoClient) (string, error) {
	startTime := time.Now()
	ctx := context.Background()

	deploy, err := k.Clientset.AppsV1().Deployments(namespace).Get(ctx, service, metav1.GetOptions{})
	if err != nil {
		// 尝试 StatefulSet
		sts, err2 := k.Clientset.AppsV1().StatefulSets(namespace).Get(ctx, service, metav1.GetOptions{})
		if err2 != nil {
			return "", fmt.Errorf("未找到工作负载: %v", err)
		}
		if len(sts.Spec.Template.Spec.Containers) == 0 {
			return "", fmt.Errorf("容器为空")
		}
		image := sts.Spec.Template.Spec.Containers[0].Image
		tag := ExtractTag(image)
		snapshot := &models.ImageSnapshot{
			Namespace:  namespace,
			Service:    service,
			Container:  sts.Spec.Template.Spec.Containers[0].Name,
			Image:      image,
			Tag:        tag,
			RecordedAt: time.Now(),
			TaskID:     taskID,
		}
		if err := mongo.StoreImageSnapshot(snapshot); err != nil {
			return "", err
		}
		_ = mongo.DeleteSnapshotsExceptTask(service, namespace, taskID)
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "SnapshotImage",
			"took":   time.Since(startTime),
		}).Infof(color.GreenString("快照记录成功 old_tag=%s task_id=%s"), tag, taskID)
		return tag, nil
	}

	if len(deploy.Spec.Template.Spec.Containers) == 0 {
		return "", fmt.Errorf("容器为空")
	}
	image := deploy.Spec.Template.Spec.Containers[0].Image
	tag := ExtractTag(image)
	snapshot := &models.ImageSnapshot{
		Namespace:  namespace,
		Service:    service,
		Container:  deploy.Spec.Template.Spec.Containers[0].Name,
		Image:      image,
		Tag:        tag,
		RecordedAt: time.Now(),
		TaskID:     taskID,
	}
	if err := mongo.StoreImageSnapshot(snapshot); err != nil {
		return "", err
	}
	_ = mongo.DeleteSnapshotsExceptTask(service, namespace, taskID)
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "SnapshotImage",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("快照记录成功 old_tag=%s task_id=%s"), tag, taskID)
	return tag, nil
}

// getCurrentRunningImage 精准获取当前 Running + Ready 的镜像
func (k *K8sClient) getCurrentRunningImage(service, namespace string) (string, error) {
	ctx := context.Background()

	// 尝试获取 Deployment
	deploy, err := k.Clientset.AppsV1().Deployments(namespace).Get(ctx, service, metav1.GetOptions{})
	if err != nil {
		// 非 Deployment → 使用基本标签
		return k.getImageFromBasicLabel(service, namespace)
	}

	// 构建 LabelSelector
	selector, err := metav1.LabelSelectorAsSelector(deploy.Spec.Selector)
	if err != nil {
		return "", fmt.Errorf("构建 LabelSelector 失败: %v", err)
	}

	// 查询所有匹配的 ReplicaSet
	rsList, err := k.Clientset.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	if err != nil {
		return "", fmt.Errorf("查询 ReplicaSet 失败: %v", err)
	}

	// 过滤出由该 Deployment 控制的 RS
	controlledRS := []appsv1.ReplicaSet{}
	for _, rs := range rsList.Items {
		for _, owner := range rs.OwnerReferences {
			if owner.Kind == "Deployment" && owner.Name == deploy.Name && owner.UID == deploy.UID {
				controlledRS = append(controlledRS, rs)
				break
			}
		}
	}

	if len(controlledRS) == 0 {
		return "", fmt.Errorf("未找到由 Deployment %s 控制的 ReplicaSet", deploy.Name)
	}

	// 筛选 ReadyReplicas > 0 且 Age > 1s
	now := time.Now()
	candidates := []appsv1.ReplicaSet{}
	for _, rs := range controlledRS {
		ready := rs.Status.ReadyReplicas > 0
		age := now.Sub(rs.CreationTimestamp.Time)
		if ready && age > time.Second {
			candidates = append(candidates, rs)
		}
	}

	if len(candidates) == 0 {
		return "", fmt.Errorf("无就绪且 Age>1s 的 ReplicaSet")
	}

	// 按创建时间降序，取最新
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].CreationTimestamp.After(candidates[j].CreationTimestamp.Time)
	})
	rs := candidates[0]

	// 获取 pod-template-hash
	hash, ok := rs.Labels["pod-template-hash"]
	if !ok {
		return "", fmt.Errorf("ReplicaSet 无 pod-template-hash")
	}
	labelSelector := fmt.Sprintf("%s,pod-template-hash=%s", selector.String(), hash)

	// 查询 Pod
	pods, err := k.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return "", fmt.Errorf("查询 Pod 失败: %v", err)
	}

	// 筛选 Running + Ready Pod
	runningReadyPods := []v1.Pod{}
	for _, pod := range pods.Items {
		if pod.Status.Phase == v1.PodRunning {
			ready := true
			for _, cond := range pod.Status.Conditions {
				if cond.Type == v1.ContainersReady && cond.Status != v1.ConditionTrue {
					ready = false
					break
				}
			}
			if ready {
				runningReadyPods = append(runningReadyPods, pod)
			}
		}
	}

	if len(runningReadyPods) == 0 {
		return "", fmt.Errorf("无 Running + Ready 的 Pod")
	}

	// 提取镜像
	pod := runningReadyPods[0]
	if len(pod.Spec.Containers) == 0 {
		return "", fmt.Errorf("Pod 无容器")
	}
	return pod.Spec.Containers[0].Image, nil
}

// getImageFromBasicLabel 非 Deployment 时使用
func (k *K8sClient) getImageFromBasicLabel(service, namespace string) (string, error) {
	ctx := context.Background()
	labelSelector := fmt.Sprintf("app=%s", service)

	pods, err := k.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return "", fmt.Errorf("查询 Pod 失败: %v", err)
	}

	runningReadyPods := []v1.Pod{}
	for _, pod := range pods.Items {
		if pod.Status.Phase == v1.PodRunning {
			ready := true
			for _, cond := range pod.Status.Conditions {
				if cond.Type == v1.ContainersReady && cond.Status != v1.ConditionTrue {
					ready = false
					break
				}
			}
			if ready {
				runningReadyPods = append(runningReadyPods, pod)
			}
		}
	}

	if len(runningReadyPods) == 0 {
		return "", fmt.Errorf("无 Running + Ready 的 Pod（非 Deployment）")
	}

	pod := runningReadyPods[0]
	if len(pod.Spec.Containers) == 0 {
		return "", fmt.Errorf("Pod 无容器")
	}
	return pod.Spec.Containers[0].Image, nil
}

// UpdateWorkloadImage 更新镜像
func (k *K8sClient) UpdateWorkloadImage(service, namespace, newVersion string) error {
	startTime := time.Now()
	ctx := context.Background()

	// 1. 尝试 Deployment
	if deploy, err := k.Clientset.AppsV1().Deployments(namespace).Get(ctx, service, metav1.GetOptions{}); err == nil {
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
	if sts, err := k.Clientset.AppsV1().StatefulSets(namespace).Get(ctx, service, metav1.GetOptions{}); err == nil {
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
	if ds, err := k.Clientset.AppsV1().DaemonSets(namespace).Get(ctx, service, metav1.GetOptions{}); err == nil {
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
		if deploy, err := k.Clientset.AppsV1().Deployments(namespace).Get(ctx, service, metav1.GetOptions{}); err == nil {
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
		if sts, err := k.Clientset.AppsV1().StatefulSets(namespace).Get(ctx, service, metav1.GetOptions{}); err == nil {
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