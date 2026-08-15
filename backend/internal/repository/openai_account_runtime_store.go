package repository

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const (
	openAIAccountRuntimeStatsPrefix          = "openai:runtime:stats:"
	openAIAccountRuntimeReportDedupeTTL      = 2 * time.Minute
	openAIAccountRuntimeReportDedupeMaxItems = int64(2048)
)

var reportOpenAIAccountRuntimeStatsScript = redis.NewScript(`
local error_sample = tonumber(ARGV[1])
local ttft_sample = nil
if ARGV[2] ~= '' then
  ttft_sample = tonumber(ARGV[2])
end
local observed_at = tonumber(ARGV[3])
local ttl_ms = tonumber(ARGV[4])
local report_id = ARGV[5]
local dedupe_at = tonumber(ARGV[6])
local dedupe_ttl_ms = tonumber(ARGV[7])
local dedupe_max_items = tonumber(ARGV[8])

if error_sample == nil or (error_sample ~= 0 and error_sample ~= 1) then
  return redis.error_reply('invalid runtime error sample')
end
if ARGV[2] ~= '' and (ttft_sample == nil or ttft_sample <= 0) then
  return redis.error_reply('invalid runtime TTFT sample')
end
if observed_at == nil or observed_at < 0 or ttl_ms == nil or ttl_ms <= 0 or report_id == '' or
   dedupe_at == nil or dedupe_at <= 0 or dedupe_ttl_ms == nil or dedupe_ttl_ms <= 0 or
   dedupe_max_items == nil or dedupe_max_items <= 0 or dedupe_max_items % 1 ~= 0 then
  return redis.error_reply('invalid runtime report metadata')
end

-- A client may replay a non-idempotent script after an ambiguous I/O error.
-- Keep recent report IDs in the same Redis Cluster slot and make that replay a
-- no-op. The sorted set is trimmed on every new report so active accounts do
-- not retain IDs forever.
if redis.call('ZSCORE', KEYS[3], report_id) ~= false then
  return 0
end

local states = {}
for i = 1, 2 do
  local values = redis.call(
    'HMGET', KEYS[i],
    'error_rate_ewma',
    'ttft_ewma',
    'sample_count',
    'ttft_updated_at',
    'last_failure_at',
    'last_success_at'
  )
  local known_field_count = 0
  for field_index = 1, 6 do
    if values[field_index] ~= false then
      known_field_count = known_field_count + 1
    end
  end
  if redis.call('HLEN', KEYS[i]) ~= known_field_count then
    return redis.error_reply('corrupt runtime hash fields')
  end

  local error_rate = 0
  if values[1] ~= false then
    error_rate = tonumber(values[1])
    if error_rate == nil or error_rate < 0 or error_rate > 1 then
      return redis.error_reply('corrupt runtime error_rate_ewma')
    end
  end

  local ttft = nil
  if values[2] ~= false then
    ttft = tonumber(values[2])
    if ttft == nil or ttft <= 0 then
      return redis.error_reply('corrupt runtime ttft_ewma')
    end
  end

  local sample_count = 0
  if values[3] ~= false then
    sample_count = tonumber(values[3])
    if sample_count == nil or sample_count < 0 or sample_count % 1 ~= 0 then
      return redis.error_reply('corrupt runtime sample_count')
    end
  end

  local timestamps = {}
  for field_index = 4, 6 do
    local timestamp = -1
    if values[field_index] ~= false then
      timestamp = tonumber(values[field_index])
      if timestamp == nil or timestamp < 0 or timestamp % 1 ~= 0 then
        return redis.error_reply('corrupt runtime timestamp')
      end
    end
    timestamps[field_index] = timestamp
  end

  states[i] = {
    error_rate = error_rate,
    ttft = ttft,
    sample_count = sample_count,
    ttft_updated_at = timestamps[4],
    last_failure_at = timestamps[5],
    last_success_at = timestamps[6],
    latest_event_at = math.max(timestamps[4], timestamps[5], timestamps[6])
  }
end

-- Validation above must finish before the first write: Redis does not roll
-- back earlier script writes when a later command raises an error.
redis.call('ZREMRANGEBYSCORE', KEYS[3], '-inf', dedupe_at - dedupe_ttl_ms)
redis.call('ZADD', KEYS[3], dedupe_at, report_id)
local dedupe_count = redis.call('ZCARD', KEYS[3])
if dedupe_count > dedupe_max_items then
  redis.call('ZREMRANGEBYRANK', KEYS[3], 0, dedupe_count - dedupe_max_items - 1)
end

for i = 1, 2 do
  local state = states[i]
  -- Each scope independently ignores observations older than its latest event.
  -- Equal microsecond timestamps remain valid for genuinely concurrent calls.
  if observed_at >= state.latest_event_at then
    local error_rate = 0.2 * error_sample + 0.8 * state.error_rate
    redis.call('HSET', KEYS[i], 'error_rate_ewma', string.format('%.17g', error_rate))

    -- Account stats keep a TTFT observed before a later failure. Model stats
    -- only learn latency from successful requests, matching the in-memory path.
    if ttft_sample ~= nil and (i == 1 or error_sample == 0) then
      local ttft = ttft_sample
      if state.ttft ~= nil then
        ttft = 0.2 * ttft_sample + 0.8 * state.ttft
      end
      redis.call(
        'HSET', KEYS[i],
        'ttft_ewma', string.format('%.17g', ttft),
        'sample_count', string.format('%.0f', state.sample_count + 1)
      )
      if observed_at > state.ttft_updated_at then
        redis.call('HSET', KEYS[i], 'ttft_updated_at', ARGV[3])
      end
    end

    if error_sample == 1 and observed_at > state.last_failure_at then
      redis.call('HSET', KEYS[i], 'last_failure_at', ARGV[3])
    end
    if error_sample == 0 and observed_at > state.last_success_at then
      redis.call('HSET', KEYS[i], 'last_success_at', ARGV[3])
    end
  end
  redis.call('PEXPIRE', KEYS[i], ttl_ms)
end
redis.call('PEXPIRE', KEYS[3], dedupe_ttl_ms)
return 1
`)

