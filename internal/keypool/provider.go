package keypool

import (
	"encoding/json"
	"errors"
	"fmt"
	"gpt-load/internal/config"
	"gpt-load/internal/encryption"
	app_errors "gpt-load/internal/errors"
	"gpt-load/internal/models"
	"gpt-load/internal/store"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type KeyProvider struct {
	db              *gorm.DB
	store           store.Store
	settingsManager *config.SystemSettingsManager
	encryptionSvc   encryption.Service
}

type failureWindowEntry struct {
	Timestamp int64   `json:"timestamp"`
	Penalty   float64 `json:"penalty"`
}

type failureWindowState struct {
	Entries []failureWindowEntry `json:"entries"`
}

type ProbeStatusUpdate struct {
	CheckedAt    time.Time
	Success      bool
	StatusCode   int
	ErrorMessage string
	FailureRate  float64
	SampleCount  int64
}

type KeyMetaUpdate struct {
	Notes               *string
	Priority            *int
	Config              *datatypes.JSONMap
	ProbeParamOverrides *datatypes.JSONMap
}

// NewProvider 创建一个新的 KeyProvider 实例。
func NewProvider(db *gorm.DB, store store.Store, settingsManager *config.SystemSettingsManager, encryptionSvc encryption.Service) *KeyProvider {
	return &KeyProvider{
		db:              db,
		store:           store,
		settingsManager: settingsManager,
		encryptionSvc:   encryptionSvc,
	}
}

func normalizePriority(priority int) int {
	if priority <= 0 {
		return models.DefaultAPIKeyPriority
	}
	return priority
}

func activeKeysListKey(groupID uint) string {
	return fmt.Sprintf("group:%d:active_keys", groupID)
}

func activeKeysPriorityListKey(groupID uint, priority int) string {
	return fmt.Sprintf("group:%d:active_keys:priority:%d", groupID, normalizePriority(priority))
}

func groupPriorityOrderKey(groupID uint) string {
	return fmt.Sprintf("group:%d:priority_order", groupID)
}

func probeWindowStoreKey(keyID uint) string {
	return fmt.Sprintf("key:%d:probe_window", keyID)
}

func failureWindowStoreKey(keyID uint) string {
	return fmt.Sprintf("key:%d:failure_window", keyID)
}

func resetProbeStatsUpdates() map[string]any {
	return map[string]any{
		"probe_failure_rate": 0,
		"probe_sample_count": 0,
	}
}

func (p *KeyProvider) clearProbeWindow(keyID uint) error {
	if err := p.store.Delete(probeWindowStoreKey(keyID)); err != nil {
		return fmt.Errorf("failed to clear probe window for key %d: %w", keyID, err)
	}
	return nil
}

func (p *KeyProvider) clearFailureWindow(keyID uint) error {
	if err := p.store.Delete(failureWindowStoreKey(keyID)); err != nil {
		return fmt.Errorf("failed to clear failure window for key %d: %w", keyID, err)
	}
	return nil
}

func (p *KeyProvider) addActiveKeyToLists(groupID, keyID uint, priority int) error {
	globalListKey := activeKeysListKey(groupID)
	if err := p.store.LRem(globalListKey, 0, keyID); err != nil {
		return fmt.Errorf("failed to remove key %d from global active list before push: %w", keyID, err)
	}
	if err := p.store.LPush(globalListKey, keyID); err != nil {
		return fmt.Errorf("failed to push key %d to global active list: %w", keyID, err)
	}

	priorityListKey := activeKeysPriorityListKey(groupID, priority)
	if err := p.store.LRem(priorityListKey, 0, keyID); err != nil {
		return fmt.Errorf("failed to remove key %d from priority active list before push: %w", keyID, err)
	}
	if err := p.store.LPush(priorityListKey, keyID); err != nil {
		return fmt.Errorf("failed to push key %d to priority active list: %w", keyID, err)
	}

	return nil
}

func (p *KeyProvider) removeActiveKeyFromLists(groupID, keyID uint, priority int) error {
	if err := p.store.LRem(activeKeysListKey(groupID), 0, keyID); err != nil {
		return fmt.Errorf("failed to remove key %d from global active list: %w", keyID, err)
	}
	if err := p.store.LRem(activeKeysPriorityListKey(groupID, priority), 0, keyID); err != nil {
		return fmt.Errorf("failed to remove key %d from priority active list: %w", keyID, err)
	}
	return nil
}

func (p *KeyProvider) syncGroupPriorityOrder(groupID uint) error {
	var priorities []int
	if err := p.db.Model(&models.APIKey{}).
		Where("group_id = ?", groupID).
		Distinct("priority").
		Pluck("priority", &priorities).Error; err != nil {
		return fmt.Errorf("failed to load group priorities: %w", err)
	}

	if len(priorities) == 0 {
		return p.store.Delete(groupPriorityOrderKey(groupID))
	}

	normalized := make([]int, 0, len(priorities))
	seen := make(map[int]struct{}, len(priorities))
	for _, priority := range priorities {
		priority = normalizePriority(priority)
		if _, ok := seen[priority]; ok {
			continue
		}
		seen[priority] = struct{}{}
		normalized = append(normalized, priority)
	}
	sort.Ints(normalized)

	parts := make([]string, 0, len(normalized))
	for _, priority := range normalized {
		parts = append(parts, strconv.Itoa(priority))
	}

	return p.store.Set(groupPriorityOrderKey(groupID), []byte(strings.Join(parts, ",")), 0)
}

func (p *KeyProvider) loadPriorityOrder(groupID uint) ([]int, error) {
	raw, err := p.store.Get(groupPriorityOrderKey(groupID))
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("failed to get group priority metadata: %w", err)
		}
		if err := p.syncGroupPriorityOrder(groupID); err != nil {
			return nil, err
		}
		raw, err = p.store.Get(groupPriorityOrderKey(groupID))
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, nil
			}
			return nil, fmt.Errorf("failed to reload group priority metadata: %w", err)
		}
	}

	text := strings.TrimSpace(string(raw))
	if text == "" {
		return nil, nil
	}

	parts := strings.Split(text, ",")
	priorities := make([]int, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		priority, err := strconv.Atoi(part)
		if err != nil {
			continue
		}
		priorities = append(priorities, normalizePriority(priority))
	}

	sort.Ints(priorities)
	return priorities, nil
}

