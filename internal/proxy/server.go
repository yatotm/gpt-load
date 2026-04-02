// Package proxy provides high-performance OpenAI multi-key proxy server
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gpt-load/internal/channel"
	"gpt-load/internal/config"
	"gpt-load/internal/encryption"
	app_errors "gpt-load/internal/errors"
	"gpt-load/internal/keypool"
	"gpt-load/internal/models"
	"gpt-load/internal/response"
	"gpt-load/internal/services"
	"gpt-load/internal/utils"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// ProxyServer represents the proxy server
type ProxyServer struct {
	keyProvider       *keypool.KeyProvider
	groupManager      *services.GroupManager
	subGroupManager   *services.SubGroupManager
	settingsManager   *config.SystemSettingsManager
	channelFactory    *channel.Factory
	requestLogService *services.RequestLogService
	encryptionSvc     encryption.Service
}

type requestLogOptions struct {
	EffectiveModel        string
	FirstVisibleLatencyMs *int64
}

// NewProxyServer creates a new proxy server
func NewProxyServer(
	keyProvider *keypool.KeyProvider,
	groupManager *services.GroupManager,
	subGroupManager *services.SubGroupManager,
	settingsManager *config.SystemSettingsManager,
	channelFactory *channel.Factory,
	requestLogService *services.RequestLogService,
	encryptionSvc encryption.Service,
) (*ProxyServer, error) {
	return &ProxyServer{
		keyProvider:       keyProvider,
		groupManager:      groupManager,
		subGroupManager:   subGroupManager,
		settingsManager:   settingsManager,
		channelFactory:    channelFactory,
		requestLogService: requestLogService,
		encryptionSvc:     encryptionSvc,
	}, nil
}

// HandleProxy is the main entry point for proxy requests, refactored based on the stable .bak logic.
func (ps *ProxyServer) HandleProxy(c *gin.Context) {
	startTime := time.Now()
	groupName := c.Param("group_name")

	originalGroup, err := ps.groupManager.GetGroupByName(groupName)
	if err != nil {
		response.Error(c, app_errors.ParseDBError(err))
		return
	}

	// Select sub-group if this is an aggregate group
	subGroupName, err := ps.subGroupManager.SelectSubGroup(originalGroup)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"aggregate_group": originalGroup.Name,
			"error":           err,
		}).Error("Failed to select sub-group from aggregate")
		response.Error(c, app_errors.NewAPIError(app_errors.ErrNoKeysAvailable, "No available sub-groups"))
		return
	}

	group := originalGroup
	if subGroupName != "" {
		group, err = ps.groupManager.GetGroupByName(subGroupName)
		if err != nil {
			response.Error(c, app_errors.ParseDBError(err))
			return
		}
	}

	channelHandler, err := ps.channelFactory.GetChannel(group)
	if err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInternalServer, fmt.Sprintf("Failed to get channel for group '%s': %v", groupName, err)))
		return
	}

	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		logrus.Errorf("Failed to read request body: %v", err)
		response.Error(c, app_errors.NewAPIError(app_errors.ErrBadRequest, "Failed to read request body"))
		return
	}
	c.Request.Body.Close()

	finalBodyBytes, err := ps.applyParamOverrides(bodyBytes, group)
	if err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInternalServer, fmt.Sprintf("Failed to apply parameter overrides: %v", err)))
		return
	}

	isStream := channelHandler.IsStreamRequest(c, bodyBytes)

	ps.executeRequestWithRetry(c, channelHandler, originalGroup, group, finalBodyBytes, isStream, startTime, 0)
}

