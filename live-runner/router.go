package liverunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	ErrMediaPathNotFoundV1     = errors.New("media_path_not_found")
	ErrUnexpectedMediaSourceV1 = errors.New("unexpected_media_source")
)

type MediaPathSourceV1 struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

type MediaPathStatusV1 struct {
	Name          string             `json:"name"`
	Online        bool               `json:"online"`
	OnlineTime    *string            `json:"onlineTime"`
	InboundBytes  uint64             `json:"inboundBytes"`
	OutboundBytes uint64             `json:"outboundBytes"`
	Source        *MediaPathSourceV1 `json:"source"`
}

type MediaPathReaderV1 interface {
	Path(context.Context, string) (MediaPathStatusV1, error)
}

type MediaRouterAPIErrorV1 struct {
	StatusCode int
}

func (e *MediaRouterAPIErrorV1) Error() string {
	return fmt.Sprintf("media router API returned HTTP %d", e.StatusCode)
}

func (e *MediaRouterAPIErrorV1) Retryable() bool {
	return e.StatusCode == http.StatusRequestTimeout || e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

type MediaRouterClientV1 struct {
	baseURL string
	client  *http.Client
}

func (c *MediaRouterClientV1) KickPublisher(ctx context.Context, ingestPath string) error {
	status, err := c.Path(ctx, ingestPath)
	if errors.Is(err, ErrMediaPathNotFoundV1) || (err == nil && (!status.Online || status.Source == nil)) {
		return nil
	}
	if err != nil {
		return err
	}
	if status.Source.ID == "" || (status.Source.Type != "rtmpConn" && status.Source.Type != "rtmpsConn") {
		return ErrUnexpectedMediaSourceV1
	}
	collection := "rtmpconns"
	if status.Source.Type == "rtmpsConn" {
		collection = "rtmpsconns"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v3/"+collection+"/kick/"+url.PathEscape(status.Source.ID), nil)
	if err != nil {
		return errors.New("media router kick request failed")
	}
	response, err := c.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("media router kick request failed")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode == http.StatusNotFound || (response.StatusCode >= 200 && response.StatusCode < 300) {
		return nil
	}
	return &MediaRouterAPIErrorV1{StatusCode: response.StatusCode}
}

func NewMediaRouterClientV1(baseURL string, transport http.RoundTripper, timeout time.Duration) (*MediaRouterClientV1, error) {
	if timeout <= 0 {
		return nil, errors.New("positive media router timeout is required")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("media router API URL is invalid")
	}
	host := parsed.Hostname()
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("media router API must be loopback-only")
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	client := &http.Client{
		Transport: transport, Timeout: timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &MediaRouterClientV1{baseURL: strings.TrimRight(baseURL, "/"), client: client}, nil
}

func (c *MediaRouterClientV1) Path(ctx context.Context, name string) (MediaPathStatusV1, error) {
	if _, _, _, ok := parseMediaPathV1(name); !ok {
		return MediaPathStatusV1{}, errors.New("media path is invalid")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v3/paths/get/"+url.PathEscape(name), nil)
	if err != nil {
		return MediaPathStatusV1{}, errors.New("media router request failed")
	}
	response, err := c.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return MediaPathStatusV1{}, ctx.Err()
		}
		return MediaPathStatusV1{}, errors.New("media router request failed")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return MediaPathStatusV1{}, ErrMediaPathNotFoundV1
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return MediaPathStatusV1{}, &MediaRouterAPIErrorV1{StatusCode: response.StatusCode}
	}
	var status MediaPathStatusV1
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64*1024))
	if err := decoder.Decode(&status); err != nil || status.Name != name {
		return MediaPathStatusV1{}, errors.New("media router response is invalid")
	}
	return status, nil
}

func (c *MediaRouterClientV1) WaitForRTMPPublisher(ctx context.Context, ingestPath string, interval time.Duration) (MediaPathStatusV1, error) {
	if interval <= 0 {
		return MediaPathStatusV1{}, errors.New("positive media router poll interval is required")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		status, err := c.Path(ctx, ingestPath)
		if err == nil && status.Online {
			if status.Source == nil || (status.Source.Type != "rtmpConn" && status.Source.Type != "rtmpsConn") {
				return MediaPathStatusV1{}, ErrUnexpectedMediaSourceV1
			}
			return status, nil
		}
		if err != nil && !errors.Is(err, ErrMediaPathNotFoundV1) {
			var apiError *MediaRouterAPIErrorV1
			if !errors.As(err, &apiError) || !apiError.Retryable() {
				return MediaPathStatusV1{}, err
			}
		}
		select {
		case <-ctx.Done():
			return MediaPathStatusV1{}, ctx.Err()
		case <-ticker.C:
		}
	}
}
