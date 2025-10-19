package main

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "log"
    "net/http"
    "os"
    "path/filepath"
    "regexp"
    "sort"
    "strings"
    "time"

    "k8s.io/apimachinery/pkg/runtime/schema"
    "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

    "k8s-cicd/internal/config"
    k8shttp "k8s-cicd/internal/http"
    "k8s-cicd/internal/k8s"
    "k8s-cicd/internal/queue"
    "k8s-cicd/internal/storage"
    "k8s-cicd/internal/telegram"
    "k8s-cicd/internal/types"
)

var (
    cfg       *config.Config
    k8sClient *k8s.Client
    taskQueue *queue.Queue
)

func main() {
    cfg = config.LoadConfig("config.yaml")
    cfg.StorageDir = "cicd_storage"
    k8sClient = k8s.NewClient(cfg.KubeConfigPath)

    if err := storage.EnsureStorageDir(cfg.StorageDir); err != nil {
        log.Fatalf("Failed to create storage dir: %v", err)
    }

    initServicesDir(cfg)

    storage.InitAllDailyFiles(cfg, k8sClient.Client())
    storage.UpdateAllDeploymentVersions(cfg, k8sClient.Client())
    go reportToGateway(cfg)

    taskQueue = queue.NewQueue(cfg, 100)

    log.Println("Performing initial task poll")
    retryPendingTasks()

    for i := 0; i < cfg.MaxConcurrency; i++ {
        go worker()
    }

    go storage.DailyMaintenance(cfg, k8sClient.Client())
    go pollGateway()

    updateAndReportServices()
    go periodicUpdateServices()

    select {}
}

func initServicesDir(cfg *config.Config) {
    if _, err := os.Stat(cfg.ServicesDir); os.IsNotExist(err) {
        if err := os.MkdirAll(cfg.ServicesDir, 0755); err != nil {
            log.Fatalf("Failed to create services dir: %v", err)
        }
        for category := range cfg.TelegramBots {
            filePath := filepath.Join(cfg.ServicesDir, fmt.Sprintf("%s.svc.list", category))
            if err := os.WriteFile(filePath, []byte(""), 0644); err != nil {
                log.Printf("Failed to init service file %s: %v", filePath, err)
            }
        }
        filePath := filepath.Join(cfg.ServicesDir, "other.svc.list")
        if err := os.WriteFile(filePath, []byte(""), 0644); err != nil {
            log.Printf("Failed to init service file %s: %v", filePath, err)
        }
        log.Println("Initialized services directory and files")
    }
}

func updateAndReportServices() {
    log.Println("Immediate updating services")
    storage.UpdateAllDeploymentVersions(cfg, k8sClient.Client())
    go reportServicesToGateway(cfg)
}

func periodicUpdateServices() {
    ticker := time.NewTicker(10 * time.Minute)
    defer ticker.Stop()

    for range ticker.C {
        updateAndReportServices()
    }
}

func reportServicesToGateway(cfg *config.Config) {
    services, err := collectAndClassifyServices(cfg)
    if err != nil {
        log.Printf("Failed to collect services: %v", err)
        return
    }

    data, err := json.Marshal(services)
    if err != nil {
        log.Printf("Failed to marshal services: %v", err)
        return
    }

    attempt := 1
    for {
        req, err := http.NewRequest("POST", cfg.GatewayURL+"/services", bytes.NewBuffer(data))
        if err != nil {
            log.Printf("Failed to create services request (attempt %d): %v", attempt, err)
            time.Sleep(getBackoff(attempt))
            attempt++
            continue
        }
        req.Header.Set("Content-Type", "application/json")

        client := &http.Client{Timeout: 10 * time.Second}
        resp, err := client.Do(req)
        if err != nil {
            log.Printf("Failed to send services to gateway (attempt %d): %v", attempt, err)
            time.Sleep(getBackoff(attempt))
            attempt++
            continue
        }
        defer resp.Body.Close()

        if resp.StatusCode != http.StatusOK {
            log.Printf("Gateway services failed with status %d (attempt %d)", resp.StatusCode, attempt)
            time.Sleep(getBackoff(attempt))
            attempt++
            continue
        }

        log.Printf("Successfully reported services to gateway")
        return
    }
}

