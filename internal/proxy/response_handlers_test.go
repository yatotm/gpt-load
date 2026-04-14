package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

var strictGeminiSemanticPolicy = streamSemanticPolicy{StrictGeminiEmptyResponse: true}

func TestHandleStreamingResponseRetriesBeforeFirstVisibleOutput(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	reader, writer := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		<-ctx.Done()
		_ = writer.CloseWithError(ctx.Err())
	}()

	go func() {
		_, _ = writer.Write([]byte(": ping\n\n"))
		time.Sleep(80 * time.Millisecond)
	}()

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
		},
		Body: reader,
	}

	server := &ProxyServer{}
	result := server.handleStreamingResponse(c, resp, newStreamStartGuard(30*time.Millisecond, cancel), time.Now())

	if !result.Retryable {
		t.Fatal("expected pre-output timeout to be retryable")
	}
	if result.Committed {
		t.Fatal("expected no bytes to be committed before timeout")
	}
	if !isStreamFirstVisibleTimeoutError(result.Err) {
		t.Fatalf("expected stream first visible timeout error, got %v", result.Err)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("expected empty downstream body, got %q", recorder.Body.String())
	}
}

func TestHandleStreamingResponseMarksEmptySSEBodyExplicitly(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	reader, writer := io.Pipe()
	defer reader.Close()

	go func() {
		defer writer.Close()
	}()

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
		},
		Body: reader,
	}

	server := &ProxyServer{}
	result := server.handleStreamingResponse(c, resp, newStreamStartGuard(time.Second, func() {}), time.Now())

	if !result.Retryable {
		t.Fatal("expected empty SSE body to be retryable")
	}
	if !errors.Is(result.Err, errStreamEndedWithoutSSEEvents) {
		t.Fatalf("expected empty SSE error, got %v", result.Err)
	}
}

func TestHandleStreamingResponseAllowsMetadataOnlySSEInPermissiveMode(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	reader, writer := io.Pipe()
	defer reader.Close()

	go func() {
		defer writer.Close()
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
	}()

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
		},
		Body: reader,
	}

	server := &ProxyServer{}
	result := server.handleStreamingResponse(c, resp, newStreamStartGuard(time.Second, func() {}), time.Now())

	if result.Err != nil {
		t.Fatalf("expected metadata-only SSE to be treated as success, got %v", result.Err)
	}
	if !result.Committed {
		t.Fatal("expected metadata-only SSE to commit in permissive mode")
	}
	if result.LogicalIssue != nil {
		t.Fatalf("expected metadata-only SSE to avoid logical failure in permissive mode, got %v", result.LogicalIssue)
	}
}

func TestHandleStreamingResponseMarksKeepAliveOnlySSEExplicitly(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	reader, writer := io.Pipe()
	defer reader.Close()

	go func() {
		defer writer.Close()
		_, _ = writer.Write([]byte(": ping\n\n"))
	}()

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
		},
		Body: reader,
	}

	server := &ProxyServer{}
	result := server.handleStreamingResponse(c, resp, newStreamStartGuard(time.Second, func() {}), time.Now())

	if !result.Retryable {
		t.Fatal("expected keepalive-only SSE to be retryable")
	}
	if !errors.Is(result.Err, errStreamEndedAfterKeepAliveOnly) {
		t.Fatalf("expected keepalive-only SSE error, got %v", result.Err)
	}
}

func TestHandleStreamingResponseCommitsOnGeminiThoughtEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	reader, writer := io.Pipe()
	defer reader.Close()

	go func() {
		defer writer.Close()
		_, _ = writer.Write([]byte(": ping\n\n"))
		_, _ = writer.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"thought chunk\",\"thought\":true}],\"role\":\"model\"},\"index\":0}]}\n\n"))
	}()

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
		},
		Body: reader,
	}

	server := &ProxyServer{}
	result := server.handleStreamingResponseWithPolicy(c, resp, newStreamStartGuard(time.Second, func() {}), time.Now(), strictGeminiSemanticPolicy)

	if result.Err != nil {
		t.Fatalf("expected successful relay, got %v", result.Err)
	}
	if !result.Committed {
		t.Fatal("expected stream to be committed after thought output")
	}
	if result.FirstVisibleLatency <= 0 {
		t.Fatalf("expected first visible latency to be recorded, got %v", result.FirstVisibleLatency)
	}
	if result.LogicalIssue == nil {
		t.Fatal("expected thought-only stream to be marked as logical failure")
	}
	body := recorder.Body.String()
	if strings.Contains(body, ": ping") {
		t.Fatalf("expected heartbeat to be dropped before commit, got %q", body)
	}
	if !strings.Contains(body, "\"thought\":true") {
		t.Fatalf("expected thought event to be forwarded, got %q", body)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("expected text/event-stream content type, got %q", got)
	}
}