// SelectKey 为指定的分组原子性地选择并轮换一个可用的 APIKey。
func (p *KeyProvider) SelectKey(groupID uint) (*models.APIKey, error) {
	priorities, err := p.loadPriorityOrder(groupID)
	if err != nil {
		return nil, err
	}
	if len(priorities) == 0 {
		return nil, app_errors.ErrNoActiveKeys
	}

	globalActiveListKey := activeKeysListKey(groupID)

	for _, priority := range priorities {
		priorityListKey := activeKeysPriorityListKey(groupID, priority)

		for {
			keyIDStr, err := p.store.Rotate(priorityListKey)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					break
				}
				return nil, fmt.Errorf("failed to rotate key from priority list: %w", err)
			}

			keyID, err := strconv.ParseUint(keyIDStr, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("failed to parse key ID '%s': %w", keyIDStr, err)
			}

			keyHashKey := fmt.Sprintf("key:%d", keyID)
			keyDetails, err := p.store.HGetAll(keyHashKey)
			if err != nil {
				return nil, fmt.Errorf("failed to get key details for key ID %d: %w", keyID, err)
			}

			if len(keyDetails) == 0 || keyDetails["status"] != models.KeyStatusActive {
				_ = p.store.LRem(priorityListKey, 0, keyID)
				_ = p.store.LRem(globalActiveListKey, 0, keyID)
				continue
			}

			failureCount := parseFailureCount(keyDetails["failure_count"])
			createdAt, _ := strconv.ParseInt(keyDetails["created_at"], 10, 64)
			keyPriority, _ := strconv.Atoi(keyDetails["priority"])
			keyPriority = normalizePriority(keyPriority)

			encryptedKeyValue := keyDetails["key_string"]
			decryptedKeyValue, err := p.encryptionSvc.Decrypt(encryptedKeyValue)
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"keyID": keyID,
					"error": err,
				}).Debug("Failed to decrypt key value, using as-is for backward compatibility")
				decryptedKeyValue = encryptedKeyValue
			}

			return &models.APIKey{
				ID:           uint(keyID),
				KeyValue:     decryptedKeyValue,
				Status:       keyDetails["status"],
				FailureCount: failureCount,
				GroupID:      groupID,
				Priority:     keyPriority,
				CreatedAt:    time.Unix(createdAt, 0),
			}, nil
		}
	}

	return nil, app_errors.ErrNoActiveKeys
}

// UpdateStatus 异步地提交一个 Key 状态更新任务。
func (p *KeyProvider) UpdateStatus(apiKey *models.APIKey, group *models.Group, isSuccess bool, errorMessage string) {
	go func() {
		keyHashKey := fmt.Sprintf("key:%d", apiKey.ID)

		if isSuccess {
			if err := p.handleSuccess(apiKey.ID, keyHashKey); err != nil {
				logrus.WithFields(logrus.Fields{"keyID": apiKey.ID, "error": err}).Error("Failed to handle key success")
			}
		} else {
			if app_errors.IsUnCounted(errorMessage) {
				logrus.WithFields(logrus.Fields{
					"keyID": apiKey.ID,
					"error": errorMessage,
				}).Debug("Uncounted error, skipping failure handling")
			} else {
				penalty := p.failurePenaltyForError(group, errorMessage)
				if penalty <= 0 {
					return
				}
				if err := p.handleFailure(apiKey, group, keyHashKey, penalty); err != nil {
					logrus.WithFields(logrus.Fields{"keyID": apiKey.ID, "error": err}).Error("Failed to handle key failure")
				}
			}
		}
	}()
}

// UpdateRequestSuccess 在普通请求成功后重置连续失败计数，并刷新窗口失败快照。
func (p *KeyProvider) UpdateRequestSuccess(apiKey *models.APIKey, group *models.Group) {
	if apiKey == nil || group == nil {
		return
	}
	go func() {
		keyHashKey := fmt.Sprintf("key:%d", apiKey.ID)
		if err := p.handleRequestSuccess(apiKey.ID, group, keyHashKey); err != nil {
			logrus.WithFields(logrus.Fields{"keyID": apiKey.ID, "error": err}).Error("Failed to handle request success")
		}
	}()
}

// UpdateRequestFailure 在普通请求失败后更新窗口失败累计和连续失败次数。
func (p *KeyProvider) UpdateRequestFailure(apiKey *models.APIKey, group *models.Group, errorMessage string) {
	if apiKey == nil || group == nil {
		return
	}
	go func() {
		if app_errors.IsUnCounted(errorMessage) {
			logrus.WithFields(logrus.Fields{
				"keyID": apiKey.ID,
				"error": errorMessage,
			}).Debug("Uncounted request error, skipping failure handling")
			return
		}

		penalty := p.failurePenaltyForError(group, errorMessage)
		if penalty <= 0 {
			return
		}

		keyHashKey := fmt.Sprintf("key:%d", apiKey.ID)
		if err := p.handleFailure(apiKey, group, keyHashKey, penalty); err != nil {
			logrus.WithFields(logrus.Fields{"keyID": apiKey.ID, "error": err}).Error("Failed to handle request failure")
		}
	}()
}

func (p *KeyProvider) UpdateKeyPriority(keyID uint, priority int) (*models.APIKey, error) {
	priority = normalizePriority(priority)

	var key models.APIKey
	var oldPriority int
	err := p.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Set("gorm:query_option", "FOR UPDATE").First(&key, keyID).Error; err != nil {
			return err
		}

		oldPriority = normalizePriority(key.Priority)
		key.Priority = priority
		if oldPriority == priority {
			return nil
		}

		return tx.Model(&key).Update("priority", priority).Error
	})
	if err != nil {
		return nil, err
	}

	if err := p.store.HSet(fmt.Sprintf("key:%d", key.ID), map[string]any{"priority": priority}); err != nil {
		return nil, fmt.Errorf("failed to update key priority in store: %w", err)
	}

	if key.Status == models.KeyStatusActive && oldPriority != priority {
		if err := p.store.LRem(activeKeysPriorityListKey(key.GroupID, oldPriority), 0, key.ID); err != nil {
			return nil, fmt.Errorf("failed to remove key from old priority list: %w", err)
		}
		if err := p.store.LRem(activeKeysPriorityListKey(key.GroupID, priority), 0, key.ID); err != nil {
			return nil, fmt.Errorf("failed to prepare new priority list: %w", err)
		}
		if err := p.store.LPush(activeKeysPriorityListKey(key.GroupID, priority), key.ID); err != nil {
			return nil, fmt.Errorf("failed to push key into new priority list: %w", err)
		}
	}

	if err := p.syncGroupPriorityOrder(key.GroupID); err != nil {
		return nil, err
	}

	return &key, nil
}

