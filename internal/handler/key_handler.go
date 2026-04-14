package handler

import (
	"fmt"
	app_errors "gpt-load/internal/errors"
	"gpt-load/internal/keypool"
	"gpt-load/internal/models"
	"gpt-load/internal/response"
	"gpt-load/internal/services"
	"io"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

func (s *Server) handleKeyError(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}

	if svcErr, ok := err.(*services.I18nError); ok {
		if svcErr == nil {
			return false
		}
		if svcErr.Template != nil {
			response.ErrorI18nFromAPIError(c, svcErr.APIError, svcErr.MessageID, svcErr.Template)
		} else {
			response.ErrorI18nFromAPIError(c, svcErr.APIError, svcErr.MessageID)
		}
		return true
	}

	if apiErr, ok := err.(*app_errors.APIError); ok {
		response.Error(c, apiErr)
		return true
	}

	return false
}

// validateGroupIDFromQuery validates and parses group ID from a query parameter.
// Returns 0 and false if validation fails (error is already sent to client)
func validateGroupIDFromQuery(c *gin.Context) (uint, bool) {
	groupIDStr := c.Query("group_id")
	if groupIDStr == "" {
		response.ErrorI18nFromAPIError(c, app_errors.ErrBadRequest, "validation.group_id_required")
		return 0, false
	}

	groupID, err := strconv.Atoi(groupIDStr)
	if err != nil || groupID <= 0 {
		response.ErrorI18nFromAPIError(c, app_errors.ErrBadRequest, "validation.invalid_group_id_format")
		return 0, false
	}

	return uint(groupID), true
}

// validateKeysText validates the keys text input
// Returns false if validation fails (error is already sent to client)
func validateKeysText(c *gin.Context, keysText string) bool {
	if strings.TrimSpace(keysText) == "" {
		response.ErrorI18nFromAPIError(c, app_errors.ErrValidation, "validation.keys_text_empty")
		return false
	}

	return true
}

func isAllowedKeyListStatusFilter(status string) bool {
	switch status {
	case "", models.KeyStatusActive, models.KeyStatusInvalid, models.KeyStatusPaused, models.KeyStatusDisabled:
		return true
	default:
		return false
	}
}

func (s *Server) fillKeyProbeDisplayState(group *models.Group, key *models.APIKey) {
	if group == nil || key == nil || key.Status != models.KeyStatusPaused || s.SettingsManager == nil || s.KeyProvider == nil {
		return
	}

	effectiveConfig := s.SettingsManager.GetEffectiveKeyConfig(group.Config, key.Config)
	stats, err := s.KeyProvider.GetProbeWindowStats(key.ID, effectiveConfig.ActiveProbeWindowMinutes, time.Now())
	if err != nil {
		logrus.WithError(err).WithField("key_id", key.ID).Warn("Failed to load probe window state for paused key")
		return
	}

	decision := keypool.DecideProbeStatusForDisplay(stats, float64(effectiveConfig.ActiveProbeFailureRateLimit))
	key.ProbeOverLimit = decision.ShouldBlacklist
}

// findGroupByID is a helper function to find a group by its ID.
func (s *Server) findGroupByID(c *gin.Context, groupID uint) (*models.Group, bool) {
	var group models.Group
	if err := s.DB.First(&group, groupID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			response.Error(c, app_errors.ErrResourceNotFound)
		} else {
			response.Error(c, app_errors.ParseDBError(err))
		}
		return nil, false
	}
	return &group, true
}

// KeyTextRequest defines a generic payload for operations requiring a group ID and a text block of keys.
type KeyTextRequest struct {
	GroupID  uint   `json:"group_id" binding:"required"`
	KeysText string `json:"keys_text" binding:"required"`
}

type AddKeysRequest struct {
	GroupID  uint   `json:"group_id" binding:"required"`
	KeysText string `json:"keys_text" binding:"required"`
	Priority int    `json:"priority"`
}

// GroupIDRequest defines a generic payload for operations requiring only a group ID.
type GroupIDRequest struct {
	GroupID uint `json:"group_id" binding:"required"`
}

// ValidateGroupKeysRequest defines the payload for validating keys in a group.
type ValidateGroupKeysRequest struct {
	GroupID uint   `json:"group_id" binding:"required"`
	Status  string `json:"status,omitempty"`
}

