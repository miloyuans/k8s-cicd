//
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

// K8sClient Kubernetes客户端
type K8sClient struct {
	Clientset *kubernetes.Clientset // Kubernetes客户端集
	cfg       *config.DeployConfig  // 部署配置
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
			}).Errorf("kubeconfig认证失败: %v", err)
			return nil, fmt.Errorf("kubeconfig认证失败: %v", err)
		}
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "NewK8sClient",
			"took":   time.Since(startTime),
		}).Info("使用kubeconfig认证成功")
	case "serviceaccount":
		config, err = rest.InClusterConfig()
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "NewK8sClient",
				"took":   time.Since(startTime),
			}).Errorf("ServiceAccount认证失败: %v", err)
			return nil, fmt.Errorf("ServiceAccount认证失败: %v", err)
		}
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "NewK8sClient",
			"took":   time.Since(startTime),
		}).Info("使用ServiceAccount认证成功")
	default:
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "NewK8sClient",
			"took":   time.Since(startTime),
		}).Errorf("不支持的认证类型: %s", k8sCfg.AuthType)
		return nil, fmt.Errorf("不支持的认证类型: %s", k8sCfg.AuthType)
	}

	// 步骤2：创建clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "NewK8sClient",
			"took":   time.Since(startTime),
		}).Errorf("创建K8s客户端失败: %v", err)
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
	}).Info("K8s客户端创建成功")
	return client, nil
}

// UpdateDeploymentImage 滚动更新Deployment镜像
func (k *K8sClient) UpdateDeploymentImage(namespace, deploymentName, newImage string) error {
	startTime := time.Now()
	// 步骤1：获取Deployment
	deploy, err := k.Clientset.AppsV1().Deployments(namespace).Get(context.TODO(), deploymentName, metav1.GetOptions{})
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "UpdateDeploymentImage",
			"took":   time.Since(startTime),
		}).Errorf("获取Deployment失败: %v", err)
		return fmt.Errorf("获取Deployment失败: %v", err)
	}

	// 步骤2：记录旧镜像
	oldImage := k.GetCurrentImage(namespace, deploymentName)
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "UpdateDeploymentImage",
		"took":   time.Since(startTime),
	}).Infof("当前镜像: %s", oldImage)

	// 步骤3：确保滚动更新策略
	k.ensureRollingUpdateStrategy(deploy)

	// 步骤4：更新镜像
	updated := false
	for i := range deploy.Spec.Template.Spec.Containers {
		if strings.Contains(deploy.Spec.Template.Spec.Containers[i].Image, ":") {
			deploy.Spec.Template.Spec.Containers[i].Image = newImage
			updated = true
		}
	}
	if !updated {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "UpdateDeploymentImage",
			"took":   time.Since(startTime),
		}).Error("未找到可更新的容器镜像")
		return fmt.Errorf("未找到可更新的容器镜像")
	}

	// 步骤5：添加版本追踪注解
	if deploy.Annotations == nil {
		deploy.Annotations = make(map[string]string)
	}
	deploy.Annotations["deployment.cicd.k8s/version"] = newImage
	deploy.Annotations["deployment.cicd.k8s/updated-at"] = time.Now().Format(time.RFC3339)

	// 步骤6：更新Deployment
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		result, err := k.Clientset.AppsV1().Deployments(namespace).Update(context.TODO(), deploy, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		*deploy = *result
		return nil
	})
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "UpdateDeploymentImage",
			"took":   time.Since(startTime),
		}).Errorf("更新Deployment失败: %v", err)
		return fmt.Errorf("更新Deployment失败: %v", err)
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "UpdateDeploymentImage",
		"took":   time.Since(startTime),
	}).Infof("滚动更新成功: %s -> %s", oldImage, newImage)
	return nil
}

// ensureRollingUpdateStrategy 确保滚动更新策略
func (k *K8sClient) ensureRollingUpdateStrategy(deploy *appsv1.Deployment) {
	startTime := time.Now()
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

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "ensureRollingUpdateStrategy",
		"took":   time.Since(startTime),
	}).Debug("滚动更新策略设置完成")
}

