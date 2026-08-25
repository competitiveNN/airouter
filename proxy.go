package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Proxy struct {
	client *http.Client
	config *Config
}

func NewProxy(cfg *Config) *Proxy {
	return &Proxy{
		client: &http.Client{
			Timeout: 0,
			Transport: &http.Transport{
				MaxIdleConns:        200,
				MaxIdleConnsPerHost: 50,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		config: cfg,
	}
}

func (p *Proxy) buildRequest(ctx context.Context, body []byte, endpoint ModelEndpoint, providerCfg *ProviderConfig, stream bool) (*http.Request, error) {
	backendBody, err := ReplaceModelName(body, endpoint.Model)
	if err != nil {
		return nil, fmt.Errorf("replace model name: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", providerCfg.URL+"/chat/completions", bytes.NewReader(backendBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	if providerCfg.APIKey() != "" {
		req.Header.Set("Authorization", "Bearer "+providerCfg.APIKey())
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	return req, nil
}

func (p *Proxy) Forward(ctx context.Context, body []byte, endpoint ModelEndpoint) (*http.Response, error) {
	providerCfg, ok := p.config.Providers[endpoint.Provider]
	if !ok {
		return nil, fmt.Errorf("unknown provider: %s", endpoint.Provider)
	}

	req, err := p.buildRequest(ctx, body, endpoint, &providerCfg, false)
	if err != nil {
		return nil, err
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, &ProviderError{Err: err}
	}
	return resp, nil
}

func (p *Proxy) StreamToClient(ctx context.Context, w io.Writer, flusher http.Flusher, body []byte, endpoint ModelEndpoint) error {
	providerCfg, ok := p.config.Providers[endpoint.Provider]
	if !ok {
		return fmt.Errorf("unknown provider: %s", endpoint.Provider)
	}

	req, err := p.buildRequest(ctx, body, endpoint, &providerCfg, true)
	if err != nil {
		return err
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return &ProviderError{Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return &ProviderError{StatusCode: resp.StatusCode, Body: bodyBytes}
	}

	return p.streamSSE(w, flusher, resp.Body)
}

func (p *Proxy) streamSSE(w io.Writer, flusher http.Flusher, body io.Reader) error {
	var eventBuf bytes.Buffer
	scanner := bufio.NewScanner(body)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()

		if len(line) == 0 {
			if eventBuf.Len() > 0 {
				if err := extractSSEErrorFromBuffer(&eventBuf); err != nil {
					return err
				}
				w.Write(eventBuf.Bytes())
				w.Write([]byte("\n\n"))
				flusher.Flush()
				eventBuf.Reset()
			}
			continue
		}

		eventBuf.Write(line)
		eventBuf.WriteByte('\n')
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	if eventBuf.Len() > 0 {
		if err := extractSSEErrorFromBuffer(&eventBuf); err != nil {
			return err
		}
		w.Write(eventBuf.Bytes())
		w.Write([]byte("\n\n"))
		flusher.Flush()
	}

	return nil
}

func extractSSEErrorFromBuffer(buf *bytes.Buffer) error {
	if buf.Len() == 0 {
		return nil
	}
	return ExtractSSEError(buf.Bytes())
}