// AddMultipleKeys handles creating new keys from a text block within a specific group.
func (s *Server) AddMultipleKeys(c *gin.Context) {
	var req AddKeysRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInvalidJSON, err.Error()))
		return
	}

	if _, ok := s.findGroupByID(c, req.GroupID); !ok {
		return
	}

	if !validateKeysText(c, req.KeysText) {
		return
	}

	if req.Priority < 0 {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrValidation, "priority must be greater than 0"))
		return
	}

	result, err := s.KeyService.AddMultipleKeys(req.GroupID, req.KeysText, req.Priority)
	if err != nil {
		if strings.Contains(err.Error(), "batch size exceeds the limit") {
			response.Error(c, app_errors.NewAPIError(app_errors.ErrValidation, err.Error()))
		} else if err.Error() == "no valid keys found in the input text" {
			response.Error(c, app_errors.NewAPIError(app_errors.ErrValidation, err.Error()))
		} else {
			response.Error(c, app_errors.ParseDBError(err))
		}
		return
	}

	response.Success(c, result)
}

// AddMultipleKeysAsync handles creating new keys from a text block or file within a specific group.
func (s *Server) AddMultipleKeysAsync(c *gin.Context) {
	var groupID uint
	var keysText string
	priority := models.DefaultAPIKeyPriority

	// Check content type to determine if it's a file upload or JSON request
	contentType := c.ContentType()

	if strings.Contains(contentType, "multipart/form-data") {
		// Handle file upload
		groupIDStr := c.PostForm("group_id")
		if groupIDStr == "" {
			response.ErrorI18nFromAPIError(c, app_errors.ErrBadRequest, "validation.group_id_required")
			return
		}

		groupIDInt, err := strconv.Atoi(groupIDStr)
		if err != nil || groupIDInt <= 0 {
			response.ErrorI18nFromAPIError(c, app_errors.ErrBadRequest, "validation.invalid_group_id_format")
			return
		}
		groupID = uint(groupIDInt)

		if priorityStr := strings.TrimSpace(c.PostForm("priority")); priorityStr != "" {
			priorityInt, err := strconv.Atoi(priorityStr)
			if err != nil || priorityInt <= 0 {
				response.Error(c, app_errors.NewAPIError(app_errors.ErrValidation, "priority must be greater than 0"))
				return
			}
			priority = priorityInt
		}

		// Get uploaded file
		file, err := c.FormFile("file")
		if err != nil {
			response.ErrorI18nFromAPIError(c, app_errors.ErrBadRequest, "validation.file_required")
			return
		}

		// Validate file extension
		ext := strings.ToLower(filepath.Ext(file.Filename))
		if ext != ".txt" {
			response.ErrorI18nFromAPIError(c, app_errors.ErrValidation, "validation.only_txt_supported")
			return
		}

		// Read file content
		fileContent, err := file.Open()
		if err != nil {
			response.ErrorI18nFromAPIError(c, app_errors.ErrBadRequest, "validation.failed_to_open_file")
			return
		}
		defer fileContent.Close()

		// Read file content as string using io.ReadAll
		buf, err := io.ReadAll(fileContent)
		if err != nil {
			response.ErrorI18nFromAPIError(c, app_errors.ErrBadRequest, "validation.failed_to_read_file")
			return
		}
		keysText = string(buf)
	} else {
		// Handle JSON request (original behavior)
		var req AddKeysRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			response.Error(c, app_errors.NewAPIError(app_errors.ErrInvalidJSON, err.Error()))
			return
		}
		groupID = req.GroupID
		keysText = req.KeysText
		if req.Priority < 0 {
			response.Error(c, app_errors.NewAPIError(app_errors.ErrValidation, "priority must be greater than 0"))
			return
		}
		if req.Priority > 0 {
			priority = req.Priority
		}
	}

	group, ok := s.findGroupByID(c, groupID)
	if !ok {
		return
	}

	if !validateKeysText(c, keysText) {
		return
	}

	taskStatus, err := s.KeyImportService.StartImportTaskWithPriority(group, keysText, priority)
	if err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrTaskInProgress, err.Error()))
		return
	}

	response.Success(c, taskStatus)
}

