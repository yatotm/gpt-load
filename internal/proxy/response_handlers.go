package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

var errStreamEndedBeforeVisibleOutput = errors.New("stream ended before first visible output")

type streamRelayResult struct {
	Committed bool
	Retryable bool
	Err       error
}

type streamStartGuard struct {
	timeout  time.Duration
	cancel   context.CancelFunc
	stopCh   chan struct{}
	stopOnce sync.Once
	timedOut atomic.Bool
}

func newStreamStartGuard(timeout time.Duration, cancel context.CancelFunc) *streamStartGuard {
	if timeout <= 0 {
		return nil
	}

	guard := &streamStartGuard{
		timeout: timeout,
		cancel:  cancel,
		stopCh:  make(chan struct{}),
	}

	go guard.run()
	return guard
}

func (g *streamStartGuard) run() {
	timer := time.NewTimer(g.timeout)
	defer timer.Stop()

	select {
	case <-timer.C:
		g.timedOut.Store(true)
		g.cancel()
	case <-g.stopCh:
	}
}

func (g *streamStartGuard) Commit() {
	g.Stop()
}

func (g *streamStartGuard) Stop() {
	if g == nil {
		return
	}
	g.stopOnce.Do(func() {
		close(g.stopCh)
	})
}

func (g *streamStartGuard) TimedOut() bool {
	return g != nil && g.timedOut.Load()
}

func (g *streamStartGuard) Timeout() time.Duration {
	if g == nil {
		return 0
	}
	return g.timeout
}

type streamFirstVisibleTimeoutError struct {
	timeout time.Duration
}

func (e *streamFirstVisibleTimeoutError) Error() string {
	return fmt.Sprintf("stream first visible output timeout after %s", e.timeout)
}

func newStreamFirstVisibleTimeoutError(timeout time.Duration) error {
	return &streamFirstVisibleTimeoutError{timeout: timeout}
}

func isStreamFirstVisibleTimeoutError(err error) bool {
	var timeoutErr *streamFirstVisibleTimeoutError
	return errors.As(err, &timeoutErr)
}

func streamRetryableStatusCode(err error) int {
	if isStreamFirstVisibleTimeoutError(err) {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

type sseEvent struct {
	raw         []byte
	eventType   string
	data        string
	hasMetadata bool
}

func (ps *ProxyServer) handleStreamingResponse(
	c *gin.Context,
	resp *http.Response,
	startGuard *streamStartGuard,
) streamRelayResult {
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		logrus.Error("Streaming unsupported by the writer, falling back to normal response")
		ps.writeStreamingHeaders(c, resp)
		if startGuard != nil {
			startGuard.Commit()
		}
		ps.handleNormalResponse(c, resp)
		return streamRelayResult{Committed: true}
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(contentType, "text/event-stream") {
		return ps.handleSSEStreamingResponse(c, resp, flusher, startGuard)
	}

	return ps.handleRawStreamingResponse(c, resp, flusher, startGuard)
}

func (ps *ProxyServer) handleSSEStreamingResponse(
	c *gin.Context,
	resp *http.Response,
	flusher http.Flusher,
	startGuard *streamStartGuard,
) streamRelayResult {
	reader := bufio.NewReader(resp.Body)
	var pending bytes.Buffer
	committed := false

	for {
		event, err := readSSEvent(reader)
		if err != nil {
			return streamReadResult(committed, startGuard, err)
		}

		classification := classifySSEvent(event)
		if !committed {
			if classification.keepAlive {
				continue
			}
			if !classification.meaningful {
				pending.Write(event.raw)
				continue
			}

			if startGuard != nil {
				startGuard.Commit()
			}
			ps.writeStreamingHeaders(c, resp)
			if pending.Len() > 0 {
				if _, writeErr := c.Writer.Write(pending.Bytes()); writeErr != nil {
					return streamRelayResult{Committed: true, Err: writeErr}
				}
			}
			if _, writeErr := c.Writer.Write(event.raw); writeErr != nil {
				return streamRelayResult{Committed: true, Err: writeErr}
			}
			flusher.Flush()
			committed = true
			continue
		}

		if _, writeErr := c.Writer.Write(event.raw); writeErr != nil {
			return streamRelayResult{Committed: true, Err: writeErr}
		}
		flusher.Flush()
	}
}

func (ps *ProxyServer) handleRawStreamingResponse(
	c *gin.Context,
	resp *http.Response,
	flusher http.Flusher,
	startGuard *streamStartGuard,
) streamRelayResult {
	buf := make([]byte, 4*1024)
	committed := false

	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if !committed {
				if startGuard != nil {
					startGuard.Commit()
				}
				ps.writeStreamingHeaders(c, resp)
				committed = true
			}
			if _, writeErr := c.Writer.Write(buf[:n]); writeErr != nil {
				return streamRelayResult{Committed: committed, Err: writeErr}
			}
			flusher.Flush()
		}

		if err != nil {
			return streamReadResult(committed, startGuard, err)
		}
	}
}