func TestHandleStreamingResponseAllowsGeminiEmptyCandidateInPermissiveMode(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	reader, writer := io.Pipe()
	defer reader.Close()

	go func() {
		defer writer.Close()
		_, _ = writer.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[],\"role\":\"model\"},\"finishReason\":\"STOP\",\"index\":0}]}\n\n"))
	}()

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
		},
		Body: reader,
	}

	server := &ProxyServer{}
	result := server.handleStreamingResponse(c, resp, newStreamStartGuard(time.Second, func() {}), time.Now())

	if result.Err != nil {
		t.Fatalf("expected permissive gemini empty candidate to succeed, got %v", result.Err)
	}
	if !result.Committed {
		t.Fatal("expected empty candidate frame to commit in permissive mode")
	}
	if result.LogicalIssue != nil {
		t.Fatalf("expected permissive gemini empty candidate to avoid logical failure, got %v", result.LogicalIssue)
	}
}

func TestHandleStreamingResponseTreatsGeminiPromptBlockAsLogicalFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	reader, writer := io.Pipe()
	defer reader.Close()

	go func() {
		defer writer.Close()
		_, _ = writer.Write([]byte("data: {\"promptFeedback\":{\"blockReason\":\"SAFETY\"}}\n\n"))
	}()

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
		},
		Body: reader,
	}

	server := &ProxyServer{}
	result := server.handleStreamingResponseWithPolicy(c, resp, newStreamStartGuard(time.Second, func() {}), time.Now(), strictGeminiSemanticPolicy)

	if result.Err != nil {
		t.Fatalf("expected successful relay, got %v", result.Err)
	}
	if !result.Committed {
		t.Fatal("expected prompt block frame to be forwarded")
	}
	if result.LogicalIssue == nil {
		t.Fatal("expected prompt block to be marked as logical failure")
	}
	if !strings.Contains(result.LogicalIssue.Error(), "gemini prompt blocked") {
		t.Fatalf("expected prompt block error, got %v", result.LogicalIssue)
	}
}

func TestHandleStreamingResponseTreatsGeminiSafetyFinishAsLogicalFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	reader, writer := io.Pipe()
	defer reader.Close()

	go func() {
		defer writer.Close()
		_, _ = writer.Write([]byte("data: {\"candidates\":[{\"finishReason\":\"SAFETY\",\"finishMessage\":\"blocked by safety\",\"content\":{\"parts\":[]},\"index\":0}]}\n\n"))
	}()

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
		},
		Body: reader,
	}

	server := &ProxyServer{}
	result := server.handleStreamingResponse(c, resp, newStreamStartGuard(time.Second, func() {}), time.Now())

	if result.Err != nil {
		t.Fatalf("expected successful relay, got %v", result.Err)
	}
	if result.LogicalIssue == nil {
		t.Fatal("expected abnormal finish reason to be marked as logical failure")
	}
	if !strings.Contains(result.LogicalIssue.Error(), "SAFETY") {
		t.Fatalf("expected safety finish reason in logical issue, got %v", result.LogicalIssue)
	}
}

func TestHandleStreamingResponseAllowsGeminiThoughtThenAnswer(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	reader, writer := io.Pipe()
	defer reader.Close()

	go func() {
		defer writer.Close()
		_, _ = writer.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"thought chunk\",\"thought\":true}],\"role\":\"model\"},\"index\":0}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"final answer\"}],\"role\":\"model\"},\"finishReason\":\"STOP\",\"index\":0}]}\n\n"))
	}()

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
		},
		Body: reader,
	}

	server := &ProxyServer{}
	result := server.handleStreamingResponse(c, resp, newStreamStartGuard(time.Second, func() {}), time.Now())

	if result.Err != nil {
		t.Fatalf("expected successful relay, got %v", result.Err)
	}
	if result.LogicalIssue != nil {
		t.Fatalf("expected final answer stream to be treated as success, got %v", result.LogicalIssue)
	}
}

