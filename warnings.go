package rest

import (
	"net/http"
	"sync"

	"k8s.io/apimachinery/pkg/util/net"
	"k8s.io/klog/v2"
)

// WarningHandler is an interface for handling warning headers
type WarningHandler interface {
	// HandleWarningHeader is called with the warn code, agent, and text when a warning header is countered.
	HandleWarningHeader(code int, agent string, text string)
}

var (
	defaultWarningHandler     WarningHandler = WarningLogger{}
	defaultWarningHandlerLock sync.RWMutex
)

// WarningLogger is an implementation of WarningHandler that logs code 299 warnings
type WarningLogger struct{}

func (WarningLogger) HandleWarningHeader(code int, agent string, message string) {
	if code != 299 || len(message) == 0 {
		return
	}
	klog.Infof("[WarningLogger] got agent: %s, message: %s", agent, message)
}

func getDefaultWarningHandler() WarningHandler {
	defaultWarningHandlerLock.RLock()
	defer defaultWarningHandlerLock.RUnlock()
	l := defaultWarningHandler
	return l
}

func handleWarnings(headers http.Header, handler WarningHandler) []net.WarningHeader {
	if handler == nil {
		handler = getDefaultWarningHandler()
	}

	warnings, _ := net.ParseWarningHeaders(headers["Warning"])
	for _, warning := range warnings {
		handler.HandleWarningHeader(warning.Code, warning.Agent, warning.Text)
	}
	return warnings
}
