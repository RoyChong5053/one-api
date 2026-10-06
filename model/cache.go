package model

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Laisky/errors/v2"
	"github.com/Laisky/zap"

	"github.com/Laisky/one-api/common"
	"github.com/Laisky/one-api/common/config"
	"github.com/Laisky/one-api/common/logger"
	"github.com/Laisky/one-api/dto"
)

var (
	TokenCacheSeconds           = config.SyncFrequency
	UserId2GroupCacheSeconds    = config.SyncFrequency
	UserId2QuotaCacheSeconds    = config.SyncFrequency
	UserId2StatusCacheSeconds   = config.SyncFrequency
	UserId2UsernameCacheSeconds = config.SyncFrequency
	GroupModelsCacheSeconds     = config.SyncFrequency
)

func CacheGetTokenByKey(ctx context.Context, key string) (*Token, error) {
	lg := logger.FromContext(ctx)
	keyCol := "`key`"
	if common.UsingPostgreSQL.Load() {
		keyCol = `"key"`
	}
	var token Token
	if !common.IsRedisEnabled() {
		if DB == nil {
			return nil, errors.New("database not initialized")
		}
		err := DB.Where(keyCol+" = ?", key).First(&token).Error
		if err != nil {
			return nil, errors.Wrapf(err, "get token by key %s", key)
		}
		return &token, nil
	}
	tokenObjectString, err := common.RedisGet(ctx, fmt.Sprintf("token:%s", key))
	if err != nil {
		if DB == nil {
			return nil, errors.Wrap(err, "database not initialized")
		}
		err := DB.Where(keyCol+" = ?", key).First(&token).Error
		if err != nil {
			return nil, errors.Wrapf(err, "get token by key %s", key)
		}
		// Marshal without custom Token.MarshalJSON to keep raw key in cache
		type plainToken Token
		jsonBytes, err := json.Marshal(plainToken(token))
		if err != nil {
			return nil, errors.Wrapf(err, "marshal token %d for cache", token.Id)
		}
		err = common.RedisSet(ctx, fmt.Sprintf("token:%s", key), string(jsonBytes), time.Duration(TokenCacheSeconds)*time.Second)
		if err != nil {
			lg.Warn("Redis set token failed, continuing without cache", zap.String("key", key), zap.Error(err))
		}
		return &token, nil
	}

	err = json.Unmarshal([]byte(tokenObjectString), &token)
	if err != nil {
		return nil, errors.Wrapf(err, "unmarshal cached token for key %s", key)
	}
	return &token, nil
}

// UserId2UserCacheSeconds controls the TTL for the full user object cache.
var UserId2UserCacheSeconds = config.SyncFrequency

// CacheGetUserById retrieves a full User (minus password/access_token) by ID,
// using Redis when available. On cache miss it falls back to GetUserById and populates the cache.
func CacheGetUserById(ctx context.Context, id int) (*User, error) {
	lg := logger.FromContext(ctx)
	if !common.IsRedisEnabled() {
		return GetUserById(id, false)
	}
	cacheKey := fmt.Sprintf("user_obj:%d", id)
	cached, err := common.RedisGet(ctx, cacheKey)
	if err == nil {
		var user User
		if jsonErr := json.Unmarshal([]byte(cached), &user); jsonErr != nil {
			lg.Warn("Redis cached user object corrupted, falling back to database", zap.Int("user_id", id), zap.Error(jsonErr))
		} else {
			return &user, nil
		}
	}
	user, err := GetUserById(id, false)
	if err != nil {
		return nil, errors.Wrapf(err, "get user %d from database", id)
	}
	payload, err := json.Marshal(user)
	if err != nil {
		lg.Warn("failed to marshal user for cache", zap.Int("user_id", id), zap.Error(err))
		return user, nil
	}
	if setErr := common.RedisSet(ctx, cacheKey, string(payload), time.Duration(UserId2UserCacheSeconds)*time.Second); setErr != nil {
		lg.Warn("Redis set user object failed, continuing without cache", zap.Int("user_id", id), zap.Error(setErr))
	}
	return user, nil
}

