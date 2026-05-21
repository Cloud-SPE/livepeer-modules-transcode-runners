package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

type syncTarget struct {
	client          *s3.Client
	endpoint        string
	region          string
	bucket          string
	prefix          string
	sessionID       string
	unsignedPayload bool
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
	target, err := newSyncTarget(rt.rec.RunnerSessionID, rt.rec.OutputCredential, rt.cfg.OutputSyncUnsignedPayload)
	if err != nil {
		return err
	}
	log.Printf("[live %s] output sync configured endpoint=%s bucket=%s prefix=%s region=%s access_key_id=%s session_token_present=%t",
		rt.rec.RunnerSessionID,
		target.endpoint,
		target.bucket,
		target.prefix,
		target.region,
		maskSecretSuffix(rt.rec.OutputCredential.AccessKeyID),
		strings.TrimSpace(rt.rec.OutputCredential.SessionToken) != "",
	)
	log.Printf("[live %s] output sync signing unsigned_payload=%t", rt.rec.RunnerSessionID, rt.cfg.OutputSyncUnsignedPayload)
	ctx, cancel := context.WithCancel(context.Background())
	rt.uploadCancel = cancel
	rt.uploadDone = make(chan struct{})
	go func() {
		defer close(rt.uploadDone)
		watchAndUpload(ctx, rt, target)
	}()
	return nil
}

