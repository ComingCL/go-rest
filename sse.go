package rest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"
)

// EventType represents the type of SSE event
type EventType string

const (
	// EventTypeData represents a data event
	EventTypeData EventType = "data"
	// EventTypeError represents an error event
	EventTypeError EventType = "error"
	// EventTypeClose represents a connection close event
	EventTypeClose EventType = "close"
	// EventTypeOpen represents a connection open event
	EventTypeOpen EventType = "open"
	// EventTypeRetry represents a retry event
	EventTypeRetry EventType = "retry"
)

// Event represents a Server-Sent Event with K8s-inspired design patterns
type Event struct {
	// Type is the event type (optional, defaults to "message")
	Type EventType `json:"type,omitempty"`
	// ID is the event ID (optional)
	ID string `json:"id,omitempty"`
	// Raw contains the original bytes for efficient parsing (K8s RawExtension pattern)
	Raw []byte `json:"raw"`
	// Retry is the retry time in milliseconds (optional)
	Retry int `json:"retry,omitempty"`
	// Timestamp is when the event was received
	Timestamp time.Time `json:"timestamp"`
}

// EventObject represents a parsed event with typed data (K8s Object pattern)
type EventObject interface {
	// GetEventType returns the event type
	GetEventType() EventType
	// GetEventID returns the event ID
	GetEventID() string
	// GetTimestamp returns the event timestamp
	GetTimestamp() time.Time
}

// EventHandler defines the interface for handling SSE events
type EventHandler interface {
	// OnEvent is called when an event is received
	OnEvent(event *Event)
	// OnError is called when an error occurs
	OnError(err error)
	// OnConnect is called when the connection is established
	OnConnect()
	// OnDisconnect is called when the connection is closed
	OnDisconnect()
}

// SSEWatcher defines the interface for SSE watching functionality
type SSEWatcher interface {
	// Start begins watching for SSE events
	Start(ctx context.Context) error
	// Stop stops watching for SSE events
	Stop()
	// Events returns a channel to receive events
	Events() <-chan *Event
	// Errors returns a channel to receive errors
	Errors() <-chan error
	// IsStopped returns true if the watcher is stopped
	IsStopped() bool
}

// sseWatcher implements SSEWatcher interface
type sseWatcher struct {
	request *Request

	// channels
	eventCh chan *Event
	errCh   chan error
	stopCh  chan struct{}

	// state
	mu      sync.RWMutex
	stopped bool
	retries int
	lastID  string

	// connection
	resp *http.Response
}

// newSSEWatcher creates a new SSE watcher
func newSSEWatcher(req *Request) *sseWatcher {
	bufferSize := req.sseBufferSize
	if bufferSize <= 0 {
		bufferSize = 100 // default buffer size
	}

	return &sseWatcher{
		request: req,
		eventCh: make(chan *Event, bufferSize),
		errCh:   make(chan error, 10),
		stopCh:  make(chan struct{}),
		lastID:  req.sseLastEventID,
	}
}

// Start implements SSEWatcher interface
func (w *sseWatcher) Start(ctx context.Context) error {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return fmt.Errorf("watcher is already stopped")
	}
	w.mu.Unlock()

	go w.watch(ctx)
	return nil
}

// Stop implements SSEWatcher interface
func (w *sseWatcher) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.stopped {
		return
	}

	w.stopped = true
	close(w.stopCh)

	if w.resp != nil && w.resp.Body != nil {
		w.resp.Body.Close()
	}

	close(w.eventCh)
	close(w.errCh)

	if w.request.sseEventHandler != nil {
		w.request.sseEventHandler.OnDisconnect()
	}
}

// Events implements SSEWatcher interface
func (w *sseWatcher) Events() <-chan *Event {
	return w.eventCh
}

// Errors implements SSEWatcher interface
func (w *sseWatcher) Errors() <-chan error {
	return w.errCh
}

// IsStopped implements SSEWatcher interface
func (w *sseWatcher) IsStopped() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.stopped
}

// watch is the main watching loop
func (w *sseWatcher) watch(ctx context.Context) {
	var err error
	runtime.RecoverFromPanic(&err)

	for {
		select {
		case <-ctx.Done():
			w.sendError(ctx.Err())
			return
		case <-w.stopCh:
			return
		default:
		}

		if err := w.connect(ctx); err != nil {
			if w.shouldRetry() {
				w.retries++
				retryInterval := w.request.sseRetryInterval
				if retryInterval <= 0 {
					retryInterval = 3 * time.Second // default retry interval
				}
				klog.V(4).Infof("SSE connection failed, retrying in %v (attempt %d): %v",
					retryInterval, w.retries, err)

				select {
				case <-time.After(retryInterval):
					continue
				case <-ctx.Done():
					w.sendError(ctx.Err())
					return
				case <-w.stopCh:
					return
				}
			} else {
				w.sendError(err)
				return
			}
		}

		// Reset retry count on successful connection
		w.retries = 0

		if err := w.readEvents(ctx); err != nil {
			if w.IsStopped() {
				return
			}

			klog.V(4).Infof("SSE read error: %v", err)
			w.sendError(err)

			// Close current connection before retry
			if w.resp != nil && w.resp.Body != nil {
				w.resp.Body.Close()
			}
		}
	}
}