func CacheGetUserGroup(ctx context.Context, id int) (group string, err error) {
	lg := logger.FromContext(ctx)
	if !common.IsRedisEnabled() {
		return GetUserGroup(id)
	}
	group, err = common.RedisGet(ctx, fmt.Sprintf("user_group:%d", id))
	if err != nil {
		group, err = GetUserGroup(id)
		if err != nil {
			return "", errors.Wrapf(err, "get user group for user %d", id)
		}
		err = common.RedisSet(ctx, fmt.Sprintf("user_group:%d", id), group, time.Duration(UserId2GroupCacheSeconds)*time.Second)
		if err != nil {
			lg.Warn("Redis set user group failed, continuing without cache", zap.Int("user_id", id), zap.Error(err))
		}
	}
	if err != nil {
		return group, errors.Wrapf(err, "cache user group for user %d", id)
	}
	return group, nil
}

// CacheGetUsername retrieves a username by user ID, using Redis cache when available.
// On cache miss it falls back to GetUsernameById and populates the cache.
// Empty usernames (non-existent users) are not cached to allow retry on the next request.
func CacheGetUsername(ctx context.Context, id int) string {
	lg := logger.FromContext(ctx)
	if !common.IsRedisEnabled() {
		return GetUsernameById(id)
	}
	username, err := common.RedisGet(ctx, fmt.Sprintf("user_username:%d", id))
	if err != nil {
		username = GetUsernameById(id)
		if username == "" {
			return username
		}
		if setErr := common.RedisSet(ctx, fmt.Sprintf("user_username:%d", id), username, time.Duration(UserId2UsernameCacheSeconds)*time.Second); setErr != nil {
			lg.Warn("Redis set username failed, continuing without cache", zap.Int("user_id", id), zap.Error(setErr))
		}
	}
	return username
}

func fetchAndUpdateUserQuota(ctx context.Context, id int) (quota int64, err error) {
	lg := logger.FromContext(ctx)
	quota, err = GetUserQuota(id)
	if err != nil {
		return 0, errors.Wrap(err, "get user quota")
	}
	err = common.RedisSet(ctx, fmt.Sprintf("user_quota:%d", id), fmt.Sprintf("%d", quota), time.Duration(UserId2QuotaCacheSeconds)*time.Second)
	if err != nil {
		lg.Warn("Redis set user quota failed, continuing without cache", zap.Int("user_id", id), zap.Error(err))
	}
	return
}

func CacheGetUserQuota(ctx context.Context, id int) (quota int64, err error) {
	lg := logger.FromContext(ctx)
	if !common.IsRedisEnabled() {
		return GetUserQuota(id)
	}
	quotaString, err := common.RedisGet(ctx, fmt.Sprintf("user_quota:%d", id))
	if err != nil {
		return fetchAndUpdateUserQuota(ctx, id)
	}
	quota, err = strconv.ParseInt(quotaString, 10, 64)
	if err != nil {
		return 0, nil
	}
	if quota <= config.PreConsumedQuota { // when user's quota is less than pre-consumed quota, we need to fetch from db
		lg.Info("user's cached quota is too low, refreshing from db", zap.Int64("quota", quota), zap.Int("user_id", id))
		return fetchAndUpdateUserQuota(ctx, id)
	}
	return quota, nil
}

func CacheUpdateUserQuota(ctx context.Context, id int) error {
	if !common.IsRedisEnabled() {
		return nil
	}
	quota, err := GetUserQuota(id)
	if err != nil {
		return errors.Wrapf(err, "get database quota for user %d", id)
	}
	err = common.RedisSet(ctx, fmt.Sprintf("user_quota:%d", id), fmt.Sprintf("%d", quota), time.Duration(UserId2QuotaCacheSeconds)*time.Second)
	if err != nil {
		return errors.Wrapf(err, "set cached quota for user %d", id)
	}
	return nil
}

func CacheDecreaseUserQuota(ctx context.Context, id int, quota int64) error {
	if !common.IsRedisEnabled() {
		return nil
	}
	err := common.RedisDecrease(ctx, fmt.Sprintf("user_quota:%d", id), int64(quota))
	if err != nil {
		return errors.Wrapf(err, "decrease cached quota for user %d", id)
	}
	return nil
}

