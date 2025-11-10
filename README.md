# Go REST Client

A powerful Go RESTful client library with fluent API design and Server-Sent Events (SSE) support.

## Features

- 🔗 **Fluent Chain API** - Elegant API design with method chaining support
- 📡 **SSE Support** - Complete Server-Sent Events client implementation
- 🚀 **High Performance** - Zero-copy design based on K8s RawExtension pattern
- 🛡️ **Type Safety** - Generic type-safe JSON parsing support
- 🔄 **Auto Retry** - Built-in retry mechanism and error handling
- ⚡ **Concurrency Safe** - Thread-safe implementation

## Quick Start

### Installation

```bash
go get github.com/ComingCL/go-rest
```

### Basic HTTP Requests

```go
package main

import (
    "context"
    "fmt"
    "net/url"
    "time"
    
    "github.com/ComingCL/go-rest"
)

func main() {
    // Create client
    serverURL, _ := url.Parse("https://api.example.com")
    client := rest.NewRESTClient(serverURL, "v1", rest.ClientContentConfig{
        ContentType: "application/json",
    }, nil)

    // Chain method calls to send request
    var result map[string]interface{}
    err := client.Verb("GET").
        Prefix("users").
        Param("page", "1").
        SetHeader("Authorization", "Bearer token").
        Timeout(30*time.Second).
        Do(context.TODO()).
        Into(&result)
    
    if err != nil {
        fmt.Printf("Request failed: %v\n", err)
        return
    }
    
    fmt.Printf("Result: %+v\n", result)
}
```

### Server-Sent Events (SSE)

#### Method 1: Using Event Handler

```go
package main

import (
    "context"
    "fmt"
    "net/url"
    "time"
    
    "github.com/ComingCL/go-rest"
)

// Implement event handler
type MyEventHandler struct{}

func (h *MyEventHandler) OnEvent(event *rest.Event) {
    fmt.Printf("Received event: %s\n", event.GetDataAsString())
    
    // Type-safe JSON parsing
    if data, err := rest.ParseTo[map[string]interface{}](event); err == nil {
        fmt.Printf("Parsed data: %+v\n", *data)
    }
}

func (h *MyEventHandler) OnError(err error) {
    fmt.Printf("SSE error: %v\n", err)
}

func (h *MyEventHandler) OnConnect() {
    fmt.Println("SSE connection established")
}

func (h *MyEventHandler) OnDisconnect() {
    fmt.Println("SSE connection closed")
}

func main() {
    serverURL, _ := url.Parse("https://api.example.com")
    client := rest.NewRESTClient(serverURL, "v1", rest.ClientContentConfig{
        ContentType: "application/json",
    }, nil)

    handler := &MyEventHandler{}

    // Send SSE request with body data support
    requestData := map[string]interface{}{
        "user_id": "user123",
        "filters": map[string]string{"category": "news"},
    }

    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()

    watcher, err := client.SSE().
        Prefix("stream").
        Body(requestData).                   // Send request body data
        WithEventHandler(handler).           // Set event handler
        WithRetryInterval(3*time.Second).    // Set retry interval
        MaxRetries(5).                       // Set max retries
        WithBufferSize(100).                 // Set buffer size
        SSEWatch(ctx)                        // Start listening

    if err != nil {
        fmt.Printf("Failed to start SSE: %v\n", err)
        return
    }
    defer watcher.Stop()

    // Listen for events and errors
    for {
        select {
        case event := <-watcher.Events():
            if event != nil {
                fmt.Printf("Channel event: %s\n", event.GetDataAsString())
            }
        case err := <-watcher.Errors():
            if err != nil {
                fmt.Printf("Channel error: %v\n", err)
            }
        case <-ctx.Done():
            fmt.Println("Context timeout")
            return
        }
    }
}
```

#### Method 2: Using Stream Reading

```go
func streamExample() {
    serverURL, _ := url.Parse("https://api.example.com")
    client := rest.NewRESTClient(serverURL, "v1", rest.ClientContentConfig{
        ContentType: "application/json",
    }, nil)

    requestData := map[string]interface{}{
        "query": "realtime data",
    }

    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()

    // Create SSE stream
    streamResult, err := client.SSE().
        Prefix("stream").
        Body(requestData).        // Send request body data
        Stream(ctx)

    if err != nil {
        fmt.Printf("Failed to create stream: %v\n", err)
        return
    }
    defer streamResult.Close()

    // Manually read events
    for {
        event, err := streamResult.ReadEvent()
        if err != nil {
            if err.Error() == "EOF" {
                fmt.Println("Stream ended")
                break
            }
            fmt.Printf("Read error: %v\n", err)
            break
        }

        if event != nil {
            fmt.Printf("Event: %s\n", event.GetDataAsString())
            
            // Type-safe parsing
            if data, err := rest.ParseTo[map[string]interface{}](event); err == nil {
                fmt.Printf("Data: %+v\n", *data)
            }
        }
    }
}
```

## API Reference

### RESTClient

```go
// Create client
client := rest.NewRESTClient(baseURL, apiPath, contentConfig, httpClient)

// HTTP methods
client.Verb("GET|POST|PUT|DELETE")

// Path building
client.Prefix("api", "v1")        // Add path prefix
client.Suffix("users", "123")     // Add path suffix

// Parameter setting
client.Param("key", "value")      // Query parameters
client.SetHeader("key", "value")  // Request headers
client.Body(data)                 // Request body
client.Timeout(duration)          // Timeout setting
client.MaxRetries(count)          // Retry count
```

### SSE Configuration

```go
// SSE specific configuration
client.SSE().
    WithLastEventID("event-id").      // Set last event ID
    WithRetryInterval(duration).      // Retry interval
    WithEventHandler(handler).        // Event handler
    WithBufferSize(size).            // Buffer size
    SSEWatch(ctx)                    // Start listening
```

### Event Handling

```go
// Basic parsing
err := event.ParseEventData(&target)

// Type-safe generic parsing
data, err := rest.ParseTo[MyStruct](event)
panicData := rest.MustParseTo[MyStruct](event)

// Data access
rawBytes := event.GetRawData()
dataString := event.GetDataAsString()

// Event information
eventType := event.GetEventType()
eventID := event.GetEventID()
timestamp := event.GetTimestamp()
```

## Design Features

### Method Chaining

All APIs support method chaining for fluent programming experience:

```go
result := client.Verb("POST").
    Prefix("api", "v1").
    Suffix("users").
    SetHeader("Content-Type", "application/json").
    Body(userData).
    Timeout(30*time.Second).
    MaxRetries(3).
    Do(ctx)
```

### High-Performance SSE

- **Zero-Copy Design**: Based on K8s RawExtension pattern, directly uses raw bytes
- **Type Safety**: Generic compile-time type checking support
- **Memory Optimization**: Avoids unnecessary string conversions and memory allocations

### Error Handling

Built-in comprehensive error handling and retry mechanism:

```go
client.MaxRetries(5).                    // Maximum retry count
    WithRetryInterval(2*time.Second).    // Retry interval
    Timeout(30*time.Second)              // Request timeout
```

## Performance

The SSE implementation features significant performance improvements:

- **Raw Byte Storage**: Direct byte array processing without string conversion overhead
- **Generic Type Safety**: Compile-time type checking with zero runtime cost
- **Efficient Parsing**: Direct JSON unmarshaling from raw bytes
- **Memory Efficient**: Minimal memory allocations and garbage collection pressure

## License

MIT License

## Contributing

Issues and Pull Requests are welcome!

---

Author: ComingCL