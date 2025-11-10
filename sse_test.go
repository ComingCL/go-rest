package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// MockEventHandler implements EventHandler interface for testing
type MockEventHandler struct {
	events       []*Event
	errors       []error
	connected    bool
	disconnected bool
}

func (m *MockEventHandler) OnEvent(event *Event) {
	m.events = append(m.events, event)
}

func (m *MockEventHandler) OnError(err error) {
	m.errors = append(m.errors, err)
}

func (m *MockEventHandler) OnConnect() {
	m.connected = true
}

func (m *MockEventHandler) OnDisconnect() {
	m.disconnected = true
}

// createSSETestServer creates a test server that sends SSE events
func createSSETestServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check headers
		if r.Header.Get("Accept") != "text/event-stream" {
			http.Error(w, "Expected text/event-stream", http.StatusBadRequest)
			return
		}

		// Set SSE headers
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
			return
		}

		// Send test events
		events := []string{
			"data: Hello World\n\n",
			"event: custom\ndata: Custom Event\nid: 1\n\n",
			"data: {\"message\": \"JSON data\"}\nid: 2\n\n",
			"event: close\ndata: Connection closing\n\n",
		}

		for _, event := range events {
			fmt.Fprint(w, event)
			flusher.Flush()
			time.Sleep(100 * time.Millisecond)
		}
	}))
}

func TestEvent_ParseEventData(t *testing.T) {
	event := &Event{
		Raw: []byte(`{"message": "test", "value": 42}`),
	}

	var result struct {
		Message string `json:"message"`
		Value   int    `json:"value"`
	}

	err := event.ParseEventData(&result)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	if result.Message != "test" {
		t.Errorf("Expected message 'test', got '%s'", result.Message)
	}

	if result.Value != 42 {
		t.Errorf("Expected value 42, got %d", result.Value)
	}
}

func TestEvent_String(t *testing.T) {
	event := &Event{
		Type:  EventTypeData,
		ID:    "123",
		Raw:   []byte("Hello World"),
		Retry: 5000,
	}

	expected := "Event{Type: data, ID: 123, Data: Hello World, Retry: 5000}"
	result := event.String()

	if result != expected {
		t.Errorf("Expected '%s', got '%s'", expected, result)
	}
}

func TestDefaultSSEOptions(t *testing.T) {
	// Test that default values work correctly in Request
	serverURL, _ := url.Parse("http://example.com")
	client := NewRESTClient(serverURL, "", ClientContentConfig{}, nil)

	req := client.SSE()

	// Test default values are properly set
	if req.sseRetryInterval != 0 {
		t.Errorf("Expected default retry interval 0 (will use 3s default), got %v", req.sseRetryInterval)
	}

	if req.maxRetries != 10 { // Default from NewRequest
		t.Errorf("Expected default max retries 10, got %d", req.maxRetries)
	}

	if req.sseBufferSize != 0 {
		t.Errorf("Expected default buffer size 0 (will use 100 default), got %d", req.sseBufferSize)
	}
}

func TestStreamResult_ReadEvent(t *testing.T) {
	// Create test SSE data
	sseData := "event: test\ndata: Hello World\nid: 123\n\n"
	reader := strings.NewReader(sseData)

	// Create a mock response with the test data
	resp := &http.Response{
		Body: io.NopCloser(reader),
	}

	streamResult := &StreamResult{
		resp: resp,
	}

	event, err := streamResult.ReadEvent()
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	if event == nil {
		t.Fatal("Expected event, got nil")
	}

	if event.Type != "test" {
		t.Errorf("Expected event type 'test', got '%s'", event.Type)
	}

	if event.GetDataAsString() != "Hello World" {
		t.Errorf("Expected data 'Hello World', got '%s'", event.GetDataAsString())
	}

	if event.ID != "123" {
		t.Errorf("Expected ID '123', got '%s'", event.ID)
	}
}

