package gateway

// The transmission client: one batch, one authenticated POST of the v1.1
// envelope. No retry logic here - retrying is the spool's job, and the
// server's batch idempotency makes every retry safe.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// SendResult is what the platform said about one batch.
type SendResult struct {
	Stored         int
	Rejected       int
	DuplicateBatch bool
	ClockDriftSec  float64
}

// Client posts batches for one plan.
type Client struct {
	API    string
	PlanID string
	Key    string
	Label  string
	HTTP   *http.Client
}

func NewClient(api, planID, key, label string) *Client {
	return &Client{API: api, PlanID: planID, Key: key, Label: label,
		HTTP: NewAPIHTTPClient(30 * time.Second)}
}

// APITransport carries every platform API call (nil: the default); tests
// point it at a TLS test server.
var APITransport http.RoundTripper

// NewAPIHTTPClient is the client for every call to the platform. It follows
// no redirect: the key (or an enrollment code) goes to the configured URL and
// nowhere else, never on to plain http or another host. A redirect is an
// error, so a batch stays spooled.
func NewAPIHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &http.Client{Timeout: timeout, Transport: APITransport,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			return fmt.Errorf("the platform redirected to %s; redirects are not followed", req.URL.Redacted())
		}}
}

// Send transmits one spooled batch. A non-nil error means "keep it spooled
// and try again later"; HTTP 4xx contract violations are returned as errors
// too, because silently dropping data is never acceptable.
func (c *Client) Send(b Batch) (SendResult, error) {
	envelope := map[string]any{
		"schemaVersion": "1.1",
		"batchId":       b.ID,
		"sentAt":        time.Now().UTC().Format(time.RFC3339),
		"dataMode":      b.DataMode,
		"gateway":       map[string]any{"label": c.Label},
		"readings":      b.Readings,
	}
	if len(b.Events) > 0 {
		envelope["events"] = b.Events
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return SendResult{}, err
	}
	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("%s/api/pro/plans/%s/telemetry", c.API, c.PlanID), bytes.NewReader(body))
	if err != nil {
		return SendResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Key != "" {
		req.Header.Set("X-Api-Key", c.Key)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return SendResult{}, err // network trouble: the spool keeps the batch
	}
	defer resp.Body.Close()
	var receipt struct {
		Stored            int     `json:"stored"`
		DuplicateBatch    bool    `json:"duplicateBatch"`
		ClockDriftSeconds float64 `json:"clockDriftSeconds"`
		Rejected          []struct {
			SensorID string `json:"sensorId"`
			Reason   string `json:"reason"`
		} `json:"rejected"`
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&receipt)
	if resp.StatusCode == http.StatusTooManyRequests {
		return SendResult{}, fmt.Errorf("rate limited; retrying later")
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return SendResult{}, fmt.Errorf("%w: HTTP %d: %s", ErrUnauthorized, resp.StatusCode, receipt.Error)
	}
	if resp.StatusCode >= 400 {
		return SendResult{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, receipt.Error)
	}
	return SendResult{
		Stored:         receipt.Stored,
		Rejected:       len(receipt.Rejected),
		DuplicateBatch: receipt.DuplicateBatch,
		ClockDriftSec:  receipt.ClockDriftSeconds,
	}, nil
}