func newSyncTarget(sessionID string, cred *s3OutputCredential, unsignedPayload bool) (*syncTarget, error) {
	if cred == nil {
		return nil, fmt.Errorf("missing output credential")
	}
	if cred.Endpoint == "" || cred.Region == "" || cred.Bucket == "" || cred.KeyPrefix == "" {
		return nil, fmt.Errorf("incomplete output credential")
	}
	baseTransport := http.DefaultTransport
	if bt, ok := http.DefaultTransport.(*http.Transport); ok {
		baseTransport = bt.Clone()
	}
	cfg := aws.Config{
		Region:      cred.Region,
		Credentials: credentials.NewStaticCredentialsProvider(cred.AccessKeyID, cred.SecretAccessKey, cred.SessionToken),
		HTTPClient: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &s3LoggingTransport{
				sessionID: sessionID,
				base:      baseTransport,
			},
		},
		EndpointResolverWithOptions: aws.EndpointResolverWithOptionsFunc(func(service, region string, _ ...interface{}) (aws.Endpoint, error) {
			return aws.Endpoint{URL: cred.Endpoint, HostnameImmutable: true}, nil
		}),
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = true
	})
	return &syncTarget{
		client:          client,
		endpoint:        strings.TrimSpace(cred.Endpoint),
		region:          strings.TrimSpace(cred.Region),
		bucket:          cred.Bucket,
		prefix:          strings.TrimLeft(cred.KeyPrefix, "/"),
		sessionID:       sessionID,
		unsignedPayload: unsignedPayload,
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
			log.Printf("[live %s] output sync failure endpoint=%s bucket=%s prefix=%s error=%q",
				rt.rec.RunnerSessionID, target.endpoint, target.bucket, target.prefix, sanitized,
			)
			rt.recordOutputError(fmt.Errorf("%s", sanitized))
			rt.emitUploadFailed(map[string]any{"error_text": sanitized})
			if threshold := rt.cfg.OutputFailureThreshold; threshold > 0 && rt.outputFailureCount() >= uint64(threshold) {
				log.Printf("[live %s] output sync degraded after %d consecutive failures endpoint=%s bucket=%s prefix=%s",
					rt.rec.RunnerSessionID, rt.outputFailureCount(), target.endpoint, target.bucket, target.prefix,
				)
			}
		} else if ok && !healthy {
			healthy = true
			rt.clearUploadFailure()
			log.Printf("[live %s] output sync recovered endpoint=%s bucket=%s prefix=%s",
				rt.rec.RunnerSessionID, target.endpoint, target.bucket, target.prefix,
			)
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
			log.Printf("[live %s] upload retry scheduled file=%s failures=%d next_attempt_in=%s",
				rt.rec.RunnerSessionID, rel, retry.consecutiveFailures, time.Until(retry.nextAttemptAt).Round(time.Millisecond),
			)
			return err
		}
		key := strings.TrimLeft(strings.TrimSuffix(target.prefix, "/")+"/"+rel, "/")
		log.Printf("[live %s] s3 put success endpoint=%s bucket=%s key=%s size=%d content_type=%s cache_control=%s",
			rt.rec.RunnerSessionID, target.endpoint, target.bucket, key, snap.size, contentTypeFor(rel), cacheControlFor(rel),
		)
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
		key := strings.TrimLeft(strings.TrimSuffix(target.prefix, "/")+"/"+rel, "/")
		log.Printf("[live %s] s3 delete success endpoint=%s bucket=%s key=%s",
			rt.rec.RunnerSessionID, target.endpoint, target.bucket, key,
		)
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
	input := &s3.PutObjectInput{
		Bucket:       aws.String(target.bucket),
		Key:          aws.String(key),
		Body:         bytes.NewReader(body),
		CacheControl: aws.String(cacheControlFor(rel)),
		ContentType:  aws.String(contentTypeFor(rel)),
	}
	if target.unsignedPayload {
		_, err = target.client.PutObject(ctx, input, s3.WithAPIOptions(v4.AddUnsignedPayloadMiddleware))
	} else {
		_, err = target.client.PutObject(ctx, input)
	}
	if err != nil {
		return fmt.Errorf("put object key=%s endpoint=%s bucket=%s content_type=%s cache_control=%s size=%d: %s",
			key, target.endpoint, target.bucket, contentTypeFor(rel), cacheControlFor(rel), len(body), describeUploadError(err),
		)
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
		return fmt.Errorf("delete object key=%s endpoint=%s bucket=%s: %s",
			key, target.endpoint, target.bucket, describeUploadError(err),
		)
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

type s3LoggingTransport struct {
	sessionID string
	base      http.RoundTripper
}

func (t *s3LoggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if req != nil && (req.Method == http.MethodPut || req.Method == http.MethodDelete) {
		log.Printf("[live %s] s3 request method=%s host=%s path=%s auth_scope=%s security_token_present=%t amz_date=%s content_sha256_present=%t",
			t.sessionID,
			req.Method,
			req.URL.Host,
			req.URL.Path,
			formatAuthorizationScope(req.Header.Get("Authorization")),
			req.Header.Get("X-Amz-Security-Token") != "",
			req.Header.Get("X-Amz-Date"),
			req.Header.Get("X-Amz-Content-Sha256") != "",
		)
	}
	resp, err := base.RoundTrip(req)
	if req != nil && (req.Method == http.MethodPut || req.Method == http.MethodDelete) {
		if err != nil {
			log.Printf("[live %s] s3 response method=%s host=%s path=%s transport_error=%q",
				t.sessionID, req.Method, req.URL.Host, req.URL.Path, err,
			)
		} else {
			log.Printf("[live %s] s3 response method=%s host=%s path=%s status=%d",
				t.sessionID, req.Method, req.URL.Host, req.URL.Path, resp.StatusCode,
			)
		}
	}
	return resp, err
}

func formatAuthorizationScope(auth string) string {
	const marker = "Credential="
	idx := strings.Index(auth, marker)
	if idx < 0 {
		return ""
	}
	rest := auth[idx+len(marker):]
	if end := strings.IndexByte(rest, ','); end >= 0 {
		rest = rest[:end]
	}
	parts := strings.Split(rest, "/")
	if len(parts) == 0 {
		return ""
	}
	parts[0] = maskSecretSuffix(parts[0])
	return strings.Join(parts, "/")
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

func describeUploadError(err error) string {
	if err == nil {
		return ""
	}
	parts := []string{err.Error()}

	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		parts = append(parts, "api_code="+apiErr.ErrorCode())
		if msg := strings.TrimSpace(apiErr.ErrorMessage()); msg != "" && msg != apiErr.Error() {
			parts = append(parts, "api_message="+msg)
		}
	}
	if statusErr, ok := err.(interface{ HTTPStatusCode() int }); ok && statusErr.HTTPStatusCode() > 0 {
		parts = append(parts, fmt.Sprintf("http_status=%d", statusErr.HTTPStatusCode()))
	}

	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		parts = append(parts, "class=url_error")
		if urlErr.Timeout() {
			parts = append(parts, "timeout=true")
		}
		var dnsErr *net.DNSError
		if errors.As(urlErr.Err, &dnsErr) {
			parts = append(parts, "class=dns")
			parts = append(parts, "dns_name="+dnsErr.Name)
		}
		var opErr *net.OpError
		if errors.As(urlErr.Err, &opErr) {
			parts = append(parts, "class=net_op")
			parts = append(parts, "net_op="+opErr.Op)
			parts = append(parts, "net_addr="+opErr.Addr.String())
			if opErr.Timeout() {
				parts = append(parts, "timeout=true")
			}
			var dnsErr *net.DNSError
			if errors.As(opErr.Err, &dnsErr) {
				parts = append(parts, "class=dns")
				parts = append(parts, "dns_name="+dnsErr.Name)
			}
			var certErr x509.UnknownAuthorityError
			if errors.As(opErr.Err, &certErr) {
				parts = append(parts, "class=tls_unknown_authority")
			}
			var hostnameErr x509.HostnameError
			if errors.As(opErr.Err, &hostnameErr) {
				parts = append(parts, "class=tls_hostname")
				if hostnameErr.Host != "" {
					parts = append(parts, "tls_host="+hostnameErr.Host)
				}
			}
			if errors.Is(opErr.Err, syscall.ECONNREFUSED) {
				parts = append(parts, "class=conn_refused")
			}
			if errors.Is(opErr.Err, syscall.ENETUNREACH) || errors.Is(opErr.Err, syscall.EHOSTUNREACH) {
				parts = append(parts, "class=unreachable")
			}
		}
	}
	return strings.Join(parts, " | ")
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