func reportEnvironmentsToGateway(cfg *config.Config) {
    data, err := json.Marshal(cfg.Environments)
    if err != nil {
        log.Printf("Failed to marshal environments: %v", err)
        return
    }
    attempt := 1
    for {
        req, err := http.NewRequest("POST", cfg.GatewayURL+"/environments", bytes.NewBuffer(data))
        if err != nil {
            log.Printf("Failed to create environments request (attempt %d): %v", attempt, err)
            time.Sleep(getBackoff(attempt))
            attempt++
            continue
        }
        req.Header.Set("Content-Type", "application/json")

        client := &http.Client{Timeout: 10 * time.Second}
        resp, err := client.Do(req)
        if err != nil {
            log.Printf("Failed to send environments to gateway (attempt %d): %v", attempt, err)
            time.Sleep(getBackoff(attempt))
            attempt++
            continue
        }
        defer resp.Body.Close()

        if resp.StatusCode != http.StatusOK {
            log.Printf("Gateway environments failed with status %d (attempt %d)", resp.StatusCode, attempt)
            time.Sleep(getBackoff(attempt))
            attempt++
            continue
        }

        log.Printf("Successfully reported environments to gateway")
        return
    }
}

func verifyDataFromGateway(cfg *config.Config) bool {
    attempt := 1
    for {
        resp, err := http.Get(cfg.GatewayURL + "/verify-data")
        if err != nil {
            log.Printf("Failed to verify data from gateway (attempt %d): %v", attempt, err)
            time.Sleep(getBackoff(attempt))
            attempt++
            if attempt > 5 {
                return false
            }
            continue
        }
        defer resp.Body.Close()

        if resp.StatusCode != http.StatusOK {
            log.Printf("Gateway verify-data failed with status %d (attempt %d)", resp.StatusCode, attempt)
            time.Sleep(getBackoff(attempt))
            attempt++
            if attempt > 5 {
                return false
            }
            continue
        }

        var status map[string]bool
        if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
            log.Printf("Failed to decode verify-data response (attempt %d): %v", attempt, err)
            time.Sleep(getBackoff(attempt))
            attempt++
            if attempt > 5 {
                return false
            }
            continue
        }

        if status["environments"] && status["services"] {
            log.Printf("Verified data persistence on gateway")
            return true
        }

        log.Printf("Data not fully persisted on gateway, retrying...")
        time.Sleep(getBackoff(attempt))
        attempt++
        if attempt > 5 {
            return false
        }
    }
}

func pollGateway() {
    ticker := time.NewTicker(time.Duration(cfg.PollInterval) * time.Second)
    defer ticker.Stop()

    for range ticker.C {
        for env, _ := range cfg.Environments { // namespace unused, kept for potential future use
            ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
            defer cancel()

            tasks, respMap, err := k8shttp.FetchTasks(ctx, cfg.GatewayURL, env)
            if err != nil {
                log.Printf("Failed to fetch tasks for env %s: %v", env, err)
                continue
            }

            // Check if gateway reported a restart
            if status, ok := respMap["status"].(string); ok && status == "restarted" {
                log.Printf("Gateway reported restarted for env %s, triggering full report", env)
                go reportToGateway(cfg)              // Report deployment info
                go reportServicesToGateway(cfg)      // Report service list
                go reportEnvironmentsToGateway(cfg)  // Report environment list
                if !verifyDataFromGateway(cfg) {
                    log.Printf("Verification failed, will retry next poll")
                }
            }

            for _, task := range tasks {
                taskKey := queue.ComputeTaskKey(task)
                if taskQueue.Exists(taskKey) {
                    log.Printf("Task %s already exists, skipping", taskKey)
                    continue
                }
                taskQueue.Enqueue(queue.Task{DeployRequest: task})
                log.Printf("Enqueued task %s for env %s", taskKey, env)
            }
        }
    }
}

func getBackoff(attempt int) time.Duration {
    backoff := time.Duration(1<<uint(attempt-1)) * time.Second
    if backoff > 60*time.Second {
        return 60 * time.Second
    }
    return backoff
}

