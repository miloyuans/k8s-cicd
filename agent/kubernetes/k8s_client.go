// 文件: kubernetes/k8s_client.go
// 功能：
// 1. 每次发布前删除 service+namespace 所有快照
// 2. 精准快照：仅就绪（ReadyReplicas>0）且 Age>1s 的 ReplicaSet
// 3. Pod 必须 Running + ContainersReady
// 4. 兼容 Deployment/StatefulSet/DaemonSet
// 5. 导出 BuildNewImage、ExtractTag

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

	"k8s-cicd/agent/client"
	"k8s-cicd/agent/config"
	"k8s-cicd/agent/models"

	"github.com/fatih/color"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	appsv1 "k8s.io/api/apps/v1"
)

// K8sClient Kubernetes 客户端
type K8sClient struct {
	Clientset *kubernetes.Clientset
	cfg       *config.DeployConfig
}

// NewK8sClient 创建 K8s 客户端
func NewK8sClient(k8sCfg *config.K8sAuthConfig, deployCfg *config.DeployConfig) (*K8sClient, error) {
	// ...（保持不变）
}

// ExtractTag / ExtractBaseImage / BuildNewImage（保持不变）
func ExtractTag(image string) string { /* ... */ }
func ExtractBaseImage(image string) string { /* ... */ }
func BuildNewImage(currentImage, newTag string) string { /* ... */ }

// DeleteOldSnapshots 删除 service+namespace 下的所有历史快照
func (k *K8sClient) DeleteOldSnapshots(service, namespace string, mongo *client.MongoClient) error {
	ctx := context.Background()
	collection := mongo.client.Database("cicd").Collection("image_snapshots")

	filter := map[string]interface{}{
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

// SnapshotAndStoreImage 获取当前镜像并存储快照（精准版）
func (k *K8sClient) SnapshotAndStoreImage(service, namespace, taskID string, mongo *client.MongoClient) (string, error) {
	ctx := context.Background()

	// Step 1: 清理历史快照
	if err := k.DeleteOldSnapshots(service, namespace, mongo); err != nil {
		return "", err
	}

	// Step 2: 尝试获取 Deployment
	deploy, err := k.Clientset.AppsV1().Deployments(namespace).Get(ctx, service, metav1.GetOptions{})
	if err != nil {
		// 非 Deployment → 使用基本标签
		return k.snapshotFromBasicLabel(service, namespace, taskID, mongo)
	}

	// Step 3: 获取所有 ReplicaSet
	rsList, err := k.Clientset.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", service),
	})
	if err != nil || len(rsList.Items) == 0 {
		return "", fmt.Errorf("未找到 ReplicaSet")
	}

	// Step 4: 筛选 ReadyReplicas > 0 且 Age > 1s 的 RS
	now := time.Now()
	candidates := []appsv1.ReplicaSet{}
	for _, rs := range rsList.Items {
		ready := rs.Status.ReadyReplicas > 0
		age := now.Sub(rs.CreationTimestamp.Time)
		if ready && age > time.Second {
			candidates = append(candidates, rs)
		}
	}

	if len(candidates) == 0 {
		return "", fmt.Errorf("无就绪且 Age>1s 的 ReplicaSet")
	}

	// Step 5: 按创建时间降序，取最新
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].CreationTimestamp.After(candidates[j].CreationTimestamp.Time)
	})
	rs := candidates[0]

	// Step 6: 获取 pod-template-hash
	hash, ok := rs.Labels["pod-template-hash"]
	if !ok {
		return "", fmt.Errorf("ReplicaSet 无 pod-template-hash")
	}
	labelSelector := fmt.Sprintf("app=%s,pod-template-hash=%s", service, hash)

	// Step 7: 查询 Pod
	pods, err := k.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return "", fmt.Errorf("查询 Pod 失败: %v", err)
	}

	// Step 8: 筛选 Running + Ready Pod
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

	// Step 9: 提取镜像
	pod := runningReadyPods[0]
	if len(pod.Spec.Containers) == 0 {
		return "", fmt.Errorf("Pod 无容器")
	}
	oldImage := pod.Spec.Containers[0].Image
	oldTag := ExtractTag(oldImage)

	// Step 10: 存储快照
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
		"task_id": taskID,
		"rs_name": rs.Name,
		"hash":    hash,
		"age":     now.Sub(rs.CreationTimestamp.Time).Round(time.Second),
		"ready":   rs.Status.ReadyReplicas,
		"pods":    len(runningReadyPods),
		"old_tag": oldTag,
	}).Info("快照记录成功（精准版）")
	return oldTag, nil
}

// snapshotFromBasicLabel 非 Deployment 时使用
func (k *K8sClient) snapshotFromBasicLabel(service, namespace, taskID string, mongo *client.MongoClient) (string, error) {
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
	oldImage := pod.Spec.Containers[0].Image
	oldTag := ExtractTag(oldImage)

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
		"task_id": taskID,
		"pods":    len(runningReadyPods),
		"old_tag": oldTag,
	}).Info("快照记录成功（基本标签）")
	return oldTag, nil
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