func TestHandleStreamingResponseAllowsGeminiFunctionCallOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	reader, writer := io.Pipe()
	defer reader.Close()

	go func() {
		defer writer.Close()
		_, _ = writer.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"lookup_weather\",\"args\":{\"city\":\"Shanghai\"}}}],\"role\":\"model\"},\"finishReason\":\"STOP\",\"index\":0}]}\n\n"))
	}()

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
		},
		Body: reader,
	}

	server := &ProxyServer{}
	result := server.handleStreamingResponse(c, resp, newStreamStartGuard(time.Second, func() {}), time.Now())

	if result.Err != nil {
		t.Fatalf("expected successful relay, got %v", result.Err)
	}
	if !result.Committed {
		t.Fatal("expected function call stream to commit")
	}
	if result.LogicalIssue != nil {
		t.Fatalf("expected function call stream to be treated as success, got %v", result.LogicalIssue)
	}
	if !strings.Contains(recorder.Body.String(), "\"functionCall\"") {
		t.Fatalf("expected function call frame to be forwarded, got %q", recorder.Body.String())
	}
}

func TestHandleStreamingResponseCommitsOnOpenAIToolCall(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	reader, writer := io.Pipe()
	defer reader.Close()

	go func() {
		defer writer.Close()
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_123\",\"type\":\"function\",\"function\":{\"name\":\"lookup_weather\",\"arguments\":\"{\\\"city\\\":\\\"Shanghai\\\"}\"}}]}}]}\n\n"))
	}()

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
		},
		Body: reader,
	}

	server := &ProxyServer{}
	result := server.handleStreamingResponse(c, resp, newStreamStartGuard(time.Second, func() {}), time.Now())

	if result.Err != nil {
		t.Fatalf("expected successful relay, got %v", result.Err)
	}
	if !result.Committed {
		t.Fatal("expected tool call stream to commit")
	}
	if !strings.Contains(recorder.Body.String(), "\"tool_calls\"") {
		t.Fatalf("expected tool call frame to be forwarded, got %q", recorder.Body.String())
	}
}

func TestHandleStreamingResponseCommitsOnOpenAIResponsesFunctionCall(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	reader, writer := io.Pipe()
	defer reader.Close()

	go func() {
		defer writer.Close()
		_, _ = writer.Write([]byte("event: response.output_item.added\n"))
		_, _ = writer.Write([]byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"name\":\"lookup_weather\",\"call_id\":\"call_123\",\"arguments\":\"{\\\"city\\\":\\\"Shanghai\\\"}\"}}\n\n"))
	}()

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
		},
		Body: reader,
	}

	server := &ProxyServer{}
	result := server.handleStreamingResponse(c, resp, newStreamStartGuard(time.Second, func() {}), time.Now())

	if result.Err != nil {
		t.Fatalf("expected successful relay, got %v", result.Err)
	}
	if !result.Committed {
		t.Fatal("expected responses function call stream to commit")
	}
	if !strings.Contains(recorder.Body.String(), "\"function_call\"") {
		t.Fatalf("expected function call frame to be forwarded, got %q", recorder.Body.String())
	}
}

func TestHandleStreamingResponseCommitsOnAnthropicToolUse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	reader, writer := io.Pipe()
	defer reader.Close()

	go func() {
		defer writer.Close()
		_, _ = writer.Write([]byte("event: content_block_start\n"))
		_, _ = writer.Write([]byte("data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_123\",\"name\":\"lookup_weather\",\"input\":{}}}\n\n"))
		_, _ = writer.Write([]byte("event: content_block_delta\n"))
		_, _ = writer.Write([]byte("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"city\\\":\\\"Shanghai\\\"}\"}}\n\n"))
	}()

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
		},
		Body: reader,
	}

	server := &ProxyServer{}
	result := server.handleStreamingResponse(c, resp, newStreamStartGuard(time.Second, func() {}), time.Now())

	if result.Err != nil {
		t.Fatalf("expected successful relay, got %v", result.Err)
	}
	if !result.Committed {
		t.Fatal("expected anthropic tool_use stream to commit")
	}
	if !strings.Contains(recorder.Body.String(), "\"tool_use\"") {
		t.Fatalf("expected tool_use frame to be forwarded, got %q", recorder.Body.String())
	}
}

func TestHandleStreamingResponseBuffersMetadataBeforeVisibleOutput(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	reader, writer := io.Pipe()
	defer reader.Close()

	go func() {
		defer writer.Close()
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"))
	}()

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
		},
		Body: reader,
	}

	server := &ProxyServer{}
	result := server.handleStreamingResponse(c, resp, newStreamStartGuard(time.Second, func() {}), time.Now())

	if result.Err != nil {
		t.Fatalf("expected successful relay, got %v", result.Err)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "\"role\":\"assistant\"") {
		t.Fatalf("expected metadata frame to be buffered and forwarded, got %q", body)
	}
	if !strings.Contains(body, "\"content\":\"hello\"") {
		t.Fatalf("expected visible output frame to be forwarded, got %q", body)
	}
}
