package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type syncTarget struct {
	client *s3.Client
	bucket string
	prefix string
}

type fileSnapshot struct {
	size    int64
	modTime time.Time
}

type retryState struct {
	consecutiveFailures uint64
	nextAttemptAt       time.Time
}

func startOutputSync(rt *sessionRuntime) error {
	if rt.rec.Mode != modeGatewayIngest || rt.rec.OutputCredential == nil {
		return nil
	}
	target, err := newSyncTarget(rt.rec.OutputCredential)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt.uploadCancel = cancel
	rt.uploadDone = make(chan struct{})
	go func() {
		defer close(rt.uploadDone)
		watchAndUpload(ctx, rt, target)
	}()
	return nil
}

func newSyncTarget(cred *s3OutputCredential) (*syncTarget, error) {
	if cred == nil {
		return nil, fmt.Errorf("missing output credential")
	}
	if cred.Endpoint == "" || cred.Region == "" || cred.Bucket == "" || cred.KeyPrefix == "" {
		return nil, fmt.Errorf("incomplete output credential")
	}
	cfg := aws.Config{
		Region:      cred.Region,
		Credentials: credentials.NewStaticCredentialsProvider(cred.AccessKeyID, cred.SecretAccessKey, cred.SessionToken),
		HTTPClient:  &http.Client{Timeout: 15 * time.Second},
		EndpointResolverWithOptions: aws.EndpointResolverWithOptionsFunc(func(service, region string, _ ...interface{}) (aws.Endpoint, error) {
			return aws.Endpoint{URL: cred.Endpoint, HostnameImmutable: true}, nil
		}),
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = true
	})
	return &syncTarget{
		client: client,
		bucket: cred.Bucket,
		prefix: strings.TrimLeft(cred.KeyPrefix, "/"),
	}, nil
}

func watchAndUpload(ctx context.Context, rt *sessionRuntime, target *syncTarget) {
	ticker := time.NewTicker(rt.cfg.OutputSyncInterval)
	defer ticker.Stop()
	seen := make(map[string]fileSnapshot)
	retries := make(map[string]retryState)
	remote := make(map[string]struct{})
	healthy := false
	for {
		ok, err := syncOutputTree(ctx, rt, target, seen, retries, remote)
		if err != nil {
			healthy = false
			sanitized := sanitizeUploadError(rt, err)
			rt.recordOutputError(fmt.Errorf("%s", sanitized))
			rt.event("session.upload.failed", rt.lastUsageTotal.Load(), 0, "", map[string]any{"error_text": sanitized})
			if threshold := rt.cfg.OutputFailureThreshold; threshold > 0 && rt.outputFailureCount() >= uint64(threshold) {
				rt.mu.Lock()
				if rt.rec.State == statePublishing {
					rt.setState(stateStalled, "output_sync_failed")
				}
				rt.mu.Unlock()
			}
		} else if ok && !healthy {
			healthy = true
			rt.mu.Lock()
			if rt.rec.State == stateStalled && rt.rec.CloseReason == "output_sync_failed" {
				rt.setState(statePublishing, "")
			}
			rt.mu.Unlock()
			rt.event("session.upload.healthy", rt.lastUsageTotal.Load(), 0, "", nil)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func syncOutputTree(ctx context.Context, rt *sessionRuntime, target *syncTarget, seen map[string]fileSnapshot, retries map[string]retryState, remote map[string]struct{}) (bool, error) {
	uploadedAny := false
	present := make(map[string]struct{})
	err := filepath.Walk(rt.rec.OutputDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(rt.rec.OutputDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		present[rel] = struct{}{}
		now := time.Now().UTC()
		snap := fileSnapshot{size: info.Size(), modTime: info.ModTime().UTC()}
		if prev, ok := seen[rel]; ok && prev == snap {
			return nil
		}
		if retry, ok := retries[rel]; ok && now.Before(retry.nextAttemptAt) {
			return nil
		}
		if err := uploadFile(ctx, target, rel, path); err != nil {
			retry := retries[rel]
			retry.consecutiveFailures++
			backoff := time.Duration(retry.consecutiveFailures)
			if backoff > 8 {
				backoff = 8
			}
			retry.nextAttemptAt = now.Add(backoff * 250 * time.Millisecond)
			retries[rel] = retry
			return err
		}
		delete(retries, rel)
		remote[rel] = struct{}{}
		seen[rel] = snap
		uploadedAny = true
		rt.recordOutputPut(rel, now)
		return nil
	})
	if err != nil {
		return false, err
	}
	stale, err := staleRemoteFiles(remote, present)
	if err != nil {
		return false, err
	}
	for _, rel := range stale {
		if err := deleteRemoteFile(ctx, target, rel); err != nil {
			return false, err
		}
		delete(remote, rel)
		delete(seen, rel)
		delete(retries, rel)
		uploadedAny = true
	}
	if uploadedAny {
		return true, nil
	}
	return false, nil
}

func uploadFile(ctx context.Context, target *syncTarget, rel, path string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	key := strings.TrimLeft(strings.TrimSuffix(target.prefix, "/")+"/"+rel, "/")
	_, err = target.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:       aws.String(target.bucket),
		Key:          aws.String(key),
		Body:         bytes.NewReader(body),
		CacheControl: aws.String(cacheControlFor(rel)),
		ContentType:  aws.String(contentTypeFor(rel)),
	})
	if err != nil {
		return fmt.Errorf("put object %s: %w", key, err)
	}
	return nil
}

func deleteRemoteFile(ctx context.Context, target *syncTarget, rel string) error {
	key := strings.TrimLeft(strings.TrimSuffix(target.prefix, "/")+"/"+rel, "/")
	_, err := target.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(target.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("delete object %s: %w", key, err)
	}
	return nil
}

func staleRemoteFiles(remote, present map[string]struct{}) ([]string, error) {
	stale := make([]string, 0)
	for rel := range remote {
		if _, ok := present[rel]; ok {
			continue
		}
		if strings.HasSuffix(rel, ".ts") || strings.HasSuffix(rel, ".m4s") {
			stale = append(stale, rel)
		}
	}
	return stale, nil
}

func sanitizeUploadError(rt *sessionRuntime, err error) string {
	if err == nil {
		return ""
	}
	secrets := []string{rt.rec.StreamKey}
	if cred := rt.rec.OutputCredential; cred != nil {
		secrets = append(secrets, cred.AccessKeyID, cred.SecretAccessKey, cred.SessionToken)
	}
	return redactSecrets(err.Error(), secrets...)
}

func cacheControlFor(name string) string {
	switch {
	case strings.HasSuffix(name, ".m3u8"):
		return "max-age=1"
	case strings.HasSuffix(name, ".m4s"):
		return "max-age=10"
	case strings.HasSuffix(name, ".ts"):
		return "max-age=10"
	case strings.HasSuffix(name, ".mp4"):
		return "max-age=86400"
	default:
		return "max-age=10"
	}
}

func contentTypeFor(name string) string {
	switch {
	case strings.HasSuffix(name, ".m3u8"):
		return "application/vnd.apple.mpegurl"
	case strings.HasSuffix(name, ".ts"):
		return "video/mp2t"
	case strings.HasSuffix(name, ".m4s"):
		return "video/iso.segment"
	case strings.HasSuffix(name, ".mp4"):
		return "video/mp4"
	default:
		return "application/octet-stream"
	}
}

func readAll(rc io.Reader) ([]byte, error) {
	return io.ReadAll(rc)
}