func streamReadResult(committed bool, startGuard *streamStartGuard, err error) streamRelayResult {
	if err == nil {
		return streamRelayResult{Committed: committed}
	}
	if errors.Is(err, io.EOF) {
		if committed {
			return streamRelayResult{Committed: true}
		}
		if startGuard != nil && startGuard.TimedOut() {
			return streamRelayResult{
				Retryable: true,
				Err:       newStreamFirstVisibleTimeoutError(startGuard.Timeout()),
			}
		}
		return streamRelayResult{Retryable: true, Err: errStreamEndedBeforeVisibleOutput}
	}
	if !committed && startGuard != nil && startGuard.TimedOut() {
		return streamRelayResult{
			Retryable: true,
			Err:       newStreamFirstVisibleTimeoutError(startGuard.Timeout()),
		}
	}
	return streamRelayResult{
		Committed: committed,
		Retryable: !committed,
		Err:       err,
	}
}

func readSSEvent(reader *bufio.Reader) (sseEvent, error) {
	var raw bytes.Buffer
	var dataLines []string
	var eventType string
	hasMetadata := false

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			raw.Write(line)
		}

		if err != nil && !errors.Is(err, io.EOF) {
			return sseEvent{}, err
		}

		trimmed := bytes.TrimRight(line, "\r\n")
		if len(trimmed) == 0 {
			if raw.Len() == 0 && errors.Is(err, io.EOF) {
				return sseEvent{}, io.EOF
			}
			return sseEvent{
				raw:         raw.Bytes(),
				eventType:   eventType,
				data:        strings.Join(dataLines, "\n"),
				hasMetadata: hasMetadata,
			}, nil
		}

		if bytes.HasPrefix(trimmed, []byte(":")) {
			if errors.Is(err, io.EOF) {
				return sseEvent{
					raw:         raw.Bytes(),
					eventType:   eventType,
					data:        strings.Join(dataLines, "\n"),
					hasMetadata: hasMetadata,
				}, nil
			}
			continue
		}

		hasMetadata = true
		switch {
		case bytes.HasPrefix(trimmed, []byte("event:")):
			eventType = strings.TrimSpace(string(trimmed[len("event:"):]))
		case bytes.HasPrefix(trimmed, []byte("data:")):
			dataLine := string(trimmed[len("data:"):])
			if strings.HasPrefix(dataLine, " ") {
				dataLine = dataLine[1:]
			}
			dataLines = append(dataLines, dataLine)
		}

		if errors.Is(err, io.EOF) {
			return sseEvent{
				raw:         raw.Bytes(),
				eventType:   eventType,
				data:        strings.Join(dataLines, "\n"),
				hasMetadata: hasMetadata,
			}, nil
		}
	}
}

type sseEventClassification struct {
	keepAlive  bool
	meaningful bool
}

func classifySSEvent(event sseEvent) sseEventClassification {
	if !event.hasMetadata && strings.TrimSpace(event.data) == "" {
		return sseEventClassification{keepAlive: true}
	}

	eventType := strings.ToLower(strings.TrimSpace(event.eventType))
	switch eventType {
	case "ping", "heartbeat", "keepalive", "keep-alive":
		return sseEventClassification{keepAlive: true}
	}

	data := strings.TrimSpace(event.data)
	if data == "" {
		return sseEventClassification{}
	}
	if data == "[DONE]" {
		return sseEventClassification{meaningful: true}
	}
	if payloadLooksLikeHeartbeat(data) {
		return sseEventClassification{keepAlive: true}
	}
	if payloadHasVisibleOutput(data) {
		return sseEventClassification{meaningful: true}
	}

	return sseEventClassification{meaningful: true}
}

