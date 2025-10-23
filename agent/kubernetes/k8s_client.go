package kubernetes

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"

	"k8s-cicd/agent/config"
	"k8s-cicd/agent/models"
	"github.com/sirupsen/logrus"
)

type K8sClient struct {
	Clientset *kubernetes.Clientset // Kubernetes客户端集
	cfg       *config.DeployConfig  // 部署配置
}

// NewK8sClient 根据配置创建K8s客户端
func NewK8sClient(k8sCfg *config.K8sAuthConfig, deployCfg *config.DeployConfig) (*K8sClient, error) {
	// 步骤1：根据认证类型选择配置方式
	var config *rest.Config
	var err error

	switch k8sCfg.AuthType {
	case "kubeconfig":
		// 使用kubeconfig文件认证
		config, err = clientcmd.BuildConfigFromFlags("", k8sCfg.Kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("kubeconfig认证失败: %v", err)
		}
		logrus.Info("使用kubeconfig认证成功")

	case "serviceaccount":
		// 使用ServiceAccount集群内认证
		config, err = rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("ServiceAccount认证失败: %v", err)
		}
		logrus.Info("使用ServiceAccount认证成功")

	default:
		return nil, fmt.Errorf("不支持的认证类型: %s", k8sCfg.AuthType)
	}

	// 步骤2：创建clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("创建K8s客户端失败: %v", err)
	}

	// 步骤3：返回封装客户端（移除固定Namespace）
	return &K8sClient{
		Clientset: clientset,
		cfg:       deployCfg,
	}, nil
}

// UpdateDeploymentImage 滚动更新Deployment镜像（确保滚动更新策略）
func (k *K8sClient) UpdateDeploymentImage(namespace, deploymentName, newImage string) error {
	// 步骤1：获取当前Deployment
	deploy, err := k.Clientset.AppsV1().Deployments(namespace).Get(context.TODO(), deploymentName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("获取Deployment失败: %v", err)
	}

	// 步骤2：记录旧镜像版本
	oldImage := k.getCurrentImage(deploy)

	// 步骤3：确保滚动更新策略
	k.ensureRollingUpdateStrategy(deploy)

	// 步骤4：更新所有容器的镜像
	updated := false
	for i := range deploy.Spec.Template.Spec.Containers {
		if strings.Contains(deploy.Spec.Template.Spec.Containers[i].Image, ":") {
			deploy.Spec.Template.Spec.Containers[i].Image = newImage
			updated = true
		}
	}
	if !updated {
		return fmt.Errorf("未找到可更新的容器镜像")
	}

	// 步骤5：添加版本追踪注解
	if deploy.Annotations == nil {
		deploy.Annotations = make(map[string]string)
	}
	deploy.Annotations["deployment.cicd.k8s/version"] = newImage
	deploy.Annotations["deployment.cicd.k8s/updated-at"] = time.Now().Format(time.RFC3339)

	// 步骤6：使用重试机制更新Deployment
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		result, err := k.Clientset.AppsV1().Deployments(namespace).Update(context.TODO(), deploy, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		*deploy = *result
		return nil
	})

	if err != nil {
		return fmt.Errorf("更新Deployment镜像失败: %v", err)
	}

	logrus.Infof("✅ 滚动更新Deployment成功: %s -> %s", oldImage, newImage)
	return nil
}

// ensureRollingUpdateStrategy 确保使用滚动更新策略
func (k *K8sClient) ensureRollingUpdateStrategy(deploy *appsv1.Deployment) {
	// 步骤1：设置策略类型
	if deploy.Spec.Strategy.Type == "" {
		deploy.Spec.Strategy.Type = appsv1.RollingUpdateDeploymentStrategyType
	}

	// 步骤2：初始化滚动更新参数
	if deploy.Spec.Strategy.RollingUpdate == nil {
		deploy.Spec.Strategy.RollingUpdate = &appsv1.RollingUpdateDeployment{}
	}

	// 步骤3：设置MaxUnavailable
	if deploy.Spec.Strategy.RollingUpdate.MaxUnavailable == nil {
		deploy.Spec.Strategy.RollingUpdate.MaxUnavailable = &intstr.IntOrString{
			Type:   intstr.String,
			StrVal: "25%",
		}
	}

	// 步骤4：设置MaxSurge
	if deploy.Spec.Strategy.RollingUpdate.MaxSurge == nil {
		deploy.Spec.Strategy.RollingUpdate.MaxSurge = &intstr.IntOrString{
			Type:   intstr.String,
			StrVal: "25%",
		}
	}
}