func (p *KeyProvider) UpdateKeyMeta(keyID uint, update KeyMetaUpdate) (*models.APIKey, error) {
	var key models.APIKey
	var oldPriority int
	var priorityChanged bool

	err := p.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Set("gorm:query_option", "FOR UPDATE").First(&key, keyID).Error; err != nil {
			return err
		}

		oldPriority = normalizePriority(key.Priority)
		updates := make(map[string]any)

		if update.Notes != nil {
			updates["notes"] = *update.Notes
			key.Notes = *update.Notes
		}
		if update.Priority != nil {
			nextPriority := normalizePriority(*update.Priority)
			updates["priority"] = nextPriority
			key.Priority = nextPriority
			priorityChanged = oldPriority != nextPriority
		}
		if update.Config != nil {
			updates["config"] = *update.Config
			key.Config = *update.Config
		}
		if update.ProbeParamOverrides != nil {
			updates["probe_param_overrides"] = *update.ProbeParamOverrides
			key.ProbeParamOverrides = *update.ProbeParamOverrides
		}

		if len(updates) == 0 {
			return nil
		}

		if err := tx.Model(&key).Updates(updates).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if update.Priority != nil {
		storeUpdates := map[string]any{"priority": normalizePriority(*update.Priority)}
		if err := p.store.HSet(fmt.Sprintf("key:%d", key.ID), storeUpdates); err != nil {
			return nil, fmt.Errorf("failed to update key priority in store: %w", err)
		}

		if key.Status == models.KeyStatusActive && priorityChanged {
			if err := p.store.LRem(activeKeysPriorityListKey(key.GroupID, oldPriority), 0, key.ID); err != nil {
				return nil, fmt.Errorf("failed to remove key from old priority list: %w", err)
			}
			if err := p.store.LRem(activeKeysPriorityListKey(key.GroupID, key.Priority), 0, key.ID); err != nil {
				return nil, fmt.Errorf("failed to prepare new priority list: %w", err)
			}
			if err := p.store.LPush(activeKeysPriorityListKey(key.GroupID, key.Priority), key.ID); err != nil {
				return nil, fmt.Errorf("failed to push key into new priority list: %w", err)
			}
		}

		if err := p.syncGroupPriorityOrder(key.GroupID); err != nil {
			return nil, err
		}
	}

	return &key, nil
}

func (p *KeyProvider) MarkKeyValidated(keyID uint, validatedAt time.Time) error {
	return p.db.Model(&models.APIKey{}).Where("id = ?", keyID).Update("last_validated_at", validatedAt).Error
}

func (p *KeyProvider) UpdateProbeStatus(apiKey *models.APIKey, update ProbeStatusUpdate, shouldBlacklist bool, shouldRestore bool) error {
	keyHashKey := fmt.Sprintf("key:%d", apiKey.ID)

	return p.executeTransactionWithRetry(func(tx *gorm.DB) error {
		var key models.APIKey
		if err := tx.Set("gorm:query_option", "FOR UPDATE").First(&key, apiKey.ID).Error; err != nil {
			return fmt.Errorf("failed to lock key %d for probe update: %w", apiKey.ID, err)
		}

		key.Priority = normalizePriority(key.Priority)
		statusChanged := false
		restoreToActive := false
		moveToInvalid := false

		updates := map[string]any{
			"last_probe_at":          update.CheckedAt,
			"last_probe_success":     update.Success,
			"last_probe_status_code": update.StatusCode,
			"last_probe_error":       update.ErrorMessage,
			"probe_failure_rate":     update.FailureRate,
			"probe_sample_count":     update.SampleCount,
		}

		if shouldRestore && key.Status != models.KeyStatusActive {
			updates["status"] = models.KeyStatusActive
			updates["failure_count"] = 0
			restoreToActive = true
			statusChanged = true
		} else if shouldBlacklist && key.Status == models.KeyStatusActive {
			updates["status"] = models.KeyStatusInvalid
			updates["failure_count"] = 0
			moveToInvalid = true
			statusChanged = true
		}

		if err := tx.Model(&key).Updates(updates).Error; err != nil {
			return fmt.Errorf("failed to update probe status in DB: %w", err)
		}

		if statusChanged {
			storeUpdates := map[string]any{"status": updates["status"]}
			if restoreToActive {
				storeUpdates["failure_count"] = 0
				storeUpdates["consecutive_failure_count"] = 0
			}
			if moveToInvalid {
				storeUpdates["failure_count"] = 0
				storeUpdates["consecutive_failure_count"] = 0
			}
			if err := p.store.HSet(keyHashKey, storeUpdates); err != nil {
				return fmt.Errorf("failed to update key status in store: %w", err)
			}
		}

		if restoreToActive {
			if err := p.clearFailureWindow(apiKey.ID); err != nil {
				return err
			}
			if err := p.addActiveKeyToLists(key.GroupID, key.ID, key.Priority); err != nil {
				return fmt.Errorf("failed to restore key to active lists after probe: %w", err)
			}
		}
		if moveToInvalid {
			if err := p.clearFailureWindow(apiKey.ID); err != nil {
				return err
			}
			if err := p.removeActiveKeyFromLists(key.GroupID, key.ID, key.Priority); err != nil {
				return fmt.Errorf("failed to remove key from active lists after probe: %w", err)
			}
		}

		return nil
	})
}

