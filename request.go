package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tmaxmax/go-sse"
	"golang.org/x/net/http2"
	"k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/klog/v2"
)

type requestRetryFunc func(maxRetries int) WithRetry

func defaultRequestRetryFn(maxRetries int) WithRetry {
	return &withRetry{maxRetries: maxRetries}
}

var (
	// longThrottleLatency defines threshold for logging requests. All requests being
	// throttled (via the provided rateLimiter) for more than longThrottleLatency will
	// be logged.
	longThrottleLatency = 50 * time.Millisecond

	// extraLongThrottleLatency defines the threshold for logging requests at log level 2.
	extraLongThrottleLatency = 1 * time.Second
)

type FileField struct {
	FieldName string // form field name
	FileName  string
	Content   io.Reader
}

type Request struct {
	c *RESTClient

	warningHandler WarningHandler

	rateLimiter flowcontrol.RateLimiter
	backoff     BackoffManager
	timeout     time.Duration
	maxRetries  int

	// generic components accessible via method setters
	verb       string
	pathPrefix string
	subPath    string
	host       string
	params     url.Values
	headers    http.Header

	// multipart upload support
	fileFields    []FileField       // multifile field
	textFields    map[string]string // text field
	multipartMode bool              // if enable multipart mode

	// output
	err error

	// only one of body / bodyBytes may be set. requests using body are not retryable.
	body      io.Reader
	bodyBytes []byte

	retryFn requestRetryFunc
}

func NewRequest(c *RESTClient) *Request {
	var pathPrefix string
	if c.base != nil {
		pathPrefix = path.Join("/", c.base.Path, c.versionedAPIPath)
	} else {
		pathPrefix = path.Join("/", c.versionedAPIPath)
	}

	var timeout time.Duration
	if c.client != nil {
		timeout = c.client.Timeout
	}

	r := &Request{
		c:           c,
		timeout:     timeout,
		rateLimiter: c.rateLimiter,
		backoff:     &NoBackoff{},
		maxRetries:  10,
		pathPrefix:  pathPrefix,
		retryFn:     defaultRequestRetryFn,
		textFields:  make(map[string]string),
	}

	switch {
	case len(c.content.AcceptContentTypes) > 0:
		r.SetHeader("Accept", c.content.AcceptContentTypes)
	case len(c.content.ContentType) > 0:
		r.SetHeader("Accept", c.content.ContentType+", */*")
	}
	return r
}

// Verb sets the verb this request will use.
func (r *Request) Verb(verb string) *Request {
	r.verb = verb
	return r
}

func (r *Request) Prefix(segments ...string) *Request {
	if r.err != nil {
		return r
	}
	r.pathPrefix = path.Join(r.pathPrefix, path.Join(segments...))
	return r
}

func (r *Request) AddFileField(fieldName, fileName string, content io.Reader) *Request {
	r.fileFields = append(r.fileFields, FileField{
		FieldName: fieldName,
		FileName:  fileName,
		Content:   content,
	})
	return r
}

func (r *Request) AddTextField(key, value string) *Request {
	r.textFields[key] = value
	return r
}

func (r *Request) MultipartUpload() *Request {
	r.multipartMode = true
	return r
}

func (r *Request) Suffix(segments ...string) *Request {
	if r.err != nil {
		return r
	}
	r.subPath = path.Join(r.subPath, path.Join(segments...))
	return r
}

func (r *Request) SetHeader(key string, values ...string) *Request {
	if r.headers == nil {
		r.headers = http.Header{}
	}
	r.headers.Del(key)
	for _, value := range values {
		r.headers.Add(key, value)
	}
	return r
}

func (r *Request) SetHost(host string) *Request {
	r.host = host
	return r
}

// Param creates a query parameter with the given string value.
func (r *Request) Param(paramName, s string) *Request {
	if r.err != nil {
		return r
	}
	return r.setParam(paramName, s)
}

func (r *Request) setParam(paramName, value string) *Request {
	if r.params == nil {
		r.params = make(url.Values)
	}
	r.params[paramName] = append(r.params[paramName], value)
	return r
}