// executeRequestWithRetry is the core recursive function for handling requests and retries.
func (ps *ProxyServer) executeRequestWithRetry(
	c *gin.Context,
	channelHandler channel.ChannelProxy,
	originalGroup *models.Group,
	group *models.Group,
	bodyBytes []byte,
	isStream bool,
	startTime time.Time,
	retryCount int,
) {
	cfg := group.EffectiveConfig

	apiKey, err := ps.keyProvider.SelectKey(group.ID)
	if err != nil {
		logrus.Errorf("Failed to select a key for group %s on attempt %d: %v", group.Name, retryCount+1, err)
		response.Error(c, app_errors.NewAPIError(app_errors.ErrNoKeysAvailable, err.Error()))
		ps.logRequest(c, originalGroup, group, nil, startTime, http.StatusServiceUnavailable, err, isStream, "", channelHandler, bodyBytes, models.RequestTypeFinal, nil)
		return
	}

	upstreamURL, err := channelHandler.BuildUpstreamURL(c.Request.URL, originalGroup.Name)
	if err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInternalServer, fmt.Sprintf("Failed to build upstream URL: %v", err)))
		return
	}

	previewReq, err := http.NewRequest(c.Request.Method, upstreamURL, bytes.NewReader(bodyBytes))
	if err != nil {
		logrus.Errorf("Failed to create upstream request: %v", err)
		response.Error(c, app_errors.ErrInternalServer)
		return
	}
	previewReq.Header = c.Request.Header.Clone()

	// Clean up client auth key
	previewReq.Header.Del("Authorization")
	previewReq.Header.Del("X-Api-Key")
	previewReq.Header.Del("X-Goog-Api-Key")

	// Apply model redirection
	finalBodyBytes, err := channelHandler.ApplyModelRedirect(previewReq, bodyBytes, group)
	effectiveModel := extractModelFromRequest(channelHandler, previewReq, finalBodyBytes)
	logOptions := &requestLogOptions{
		EffectiveModel: effectiveModel,
	}
	if err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrBadRequest, err.Error()))
		ps.logRequest(c, originalGroup, group, apiKey, startTime, http.StatusBadRequest, err, isStream, previewReq.URL.String(), channelHandler, bodyBytes, models.RequestTypeFinal, logOptions)
		return
	}

	var ctx context.Context
	var cancel context.CancelFunc
	var streamStartGuard *streamStartGuard
	if isStream {
		timeoutSeconds := resolveStreamFirstVisibleTimeoutSeconds(group, effectiveModel)
		ctx, cancel = context.WithCancel(c.Request.Context())
		streamStartGuard = newStreamStartGuard(
			time.Duration(timeoutSeconds)*time.Second,
			cancel,
		)
	} else {
		timeout := time.Duration(cfg.RequestTimeout) * time.Second
		ctx, cancel = context.WithTimeout(c.Request.Context(), timeout)
	}
	defer cancel()
	if streamStartGuard != nil {
		defer streamStartGuard.Stop()
	}

	req, err := http.NewRequestWithContext(ctx, c.Request.Method, previewReq.URL.String(), bytes.NewReader(finalBodyBytes))
	if err != nil {
		logrus.Errorf("Failed to create upstream request: %v", err)
		response.Error(c, app_errors.ErrInternalServer)
		return
	}
	req.ContentLength = int64(len(finalBodyBytes))
	req.Header = c.Request.Header.Clone()
	req.Header.Del("Authorization")
	req.Header.Del("X-Api-Key")
	req.Header.Del("X-Goog-Api-Key")

	channelHandler.ModifyRequest(req, apiKey, group)

	// Apply custom header rules
	if len(group.HeaderRuleList) > 0 {
		headerCtx := utils.NewHeaderVariableContextFromGin(c, group, apiKey)
		utils.ApplyHeaderRules(req, group.HeaderRuleList, headerCtx)
	}

	var client *http.Client
	if isStream {
		client = channelHandler.GetStreamClient()
		req.Header.Set("X-Accel-Buffering", "no")
	} else {
		client = channelHandler.GetHTTPClient()
	}

	attemptStart := time.Now()
	resp, err := client.Do(req)
	if resp != nil {
		defer resp.Body.Close()
	}
	if streamStartGuard != nil && streamStartGuard.TimedOut() {
		err = newStreamFirstVisibleTimeoutError(streamStartGuard.Timeout())
	}

	// Unified error handling for retries. Exclude 404 from being a retryable error.
	if err != nil || (resp != nil && resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotFound) {
		if err != nil && !isStreamFirstVisibleTimeoutError(err) && app_errors.IsIgnorableError(err) {
			logrus.Debugf("Client-side ignorable error for key %s, aborting retries: %v", utils.MaskAPIKey(apiKey.KeyValue), err)
			ps.logRequest(c, originalGroup, group, apiKey, startTime, 499, err, isStream, req.URL.String(), channelHandler, bodyBytes, models.RequestTypeFinal, logOptions)
			return
		}

		var statusCode int
		var errorMessage string
		var parsedError string

		if err != nil {
			statusCode = http.StatusInternalServerError
			if isStreamFirstVisibleTimeoutError(err) {
				statusCode = http.StatusGatewayTimeout
			}
			errorMessage = err.Error()
			parsedError = errorMessage
			logrus.Debugf("Request failed (attempt %d/%d) for key %s: %v", retryCount+1, cfg.MaxRetries, utils.MaskAPIKey(apiKey.KeyValue), err)
		} else {
			// HTTP-level error (status >= 400)
			statusCode = resp.StatusCode
			errorBody, readErr := io.ReadAll(resp.Body)
			if readErr != nil {
				logrus.Errorf("Failed to read error body: %v", readErr)
				errorBody = []byte("Failed to read error body")
			}

			errorBody = handleGzipCompression(resp, errorBody)
			errorMessage = string(errorBody)
			parsedError = app_errors.ParseUpstreamError(errorBody)
			logrus.Debugf("Request failed with status %d (attempt %d/%d) for key %s. Parsed Error: %s", statusCode, retryCount+1, cfg.MaxRetries, utils.MaskAPIKey(apiKey.KeyValue), parsedError)
		}

		// 使用解析后的错误信息更新密钥状态
		ps.keyProvider.UpdateRequestFailure(apiKey, group, parsedError)

		// 判断是否为最后一次尝试
		isLastAttempt := retryCount >= cfg.MaxRetries
		requestType := models.RequestTypeRetry
		if isLastAttempt {
			requestType = models.RequestTypeFinal
		}

		ps.logRequest(c, originalGroup, group, apiKey, startTime, statusCode, errors.New(parsedError), isStream, req.URL.String(), channelHandler, bodyBytes, requestType, logOptions)

		// 如果是最后一次尝试，直接返回错误，不再递归
		if isLastAttempt {
			var errorJSON map[string]any
			if err := json.Unmarshal([]byte(errorMessage), &errorJSON); err == nil {
				c.JSON(statusCode, errorJSON)
			} else {
				response.Error(c, app_errors.NewAPIErrorWithUpstream(statusCode, "UPSTREAM_ERROR", errorMessage))
			}
			return
		}

		ps.executeRequestWithRetry(c, channelHandler, originalGroup, group, bodyBytes, isStream, startTime, retryCount+1)
		return
	}

	logrus.Debugf("Request for group %s succeeded on attempt %d with key %s", group.Name, retryCount+1, utils.MaskAPIKey(apiKey.KeyValue))

	// Check if this is a model list request (needs special handling)
	if shouldInterceptModelList(c.Request.URL.Path, c.Request.Method) {
		ps.handleModelListResponse(c, resp, group, channelHandler)
	} else {
		if isStream {
			streamResult := ps.handleStreamingResponse(c, resp, streamStartGuard, attemptStart)
			if streamResult.Err != nil {
				if app_errors.IsIgnorableError(streamResult.Err) {
					ps.logRequest(c, originalGroup, group, apiKey, startTime, 499, streamResult.Err, isStream, req.URL.String(), channelHandler, bodyBytes, models.RequestTypeFinal, logOptions)
					return
				}
				if streamResult.Retryable {
					ps.keyProvider.UpdateRequestFailure(apiKey, group, streamResult.Err.Error())
					statusCode := streamRetryableStatusCode(streamResult.Err)

					isLastAttempt := retryCount >= cfg.MaxRetries
					requestType := models.RequestTypeRetry
					if isLastAttempt {
						requestType = models.RequestTypeFinal
					}

					ps.logRequest(c, originalGroup, group, apiKey, startTime, statusCode, streamResult.Err, isStream, req.URL.String(), channelHandler, bodyBytes, requestType, logOptions)

					if isLastAttempt {
						response.Error(c, app_errors.NewAPIErrorWithUpstream(statusCode, "UPSTREAM_TIMEOUT", streamResult.Err.Error()))
						return
					}

					ps.executeRequestWithRetry(c, channelHandler, originalGroup, group, bodyBytes, isStream, startTime, retryCount+1)
					return
				}

				statusCode := resp.StatusCode
				if app_errors.IsIgnorableError(streamResult.Err) {
					statusCode = 499
				}
				ps.logRequest(c, originalGroup, group, apiKey, startTime, statusCode, streamResult.Err, isStream, req.URL.String(), channelHandler, bodyBytes, models.RequestTypeFinal, logOptions)
				return
			}
			logOptions.FirstVisibleLatencyMs = durationMillisPtr(streamResult.FirstVisibleLatency)
		} else {
			ps.writeResponseHeaders(c, resp)
			ps.handleNormalResponse(c, resp)
		}
	}

	ps.keyProvider.UpdateRequestSuccess(apiKey, group)
	ps.logRequest(c, originalGroup, group, apiKey, startTime, resp.StatusCode, nil, isStream, req.URL.String(), channelHandler, bodyBytes, models.RequestTypeFinal, logOptions)
}

