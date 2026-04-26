package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"time"
)

type RetryConfig struct {
	MaxRetries int
	BaseDelay  time.Duration
	MaxDelay   time.Duration
}

type PaymentClient struct {
	httpClient *http.Client
	config     RetryConfig
}

func NewPaymentClient(cfg RetryConfig) *PaymentClient {
	return &PaymentClient{
		httpClient: &http.Client{Timeout: 5 * time.Second},
		config:     cfg,
	}
}

func IsRetryable(resp *http.Response, err error) bool {
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) {
			return netErr.Timeout()
		}
		return true
	}
	switch resp.StatusCode {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}

func (c *PaymentClient) CalculateBackoff(attempt int) time.Duration {
	backoff := c.config.BaseDelay * time.Duration(math.Pow(2, float64(attempt)))
	if backoff > c.config.MaxDelay {
		backoff = c.config.MaxDelay
	}
	if backoff <= 0 {
		return c.config.BaseDelay
	}
	return time.Duration(rand.Int63n(int64(backoff)))
}

func (c *PaymentClient) ExecutePayment(ctx context.Context, url string) (*http.Response, error) {
	var lastResp *http.Response
	var lastErr error

	for attempt := 0; attempt < c.config.MaxRetries; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
		if err != nil {
			return nil, err
		}

		resp, err := c.httpClient.Do(req)

		if !IsRetryable(resp, err) {
			fmt.Printf("Attempt %d: Success!\n", attempt+1)
			return resp, err
		}

		lastResp = resp
		lastErr = err

		if attempt == c.config.MaxRetries-1 {
			fmt.Printf("Attempt %d failed: max retries reached\n", attempt+1)
			break
		}

		waitTime := c.CalculateBackoff(attempt)
		fmt.Printf("Attempt %d failed: waiting %v...\n", attempt+1, waitTime.Round(time.Millisecond))

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(waitTime):
		}
	}

	return lastResp, lastErr
}

func main() {
	var requestCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&requestCount, 1)
		if count <= 3 {
			fmt.Printf("[Server] Request %d -> 503 Service Unavailable\n", count)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Printf("[Server] Request %d -> 200 OK\n", count)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "success"})
	}))
	defer server.Close()

	cfg := RetryConfig{
		MaxRetries: 5,
		BaseDelay:  500 * time.Millisecond,
		MaxDelay:   10 * time.Second,
	}

	client := NewPaymentClient(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fmt.Println("=== Payment Execution Started ===")
	resp, err := client.ExecutePayment(ctx, server.URL)
	if err != nil {
		fmt.Printf("Payment failed: %v\n", err)
		return
	}
	defer resp.Body.Close()

	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)
	fmt.Printf("Payment result: %v\n", result)
}
