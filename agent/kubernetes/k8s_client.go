// k8s_client.go
package kubernetes

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"

	"k8s-cicd/agent/config"
	"k8s-cicd/agent/models"
	"github.com/fatih/color"
	"github.com/sirupsen/logrus"
)

// ======================
// 工具函数
// ======================

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

// buildNewImage 拼接新镜像：registry/repo + 新 tag
func buildNewImage(currentImage, newTag string) string {
	base := extractBaseImage(currentImage)
	if base == "" {
		return newTag
	}
	return fmt.Sprintf("%s:%s", base, newTag)
}

// ======================
// K8sClient
// ======================
type K8sClient struct {
	Clientset *kubernetes.Clientset
	cfg       *config.DeployConfig
}

// NewK8sClient 根据配置创建K8s客户端
func NewK8sClient(k8sCfg *config.K8sAuthConfig, deployCfg *config.DeployConfig) (*K8sClient, error) {
	startTime := time.Now()
	// 步骤1：根据认证类型选择配置方式
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
			}).Errorf(color.RedString("kubeconfig认证失败: %v", err))
			return nil, fmt.Errorf("kubeconfig认证失败: %v", err)
		}
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "NewK8sClient",
			"took":   time.Since(startTime),
		}).Infof(color.GreenString("使用kubeconfig认证成功"))
	case "serviceaccount":
		config, err = rest.InClusterConfig()
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "NewK8sClient",
				"took":   time.Since(startTime),
			}).Errorf(color.RedString("ServiceAccount认证失败: %v", err))
			return nil, fmt.Errorf("ServiceAccount认证失败: %v", err)
		}
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "NewK8sClient",
			"took":   time.Since(startTime),
		}).Infof(color.GreenString("使用ServiceAccount认证成功"))
	default:
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "NewK8sClient",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("不支持的认证类型: %s", k8sCfg.AuthType))
		return nil, fmt.Errorf("不支持的认证类型: %s", k8sCfg.AuthType)
	}

	// 步骤2：创建clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "NewK8sClient",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("创建K8s客户端失败: %v", err))
		return nil, fmt.Errorf("创建K8s客户端失败: %v", err)
	}

	// 步骤3：返回客户端
	client := &K8sClient{
		Clientset: clientset,
		cfg:       deployCfg,
	}
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "NewK8sClient",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("K8s客户端创建成功"))
	return client, nil
}

// ======================
// UpdateWorkloadImage（核心修复）
// ======================
// UpdateWorkloadImage 返回快照，用于回滚
func (k *K8sClient) UpdateWorkloadImage(namespace, serviceName, newTag string) error {
	startTime := time.Now()

	// 步骤1：获取运行中 Pod 镜像快照
	snapshot, err := k.captureRunningImageSnapshot(namespace, serviceName)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "UpdateWorkloadImage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("获取运行镜像快照失败: %v", err))
		return err
	}
	if snapshot == nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "UpdateWorkloadImage",
			"took":   time.Since(startTime),
		}).Warnf(color.YellowString("无运行 Pod，跳过更新: %s [%s]", serviceName, namespace))
		return nil
	}

	// 步骤2：构建新镜像
	newImage := buildNewImage(snapshot.Image, newTag)

	// 步骤3：尝试更新 Deployment
	if deploy, err := k.Clientset.AppsV1().Deployments(namespace).Get(context.TODO(), serviceName, metav1.GetOptions{}); err == nil {
		return k.updateDeploymentImage(deploy, newImage, namespace, serviceName, startTime, snapshot)
	}

	// 步骤4：尝试更新 DaemonSet
	if ds, err := k.Clientset.AppsV1().DaemonSets(namespace).Get(context.TODO(), serviceName, metav1.GetOptions{}); err == nil {
		return k.updateDaemonSetImage(ds, newImage, namespace, serviceName, startTime, snapshot)
	}

	err = fmt.Errorf("未找到工作负载: %s [%s]", serviceName, namespace)
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "UpdateWorkloadImage",
		"took":   time.Since(startTime),
	}).Errorf(color.RedString("%v", err))
	return err
}