func CacheIsUserEnabled(ctx context.Context, userId int) (bool, error) {
	lg := logger.FromContext(ctx)
	if !common.IsRedisEnabled() {
		return IsUserEnabled(userId)
	}
	enabled, err := common.RedisGet(ctx, fmt.Sprintf("user_enabled:%d", userId))
	if err == nil {
		return enabled == "1", nil
	}

	userEnabled, err := IsUserEnabled(userId)
	if err != nil {
		return false, errors.Wrapf(err, "check user %d enabled", userId)
	}
	enabled = "0"
	if userEnabled {
		enabled = "1"
	}
	err = common.RedisSet(ctx, fmt.Sprintf("user_enabled:%d", userId), enabled, time.Duration(UserId2StatusCacheSeconds)*time.Second)
	if err != nil {
		lg.Warn("Redis set user enabled failed, continuing without cache", zap.Int("user_id", userId), zap.Error(err))
	}
	if err != nil {
		return userEnabled, errors.Wrapf(err, "cache enabled status for user %d", userId)
	}
	return userEnabled, nil
}

// CacheGetGroupModels returns models of a group
//
// Deprecated: use CacheGetGroupModelsV2 instead
func CacheGetGroupModels(ctx context.Context, group string) (models []string, err error) {
	lg := logger.FromContext(ctx)
	if !common.IsRedisEnabled() {
		return GetGroupModels(ctx, group)
	}
	modelsStr, err := common.RedisGet(ctx, fmt.Sprintf("group_models:%s", group))
	if err == nil {
		return strings.Split(modelsStr, ","), nil
	}
	models, err = GetGroupModels(ctx, group)
	if err != nil {
		return nil, errors.Wrap(err, "get group models")
	}
	err = common.RedisSet(ctx, fmt.Sprintf("group_models:%s", group), strings.Join(models, ","), time.Duration(GroupModelsCacheSeconds)*time.Second)
	if err != nil {
		lg.Warn("Redis set group models failed, continuing without cache", zap.String("group", group), zap.Error(err))
	}
	return models, nil
}

// CacheGetGroupModelsV2 is a version of CacheGetGroupModels that returns EnabledAbility instead of string
func CacheGetGroupModelsV2(ctx context.Context, group string) (models []dto.EnabledAbility, err error) {
	lg := logger.FromContext(ctx)
	if !common.IsRedisEnabled() {
		return GetGroupModelsV2(ctx, group)
	}
	modelsStr, err := common.RedisGet(ctx, fmt.Sprintf("group_models_v2:%s", group))
	if err != nil {
		lg.Debug("Redis cache miss for group models, falling back to database", zap.String("group", group), zap.Error(err))
	} else {
		if err = json.Unmarshal([]byte(modelsStr), &models); err != nil {
			lg.Warn("Redis cached group models data corrupted, falling back to database", zap.String("group", group), zap.Error(err))
		} else {
			return models, nil
		}
	}

	models, err = GetGroupModelsV2(ctx, group)
	if err != nil {
		return nil, errors.Wrap(err, "get group models")
	}

	cachePayload, err := json.Marshal(models)
	if err != nil {
		lg.Warn("failed to marshal group models for cache, continuing without cache", zap.String("group", group), zap.Error(err))
		return models, nil
	}

	err = common.RedisSet(ctx, fmt.Sprintf("group_models_v2:%s", group), string(cachePayload),
		time.Duration(GroupModelsCacheSeconds)*time.Second)
	if err != nil {
		lg.Warn("Redis set group models failed, continuing without cache", zap.String("group", group), zap.Error(err))
	}

	return models, nil
}

var group2model2channels map[string]map[string][]*Channel
var channelSyncLock sync.RWMutex

// ---------------------------------------------------------------------------
// Channel health tracking and instant circuit-breaking
//
// The health scoring engine itself lives in channel_health.go; this section
// owns only the circuit-breaker side of it (suspension bookkeeping).
// ---------------------------------------------------------------------------

var (
	// suspendedChannels holds channels that have been temporarily excluded
	// from selection by the in-memory circuit breaker (before the periodic
	// SYNC_FREQUENCY cache rebuild picks up the DB suspension).
	// The value is the time until which the channel is excluded.
	suspendedChannels   = make(map[int]time.Time)
	suspendedChannelsMu sync.RWMutex
)

