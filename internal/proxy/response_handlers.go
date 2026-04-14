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
	Committed           bool
	Retryable           bool
	Err                 error
	LogicalIssue        error
	FirstVisibleLatency time.Duration
}

type logicalResponseIssue struct {
	message string
}

func (e *logicalResponseIssue) Error() string {
	return e.message
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
	attemptStart time.Time,
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
		return ps.handleSSEStreamingResponse(c, resp, flusher, startGuard, attemptStart)
	}

	return ps.handleRawStreamingResponse(c, resp, flusher, startGuard, attemptStart)
}

func (ps *ProxyServer) handleSSEStreamingResponse(
	c *gin.Context,
	resp *http.Response,
	flusher http.Flusher,
	startGuard *streamStartGuard,
	attemptStart time.Time,
) streamRelayResult {
	reader := bufio.NewReader(resp.Body)
	var pending bytes.Buffer
	committed := false
	var firstVisibleLatency time.Duration
	semanticTracker := newStreamSemanticTracker()

	for {
		event, err := readSSEvent(reader)
		if err != nil {
			result := streamReadResult(committed, startGuard, err, firstVisibleLatency)
			if result.Err == nil {
				result.LogicalIssue = semanticTracker.Finalize()
			}
			return result
		}

		classification := classifySSEvent(event)
		semanticTracker.Observe(event)
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
			firstVisibleLatency = time.Since(attemptStart)
			ps.writeStreamingHeaders(c, resp)
			if pending.Len() > 0 {
				if _, writeErr := c.Writer.Write(pending.Bytes()); writeErr != nil {
					return streamRelayResult{Committed: true, Err: writeErr, FirstVisibleLatency: firstVisibleLatency}
				}
			}
			if _, writeErr := c.Writer.Write(event.raw); writeErr != nil {
				return streamRelayResult{Committed: true, Err: writeErr, FirstVisibleLatency: firstVisibleLatency}
			}
			flusher.Flush()
			committed = true
			continue
		}

		if _, writeErr := c.Writer.Write(event.raw); writeErr != nil {
			return streamRelayResult{Committed: true, Err: writeErr, FirstVisibleLatency: firstVisibleLatency}
		}
		flusher.Flush()
	}
}

func (ps *ProxyServer) handleRawStreamingResponse(
	c *gin.Context,
	resp *http.Response,
	flusher http.Flusher,
	startGuard *streamStartGuard,
	attemptStart time.Time,
) streamRelayResult {
	buf := make([]byte, 4*1024)
	committed := false
	var firstVisibleLatency time.Duration

	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if !committed {
				if startGuard != nil {
					startGuard.Commit()
				}
				firstVisibleLatency = time.Since(attemptStart)
				ps.writeStreamingHeaders(c, resp)
				committed = true
			}
			if _, writeErr := c.Writer.Write(buf[:n]); writeErr != nil {
				return streamRelayResult{Committed: committed, Err: writeErr, FirstVisibleLatency: firstVisibleLatency}
			}
			flusher.Flush()
		}

		if err != nil {
			return streamReadResult(committed, startGuard, err, firstVisibleLatency)
		}
	}
}