// getCurrentImage 从Deployment获取当前镜像
func (k *K8sClient) getCurrentImage(deploy *appsv1.Deployment) string {
	// 步骤1：遍历容器查找镜像
	for _, container := range deploy.Spec.Template.Spec.Containers {
		if strings.Contains(container.Image, ":") {
			return container.Image
		}
	}
	// 步骤2：未找到返回unknown
	return "unknown"
}

// WaitForDeploymentReady 等待新版本Deployment就绪（使用配置超时）
func (k *K8sClient) WaitForDeploymentReady(namespace, deploymentName, newImageTag string) error {
	// 步骤1：使用Poll等待Deployment就绪
	return wait.Poll(k.cfg.PollInterval, k.cfg.WaitTimeout, func() (bool, error) {
		// 步骤1.1：获取Deployment
		deploy, err := k.Clientset.AppsV1().Deployments(namespace).Get(context.TODO(), deploymentName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		// 步骤1.2：检查可用副本
		if deploy.Status.AvailableReplicas != *deploy.Spec.Replicas {
			return false, nil
		}

		// 步骤1.3：检查新版本Pod就绪
		return k.checkNewVersionPodsReady(deploy, newImageTag)
	})
}

// checkNewVersionPodsReady 检查新版本Pod是否就绪
func (k *K8sClient) checkNewVersionPodsReady(deploy *appsv1.Deployment, newImageTag string) (bool, error) {
	// 步骤1：获取Pod列表
	labelSelector := metav1.LabelSelector{MatchLabels: deploy.Spec.Selector.MatchLabels}
	pods, err := k.Clientset.CoreV1().Pods(deploy.Namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: labels.Set(labelSelector.MatchLabels).String(),
	})
	if err != nil {
		return false, err
	}

	// 步骤2：检查每个Pod的状态和镜像
	newVersionReady := 0
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodRunning {
			for _, container := range pod.Spec.Containers {
				if strings.HasSuffix(container.Image, newImageTag) {
					newVersionReady++
					break
				}
			}
		}
	}

	// 步骤3：检查是否所有副本就绪
	return newVersionReady == int(*deploy.Spec.Replicas), nil
}

// RollbackDeployment 回滚Deployment到旧版本
func (k *K8sClient) RollbackDeployment(namespace, deploymentName, oldImage string) error {
	// 步骤1：更新镜像为旧版本
	err := k.UpdateDeploymentImage(namespace, deploymentName, oldImage)
	if err != nil {
		return err
	}

	// 步骤2：等待回滚完成
	err = k.WaitForRollbackReady(namespace, deploymentName)
	if err != nil {
		return err
	}

	return nil
}