// Timeout makes the request use the given duration as an overall timeout for the
// request. Additionally, if set passes the value as "timeout" parameter in URL.
func (r *Request) Timeout(d time.Duration) *Request {
	if r.err != nil {
		return r
	}
	r.timeout = d
	return r
}

func (r *Request) MaxRetries(maxRetries int) *Request {
	if maxRetries < 0 {
		maxRetries = 0
	}
	r.maxRetries = maxRetries
	return r
}

func (r *Request) RequestURI(uri string) *Request {
	if r.err != nil {
		return r
	}
	locator, err := url.Parse(uri)
	if err != nil {
		r.err = err
		return r
	}
	// Ensure path starts with '/'
	if locator.Path != "" && !strings.HasPrefix(locator.Path, "/") {
		r.pathPrefix = "/" + locator.Path
	} else {
		r.pathPrefix = locator.Path
	}
	if len(locator.Query()) > 0 {
		if r.params == nil {
			r.params = make(url.Values)
		}
		for k, v := range locator.Query() {
			r.params[k] = v
		}
	}
	return r
}

// URL returns the current working URL. Check the result of Error() to ensure
// that the returned URL is valid.
func (r *Request) URL() *url.URL {
	p := r.pathPrefix

	finalURL := &url.URL{}
	if r.c.base != nil {
		*finalURL = *r.c.base
	}
	finalURL.Path = p

	query := url.Values{}
	for key, values := range r.params {
		for _, value := range values {
			query.Add(key, value)
		}
	}

	// timeout is handled specially here.
	if r.timeout != 0 {
		query.Set("timeout", r.timeout.String())
	}
	finalURL.RawQuery = query.Encode()
	return finalURL
}

// Do formats and executes the request. Returns a Result object for easy response
// processing.
//
// Error type:
//   - If the server responds with a status: *errors.StatusError or *errors.UnexpectedObjectError
//   - http.Client.Do errors are returned directly.
func (r *Request) Do(ctx context.Context) Result {
	var result Result
	err := r.request(ctx, func(req *http.Request, resp *http.Response) {
		result = r.transformResponse(resp, req)
	})
	if err != nil {
		return Result{err: err}
	}

	return result
}

// request connects to the server and invokes the provided function when a server response is
// received. It handles retry behavior and up front validation of requests. It will invoke
// fn at most once. It will return an error if a problem occurred prior to connecting to the
// server - the provided function is responsible for handling server errors.
func (r *Request) request(ctx context.Context, fn func(*http.Request, *http.Response)) error {
	if r.err != nil {
		return r.err
	}
	client := r.c.client
	if client == nil {
		client = http.DefaultClient
	}

	if r.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}

	isErrRetryableFunc := func(req *http.Request, err error) bool {
		// "Connection reset by peer" or "apiserver is shutting down" are usually a transient errors.
		// Thus in case of "GET" operations, we simply retry it.
		// We are not automatically retrying "write" operations, as they are not idempotent.
		if req.Method != http.MethodGet {
			return false
		}
		// For connection errors and apiserver shutdown errors retry
		if net.IsConnectionReset(err) || !net.IsProbableEOF(err) {
			return true
		}
		return false
	}

	// Right now we make about ten retry attempts if we get a Retry-After response.
	retry := r.retryFn(r.maxRetries)
	for {
		if err := retry.Before(ctx, r); err != nil {
			return retry.WrapPreviousError(err)
		}
		req, err := r.newHTTPRequest(ctx)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		// The value -1 or a value of 0 with a non-nil Body indicates that the length is unknown.
		// https://pkg.go.dev/net/http#Request
		if req.ContentLength >= 0 && !(req.Body != nil && req.ContentLength == 0) {
			//log.Logger.Error(fmt.Errorf("%+v", req), "Error in request")
		}
		retry.After(ctx, r, resp, err)
		done := func() bool {
			defer readAndCloseResponseBody(resp)

			// if the server returns an error in err, the response will be nil.
			f := func(req *http.Request, resp *http.Response) {
				if resp == nil {
					return
				}
				fn(req, resp)
			}

			if retry.IsNextRetry(ctx, r, req, resp, err, isErrRetryableFunc) {
				return false
			}
			f(req, resp)
			return true
		}()
		if done {
			return retry.WrapPreviousError(err)
		}
	}
}