// GetCurrentImage 获取当前镜像
func (k *K8sClient) GetCurrentImage(namespace, deploymentName string) string {
	startTime := time.Now()
	// 步骤1：获取Deployment
	deploy, err := k.Clientset.AppsV1().Deployments(namespace).Get(context.TODO(), deploymentName, metav1.GetOptions{})
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "GetCurrentImage",
			"took":   time.Since(startTime),
		}).Warnf("获取Deployment失败: %v", err)
		return "unknown"
	}

	// 步骤2：查找镜像
	for _, container := range deploy.Spec.Template.Spec.Containers {
		if strings.Contains(container.Image, ":") {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "GetCurrentImage",
				"took":   time.Since(startTime),
			}).Debugf("获取镜像: %s", container.Image)
			return container.Image
		}
	}
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "GetCurrentImage",
		"took":   time.Since(startTime),
	}).Warn("未找到有效镜像")
	return "unknown"
}

// WaitForDeploymentReady 等待新版本Deployment就绪
func (k *K8sClient) WaitForDeploymentReady(namespace, deploymentName, newImageTag string) error {
	startTime := time.Now()
	// 步骤1：使用Poll等待Deployment就绪
	err := wait.Poll(k.cfg.PollInterval, k.cfg.WaitTimeout, func() (bool, error) {
		// 步骤1.1：获取Deployment
		deploy, err := k.Clientset.AppsV1().Deployments(namespace).Get(context.TODO(), deploymentName, metav1.GetOptions{})
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "WaitForDeploymentReady",
				"took":   time.Since(startTime),
			}).Errorf("获取Deployment失败: %v", err)
			return false, err
		}

		// 步骤1.2：检查可用副本
		if deploy.Status.AvailableReplicas != *deploy.Spec.Replicas {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "WaitForDeploymentReady",
				"took":   time.Since(startTime),
			}).Debug("副本未完全就绪")
			return false, nil
		}

		// 步骤1.3：检查新版本Pod就绪
		ready, err := k.checkNewVersionPodsReady(deploy, newImageTag)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "WaitForDeploymentReady",
				"took":   time.Since(startTime),
			}).Errorf("检查Pod就绪失败: %v", err)
			return false, err
		}
		return ready, nil
	})
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "WaitForDeploymentReady",
			"took":   time.Since(startTime),
		}).Errorf("等待Deployment就绪失败: %v", err)
		return err
	}
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "WaitForDeploymentReady",
		"took":   time.Since(startTime),
	}).Info("Deployment就绪")
	return nil
}

// checkNewVersionPodsReady 检查新版本Pod是否就绪
func (k *K8sClient) checkNewVersionPodsReady(deploy *appsv1.Deployment, newImageTag string) (bool, error) {
	startTime := time.Now()
	// 步骤1：获取Pod列表
	labelSelector := metav1.LabelSelector{MatchLabels: deploy.Spec.Selector.MatchLabels}
	pods, err := k.Clientset.CoreV1().Pods(deploy.Namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: labels.Set(labelSelector.MatchLabels).String(),
	})
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "checkNewVersionPodsReady",
			"took":   time.Since(startTime),
		}).Errorf("获取Pod列表失败: %v", err)
		return false, err
	}

	// 步骤2：检查Pod状态和镜像
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

	// 步骤3：检查副本是否就绪
	ready := newVersionReady == int(*deploy.Spec.Replicas)
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "checkNewVersionPodsReady",
		"took":   time.Since(startTime),
	}).Debugf("新版本Pod就绪: %d/%d", newVersionReady, *deploy.Spec.Replicas)
	return ready, nil
}

// RollbackDeployment 回滚Deployment到旧版本
func (k *K8sClient) RollbackDeployment(namespace, deploymentName, oldImage string) error {
	startTime := time.Now()
	// 步骤1：更新镜像为旧版本
	err := k.UpdateDeploymentImage(namespace, deploymentName, oldImage)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "RollbackDeployment",
			"took":   time.Since(startTime),
		}).Errorf("回滚镜像失败: %v", err)
		return err
	}

	// 步骤2：等待回滚完成
	err = k.WaitForRollbackReady(namespace, deploymentName)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "RollbackDeployment",
			"took":   time.Since(startTime),
		}).Errorf("等待回滚失败: %v", err)
		return err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "RollbackDeployment",
		"took":   time.Since(startTime),
	}).Info("回滚成功")
	return nil
}

