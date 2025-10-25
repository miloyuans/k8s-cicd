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
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
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
	}).Errorf(color.RedString("获取工作负载失败: %v", err))
	return fmt.Errorf("获取工作负载失败: %v", err)
}

// updateDeploymentImage 更新Deployment镜像
func (k *K8sClient) updateDeploymentImage(deploy *appsv1.Deployment, newImage, namespace, name string, startTime time.Time) error {
	// 步骤1：记录旧镜像
	oldImage := k.GetCurrentImage(namespace, name)
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "updateDeploymentImage",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("当前镜像: %s", oldImage))

	// 步骤2：确保滚动更新策略
	k.ensureRollingUpdateStrategy(deploy)

	// 步骤3：更新镜像
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
			"method": "updateDeploymentImage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("未找到镜像可更新"))
		return fmt.Errorf("未找到镜像可更新")
	}

	// 步骤4：更新Deployment
	_, err := k.Clientset.AppsV1().Deployments(namespace).Update(context.TODO(), deploy, metav1.UpdateOptions{})
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "updateDeploymentImage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("更新Deployment失败: %v", err))
		return fmt.Errorf("更新Deployment失败: %v", err)
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "updateDeploymentImage",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("Deployment镜像更新成功: %s -> %s", oldImage, newImage))
	return nil
}

// updateDaemonSetImage 更新DaemonSet镜像
func (k *K8sClient) updateDaemonSetImage(ds *appsv1.DaemonSet, newImage, namespace, name string, startTime time.Time) error {
	// 步骤1：记录旧镜像
	oldImage := k.GetCurrentImage(namespace, name)
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "updateDaemonSetImage",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("当前镜像: %s", oldImage))

	// 步骤2：更新镜像
	updated := false
	for i := range ds.Spec.Template.Spec.Containers {
		if strings.Contains(ds.Spec.Template.Spec.Containers[i].Image, ":") {
			ds.Spec.Template.Spec.Containers[i].Image = newImage
			updated = true
		}
	}
	if !updated {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "updateDaemonSetImage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("未找到镜像可更新"))
		return fmt.Errorf("未找到镜像可更新")
	}

	// 步骤3：更新DaemonSet
	_, err := k.Clientset.AppsV1().DaemonSets(namespace).Update(context.TODO(), ds, metav1.UpdateOptions{})
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "updateDaemonSetImage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("更新DaemonSet失败: %v", err))
		return fmt.Errorf("更新DaemonSet失败: %v", err)
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "updateDaemonSetImage",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("DaemonSet镜像更新成功: %s -> %s", oldImage, newImage))
	return nil
}

// GetCurrentImage 获取当前镜像
func (k *K8sClient) GetCurrentImage(namespace, name string) string {
	// 尝试获取Deployment
	deploy, err := k.Clientset.AppsV1().Deployments(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err == nil && len(deploy.Spec.Template.Spec.Containers) > 0 {
		return deploy.Spec.Template.Spec.Containers[0].Image
	}

	// 尝试获取DaemonSet
	ds, err := k.Clientset.AppsV1().DaemonSets(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err == nil && len(ds.Spec.Template.Spec.Containers) > 0 {
		return ds.Spec.Template.Spec.Containers[0].Image
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "GetCurrentImage",
		"took":   time.Since(time.Now()),
	}).Warnf(color.RedString("无法获取工作负载 %s 的镜像", name))
	return "unknown"
}

// WaitForWorkloadReady 等待Deployment或DaemonSet就绪
func (k *K8sClient) WaitForWorkloadReady(namespace, name string) error {
	startTime := time.Now()
	// 步骤1：尝试等待Deployment就绪
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deploy, err := k.Clientset.AppsV1().Deployments(namespace).Get(context.TODO(), name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if deploy.Status.AvailableReplicas == *deploy.Spec.Replicas {
			return nil
		}
		return fmt.Errorf("Deployment未就绪")
	})
	if err == nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "WaitForWorkloadReady",
			"took":   time.Since(startTime),
		}).Infof(color.GreenString("Deployment %s 就绪", name))
		return nil
	}

	// 步骤2：尝试等待DaemonSet就绪
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		ds, err := k.Clientset.AppsV1().DaemonSets(namespace).Get(context.TODO(), name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if ds.Status.NumberAvailable == ds.Status.DesiredNumberScheduled {
			return nil
		}
		return fmt.Errorf("DaemonSet未就绪")
	})
	if err == nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "WaitForWorkloadReady",
			"took":   time.Since(startTime),
		}).Infof(color.GreenString("DaemonSet %s 就绪", name))
		return nil
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "WaitForWorkloadReady",
		"took":   time.Since(startTime),
	}).Errorf(color.RedString("等待工作负载 %s 就绪失败: %v", name, err))
	return fmt.Errorf("等待工作负载就绪失败: %v", err)
}