func (r *Request) newHTTPRequest(ctx context.Context) (*http.Request, error) {
	var body io.Reader
	switch {
	case r.body != nil && r.bodyBytes != nil:
		return nil, fmt.Errorf("cannot use both Body and BodyBytes")
	case r.bodyBytes == nil:
		body = r.body
	case r.bodyBytes != nil:
		// Create a new reader specifically for this request.
		// Giving each request a dedicated reader allows retries to avoid races resetting the request body.
		body = bytes.NewReader(r.bodyBytes)
	}
	var writer *multipart.Writer
	if r.multipartMode {
		buffer := &bytes.Buffer{}
		writer = multipart.NewWriter(buffer)

		// handle multifile upload
		if len(r.fileFields) > 0 {
			for _, fileField := range r.fileFields {
				part, err := writer.CreateFormFile(fileField.FieldName, fileField.FileName)
				if err != nil {
					return nil, fmt.Errorf("error creating form file %s: %w", fileField.FieldName, err)
				}
				if _, err = io.Copy(part, fileField.Content); err != nil {
					return nil, fmt.Errorf("error copying file content for %s: %w", fileField.FieldName, err)
				}
			}
		}
		// add all text field
		for key, value := range r.textFields {
			if err := writer.WriteField(key, value); err != nil {
				return nil, fmt.Errorf("error writing text field %s: %w", key, err)
			}
		}

		if err := writer.Close(); err != nil {
			return nil, fmt.Errorf("error closing multipart writer: %w", err)
		}
		body = buffer
	}

	Url := r.URL().String()
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, newDNSMetricsTrace(ctx)), r.verb, Url, body)
	if err != nil {
		return nil, err
	}
	if r.host != "" {
		req.Host = r.host
	}
	if r.multipartMode && writer != nil {
		if r.headers == nil {
			r.headers = http.Header{}
		}
		r.headers.Set("Content-Type", writer.FormDataContentType())
	}

	req.Header = r.headers
	return req, nil
}

// Body makes the request use obj as the body. Optional.
// If obj is a string, try to read a file of that name.
// If obj is a []byte, send it directly.
// If obj is an io.Reader, use it directly.
// Otherwise, set an error.
func (r *Request) Body(obj interface{}) *Request {
	if r.err != nil {
		return r
	}
	switch t := obj.(type) {
	case string:
		data, err := os.ReadFile(t)
		if err != nil {
			r.err = err
			return r
		}
		r.body = nil
		r.bodyBytes = data
	case []byte:
		r.body = nil
		r.bodyBytes = t
	case io.Reader:
		r.body = t
		r.bodyBytes = nil
	default:
		body, err := json.Marshal(obj)
		if err != nil {
			r.err = err
			return r
		}
		r.body = nil
		r.bodyBytes = body
		r.SetHeader("Content-Type", r.c.content.ContentType)
	}
	return r
}

// newDNSMetricsTrace returns an HTTP trace that tracks time spent on DNS lookups per host.
// This metric is available in client as "rest_client_dns_resolution_duration_seconds".
func newDNSMetricsTrace(ctx context.Context) *httptrace.ClientTrace {
	type dnsMetric struct {
		start time.Time
		host  string
		sync.Mutex
	}
	dns := &dnsMetric{}
	return &httptrace.ClientTrace{
		DNSStart: func(info httptrace.DNSStartInfo) {
			dns.Lock()
			defer dns.Unlock()
			dns.start = time.Now()
			dns.host = info.Host
		},
		DNSDone: func(info httptrace.DNSDoneInfo) {
			dns.Lock()
			defer dns.Unlock()
			klog.V(4).Infof("dns done: %s, time: %dns", dns.host, time.Since(dns.start))
		},
	}
}

