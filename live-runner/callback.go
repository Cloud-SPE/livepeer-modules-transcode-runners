package liverunner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"time"
)

var callbackErrorCodePatternV1 = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

type CallbackDeliveryErrorV1 struct {
	StatusCode int
	Code       string
	Retryable  bool
}

func (e *CallbackDeliveryErrorV1) Error() string {
	if e.StatusCode == 0 {
		return "runner event callback transport failed"
	}
	return fmt.Sprintf("runner event callback returned HTTP %d", e.StatusCode)
}

type CallbackDispatcherV1 struct {
	store          *EncryptedFileSessionStoreV1
	client         *http.Client
	now            func() time.Time
	retryInitial   time.Duration
	retryMaximum   time.Duration
	log            *slog.Logger
	rejectionCount func(statusCode int, errorCode string)
}

type CallbackWorkerV1 struct {
	store      *EncryptedFileSessionStoreV1
	dispatcher *CallbackDispatcherV1
	interval   time.Duration
}

func NewCallbackDispatcherV1(store *EncryptedFileSessionStoreV1, transport http.RoundTripper, timeout time.Duration) (*CallbackDispatcherV1, error) {
	if store == nil || timeout <= 0 {
		return nil, errors.New("session store and positive callback timeout are required")
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &CallbackDispatcherV1{
		store: store, client: client, now: time.Now,
		retryInitial: 500 * time.Millisecond, retryMaximum: 30 * time.Second,
		log: slog.Default(),
	}, nil
}

func NewCallbackWorkerV1(store *EncryptedFileSessionStoreV1, dispatcher *CallbackDispatcherV1, interval time.Duration) (*CallbackWorkerV1, error) {
	if store == nil || dispatcher == nil || dispatcher.store != store || interval <= 0 {
		return nil, errors.New("callback worker dependencies are invalid")
	}
	return &CallbackWorkerV1{store: store, dispatcher: dispatcher, interval: interval}, nil
}

func (w *CallbackWorkerV1) Run(ctx context.Context) error {
	for {
		if err := w.SweepOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		timer := time.NewTimer(w.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

// SweepOnce gives every recoverable session one bounded opportunity to drain
// the outbox snapshot observed at the start of the sweep. Retryable failures
// preserve the head; permanent responses atomically park it and allow later
// events through. Terminal secrets are erased only after every pending event
// is acknowledged or parked, including after a crash at either boundary.
func (w *CallbackWorkerV1) SweepOnce(ctx context.Context) error {
	records, err := w.store.Recoverable()
	if err != nil {
		return err
	}
	for _, record := range records {
		for range len(record.PendingEvents) {
			if ctx.Err() != nil {
				return nil
			}
			delivered, err := w.dispatcher.DeliverNext(ctx, record.BrokerSessionID)
			if err != nil {
				var deliveryError *CallbackDeliveryErrorV1
				if errors.As(err, &deliveryError) {
					if deliveryError.Retryable {
						break
					}
					// A permanent response has already been durably parked, so
					// continue with the next event from the original snapshot.
					continue
				}
				return err
			}
			if !delivered {
				break
			}
		}
		current, secrets, err := w.store.Load(record.BrokerSessionID)
		if err != nil {
			return err
		}
		if current.State != "active" && len(current.PendingEvents) == 0 && secrets != nil {
			if err := w.store.ClearTerminalSecrets(record.BrokerSessionID); err != nil {
				return err
			}
		}
	}
	return nil
}

// DeliverNext posts at most one durable event. A successful response
// acknowledges the head; a permanent response parks it; a retryable response
// leaves it in place. Every path preserves strict per-session ordering.
func (d *CallbackDispatcherV1) DeliverNext(ctx context.Context, brokerSessionID string) (bool, error) {
	record, secrets, err := d.store.Load(brokerSessionID)
	if err != nil {
		return false, err
	}
	if len(record.PendingEvents) == 0 {
		return false, nil
	}
	if secrets == nil {
		return false, errors.New("pending runner event has no callback credentials")
	}
	event := record.PendingEvents[0]
	now := d.now().UTC()
	if record.CallbackDelivery != nil {
		nextAttempt, err := time.Parse(time.RFC3339Nano, record.CallbackDelivery.NextAttemptAt)
		if err != nil {
			return false, errors.New("callback retry time is invalid")
		}
		if now.Before(nextAttempt) {
			return false, nil
		}
	}
	if _, err := d.store.ReserveCallbackAttempt(brokerSessionID, event.EventID, now, d.retryInitial, d.retryMaximum); err != nil {
		if errors.Is(err, ErrCallbackNotDueV1) {
			return false, nil
		}
		return false, err
	}
	body, err := json.Marshal(event)
	if err != nil {
		return false, errors.New("runner event serialization failed")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, secrets.CreateRequest.CallbackURL, bytes.NewReader(body))
	if err != nil {
		return false, errors.New("runner event callback request failed")
	}
	request.Header.Set("Authorization", "Bearer "+secrets.CreateRequest.CallbackToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "livepeer-modules-transcode-live-runner/1")
	response, err := d.client.Do(request)
	if err != nil {
		if storeErr := d.store.RecordCallbackRetryFailure(brokerSessionID, event.EventID, 0, "transport_error"); storeErr != nil {
			return false, storeErr
		}
		return false, &CallbackDeliveryErrorV1{Code: "transport_error", Retryable: true}
	}
	defer response.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		retryable := callbackStatusRetryableV1(response.StatusCode)
		code := safeCallbackErrorCodeV1(response.StatusCode, responseBody)
		if retryable {
			if err := d.store.RecordCallbackRetryFailure(brokerSessionID, event.EventID, response.StatusCode, code); err != nil {
				return false, err
			}
			return false, &CallbackDeliveryErrorV1{StatusCode: response.StatusCode, Code: code, Retryable: true}
		}
		if err := d.store.ParkCallbackRejection(brokerSessionID, event.EventID, response.StatusCode, code, now); err != nil {
			return false, err
		}
		d.log.Warn("live callback event permanently rejected", "runner_session_id", record.RunnerSessionID, "event_id", event.EventID, "status", response.StatusCode, "code", code)
		if d.rejectionCount != nil {
			d.rejectionCount(response.StatusCode, code)
		}
		return false, &CallbackDeliveryErrorV1{StatusCode: response.StatusCode, Code: code, Retryable: false}
	}
	if err := d.store.AcknowledgeEvent(brokerSessionID, event.EventID); err != nil {
		return false, err
	}
	return true, nil
}

func callbackStatusRetryableV1(statusCode int) bool {
	return statusCode == http.StatusRequestTimeout || statusCode == http.StatusTooManyRequests || statusCode >= 500 && statusCode <= 599
}

func callbackRetryDelayV1(eventID string, attempt uint32, initial, maximum time.Duration) time.Duration {
	base := initial
	for index := uint32(1); index < attempt && base < maximum; index++ {
		if base > maximum/2 {
			base = maximum
			break
		}
		base *= 2
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", eventID, attempt)))
	value := uint64(0)
	for _, item := range digest[:8] {
		value = value<<8 | uint64(item)
	}
	// Stable jitter in [75%, 125%] makes the persisted schedule reproducible
	// across restarts while desynchronizing sessions.
	factor := time.Duration(750 + value%501)
	if factor == 1000 {
		return base
	}
	if factor < 1000 {
		delta := 1000 - factor
		return base - (base/1000)*delta - (base%1000)*delta/1000
	}
	delta := factor - 1000
	extra := (base/1000)*delta + (base%1000)*delta/1000
	if extra > maximum-base {
		return maximum
	}
	return base + extra
}

func safeCallbackErrorCodeV1(statusCode int, body []byte) string {
	switch statusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "callback_auth_rejected"
	case http.StatusNotFound, http.StatusGone:
		return "broker_session_terminal"
	}
	var response struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if json.Unmarshal(body, &response) == nil {
		for _, code := range []string{response.Error, response.Code} {
			if safeBrokerCallbackCodeV1(code) {
				return code
			}
		}
	}
	return fmt.Sprintf("http_%d", statusCode)
}

func safeBrokerCallbackCodeV1(code string) bool {
	if !callbackErrorCodePatternV1.MatchString(code) {
		return false
	}
	switch code {
	case "duplicate_event", "event_not_supported", "invalid_event", "sequence_conflict", "session_terminal":
		return true
	default:
		return false
	}
}

func validCallbackFailureV1(statusCode int, errorCode string, allowTransport bool) bool {
	if statusCode == 0 {
		return allowTransport && (errorCode == "" || errorCode == "transport_error")
	}
	return statusCode >= 100 && statusCode <= 599 && callbackErrorCodePatternV1.MatchString(errorCode)
}