// ======================
// captureRunningImageSnapshot 获取 Running Pod 镜像
// ======================
func (k *K8sClient) captureRunningImageSnapshot(namespace, serviceName string) (*models.ImageSnapshot, error) {
	pods, err := k.Clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", serviceName),
	})
	if err != nil {
		return nil, err
	}

	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, status := range pod.Status.ContainerStatuses {
			if !status.Ready {
				continue
			}
			image := status.Image
			tag := ExtractTag(image)
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "captureRunningImageSnapshot",
			}).Infof("捕获运行镜像: %s (tag: %s)", image, tag)
			return &models.ImageSnapshot{
				Namespace:  namespace,
				Service:    serviceName,
				Container:  status.Name,
				Image:      image,
				Tag:        tag,
				RecordedAt: time.Now(),
			}, nil
		}
	}
	return nil, nil
}

// ======================
// RollbackWithSnapshot 使用快照回滚
// ======================
func (k *K8sClient) RollbackWithSnapshot(snapshot *models.ImageSnapshot) error {
	if snapshot == nil {
		return fmt.Errorf("快照为空，无法回滚")
	}
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "RollbackWithSnapshot",
	}).Infof("执行回滚: %s [%s] -> %s", snapshot.Service, snapshot.Namespace, snapshot.Tag)
	return k.UpdateWorkloadImage(snapshot.Namespace, snapshot.Service, snapshot.Tag)
}

// updateDeploymentImage 更新 Deployment 镜像（仅替换 tag）
func (k *K8sClient) updateDeploymentImage(deploy *appsv1.Deployment, newImage, namespace, name string, startTime time.Time, snapshot *models.ImageSnapshot) error {
	k.ensureRollingUpdateStrategy(deploy)

	updated := false
	for i := range deploy.Spec.Template.Spec.Containers {
		container := &deploy.Spec.Template.Spec.Containers[i]
		if container.Image == newImage {
			continue
		}
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "updateDeploymentImage",
		}).Infof("更新容器镜像: %s -> %s", container.Image, newImage)
		container.Image = newImage
		updated = true
	}

	if !updated {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "updateDeploymentImage",
			"took":   time.Since(startTime),
		}).Infof(color.YellowString("Deployment %s 镜像已是最新 tag: %s", name, ExtractTag(newImage)))
		return nil
	}

	_, err := k.Clientset.AppsV1().Deployments(namespace).Update(context.TODO(), deploy, metav1.UpdateOptions{})
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "updateDeploymentImage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("更新Deployment失败: %v", err))
		return err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "updateDeploymentImage",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("Deployment镜像更新成功: %s -> %s", name, ExtractTag(newImage)))
	return nil
}

// updateDaemonSetImage 更新 DaemonSet 镜像（仅替换 tag）
func (k *K8sClient) updateDaemonSetImage(ds *appsv1.DaemonSet, newImage, namespace, name string, startTime time.Time, snapshot *models.ImageSnapshot) error {
	updated := false
	for i := range ds.Spec.Template.Spec.Containers {
		container := &ds.Spec.Template.Spec.Containers[i]
		if container.Image == newImage {
			continue
		}
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "updateDaemonSetImage",
		}).Infof("更新容器镜像: %s -> %s", container.Image, newImage)
		container.Image = newImage
		updated = true
	}

	if !updated {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "updateDaemonSetImage",
			"took":   time.Since(startTime),
		}).Infof(color.YellowString("DaemonSet %s 镜像已是最新 tag: %s", name, ExtractTag(newImage)))
		return nil
	}

	_, err := k.Clientset.AppsV1().DaemonSets(namespace).Update(context.TODO(), ds, metav1.UpdateOptions{})
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "updateDaemonSetImage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("更新DaemonSet失败: %v", err))
		return err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "updateDaemonSetImage",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("DaemonSet镜像更新成功: %s -> %s", name, ExtractTag(newImage)))
	return nil
}

// ======================
// RollbackWorkload（同步更新）
// ======================
func (k *K8sClient) RollbackWorkload(namespace, serviceName, oldTag string) error {
	startTime := time.Now()

	// 与 Update 逻辑一致：只替换 tag
	if deploy, err := k.Clientset.AppsV1().Deployments(namespace).Get(context.TODO(), serviceName, metav1.GetOptions{}); err == nil {
		return k.rollbackDeploymentImage(deploy, oldTag, namespace, serviceName, startTime)
	}
	if ds, err := k.Clientset.AppsV1().DaemonSets(namespace).Get(context.TODO(), serviceName, metav1.GetOptions{}); err == nil {
		return k.rollbackDaemonSetImage(ds, oldTag, namespace, serviceName, startTime)
	}

	err := fmt.Errorf("未找到工作负载: %s [%s]", serviceName, namespace)
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "RollbackWorkload",
		"took":   time.Since(startTime),
	}).Errorf(color.RedString("%v", err))
	return err
}