// logRequest is a helper function to create and record a request log.
func (ps *ProxyServer) logRequest(
	c *gin.Context,
	originalGroup *models.Group,
	group *models.Group,
	apiKey *models.APIKey,
	startTime time.Time,
	statusCode int,
	finalError error,
	isStream bool,
	upstreamAddr string,
	channelHandler channel.ChannelProxy,
	bodyBytes []byte,
	requestType string,
	options *requestLogOptions,
) {
	if ps.requestLogService == nil {
		return
	}

	var requestBodyToLog, userAgent string

	if group.EffectiveConfig.EnableRequestBodyLogging {
		requestBodyToLog = utils.TruncateString(string(bodyBytes), 65000)
		userAgent = c.Request.UserAgent()
	}

	duration := time.Since(startTime).Milliseconds()

	logEntry := &models.RequestLog{
		GroupID:      group.ID,
		GroupName:    group.Name,
		IsSuccess:    finalError == nil && statusCode < 400,
		SourceIP:     c.ClientIP(),
		StatusCode:   statusCode,
		RequestPath:  utils.TruncateString(c.Request.URL.String(), 500),
		Duration:     duration,
		UserAgent:    userAgent,
		RequestType:  requestType,
		IsStream:     isStream,
		UpstreamAddr: utils.TruncateString(upstreamAddr, 500),
		RequestBody:  requestBodyToLog,
	}

	// Set parent group
	if originalGroup != nil && originalGroup.GroupType == "aggregate" && originalGroup.ID != group.ID {
		logEntry.ParentGroupID = originalGroup.ID
		logEntry.ParentGroupName = originalGroup.Name
	}

	if channelHandler != nil && bodyBytes != nil {
		logEntry.Model = channelHandler.ExtractModel(c, bodyBytes)
	}
	if options != nil {
		logEntry.EffectiveModel = strings.TrimSpace(options.EffectiveModel)
	}
	if logEntry.EffectiveModel == "" {
		logEntry.EffectiveModel = logEntry.Model
	}
	if logEntry.IsSuccess && logEntry.IsStream && options != nil && options.FirstVisibleLatencyMs != nil {
		logEntry.FirstVisibleLatencyMs = options.FirstVisibleLatencyMs
	}

	if apiKey != nil {
		// 加密密钥值用于日志存储
		encryptedKeyValue, err := ps.encryptionSvc.Encrypt(apiKey.KeyValue)
		if err != nil {
			logrus.WithError(err).Error("Failed to encrypt key value for logging")
			logEntry.KeyValue = "failed-to-encryption"
		} else {
			logEntry.KeyValue = encryptedKeyValue
		}
		// 添加 KeyHash 用于反查
		logEntry.KeyHash = ps.encryptionSvc.Hash(apiKey.KeyValue)
	}

	if finalError != nil {
		logEntry.ErrorMessage = finalError.Error()
	}

	if err := ps.requestLogService.Record(logEntry); err != nil {
		logrus.Errorf("Failed to record request log: %v", err)
	}
}