// ListKeysInGroup handles listing all keys within a specific group with pagination.
func (s *Server) ListKeysInGroup(c *gin.Context) {
	groupID, ok := validateGroupIDFromQuery(c)
	if !ok {
		return
	}

	group, ok := s.findGroupByID(c, groupID)
	if !ok {
		return
	}

	statusFilter := c.Query("status")
	if !isAllowedKeyListStatusFilter(statusFilter) {
		response.ErrorI18nFromAPIError(c, app_errors.ErrValidation, "validation.invalid_status_filter")
		return
	}

	searchKeyword := c.Query("key_value")
	searchHash := ""
	if searchKeyword != "" {
		searchHash = s.EncryptionSvc.Hash(searchKeyword)
	}

	query := s.KeyService.ListKeysInGroupQuery(groupID, statusFilter, searchHash)

	var keys []models.APIKey
	paginatedResult, err := response.Paginate(c, query, &keys)
	if err != nil {
		response.Error(c, app_errors.ParseDBError(err))
		return
	}

	// Decrypt all keys for display
	for i := range keys {
		if keys[i].Priority <= 0 {
			keys[i].Priority = models.DefaultAPIKeyPriority
		}
		decryptedValue, err := s.EncryptionSvc.Decrypt(keys[i].KeyValue)
		if err != nil {
			logrus.WithError(err).WithField("key_id", keys[i].ID).Error("Failed to decrypt key value for listing")
			keys[i].KeyValue = "failed-to-decrypt"
		} else {
			keys[i].KeyValue = decryptedValue
		}
		s.fillKeyProbeDisplayState(group, &keys[i])
	}
	paginatedResult.Items = keys

	response.Success(c, paginatedResult)
}

// DeleteMultipleKeys handles deleting keys from a text block within a specific group.
func (s *Server) DeleteMultipleKeys(c *gin.Context) {
	var req KeyTextRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInvalidJSON, err.Error()))
		return
	}

	if _, ok := s.findGroupByID(c, req.GroupID); !ok {
		return
	}

	if !validateKeysText(c, req.KeysText) {
		return
	}

	result, err := s.KeyService.DeleteMultipleKeys(req.GroupID, req.KeysText)
	if err != nil {
		if strings.Contains(err.Error(), "batch size exceeds the limit") {
			response.Error(c, app_errors.NewAPIError(app_errors.ErrValidation, err.Error()))
		} else if err.Error() == "no valid keys found in the input text" {
			response.Error(c, app_errors.NewAPIError(app_errors.ErrValidation, err.Error()))
		} else {
			response.Error(c, app_errors.ParseDBError(err))
		}
		return
	}

	response.Success(c, result)
}

// DeleteMultipleKeysAsync handles deleting keys from a text block within a specific group using async task.
func (s *Server) DeleteMultipleKeysAsync(c *gin.Context) {
	var req KeyTextRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInvalidJSON, err.Error()))
		return
	}

	group, ok := s.findGroupByID(c, req.GroupID)
	if !ok {
		return
	}

	if !validateKeysText(c, req.KeysText) {
		return
	}

	taskStatus, err := s.KeyDeleteService.StartDeleteTask(group, req.KeysText)
	if err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrTaskInProgress, err.Error()))
		return
	}

	response.Success(c, taskStatus)
}

// RestoreMultipleKeys handles restoring keys from a text block within a specific group.
func (s *Server) RestoreMultipleKeys(c *gin.Context) {
	var req KeyTextRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInvalidJSON, err.Error()))
		return
	}

	if _, ok := s.findGroupByID(c, req.GroupID); !ok {
		return
	}

	if !validateKeysText(c, req.KeysText) {
		return
	}

	result, err := s.KeyService.RestoreMultipleKeys(req.GroupID, req.KeysText)
	if err != nil {
		if strings.Contains(err.Error(), "batch size exceeds the limit") {
			response.Error(c, app_errors.NewAPIError(app_errors.ErrValidation, err.Error()))
		} else if err.Error() == "no valid keys found in the input text" {
			response.Error(c, app_errors.NewAPIError(app_errors.ErrValidation, err.Error()))
		} else {
			response.Error(c, app_errors.ParseDBError(err))
		}
		return
	}

	response.Success(c, result)
}

