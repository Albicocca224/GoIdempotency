package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"
)

type ResponseStatus string

const (
	StatusProcessing ResponseStatus = "processing"
	StatusCompleted  ResponseStatus = "completed"
)

type CachedResponse struct {
	Status     ResponseStatus
	StatusCode int
	Body       []byte
}

type MemoryStore struct {
	mu   sync.Mutex
	data map[string]*CachedResponse
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{data: make(map[string]*CachedResponse)}
}

func (m *MemoryStore) Get(key string) (*CachedResponse, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	resp, exists := m.data[key]
	return resp, exists
}

func (m *MemoryStore) StartProcessing(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.data[key]; exists {
		return false
	}
	m.data[key] = &CachedResponse{Status: StatusProcessing}
	return true
}

func (m *MemoryStore) Finish(key string, statusCode int, body []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if resp, exists := m.data[key]; exists {
		resp.Status = StatusCompleted
		resp.StatusCode = statusCode
		resp.Body = body
	}
}

func IdempotencyMiddleware(store *MemoryStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":"Idempotency-Key header required"}`, http.StatusBadRequest)
			return
		}

		shortKey := key
		if len(key) > 8 {
			shortKey = key[:8]
		}

		if cached, exists := store.Get(key); exists {
			if cached.Status == StatusCompleted {
				fmt.Printf("[Middleware] Key %s...: returning cached result (status %d)\n", shortKey, cached.StatusCode)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Idempotent-Replayed", "true")
				w.WriteHeader(cached.StatusCode)
				w.Write(cached.Body)
				return
			}
			fmt.Printf("[Middleware] Key %s...: request in progress -> 409 Conflict\n", shortKey)
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":"Duplicate request in progress"}`, http.StatusConflict)
			return
		}

		if !store.StartProcessing(key) {
			if cached, exists := store.Get(key); exists && cached.Status == StatusCompleted {
				fmt.Printf("[Middleware] Key %s...: returning cached result (race won)\n", shortKey)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Idempotent-Replayed", "true")
				w.WriteHeader(cached.StatusCode)
				w.Write(cached.Body)
				return
			}
			fmt.Printf("[Middleware] Key %s...: lost race -> 409 Conflict\n", shortKey)
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":"Duplicate request in progress"}`, http.StatusConflict)
			return
		}

		fmt.Printf("[Middleware] Key %s...: processing started\n", shortKey)
		recorder := httptest.NewRecorder()
		next.ServeHTTP(recorder, r)

		store.Finish(key, recorder.Code, recorder.Body.Bytes())
		fmt.Printf("[Middleware] Key %s...: processing finished, result cached\n", shortKey)

		for k, vals := range recorder.Header() {
			for _, v := range vals {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(recorder.Code)
		w.Write(recorder.Body.Bytes())
	})
}

func generateTransactionID() string {
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		rand.Int31(),
		rand.Int31n(0xffff),
		rand.Int31n(0xffff),
		rand.Int31n(0xffff),
		rand.Int63n(0xffffffffffff),
	)
}

func paymentHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Println("[Handler] Heavy operation started (2s simulation)...")
	time.Sleep(2 * time.Second)

	result := map[string]interface{}{
		"status":         "paid",
		"amount":         1000,
		"transaction_id": generateTransactionID(),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(result)
	fmt.Println("[Handler] Payment completed")
}

func main() {
	rand.New(rand.NewSource(time.Now().UnixNano()))

	store := NewMemoryStore()
	mux := http.NewServeMux()
	mux.Handle("/pay", IdempotencyMiddleware(store, http.HandlerFunc(paymentHandler)))

	server := httptest.NewServer(mux)
	defer server.Close()

	idempotencyKey := generateTransactionID()
	fmt.Printf("=== Simulating Double-Click Attack ===\n")
	fmt.Printf("Idempotency Key: %s\n\n", idempotencyKey)

	var wg sync.WaitGroup
	numRequests := 7

	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func(id int, delay time.Duration) {
			defer wg.Done()
			time.Sleep(delay)

			req, _ := http.NewRequest(http.MethodPost, server.URL+"/pay", nil)
			req.Header.Set("Idempotency-Key", idempotencyKey)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				fmt.Printf("[Goroutine %d] Error: %v\n", id, err)
				return
			}
			defer resp.Body.Close()

			var result map[string]interface{}
			json.NewDecoder(resp.Body).Decode(&result)
			fmt.Printf("[Goroutine %d] Status: %d | Body: %v\n", id, resp.StatusCode, result)
		}(i+1, time.Duration(i)*30*time.Millisecond)
	}

	wg.Wait()

	fmt.Println("\n=== Sending request after completion ===")
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/pay", nil)
	req.Header.Set("Idempotency-Key", idempotencyKey)
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()

	var final map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&final)
	replayed := resp.Header.Get("X-Idempotent-Replayed")
	fmt.Printf("Late request -> Status: %d | Replayed: %s | Body: %v\n", resp.StatusCode, replayed, final)

	fmt.Println("\n=== Simulation complete ===")
}
