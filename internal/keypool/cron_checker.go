package keypool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"gpt-load/internal/config"
	"gpt-load/internal/encryption"
	"gpt-load/internal/models"
	"gpt-load/internal/requestoverride"
	"gpt-load/internal/store"
	"gpt-load/internal/types"
	"gpt-load/internal/utils"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

type probeWindowEntry struct {
	Timestamp int64 `json:"timestamp"`
	Success   bool  `json:"success"`
}

type probeWindowStats struct {
	SampleCount  int64
	FailureCount int64
	FailureRate  float64
}

type probeJob struct {
	Key                 models.APIKey
	EffectiveConfig     types.SystemSettings
	ProbeParamOverrides map[string]any
}

type RequestLogger interface {
	Record(log *models.RequestLog) error
}

// NewCronChecker is responsible for periodically validating invalid keys and running active probes.
type CronChecker struct {
	DB                    *gorm.DB
	SettingsManager       *config.SystemSettingsManager
	Validator             *KeyValidator
	EncryptionSvc         encryption.Service
	RequestLogService     RequestLogger
	KeyProvider           *KeyProvider
	Store                 store.Store
	stopChan              chan struct{}
	wg                    sync.WaitGroup
	probeMu               sync.Mutex
	runningProbeJobs      map[uint]bool
	validationMu          sync.Mutex
	runningValidationJobs map[uint]bool
}

// NewCronChecker creates a new CronChecker.
func NewCronChecker(
	db *gorm.DB,
	settingsManager *config.SystemSettingsManager,
	validator *KeyValidator,
	encryptionSvc encryption.Service,
	requestLogService RequestLogger,
	keyProvider *KeyProvider,
	dataStore store.Store,
) *CronChecker {
	return &CronChecker{
		DB:                    db,
		SettingsManager:       settingsManager,
		Validator:             validator,
		EncryptionSvc:         encryptionSvc,
		RequestLogService:     requestLogService,
		KeyProvider:           keyProvider,
		Store:                 dataStore,
		stopChan:              make(chan struct{}),
		runningProbeJobs:      make(map[uint]bool),
		runningValidationJobs: make(map[uint]bool),
	}
}

// Start begins the cron job execution.
func (s *CronChecker) Start() {
	logrus.Debug("Starting CronChecker...")
	s.wg.Add(1)
	go s.runLoop()
}

// Stop stops the cron job, respecting the context for shutdown timeout.
func (s *CronChecker) Stop(ctx context.Context) {
	close(s.stopChan)

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		logrus.Info("CronChecker stopped gracefully.")
	case <-ctx.Done():
		logrus.Warn("CronChecker stop timed out.")
	}
}

func (s *CronChecker) runLoop() {
	defer s.wg.Done()

	s.submitValidationJobs()
	s.submitProbeJobs()

	probeTicker := time.NewTicker(5 * time.Second)
	defer probeTicker.Stop()

	validationTicker := time.NewTicker(time.Minute)
	defer validationTicker.Stop()

	for {
		select {
		case <-probeTicker.C:
			s.submitProbeJobs()
		case <-validationTicker.C:
			logrus.Debug("CronChecker: submitting validation jobs.")
			s.submitValidationJobs()
		case <-s.stopChan:
			return
		}
	}
}

func (s *CronChecker) submitProbeJobs() {
	var groups []models.Group
	if err := s.DB.Where("group_type != ? OR group_type IS NULL", "aggregate").Find(&groups).Error; err != nil {
		logrus.Errorf("CronChecker: Failed to get groups for probing: %v", err)
		return
	}

	for i := range groups {
		group := &groups[i]
		group.EffectiveConfig = s.SettingsManager.GetEffectiveConfig(group.Config)
		if len(group.HeaderRules) > 0 {
			if err := json.Unmarshal(group.HeaderRules, &group.HeaderRuleList); err != nil {
				logrus.WithError(err).WithField("group_id", group.ID).Warn("CronChecker: failed to parse header rules for active probe")
				group.HeaderRuleList = nil
			}
		}
		if !s.tryStartProbe(group.ID) {
			continue
		}

		g := *group
		go func() {
			defer s.finishProbe(g.ID)
			s.probeGroupKeys(&g)
		}()
	}
}

func (s *CronChecker) tryStartProbe(groupID uint) bool {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()

	if s.runningProbeJobs[groupID] {
		return false
	}

	s.runningProbeJobs[groupID] = true
	return true
}

func (s *CronChecker) finishProbe(groupID uint) {
	s.probeMu.Lock()
	delete(s.runningProbeJobs, groupID)
	s.probeMu.Unlock()
}

