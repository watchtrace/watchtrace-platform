package workqueue

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/watchtrace/watchtrace-platform/internal/envelope"
	"net/http"
	"strings"
	"time"
)

type HTTPS struct {
	BaseURL, PoolToken string
	Client             *http.Client
}
type httpPull struct {
	Body         string              `json:"body"`
	Attributes   envelope.Attributes `json:"attributes"`
	LeaseToken   string              `json:"lease_token"`
	ReceiveCount int                 `json:"receive_count"`
}

func (h *HTTPS) Pull(ctx context.Context, _ time.Duration) (Delivery, error) {
	var out httpPull
	status, err := h.call(ctx, "/v1/jobs/pull", nil, &out)
	if err != nil {
		return Delivery{}, err
	}
	if status == 204 {
		return Delivery{}, ErrNoMessage
	}
	body, err := base64.StdEncoding.DecodeString(out.Body)
	if err != nil {
		return Delivery{}, err
	}
	return Delivery{Body: body, Attributes: envelopeAttrs(out), LeaseToken: out.LeaseToken, ReceiveCount: out.ReceiveCount}, nil
}
func (h *HTTPS) Extend(ctx context.Context, d Delivery, v time.Duration) error {
	_, err := h.call(ctx, "/v1/jobs/extend", map[string]any{"lease_token": d.LeaseToken, "seconds": int(v / time.Second)}, nil)
	return err
}
func (h *HTTPS) PublishResultAndAcknowledge(ctx context.Context, d Delivery, result []byte) error {
	_, err := h.call(ctx, "/v1/jobs/result", map[string]any{"lease_token": d.LeaseToken, "body": base64.StdEncoding.EncodeToString(result)}, nil)
	return err
}
func (h *HTTPS) AcknowledgeExpired(ctx context.Context, d Delivery, ack []byte) error {
	_, err := h.call(ctx, "/v1/jobs/expired", map[string]any{"lease_token": d.LeaseToken, "body": base64.StdEncoding.EncodeToString(ack)}, nil)
	return err
}
func (h *HTTPS) call(ctx context.Context, path string, input any, output any) (int, error) {
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	var body bytes.Buffer
	if input != nil {
		_ = json.NewEncoder(&body).Encode(input)
	}
	request, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(h.BaseURL, "/")+path, &body)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	if h.PoolToken != "" {
		request.Header.Set("Authorization", "Bearer "+h.PoolToken)
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode == 204 {
		return 204, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response.StatusCode, fmt.Errorf("gateway status %d", response.StatusCode)
	}
	if output != nil {
		if err = json.NewDecoder(response.Body).Decode(output); err != nil {
			return response.StatusCode, err
		}
	}
	return response.StatusCode, nil
}
func envelopeAttrs(p httpPull) envelope.Attributes {
	return p.Attributes
}