// InvalidateChannelInCache marks a channel as temporarily unavailable in
// the in-memory cache so that subsequent requests skip it. The exclusion
// lasts for the given duration, after which the channel may be selected again.
// This is a fast-path before the periodic SYNC_FREQUENCY cache rebuild.
func InvalidateChannelInCache(channelId int, duration time.Duration) {
	suspendedChannelsMu.Lock()
	defer suspendedChannelsMu.Unlock()
	suspendedChannels[channelId] = time.Now().Add(duration)
}

// suspendedUntil returns the expiry of a channel's in-memory circuit-breaker
// suspension, and whether one is active.
func suspendedUntil(channelId int) (time.Time, bool) {
	suspendedChannelsMu.RLock()
	defer suspendedChannelsMu.RUnlock()
	until, ok := suspendedChannels[channelId]
	if !ok {
		return time.Time{}, false
	}
	if time.Now().After(until) {
		return time.Time{}, false
	}
	return until, true
}

// removeExpiredSuspensions cleans up entries whose suspension has expired.
// Called periodically and during channel selection.
func removeExpiredSuspensions() {
	now := time.Now()
	suspendedChannelsMu.Lock()
	defer suspendedChannelsMu.Unlock()
	for id, until := range suspendedChannels {
		if now.After(until) {
			delete(suspendedChannels, id)
		}
	}
}

// IsChannelSuspendedInCache reports whether the channel is currently excluded
// by the in-memory circuit breaker.
func IsChannelSuspendedInCache(channelId int) bool {
	suspendedChannelsMu.RLock()
	defer suspendedChannelsMu.RUnlock()
	until, ok := suspendedChannels[channelId]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		return false
	}
	return true
}

// rebuildSuspendedChannelsFromDB reloads suspension state from the abilities
// table. This is called alongside InitChannelCache to keep the fast-path in sync.
//
// The map is rebuilt rather than merged: abilities rows carry the authoritative
// per-model suspension, and a previously merged rebuild would leave channels
// suspended forever once their DB row expired.
func rebuildSuspendedChannelsFromDB() {
	var abilities []*Ability
	DB.Find(&abilities)
	now := time.Now()

	fresh := make(map[int]time.Time)
	for _, a := range abilities {
		if a.SuspendUntil == nil || !a.SuspendUntil.After(now) {
			continue
		}
		// A channel may serve several models; keep the furthest expiry so
		// the channel stays out of selection until every suspended
		// ability has come back.
		if existing, ok := fresh[a.ChannelId]; !ok || a.SuspendUntil.After(existing) {
			fresh[a.ChannelId] = *a.SuspendUntil
		}
	}

	suspendedChannelsMu.Lock()
	defer suspendedChannelsMu.Unlock()
	for id := range suspendedChannels {
		if _, ok := fresh[id]; !ok {
			delete(suspendedChannels, id)
		}
	}
	for id, until := range fresh {
		suspendedChannels[id] = until
	}
}