// executeTransactionWithRetry wraps a database transaction with a retry mechanism.
func (p *KeyProvider) executeTransactionWithRetry(operation func(tx *gorm.DB) error) error {
	const maxRetries = 3
	const baseDelay = 50 * time.Millisecond
	const maxJitter = 150 * time.Millisecond
	var err error

	for i := range maxRetries {
		err = p.db.Transaction(operation)
		if err == nil {
			return nil
		}

		if strings.Contains(err.Error(), "database is locked") {
			jitter := time.Duration(rand.Intn(int(maxJitter)))
			totalDelay := baseDelay + jitter
			logrus.Debugf("Database is locked, retrying in %v... (attempt %d/%d)", totalDelay, i+1, maxRetries)
			time.Sleep(totalDelay)
			continue
		}

		break
	}

	return err
}

func (p *KeyProvider) handleSuccess(keyID uint, keyHashKey string) error {
	keyDetails, err := p.store.HGetAll(keyHashKey)
	if err != nil {
		return fmt.Errorf("failed to get key details from store: %w", err)
	}

	failureCount := parseFailureCount(keyDetails["failure_count"])
	consecutiveFailureCount := parseConsecutiveFailureCount(keyDetails["consecutive_failure_count"])
	isActive := keyDetails["status"] == models.KeyStatusActive
	priority, _ := strconv.Atoi(keyDetails["priority"])
	priority = normalizePriority(priority)

	if failureCount <= 0 && consecutiveFailureCount <= 0 && isActive {
		return p.clearFailureWindow(keyID)
	}

	return p.executeTransactionWithRetry(func(tx *gorm.DB) error {
		var key models.APIKey
		if err := tx.Set("gorm:query_option", "FOR UPDATE").First(&key, keyID).Error; err != nil {
			return fmt.Errorf("failed to lock key %d for update: %w", keyID, err)
		}

		updates := map[string]any{"failure_count": 0.0}
		if !isActive {
			updates["status"] = models.KeyStatusActive
			for field, value := range resetProbeStatsUpdates() {
				updates[field] = value
			}
		}

		if err := tx.Model(&key).Updates(updates).Error; err != nil {
			return fmt.Errorf("failed to update key in DB: %w", err)
		}

		if err := p.clearFailureWindow(keyID); err != nil {
			return err
		}

		storeUpdates := map[string]any{
			"failure_count":             0.0,
			"consecutive_failure_count": 0,
		}
		if err := p.store.HSet(keyHashKey, updates); err != nil {
			return fmt.Errorf("failed to update key details in store: %w", err)
		}
		if err := p.store.HSet(keyHashKey, storeUpdates); err != nil {
			return fmt.Errorf("failed to reset key request failure state in store: %w", err)
		}

		if !isActive {
			if err := p.clearProbeWindow(keyID); err != nil {
				return err
			}
			logrus.WithField("keyID", keyID).Debug("Key has recovered and is being restored to active pool.")
			if err := p.addActiveKeyToLists(key.GroupID, keyID, priority); err != nil {
				return fmt.Errorf("failed to restore key to active lists: %w", err)
			}
		}

		return nil
	})
}

func (p *KeyProvider) handleRequestSuccess(keyID uint, group *models.Group, keyHashKey string) error {
	return p.executeTransactionWithRetry(func(tx *gorm.DB) error {
		var key models.APIKey
		if err := tx.Set("gorm:query_option", "FOR UPDATE").First(&key, keyID).Error; err != nil {
			return fmt.Errorf("failed to lock key %d for request success update: %w", keyID, err)
		}

		if key.Status == models.KeyStatusInvalid {
			return nil
		}

		effectiveConfig := group.EffectiveConfig
		if p.settingsManager != nil {
			effectiveConfig = p.settingsManager.GetEffectiveKeyConfig(group.Config, key.Config)
		}

		windowFailureCount, err := p.snapshotFailureWindow(keyID, effectiveConfig.BlacklistWindowMinutes, time.Now())
		if err != nil {
			return err
		}

		keyDetails, err := p.store.HGetAll(keyHashKey)
		if err != nil {
			return fmt.Errorf("failed to get key details from store: %w", err)
		}
		consecutiveFailureCount := parseConsecutiveFailureCount(keyDetails["consecutive_failure_count"])

		updates := map[string]any{}
		if key.FailureCount != windowFailureCount {
			updates["failure_count"] = windowFailureCount
		}
		if len(updates) > 0 {
			if err := tx.Model(&key).Updates(updates).Error; err != nil {
				return fmt.Errorf("failed to update key request success snapshot in DB: %w", err)
			}
		}

		if consecutiveFailureCount <= 0 && len(updates) == 0 {
			return nil
		}

		storeUpdates := map[string]any{"consecutive_failure_count": 0}
		if len(updates) > 0 {
			storeUpdates["failure_count"] = windowFailureCount
		}
		if err := p.store.HSet(keyHashKey, storeUpdates); err != nil {
			return fmt.Errorf("failed to update key request success state in store: %w", err)
		}

		return nil
	})
}

