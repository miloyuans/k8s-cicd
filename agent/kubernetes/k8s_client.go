package kubernetes

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"k8s-cicd/agent/config"
	"github.com/sirupsen/logrus"
)

// K8sClient K8s客户端封装，支持双重认证方式
type K8sClient struct {
	Clientset *kubernetes.Clientset
	Namespace string
}

// NewK8sClient 根据配置创建K8s客户端（支持kubeconfig和ServiceAccount）
func NewK8sClient(cfg *config.K8sAuthConfig) (*K8sClient, error) {
	var config *rest.Config
	var err error

	// 步骤1：根据认证类型选择配置方式
	switch cfg.AuthType {
	case "kubeconfig":
		// 使用kubeconfig文件认证
		config, err = clientcmd.BuildConfigFromFlags("", cfg.Kubeconfig)
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
		return nil, fmt.Errorf("不支持的认证类型: %s", cfg.AuthType)
	}

	// 步骤2：创建clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("创建K8s客户端失败: %v", err)
	}

	// 步骤3：返回封装客户端
	return &K8sClient{
		Clientset: clientset,
		Namespace: cfg.Namespace,
	}, nil
}

// UpdateDeploymentImage 更新Deployment的镜像版本
func (k *K8sClient) UpdateDeploymentImage(deploymentName, image string) error {
	// 步骤1：获取当前Deployment
	deploy, err := k.Clientset.AppsV1().Deployments(k.Namespace).Get(context.TODO(), deploymentName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("获取Deployment失败: %v", err)
	}

	// 步骤2：记录旧镜像版本
	oldImage := ""
	for i := range deploy.Spec.Template.Spec.Containers {
		if strings.Contains(deploy.Spec.Template.Spec.Containers[i].Image, ":") {
			oldImage = deploy.Spec.Template.Spec.Containers[i].Image
		}
	}

	// 步骤3：更新所有容器的镜像
	for i := range deploy.Spec.Template.Spec.Containers {
		deploy.Spec.Template.Spec.Containers[i].Image = image
	}

	// 步骤4：添加版本追踪注解
	if deploy.Annotations == nil {
		deploy.Annotations = make(map[string]string)
	}
	deploy.Annotations["deployment.cicd.k8s/version"] = image
	deploy.Annotations["deployment.cicd.k8s/updated-at"] = time.Now().Format(time.RFC3339)

	// 步骤5：更新Deployment
	_, err = k.Clientset.AppsV1().Deployments(k.Namespace).Update(context.TODO(), deploy, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("更新Deployment镜像失败: %v", err)
	}

	logrus.Infof("Deployment %s 镜像更新成功: %s -> %s", deploymentName, oldImage, image)
	return nil
}

// GetCurrentImage 获取Deployment当前镜像版本
func (k *K8sClient) GetCurrentImage(deploymentName string) (string, error) {
	// 步骤1：获取Deployment
	deploy, err := k.Clientset.AppsV1().Deployments(k.Namespace).Get(context.TODO(), deploymentName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("获取Deployment失败: %v", err)
	}

	// 步骤2：返回第一个容器的镜像
	if len(deploy.Spec.Template.Spec.Containers) > 0 {
		return deploy.Spec.Template.Spec.Containers[0].Image, nil
	}
	return "", fmt.Errorf("Deployment中没有容器")
}

// WaitForDeploymentReady 等待Deployment就绪（超时检查）
func (k *K8sClient) WaitForDeploymentReady(deploymentName string, timeout time.Duration) error {
	// 步骤1：轮询等待Deployment就绪
	return wait.Poll(5*time.Second, timeout, func() (bool, error) {
		// 获取Deployment状态
		deploy, err := k.Clientset.AppsV1().Deployments(k.Namespace).Get(context.TODO(), deploymentName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		// 检查ReadyReplicas
		if deploy.Status.ReadyReplicas == *deploy.Spec.Replicas {
			// 进一步检查所有Pod状态
			return k.checkAllPodsRunning(deploy)
		}
		return false, nil
	})
}

// checkAllPodsRunning 检查Deployment下所有Pod是否Running
func (k *K8sClient) checkAllPodsRunning(deploy *appsv1.Deployment) (bool, error) {
	// 步骤1：列出Pod列表
	pods, err := k.Clientset.CoreV1().Pods(k.Namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: metav1.FormatLabelSelector(deploy.Spec.Selector),
	})
	if err != nil {
		return false, err
	}

	// 步骤2：检查每个Pod状态
	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodRunning {
			logrus.Warnf("Pod %s 状态异常: %s", pod.Name, pod.Status.Phase)
			return false, nil
		}
	}

	logrus.Info("所有Pod已就绪")
	return true, nil
}

// RollbackDeployment 执行Deployment回滚
func (k *K8sClient) RollbackDeployment(deploymentName string) error {
	// 步骤1：执行回滚操作
	err := k.Clientset.AppsV1().Deployments(k.Namespace).Rollback(&appsv1.Rollback{
		Name: deploymentName,
	})
	if err != nil {
		return fmt.Errorf("回滚Deployment失败: %v", err)
	}

	// 步骤2：等待回滚完成
	err = k.WaitForDeploymentReady(deploymentName, 3*time.Minute)
	if err != nil {
		logrus.Warnf("回滚后Deployment未完全就绪: %v", err)
	}

	logrus.Info("Deployment回滚完成: ", deploymentName)
	return nil
}

// CheckDeploymentHealth 检查Deployment健康状态
func (k *K8sClient) CheckDeploymentHealth(deploymentName string) bool {
	// 步骤1：检查Deployment状态
	deploy, err := k.Clientset.AppsV1().Deployments(k.Namespace).Get(context.TODO(), deploymentName, metav1.GetOptions{})
	if err != nil {
		logrus.Error("检查Deployment健康状态失败: ", err)
		return false
	}

	// 步骤2：检查ReadyReplicas
	if deploy.Status.ReadyReplicas != *deploy.Spec.Replicas {
		return false
	}

	// 步骤3：检查Pod状态
	return k.checkAllPodsRunning(deploy) == true
}