func TestRESTClient_SSE(t *testing.T) {
	server := createSSETestServer()
	defer server.Close()

	serverURL, _ := url.Parse(server.URL)
	client := NewRESTClient(serverURL, "", ClientContentConfig{}, nil)

	request := client.SSE()
	if request == nil {
		t.Fatal("Expected request, got nil")
	}

	// Test that it creates a GET request
	if request.verb != "GET" {
		t.Errorf("Expected verb 'GET', got '%s'", request.verb)
	}
}

func TestRequest_Stream(t *testing.T) {
	server := createSSETestServer()
	defer server.Close()

	serverURL, _ := url.Parse(server.URL)
	client := NewRESTClient(serverURL, "", ClientContentConfig{}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	streamResult, err := client.SSE().Stream(ctx)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	defer streamResult.Close()

	if streamResult.Response().StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", streamResult.Response().StatusCode)
	}

	// Read first event
	event, err := streamResult.ReadEvent()
	if err != nil {
		t.Fatalf("Expected no error reading event, got %v", err)
	}

	if event == nil {
		t.Fatal("Expected event, got nil")
	}

	if event.GetDataAsString() != "Hello World" {
		t.Errorf("Expected data 'Hello World', got '%s'", event.GetDataAsString())
	}
}

func TestRequest_Watch(t *testing.T) {
	server := createSSETestServer()
	defer server.Close()

	serverURL, _ := url.Parse(server.URL)
	client := NewRESTClient(serverURL, "", ClientContentConfig{}, nil)

	handler := &MockEventHandler{}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	watcher, err := client.SSE().
		WithEventHandler(handler).
		WithRetryInterval(1 * time.Second).
		MaxRetries(3).
		WithBufferSize(10).
		SSEWatch(ctx)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	defer watcher.Stop()

	// Wait for events
	time.Sleep(500 * time.Millisecond)

	// Check that we received events through channels
	select {
	case event := <-watcher.Events():
		if event == nil {
			t.Error("Expected event, got nil")
		}
	case <-time.After(1 * time.Second):
		t.Error("Expected to receive event within 1 second")
	}

	// Check handler received events
	if len(handler.events) == 0 {
		t.Error("Expected handler to receive events")
	}

	if !handler.connected {
		t.Error("Expected handler to be notified of connection")
	}
}

func TestSSEWatcher_Stop(t *testing.T) {
	server := createSSETestServer()
	defer server.Close()

	serverURL, _ := url.Parse(server.URL)
	client := NewRESTClient(serverURL, "", ClientContentConfig{}, nil)

	handler := &MockEventHandler{}

	ctx := context.Background()
	watcher, err := client.SSE().
		WithEventHandler(handler).
		WithBufferSize(10).
		SSEWatch(ctx)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	// Wait a bit then stop
	time.Sleep(100 * time.Millisecond)
	watcher.Stop()

	if !watcher.IsStopped() {
		t.Error("Expected watcher to be stopped")
	}

	if !handler.disconnected {
		t.Error("Expected handler to be notified of disconnection")
	}
}

