package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"k8s-cicd/internal/storage"
)

func FetchTasks(ctx context.Context, gatewayURL string) ([]storage.DeployRequest, error) {
	body, _ := json.Marshal(map[string]string{"env": "prod"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatewayURL+"/tasks", bytes.NewBuffer(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch tasks from gateway: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gateway returned status: %d", resp.StatusCode)
	}

	var tasks []storage.DeployRequest
	if err := json.NewDecoder(resp.Body).Decode(&tasks); err != nil {
		return nil, fmt.Errorf("failed to decode tasks response: %v", err)
	}
	return tasks, nil
}