// TestMultipleKeys handles a one-off validation test for multiple keys.
func (s *Server) TestMultipleKeys(c *gin.Context) {
	var req KeyTextRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInvalidJSON, err.Error()))
		return
	}

	groupDB, ok := s.findGroupByID(c, req.GroupID)
	if !ok {
		return
	}

	group, err := s.GroupManager.GetGroupByName(groupDB.Name)
	if err != nil {
		response.ErrorI18nFromAPIError(c, app_errors.ErrResourceNotFound, "validation.group_not_found")
		return
	}

	if !validateKeysText(c, req.KeysText) {
		return
	}

	start := time.Now()
	results, err := s.KeyService.TestMultipleKeys(group, req.KeysText)
	duration := time.Since(start).Milliseconds()
	if err != nil {
		if strings.Contains(err.Error(), "batch size exceeds the limit") {
			response.Error(c, app_errors.NewAPIError(app_errors.ErrValidation, err.Error()))
		} else if err.Error() == "no valid keys found in the input text" {
			response.Error(c, app_errors.NewAPIError(app_errors.ErrValidation, err.Error()))
		} else {
			response.Error(c, app_errors.ParseDBError(err))
		}
		return
	}

	response.Success(c, gin.H{
		"results":        results,
		"total_duration": duration,
	})
}

// ValidateGroupKeys initiates a manual validation task for all keys in a group.
func (s *Server) ValidateGroupKeys(c *gin.Context) {
	var req ValidateGroupKeysRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInvalidJSON, err.Error()))
		return
	}

	// Validate status if provided
	if req.Status != "" && req.Status != models.KeyStatusActive && req.Status != models.KeyStatusInvalid {
		response.ErrorI18nFromAPIError(c, app_errors.ErrValidation, "validation.invalid_status_value")
		return
	}

	groupDB, ok := s.findGroupByID(c, req.GroupID)
	if !ok {
		return
	}

	group, err := s.GroupManager.GetGroupByName(groupDB.Name)
	if err != nil {
		response.ErrorI18nFromAPIError(c, app_errors.ErrResourceNotFound, "validation.group_not_found")
		return
	}

	taskStatus, err := s.KeyManualValidationService.StartValidationTask(group, req.Status)
	if err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrTaskInProgress, err.Error()))
		return
	}

	response.Success(c, taskStatus)
}

// RestoreAllInvalidKeys sets the status of all 'inactive' keys in a group to 'active'.
func (s *Server) RestoreAllInvalidKeys(c *gin.Context) {
	var req GroupIDRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInvalidJSON, err.Error()))
		return
	}

	if _, ok := s.findGroupByID(c, req.GroupID); !ok {
		return
	}

	rowsAffected, err := s.KeyService.RestoreAllInvalidKeys(req.GroupID)
	if err != nil {
		response.Error(c, app_errors.ParseDBError(err))
		return
	}

	response.SuccessI18n(c, "success.keys_restored", nil, map[string]any{"count": rowsAffected})
}

// ClearAllInvalidKeys deletes all 'inactive' keys from a group.
func (s *Server) ClearAllInvalidKeys(c *gin.Context) {
	var req GroupIDRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInvalidJSON, err.Error()))
		return
	}

	if _, ok := s.findGroupByID(c, req.GroupID); !ok {
		return
	}

	rowsAffected, err := s.KeyService.ClearAllInvalidKeys(req.GroupID)
	if err != nil {
		response.Error(c, app_errors.ParseDBError(err))
		return
	}

	response.SuccessI18n(c, "success.invalid_keys_cleared", nil, map[string]any{"count": rowsAffected})
}

// ClearAllKeys deletes all keys from a group.
func (s *Server) ClearAllKeys(c *gin.Context) {
	var req GroupIDRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInvalidJSON, err.Error()))
		return
	}

	if _, ok := s.findGroupByID(c, req.GroupID); !ok {
		return
	}

	rowsAffected, err := s.KeyService.ClearAllKeys(req.GroupID)
	if err != nil {
		response.Error(c, app_errors.ParseDBError(err))
		return
	}

	response.SuccessI18n(c, "success.all_keys_cleared", nil, map[string]any{"count": rowsAffected})
}

// ExportKeys handles exporting keys to a text file.
func (s *Server) ExportKeys(c *gin.Context) {
	groupID, ok := validateGroupIDFromQuery(c)
	if !ok {
		return
	}

	statusFilter := c.Query("status")
	if statusFilter == "" {
		statusFilter = "all"
	}

	switch statusFilter {
	case "all", models.KeyStatusActive, models.KeyStatusInvalid, models.KeyStatusPaused, models.KeyStatusDisabled:
	default:
		response.ErrorI18nFromAPIError(c, app_errors.ErrValidation, "validation.invalid_status_filter")
		return
	}

	group, ok := s.findGroupByID(c, groupID)
	if !ok {
		return
	}

	filename := fmt.Sprintf("keys-%s-%s.txt", group.Name, statusFilter)
	c.Header("Content-Disposition", "attachment; filename="+filename)
	c.Header("Content-Type", "text/plain; charset=utf-8")

	if err := s.KeyService.StreamKeysToWriter(groupID, statusFilter, c.Writer); err != nil {
		log.Printf("Failed to stream keys: %v", err)
	}
}

