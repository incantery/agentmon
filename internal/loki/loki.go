// Package loki is a minimal client for Grafana Loki's HTTP push API.
// Events are pushed as JSON streams; the caller (internal/drain) owns
// batching, retries, and segment lifecycle.
package loki

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// PermanentError is a push Loki rejected for reasons a retry cannot fix
// (4xx other than 429): malformed batch, limits, out-of-order policy.
type PermanentError struct {
	Status int
	Body   string
}

func (e *PermanentError) Error() string {
	return fmt.Sprintf("loki: permanent rejection %d: %s", e.Status, e.Body)
}

type Entry struct {
	TS   time.Time
	Line []byte
}

type Stream struct {
	Labels  map[string]string
	Entries []Entry
}

type Client struct {
	url    string
	tenant string
	hc     *http.Client
}

func New(baseURL, tenant string) *Client {
	return &Client{
		url:    strings.TrimSuffix(baseURL, "/") + "/loki/api/v1/push",
		tenant: tenant,
		hc:     &http.Client{Timeout: 30 * time.Second},
	}
}

type pushStream struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}

type pushBody struct {
	Streams []pushStream `json:"streams"`
}

// Push sends the streams in one request. nil on 2xx. A *PermanentError
// means the batch will never be accepted; any other error is retryable.
func (c *Client) Push(streams []Stream) error {
	var body pushBody
	for _, s := range streams {
		ps := pushStream{Stream: s.Labels}
		for _, e := range s.Entries {
			ps.Values = append(ps.Values, [2]string{
				strconv.FormatInt(e.TS.UnixNano(), 10),
				string(e.Line),
			})
		}
		body.Streams = append(body.Streams, ps)
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.tenant != "" {
		req.Header.Set("X-Scope-OrgID", c.tenant)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err // network: retryable
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(b))
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
		return &PermanentError{Status: resp.StatusCode, Body: msg}
	}
	return fmt.Errorf("loki: push failed %d: %s", resp.StatusCode, msg)
}