func collectAndClassifyServices(cfg *config.Config) (map[string][]string, error) {
    fileName := storage.GetDailyFileName(time.Now(), "deploy", cfg.StorageDir)
    data, err := os.ReadFile(fileName)
    if err != nil {
        return nil, fmt.Errorf("failed to read deploy file: %v", err)
    }
    var infos []storage.DeploymentInfo
    if err := json.Unmarshal(data, &infos); err != nil {
        return nil, fmt.Errorf("failed to unmarshal deploy file: %v", err)
    }

    classified := make(map[string][]string)
    seenServices := make(map[string]bool)
    for _, info := range infos {
        if _, ok := seenServices[info.Service]; !ok {
            seenServices[info.Service] = true
            category := classifyService(info.Service, cfg.ServiceKeywords)
            classified[category] = append(classified[category], info.Service)
        }
    }

    for category := range classified {
        sort.Strings(classified[category])
    }
    return classified, nil
}

func classifyService(service string, keywords map[string][]string) string {
    lowerService := strings.ToLower(service)
    for category, patterns := range keywords {
        for _, pattern := range patterns {
            lowerPattern := strings.ToLower(pattern)
            if strings.HasPrefix(lowerPattern, "^") || strings.HasSuffix(lowerPattern, "$") || strings.Contains(lowerPattern, ".*") {
                re, err := regexp.Compile(lowerPattern)
                if err == nil && re.MatchString(lowerService) {
                    return category
                }
            } else if strings.Contains(lowerService, lowerPattern) {
                return category
            }
        }
    }
    return "other"
}

func retryPendingTasks() {
    fileName := storage.GetDailyFileName(time.Now(), "deploy", cfg.StorageDir)
    data, err := os.ReadFile(fileName)
    if err != nil {
        log.Printf("Failed to read deploy file for retry: %v", err)
        return
    }
    var infos []storage.DeploymentInfo
    if err := json.Unmarshal(data, &infos); err != nil {
        log.Printf("Failed to unmarshal deploy file for retry: %v", err)
        return
    }

    for _, info := range infos {
        if info.Status == "pending" {
            task := queue.Task{
                DeployRequest: types.DeployRequest{
                    Service:   info.Service,
                    Env:       info.Env,
                    Version:   getVersionFromImage(info.Image),
                    Timestamp: info.Timestamp,
                    UserName:  info.UserName,
                    Status:    "pending",
                },
            }
            taskQueue.Enqueue(task)
            log.Printf("Retried pending task for service %s in env %s", info.Service, info.Env)
        }
    }
}