// submitValidationJobs finds groups whose keys need validation and validates them concurrently.
func (s *CronChecker) submitValidationJobs() {
	var groups []models.Group
	if err := s.DB.Where("group_type != ? OR group_type IS NULL", "aggregate").Find(&groups).Error; err != nil {
		logrus.Errorf("CronChecker: Failed to get groups: %v", err)
		return
	}

	var wg sync.WaitGroup

	for i := range groups {
		group := &groups[i]
		group.EffectiveConfig = s.SettingsManager.GetEffectiveConfig(group.Config)
		if len(group.HeaderRules) > 0 {
			if err := json.Unmarshal(group.HeaderRules, &group.HeaderRuleList); err != nil {
				logrus.WithError(err).WithField("group_id", group.ID).Warn("CronChecker: failed to parse header rules for validation")
				group.HeaderRuleList = nil
			}
		}
		if !s.tryStartValidation(group.ID) {
			continue
		}

		wg.Add(1)
		g := group
		go func() {
			defer wg.Done()
			defer s.finishValidation(g.ID)
			s.validateGroupKeys(g)
		}()
	}

	wg.Wait()
}

func (s *CronChecker) tryStartValidation(groupID uint) bool {
	s.validationMu.Lock()
	defer s.validationMu.Unlock()

	if s.runningValidationJobs[groupID] {
		return false
	}

	s.runningValidationJobs[groupID] = true
	return true
}

func (s *CronChecker) finishValidation(groupID uint) {
	s.validationMu.Lock()
	delete(s.runningValidationJobs, groupID)
	s.validationMu.Unlock()
}

func (s *CronChecker) probeGroupKeys(group *models.Group) {
	var keys []models.APIKey
	if err := s.DB.Where("group_id = ?", group.ID).Find(&keys).Error; err != nil {
		logrus.Errorf("CronChecker: Failed to get keys for active probe in group %s: %v", group.Name, err)
		return
	}

	if len(keys) == 0 {
		return
	}

	dueJobs := make([]probeJob, 0, len(keys))
	now := time.Now()
	for i := range keys {
		effectiveConfig := s.SettingsManager.GetEffectiveKeyConfig(group.Config, keys[i].Config)
		if !effectiveConfig.ActiveProbeEnabled {
			continue
		}

		interval := time.Duration(effectiveConfig.ActiveProbeIntervalSeconds) * time.Second
		if interval <= 0 {
			continue
		}
		if keys[i].LastProbeAt != nil && now.Sub(*keys[i].LastProbeAt) < interval {
			continue
		}

		mergedProbeOverrides, err := requestoverride.Merge(group.ProbeParamOverrides, keys[i].ProbeParamOverrides)
		if err != nil {
			logrus.WithError(err).WithFields(logrus.Fields{
				"group_id": group.ID,
				"key_id":   keys[i].ID,
			}).Warn("CronChecker: failed to merge probe param overrides, using key-level fallback")
			mergedProbeOverrides = keys[i].ProbeParamOverrides
		}

		dueJobs = append(dueJobs, probeJob{
			Key:                 keys[i],
			EffectiveConfig:     effectiveConfig,
			ProbeParamOverrides: mergedProbeOverrides,
		})
	}

	if len(dueJobs) == 0 {
		return
	}

	concurrency := group.EffectiveConfig.KeyValidationConcurrency
	if concurrency <= 0 {
		concurrency = 1
	}

	var wg sync.WaitGroup
	jobs := make(chan probeJob, len(dueJobs))

	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case job, ok := <-jobs:
					if !ok {
						return
					}
					s.runSingleProbe(group, job)
				case <-s.stopChan:
					return
				}
			}
		}()
	}

	for i := range dueJobs {
		select {
		case jobs <- dueJobs[i]:
		case <-s.stopChan:
			close(jobs)
			wg.Wait()
			return
		}
	}

	close(jobs)
	wg.Wait()
}

