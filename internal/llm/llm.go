// Package llm is a minimal client for the Claude Messages API
// (POST /v1/messages) using only the standard library.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Defaults. The model is configurable with RAGLOOP_MODEL because model
// names change faster than code.
const (
	DefaultBaseURL = "https://api.anthropic.com"
	DefaultVersion = "2023-06-01"
	DefaultModel   = "claude-haiku-5-5"
	maxBody        = 8 << 20
)

// Request is one single-turn completion.
type Request struct {
	System    string
	Prompt    string
	MaxTokens int
}

// Model completes a single-turn prompt.
type Model interface {
	Complete(ctx context.Context, req Request) (string, error)
}

// Anthropic calls the Claude Messages API.
type Anthropic struct {
	APIKey     string
	Model      string
	BaseURL    string
	Version    string
	HTTP       *http.Client
	MaxRetries int           // retries on 429, 529 and 5xx
	Backoff    time.Duration // first retry delay; doubles each time
}

// FromEnv builds a client from ANTHROPIC_API_KEY, with optional
// RAGLOOP_MODEL and ANTHROPIC_BASE_URL. It reports false when no key is set.
func FromEnv() (*Anthropic, bool) {
	key := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
	if key == "" {
		return nil, false
	}
	return &Anthropic{
		APIKey:     key,
		Model:      envOr("RAGLOOP_MODEL", DefaultModel),
		BaseURL:    envOr("ANTHROPIC_BASE_URL", DefaultBaseURL),
		Version:    DefaultVersion,
		HTTP:       &http.Client{Timeout: 60 * time.Second},
		MaxRetries: 2,
		Backoff:    time.Second,
	}, true
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

// StatusError is a non-2xx API response.
type StatusError struct {
	Status     int
	Type       string
	Message    string
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("claude api: HTTP %d %s: %s", e.Status, e.Type, e.Message)
}

type apiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type apiRequest struct {
	Model     string       `json:"model"`
	MaxTokens int          `json:"max_tokens"`
	System    string       `json:"system,omitempty"`
	Messages  []apiMessage `json:"messages"`
}

type apiResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

type apiError struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Complete implements Model.
func (a *Anthropic) Complete(ctx context.Context, req Request) (string, error) {
	if req.MaxTokens <= 0 {
		req.MaxTokens = 1024
	}
	body, err := json.Marshal(apiRequest{
		Model: a.Model, MaxTokens: req.MaxTokens, System: req.System,
		Messages: []apiMessage{{Role: "user", Content: req.Prompt}},
	})
	if err != nil {
		return "", err
	}
	url := strings.TrimRight(a.BaseURL, "/") + "/v1/messages"
	var last error
	for attempt := 0; attempt <= a.MaxRetries; attempt++ {
		if attempt > 0 {
			wait := a.Backoff << (attempt - 1)
			var se *StatusError
			if errors.As(last, &se) && se.RetryAfter > 0 {
				wait = min(se.RetryAfter, 30*time.Second)
			}
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return "", ctx.Err()
			case <-timer.C:
			}
		}
		text, retry, err := a.do(ctx, url, body)
		if err == nil {
			return text, nil
		}
		last = err
		if !retry {
			break
		}
	}
	return "", last
}

func (a *Anthropic) do(ctx context.Context, url string, body []byte) (string, bool, error) {
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", false, err
	}
	hr.Header.Set("x-api-key", a.APIKey)
	hr.Header.Set("anthropic-version", a.Version)
	hr.Header.Set("content-type", "application/json")
	client := a.HTTP
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := client.Do(hr)
	if err != nil {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		return "", true, fmt.Errorf("claude api request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", true, fmt.Errorf("claude api read: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		se := &StatusError{Status: resp.StatusCode}
		var ae apiError
		if json.Unmarshal(data, &ae) == nil && ae.Error.Message != "" {
			se.Type, se.Message = ae.Error.Type, ae.Error.Message
		} else {
			se.Message = truncate(strings.TrimSpace(string(data)), 200)
		}
		if s, err := strconv.Atoi(resp.Header.Get("retry-after")); err == nil && s > 0 {
			se.RetryAfter = time.Duration(s) * time.Second
		}
		retry := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return "", retry, se
	}
	var ar apiResponse
	if err := json.Unmarshal(data, &ar); err != nil {
		return "", false, fmt.Errorf("claude api decode: %w", err)
	}
	var b strings.Builder
	for _, c := range ar.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	if b.Len() == 0 {
		return "", false, errors.New("claude api: response had no text content")
	}
	return b.String(), false, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