// connect establishes the SSE connection
func (w *sseWatcher) connect(ctx context.Context) error {
	// Set Last-Event-ID if available
	if w.lastID != "" {
		w.request.SetHeader("Last-Event-ID", w.lastID)
	}

	// Execute streaming request
	streamResult, err := w.request.Stream(ctx)
	if err != nil {
		return err
	}

	w.resp = streamResult.Response()

	// Notify connection established
	if w.request.sseEventHandler != nil {
		w.request.sseEventHandler.OnConnect()
	}

	return nil
}

// shouldRetry determines if we should retry the connection
func (w *sseWatcher) shouldRetry() bool {
	if w.IsStopped() {
		return false
	}

	maxRetries := w.request.maxRetries
	if maxRetries == 0 {
		maxRetries = -1 // default unlimited retries
	}

	if maxRetries == -1 {
		return true // unlimited retries
	}

	return w.retries < maxRetries
}

// sendError sends an error to the error channel
func (w *sseWatcher) sendError(err error) {
	if w.IsStopped() {
		return
	}

	select {
	case w.errCh <- err:
	default:
		klog.V(4).Infof("Error channel full, dropping error: %v", err)
	}

	if w.request.sseEventHandler != nil {
		w.request.sseEventHandler.OnError(err)
	}
}

// sendEvent sends an event to the event channel
func (w *sseWatcher) sendEvent(event *Event) {
	if w.IsStopped() {
		return
	}

	select {
	case w.eventCh <- event:
	default:
		klog.V(4).Infof("Event channel full, dropping event: %+v", event)
	}

	if w.request.sseEventHandler != nil {
		w.request.sseEventHandler.OnEvent(event)
	}
}

// readEvents reads and parses SSE events from the response stream
func (w *sseWatcher) readEvents(ctx context.Context) error {
	if w.resp == nil || w.resp.Body == nil {
		return fmt.Errorf("no response body to read from")
	}

	reader := bufio.NewReader(w.resp.Body)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.stopCh:
			return nil
		default:
		}

		event, err := w.parseEvent(reader)
		if err != nil {
			if err == io.EOF {
				return err
			}
			klog.V(4).Infof("Error parsing SSE event: %v", err)
			continue
		}

		if event != nil {
			// Update last event ID
			if event.ID != "" {
				w.lastID = event.ID
			}

			w.sendEvent(event)
		}
	}
}

// parseEvent parses a single SSE event from the stream
func (w *sseWatcher) parseEvent(reader *bufio.Reader) (*Event, error) {
	event := &Event{
		Type:      EventTypeData,
		Timestamp: time.Now(),
	}

	var dataLines []string

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}

		line = strings.TrimRight(line, "\r\n")

		// Empty line indicates end of event
		if line == "" {
			break
		}

		// Skip comments
		if strings.HasPrefix(line, ":") {
			continue
		}

		// Parse field
		if colonIndex := strings.Index(line, ":"); colonIndex != -1 {
			field := line[:colonIndex]
			value := strings.TrimSpace(line[colonIndex+1:])

			switch field {
			case "event":
				event.Type = EventType(value)
			case "data":
				dataLines = append(dataLines, value)
			case "id":
				event.ID = value
			case "retry":
				if retry, err := strconv.Atoi(value); err == nil {
					event.Retry = retry
				}
			}
		} else {
			// Field without value
			switch line {
			case "data":
				dataLines = append(dataLines, "")
			}
		}
	}

	// Join data lines and store as raw bytes for efficient parsing (K8s RawExtension pattern)
	if len(dataLines) > 0 {
		joinedData := strings.Join(dataLines, "\n")
		event.Raw = []byte(joinedData)
		return event, nil
	}

	// Empty event, continue reading
	return nil, nil
}

// ParseEventData parses event data as JSON into the target object
func (e *Event) ParseEventData(target interface{}) error {
	if len(e.Raw) == 0 {
		return fmt.Errorf("no data to parse")
	}
	return json.Unmarshal(e.Raw, target)
}

// GetRawData returns the raw bytes for direct processing
func (e *Event) GetRawData() []byte {
	return e.Raw
}

// GetDataAsString returns the raw data as string for display purposes
func (e *Event) GetDataAsString() string {
	return string(e.Raw)
}

// ParseTo parses event data directly into the provided type (generic method)
func ParseTo[T any](e *Event) (*T, error) {
	if len(e.Raw) == 0 {
		return nil, fmt.Errorf("no data to parse")
	}

	var result T
	if err := json.Unmarshal(e.Raw, &result); err != nil {
		return nil, fmt.Errorf("failed to parse event data: %w", err)
	}

	return &result, nil
}

// GetEventType implements EventObject interface
func (e *Event) GetEventType() EventType {
	return e.Type
}

// GetEventID implements EventObject interface
func (e *Event) GetEventID() string {
	return e.ID
}

// GetTimestamp implements EventObject interface
func (e *Event) GetTimestamp() time.Time {
	return e.Timestamp
}

// String returns a string representation of the event
func (e *Event) String() string {
	return fmt.Sprintf("Event{Type: %s, ID: %s, Data: %s, Retry: %d}",
		e.Type, e.ID, string(e.Raw), e.Retry)
}