func (s *CronChecker) runSingleProbe(group *models.Group, job probeJob) {
	key := &job.Key
	decryptedKey, err := s.decryptKey(key)
	if err != nil {
		logrus.WithError(err).WithField("key_id", key.ID).Error("CronChecker: failed to decrypt key for active probe")
		return
	}

	keyForValidation := *key
	keyForValidation.KeyValue = decryptedKey

	start := time.Now()
	isValid, probeErr := s.Validator.CheckKeyConnectivityWithOptions(&keyForValidation, group, ConnectivityCheckOptions{
		TimeoutSeconds:      job.EffectiveConfig.ActiveProbeTimeoutSeconds,
		ProbeParamOverrides: job.ProbeParamOverrides,
	})
	duration := time.Since(start).Milliseconds()
	checkedAt := time.Now()

	statusCode := parseProbeStatusCode(probeErr, isValid)
	errorMessage := ""
	if probeErr != nil {
		errorMessage = probeErr.Error()
	}

	stats, err := s.recordProbeWindow(key.ID, job.EffectiveConfig.ActiveProbeWindowMinutes, isValid, checkedAt)
	if err != nil {
		logrus.WithError(err).WithField("key_id", key.ID).Warn("CronChecker: failed to update probe window")
	}

	failureRateLimit := float64(job.EffectiveConfig.ActiveProbeFailureRateLimit)
	shouldBlacklist := stats.SampleCount > 0 && stats.FailureRate > failureRateLimit
	shouldRestore := isValid && stats.SampleCount > 0 && stats.FailureRate <= failureRateLimit

	if err := s.KeyProvider.UpdateProbeStatus(key, ProbeStatusUpdate{
		CheckedAt:    checkedAt,
		Success:      isValid,
		StatusCode:   statusCode,
		ErrorMessage: errorMessage,
		FailureRate:  stats.FailureRate,
		SampleCount:  stats.SampleCount,
	}, shouldBlacklist, shouldRestore); err != nil {
		logrus.WithError(err).WithField("key_id", key.ID).Error("CronChecker: failed to update probe status")
	}

	s.recordProbeLog(group, key, decryptedKey, isValid, statusCode, errorMessage, duration)
}

func (s *CronChecker) decryptKey(key *models.APIKey) (string, error) {
	decryptedKey, err := s.EncryptionSvc.Decrypt(key.KeyValue)
	if err != nil {
		logrus.WithError(err).WithField("key_id", key.ID).Debug("CronChecker: failed to decrypt key, using raw value for compatibility")
		return key.KeyValue, nil
	}
	return decryptedKey, nil
}

func (s *CronChecker) recordProbeWindow(keyID uint, windowMinutes int, success bool, now time.Time) (probeWindowStats, error) {
	windowKey := probeWindowStoreKey(keyID)
	var entries []probeWindowEntry

	raw, err := s.Store.Get(windowKey)
	if err == nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, &entries); err != nil {
			return probeWindowStats{}, fmt.Errorf("failed to unmarshal probe window: %w", err)
		}
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return probeWindowStats{}, fmt.Errorf("failed to load probe window: %w", err)
	}

	cutoff := now.Add(-time.Duration(windowMinutes) * time.Minute).Unix()
	filtered := make([]probeWindowEntry, 0, len(entries)+1)
	for _, entry := range entries {
		if entry.Timestamp >= cutoff {
			filtered = append(filtered, entry)
		}
	}
	filtered = append(filtered, probeWindowEntry{
		Timestamp: now.Unix(),
		Success:   success,
	})

	var failureCount int64
	for _, entry := range filtered {
		if !entry.Success {
			failureCount++
		}
	}

	stats := probeWindowStats{
		SampleCount:  lenAsInt64(filtered),
		FailureCount: failureCount,
	}
	if stats.SampleCount > 0 {
		stats.FailureRate = float64(stats.FailureCount) * 100 / float64(stats.SampleCount)
	}

	payload, err := json.Marshal(filtered)
	if err != nil {
		return probeWindowStats{}, fmt.Errorf("failed to marshal probe window: %w", err)
	}

	ttl := time.Duration(windowMinutes*3) * time.Minute
	if ttl < 10*time.Minute {
		ttl = 10 * time.Minute
	}
	if err := s.Store.Set(windowKey, payload, ttl); err != nil {
		return probeWindowStats{}, fmt.Errorf("failed to store probe window: %w", err)
	}

	return stats, nil
}

func (s *CronChecker) recordProbeLog(group *models.Group, key *models.APIKey, decryptedKey string, isSuccess bool, statusCode int, errorMessage string, duration int64) {
	if s.RequestLogService == nil {
		return
	}

	encryptedKeyValue, err := s.EncryptionSvc.Encrypt(decryptedKey)
	if err != nil {
		logrus.WithError(err).WithField("key_id", key.ID).Error("CronChecker: failed to encrypt key for probe log")
		encryptedKeyValue = "failed-to-encryption"
	}

	logEntry := &models.RequestLog{
		GroupID:      group.ID,
		GroupName:    group.Name,
		KeyValue:     encryptedKeyValue,
		KeyHash:      key.KeyHash,
		Model:        group.TestModel,
		IsSuccess:    isSuccess,
		SourceIP:     "system",
		StatusCode:   statusCode,
		RequestPath:  probeRequestPath(group),
		Duration:     duration,
		ErrorMessage: errorMessage,
		UserAgent:    "active-probe",
		RequestType:  models.RequestTypeProbe,
		IsStream:     false,
	}

	if err := s.RequestLogService.Record(logEntry); err != nil {
		logrus.WithError(err).WithField("key_id", key.ID).Error("CronChecker: failed to record probe log")
	}
}