func payloadLooksLikeHeartbeat(payload string) bool {
	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		return false
	}

	if value, ok := decoded["type"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "ping", "heartbeat", "keepalive", "keep-alive":
			return true
		}
	}

	if value, ok := decoded["message"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "ping", "heartbeat":
			return true
		}
	}

	return false
}

func payloadHasVisibleOutput(payload string) bool {
	var decoded any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		return strings.TrimSpace(payload) != ""
	}

	return hasGeminiVisibleOutput(decoded) ||
		hasOpenAIVisibleOutput(decoded) ||
		hasOpenAIResponsesVisibleOutput(decoded) ||
		hasAnthropicVisibleOutput(decoded)
}

func hasGeminiVisibleOutput(value any) bool {
	root, ok := value.(map[string]any)
	if !ok {
		return false
	}

	candidates, ok := root["candidates"].([]any)
	if !ok {
		return false
	}

	for _, candidate := range candidates {
		candidateMap, ok := candidate.(map[string]any)
		if !ok {
			continue
		}
		content, ok := candidateMap["content"].(map[string]any)
		if !ok {
			continue
		}
		if partsHaveVisibleText(content["parts"]) {
			return true
		}
	}

	return false
}

func hasOpenAIVisibleOutput(value any) bool {
	root, ok := value.(map[string]any)
	if !ok {
		return false
	}

	choices, ok := root["choices"].([]any)
	if !ok {
		return false
	}

	for _, choice := range choices {
		choiceMap, ok := choice.(map[string]any)
		if !ok {
			continue
		}
		if deltaMap, ok := choiceMap["delta"].(map[string]any); ok {
			if stringFieldsHaveVisibleText(deltaMap, "content", "reasoning_content", "reasoning", "text") {
				return true
			}
		}
		if messageMap, ok := choiceMap["message"].(map[string]any); ok {
			if stringFieldsHaveVisibleText(messageMap, "content", "reasoning_content", "reasoning", "text") {
				return true
			}
		}
	}

	return false
}

func hasOpenAIResponsesVisibleOutput(value any) bool {
	root, ok := value.(map[string]any)
	if !ok {
		return false
	}

	if delta, ok := root["delta"].(string); ok && strings.TrimSpace(delta) != "" {
		if eventType, ok := root["type"].(string); ok {
			lower := strings.ToLower(eventType)
			if strings.Contains(lower, "output_text") || strings.Contains(lower, "reasoning") {
				return true
			}
		}
	}

	return stringFieldsHaveVisibleText(root, "output_text", "reasoning", "reasoning_content")
}

func hasAnthropicVisibleOutput(value any) bool {
	root, ok := value.(map[string]any)
	if !ok {
		return false
	}

	if deltaMap, ok := root["delta"].(map[string]any); ok {
		if stringFieldsHaveVisibleText(deltaMap, "text", "thinking") {
			return true
		}
	}
	if blockMap, ok := root["content_block"].(map[string]any); ok {
		if stringFieldsHaveVisibleText(blockMap, "text", "thinking") {
			return true
		}
	}

	return false
}

func partsHaveVisibleText(value any) bool {
	parts, ok := value.([]any)
	if !ok {
		return false
	}

	for _, part := range parts {
		partMap, ok := part.(map[string]any)
		if !ok {
			continue
		}
		if stringFieldsHaveVisibleText(partMap, "text", "reasoning", "reasoning_content") {
			return true
		}
	}

	return false
}

func stringFieldsHaveVisibleText(values map[string]any, keys ...string) bool {
	for _, key := range keys {
		value, ok := values[key]
		if !ok {
			continue
		}

		switch typed := value.(type) {
		case string:
			if strings.TrimSpace(typed) != "" {
				return true
			}
		case []any:
			for _, item := range typed {
				if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
					return true
				}
			}
		}
	}

	return false
}

func (ps *ProxyServer) writeResponseHeaders(c *gin.Context, resp *http.Response) {
	for key, values := range resp.Header {
		for _, value := range values {
			c.Header(key, value)
		}
	}
	c.Status(resp.StatusCode)
}

func (ps *ProxyServer) writeStreamingHeaders(c *gin.Context, resp *http.Response) {
	for key, values := range resp.Header {
		for _, value := range values {
			c.Header(key, value)
		}
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(resp.StatusCode)
}

func (ps *ProxyServer) handleNormalResponse(c *gin.Context, resp *http.Response) {
	if _, err := io.Copy(c.Writer, resp.Body); err != nil {
		logUpstreamError("copying response body", err)
	}
}
