package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

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
	result := server.handleStreamingResponse(c, resp, newStreamStartGuard(time.Second, func() {}), time.Now())

	if result.Err != nil {
		t.Fatalf("expected successful relay, got %v", result.Err)
	}
	if !result.Committed {
		t.Fatal("expected stream to be committed after thought output")
	}
	if result.FirstVisibleLatency <= 0 {
		t.Fatalf("expected first visible latency to be recorded, got %v", result.FirstVisibleLatency)
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