func openAIAccountRuntimeStatsKeys(accountID int64, normalizedModel string) (accountKey, modelKey, dedupeKey string) {
	hashTag := fmt.Sprintf("{%d}", accountID)
	base := openAIAccountRuntimeStatsPrefix + hashTag
	modelHash := sha256.Sum256([]byte(normalizedModel))
	return base + ":account", base + ":model:" + hex.EncodeToString(modelHash[:]), base + ":dedupe"
}

func newOpenAIAccountRuntimeReportID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate OpenAI runtime report ID: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func (c *gatewayCache) Report(
	ctx context.Context,
	accountID int64,
	model string,
	success bool,
	firstTokenMs *int,
	observedAt time.Time,
	ttl time.Duration,
) error {
	reportID, err := newOpenAIAccountRuntimeReportID()
	if err != nil {
		return err
	}
	return c.reportOpenAIAccountRuntimeStats(ctx, accountID, model, success, firstTokenMs, observedAt, ttl, reportID)
}

func (c *gatewayCache) reportOpenAIAccountRuntimeStats(
	ctx context.Context,
	accountID int64,
	model string,
	success bool,
	firstTokenMs *int,
	observedAt time.Time,
	ttl time.Duration,
	reportID string,
) error {
	if c == nil || c.rdb == nil {
		return errors.New("OpenAI account runtime stats store unavailable")
	}
	normalizedModel := service.NormalizeOpenAIAccountRuntimeModel(model)
	if accountID <= 0 {
		return errors.New("OpenAI account runtime stats account ID must be positive")
	}
	if normalizedModel == "" {
		return errors.New("OpenAI account runtime stats model is required")
	}
	if ttl <= 0 {
		return errors.New("OpenAI account runtime stats TTL must be positive")
	}
	if reportID == "" {
		return errors.New("OpenAI account runtime stats report ID is required")
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	observedAtMicros := observedAt.UnixMicro()
	if observedAtMicros < 0 {
		return errors.New("OpenAI account runtime stats observed time must be after the Unix epoch")
	}

	ttlMillis := ttl.Milliseconds()
	if ttlMillis <= 0 {
		ttlMillis = 1
	}
	ttftArg := ""
	if firstTokenMs != nil && *firstTokenMs > 0 {
		ttftArg = strconv.Itoa(*firstTokenMs)
	}
	errorSample := "1"
	if success {
		errorSample = "0"
	}
	accountKey, modelKey, dedupeKey := openAIAccountRuntimeStatsKeys(accountID, normalizedModel)
	if _, err := reportOpenAIAccountRuntimeStatsScript.Run(
		ctx,
		c.rdb,
		[]string{accountKey, modelKey, dedupeKey},
		errorSample,
		ttftArg,
		strconv.FormatInt(observedAtMicros, 10),
		strconv.FormatInt(ttlMillis, 10),
		reportID,
		strconv.FormatInt(time.Now().UnixMilli(), 10),
		strconv.FormatInt(openAIAccountRuntimeReportDedupeTTL.Milliseconds(), 10),
		strconv.FormatInt(openAIAccountRuntimeReportDedupeMaxItems, 10),
	).Result(); err != nil {
		return fmt.Errorf("report OpenAI account runtime stats: %w", err)
	}
	return nil
}

func (c *gatewayCache) Load(
	ctx context.Context,
	accountID int64,
	model string,
) (service.OpenAIAccountRuntimeStatSnapshot, service.OpenAIAccountRuntimeStatSnapshot, error) {
	if c == nil || c.rdb == nil {
		return service.OpenAIAccountRuntimeStatSnapshot{}, service.OpenAIAccountRuntimeStatSnapshot{}, errors.New("OpenAI account runtime stats store unavailable")
	}
	normalizedModel := service.NormalizeOpenAIAccountRuntimeModel(model)
	if accountID <= 0 {
		return service.OpenAIAccountRuntimeStatSnapshot{}, service.OpenAIAccountRuntimeStatSnapshot{}, errors.New("OpenAI account runtime stats account ID must be positive")
	}
	if normalizedModel == "" {
		return service.OpenAIAccountRuntimeStatSnapshot{}, service.OpenAIAccountRuntimeStatSnapshot{}, errors.New("OpenAI account runtime stats model is required")
	}

	accountKey, modelKey, _ := openAIAccountRuntimeStatsKeys(accountID, normalizedModel)
	pipe := c.rdb.Pipeline()
	accountCommand := pipe.HGetAll(ctx, accountKey)
	modelCommand := pipe.HGetAll(ctx, modelKey)
	if _, err := pipe.Exec(ctx); err != nil {
		return service.OpenAIAccountRuntimeStatSnapshot{}, service.OpenAIAccountRuntimeStatSnapshot{}, fmt.Errorf("load OpenAI account runtime stats: %w", err)
	}
	accountSnapshot, err := parseOpenAIAccountRuntimeStatSnapshot(accountKey, accountCommand.Val())
	if err != nil {
		return service.OpenAIAccountRuntimeStatSnapshot{}, service.OpenAIAccountRuntimeStatSnapshot{}, err
	}
	modelSnapshot, err := parseOpenAIAccountRuntimeStatSnapshot(modelKey, modelCommand.Val())
	if err != nil {
		return service.OpenAIAccountRuntimeStatSnapshot{}, service.OpenAIAccountRuntimeStatSnapshot{}, err
	}
	return accountSnapshot, modelSnapshot, nil
}

func parseOpenAIAccountRuntimeStatSnapshot(key string, values map[string]string) (service.OpenAIAccountRuntimeStatSnapshot, error) {
	var snapshot service.OpenAIAccountRuntimeStatSnapshot
	if len(values) == 0 {
		return snapshot, nil
	}
	knownFields := map[string]bool{
		"error_rate_ewma": true,
		"ttft_ewma":       true,
		"sample_count":    true,
		"ttft_updated_at": true,
		"last_failure_at": true,
		"last_success_at": true,
	}
	for field := range values {
		if !knownFields[field] {
			return snapshot, fmt.Errorf("parse OpenAI account runtime stats %q: unknown field %q", key, field)
		}
	}

	var err error
	if raw, ok := values["error_rate_ewma"]; ok {
		snapshot.ErrorRateEWMA, err = parseOpenAIAccountRuntimeFloat(key, "error_rate_ewma", raw, true)
		if err != nil {
			return snapshot, err
		}
	}
	if raw, ok := values["ttft_ewma"]; ok {
		snapshot.TTFTEWMA, err = parseOpenAIAccountRuntimeFloat(key, "ttft_ewma", raw, false)
		if err != nil {
			return snapshot, err
		}
		if snapshot.TTFTEWMA <= 0 {
			return snapshot, fmt.Errorf("parse OpenAI account runtime stats %q field %q: value must be positive", key, "ttft_ewma")
		}
	}
	if raw, ok := values["sample_count"]; ok {
		snapshot.SampleCount, err = parseOpenAIAccountRuntimeInt(key, "sample_count", raw)
		if err != nil {
			return snapshot, err
		}
		if snapshot.SampleCount < 0 {
			return snapshot, fmt.Errorf("parse OpenAI account runtime stats %q field %q: value must not be negative", key, "sample_count")
		}
	}
	if snapshot.TTFTUpdatedAt, err = parseOpenAIAccountRuntimeTime(key, "ttft_updated_at", values); err != nil {
		return snapshot, err
	}
	if snapshot.LastFailureAt, err = parseOpenAIAccountRuntimeTime(key, "last_failure_at", values); err != nil {
		return snapshot, err
	}
	if snapshot.LastSuccessAt, err = parseOpenAIAccountRuntimeTime(key, "last_success_at", values); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func parseOpenAIAccountRuntimeFloat(key, field, raw string, unitInterval bool) (float64, error) {
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("parse OpenAI account runtime stats %q field %q: invalid float %q", key, field, raw)
	}
	if value < 0 || (unitInterval && value > 1) {
		return 0, fmt.Errorf("parse OpenAI account runtime stats %q field %q: out-of-range value %q", key, field, raw)
	}
	return value, nil
}

func parseOpenAIAccountRuntimeInt(key, field, raw string) (int64, error) {
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse OpenAI account runtime stats %q field %q: invalid integer %q", key, field, raw)
	}
	return value, nil
}

func parseOpenAIAccountRuntimeTime(key, field string, values map[string]string) (time.Time, error) {
	raw, ok := values[field]
	if !ok {
		return time.Time{}, nil
	}
	unixMicros, err := parseOpenAIAccountRuntimeInt(key, field, raw)
	if err != nil {
		return time.Time{}, err
	}
	if unixMicros < 0 {
		return time.Time{}, fmt.Errorf("parse OpenAI account runtime stats %q field %q: value must not be negative", key, field)
	}
	return time.UnixMicro(unixMicros).UTC(), nil
}
