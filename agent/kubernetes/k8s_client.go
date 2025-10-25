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
	//"k8s.io/apimachinery/pkg/labels"
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

// UpdateWorkloadImage 滚动更新Deployment或DaemonSet镜像
func (k *K8sClient) UpdateWorkloadImage(namespace, name, newImage string) error {
	startTime := time.Now()
	// 步骤1：检查工作负载是否运行
	if !k.IsWorkloadRunning(namespace, name) {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "UpdateWorkloadImage",
			"took":   time.Since(startTime),
		}).Infof(color.GreenString("工作负载 %s 在命名空间 %s 无运行中的Pod，跳过更新", name, namespace))
		return nil
	}

	// 步骤2：尝试获取Deployment
	deploy, err := k.Clientset.AppsV1().Deployments(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err == nil {
		// 是Deployment
		return k.updateDeploymentImage(deploy, newImage, namespace, name, startTime)
	}

	// 步骤3：尝试获取DaemonSet
	ds, err := k.Clientset.AppsV1().DaemonSets(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err == nil {
		// 是DaemonSet
		return k.updateDaemonSetImage(ds, newImage, namespace, name, startTime)
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "UpdateWorkloadImage",
		"took":   time.Since(startTime),
	}).Errorf(color.RedString("未找到工作负载: %s [%s]", name, namespace))
	return fmt.Errorf("未找到工作负载: %s [%s]", name, namespace)
}

// updateDeploymentImage 更新Deployment镜像
func (k *K8sClient) updateDeploymentImage(deploy *appsv1.Deployment, newImage, namespace, name string, startTime time.Time) error {
	// 步骤1：确保滚动更新策略
	k.ensureRollingUpdateStrategy(deploy)

	// 步骤2：更新镜像
	deploy.Spec.Template.Spec.Containers[0].Image = newImage

	// 步骤3：更新Deployment
	_, err := k.Clientset.AppsV1().Deployments(namespace).Update(context.TODO(), deploy, metav1.UpdateOptions{})
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "updateDeploymentImage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("更新Deployment镜像失败: %v", err))
		return err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "updateDeploymentImage",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("Deployment镜像更新成功: %s [%s]", name, namespace))
	return nil
}

// updateDaemonSetImage 更新DaemonSet镜像
func (k *K8sClient) updateDaemonSetImage(ds *appsv1.DaemonSet, newImage, namespace, name string, startTime time.Time) error {
	// 步骤1：更新镜像
	ds.Spec.Template.Spec.Containers[0].Image = newImage

	// 步骤2：更新DaemonSet
	_, err := k.Clientset.AppsV1().DaemonSets(namespace).Update(context.TODO(), ds, metav1.UpdateOptions{})
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "updateDaemonSetImage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("更新DaemonSet镜像失败: %v", err))
		return err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "updateDaemonSetImage",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("DaemonSet镜像更新成功: %s [%s]", name, namespace))
	return nil
}

// RollbackWorkload 回滚Deployment或DaemonSet镜像
func (k *K8sClient) RollbackWorkload(namespace, name, oldImage string) error {
	startTime := time.Now()
	// 步骤1：尝试获取Deployment
	deploy, err := k.Clientset.AppsV1().Deployments(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err == nil {
		// 是Deployment
		return k.rollbackDeploymentImage(deploy, oldImage, namespace, name, startTime)
	}

	// 步骤2：尝试获取DaemonSet
	ds, err := k.Clientset.AppsV1().DaemonSets(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err == nil {
		// 是DaemonSet
		return k.rollbackDaemonSetImage(ds, oldImage, namespace, name, startTime)
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "RollbackWorkload",
		"took":   time.Since(startTime),
	}).Errorf(color.RedString("未找到工作负载: %s [%s]", name, namespace))
	return fmt.Errorf("未找到工作负载: %s [%s]", name, namespace)
}

// rollbackDeploymentImage 回滚Deployment镜像
func (k *K8sClient) rollbackDeploymentImage(deploy *appsv1.Deployment, oldImage, namespace, name string, startTime time.Time) error {
	// 步骤1：确保滚动更新策略
	k.ensureRollingUpdateStrategy(deploy)

	// 步骤2：回滚镜像
	deploy.Spec.Template.Spec.Containers[0].Image = oldImage

	// 步骤3：更新Deployment
	_, err := k.Clientset.AppsV1().Deployments(namespace).Update(context.TODO(), deploy, metav1.UpdateOptions{})
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "rollbackDeploymentImage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("回滚Deployment镜像失败: %v", err))
		return err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "rollbackDeploymentImage",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("Deployment镜像回滚成功: %s [%s]", name, namespace))
	return nil
}

// rollbackDaemonSetImage 回滚DaemonSet镜像
func (k *K8sClient) rollbackDaemonSetImage(ds *appsv1.DaemonSet, oldImage, namespace, name string, startTime time.Time) error {
	// 步骤1：回滚镜像
	ds.Spec.Template.Spec.Containers[0].Image = oldImage

	// 步骤2：更新DaemonSet
	_, err := k.Clientset.AppsV1().DaemonSets(namespace).Update(context.TODO(), ds, metav1.UpdateOptions{})
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "rollbackDaemonSetImage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("回滚DaemonSet镜像失败: %v", err))
		return err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "rollbackDaemonSetImage",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("DaemonSet镜像回滚成功: %s [%s]", name, namespace))
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
func (k *K8sClient) WaitForRolloutComplete(namespace, name string, timeout time.Duration) (bool, error) {
	startTime := time.Now()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// 检查Deployment
		deploy, err := k.Clientset.AppsV1().Deployments(namespace).Get(context.TODO(), name, metav1.GetOptions{})
		if err == nil {
			if deploy.Status.ObservedGeneration >= deploy.Generation && deploy.Status.AvailableReplicas == *deploy.Spec.Replicas {
				return nil
			}
			return fmt.Errorf("Deployment rollout 未完成")
		}

		// 检查DaemonSet
		ds, err := k.Clientset.AppsV1().DaemonSets(namespace).Get(context.TODO(), name, metav1.GetOptions{})
		if err == nil {
			if ds.Status.ObservedGeneration >= ds.Generation && ds.Status.NumberAvailable == ds.Status.DesiredNumberScheduled {
				return nil
			}
			return fmt.Errorf("DaemonSet rollout 未完成")
		}

		return fmt.Errorf("未找到工作负载")
	})

	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "WaitForRolloutComplete",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("等待rollout完成失败: %v", err))
		return false, err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "WaitForRolloutComplete",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("rollout完成"))
	return true, nil
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