func InitChannelCache() {
	newChannelId2channel := make(map[int]*Channel)
	var channels []*Channel
	DB.Where("status = ?", ChannelStatusEnabled).Find(&channels)
	for _, channel := range channels {
		newChannelId2channel[channel.Id] = channel
	}

	var allAbilities []*Ability
	DB.Find(&allAbilities) // Fetch all abilities

	// Filter abilities: must be enabled and not currently suspended
	// And create a quick lookup map for valid abilities
	// key: "group:model:channelId"
	validAbilityMap := make(map[string]bool)
	now := time.Now()
	for _, ability := range allAbilities {
		// Ensure the ability corresponds to an enabled channel (via ability.Enabled flag)
		// and is not currently suspended.
		// The ability.Enabled should have been set correctly based on channel.Status during AddAbilities/UpdateAbilities.
		if ability.Enabled && (ability.SuspendUntil == nil || ability.SuspendUntil.Before(now)) {
			// Check if the channel itself is in our list of enabled channels
			if _, channelExists := newChannelId2channel[ability.ChannelId]; channelExists {
				key := fmt.Sprintf("%s:%s:%d", ability.Group, ability.Model, ability.ChannelId)
				validAbilityMap[key] = true
			}
		}
	}

	newGroup2model2channels := make(map[string]map[string][]*Channel)

	// Iterate over channels that are confirmed to be enabled
	for _, channel := range channels { // channels are already filtered by status = ChannelStatusEnabled
		channelGroups := channel.GetGroupNames()
		channelModels := channel.GetSupportedModelNames()

		for _, groupName := range channelGroups {
			if _, ok := newGroup2model2channels[groupName]; !ok {
				newGroup2model2channels[groupName] = make(map[string][]*Channel)
			}
			for _, modelName := range channelModels {
				// Check if this specific ability (group, model, channel.Id) is in our valid map
				abilityKey := fmt.Sprintf("%s:%s:%d", groupName, modelName, channel.Id)
				if _, isValidAbility := validAbilityMap[abilityKey]; isValidAbility {
					if _, ok := newGroup2model2channels[groupName][modelName]; !ok {
						newGroup2model2channels[groupName][modelName] = make([]*Channel, 0)
					}
					// Add the channel to the cache for this group and model
					newGroup2model2channels[groupName][modelName] = append(newGroup2model2channels[groupName][modelName], channel)
				}
			}
		}
	}

	// sort by priority
	for group, model2channels := range newGroup2model2channels {
		for model, channels := range model2channels {
			sort.Slice(channels, func(i, j int) bool {
				return channels[i].GetPriority() > channels[j].GetPriority()
			})
			newGroup2model2channels[group][model] = channels
		}
	}

	channelSyncLock.Lock()
	group2model2channels = newGroup2model2channels
	channelSyncLock.Unlock()

	// Rebuild the fast-path in-memory suspension map so that the circuit breaker
	// does not need to wait for the next periodic sync.
	rebuildSuspendedChannelsFromDB()

	logger.Logger.Info("channels synced from database, considering suspensions")
}

func SyncChannelCache(frequency int) {
	for {
		time.Sleep(time.Duration(frequency) * time.Second)
		logger.Logger.Info("syncing channels from database")
		InitChannelCache()
	}
}

// CleanExpiredSuspensions periodically removes expired in-memory suspension
// entries. Call this in a background goroutine on startup.
//
// Prefer StartChannelHealthJanitor, which runs this loop and also GCs health
// records for deleted channels.
func CleanExpiredSuspensions() {
	for {
		time.Sleep(30 * time.Second)
		removeExpiredSuspensions()
	}
}

func GetChannelsFromCache(group string, model string) ([]*Channel, error) {
	if !config.MemoryCacheEnabled {
		return nil, errors.New("MemoryCache is disabled")
	}
	channelSyncLock.RLock()
	channelsFromCache := group2model2channels[group][model]
	if len(channelsFromCache) == 0 {
		channelSyncLock.RUnlock()
		return nil, errors.New("channel not found in memory cache")
	}

	candidateChannels := make([]*Channel, len(channelsFromCache))
	copy(candidateChannels, channelsFromCache)
	channelSyncLock.RUnlock()

	return candidateChannels, nil
}

// FetchChannelsForModel queries the database directly for enabled, non-suspended
// channels that support the given group and model. This acts as a fallback when
// the in-memory cache is disabled or hasn't been populated yet.
func FetchChannelsForModel(group, model string) ([]*Channel, error) {
	groupCol := "`group`"
	trueVal := "1"
	if common.UsingPostgreSQL.Load() {
		groupCol = `"group"`
		trueVal = "true"
	}

	var channelIDs []int
	now := time.Now()
	if err := DB.Model(&Ability{}).
		Where(groupCol+" = ? AND model = ? AND enabled = "+trueVal+" AND (suspend_until IS NULL OR suspend_until < ?)",
			group, model, now).
		Pluck("channel_id", &channelIDs).Error; err != nil {
		return nil, errors.Wrap(err, "query abilities for model channels")
	}
	if len(channelIDs) == 0 {
		return nil, errors.Errorf("no channels available for model %s in group %s", model, group)
	}

	var channels []*Channel
	if err := DB.Where("id IN (?) AND status = ?", channelIDs, ChannelStatusEnabled).Find(&channels).Error; err != nil {
		return nil, errors.Wrap(err, "load channels from DB")
	}

	return channels, nil
}