func (p *KeyProvider) handleFailure(apiKey *models.APIKey, group *models.Group, keyHashKey string, penalty float64) error {
	keyDetails, err := p.store.HGetAll(keyHashKey)
	if err != nil {
		return fmt.Errorf("failed to get key details from store: %w", err)
	}

	if keyDetails["status"] == models.KeyStatusInvalid {
		return nil
	}

	priority, _ := strconv.Atoi(keyDetails["priority"])
	priority = normalizePriority(priority)

	return p.executeTransactionWithRetry(func(tx *gorm.DB) error {
		var key models.APIKey
		if err := tx.Set("gorm:query_option", "FOR UPDATE").First(&key, apiKey.ID).Error; err != nil {
			return fmt.Errorf("failed to lock key %d for update: %w", apiKey.ID, err)
		}

		effectiveConfig := group.EffectiveConfig
		if p.settingsManager != nil {
			effectiveConfig = p.settingsManager.GetEffectiveKeyConfig(group.Config, key.Config)
		}

		windowFailureCount, err := p.recordFailureWindow(apiKey.ID, effectiveConfig.BlacklistWindowMinutes, penalty, time.Now())
		if err != nil {
			return err
		}

		keyDetails, err := p.store.HGetAll(keyHashKey)
		if err != nil {
			return fmt.Errorf("failed to get key details from store: %w", err)
		}
		newConsecutiveFailureCount := parseConsecutiveFailureCount(keyDetails["consecutive_failure_count"]) + 1

		updates := map[string]any{"failure_count": windowFailureCount}
		shouldBlacklistByWindow := effectiveConfig.BlacklistThreshold > 0 && windowFailureCount >= float64(effectiveConfig.BlacklistThreshold)
		shouldBlacklistByConsecutive := effectiveConfig.ConsecutiveFailureThreshold > 0 && newConsecutiveFailureCount >= effectiveConfig.ConsecutiveFailureThreshold
		shouldBlacklist := shouldBlacklistByWindow || shouldBlacklistByConsecutive
		if shouldBlacklist {
			updates["status"] = models.KeyStatusInvalid
			for field, value := range resetProbeStatsUpdates() {
				updates[field] = value
			}
		}

		if err := tx.Model(&key).Updates(updates).Error; err != nil {
			return fmt.Errorf("failed to update key stats in DB: %w", err)
		}

		storeUpdates := map[string]any{
			"failure_count":             windowFailureCount,
			"consecutive_failure_count": newConsecutiveFailureCount,
		}

		if shouldBlacklist {
			logrus.WithFields(logrus.Fields{
				"keyID":                     apiKey.ID,
				"window_threshold":          effectiveConfig.BlacklistThreshold,
				"window_failure_count":      windowFailureCount,
				"consecutive_threshold":     effectiveConfig.ConsecutiveFailureThreshold,
				"consecutive_failure_count": newConsecutiveFailureCount,
			}).Warn("Key has reached blacklist threshold, disabling.")
			if err := p.removeActiveKeyFromLists(group.ID, apiKey.ID, priority); err != nil {
				return fmt.Errorf("failed to remove key from active lists: %w", err)
			}
			storeUpdates["status"] = models.KeyStatusInvalid
			storeUpdates["consecutive_failure_count"] = 0
			for field, value := range resetProbeStatsUpdates() {
				storeUpdates[field] = value
			}
			if err := p.clearProbeWindow(apiKey.ID); err != nil {
				return err
			}
			if err := p.clearFailureWindow(apiKey.ID); err != nil {
				return err
			}
		}

		if err := p.store.HSet(keyHashKey, storeUpdates); err != nil {
			return fmt.Errorf("failed to update key status in store: %w", err)
		}

		return nil
	})
}

// LoadKeysFromDB 从数据库加载所有分组和密钥，并填充到 Store 中。
func (p *KeyProvider) LoadKeysFromDB() error {
	logrus.Debug("First time startup, loading keys from DB...")

	// 1. 分批从数据库加载并使用 Pipeline 写入 Redis
	allActiveKeyIDs := make(map[uint][]any)
	allActiveKeyIDsByPriority := make(map[uint]map[int][]any)
	allGroupPriorities := make(map[uint]map[int]struct{})
	batchSize := 10000
	var batchKeys []*models.APIKey

	err := p.db.Model(&models.APIKey{}).FindInBatches(&batchKeys, batchSize, func(tx *gorm.DB, batch int) error {
		logrus.Debugf("Processing batch %d with %d keys...", batch, len(batchKeys))

		var pipeline store.Pipeliner
		if redisStore, ok := p.store.(store.RedisPipeliner); ok {
			pipeline = redisStore.Pipeline()
		}

		for _, key := range batchKeys {
			key.Priority = normalizePriority(key.Priority)
			keyHashKey := fmt.Sprintf("key:%d", key.ID)
			keyDetails := p.apiKeyToMap(key)

			if pipeline != nil {
				pipeline.HSet(keyHashKey, keyDetails)
			} else {
				if err := p.store.HSet(keyHashKey, keyDetails); err != nil {
					logrus.WithFields(logrus.Fields{"keyID": key.ID, "error": err}).Error("Failed to HSet key details")
				}
			}

			if _, ok := allGroupPriorities[key.GroupID]; !ok {
				allGroupPriorities[key.GroupID] = make(map[int]struct{})
			}
			allGroupPriorities[key.GroupID][key.Priority] = struct{}{}

			if key.Status == models.KeyStatusActive {
				allActiveKeyIDs[key.GroupID] = append(allActiveKeyIDs[key.GroupID], key.ID)
				if _, ok := allActiveKeyIDsByPriority[key.GroupID]; !ok {
					allActiveKeyIDsByPriority[key.GroupID] = make(map[int][]any)
				}
				allActiveKeyIDsByPriority[key.GroupID][key.Priority] = append(allActiveKeyIDsByPriority[key.GroupID][key.Priority], key.ID)
			}
		}

		if pipeline != nil {
			if err := pipeline.Exec(); err != nil {
				return fmt.Errorf("failed to execute pipeline for batch %d: %w", batch, err)
			}
		}
		return nil
	}).Error

	if err != nil {
		return fmt.Errorf("failed during batch processing of keys: %w", err)
	}

	// 2. 更新所有分组的 active_keys 列表
	logrus.Info("Updating active key lists for all groups...")
	for groupID, activeIDs := range allActiveKeyIDs {
		if len(activeIDs) > 0 {
			listKey := activeKeysListKey(groupID)
			p.store.Delete(listKey)
			if err := p.store.LPush(listKey, activeIDs...); err != nil {
				logrus.WithFields(logrus.Fields{"groupID": groupID, "error": err}).Error("Failed to LPush active keys for group")
			}
		}
	}

	for groupID, priorityMap := range allActiveKeyIDsByPriority {
		for priority, activeIDs := range priorityMap {
			if len(activeIDs) == 0 {
				continue
			}
			listKey := activeKeysPriorityListKey(groupID, priority)
			p.store.Delete(listKey)
			if err := p.store.LPush(listKey, activeIDs...); err != nil {
				logrus.WithFields(logrus.Fields{
					"groupID":  groupID,
					"priority": priority,
					"error":    err,
				}).Error("Failed to LPush active priority keys for group")
			}
		}
	}

	for groupID, priorities := range allGroupPriorities {
		items := make([]int, 0, len(priorities))
		for priority := range priorities {
			items = append(items, priority)
		}
		sort.Ints(items)

		parts := make([]string, 0, len(items))
		for _, priority := range items {
			parts = append(parts, strconv.Itoa(priority))
		}

		if err := p.store.Set(groupPriorityOrderKey(groupID), []byte(strings.Join(parts, ",")), 0); err != nil {
			logrus.WithFields(logrus.Fields{"groupID": groupID, "error": err}).Error("Failed to update group priority order")
		}
	}

	return nil
}