func extractModelFromRequest(channelHandler channel.ChannelProxy, req *http.Request, bodyBytes []byte) string {
	if channelHandler == nil || req == nil {
		return ""
	}
	tmpContext := &gin.Context{Request: req}
	return strings.TrimSpace(channelHandler.ExtractModel(tmpContext, bodyBytes))
}

func resolveStreamFirstVisibleTimeoutSeconds(group *models.Group, effectiveModel string) int {
	if group == nil {
		return 0
	}
	defaultTimeout := group.EffectiveConfig.StreamFirstVisibleTimeoutSeconds
	if len(group.StreamTimeoutRules) == 0 {
		return defaultTimeout
	}

	normalizedModel := strings.TrimSpace(effectiveModel)
	if normalizedModel == "" {
		return defaultTimeout
	}

	longestPrefixLen := -1
	longestPrefixTimeout := defaultTimeout
	for rawKey, rawValue := range group.StreamTimeoutRules {
		rule := strings.TrimSpace(rawKey)
		timeout, ok := jsonNumberToInt(rawValue)
		if !ok {
			continue
		}
		if strings.HasSuffix(rule, "*") {
			prefix := strings.TrimSuffix(rule, "*")
			if strings.HasPrefix(normalizedModel, prefix) && len(prefix) > longestPrefixLen {
				longestPrefixLen = len(prefix)
				longestPrefixTimeout = timeout
			}
			continue
		}
		if rule == normalizedModel {
			return timeout
		}
	}

	if longestPrefixLen >= 0 {
		return longestPrefixTimeout
	}
	return defaultTimeout
}

func jsonNumberToInt(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int8:
		return int(v), true
	case int16:
		return int(v), true
	case int32:
		return int(v), true
	case int64:
		return int(v), true
	case uint:
		return int(v), true
	case uint8:
		return int(v), true
	case uint16:
		return int(v), true
	case uint32:
		return int(v), true
	case uint64:
		if v > uint64(^uint(0)>>1) {
			return 0, false
		}
		return int(v), true
	case float32:
		if float32(int(v)) != v {
			return 0, false
		}
		return int(v), true
	case float64:
		if float64(int(v)) != v {
			return 0, false
		}
		return int(v), true
	case json.Number:
		number, err := v.Int64()
		if err != nil {
			return 0, false
		}
		if int64(int(number)) != number {
			return 0, false
		}
		return int(number), true
	case string:
		number, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, false
		}
		return number, true
	default:
		return 0, false
	}
}

func durationMillisPtr(duration time.Duration) *int64 {
	if duration <= 0 {
		return nil
	}
	milliseconds := duration.Milliseconds()
	return &milliseconds
}