func CacheGetSatisfiedChannel(group string, model string, preferLowestPriority bool) (*Channel, error) {
	if !config.MemoryCacheEnabled {
		return GetRandomSatisfiedChannel(group, model, preferLowestPriority)
	}
	channelSyncLock.RLock()
	// It is important to make a copy if we are going to modify or iterate outside lock,
	// or ensure operations are safe. Here, we are just reading.
	channelsFromCache := group2model2channels[group][model]

	// Create a new slice to operate on, to avoid issues if the underlying array is changed by a concurrent Sync.
	// And to filter out channels that might have been suspended since cache was built.
	// However, for simplicity and given SyncChannelCache rebuilds the map,
	// we'll rely on SyncChannelCache to clear out suspended channels periodically.
	// A live check here would add DB calls, negating some cache benefits.
	// The current InitChannelCache already filters by suspension.
	// If a channel is suspended *between* syncs, this cache might serve it.
	// The application's retry logic will then handle it.

	if len(channelsFromCache) == 0 {
		channelSyncLock.RUnlock()
		return nil, errors.New("channel not found in memory cache")
	}

	// Make a copy to safely work with outside the lock for selection logic
	candidateChannels := make([]*Channel, len(channelsFromCache))
	copy(candidateChannels, channelsFromCache)
	channelSyncLock.RUnlock()

	if len(candidateChannels) == 0 {
		return nil, errors.Errorf("no channels in cache support model %s", model)
	}

	// Liveness for IP-literal upstreams is handled out of band by the LAN
	// supervisor (controller/channel_local_probe.go), which auto-disables an
	// unreachable node and auto-recovers it. Keeping a probe on this path
	// stalled every selection by the TCP blackhole budget and only suspended
	// the channel for a window short enough that it was promptly re-selected.
	endIdx := len(candidateChannels)
	// choose by priority
	if endIdx == 0 { // Should be caught by earlier check, but as a safeguard
		return nil, errors.New("no channels available after cache check")
	}
	firstChannel := candidateChannels[0]
	if firstChannel.GetPriority() > 0 {
		for i := range candidateChannels {
			if candidateChannels[i].GetPriority() != firstChannel.GetPriority() {
				endIdx = i
				break
			}
		}
	}

	if config.DefaultUseMinMaxTokensModel {
		candidateChannels = candidateChannels[:endIdx]

		sort.Slice(candidateChannels, func(i, j int) bool {
			iModelConfig, jModelConfig := candidateChannels[i].GetModelConfig(model), candidateChannels[j].GetModelConfig(model)
			// Treat 0 as infinity (no limit)
			if iModelConfig == nil || iModelConfig.MaxTokens == 0 {
				return false // i has no limit, so it's not less than j
			}
			if jModelConfig == nil || jModelConfig.MaxTokens == 0 {
				return true // j has no limit, so i is less than j
			}

			return iModelConfig.MaxTokens < jModelConfig.MaxTokens
		})

		minTokensChannel := candidateChannels[0]
		minTokensModelConfig := minTokensChannel.GetModelConfig(model)
		if minTokensModelConfig.MaxTokens > 0 {
			for i := range candidateChannels {
				modelConfig := candidateChannels[i].GetModelConfig(model)
				if modelConfig.MaxTokens != minTokensModelConfig.MaxTokens {
					endIdx = i
					break
				}
			}
		}
	}

	var channel *Channel
	if preferLowestPriority && endIdx < len(candidateChannels) {
		// The lower-priority tail still gets band-gated selection. It used
		// to be a uniform random pick, which meant a known-dead channel
		// sitting at the back of the queue was as likely to be picked as a
		// healthy one.
		channel = selectByHealthBand(candidateChannels[endIdx:], model)
		logger.Logger.Debug("select channel in cache (lowest priority, health-banded)",
			zap.String("channel_name", channel.Name), zap.Int("channel_id", channel.Id))
		return channel, nil
	}
	channel = selectByHealthBand(candidateChannels[:endIdx], model)
	logger.Logger.Info("select channel in cache", zap.String("channel_name", channel.Name), zap.Int("channel_id", channel.Id))
	return channel, nil
}