// WaitForRollbackReady 等待回滚后Deployment就绪
func (k *K8sClient) WaitForRollbackReady(namespace, deploymentName string) error {
	// 步骤1：使用Poll等待就绪
	return wait.Poll(k.cfg.PollInterval, k.cfg.RollbackTimeout, func() (bool, error) {
		deploy, err := k.Clientset.AppsV1().Deployments(namespace).Get(context.TODO(), deploymentName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return deploy.Status.ReadyReplicas == *deploy.Spec.Replicas, nil
	})
}

// GetCurrentImage 获取Deployment当前镜像版本
func (k *K8sClient) GetCurrentImage(namespace, deploymentName string) (string, error) {
	// 步骤1：获取Deployment
	deploy, err := k.Clientset.AppsV1().Deployments(namespace).Get(context.TODO(), deploymentName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	// 步骤2：返回当前镜像
	return k.getCurrentImage(deploy), nil
}

// CheckDeploymentHealth 检查Deployment健康状态
func (k *K8sClient) CheckDeploymentHealth(namespace, deploymentName string) bool {
	// 步骤1：获取Deployment
	deploy, err := k.Clientset.AppsV1().Deployments(namespace).Get(context.TODO(), deploymentName, metav1.GetOptions{})
	if err != nil {
		return false
	}

	// 步骤2：检查就绪副本
	if deploy.Status.ReadyReplicas != *deploy.Spec.Replicas {
		return false
	}

	// 步骤3：检查Pod就绪
	ready, _ := k.checkNewVersionPodsReady(deploy, "unknown")
	return ready
}

// DiscoverServicesFromNamespace 从指定命名空间发现所有Deployment服务
func (k *K8sClient) DiscoverServicesFromNamespace(namespace string) ([]string, error) {
	// 步骤1：记录发现日志
	logrus.Infof("🔍 开始从命名空间 [%s] 发现服务", namespace)

	// 步骤2：列出所有Deployment
	deploys, err := k.Clientset.AppsV1().Deployments(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("列出Deployment失败: %v", err)
	}

	// 步骤3：提取服务名（Deployment名称）
	var services []string
	for _, deploy := range deploys.Items {
		services = append(services, deploy.Name)
		logrus.Debugf("   📋 发现服务: %s", deploy.Name)
	}

	// 步骤4：获取当前运行的镜像版本
	var filteredServices []string
	for _, service := range services {
		version, err := k.GetCurrentImage(namespace, service)
		if err != nil {
			logrus.Warnf("⚠️ 获取服务 [%s] 版本失败: %v", service, err)
			continue // 跳过无效服务
		}
		logrus.Infof("✅ 服务 [%s] 当前版本: %s", service, version)
		filteredServices = append(filteredServices, fmt.Sprintf("%s:%s", service, version))
	}

	// 步骤5：记录发现总结
	logrus.Infof("✅ 命名空间 [%s] 发现 %d 个有效服务", namespace, len(filteredServices))
	return filteredServices, nil
}

// BuildPushRequest 根据配置构建 /push 请求数据
func (k *K8sClient) BuildPushRequest(cfg *config.Config) (models.PushRequest, error) {
	// 步骤1：记录构建日志
	logrus.Info("🏗️ 构建 /push 请求数据")

	serviceSet := make(map[string]struct{}) // 用于去重服务名字
	envSet := make(map[string]struct{})     // 用于去重环境名字
	var deployments []models.DeployRequest

	user := cfg.User.Default
	status := "running" // 当前运行状态

	// 步骤2：遍历环境映射
	for env, namespace := range cfg.EnvMapping.Mappings {
		logrus.Infof("🔄 处理环境 [%s] -> 命名空间 [%s]", env, namespace)

		// 步骤2.1：获取该命名空间的服务列表
		nsServices, err := k.DiscoverServicesFromNamespace(namespace)
		if err != nil {
			logrus.Errorf("❌ 环境 [%s] 服务发现失败: %v", env, err)
			continue
		}

		// 步骤2.2：添加到环境集合（去重）
		envSet[env] = struct{}{}

		// 步骤2.3：为每个服务创建部署记录，并去重服务
		for _, serviceWithVersion := range nsServices {
			parts := strings.Split(serviceWithVersion, ":")
			serviceName := parts[0]
			version := ""
			if len(parts) > 1 {
				version = parts[1]
			}

			// 去重服务名字
			serviceSet[serviceName] = struct{}{}

			// 创建部署请求（优化：仅包含有效版本）
			deploy := models.DeployRequest{
				Service:      serviceName,
				Environments: []string{env},
				Version:      version,
				User:         user,
				Status:       status,
			}

			deployments = append(deployments, deploy)
			logrus.Debugf("   📝 添加部署: %s v%s [%s/%s]", serviceName, version, env, user)
		}
	}

	// 步骤3：转换为切片（已去重）
	var services []string
	for s := range serviceSet {
		services = append(services, s)
	}
	var environments []string
	for e := range envSet {
		environments = append(environments, e)
	}

	// 步骤4：检查数据有效性
	if len(services) == 0 || len(environments) == 0 {
		return models.PushRequest{}, fmt.Errorf("❌ services 或 environments 不能为空")
	}

	// 步骤5：构建PushRequest（优化：包含Deployments以推送当前版本信息）
	pushReq := models.PushRequest{
		Services:     services,
		Environments: environments,
		Deployments:  deployments,
	}

	// 步骤6：记录构建总结
	logrus.Infof("✅ 构建完成 /push 数据: %d 服务, %d 环境, %d 部署",
		len(services), len(environments), len(deployments))

	return pushReq, nil
}