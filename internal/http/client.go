package httpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"internal/dialog"
)

func FetchTasks(ctx context.Context, gatewayURL string) ([]dialog.DeployRequest, error) {
	body, _ := json.Marshal(map[string]string{"env": "prod"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatewayURL+"/tasks", bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gateway returned status: %d", resp.StatusCode)
	}

	var tasks []dialog.DeployRequest
	if err := json.NewDecoder(resp.Body).Decode(&tasks); err != nil {
		return nil, err
	}
	return tasks, nil
}