// UpdateKeyNotesRequest defines the payload for updating a key's notes.
type UpdateKeyNotesRequest struct {
	Notes string `json:"notes"`
}

type UpdateKeyPriorityRequest struct {
	Priority int `json:"priority" binding:"required"`
}

type UpdateKeyRequest struct {
	Notes               *string        `json:"notes"`
	Priority            *int           `json:"priority"`
	Status              *string        `json:"status"`
	Config              map[string]any `json:"config"`
	ProbeParamOverrides map[string]any `json:"probe_param_overrides"`
}

// UpdateKeyNotes handles updating the notes of a specific API key.
func (s *Server) UpdateKeyNotes(c *gin.Context) {
	keyIDStr := c.Param("id")
	keyID, err := strconv.Atoi(keyIDStr)
	if err != nil || keyID <= 0 {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrBadRequest, "invalid key ID format"))
		return
	}

	var req UpdateKeyNotesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInvalidJSON, err.Error()))
		return
	}

	// Normalize and enforce length explicitly
	req.Notes = strings.TrimSpace(req.Notes)
	if utf8.RuneCountInString(req.Notes) > 255 {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrValidation, "notes length must be <= 255 characters"))
		return
	}

	// Check if the key exists and update its notes
	var key models.APIKey
	if err := s.DB.First(&key, keyID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			response.Error(c, app_errors.ErrResourceNotFound)
		} else {
			response.Error(c, app_errors.ParseDBError(err))
		}
		return
	}

	// Update notes
	if err := s.DB.Model(&key).Update("notes", req.Notes).Error; err != nil {
		response.Error(c, app_errors.ParseDBError(err))
		return
	}

	response.Success(c, nil)
}

func (s *Server) UpdateKeyPriority(c *gin.Context) {
	keyIDStr := c.Param("id")
	keyID, err := strconv.Atoi(keyIDStr)
	if err != nil || keyID <= 0 {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrBadRequest, "invalid key ID format"))
		return
	}

	var req UpdateKeyPriorityRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInvalidJSON, err.Error()))
		return
	}

	if req.Priority <= 0 {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrValidation, "priority must be greater than 0"))
		return
	}

	key, err := s.KeyProvider.UpdateKeyPriority(uint(keyID), req.Priority)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			response.Error(c, app_errors.ErrResourceNotFound)
		} else {
			response.Error(c, app_errors.ParseDBError(err))
		}
		return
	}

	response.Success(c, key)
}

func (s *Server) UpdateKey(c *gin.Context) {
	keyIDStr := c.Param("id")
	keyID, err := strconv.Atoi(keyIDStr)
	if err != nil || keyID <= 0 {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrBadRequest, "invalid key ID format"))
		return
	}

	var req UpdateKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInvalidJSON, err.Error()))
		return
	}

	if req.Notes != nil && utf8.RuneCountInString(strings.TrimSpace(*req.Notes)) > 255 {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrValidation, "notes length must be <= 255 characters"))
		return
	}
	if req.Priority != nil && *req.Priority <= 0 {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrValidation, "priority must be greater than 0"))
		return
	}
	if req.Status != nil && !models.IsManuallySwitchableKeyStatus(*req.Status) {
		response.ErrorI18nFromAPIError(c, app_errors.ErrValidation, "validation.invalid_status_value")
		return
	}

	key, err := s.KeyService.UpdateKey(c.Request.Context(), uint(keyID), services.KeyUpdateParams{
		Notes:               req.Notes,
		Priority:            req.Priority,
		Status:              req.Status,
		Config:              req.Config,
		ProbeParamOverrides: req.ProbeParamOverrides,
	})
	if s.handleKeyError(c, err) {
		return
	}
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			response.Error(c, app_errors.ErrResourceNotFound)
		} else {
			response.Error(c, app_errors.ParseDBError(err))
		}
		return
	}

	response.Success(c, key)
}