func streamReadResult(committed bool, startGuard *streamStartGuard, err error, firstVisibleLatency time.Duration) streamRelayResult {
	if err == nil {
		return streamRelayResult{Committed: committed, FirstVisibleLatency: firstVisibleLatency}
	}
	if errors.Is(err, io.EOF) {
		if committed {
			return streamRelayResult{Committed: true, FirstVisibleLatency: firstVisibleLatency}
		}
		if startGuard != nil && startGuard.TimedOut() {
			return streamRelayResult{
				Retryable:           true,
				Err:                 newStreamFirstVisibleTimeoutError(startGuard.Timeout()),
				FirstVisibleLatency: firstVisibleLatency,
			}
		}
		return streamRelayResult{Retryable: true, Err: errStreamEndedBeforeVisibleOutput, FirstVisibleLatency: firstVisibleLatency}
	}
	if !committed && startGuard != nil && startGuard.TimedOut() {
		return streamRelayResult{
			Retryable:           true,
			Err:                 newStreamFirstVisibleTimeoutError(startGuard.Timeout()),
			FirstVisibleLatency: firstVisibleLatency,
		}
	}
	return streamRelayResult{
		Committed:           committed,
		Retryable:           !committed,
		Err:                 err,
		FirstVisibleLatency: firstVisibleLatency,
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
	if payloadHasTerminalIssue(data) {
		return sseEventClassification{meaningful: true}
	}

	return sseEventClassification{}
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

func payloadHasTerminalIssue(payload string) bool {
	var decoded any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		return false
	}

	if extractStructuredPayloadError(decoded) != nil {
		return true
	}

	inspection := inspectGeminiPayload(decoded)
	return inspection.PromptBlockIssue != nil || inspection.CandidateIssue != nil
}

type streamSemanticTracker struct {
	sawGemini             bool
	sawGeminiCandidates   bool
	sawGeminiUsefulOutput bool
	structuredIssue       error
	promptBlockIssue      error
	candidateIssue        error
}

func newStreamSemanticTracker() *streamSemanticTracker {
	return &streamSemanticTracker{}
}

func (t *streamSemanticTracker) Observe(event sseEvent) {
	if t == nil {
		return
	}

	data := strings.TrimSpace(event.data)
	if data == "" || data == "[DONE]" {
		return
	}

	var decoded any
	if err := json.Unmarshal([]byte(data), &decoded); err != nil {
		return
	}

	if issue := extractStructuredPayloadError(decoded); issue != nil && t.structuredIssue == nil {
		t.structuredIssue = issue
	}

	inspection := inspectGeminiPayload(decoded)
	if !inspection.Recognized {
		return
	}

	t.sawGemini = true
	if inspection.HasCandidates {
		t.sawGeminiCandidates = true
	}
	if inspection.HasUsefulOutput {
		t.sawGeminiUsefulOutput = true
	}
	if inspection.PromptBlockIssue != nil && t.promptBlockIssue == nil {
		t.promptBlockIssue = inspection.PromptBlockIssue
	}
	if inspection.CandidateIssue != nil && t.candidateIssue == nil {
		t.candidateIssue = inspection.CandidateIssue
	}
}

func (t *streamSemanticTracker) Finalize() error {
	if t == nil {
		return nil
	}
	if t.structuredIssue != nil {
		return t.structuredIssue
	}
	if t.promptBlockIssue != nil {
		return t.promptBlockIssue
	}
	if t.candidateIssue != nil {
		return t.candidateIssue
	}
	if !t.sawGemini {
		return nil
	}
	if !t.sawGeminiCandidates {
		return &logicalResponseIssue{message: "gemini returned no candidates"}
	}
	if !t.sawGeminiUsefulOutput {
		return &logicalResponseIssue{message: "gemini returned no usable content"}
	}
	return nil
}

type geminiPayloadInspection struct {
	Recognized       bool
	HasCandidates    bool
	HasUsefulOutput  bool
	PromptBlockIssue error
	CandidateIssue   error
}

func inspectGeminiPayload(value any) geminiPayloadInspection {
	root, ok := value.(map[string]any)
	if !ok {
		return geminiPayloadInspection{}
	}

	inspection := geminiPayloadInspection{
		Recognized: hasAnyMapKey(root, "candidates", "promptFeedback", "usageMetadata", "modelVersion", "responseId", "modelStatus"),
	}
	if !inspection.Recognized {
		return inspection
	}

	if promptFeedback, ok := root["promptFeedback"].(map[string]any); ok {
		blockReason := strings.ToUpper(strings.TrimSpace(asString(promptFeedback["blockReason"])))
		if blockReason != "" && blockReason != "BLOCK_REASON_UNSPECIFIED" {
			inspection.PromptBlockIssue = &logicalResponseIssue{
				message: fmt.Sprintf("gemini prompt blocked: %s", blockReason),
			}
		}
	}

	candidates, ok := root["candidates"].([]any)
	if !ok {
		return inspection
	}
	inspection.HasCandidates = len(candidates) > 0

	for _, candidate := range candidates {
		candidateMap, ok := candidate.(map[string]any)
		if !ok {
			continue
		}
		if geminiCandidateHasUsefulOutput(candidateMap) {
			inspection.HasUsefulOutput = true
		}

		finishReason := strings.ToUpper(strings.TrimSpace(asString(candidateMap["finishReason"])))
		if !geminiFinishReasonIsFailure(finishReason) || inspection.CandidateIssue != nil {
			continue
		}

		message := fmt.Sprintf("gemini candidate finished with %s", finishReason)
		if finishMessage := strings.TrimSpace(asString(candidateMap["finishMessage"])); finishMessage != "" {
			message += ": " + finishMessage
		}
		inspection.CandidateIssue = &logicalResponseIssue{message: message}
	}

	return inspection
}

func extractStructuredPayloadError(value any) error {
	root, ok := value.(map[string]any)
	if !ok {
		return nil
	}

	rawError, exists := root["error"]
	if !exists {
		return nil
	}

	switch typed := rawError.(type) {
	case string:
		if msg := strings.TrimSpace(typed); msg != "" {
			return &logicalResponseIssue{message: msg}
		}
	case map[string]any:
		if msg := strings.TrimSpace(asString(typed["message"])); msg != "" {
			return &logicalResponseIssue{message: msg}
		}
		if status := strings.TrimSpace(asString(typed["status"])); status != "" {
			return &logicalResponseIssue{message: status}
		}
		if code := strings.TrimSpace(asString(typed["code"])); code != "" {
			return &logicalResponseIssue{message: code}
		}
	default:
		if marshaled, err := json.Marshal(typed); err == nil {
			if msg := strings.TrimSpace(string(marshaled)); msg != "" {
				return &logicalResponseIssue{message: msg}
			}
		}
	}

	return nil
}

func geminiCandidateHasUsefulOutput(candidate map[string]any) bool {
	content, ok := candidate["content"].(map[string]any)
	if !ok {
		return false
	}

	parts, ok := content["parts"].([]any)
	if !ok {
		return false
	}

	for _, part := range parts {
		partMap, ok := part.(map[string]any)
		if !ok {
			continue
		}

		if text := strings.TrimSpace(asString(partMap["text"])); text != "" {
			if thought, ok := partMap["thought"].(bool); !ok || !thought {
				return true
			}
		}

		for _, key := range []string{"functionCall", "functionResponse", "inlineData", "fileData", "executableCode", "codeExecutionResult"} {
			if _, ok := partMap[key]; ok {
				return true
			}
		}
	}

	return false
}

func geminiFinishReasonIsFailure(reason string) bool {
	switch reason {
	case "", "FINISH_REASON_UNSPECIFIED", "STOP", "MAX_TOKENS":
		return false
	case "SAFETY", "RECITATION", "LANGUAGE", "OTHER", "BLOCKLIST", "PROHIBITED_CONTENT",
		"SPII", "MALFORMED_FUNCTION_CALL", "IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT",
		"IMAGE_OTHER", "NO_IMAGE", "IMAGE_RECITATION", "UNEXPECTED_TOOL_CALL",
		"TOO_MANY_TOOL_CALLS", "MISSING_THOUGHT_SIGNATURE", "MALFORMED_RESPONSE":
		return true
	default:
		return false
	}
}

func hasAnyMapKey(values map[string]any, keys ...string) bool {
	for _, key := range keys {
		if _, ok := values[key]; ok {
			return true
		}
	}
	return false
}

func asString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	case json.Number:
		return typed.String()
	case float64:
		return fmt.Sprintf("%.0f", typed)
	case int:
		return fmt.Sprintf("%d", typed)
	case int64:
		return fmt.Sprintf("%d", typed)
	default:
		return ""
	}
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
		if partsHaveVisibleText(content["parts"]) || partsHaveStructuredOutput(content["parts"]) {
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
			if stringFieldsHaveVisibleText(deltaMap, "content", "reasoning_content", "reasoning", "text") ||
				openAIMessageHasStructuredOutput(deltaMap) {
				return true
			}
		}
		if messageMap, ok := choiceMap["message"].(map[string]any); ok {
			if stringFieldsHaveVisibleText(messageMap, "content", "reasoning_content", "reasoning", "text") ||
				openAIMessageHasStructuredOutput(messageMap) {
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

	if stringFieldsHaveVisibleText(root, "output_text", "reasoning", "reasoning_content") {
		return true
	}

	eventType := strings.ToLower(strings.TrimSpace(asString(root["type"])))
	if strings.Contains(eventType, "function_call") ||
		strings.Contains(eventType, "custom_tool_call") ||
		strings.Contains(eventType, "mcp_call") ||
		strings.Contains(eventType, "web_search_call") ||
		strings.Contains(eventType, "file_search_call") ||
		strings.Contains(eventType, "computer_call") {
		return true
	}

	if item, ok := root["item"].(map[string]any); ok && responseItemHasStructuredOutput(item) {
		return true
	}

	output, ok := root["output"].([]any)
	if !ok {
		return false
	}

	for _, item := range output {
		itemMap, ok := item.(map[string]any)
		if ok && responseItemHasStructuredOutput(itemMap) {
			return true
		}
	}

	return false
}

func hasAnthropicVisibleOutput(value any) bool {
	root, ok := value.(map[string]any)
	if !ok {
		return false
	}

	if deltaMap, ok := root["delta"].(map[string]any); ok {
		if stringFieldsHaveVisibleText(deltaMap, "text", "thinking") || anthropicDeltaHasStructuredOutput(deltaMap) {
			return true
		}
	}
	if blockMap, ok := root["content_block"].(map[string]any); ok {
		if stringFieldsHaveVisibleText(blockMap, "text", "thinking") || anthropicContentBlockHasStructuredOutput(blockMap) {
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

func partsHaveStructuredOutput(value any) bool {
	parts, ok := value.([]any)
	if !ok {
		return false
	}

	for _, part := range parts {
		partMap, ok := part.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"functionCall", "functionResponse", "inlineData", "fileData", "executableCode", "codeExecutionResult"} {
			if _, ok := partMap[key]; ok {
				return true
			}
		}
	}

	return false
}

func openAIMessageHasStructuredOutput(values map[string]any) bool {
	if _, ok := values["function_call"]; ok {
		return true
	}

	toolCalls, ok := values["tool_calls"].([]any)
	return ok && len(toolCalls) > 0
}

func responseItemHasStructuredOutput(item map[string]any) bool {
	itemType := strings.ToLower(strings.TrimSpace(asString(item["type"])))
	return strings.Contains(itemType, "function_call") ||
		strings.Contains(itemType, "custom_tool_call") ||
		strings.Contains(itemType, "mcp_call") ||
		strings.Contains(itemType, "web_search_call") ||
		strings.Contains(itemType, "file_search_call") ||
		strings.Contains(itemType, "computer_call")
}

func anthropicDeltaHasStructuredOutput(delta map[string]any) bool {
	deltaType := strings.ToLower(strings.TrimSpace(asString(delta["type"])))
	if deltaType == "input_json_delta" {
		return strings.TrimSpace(asString(delta["partial_json"])) != ""
	}
	return false
}

func anthropicContentBlockHasStructuredOutput(block map[string]any) bool {
	blockType := strings.ToLower(strings.TrimSpace(asString(block["type"])))
	return blockType == "tool_use" || blockType == "server_tool_use"
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