// CacheGetSatisfiedChannelExcluding selects a channel by strict priority ordering
// while excluding specified channel IDs and provider types.
//
// Priority is the primary sort key: the highest-priority tier is always selected first.
// Within the same priority tier, health-weighted selection picks the healthiest channel.
//
// When excludedProviderTypes is non-empty, channels whose Type field appears in this map are
// excluded from selection. If no candidates remain after provider filtering, the exclusion is
// dropped and all remaining candidates are considered (fallback within same provider).
//
// preferLowestPriority=true inverts the selection to pick from the lowest priority tier instead.
func CacheGetSatisfiedChannelExcluding(group string, model string, preferLowestPriority bool, excludeChannelIds map[int]bool, excludedProviderTypes map[int]bool, tryLargerMaxTokens bool) (*Channel, error) {
	if !config.MemoryCacheEnabled {
		return GetRandomSatisfiedChannelExcluding(group, model, preferLowestPriority, excludeChannelIds)
	}

	removeExpiredSuspensions()

	channelSyncLock.RLock()
	channelsFromCache := group2model2channels[group][model]

	if len(channelsFromCache) == 0 {
		channelSyncLock.RUnlock()
		return nil, errors.New("channel not found in memory cache")
	}

	// Filter out excluded channels and in-memory suspended channels
	var candidateChannels []*Channel
	for _, channel := range channelsFromCache {
		if excludeChannelIds[channel.Id] {
			continue
		}
		if IsChannelSuspendedInCache(channel.Id) {
			continue
		}
		candidateChannels = append(candidateChannels, channel)
	}

	// For HTTP Code 413
	// Filter out small max_tokens channels
	if tryLargerMaxTokens {
		smallerMaxTokensSizes := make(map[int32]bool)
		for _, channel := range channelsFromCache {
			if excludeChannelIds[channel.Id] {
				modelConfig := channel.GetModelConfig(model)
				if modelConfig != nil {
					smallerMaxTokensSizes[modelConfig.MaxTokens] = true
				}
			}
		}

		var LargerMaxTokensSizeChannels []*Channel
		// Work on already-filtered candidateChannels, not the original channelsFromCache
		for _, channel := range candidateChannels {
			modelConfig := channel.GetModelConfig(model)
			if modelConfig != nil && !smallerMaxTokensSizes[modelConfig.MaxTokens] {
				LargerMaxTokensSizeChannels = append(LargerMaxTokensSizeChannels, channel)
			} else if modelConfig == nil {
				LargerMaxTokensSizeChannels = append(LargerMaxTokensSizeChannels, channel)
			}
		}

		candidateChannels = LargerMaxTokensSizeChannels
	}
	channelSyncLock.RUnlock()

	if len(candidateChannels) == 0 {
		return nil, errors.Errorf("no available channels support model %s after exclusions", model)
	}

	// NEW: Exclude channels belonging to failed provider types.
	// If no candidates remain after provider filtering, fall back to all candidates.
	if len(excludedProviderTypes) > 0 {
		var providerFiltered []*Channel
		for _, ch := range candidateChannels {
			if !excludedProviderTypes[ch.Type] {
				providerFiltered = append(providerFiltered, ch)
			}
		}
		if len(providerFiltered) > 0 {
			candidateChannels = providerFiltered
		} else {
			logger.Logger.Info("all providers excluded, falling back to same-provider channels",
				zap.Int("excluded_providers", len(excludedProviderTypes)),
				zap.Int("candidates_before_fallback", len(candidateChannels)),
			)
		}
	}

	// Liveness for IP-literal upstreams is handled out of band by the LAN
	// supervisor; see CacheGetSatisfiedChannel for why it is not on this path.

	// When preferLowestPriority is true, select from the lowest priority tier.
	// When preferLowestPriority is false, select from the highest priority tier.
	// Priority is always the primary sort key — selection never crosses tiers randomly.
	if preferLowestPriority {
		// Find the boundary where highest priority channels end
		endIdx := len(candidateChannels)
		firstChannel := candidateChannels[0]
		if firstChannel.GetPriority() > 0 {
			for i := range candidateChannels {
				if candidateChannels[i].GetPriority() != firstChannel.GetPriority() {
					endIdx = i
					break
				}
			}
		}

		// If there are lower priority channels available, select from them
		if endIdx < len(candidateChannels) {
			channel := selectByHealthBand(candidateChannels[endIdx:], model)
			logger.Logger.Debug("select channel in cache (lowest priority, health-banded)", zap.String("channel_name", channel.Name), zap.Int("channel_id", channel.Id))
			return channel, nil
		} else {
			// No lower priority channels available, return error to indicate we should try a different approach
			return nil, errors.New("no lower priority channels available after excluding failed channels")
		}
	} else {
		// Select from highest priority channels among the available candidates
		// Since candidateChannels maintains the original cache order (sorted by priority desc),
		// we need to find the highest priority among the remaining candidates
		if len(candidateChannels) == 0 {
			return nil, errors.New("no candidate channels available")
		}

		// Find the maximum priority among available candidates
		maxPriority := candidateChannels[0].GetPriority()
		for _, channel := range candidateChannels {
			if channel.GetPriority() > maxPriority {
				maxPriority = channel.GetPriority()
			}
		}

		// Collect channels with the maximum priority
		var maxPriorityChannels []*Channel
		for _, channel := range candidateChannels {
			if channel.GetPriority() == maxPriority {
				maxPriorityChannels = append(maxPriorityChannels, channel)
			}
		}

		if len(maxPriorityChannels) == 0 {
			return nil, errors.New("no channels with maximum priority available")
		}

		channel := selectByHealthBand(maxPriorityChannels, model)
		logger.Logger.Debug("select channel in cache (highest priority, health-banded)", zap.String("channel_name", channel.Name), zap.Int("channel_id", channel.Id))
		return channel, nil
	}
}