// isTextResponse returns true if the response appears to be a textual media type.
func isTextResponse(resp *http.Response) bool {
	contentType := resp.Header.Get("Content-Type")
	if len(contentType) == 0 {
		return true
	}
	media, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return strings.HasPrefix(media, "text/")
}

// retryAfterSeconds returns the value of the Retry-After header and true, or 0 and false if
// the header was missing or not a valid number.
func retryAfterSeconds(resp *http.Response) (int, bool) {
	if h := resp.Header.Get("Retry-After"); len(h) > 0 {
		if i, err := strconv.Atoi(h); err == nil {
			return i, true
		}
	}
	return 0, false
}

// transformResponse converts an API response into a structured API object
func (r *Request) transformResponse(resp *http.Response, req *http.Request) Result {
	var body []byte
	if resp.Body != nil {
		data, err := io.ReadAll(resp.Body)
		switch err.(type) {
		case nil:
			body = data
		case http2.StreamError:
			klog.Infof("Stream error %#v when reading response body, may be caused by closed connection.", err)
			streamErr := fmt.Errorf("stream error when reading response body, may be caused by closed connection. Please retry. Original error: %w", err)
			return Result{
				err: streamErr,
			}
		default:
			klog.Errorf("Unexpected error when reading response body: %v", err)
			unexpectedErr := fmt.Errorf("unexpected error when reading response body. Please retry. Original error: %w", err)
			return Result{
				err: unexpectedErr,
			}
		}
	}

	klog.V(5).Infof("[Response] Body: %s, url: %s, [Request] body: %s", string(body), r.URL().String(), string(r.bodyBytes))

	contentType := resp.Header.Get("Content-Type")
	if len(contentType) == 0 {
		contentType = r.c.content.ContentType
	}

	switch {
	case resp.StatusCode == http.StatusSwitchingProtocols:
		// no-op, we've been upgraded
	case resp.StatusCode < http.StatusOK || resp.StatusCode > http.StatusPartialContent:
		// calculate an unstructured error from the response which the Result object may use if the caller
		// did not return a structured error.
		retryAfter, _ := retryAfterSeconds(resp)
		err := r.newUnstructuredResponseError(body, isTextResponse(resp), resp.StatusCode, req.Method, retryAfter)
		return Result{
			body:        body,
			contentType: contentType,
			statusCode:  resp.StatusCode,
			err:         err,
		}
	}

	return Result{
		body:        body,
		contentType: contentType,
		statusCode:  resp.StatusCode,
		warnings:    handleWarnings(resp.Header, r.warningHandler),
	}
}

func (r *Request) tryThrottleWithInfo(ctx context.Context, retryInfo string) error {
	if r.rateLimiter == nil {
		return nil
	}

	now := time.Now()

	err := r.rateLimiter.Wait(ctx)
	if err != nil {
		err = fmt.Errorf("client rate limiter Wait returned an error: %w", err)
	}
	latency := time.Since(now)

	var message string
	switch {
	case len(retryInfo) > 0:
		message = fmt.Sprintf("Waited for %v, %s - request: %s:%s", latency, retryInfo, r.verb, r.URL().String())
	default:
		message = fmt.Sprintf("Waited for %v due to client-side throttling, not priority and fairness, request: %s:%s", latency, r.verb, r.URL().String())
	}

	if latency > longThrottleLatency {
		klog.Info(message)
	}
	if latency > extraLongThrottleLatency {
		// If the rate limiter latency is very high, the log message should be printed at a higher log level,
		// but we use a throttled logger to prevent spamming.
		klog.Info(message)
	}

	return err
}

// maxUnstructuredResponseTextBytes is an upper bound on how much output to include in the unstructured error.
const maxUnstructuredResponseTextBytes = 2048