// AddKeys 批量添加新的 Key 到池和数据库中。
func (p *KeyProvider) AddKeys(groupID uint, keys []models.APIKey) error {
	if len(keys) == 0 {
		return nil
	}

	for i := range keys {
		keys[i].Priority = normalizePriority(keys[i].Priority)
	}

	err := p.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&keys).Error; err != nil {
			return err
		}

		// 使用批量方法添加到缓存
		return p.addKeysToCacheBatch(groupID, keys)
	})

	if err != nil {
		return err
	}

	return p.syncGroupPriorityOrder(groupID)
}

// RemoveKeys 批量从池和数据库中移除 Key。
func (p *KeyProvider) RemoveKeys(groupID uint, keyValues []string) (int64, error) {
	if len(keyValues) == 0 {
		return 0, nil
	}

	var keysToDelete []models.APIKey
	var deletedCount int64

	err := p.db.Transaction(func(tx *gorm.DB) error {
		var keyHashes []string
		for _, keyValue := range keyValues {
			keyHash := p.encryptionSvc.Hash(keyValue)
			if keyHash != "" {
				keyHashes = append(keyHashes, keyHash)
			}
		}

		if len(keyHashes) == 0 {
			return nil
		}

		if err := tx.Where("group_id = ? AND key_hash IN ?", groupID, keyHashes).Find(&keysToDelete).Error; err != nil {
			return err
		}

		if len(keysToDelete) == 0 {
			return nil
		}

		keyIDsToDelete := pluckIDs(keysToDelete)

		result := tx.Where("id IN ?", keyIDsToDelete).Delete(&models.APIKey{})
		if result.Error != nil {
			return result.Error
		}
		deletedCount = result.RowsAffected

		for _, key := range keysToDelete {
			if err := p.removeKeyFromStore(&key); err != nil {
				logrus.WithFields(logrus.Fields{"keyID": key.ID, "error": err}).Error("Failed to remove key from store after DB deletion, rolling back transaction")
				return err
			}
		}

		return nil
	})

	if err != nil {
		return deletedCount, err
	}

	return deletedCount, p.syncGroupPriorityOrder(groupID)
}

// RestoreKeys 恢复组内所有无效的 Key。
func (p *KeyProvider) RestoreKeys(groupID uint) (int64, error) {
	var invalidKeys []models.APIKey
	var restoredCount int64

	err := p.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("group_id = ? AND status = ?", groupID, models.KeyStatusInvalid).Find(&invalidKeys).Error; err != nil {
			return err
		}

		if len(invalidKeys) == 0 {
			return nil
		}

		updates := map[string]any{
			"status":             models.KeyStatusActive,
			"failure_count":      0,
			"probe_failure_rate": 0,
			"probe_sample_count": 0,
		}
		result := tx.Model(&models.APIKey{}).Where("group_id = ? AND status = ?", groupID, models.KeyStatusInvalid).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		restoredCount = result.RowsAffected

		for _, key := range invalidKeys {
			key.Status = models.KeyStatusActive
			key.FailureCount = 0
			if err := p.addKeyToStore(&key); err != nil {
				logrus.WithFields(logrus.Fields{"keyID": key.ID, "error": err}).Error("Failed to restore key in store after DB update, rolling back transaction")
				return err
			}
			if err := p.clearFailureWindow(key.ID); err != nil {
				logrus.WithFields(logrus.Fields{"keyID": key.ID, "error": err}).Error("Failed to clear failure window after restoring key, rolling back transaction")
				return err
			}
			if err := p.clearProbeWindow(key.ID); err != nil {
				logrus.WithFields(logrus.Fields{"keyID": key.ID, "error": err}).Error("Failed to clear probe window after restoring key, rolling back transaction")
				return err
			}
		}
		return nil
	})

	return restoredCount, err
}

// RestoreMultipleKeys 恢复指定的 Key。
func (p *KeyProvider) RestoreMultipleKeys(groupID uint, keyValues []string) (int64, error) {
	if len(keyValues) == 0 {
		return 0, nil
	}

	var keysToRestore []models.APIKey
	var restoredCount int64

	err := p.db.Transaction(func(tx *gorm.DB) error {
		var keyHashes []string
		for _, keyValue := range keyValues {
			keyHash := p.encryptionSvc.Hash(keyValue)
			if keyHash != "" {
				keyHashes = append(keyHashes, keyHash)
			}
		}

		if len(keyHashes) == 0 {
			return nil
		}

		if err := tx.Where("group_id = ? AND key_hash IN ? AND status = ?", groupID, keyHashes, models.KeyStatusInvalid).Find(&keysToRestore).Error; err != nil {
			return err
		}

		if len(keysToRestore) == 0 {
			return nil
		}

		keyIDsToRestore := pluckIDs(keysToRestore)

		updates := map[string]any{
			"status":             models.KeyStatusActive,
			"failure_count":      0,
			"probe_failure_rate": 0,
			"probe_sample_count": 0,
		}
		result := tx.Model(&models.APIKey{}).Where("id IN ?", keyIDsToRestore).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		restoredCount = result.RowsAffected

		for _, key := range keysToRestore {
			key.Status = models.KeyStatusActive
			key.FailureCount = 0
			if err := p.addKeyToStore(&key); err != nil {
				logrus.WithFields(logrus.Fields{"keyID": key.ID, "error": err}).Error("Failed to restore key in store after DB update")
				return err
			}
			if err := p.clearFailureWindow(key.ID); err != nil {
				logrus.WithFields(logrus.Fields{"keyID": key.ID, "error": err}).Error("Failed to clear failure window after restoring key")
				return err
			}
			if err := p.clearProbeWindow(key.ID); err != nil {
				logrus.WithFields(logrus.Fields{"keyID": key.ID, "error": err}).Error("Failed to clear probe window after restoring key")
				return err
			}
		}

		return nil
	})

	return restoredCount, err
}

// RemoveInvalidKeys 移除组内所有无效的 Key。
func (p *KeyProvider) RemoveInvalidKeys(groupID uint) (int64, error) {
	return p.removeKeysByStatus(groupID, models.KeyStatusInvalid)
}

// RemoveAllKeys 移除组内所有的 Key。
func (p *KeyProvider) RemoveAllKeys(groupID uint) (int64, error) {
	return p.removeKeysByStatus(groupID)
}