// selectByHealthBand picks a channel from a set that already shares one
// priority, in two stages.
//
// Stage one is a hard gate on health. Channels are bucketed into healthy,
// degraded and unhealthy by their composite health score, and only the best
// non-empty bucket is considered. This is why a channel with weight=100 but an
// unhealthy band receives zero traffic: the configured weight is never allowed
// to outvote the health verdict.
//
// Stage two applies the operator's configured Weight, but only as a tie-breaker
// among channels that already landed in the same bucket. Weight=3 therefore
// means "send about three times the traffic of its equally-healthy peers", not
// "always prefer this one".
//
// The final fallback exists so a total outage in the health signal degrades into
// "spread traffic anyway" rather than an error. When every candidate is
// unhealthy there is no better option, and starving them all would turn a
// degraded fleet into a hard outage.
func selectByHealthBand(channels []*Channel, model string) *Channel {
	if len(channels) == 0 {
		return nil
	}
	if len(channels) == 1 {
		return channels[0]
	}

	var healthy, degraded, unhealthy []*Channel
	for _, ch := range channels {
		switch GetChannelHealthBand(ch.Id) {
		case BandHealthy:
			healthy = append(healthy, ch)
		case BandDegraded, BandUnknown:
			// Unknown is grouped with degraded rather than healthy:
			// a channel with too little evidence to judge should not
			// outrank one that has been demonstrably working.
			degraded = append(degraded, ch)
		default:
			unhealthy = append(unhealthy, ch)
		}
	}

	pool := healthy
	switch {
	case len(pool) > 0:
	case len(degraded) > 0:
		pool = degraded
		logger.Logger.Debug("no healthy channel available, falling back to degraded",
			zap.String("model", model),
			zap.Int("degraded", len(degraded)),
			zap.Int("unhealthy", len(unhealthy)),
		)
	case len(unhealthy) > 0:
		pool = unhealthy
		logger.Logger.Warn("all candidate channels are unhealthy, falling back to full set",
			zap.String("model", model),
			zap.Int("candidates", len(unhealthy)),
		)
	default:
		return channels[0]
	}

	// Inside the chosen band, apply the configured weight. Any channel whose
	// weight is unset reads as 1 via GetWeight, so this degrades gracefully to
	// a uniform pick across an evenly configured tier.
	totalWeight := 0.0
	weights := make([]float64, len(pool))
	for i, ch := range pool {
		w := float64(ch.GetWeight())
		if w <= 0 {
			w = 1
		}
		weights[i] = w
		totalWeight += w
	}
	if totalWeight <= 0 {
		return pool[0]
	}

	r := rand.Float64() * totalWeight
	cumulative := 0.0
	for i, w := range weights {
		cumulative += w
		if r < cumulative {
			return pool[i]
		}
	}
	return pool[len(pool)-1]
}

// selectByHealthWeight is retained for callers and tests written against the old
// name. It now applies band gating rather than multiplying weight by a floored
// health score.
func selectByHealthWeight(channels []*Channel, model string) *Channel {
	return selectByHealthBand(channels, model)
}