func probeRequestPath(group *models.Group) string {
	if group.ChannelType == "gemini" {
		return fmt.Sprintf("/v1beta/models/%s:generateContent", group.TestModel)
	}
	return utils.GetValidationEndpoint(group)
}

func parseProbeStatusCode(err error, isSuccess bool) int {
	if isSuccess {
		return 200
	}
	if err == nil {
		return 500
	}

	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "context deadline exceeded") {
		return 408
	}

	msg := err.Error()
	start := strings.Index(msg, "[status ")
	if start == -1 {
		return 500
	}
	start += len("[status ")
	end := strings.Index(msg[start:], "]")
	if end == -1 {
		return 500
	}

	statusCode, parseErr := strconv.Atoi(msg[start : start+end])
	if parseErr != nil {
		return 500
	}
	return statusCode
}

func lenAsInt64[T any](items []T) int64 {
	return int64(len(items))
}

// validateGroupKeys validates all invalid keys for a single group concurrently.
func (s *CronChecker) validateGroupKeys(group *models.Group) {
	groupProcessStart := time.Now()

	var invalidKeys []models.APIKey
	err := s.DB.Where("group_id = ? AND status = ?", group.ID, models.KeyStatusInvalid).Find(&invalidKeys).Error
	if err != nil {
		logrus.Errorf("CronChecker: Failed to get invalid keys for group %s: %v", group.Name, err)
		return
	}

	if len(invalidKeys) == 0 {
		logrus.Infof("CronChecker: Group '%s' has no invalid keys to check.", group.Name)
		return
	}

	dueKeys := make([]models.APIKey, 0, len(invalidKeys))
	now := time.Now()
	for i := range invalidKeys {
		effectiveConfig := s.SettingsManager.GetEffectiveKeyConfig(group.Config, invalidKeys[i].Config)
		if effectiveConfig.ActiveProbeEnabled {
			continue
		}

		interval := time.Duration(effectiveConfig.KeyValidationIntervalMinutes) * time.Minute
		if interval <= 0 {
			continue
		}
		if invalidKeys[i].LastValidatedAt != nil && now.Sub(*invalidKeys[i].LastValidatedAt) < interval {
			continue
		}
		dueKeys = append(dueKeys, invalidKeys[i])
	}

	if len(dueKeys) == 0 {
		return
	}

	var becameValidCount int32
	var keyWg sync.WaitGroup
	jobs := make(chan *models.APIKey, len(dueKeys))

	concurrency := group.EffectiveConfig.KeyValidationConcurrency
	for range concurrency {
		keyWg.Add(1)
		go func() {
			defer keyWg.Done()
			for {
				select {
				case key, ok := <-jobs:
					if !ok {
						return
					}

					decryptedKey, err := s.decryptKey(key)
					if err != nil {
						logrus.WithError(err).WithField("key_id", key.ID).Error("CronChecker: Failed to decrypt key for validation, skipping")
						continue
					}

					keyForValidation := *key
					keyForValidation.KeyValue = decryptedKey

					isValid, _ := s.Validator.ValidateSingleKey(&keyForValidation, group)
					if isValid {
						atomic.AddInt32(&becameValidCount, 1)
					}
				case <-s.stopChan:
					return
				}
			}
		}()
	}

DistributeLoop:
	for i := range dueKeys {
		select {
		case jobs <- &dueKeys[i]:
		case <-s.stopChan:
			break DistributeLoop
		}
	}
	close(jobs)

	keyWg.Wait()

	if err := s.DB.Model(group).Update("last_validated_at", time.Now()).Error; err != nil {
		logrus.Errorf("CronChecker: Failed to update last_validated_at for group %s: %v", group.Name, err)
	}

	duration := time.Since(groupProcessStart)
	logrus.Infof(
		"CronChecker: Group '%s' validation finished. Total checked: %d, became valid: %d. Duration: %s.",
		group.Name,
		len(dueKeys),
		becameValidCount,
		duration.String(),
	)
}