// removeKeysByStatus is a generic function to remove keys by status.
// If no status is provided, it removes all keys in the group.
func (p *KeyProvider) removeKeysByStatus(groupID uint, status ...string) (int64, error) {
	var keysToRemove []models.APIKey
	var removedCount int64

	err := p.db.Transaction(func(tx *gorm.DB) error {
		query := tx.Where("group_id = ?", groupID)
		if len(status) > 0 {
			query = query.Where("status IN ?", status)
		}

		if err := query.Find(&keysToRemove).Error; err != nil {
			return err
		}

		if len(keysToRemove) == 0 {
			return nil
		}

		deleteQuery := tx.Where("group_id = ?", groupID)
		if len(status) > 0 {
			deleteQuery = deleteQuery.Where("status IN ?", status)
		}
		result := deleteQuery.Delete(&models.APIKey{})
		if result.Error != nil {
			return result.Error
		}
		removedCount = result.RowsAffected

		for _, key := range keysToRemove {
			if err := p.removeKeyFromStore(&key); err != nil {
				logrus.WithFields(logrus.Fields{"keyID": key.ID, "error": err}).Error("Failed to remove key from store after DB deletion, rolling back transaction")
				return err
			}
		}
		return nil
	})

	if err != nil {
		return removedCount, err
	}

	return removedCount, p.syncGroupPriorityOrder(groupID)
}

// RemoveKeysFromStore 直接从内存存储中移除指定的键，不涉及数据库操作
// 这个方法适用于数据库已经删除但需要清理内存存储的场景
func (p *KeyProvider) RemoveKeysFromStore(groupID uint, keyIDs []uint) error {
	if len(keyIDs) == 0 {
		return nil
	}

	activeKeysListKey := fmt.Sprintf("group:%d:active_keys", groupID)

	// 第一步：直接删除整个 active_keys 列表
	if err := p.store.Delete(activeKeysListKey); err != nil {
		logrus.WithFields(logrus.Fields{
			"groupID": groupID,
			"error":   err,
		}).Error("Failed to delete active keys list")
		return err
	}

	// 第二步：批量删除所有相关的key hash
	for _, keyID := range keyIDs {
		keyHashKey := fmt.Sprintf("key:%d", keyID)
		if err := p.store.Delete(keyHashKey); err != nil {
			logrus.WithFields(logrus.Fields{
				"keyID": keyID,
				"error": err,
			}).Error("Failed to delete key hash")
		}
		if err := p.clearFailureWindow(keyID); err != nil {
			logrus.WithFields(logrus.Fields{
				"keyID": keyID,
				"error": err,
			}).Warn("Failed to delete failure window")
		}
		if err := p.clearProbeWindow(keyID); err != nil {
			logrus.WithFields(logrus.Fields{
				"keyID": keyID,
				"error": err,
			}).Warn("Failed to delete probe window")
		}
	}

	logrus.WithFields(logrus.Fields{
		"groupID":  groupID,
		"keyCount": len(keyIDs),
	}).Info("Successfully cleaned up group keys from store")

	return nil
}

// addKeyToStore is a helper to add a single key to the cache.
func (p *KeyProvider) addKeyToStore(key *models.APIKey) error {
	key.Priority = normalizePriority(key.Priority)

	// 1. Store key details in HASH
	keyHashKey := fmt.Sprintf("key:%d", key.ID)
	keyDetails := p.apiKeyToMap(key)
	if err := p.store.HSet(keyHashKey, keyDetails); err != nil {
		return fmt.Errorf("failed to HSet key details for key %d: %w", key.ID, err)
	}

	// 2. If active, add to the active LIST
	if key.Status == models.KeyStatusActive {
		if err := p.addActiveKeyToLists(key.GroupID, key.ID, key.Priority); err != nil {
			return fmt.Errorf("failed to add key %d to active lists for group %d: %w", key.ID, key.GroupID, err)
		}
	}
	return nil
}

// addKeysToCacheBatch 批量添加密钥到缓存（用于批量导入场景）
func (p *KeyProvider) addKeysToCacheBatch(groupID uint, keys []models.APIKey) error {
	if len(keys) == 0 {
		return nil
	}

	// 1. 批量 HSet 密钥详情
	if pipeliner, ok := p.store.(store.RedisPipeliner); ok {
		// Redis: 使用 Pipeline 批量操作
		pipe := pipeliner.Pipeline()
		for i := range keys {
			keyHashKey := fmt.Sprintf("key:%d", keys[i].ID)
			pipe.HSet(keyHashKey, p.apiKeyToMap(&keys[i]))
		}
		if err := pipe.Exec(); err != nil {
			return fmt.Errorf("failed to batch HSet keys: %w", err)
		}
	} else {
		// MemoryStore: 降级为逐个 HSet
		for i := range keys {
			keyHashKey := fmt.Sprintf("key:%d", keys[i].ID)
			if err := p.store.HSet(keyHashKey, p.apiKeyToMap(&keys[i])); err != nil {
				return fmt.Errorf("failed to HSet key %d: %w", keys[i].ID, err)
			}
		}
	}

	// 2. 收集所有密钥 ID
	activeKeyIDs := make([]any, 0, len(keys))
	activeKeyIDsByPriority := make(map[int][]any)
	for i := range keys {
		keys[i].Priority = normalizePriority(keys[i].Priority)
		if keys[i].Status != models.KeyStatusActive {
			continue
		}
		activeKeyIDs = append(activeKeyIDs, keys[i].ID)
		activeKeyIDsByPriority[keys[i].Priority] = append(activeKeyIDsByPriority[keys[i].Priority], keys[i].ID)
	}

	if len(activeKeyIDs) > 0 {
		if err := p.store.LPush(activeKeysListKey(groupID), activeKeyIDs...); err != nil {
			return fmt.Errorf("failed to batch LPush keys to group %d: %w", groupID, err)
		}
	}

	for priority, keyIDs := range activeKeyIDsByPriority {
		if err := p.store.LPush(activeKeysPriorityListKey(groupID, priority), keyIDs...); err != nil {
			return fmt.Errorf("failed to batch LPush keys to group %d priority %d: %w", groupID, priority, err)
		}
	}

	return nil
}

