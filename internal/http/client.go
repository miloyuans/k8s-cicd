// http/client.go
package http

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"k8s-cicd/internal/types"
)

func FetchTasks(ctx context.Context, gatewayURL, env string) ([]types.DeployRequest, error) {
	log.Printf("Fetching tasks for env %s from %s", env, gatewayURL)
	const maxRetries = 3
	for attempt := 1; attempt <= maxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, gatewayURL+"/tasks?env="+env, nil)
		if err != nil {
			log.Printf("Failed to create request for env %s (attempt %d/%d): %v", env, attempt, maxRetries, err)
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("Failed to fetch tasks from gateway for env %s (attempt %d/%d): %v", env, attempt, maxRetries, err)
			if attempt == maxRetries {
				return nil, fmt.Errorf("failed to fetch tasks from gateway after %d attempts: %v", maxRetries, err)
			}
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			log.Printf("Gateway returned status %d for env %s (attempt %d/%d)", resp.StatusCode, env, attempt, maxRetries)
			if attempt == maxRetries {
				return nil, fmt.Errorf("gateway returned status: %d", resp.StatusCode)
			}
			continue
		}

		var tasks []types.DeployRequest
		if err := json.NewDecoder(resp.Body).Decode(&tasks); err != nil {
			log.Printf("Failed to decode tasks response for env %s (attempt %d/%d): %v", env, attempt, maxRetries, err)
			return nil, fmt.Errorf("failed to decode tasks response: %v", err)
		}
		return tasks, nil
	}
	return nil, fmt.Errorf("failed to fetch tasks after %d attempts", maxRetries)
}