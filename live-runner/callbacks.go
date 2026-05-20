package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

type callbackClient struct {
	httpClient *http.Client
}

func newCallbackClient(timeout time.Duration) *callbackClient {
	return &callbackClient{
		httpClient: &http.Client{Timeout: timeout},
	}
}

func (c *callbackClient) disabled() bool {
	return c == nil || c.httpClient == nil
}

func (c *callbackClient) send(ctx context.Context, cfg brokerCallbackConfig, env eventEnvelope) {
	if c.disabled() || cfg.EventURL == "" {
		return
	}
	body, err := json.Marshal(env)
	if err != nil {
		log.Printf("live callback marshal error: %v", err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.EventURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("live callback build req error: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		log.Printf("live callback send error session=%s type=%s: %v", env.RunnerSessionID, env.EventType, err)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 400 {
		log.Printf("live callback error session=%s type=%s status=%d", env.RunnerSessionID, env.EventType, resp.StatusCode)
	}
}