func (k *K8sClient) rollbackDeploymentImage(deploy *appsv1.Deployment, oldTag, namespace, name string, startTime time.Time) error {
	k.ensureRollingUpdateStrategy(deploy)
	updated := false

	for i := range deploy.Spec.Template.Spec.Containers {
		container := &deploy.Spec.Template.Spec.Containers[i]
		currentImage := container.Image
		newImage := buildNewImage(currentImage, oldTag)

		if currentImage != newImage {
			container.Image = newImage
			updated = true
		}
	}

	if !updated {
		return nil
	}

	_, err := k.Clientset.AppsV1().Deployments(namespace).Update(context.TODO(), deploy, metav1.UpdateOptions{})
	if err != nil {
		logrus.Errorf(color.RedString("回滚Deployment失败: %v", err))
		return err
	}
	logrus.Infof(color.GreenString("Deployment回滚成功: %s -> %s", name, oldTag))
	return nil
}

// rollbackDaemonSetImage 回滚DaemonSet镜像
func (k *K8sClient) rollbackDaemonSetImage(ds *appsv1.DaemonSet, oldTag, namespace, name string, startTime time.Time) error {
	updated := false
	for i := range ds.Spec.Template.Spec.Containers {
		container := &ds.Spec.Template.Spec.Containers[i]
		container.Image = buildNewImage(container.Image, oldTag)
		updated = true
	}
	if !updated {
		return nil
	}
	_, err := k.Clientset.AppsV1().DaemonSets(namespace).Update(context.TODO(), ds, metav1.UpdateOptions{})
	if err != nil {
		logrus.Errorf(color.RedString("回滚DaemonSet失败: %v", err))
		return err
	}
	logrus.Infof(color.GreenString("DaemonSet回滚成功: %s -> %s", name, oldTag))
	return nil
}

// IsWorkloadRunning 检查工作负载是否有运行中的Pod
func (k *K8sClient) IsWorkloadRunning(namespace, name string) bool {
	startTime := time.Now()
	selector := fmt.Sprintf("app=%s", name)
	pods, err := k.Clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "IsWorkloadRunning",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("查询Pod失败: %v", err))
		return false
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodRunning {
			return true
		}
	}
	return false
}

// GetCurrentImage 获取当前镜像
func (k *K8sClient) GetCurrentImage(namespace, name string) string {
	startTime := time.Now()
	// 尝试Deployment
	deploy, err := k.Clientset.AppsV1().Deployments(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err == nil {
		return deploy.Spec.Template.Spec.Containers[0].Image
	}
	// 尝试DaemonSet
	ds, err := k.Clientset.AppsV1().DaemonSets(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err == nil {
		return ds.Spec.Template.Spec.Containers[0].Image
	}
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "GetCurrentImage",
		"took":   time.Since(startTime),
	}).Warnf(color.RedString("获取镜像失败: %s [%s]", name, namespace))
	return "unknown"
}

// WaitForRolloutComplete 等待 rollout 完成
// WaitForRolloutComplete 等待 rollout 完成（修复：添加真实超时轮询）
func (k *K8sClient) WaitForRolloutComplete(namespace, name string, timeout time.Duration) (bool, error) {
	startTime := time.Now()
	ticker := time.NewTicker(2 * time.Second) // 每2s检查一次
	defer ticker.Stop()

	timeoutCh := time.After(timeout)
	for {
		select {
		case <-timeoutCh:
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "WaitForRolloutComplete",
				"took":   time.Since(startTime),
			}).Errorf(color.RedString("rollout 超时: %s [%s]", name, namespace))
			return false, fmt.Errorf("rollout 超时: %v", timeout)
		case <-ticker.C:
			// 检查 Deployment
			if deploy, err := k.Clientset.AppsV1().Deployments(namespace).Get(context.TODO(), name, metav1.GetOptions{}); err == nil {
				if deploy.Status.ObservedGeneration >= deploy.Generation && deploy.Status.AvailableReplicas == *deploy.Spec.Replicas {
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "WaitForRolloutComplete",
						"took":   time.Since(startTime),
					}).Infof(color.GreenString("Deployment rollout 完成: %s [%s]", name, namespace))
					return true, nil
				}
				continue
			}

			// 检查 DaemonSet
			if ds, err := k.Clientset.AppsV1().DaemonSets(namespace).Get(context.TODO(), name, metav1.GetOptions{}); err == nil {
				if ds.Status.ObservedGeneration >= ds.Generation && ds.Status.NumberAvailable == ds.Status.DesiredNumberScheduled {
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "WaitForRolloutComplete",
						"took":   time.Since(startTime),
					}).Infof(color.GreenString("DaemonSet rollout 完成: %s [%s]", name, namespace))
					return true, nil
				}
				continue
			}

			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "WaitForRolloutComplete",
			}).Debugf("rollout 进行中: %s [%s]", name, namespace)
		}
	}
}