// RollbackWorkload 回滚Deployment或DaemonSet到指定镜像
func (k *K8sClient) RollbackWorkload(namespace, name, oldImage string) error {
	startTime := time.Now()
	// 步骤1：尝试回滚Deployment
	deploy, err := k.Clientset.AppsV1().Deployments(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err == nil {
		updated := false
		for i := range deploy.Spec.Template.Spec.Containers {
			if strings.Contains(deploy.Spec.Template.Spec.Containers[i].Image, ":") {
				deploy.Spec.Template.Spec.Containers[i].Image = oldImage
				updated = true
			}
		}
		if !updated {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "RollbackWorkload",
				"took":   time.Since(startTime),
			}).Errorf(color.RedString("未找到镜像可回滚"))
			return fmt.Errorf("未找到镜像可回滚")
		}
		_, err := k.Clientset.AppsV1().Deployments(namespace).Update(context.TODO(), deploy, metav1.UpdateOptions{})
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "RollbackWorkload",
				"took":   time.Since(startTime),
			}).Errorf(color.RedString("回滚Deployment失败: %v", err))
			return fmt.Errorf("回滚Deployment失败: %v", err)
		}
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "RollbackWorkload",
			"took":   time.Since(startTime),
		}).Infof(color.GreenString("Deployment回滚成功: %s", oldImage))
		return nil
	}

	// 步骤2：尝试回滚DaemonSet
	ds, err := k.Clientset.AppsV1().DaemonSets(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err == nil {
		updated := false
		for i := range ds.Spec.Template.Spec.Containers {
			if strings.Contains(ds.Spec.Template.Spec.Containers[i].Image, ":") {
				ds.Spec.Template.Spec.Containers[i].Image = oldImage
				updated = true
			}
		}
		if !updated {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "RollbackWorkload",
				"took":   time.Since(startTime),
			}).Errorf(color.RedString("未找到镜像可回滚"))
			return fmt.Errorf("未找到镜像可回滚")
		}
		_, err := k.Clientset.AppsV1().DaemonSets(namespace).Update(context.TODO(), ds, metav1.UpdateOptions{})
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "RollbackWorkload",
				"took":   time.Since(startTime),
			}).Errorf(color.RedString("回滚DaemonSet失败: %v", err))
			return fmt.Errorf("回滚DaemonSet失败: %v", err)
		}
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "RollbackWorkload",
			"took":   time.Since(startTime),
		}).Infof(color.GreenString("DaemonSet回滚成功: %s", oldImage))
		return nil
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "RollbackWorkload",
		"took":   time.Since(startTime),
	}).Errorf(color.RedString("回滚工作负载失败: %v", err))
	return fmt.Errorf("回滚工作负载失败: %v", err)
}

// IsWorkloadRunning 检查是否有Pod运行
func (k *K8sClient) IsWorkloadRunning(namespace, name string) bool {
	startTime := time.Now()
	// 步骤1：获取Pod列表
	pods, err := k.Clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{"app": name}).String(),
	})
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "IsWorkloadRunning",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("获取Pod列表失败: %v", err))
		return false
	}

	// 步骤2：检查是否有运行中的Pod
	if len(pods.Items) == 0 {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "IsWorkloadRunning",
			"took":   time.Since(startTime),
		}).Infof(color.GreenString("工作负载 %s 在命名空间 %s 无运行中的Pod", name, namespace))
		return false
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "IsWorkloadRunning",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("工作负载 %s 在命名空间 %s 有 %d 个运行中的Pod", name, namespace, len(pods.Items)))
	return true
}

// DiscoverServicesFromNamespace 发现命名空间中的服务（Deployment和DaemonSet）
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
		}).Errorf(color.RedString("列出Deployment失败: %v", err))
		return nil, fmt.Errorf("列出Deployment失败: %v", err)
	}

	// 步骤2：列出DaemonSet
	dss, err := k.Clientset.AppsV1().DaemonSets(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "DiscoverServicesFromNamespace",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("列出DaemonSet失败: %v", err))
		return nil, fmt.Errorf("列出DaemonSet失败: %v", err)
	}

	// 步骤3：收集服务和版本
	var services []string
	for _, deploy := range deploys.Items {
		serviceName := deploy.Name
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "DiscoverServicesFromNamespace",
			"took":   time.Since(startTime),
		}).Debugf("发现Deployment服务: %s", serviceName)

		image := k.GetCurrentImage(namespace, serviceName)
		if image == "unknown" {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "DiscoverServicesFromNamespace",
				"took":   time.Since(startTime),
			}).Warnf(color.RedString("获取Deployment服务 [%s] 版本失败", serviceName))
			continue
		}
		services = append(services, fmt.Sprintf("%s:%s", serviceName, image))
	}

	for _, ds := range dss.Items {
		serviceName := ds.Name
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "DiscoverServicesFromNamespace",
			"took":   time.Since(startTime),
		}).Debugf("发现DaemonSet服务: %s", serviceName)

		image := k.GetCurrentImage(namespace, serviceName)
		if image == "unknown" {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "DiscoverServicesFromNamespace",
				"took":   time.Since(startTime),
			}).Warnf(color.RedString("获取DaemonSet服务 [%s] 版本失败", serviceName))
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