// WaitForRollbackReady 等待回滚后Deployment就绪
func (k *K8sClient) WaitForRollbackReady(namespace, deploymentName string) error {
	startTime := time.Now()
	// 步骤1：使用Poll等待就绪
	err := wait.Poll(k.cfg.PollInterval, k.cfg.RollbackTimeout, func() (bool, error) {
		deploy, err := k.Clientset.AppsV1().Deployments(namespace).Get(context.TODO(), deploymentName, metav1.GetOptions{})
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "WaitForRollbackReady",
				"took":   time.Since(startTime),
			}).Errorf("获取Deployment失败: %v", err)
			return false, err
		}
		ready := deploy.Status.ReadyReplicas == *deploy.Spec.Replicas
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "WaitForRollbackReady",
			"took":   time.Since(startTime),
		}).Debugf("回滚副本状态: %d/%d", deploy.Status.ReadyReplicas, *deploy.Spec.Replicas)
		return ready, nil
	})
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "WaitForRollbackReady",
			"took":   time.Since(startTime),
		}).Errorf("等待回滚就绪失败: %v", err)
		return err
	}
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "WaitForRollbackReady",
		"took":   time.Since(startTime),
	}).Info("回滚就绪")
	return nil
}

// DiscoverServicesFromNamespace 发现命名空间中的服务
func (k *K8sClient) DiscoverServicesFromNamespace(namespace string) ([]string, error) {
	startTime := time.Now()
	// 步骤1：列出Deployment
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "DiscoverServicesFromNamespace",
		"took":   time.Since(startTime),
	}).Infof("开始发现命名空间 [%s] 的服务", namespace)

	deploys, err := k.Clientset.AppsV1().Deployments(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "DiscoverServicesFromNamespace",
			"took":   time.Since(startTime),
		}).Errorf("列出Deployment失败: %v", err)
		return nil, fmt.Errorf("列出Deployment失败: %v", err)
	}

	// 步骤2：收集服务和版本
	var services []string
	for _, deploy := range deploys.Items {
		serviceName := deploy.Name
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "DiscoverServicesFromNamespace",
			"took":   time.Since(startTime),
		}).Debugf("发现服务: %s", serviceName)

		image := k.GetCurrentImage(namespace, serviceName)
		if image == "unknown" {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "DiscoverServicesFromNamespace",
				"took":   time.Since(startTime),
			}).Warnf("获取服务 [%s] 版本失败", serviceName)
			continue
		}
		services = append(services, fmt.Sprintf("%s:%s", serviceName, image))
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "DiscoverServicesFromNamespace",
		"took":   time.Since(startTime),
	}).Infof("命名空间 [%s] 发现 %d 个服务", namespace, len(services))
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
	var deployments []models.DeployRequest
	user := cfg.User.Default
	status := "running"

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
			}).Errorf("环境 [%s] 服务发现失败: %v", env, err)
			continue
		}

		// 步骤3：添加环境
		envSet[env] = struct{}{}

		// 步骤4：创建部署记录
		for _, serviceWithVersion := range nsServices {
			parts := strings.SplitN(serviceWithVersion, ":", 2)
			serviceName := parts[0]
			version := parts[1]

			serviceSet[serviceName] = struct{}{}
			deploy := models.DeployRequest{
				Service:      serviceName,
				Environments: []string{env},
				Version:      version,
				User:         user,
				Status:       status,
			}
			deployments = append(deployments, deploy)
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "BuildPushRequest",
				"took":   time.Since(startTime),
			}).Debugf("添加部署: %s v%s [%s/%s]", serviceName, version, env, user)
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
		}).Error("services 或 environments 不能为空")
		return models.PushRequest{}, fmt.Errorf("services 或 environments 不能为空")
	}

	// 步骤7：构建请求
	pushReq := models.PushRequest{
		Services:     services,
		Environments: environments,
		Deployments:  deployments,
	}
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "BuildPushRequest",
		"took":   time.Since(startTime),
	}).Infof("构建完成: %d 服务, %d 环境, %d 部署", len(services), len(environments), len(deployments))
	return pushReq, nil
}