func TestSSEWatcher_Errors(t *testing.T) {
	// Create a server that returns an error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}))
	defer server.Close()

	serverURL, _ := url.Parse(server.URL)
	client := NewRESTClient(serverURL, "", ClientContentConfig{}, nil)

	handler := &MockEventHandler{}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	watcher, err := client.SSE().
		WithEventHandler(handler).
		WithRetryInterval(100 * time.Millisecond).
		MaxRetries(1). // Limit retries for test
		WithBufferSize(10).
		SSEWatch(ctx)
	if err != nil {
		t.Fatalf("Expected no error starting watcher, got %v", err)
	}
	defer watcher.Stop()

	// Wait for error
	select {
	case err := <-watcher.Errors():
		if err == nil {
			t.Error("Expected error, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Error("Expected to receive error within 2 seconds")
	}

	// Check handler received error
	if len(handler.errors) == 0 {
		t.Error("Expected handler to receive errors")
	}
}

// Benchmark tests
func BenchmarkEvent_ParseEventData(b *testing.B) {
	event := &Event{
		Raw: []byte(`{"message": "benchmark test", "value": 12345, "timestamp": "2024-11-10T13:00:00Z"}`),
	}

	var result struct {
		Message   string `json:"message"`
		Value     int    `json:"value"`
		Timestamp string `json:"timestamp"`
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = event.ParseEventData(&result)
	}
}

func BenchmarkStreamResult_ReadEvent(b *testing.B) {
	sseData := strings.Repeat("event: test\ndata: Hello World\nid: 123\n\n", 1000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		reader := strings.NewReader(sseData)
		resp := &http.Response{
			Body: io.NopCloser(reader),
		}
		streamResult := &StreamResult{
			resp: resp,
		}
		b.StartTimer()

		for {
			_, err := streamResult.ReadEvent()
			if err != nil {
				break
			}
		}
	}
}

type SSEBody struct {
	TraceID string `json:"traceId"`
	ReqID   string `json:"reqId"`
	Erp     string `json:"erp"`
	Keyword string `json:"keyword"`
}

// TestTypedEventPerformance tests the performance of typed events vs traditional parsing
func TestTypedEventPerformance(t *testing.T) {
	// Create test data
	testData := map[string]interface{}{
		"message":   "performance test data",
		"value":     12345,
		"timestamp": "2024-11-10T13:00:00Z",
		"metadata": map[string]string{
			"source": "test",
			"type":   "benchmark",
		},
	}

	jsonData, _ := json.Marshal(testData)

	// Create event with raw data
	event := &Event{
		Type:      EventTypeData,
		ID:        "perf-test",
		Raw:       jsonData,
		Timestamp: time.Now(),
	}

	// Test traditional parsing
	var result1 map[string]interface{}
	err := event.ParseEventData(&result1)
	if err != nil {
		t.Fatalf("Traditional parsing failed: %v", err)
	}

	// Test fast parsing with Raw bytes
	var result2 map[string]interface{}
	err = event.ParseEventData(&result2)
	if err != nil {
		t.Fatalf("Fast parsing failed: %v", err)
	}

	// Test generic parsing
	result3, err := ParseTo[map[string]interface{}](event)
	if err != nil {
		t.Fatalf("Generic parsing failed: %v", err)
	}

	// Verify results are equivalent
	if result1["message"] != result2["message"] || result1["message"] != (*result3)["message"] {
		t.Error("Parsing results are not equivalent")
	}
}

// TestEventObjectInterface tests the EventObject interface implementation
func TestEventObjectInterface(t *testing.T) {
	event := &Event{
		Type:      EventTypeData,
		ID:        "interface-test",
		Raw:       []byte(`{"test": "data"}`),
		Timestamp: time.Now(),
	}

	// Test interface methods directly on Event
	if event.GetEventType() != EventTypeData {
		t.Errorf("Expected event type %s, got %s", EventTypeData, event.GetEventType())
	}

	if event.GetEventID() != "interface-test" {
		t.Errorf("Expected event ID 'interface-test', got '%s'", event.GetEventID())
	}

	if event.GetTimestamp().IsZero() {
		t.Error("Expected non-zero timestamp")
	}

	// Test generic parsing
	data, err := ParseTo[map[string]interface{}](event)
	if err != nil {
		t.Fatalf("Generic parsing failed: %v", err)
	}

	if (*data)["test"] != "data" {
		t.Errorf("Expected 'data', got '%v'", (*data)["test"])
	}
}

// BenchmarkTypedEventVsTraditional compares performance of different parsing methods
func BenchmarkTypedEventVsTraditional(b *testing.B) {
	testData := map[string]interface{}{
		"message":   "benchmark test data",
		"value":     12345,
		"timestamp": "2024-11-10T13:00:00Z",
		"metadata": map[string]string{
			"source": "benchmark",
			"type":   "performance",
		},
	}

	jsonData, _ := json.Marshal(testData)

	event := &Event{
		Type:      EventTypeData,
		ID:        "bench-test",
		Raw:       jsonData,
		Timestamp: time.Now(),
	}

	b.Run("Traditional", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			var result map[string]interface{}
			_ = event.ParseEventData(&result)
		}
	})

	b.Run("FastRaw", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			var result map[string]interface{}
			_ = event.ParseEventData(&result)
		}
	})

	b.Run("GenericParsing", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = ParseTo[map[string]interface{}](event)
		}
	})
}