func worker() {
    for task := range taskQueue.Dequeue() {
        taskKey := queue.ComputeTaskKey(task.DeployRequest)
        log.Printf("Processing task %s: service=%s, env=%s, version=%s, user=%s",
            taskKey, task.DeployRequest.Service, task.DeployRequest.Env, task.DeployRequest.Version, task.DeployRequest.UserName)

        namespace, ok := cfg.Environments[strings.ToLower(task.DeployRequest.Env)]
        if !ok {
            log.Printf("Invalid environment %s for task %s", task.DeployRequest.Env, taskKey)
            continue
        }

        result := storage.DeployResult{
            Request: task.DeployRequest,
            Envs:    make(map[string]string),
        }

        newImage, err := k8sClient.GetNewImage(task.DeployRequest.Service, task.DeployRequest.Version, namespace)
        if err != nil {
            result.Success = false
            result.ErrorMsg = fmt.Sprintf("Failed to get new image: %v", err)
            log.Printf("Failed to get new image for task %s: %v", taskKey, err)
            storage.PersistDeployment(cfg, result.Request.Service, result.Request.Env, newImage, "failed", result.Request.UserName)
            go telegram.SendTelegramNotification(cfg, &result)
            go telegram.NotifyDeployTeam(cfg, &result)
            feedbackCompleteToGateway(cfg, taskKey)
            continue
        }

        updateCtx, updateCancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSeconds)*time.Second)
        defer updateCancel()

        success, errMsg, oldImage := k8sClient.UpdateDeployment(updateCtx, task.DeployRequest.Service, newImage, namespace)
        if success {
            checkCtx, checkCancel := context.WithTimeout(context.Background(), 1*time.Minute)
            defer checkCancel()

            // Enhanced pod status checking
            maxAttempts := 10
            interval := 5 * time.Second
            newPodsReady := false
            var checkErr error
            for attempt := 1; attempt <= maxAttempts; attempt++ {
                pods := k8sClient.GetPodsForDeployment(checkCtx, task.DeployRequest.Service, namespace)
                newPodsReady = true
                var errMsg strings.Builder
                var logsBuf strings.Builder
                result.Events = k8sClient.GetPodEvents(checkCtx, task.DeployRequest.Service, namespace)

                for _, pod := range pods {
                    phase := pod.Status.Phase
                    podGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
                    podObj, err := k8sClient.Client().CoreV1().Pods(namespace).Get(checkCtx, pod.Name, v1.GetOptions{})
                    if err != nil {
                        newPodsReady = false
                        errMsg.WriteString(fmt.Sprintf("Pod %s: Failed to get status: %v\n", pod.Name, err))
                        continue
                    }
                    conditions, _, _ := unstructured.NestedSlice(podObj.Object, "status", "conditions")
                    ready := false
                    for _, cond := range conditions {
                        c, ok := cond.(map[string]interface{})
                        if ok && c["type"] == "Ready" && c["status"] == "True" {
                            ready = true
                            break
                        }
                    }
                    if phase == "Failed" || phase == "CrashLoopBackOff" || phase == "Evicted" {
                        newPodsReady = false
                        logsBuf.WriteString(fmt.Sprintf("Pod %s (%s): %s\n", pod.Name, phase, k8sClient.GetPodLogs(checkCtx, pod.Name, namespace)))
                    } else if phase != "Running" || !ready {
                        newPodsReady = false
                        logsBuf.WriteString(fmt.Sprintf("Pod %s (%s, Ready=%v): %s\n", pod.Name, phase, ready, k8sClient.GetPodLogs(checkCtx, pod.Name, namespace)))
                    }
                }
                result.Logs = logsBuf.String()
                if !newPodsReady {
                    checkErr = fmt.Errorf("new pods not ready: %s", errMsg.String())
                }
                if newPodsReady {
                    break
                }
                if attempt < maxAttempts {
                    select {
                    case <-checkCtx.Done():
                        checkErr = fmt.Errorf("context cancelled: %v", checkCtx.Err())
                        break
                    case <-time.After(interval):
                    }
                }
            }

            if newPodsReady {
                result.Success = true
                result.OldImage = oldImage
                storage.PersistDeployment(cfg, result.Request.Service, result.Request.Env, newImage, "success", result.Request.UserName)
                log.Printf("Deployment succeeded for task %s: service=%s, env=%s, version=%s, user=%s",
                    taskKey, result.Request.Service, result.Request.Env, result.Request.Version, result.Request.UserName)
                go telegram.SendTelegramNotification(cfg, &result)
                feedbackCompleteToGateway(cfg, taskKey)
            } else {
                result.Success = false
                result.ErrorMsg = fmt.Sprintf("Pod check failed: %v", checkErr)
                result.OldImage = oldImage

                // Perform rollback
                rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), 1*time.Minute)
                defer rollbackCancel()
                rollbackErr := k8sClient.RollbackDeployment(rollbackCtx, task.DeployRequest.Service, namespace)
                if rollbackErr != nil {
                    result.ErrorMsg += fmt.Sprintf("; Rollback failed: %v", rollbackErr)
                } else {
                    rollbackReady, rollbackErr := k8sClient.CheckNewPodStatus(rollbackCtx, task.DeployRequest.Service, oldImage, namespace)
                    if rollbackReady {
                        result.ErrorMsg += fmt.Sprintf("; Rollback succeeded to version: %s", getVersionFromImage(oldImage))
                    } else {
                        result.ErrorMsg += fmt.Sprintf("; Rollback failed: %v", rollbackErr)
                    }
                }

                storage.PersistDeployment(cfg, result.Request.Service, result.Request.Env, newImage, "failed", result.Request.UserName)
                go telegram.SendTelegramNotification(cfg, &result)
                go telegram.NotifyDeployTeam(cfg, &result)
                feedbackCompleteToGateway(cfg, taskKey)
            }
        } else {
            result.Success = false
            result.ErrorMsg = errMsg
            result.OldImage = oldImage
            result.Events = k8sClient.GetPodEvents(updateCtx, task.DeployRequest.Service, namespace)
            result.Logs = "Deployment update failed"
            storage.PersistDeployment(cfg, result.Request.Service, result.Request.Env, newImage, "failed", result.Request.UserName)
            go telegram.SendTelegramNotification(cfg, &result)
            go telegram.NotifyDeployTeam(cfg, &result)
            feedbackCompleteToGateway(cfg, taskKey)
        }
    }
}

