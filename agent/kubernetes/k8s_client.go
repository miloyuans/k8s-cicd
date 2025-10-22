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
	"github.com/sirupsen/logrus"
)

type K8sClient struct {
	Clientset *kubernetes.Clientset
	Namespace string
	cfg       *config.DeployConfig
}

// NewK8sClient 根据配置创建K8s客户端
func NewK8sClient(k8sCfg *config.K8sAuthConfig, deployCfg *config.DeployConfig) (*K8sClient, error) {
	var config *rest.Config
	var err error

	// 步骤1：根据认证类型选择配置方式
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

	// 步骤3：返回封装客户端
	return &K8sClient{
		Clientset: clientset,
		Namespace: k8sCfg.Namespace,
		cfg:       deployCfg,
	}, nil
}

// UpdateDeploymentImage 滚动更新Deployment镜像（确保滚动更新策略）
func (k *K8sClient) UpdateDeploymentImage(deploymentName, newImage string) error {
	// 步骤1：获取当前Deployment
	deploy, err := k.Clientset.AppsV1().Deployments(k.Namespace).Get(context.TODO(), deploymentName, metav1.GetOptions{})
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
		result, err := k.Clientset.AppsV1().Deployments(k.Namespace).Update(context.TODO(), deploy, metav1.UpdateOptions{})
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
	if deploy.Spec.Strategy.Type == "" {
		deploy.Spec.Strategy.Type = appsv1.DeploymentStrategyTypeRollingUpdate
	}
	
	if deploy.Spec.Strategy.RollingUpdate == nil {
		deploy.Spec.Strategy.RollingUpdate = &appsv1.RollingUpdateDeployment{}
	}
	
	// 设置滚动更新参数
	if deploy.Spec.Strategy.RollingUpdate.MaxUnavailable == nil {
		deploy.Spec.Strategy.RollingUpdate.MaxUnavailable = &intstr.IntOrString{
			Type:   intstr.String,
			StrVal: "25%",
		}
	}
	if deploy.Spec.Strategy.RollingUpdate.MaxSurge == nil {
		deploy.Spec.Strategy.RollingUpdate.MaxSurge = &intstr.IntOrString{
			Type:   intstr.String,
			StrVal: "25%",
		}
	}
}

// getCurrentImage 从Deployment获取当前镜像
func (k *K8sClient) getCurrentImage(deploy *appsv1.Deployment) string {
	for _, container := range deploy.Spec.Template.Spec.Containers {
		if strings.Contains(container.Image, ":") {
			return container.Image
		}
	}
	return "unknown"
}

// WaitForDeploymentReady 等待新版本Deployment就绪（使用配置超时）
func (k *K8sClient) WaitForDeploymentReady(deploymentName, newImageTag string) error {
	// 使用配置的超时时间和轮询间隔
	return wait.Poll(k.cfg.PollInterval, k.cfg.WaitTimeout, func() (bool, error) {
		// 步骤1：检查Deployment状态
		deploy, err := k.Clientset.AppsV1().Deployments(k.Namespace).Get(context.TODO(), deploymentName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		// 步骤2：检查ReadyReplicas
		if deploy.Status.ReadyReplicas != *deploy.Spec.Replicas {
			logrus.Infof("⏳ Deployment %s ReadyReplicas: %d/%d", 
				deploymentName, deploy.Status.ReadyReplicas, *deploy.Spec.Replicas)
			return false, nil
		}

		// 步骤3：检查新版本Pod状态
		ready, err := k.checkNewVersionPodsReady(deploy, newImageTag)
		if err != nil {
			return false, err
		}
		return ready, nil
	})
}

// checkNewVersionPodsReady 检查新版本Pod是否全部就绪
func (k *K8sClient) checkNewVersionPodsReady(deploy *appsv1.Deployment, newImageTag string) (bool, error) {
	// 步骤1：构建标签选择器
	selector := labels.SelectorFromSet(deploy.Spec.Selector.MatchLabels)

	// 步骤2：列出所有Pod
	pods, err := k.Clientset.CoreV1().Pods(k.Namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	if err != nil {
		return false, err
	}

	// 步骤3：统计新旧版本Pod
	newVersionPods := 0
	runningNewPods := 0

	for _, pod := range pods.Items {
		// 检查Pod是否使用新版本镜像
		for _, container := range pod.Spec.Containers {
			if strings.Contains(container.Image, newImageTag) {
				newVersionPods++
				if pod.Status.Phase == corev1.PodRunning && 
				   k.allContainersReady(&pod) {
					runningNewPods++
				}
				break
			}
		}
	}

	// 步骤4：验证所有新Pod都就绪
	if newVersionPods > 0 && runningNewPods == newVersionPods {
		logrus.Infof("✅ 新版本Pod全部就绪: %d/%d", runningNewPods, newVersionPods)
		return true, nil
	}

	logrus.Infof("⏳ 等待新版本Pod就绪: %d/%d", runningNewPods, newVersionPods)
	return false, nil
}

// allContainersReady 检查Pod所有容器是否就绪
func (k *K8sClient) allContainersReady(pod *corev1.Pod) bool {
	for _, status := range pod.Status.ContainerStatuses {
		if !status.Ready {
			return false
		}
	}
	return true
}

// RollbackDeployment 使用官方Rollback API执行回滚（使用配置超时）
func (k *K8sClient) RollbackDeployment(deploymentName string) error {
	// 步骤1：创建回滚请求
	rollback := &appsv1.DeploymentRollback{
		Name: deploymentName,
		RollbackTo: &appsv1.RollbackTo{
			Revision: 1, // 回滚到上一个版本
		},
	}

	// 步骤2：执行回滚
	err := k.Clientset.AppsV1().Deployments(k.Namespace).Rollback(rollback)
	if err != nil {
		return fmt.Errorf("回滚Deployment失败: %v", err)
	}

	logrus.Infof("🔄 已触发回滚: %s 到上一个版本", deploymentName)

	// 步骤3：等待回滚完成（使用配置的回滚超时）
	err = wait.Poll(k.cfg.PollInterval, k.cfg.RollbackTimeout, func() (bool, error) {
		// 检查回滚后Deployment状态
		deploy, err := k.Clientset.AppsV1().Deployments(k.Namespace).Get(context.TODO(), deploymentName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return deploy.Status.ReadyReplicas == *deploy.Spec.Replicas, nil
	})

	if err != nil {
		logrus.Warnf("回滚后Deployment未完全就绪: %v", err)
		return err
	}

	logrus.Infof("✅ Deployment回滚完成: %s", deploymentName)
	return nil
}

// GetCurrentImage 获取Deployment当前镜像版本
func (k *K8sClient) GetCurrentImage(deploymentName string) (string, error) {
	deploy, err := k.Clientset.AppsV1().Deployments(k.Namespace).Get(context.TODO(), deploymentName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return k.getCurrentImage(deploy), nil
}

// CheckDeploymentHealth 检查Deployment健康状态
func (k *K8sClient) CheckDeploymentHealth(deploymentName string) bool {
	deploy, err := k.Clientset.AppsV1().Deployments(k.Namespace).Get(context.TODO(), deploymentName, metav1.GetOptions{})
	if err != nil {
		return false
	}

	if deploy.Status.ReadyReplicas != *deploy.Spec.Replicas {
		return false
	}

	ready, _ := k.checkNewVersionPodsReady(deploy, "unknown")
	return ready
}