// DiscoverServicesFromNamespace 从命名空间发现服务
func (k *K8sClient) DiscoverServicesFromNamespace(namespace string) ([]string, error) {
	startTime := time.Now()
	var services []string

	// 发现Deployment
	deploys, err := k.Clientset.AppsV1().Deployments(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "DiscoverServicesFromNamespace",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("发现Deployment失败: %v", err))
		return nil, err
	}
	for _, deploy := range deploys.Items {
		serviceName := deploy.Name
		image := k.GetCurrentImage(namespace, serviceName)
		if image == "unknown" {
			continue
		}
		services = append(services, fmt.Sprintf("%s:%s", serviceName, image))
	}

	// 发现DaemonSet
	dss, err := k.Clientset.AppsV1().DaemonSets(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "DiscoverServicesFromNamespace",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("发现DaemonSet失败: %v", err))
		return nil, err
	}
	for _, ds := range dss.Items {
		serviceName := ds.Name
		image := k.GetCurrentImage(namespace, serviceName)
		if image == "unknown" {
			continue
		}
		services = append(services, fmt.Sprintf("%s:%s", serviceName, image))
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "DiscoverServicesFromNamespace",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("命名空间 [%s] 发现 %d 个服务", namespace, len(services)))
	return services, nil
}

// BuildPushRequest 构建推送请求
func (k *K8sClient) BuildPushRequest(cfg *config.Config) (models.PushRequest, error) {
	startTime := time.Now()
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "BuildPushRequest",
		"took":   time.Since(startTime),
	}).Info("开始构建 /push 请求数据")

	serviceSet := make(map[string]struct{})
	envSet := make(map[string]struct{})

	// 步骤1：遍历环境映射
	for env, namespace := range cfg.EnvMapping.Mappings {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "BuildPushRequest",
			"took":   time.Since(startTime),
		}).Infof("处理环境 [%s] -> 命名空间 [%s]", env, namespace)

		// 步骤2：发现服务
		nsServices, err := k.DiscoverServicesFromNamespace(namespace)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "BuildPushRequest",
				"took":   time.Since(startTime),
			}).Errorf(color.RedString("环境 [%s] 服务发现失败: %v", env, err))
			continue
		}

		// 步骤3：添加环境
		envSet[env] = struct{}{}

		// 步骤4：添加服务
		for _, serviceWithVersion := range nsServices {
			parts := strings.SplitN(serviceWithVersion, ":", 2)
			serviceName := parts[0]
			serviceSet[serviceName] = struct{}{}
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "BuildPushRequest",
				"took":   time.Since(startTime),
			}).Debugf("发现服务: %s", serviceName)
		}
	}

	// 步骤5：转换为切片
	var services []string
	for s := range serviceSet {
		services = append(services, s)
	}
	var environments []string
	for e := range envSet {
		environments = append(environments, e)
	}

	// 步骤6：检查数据
	if len(services) == 0 || len(environments) == 0 {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "BuildPushRequest",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("services 或 environments 不能为空"))
		return models.PushRequest{}, fmt.Errorf("services 或 environments 不能为空")
	}

	// 步骤7：构建请求
	pushReq := models.PushRequest{
		Services:     services,
		Environments: environments,
	}
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "BuildPushRequest",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("构建完成: %d 服务, %d 环境", len(services), len(environments)))
	return pushReq, nil
}

// ensureRollingUpdateStrategy 确保滚动更新策略（仅适用于Deployment）
func (k *K8sClient) ensureRollingUpdateStrategy(deploy *appsv1.Deployment) {
	startTime := time.Now()
	if deploy.Spec.Strategy.Type == "" {
		deploy.Spec.Strategy.Type = appsv1.RollingUpdateDeploymentStrategyType
	}
	if deploy.Spec.Strategy.RollingUpdate == nil {
		deploy.Spec.Strategy.RollingUpdate = &appsv1.RollingUpdateDeployment{
			MaxUnavailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 1},
			MaxSurge:       &intstr.IntOrString{Type: intstr.Int, IntVal: 1},
		}
	}
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "ensureRollingUpdateStrategy",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("已确保Deployment %s 的滚动更新策略", deploy.Name))
}