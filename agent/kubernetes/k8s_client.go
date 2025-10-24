//
package kubernetes

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	oldImage := k.getCurrentImage(deploy)
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "UpdateDeploymentImage",
		"took":   time.Since(startTime),
	}).Infof("当前镜像: %s", oldImage)

	// 步骤3：更新镜像
	for i := range deploy.Spec.Template.Spec.Containers {
		deploy.Spec.Template.Spec.Containers[i].Image = newImage
	}

	// 步骤4：添加版本追踪注解
	if deploy.Annotations == nil {
		deploy.Annotations = make(map[string]string)
	}
	deploy.Annotations["deployment.cicd.k8s/version"] = newImage
	deploy.Annotations["deployment.cicd.k8s/updated-at"] = time.Now().Format(time.RFC3339)

	// 步骤5：更新Deployment
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

// getCurrentImage 获取当前镜像
func (k *K8sClient) getCurrentImage(deploy *appsv1.Deployment) string {
	for _, container := range deploy.Spec.Template.Spec.Containers {
		if strings.Contains(container.Image, ":") {
			return container.Image
		}
	}
	return "unknown"
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

		image := k.getCurrentImage(&deploy)
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
//