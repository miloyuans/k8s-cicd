// 文件: kubernetes/k8s_client.go
// 修改: SnapshotAndStoreImage 升级为：仅记录 Running 状态的 Pod 镜像作为旧版本快照
// 1. 获取所有 Pod
// 2. 过滤出 Running 状态的 Pod
// 3. 若无 Running Pod → 返回错误，任务失败
// 4. 若有多个 Running Pod，取第一个容器的镜像（假设一致）
// 5. 存储快照并返回旧 tag
// 文件: kubernetes/k8s_client.go
// 功能：
// 1. 每次发布前删除 service+namespace 下的所有历史快照
// 2. 精准快照：仅 Running + Ready 的 Pod（使用 ReplicaSet pod-template-hash）
// 3. 兼容 Deployment/StatefulSet/DaemonSet
// 4. 导出 BuildNewImage、ExtractTag 等

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

// DeleteOldSnapshots 删除 service+namespace 下的所有历史快照
func (k *K8sClient) DeleteOldSnapshots(service, namespace string, mongo *client.MongoClient) error {
	ctx := context.Background()
	collection := mongo.client.Database("cicd").Collection("image_snapshots")

	filter := bson.M{
		"service":   service,
		"namespace": namespace,
	}

	result, err := collection.DeleteMany(ctx, filter)
	if err != nil {
		return fmt.Errorf("删除历史快照失败: %v", err)
	}

	logrus.WithFields(logrus.Fields{
		"service":   service,
		"namespace": namespace,
		"deleted":   result.DeletedCount,
	}).Info("历史快照清理完成")
	return nil
}

// SnapshotAndStoreImage 获取当前镜像并存储快照（仅 Running + Ready Pod）
func (k *K8sClient) SnapshotAndStoreImage(service, namespace, taskID string, mongo *client.MongoClient) (string, error) {
	ctx := context.Background()

	// Step 1: 清理历史快照
	if err := k.DeleteOldSnapshots(service, namespace, mongo); err != nil {
		return "", err
	}

	// Step 2: 获取工作负载类型
	var rs *appsv1.ReplicaSet
	var ownerRef *metav1.OwnerReference
	var labelSelector string

	// 尝试 Deployment
	deploy, err := k.Clientset.AppsV1().Deployments(namespace).Get(ctx, service, metav1.GetOptions{})
	if err == nil {
		// 获取最新 ReplicaSet
		rsList, err := k.Clientset.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app=%s", service),
		})
		if err != nil || len(rsList.Items) == 0 {
			return "", fmt.Errorf("未找到 ReplicaSet")
		}
		// 按创建时间降序
		sort.Slice(rsList.Items, func(i, j int) bool {
			return rsList.Items[i].CreationTimestamp.After(rsList.Items[j].CreationTimestamp.Time)
		})
		rs = &rsList.Items[0]
		ownerRef = &metav1.OwnerReference{
			Kind: "ReplicaSet",
			Name: rs.Name,
			UID:  rs.UID,
		}
		hash, ok := rs.Labels["pod-template-hash"]
		if !ok {
			return "", fmt.Errorf("ReplicaSet 无 pod-template-hash")
		}
		labelSelector = fmt.Sprintf("app=%s,pod-template-hash=%s", service, hash)
	} else {
		// 尝试 StatefulSet / DaemonSet
		labelSelector = fmt.Sprintf("app=%s", service)
	}

	// Step 3: 查询 Pod
	pods, err := k.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return "", fmt.Errorf("查询 Pod 失败: %v", err)
	}

	// Step 4: 筛选 Running + Ready Pod
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
		return "", fmt.Errorf("无 Running + Ready 的 Pod，无法记录快照")
	}

	// Step 5: 提取镜像（假设一致）
	pod := runningReadyPods[0]
	if len(pod.Spec.Containers) == 0 {
		return "", fmt.Errorf("Pod 无容器")
	}
	oldImage := pod.Spec.Containers[0].Image
	oldTag := ExtractTag(oldImage)

	// Step 6: 存储快照
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
		"task_id":   taskID,
		"service":   service,
		"namespace": namespace,
		"old_image": oldImage,
		"pods":      len(runningReadyPods),
	}).Info("快照记录成功（基于 Running + Ready Pod）")
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