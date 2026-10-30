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
	"regexp"
	"strings"
	"time"
)

// DefaultLocalURL is where Ollama serves its OpenAI-compatible API.
const DefaultLocalURL = "http://localhost:11434/v1"

// Local calls a model running on this machine through the OpenAI-compatible
// chat completions API (POST /v1/chat/completions), which Ollama, LM Studio
// and llama.cpp's server all provide. It needs no key and costs nothing.
type Local struct {
	BaseURL string // up to and including /v1
	Model   string
	HTTP    *http.Client
}

// LocalFromEnv builds a client for model, or for PLUMBLINE_LOCAL_MODEL when
// model is empty, at PLUMBLINE_LOCAL_URL or DefaultLocalURL. It reports
// false when no model is named.
func LocalFromEnv(model string) (*Local, bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		model = strings.TrimSpace(os.Getenv("PLUMBLINE_LOCAL_MODEL"))
	}
	if model == "" {
		return nil, false
	}
	return &Local{
		BaseURL: strings.TrimRight(envOr("PLUMBLINE_LOCAL_URL", DefaultLocalURL), "/"),
		Model:   model,
		// A laptop can take a while, and loading the model makes the first call slower still.
		HTTP: &http.Client{Timeout: 3 * time.Minute},
	}, true
}

type chatRequest struct {
	Model     string       `json:"model"`
	Messages  []apiMessage `json:"messages"`
	MaxTokens int          `json:"max_tokens"`
	Stream    bool         `json:"stream"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

var thinkRe = regexp.MustCompile(`(?s)<think>.*?</think>`)

// Complete implements Model. It does not retry: a local server that fails
// once usually fails again, and every caller has an offline fallback.
func (l *Local) Complete(ctx context.Context, req Request) (string, error) {
	if req.MaxTokens <= 0 {
		req.MaxTokens = 1024
	}
	var msgs []apiMessage
	if req.System != "" {
		msgs = append(msgs, apiMessage{Role: "system", Content: req.System})
	}
	msgs = append(msgs, apiMessage{Role: "user", Content: req.Prompt})
	body, err := json.Marshal(chatRequest{Model: l.Model, Messages: msgs, MaxTokens: req.MaxTokens})
	if err != nil {
		return "", err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, l.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	hr.Header.Set("content-type", "application/json")
	data, status, err := l.do(hr)
	if err != nil {
		return "", err
	}
	if status/100 != 2 {
		se := &StatusError{Status: status}
		var ae apiError
		if json.Unmarshal(data, &ae) == nil && ae.Error.Message != "" {
			se.Type, se.Message = ae.Error.Type, ae.Error.Message
		} else {
			se.Message = truncate(strings.TrimSpace(string(data)), 200)
		}
		return "", se
	}
	var cr chatResponse
	if err := json.Unmarshal(data, &cr); err != nil {
		return "", fmt.Errorf("local model decode: %w", err)
	}
	if len(cr.Choices) == 0 {
		return "", errors.New("local model: response had no choices")
	}
	// Reasoning models think inline first; drop that, including a block
	// cut off by the token limit, so callers see only the answer.
	out := thinkRe.ReplaceAllString(cr.Choices[0].Message.Content, "")
	if i := strings.Index(out, "<think>"); i >= 0 {
		out = out[:i]
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", errors.New("local model: response had no answer text")
	}
	return out, nil
}

// Check confirms the server is up and has the model downloaded, so a
// missing setup step is reported once instead of on every call.
func (l *Local) Check(ctx context.Context) error {
	hr, err := http.NewRequestWithContext(ctx, http.MethodGet, l.BaseURL+"/models", nil)
	if err != nil {
		return err
	}
	data, status, err := l.do(hr)
	if err != nil {
		return fmt.Errorf("no local model server at %s (start it with: ollama serve): %w", l.BaseURL, err)
	}
	if status/100 != 2 {
		return fmt.Errorf("local model server at %s: HTTP %d", l.BaseURL, status)
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("local model server at %s: listing models: %w", l.BaseURL, err)
	}
	for _, m := range list.Data {
		if m.ID == l.Model || m.ID == l.Model+":latest" {
			return nil
		}
	}
	return fmt.Errorf("model %q is not downloaded (run: ollama pull %s)", l.Model, l.Model)
}

func (l *Local) do(hr *http.Request) ([]byte, int, error) {
	client := l.HTTP
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Minute}
	}
	resp, err := client.Do(hr)
	if err != nil {
		if err := hr.Context().Err(); err != nil {
			return nil, 0, err
		}
		return nil, 0, fmt.Errorf("local model request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, 0, fmt.Errorf("local model read: %w", err)
	}
	return data, resp.StatusCode, nil
}