// transformUnstructuredResponseError handles an error from the server that is not in a structured form.
// It is expected to transform any response that is not recognizable as a clear server sent error from the
// K8S API using the information provided with the request. In practice, HTTP proxies and client libraries
// introduce a level of uncertainty to the responses returned by servers that in common use result in
// unexpected responses. The rough structure is:
//
// 1. Assume the server sends you something sane - JSON + well defined error objects + proper codes
//   - this is the happy path
//   - when you get this output, trust what the server sends
//     2. Guard against empty fields / bodies in received JSON and attempt to cull sufficient info from them to
//     generate a reasonable facsimile of the original failure.
//   - Be sure to use a distinct error type or flag that allows a client to distinguish between this and error 1 above
//     3. Handle true disconnect failures / completely malformed data by moving up to a more generic client error
//     4. Distinguish between various connection failures like SSL certificates, timeouts, proxy errors, unexpected
//     initial contact, the presence of mismatched body contents from posted content types
//   - Give these a separate distinct error type and capture as much as possible of the original message
//
// TODO: introduce transformation of generic http.Client.Do() errors that separates 4.
func (r *Request) transformUnstructuredResponseError(resp *http.Response, req *http.Request, body []byte) error {
	if body == nil && resp.Body != nil {
		if data, err := io.ReadAll(&io.LimitedReader{R: resp.Body, N: maxUnstructuredResponseTextBytes}); err == nil {
			body = data
		}
	}
	retryAfter, _ := retryAfterSeconds(resp)
	return r.newUnstructuredResponseError(body, isTextResponse(resp), resp.StatusCode, req.Method, retryAfter)
}

// newUnstructuredResponseError instantiates the appropriate generic error for the provided input. It also logs the body.
func (r *Request) newUnstructuredResponseError(body []byte, isTextResponse bool, statusCode int, method string, retryAfter int) error {
	// cap the amount of output we create
	if len(body) > maxUnstructuredResponseTextBytes {
		body = body[:maxUnstructuredResponseTextBytes]
	}
	message := strings.TrimSpace(string(body))

	return fmt.Errorf(message)
}

// Result contains the result of calling Request.Do().
type Result struct {
	body        []byte
	warnings    []net.WarningHeader
	contentType string
	err         error
	statusCode  int
}

// Raw returns the raw result.
func (r Result) Raw() ([]byte, error) {
	return r.body, r.err
}

// Into parse result body to target, WARNING: target should be a pointer
func (r Result) Into(target any) error {
	if r.err != nil {
		if errors.Is(r.err, context.DeadlineExceeded) {
			return fmt.Errorf("connection timeout: %w", r.err)
		}
		return r.err
	}
	return json.Unmarshal(r.body, target)
}

// Error return remote server errors
func (r Result) Error() error {
	return r.err
}

// Stream executes the request and returns a streaming result for SSE
func (r *Request) Stream(ctx context.Context) (*StreamResult, error) {
	if r.err != nil {
		return nil, r.err
	}

	// Set SSE headers
	r.SetHeader("Accept", "text/event-stream")
	r.SetHeader("Cache-Control", "no-cache")
	r.SetHeader("Connection", "keep-alive")

	client := r.c.client
	if client == nil {
		client = http.DefaultClient
	}

	if r.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}

	req, err := r.newHTTPRequest(ctx)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	// Check if response is successful
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	return &StreamResult{
		resp: resp,
	}, nil
}

// SSE creates an SSE watcher for streaming events
func (r *Request) SSE(ctx context.Context, cfg *sse.ReadConfig) (func(func(sse.Event, error) bool), io.Closer, error) {
	if r.err != nil {
		return nil, nil, r.err
	}
	if r.c.client == nil {
		r.c.client = http.DefaultClient
	}

	var err error
	req, err := r.newHTTPRequest(ctx)
	if err != nil {
		return nil, nil, err
	}
	resp, err := r.c.client.Do(req)
	if err != nil {
		return nil, nil, err
	}

	return sse.Read(resp.Body, cfg), resp.Body, nil
}

// StreamResult represents a streaming HTTP response
type StreamResult struct {
	resp *http.Response
}

// Response returns the underlying HTTP response
func (sr *StreamResult) Response() *http.Response {
	return sr.resp
}

// Close closes the streaming response
func (sr *StreamResult) Close() error {
	if sr.resp != nil && sr.resp.Body != nil {
		return sr.resp.Body.Close()
	}
	return nil
}
