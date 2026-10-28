package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// checkHealth also runs inside the scratch image, which has no shell or curl.
// It never loads secrets, opens the outbox, or sends conversion events.
func checkHealth(endpoint string) error {
	client := &http.Client{
		Timeout: 3 * time.Second,
		// Health probes must not travel through a deployment's HTTP proxy.
		Transport:     &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	response, err := client.Get(endpoint)
	if err != nil {
		return fmt.Errorf("healthcheck: request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(body) > 4096 {
		return fmt.Errorf("healthcheck: invalid response")
	}
	var result struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(body, &result) != nil || result.Status != "ok" {
		return fmt.Errorf("healthcheck: invalid response")
	}
	return nil
}