// removeKeyFromStore is a helper to remove a single key from the cache.
func (p *KeyProvider) removeKeyFromStore(key *models.APIKey) error {
	key.Priority = normalizePriority(key.Priority)
	if err := p.removeActiveKeyFromLists(key.GroupID, key.ID, key.Priority); err != nil {
		logrus.WithFields(logrus.Fields{"keyID": key.ID, "groupID": key.GroupID, "error": err}).Error("Failed to remove key from active list")
	}

	keyHashKey := fmt.Sprintf("key:%d", key.ID)
	if err := p.store.Delete(keyHashKey); err != nil {
		return fmt.Errorf("failed to delete key HASH for key %d: %w", key.ID, err)
	}
	if err := p.clearProbeWindow(key.ID); err != nil {
		logrus.WithFields(logrus.Fields{"keyID": key.ID, "error": err}).Warn("Failed to clear probe window while removing key")
	}
	if err := p.clearFailureWindow(key.ID); err != nil {
		logrus.WithFields(logrus.Fields{"keyID": key.ID, "error": err}).Warn("Failed to clear failure window while removing key")
	}
	return nil
}

// apiKeyToMap converts an APIKey model to a map for HSET.
func (p *KeyProvider) apiKeyToMap(key *models.APIKey) map[string]any {
	return map[string]any{
		"id":                        fmt.Sprint(key.ID),
		"key_string":                key.KeyValue,
		"status":                    key.Status,
		"priority":                  normalizePriority(key.Priority),
		"failure_count":             key.FailureCount,
		"consecutive_failure_count": 0,
		"group_id":                  key.GroupID,
		"created_at":                key.CreatedAt.Unix(),
	}
}

func (p *KeyProvider) failurePenaltyForError(group *models.Group, errorMessage string) float64 {
	if strings.Contains(errorMessage, "stream first visible output timeout") {
		if group == nil {
			return 1
		}
		if group.EffectiveConfig.StreamTimeoutFailurePenaltyMultiplier < 0 {
			return 0
		}
		return group.EffectiveConfig.StreamTimeoutFailurePenaltyMultiplier
	}
	return 1
}

func parseFailureCount(raw string) float64 {
	if raw == "" {
		return 0
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0
	}
	return value
}

func parseConsecutiveFailureCount(raw string) int {
	if raw == "" {
		return 0
	}
	value, err := strconv.Atoi(raw)
	if err == nil {
		return value
	}
	floatValue, floatErr := strconv.ParseFloat(raw, 64)
	if floatErr != nil {
		return 0
	}
	return int(floatValue)
}

func (p *KeyProvider) loadFailureWindowState(keyID uint) (failureWindowState, error) {
	raw, err := p.store.Get(failureWindowStoreKey(keyID))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return failureWindowState{}, nil
		}
		return failureWindowState{}, fmt.Errorf("failed to load failure window for key %d: %w", keyID, err)
	}
	if len(raw) == 0 {
		return failureWindowState{}, nil
	}

	var state failureWindowState
	if err := json.Unmarshal(raw, &state); err == nil {
		return state, nil
	}

	var legacyEntries []failureWindowEntry
	if err := json.Unmarshal(raw, &legacyEntries); err == nil {
		return failureWindowState{Entries: legacyEntries}, nil
	}

	return failureWindowState{}, fmt.Errorf("failed to unmarshal failure window for key %d", keyID)
}

func pruneFailureWindowEntries(entries []failureWindowEntry, cutoff int64) []failureWindowEntry {
	filtered := make([]failureWindowEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Timestamp < cutoff || entry.Penalty <= 0 {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func sumFailureWindowEntries(entries []failureWindowEntry) float64 {
	var total float64
	for _, entry := range entries {
		total += entry.Penalty
	}
	return total
}

func failureWindowTTL(windowMinutes int) time.Duration {
	ttl := time.Duration(windowMinutes*3) * time.Minute
	if ttl < 10*time.Minute {
		ttl = 10 * time.Minute
	}
	return ttl
}

func (p *KeyProvider) storeFailureWindowState(keyID uint, windowMinutes int, entries []failureWindowEntry) error {
	if len(entries) == 0 {
		return p.clearFailureWindow(keyID)
	}

	payload, err := json.Marshal(failureWindowState{Entries: entries})
	if err != nil {
		return fmt.Errorf("failed to marshal failure window for key %d: %w", keyID, err)
	}
	if err := p.store.Set(failureWindowStoreKey(keyID), payload, failureWindowTTL(windowMinutes)); err != nil {
		return fmt.Errorf("failed to store failure window for key %d: %w", keyID, err)
	}
	return nil
}

func (p *KeyProvider) snapshotFailureWindow(keyID uint, windowMinutes int, now time.Time) (float64, error) {
	if windowMinutes <= 0 {
		if err := p.clearFailureWindow(keyID); err != nil {
			return 0, err
		}
		return 0, nil
	}

	state, err := p.loadFailureWindowState(keyID)
	if err != nil {
		return 0, err
	}

	filtered := pruneFailureWindowEntries(state.Entries, now.Add(-time.Duration(windowMinutes)*time.Minute).Unix())
	if err := p.storeFailureWindowState(keyID, windowMinutes, filtered); err != nil {
		return 0, err
	}
	return sumFailureWindowEntries(filtered), nil
}

func (p *KeyProvider) recordFailureWindow(keyID uint, windowMinutes int, penalty float64, now time.Time) (float64, error) {
	if windowMinutes <= 0 {
		return penalty, nil
	}

	state, err := p.loadFailureWindowState(keyID)
	if err != nil {
		return 0, err
	}

	filtered := pruneFailureWindowEntries(state.Entries, now.Add(-time.Duration(windowMinutes)*time.Minute).Unix())
	filtered = append(filtered, failureWindowEntry{
		Timestamp: now.Unix(),
		Penalty:   penalty,
	})
	if err := p.storeFailureWindowState(keyID, windowMinutes, filtered); err != nil {
		return 0, err
	}
	return sumFailureWindowEntries(filtered), nil
}

// pluckIDs extracts IDs from a slice of APIKey.
func pluckIDs(keys []models.APIKey) []uint {
	ids := make([]uint, len(keys))
	for i, key := range keys {
		ids[i] = key.ID
	}
	return ids
}