func getVersionFromImage(image string) string {
    parts := strings.Split(image, ":")
    if len(parts) == 2 {
        return parts[1]
    }
    return "unknown"
}

func feedbackCompleteToGateway(cfg *config.Config, taskKey string) {
    data, err := json.Marshal(map[string]string{"task_key": taskKey})
    if err != nil {
        log.Printf("Failed to marshal complete data for task %s: %v", taskKey, err)
        return
    }

    attempt := 1
    for {
        req, err := http.NewRequest("POST", cfg.GatewayURL+"/complete", bytes.NewBuffer(data))
        if err != nil {
            log.Printf("Failed to create complete request for task %s (attempt %d): %v", taskKey, attempt, err)
            time.Sleep(getBackoff(attempt))
            attempt++
            continue
        }
        req.Header.Set("Content-Type", "application/json")

        client := &http.Client{Timeout: 10 * time.Second}
        resp, err := client.Do(req)
        if err != nil {
            log.Printf("Failed to send complete to gateway for task %s (attempt %d): %v", taskKey, attempt, err)
            time.Sleep(getBackoff(attempt))
            attempt++
            continue
        }
        defer resp.Body.Close()

        if resp.StatusCode != http.StatusOK {
            log.Printf("Gateway complete failed with status %d for task %s (attempt %d)", resp.StatusCode, taskKey, attempt)
            time.Sleep(getBackoff(attempt))
            attempt++
            continue
        }

        log.Printf("Feedback complete for task %s", taskKey)
        return
    }
}

func reportToGateway(cfg *config.Config) {
    fileName := storage.GetDailyFileName(time.Now(), "deploy", cfg.StorageDir)
    attempt := 1
    for {
        data, err := os.ReadFile(fileName)
        if err != nil {
            log.Printf("Failed to read deploy file for report (attempt %d): %v", attempt, err)
            time.Sleep(getBackoff(attempt))
            attempt++
            continue
        }
        var infos []storage.DeploymentInfo
        if err := json.Unmarshal(data, &infos); err != nil {
            log.Printf("Failed to unmarshal deploy file for report (attempt %d): %v", attempt, err)
            time.Sleep(getBackoff(attempt))
            attempt++
            continue
        }

        reportData, err := json.Marshal(infos)
        if err != nil {
            log.Printf("Failed to marshal report (attempt %d): %v", attempt, err)
            time.Sleep(getBackoff(attempt))
            attempt++
            continue
        }

        req, err := http.NewRequest("POST", cfg.GatewayURL+"/report", bytes.NewBuffer(reportData))
        if err != nil {
            log.Printf("Failed to create report request (attempt %d): %v", attempt, err)
            time.Sleep(getBackoff(attempt))
            attempt++
            continue
        }
        req.Header.Set("Content-Type", "application/json")

        client := &http.Client{Timeout: 10 * time.Second}
        resp, err := client.Do(req)
        if err != nil {
            log.Printf("Failed to send report to gateway (attempt %d): %v", attempt, err)
            time.Sleep(getBackoff(attempt))
            attempt++
            continue
        }
        defer resp.Body.Close()

        if resp.StatusCode != http.StatusOK {
            log.Printf("Gateway report failed with status %d (attempt %d)", resp.StatusCode, attempt)
            time.Sleep(getBackoff(attempt))
            attempt++
            continue
        }

        log.Printf("Successfully reported to gateway")
        return
    }
}