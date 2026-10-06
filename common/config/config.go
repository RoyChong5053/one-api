// Package config provides centralized configuration management for one-api.
//
// This file defines all environment variables and runtime configuration options
// used throughout the application. Variables are organized into logical groups
// based on their functionality. Each group contains a header comment explaining
// its purpose and usage context.
//
// # Environment Variable Loading
//
// Environment variables are loaded at package initialization time using the
// env helper package. Default values are provided for all variables to ensure
// the application can start with minimal configuration.
//
// # Configuration Groups
//
// The configuration is organized into the following groups:
//   - Server Configuration: Core server settings (port, mode, node type)
//   - Session & Cookie Security: Authentication session management
//   - Database Configuration: Primary and logging database settings
//   - Redis Configuration: Cache and distributed state settings
//   - API Format Detection: Automatic request format handling
//   - Channel Management: Upstream provider channel settings
//   - Rate Limiting: Request throttling at various levels
//   - Billing & Quota: Token consumption and quota management
//   - Relay & Timeout: Upstream request handling
//   - Batch Update System: Background usage aggregation
//   - Metrics & Monitoring: Prometheus and health monitoring
//   - Logging Configuration: Log rotation and retention
//   - Proxy Configuration: HTTP proxy settings for requests
//   - Provider-Specific Settings: Gemini, OpenRouter, etc.
//   - UI & Theme: Frontend appearance settings
//   - Token & API Key: Token generation settings
//   - Testing Configuration: Smoke test settings
//   - Runtime Variables: Dynamic settings modified at runtime
//   - Authentication Providers: Login method configurations
//   - Email Configuration: SMTP and email settings
//   - Push Notifications: External notification settings
//   - Security Features: Turnstile and related security
//   - User Quota Settings: Registration and referral quotas
package config

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Laisky/one-api/common/env"
)

// =============================================================================
// SERVER CONFIGURATION
// =============================================================================
// Core server settings that control how the one-api process runs, including
// the HTTP server port, Gin framework mode, and cluster node type.
// These settings are typically configured once at deployment time.

var (
	// ServerPort overrides the --port flag when running inside container or
	// PaaS environments. When set, the HTTP server listens on this port instead
	// of the command-line argument.
	//
	// Environment variable: PORT
	// Default: "" (uses command-line --port flag, typically 3000)
	// Example: "8080", "3000"
	ServerPort = strings.TrimSpace(env.String("PORT", ""))

	// GinMode controls the Gin HTTP framework's operating mode.
	// Use "release" for production deployments to disable debug features.
	//
	// Environment variable: GIN_MODE
	// Default: "release"
	// Allowed values: "debug", "release", "test"
	GinMode = strings.TrimSpace(env.String("GIN_MODE", "release"))

	// IsMasterNode determines whether this process should serve the web UI.
	// In multi-node deployments, only master nodes serve the dashboard while
	// slave nodes handle only API traffic.
	//
	// Environment variable: NODE_TYPE
	// Default: true (master node)
	// Set to "slave" to disable dashboard serving
	IsMasterNode = !strings.EqualFold(env.String("NODE_TYPE", ""), "slave")

	// ShutdownTimeoutSec specifies the graceful shutdown timeout for the HTTP
	// server and background workers. During shutdown, the server stops accepting
	// new requests and waits up to this duration for in-flight requests to complete.
	//
	// Environment variable: SHUTDOWN_TIMEOUT
	// Default: 360 (6 minutes)
	// Unit: seconds
	ShutdownTimeoutSec = env.Int("SHUTDOWN_TIMEOUT", 360)

	// FrontendBaseURL redirects dashboard traffic to an external frontend.
	// Useful when hosting the UI separately from the API server.
	// Follower/slave nodes ignore this setting.
	//
	// Environment variable: FRONTEND_BASE_URL
	// Default: "" (serve embedded frontend)
	// Example: "https://dashboard.example.com"
	FrontendBaseURL = strings.TrimSuffix(strings.TrimSpace(env.String("FRONTEND_BASE_URL", "")), "/")

	// MaxItemsPerPage caps paginated API and UI responses to keep database
	// queries predictable and prevent excessive memory usage.
	//
	// Environment variable: MAX_ITEMS_PER_PAGE
	// Default: 100
	// Range: positive integer
	MaxItemsPerPage = env.Int("MAX_ITEMS_PER_PAGE", 100)

	// TokenTransactionsMaxHistory caps the maximum number of historical
	// transactions retrievable for a token via the API.
	//
	// Environment variable: TOKEN_TRANSACTIONS_MAX_HISTORY
	// Default: 1000
	// Range: positive integer
	TokenTransactionsMaxHistory = env.Int("TOKEN_TRANSACTIONS_MAX_HISTORY", 1000)
)

// =============================================================================
// SESSION & COOKIE SECURITY
// =============================================================================
// Settings that control user session management, including session encryption
// and cookie behavior. These settings are critical for security and should be
// configured appropriately for production deployments.

var (
	// SessionSecretEnvValue keeps the raw SESSION_SECRET input so other packages
	// can warn about placeholder values. This is the unprocessed environment value.
	//
	// Environment variable: SESSION_SECRET
	// Default: "" (will generate random secret)
	SessionSecretEnvValue = strings.TrimSpace(env.String("SESSION_SECRET", ""))

	// SessionSecret stores the effective session secret used for encrypting
	// session data. When the provided secret is absent or has an unsupported
	// length (not 16, 24, or 32 bytes), it is replaced with a random secret
	// or hashed to a 32-byte base64 token in init().
	//
	// IMPORTANT: Set a stable secret in production to preserve sessions across restarts.
	//
	// Environment variable: SESSION_SECRET
	// Default: randomly generated 32-byte base64 string
	// Recommended lengths: 16, 24, or 32 bytes
	SessionSecret = SessionSecretEnvValue

	// CookieMaxAgeHours controls how long session cookies stay valid.
	// The value is interpreted in hours by the session store.
	// Users will need to re-authenticate after this period.
	//
	// Environment variable: COOKIE_MAXAGE_HOURS
	// Default: 168 (7 days)
	// Unit: hours
	CookieMaxAgeHours = env.Int("COOKIE_MAXAGE_HOURS", 168)

	// EnableCookieSecure forces the browser to send session cookies only over
	// HTTPS. Defaults to `true` because that is what production deployments
	// must use — without it, Safari/iOS Chrome may evict the cookie under ITP
	// while `localStorage` survives, producing repeated logout/redirect cycles
	// on mobile even when the site is served over HTTPS.
	//
	// Local HTTP development should explicitly set ENABLE_COOKIE_SECURE=false.
	// Tying the flag to DEBUG was considered and rejected: operators sometimes
	// enable DEBUG in production for log verbosity during incident response,
	// and the cookie posture must not change in that case.
	//
	// Environment variable: ENABLE_COOKIE_SECURE
	// Default: true
	// Allowed values: true, false
	EnableCookieSecure = env.Bool("ENABLE_COOKIE_SECURE", true)
)

// =============================================================================
// WEBAUTHN / PASSKEY CONFIGURATION
// =============================================================================
// Settings for WebAuthn (Passkey) authentication.  The Relying Party (RP)
// values must match the domain that serves the login page.

var (
	// WebAuthnRPID is the Relying Party Identifier, typically the domain
	// name without port (e.g. "example.com").  When empty the RP ID is
	// derived from ServerAddress at runtime.
	//
	// Environment variable: WEBAUTHN_RP_ID
	// Default: "" (derived from ServerAddress)
	WebAuthnRPID = strings.TrimSpace(env.String("WEBAUTHN_RP_ID", ""))

	// WebAuthnRPOrigins lists the allowed origins for WebAuthn ceremonies,
	// comma-separated (e.g. "https://example.com,https://www.example.com").
	// When empty the origin is derived from ServerAddress at runtime.
	//
	// Environment variable: WEBAUTHN_RP_ORIGINS
	// Default: "" (derived from ServerAddress)
	WebAuthnRPOrigins = strings.TrimSpace(env.String("WEBAUTHN_RP_ORIGINS", ""))
)

// =============================================================================
// DATABASE CONFIGURATION
// =============================================================================
// Settings for the primary database and optional logging database connections.
// One-api supports both MySQL/PostgreSQL (via SQL_DSN) and SQLite (via SQLITE_PATH).
// Connection pool settings should be tuned based on expected load.

var (
	// SQLDSN provides the primary database DSN (Data Source Name).
	// When empty, SQLite is used with the path specified by SQLITE_PATH.
	// For MySQL/PostgreSQL, provide a full connection string.
	//
	// Environment variable: SQL_DSN
	// Default: "" (uses SQLite)
	// MySQL example: "user:password@tcp(localhost:3306)/oneapi?charset=utf8mb4&parseTime=True&loc=Local"
	// PostgreSQL example: "host=localhost user=postgres password=secret dbname=oneapi port=5432 sslmode=disable"
	SQLDSN = strings.TrimSpace(env.String("SQL_DSN", ""))

	// SQLitePath specifies the SQLite database file path when SQL_DSN is absent.
	// The file will be created if it doesn't exist.
	//
	// Environment variable: SQLITE_PATH
	// Default: "one-api.db"
	// Example: "/var/lib/one-api/data.db"
	SQLitePath = env.String("SQLITE_PATH", "one-api.db")

	// SQLiteBusyTimeout configures SQLite busy timeout to mitigate locking errors
	// during concurrent access. Higher values reduce lock errors but increase latency.
	//
	// Environment variable: SQLITE_BUSY_TIMEOUT
	// Default: 10000 (10 seconds) — long enough to absorb write bursts without
	// surfacing transient lock errors; pairs with WAL journaling and the
	// sqlite_retry helper for production-grade resilience.
	// Unit: milliseconds
	SQLiteBusyTimeout = env.Int("SQLITE_BUSY_TIMEOUT", 10000)

	// SQLMaxIdleConns controls the primary database pool's idle connection count.
	// Set based on expected concurrent connections and database server capacity.
	//
	// Environment variable: SQL_MAX_IDLE_CONNS
	// Default: 200
	SQLMaxIdleConns = env.Int("SQL_MAX_IDLE_CONNS", 200)

	// SQLMaxOpenConns controls the primary database pool's maximum open connections.
	// Limit this based on database server connection limits.
	//
	// Environment variable: SQL_MAX_OPEN_CONNS
	// Default: 2000
	SQLMaxOpenConns = env.Int("SQL_MAX_OPEN_CONNS", 2000)

	// SQLMaxLifetimeSeconds sets how long database connections live before being
	// recycled. Helps balance connection freshness with connection setup overhead.
	//
	// Environment variable: SQL_MAX_LIFETIME
	// Default: 300 (5 minutes)
	// Unit: seconds
	SQLMaxLifetimeSeconds = env.Int("SQL_MAX_LIFETIME", 300)

	// LogSQLDSN overrides the DSN used for the logging database.
	// Useful for separating high-volume logging writes from transactional data.
	// Falls back to SQL_DSN when empty.
	//
	// Environment variable: LOG_SQL_DSN
	// Default: "" (uses SQL_DSN)
	LogSQLDSN = env.String("LOG_SQL_DSN", "")
)

// =============================================================================
// REDIS CONFIGURATION
// =============================================================================
// Redis is used for distributed caching, rate limiting, and session storage
// in multi-node deployments. When Redis is not configured, the application
// falls back to in-memory caching (suitable for single-node deployments).

var (
	// RedisConnString defines the Redis connection string.
	// Leaving it empty disables Redis features and uses in-memory alternatives.
	//
	// Environment variable: REDIS_CONN_STRING
	// Default: "" (Redis disabled)
	// Standalone example: "localhost:6379"
	// With database: "localhost:6379/0"
	RedisConnString = strings.TrimSpace(env.String("REDIS_CONN_STRING", ""))

	// RedisMasterName enables Redis Sentinel/Cluster discovery when provided.
	// Use this for high-availability Redis deployments.
	//
	// Environment variable: REDIS_MASTER_NAME
	// Default: "" (standalone mode)
	// Example: "mymaster"
	RedisMasterName = strings.TrimSpace(env.String("REDIS_MASTER_NAME", ""))

	// RedisPassword supplies the Redis authentication password when required.
	//
	// Environment variable: REDIS_PASSWORD
	// Default: "" (no authentication)
	RedisPassword = env.String("REDIS_PASSWORD", "")

	// MemoryCacheEnabled forces the in-process cache to stay enabled even with Redis.
	// Useful for reducing Redis load with a local cache layer.
	//
	// Environment variable: MEMORY_CACHE_ENABLED
	// Default: false
	MemoryCacheEnabled = env.Bool("MEMORY_CACHE_ENABLED", false)

	// RateLimitKeyExpirationDuration controls how long Redis keys for rate limiting
	// remain valid. Should be longer than the longest rate limit window.
	RateLimitKeyExpirationDuration = 20 * time.Minute
)

// =============================================================================
// DEBUG CONFIGURATION
// =============================================================================
// Settings for development and debugging. These should generally be disabled
// in production for security and performance reasons.

var (
	// DebugEnabled toggles verbose structured logging when DEBUG=true.
	// Adds detailed request/response logging useful for troubleshooting.
	//
	// Environment variable: DEBUG
	// Default: false
	// WARNING: May log sensitive information; disable in production
	DebugEnabled = env.Bool("DEBUG", false)

	// DebugSQLEnabled toggles per-query SQL logging when DEBUG_SQL=true.
	// Useful for debugging database queries but generates high log volume.
	//
	// Environment variable: DEBUG_SQL
	// Default: false
	// WARNING: High log volume; disable in production
	DebugSQLEnabled = env.Bool("DEBUG_SQL", false)
)

// =============================================================================
// API FORMAT DETECTION
// =============================================================================
// One-api supports multiple API formats (ChatCompletion, Response API, Claude
// Messages). These settings control automatic format detection and handling
// when clients send requests to incorrect endpoints.

var (
	// AutoDetectAPIFormat enables automatic detection of API request format when
	// a client sends a request to an incorrect endpoint (e.g., Response API format
	// to /v1/chat/completions). This provides seamless compatibility with
	// misbehaving clients.
	//
	// Environment variable: AUTO_DETECT_API_FORMAT
	// Default: true
	AutoDetectAPIFormat = env.Bool("AUTO_DETECT_API_FORMAT", true)

	// AutoDetectAPIFormatAction specifies the action when a format mismatch is detected.
	//
	// Environment variable: AUTO_DETECT_API_FORMAT_ACTION
	// Default: "transparent"
	// Allowed values:
	//   - "transparent": process the request transparently in the correct format
	//   - "redirect": return a 302 redirect to the correct endpoint
	AutoDetectAPIFormatAction = strings.ToLower(strings.TrimSpace(env.String("AUTO_DETECT_API_FORMAT_ACTION", "transparent")))
)

// =============================================================================
// CHANNEL MANAGEMENT
// =============================================================================
// Settings for managing upstream provider channels, including suspension
// behavior after errors, automatic health testing, and channel disabling.
// Channels represent connections to AI providers like OpenAI, Anthropic, etc.

var (
	// ChannelSuspendSecondsFor5XX defines how long an ability is paused after
	// upstream 5xx (server error) failures. Prevents hammering failing providers.
	//
	// Environment variable: CHANNEL_SUSPEND_SECONDS_FOR_5XX
	// Default: 30 seconds
	ChannelSuspendSecondsFor5XX = time.Second * time.Duration(env.Int("CHANNEL_SUSPEND_SECONDS_FOR_5XX", 30))

	// ChannelSuspendSecondsForAuth defines the backoff window applied after
	// quota/auth/permission errors (e.g., invalid API key, exceeded quota).
	//
	// Environment variable: CHANNEL_SUSPEND_SECONDS_FOR_AUTH
	// Default: 60 seconds
	ChannelSuspendSecondsForAuth = time.Second * time.Duration(env.Int("CHANNEL_SUSPEND_SECONDS_FOR_AUTH", 60))

	// ChannelTestFrequencyRaw retains the raw CHANNEL_TEST_FREQUENCY input for
	// validation and documentation purposes.
	//
	// Environment variable: CHANNEL_TEST_FREQUENCY
	// Default: "" (disabled)
	ChannelTestFrequencyRaw = strings.TrimSpace(env.String("CHANNEL_TEST_FREQUENCY", ""))

	// ChannelTestFrequency triggers automatic channel health probes when greater
	// than zero. The value specifies minutes between probes.
	//
	// Environment variable: CHANNEL_TEST_FREQUENCY
	// Default: 0 (disabled)
	// Unit: minutes
	// Example: 360 (test every 6 hours, recommended for free-tier channels)
	ChannelTestFrequency = func() int {
		if ChannelTestFrequencyRaw == "" {
			return 0
		}
		v, err := strconv.Atoi(ChannelTestFrequencyRaw)
		if err != nil {
			panic(fmt.Sprintf("invalid CHANNEL_TEST_FREQUENCY: %q", ChannelTestFrequencyRaw))
		}
		if v < 0 {
			return 0
		}
		return v
	}()

	// ChannelDisableThreshold defines the failure ratio that triggers automatic
	// channel disablement when AutomaticDisableChannelEnabled is true.
	//
	// Runtime variable (set via admin UI)
	// Default: 5.0 (500% - effectively disabled by default)
	ChannelDisableThreshold = 5.0

	// AutomaticDisableChannelEnabled enables automatic channel disabling when
	// failure rate exceeds ChannelDisableThreshold.
	//
	// Runtime variable (set via admin UI)
	// Default: false
	AutomaticDisableChannelEnabled = false

	// AutomaticEnableChannelEnabled re-enables channels automatically when
	// their health recovers and they pass health checks.
	//
	// Runtime variable (set via admin UI)
	// Default: false
	AutomaticEnableChannelEnabled = false

	// SyncFrequency controls how frequently option/channel caches refresh from
	// the database. Set to 0 to disable automatic syncing.
	//
	// Environment variable: SYNC_FREQUENCY
	// Default: 120 (2 minutes)
	// Unit: seconds
	SyncFrequency = env.Int("SYNC_FREQUENCY", 2*60)

	// ChannelHealthWindowSize defines the sliding window size for tracking channel
	// health. A larger window smooths out transient blips but reacts slower to
	// sustained degradation.
	//
	// Environment variable: CHANNEL_HEALTH_WINDOW_SIZE
	// Default: 50
	// Unit: requests
	ChannelHealthWindowSize = env.Int("CHANNEL_HEALTH_WINDOW_SIZE", 50)

	// ChannelHealthThreshold is the lower bound of the healthy band: a channel
	// whose composite health score is at or above this value competes normally.
	//
	// Environment variable: CHANNEL_HEALTH_THRESHOLD
	// Default: 0.7
	// Range: 0.0 – 1.0
	ChannelHealthThreshold = env.Float64("CHANNEL_HEALTH_THRESHOLD", 0.7)

	// ChannelHealthDegradedThreshold is the lower bound of the degraded band.
	// Channels below it are unhealthy: they are excluded from selection whenever
	// at least one healthy candidate exists for the same (group, model, priority).
	//
	// Environment variable: CHANNEL_HEALTH_DEGRADED_THRESHOLD
	// Default: 0.35
	// Range: 0.0 – 1.0
	ChannelHealthDegradedThreshold = env.Float64("CHANNEL_HEALTH_DEGRADED_THRESHOLD", 0.35)

	// ChannelHealthMinSamples is the number of observations required before a
	// channel's band is trusted. Below this the band is "unknown" and the channel
	// is treated as healthy, so a freshly added channel is not penalised before it
	// has served any traffic.
	//
	// Environment variable: CHANNEL_HEALTH_MIN_SAMPLES
	// Default: 3
	ChannelHealthMinSamples = env.Int("CHANNEL_HEALTH_MIN_SAMPLES", 3)

	// ChannelHealthEWMAAlpha is the smoothing factor of the exponentially weighted
	// moving averages backing every health signal. Higher reacts faster to
	// degradation but is noisier.
	//
	// Environment variable: CHANNEL_HEALTH_EWMA_ALPHA
	// Default: 0.3
	// Range: (0.0 – 1.0]
	ChannelHealthEWMAAlpha = env.Float64("CHANNEL_HEALTH_EWMA_ALPHA", 0.3)

	// ChannelHealthLatencyFastMs / ChannelHealthLatencySlowMs bound the latency
	// score. At or below Fast the latency multiplier is 1.0; at or above Slow it
	// is 0.0. Values in between are interpolated linearly. SlowMs doubles as the
	// "degrade this channel" threshold the operator asked for (default 5s).
	//
	// Environment variables: CHANNEL_HEALTH_LATENCY_FAST_MS, CHANNEL_HEALTH_LATENCY_SLOW_MS
	// Unit: milliseconds
	ChannelHealthLatencyFastMs = float64(env.Int("CHANNEL_HEALTH_LATENCY_FAST_MS", 1000))
	ChannelHealthLatencySlowMs = float64(env.Int("CHANNEL_HEALTH_LATENCY_SLOW_MS", 5000))

	// ChannelHealthTTFTFastMs / ChannelHealthTTFTSlowMs bound the time-to-first-token
	// score using the same interpolation as the latency score. Non-streaming
	// responses have no TTFT and are exempt.
	//
	// Environment variables: CHANNEL_HEALTH_TTFT_FAST_MS, CHANNEL_HEALTH_TTFT_SLOW_MS
	// Unit: milliseconds
	ChannelHealthTTFTFastMs = float64(env.Int("CHANNEL_HEALTH_TTFT_FAST_MS", 500))
	ChannelHealthTTFTSlowMs = float64(env.Int("CHANNEL_HEALTH_TTFT_SLOW_MS", 5000))

	// ChannelHealthTPSFloor / ChannelHealthTPSGood bound the throughput score.
	// This is what catches providers that "fake" availability by accepting the
	// request but streaming at ~1 token/second. At or above Good the multiplier is
	// 1.0; at or below Floor it is 0.0.
	//
	// Environment variables: CHANNEL_HEALTH_TPS_FLOOR, CHANNEL_HEALTH_TPS_GOOD
	// Unit: tokens per second
	ChannelHealthTPSFloor = float64(env.Int("CHANNEL_HEALTH_TPS_FLOOR", 5))
	ChannelHealthTPSGood  = float64(env.Int("CHANNEL_HEALTH_TPS_GOOD", 20))

	// ChannelHealthStreamCutPenalty / ChannelHealthRateLimitPenalty are the
	// fractional score penalties applied at a cutRate / rateLimitRate of 1.0. A
	// channel that always cuts loses 70% of its score; one that is always rate
	// limited loses 50%.
	//
	// Environment variables: CHANNEL_HEALTH_STREAM_CUT_PENALTY, CHANNEL_HEALTH_RATE_LIMIT_PENALTY
	// Range: 0.0 – 1.0
	ChannelHealthStreamCutPenalty = env.Float64("CHANNEL_HEALTH_STREAM_CUT_PENALTY", 0.7)
	ChannelHealthRateLimitPenalty = env.Float64("CHANNEL_HEALTH_RATE_LIMIT_PENALTY", 0.5)

	// ChannelHealthDisableLatencyMs is the probe latency above which a channel is
	// considered hung and auto-disabled outright. Latencies between
	// ChannelHealthLatencySlowMs and this value only degrade the channel's score.
	//
	// Environment variable: CHANNEL_HEALTH_DISABLE_LATENCY_MS
	// Unit: milliseconds
	ChannelHealthDisableLatencyMs = int64(env.Int("CHANNEL_HEALTH_DISABLE_LATENCY_MS", 30000))

	// ChannelHealthProbeIdleMin is how long a channel may go without any
	// observation before the background prober considers it stale and refreshes
	// its score. Channels with live traffic are never probed unnecessarily.
	//
	// Environment variable: CHANNEL_HEALTH_PROBE_IDLE_MIN
	// Default: 180 (3 hours)
	// Unit: minutes
	ChannelHealthProbeIdleMin = env.Int("CHANNEL_HEALTH_PROBE_IDLE_MIN", 180)

	// ChannelHealthProbeMaxPerRun caps how many channels a single prober tick
	// refreshes, so a large fleet is spread across ticks instead of bursting.
	//
	// Environment variable: CHANNEL_HEALTH_PROBE_MAX_PER_RUN
	// Default: 8
	ChannelHealthProbeMaxPerRun = env.Int("CHANNEL_HEALTH_PROBE_MAX_PER_RUN", 8)

	// ChannelHealthProbeConcurrency bounds simultaneous probe requests.
	//
	// Environment variable: CHANNEL_HEALTH_PROBE_CONCURRENCY
	// Default: 4
	ChannelHealthProbeConcurrency = env.Int("CHANNEL_HEALTH_PROBE_CONCURRENCY", 4)

	// ChannelHealthProbeFrequency is how often the background health prober runs.
	// Set to 0 to disable it.
	//
	// Environment variable: CHANNEL_HEALTH_PROBE_FREQUENCY
	// Default: 60 (1 hour)
	// Unit: minutes
	ChannelHealthProbeFrequency = env.Int("CHANNEL_HEALTH_PROBE_FREQUENCY", 60)

	// ChannelAutoRecoverEnabled re-enables auto-disabled channels once the health
	// engine has seen ChannelAutoRecoverConsecutive clean probes. This is separate
	// from AutomaticEnableChannelEnabled, which re-enables on any single clean
	// test.
	//
	// Environment variable: CHANNEL_AUTO_RECOVER_ENABLED
	// Default: true
	ChannelAutoRecoverEnabled = env.Bool("CHANNEL_AUTO_RECOVER_ENABLED", true)

	// ChannelAutoRecoverConsecutive is how many consecutive clean probes are
	// required before an auto-disabled channel is re-enabled.
	//
	// Deprecated: recovery now resets the score to full after a single clean
	// probe, so this value is no longer consulted by the auto-recovery path.
	// It is retained for callers that still reference it.
	//
	// Environment variable: CHANNEL_AUTO_RECOVER_CONSECUTIVE
	// Default: 2
	ChannelAutoRecoverConsecutive = env.Int("CHANNEL_AUTO_RECOVER_CONSECUTIVE", 2)

	// ChannelHealthAutoDisableThreshold is the composite score below which an
	// enabled channel is auto-disabled outright. Sliding a channel through the
	// health bands only downranks it: with healthy peers present it receives no
	// traffic and therefore never accumulates the observations that would let it
	// recover. Auto-disabling it instead sends it through the periodic recovery
	// test, which resets the score to full on success. Set to 0 to disable.
	//
	// Environment variable: CHANNEL_HEALTH_AUTO_DISABLE_THRESHOLD
	// Default: 0.5
	ChannelHealthAutoDisableThreshold = env.Float64("CHANNEL_HEALTH_AUTO_DISABLE_THRESHOLD", 0.5)

	// LocalChannelProbeEnabled turns on the background supervisor for channels
	// whose upstream is an IP literal (LAN/GPU nodes, a local llama.cpp). The
	// supervisor pings the host first (ICMP, bounded) and only then does a TCP
	// connect and an HTTP readiness check, so a powered-off host is taken out of
	// rotation instead of black-holing requests for the full TCP SYN budget.
	//
	// Environment variable: LOCAL_CHANNEL_PROBE_ENABLED
	// Default: true
	LocalChannelProbeEnabled = env.Bool("LOCAL_CHANNEL_PROBE_ENABLED", true)

	// LocalChannelProbeIntervalSec is how often the LAN supervisor probes each
	// IP-literal channel. Local probes cost no upstream quota, so this can be
	// far more aggressive than the public health prober.
	//
	// Environment variable: LOCAL_CHANNEL_PROBE_INTERVAL_SEC
	// Default: 60
	LocalChannelProbeIntervalSec = env.Int("LOCAL_CHANNEL_PROBE_INTERVAL_SEC", 60)

	// LocalChannelProbeTimeoutMs bounds the ICMP/TCP/HTTP stages of one probe.
	//
	// Environment variable: LOCAL_CHANNEL_PROBE_TIMEOUT_MS
	// Default: 1500
	LocalChannelProbeTimeoutMs = env.Int("LOCAL_CHANNEL_PROBE_TIMEOUT_MS", 1500)

	// ChannelSuspendBackoffBase is the initial suspension duration used by the
	// exponential-backoff circuit breaker. Each consecutive failure multiplies
	// this value by ChannelSuspendBackoffMultiplier.
	//
	// Environment variable: CHANNEL_SUSPEND_BACKOFF_BASE
	// Default: 60 (seconds)
	// Unit: seconds
	ChannelSuspendBackoffBase = time.Second * time.Duration(env.Int("CHANNEL_SUSPEND_BACKOFF_BASE", 60))

	// ChannelSuspendBackoffMultiplier is the exponential factor applied to each
	// consecutive failure. Duration = base * multiplier^(failures-1), capped by
	// ChannelSuspendBackoffMax.
	//
	// Environment variable: CHANNEL_SUSPEND_BACKOFF_MULTIPLIER
	// Default: 5
	ChannelSuspendBackoffMultiplier = env.Int("CHANNEL_SUSPEND_BACKOFF_MULTIPLIER", 5)

	// ChannelSuspendBackoffMax caps the maximum suspension duration for the
	// exponential-backoff circuit breaker. Set this to ~24 h for providers
	// with daily quota resets (e.g. Google AI Studio).
	//
	// Environment variable: CHANNEL_SUSPEND_BACKOFF_MAX
	// Default: 86400 (24 hours)
	// Unit: seconds
	ChannelSuspendBackoffMax = time.Second * time.Duration(env.Int("CHANNEL_SUSPEND_BACKOFF_MAX", 86400))

	// ChannelRetryExhaustAll makes the relay try every available channel for
	// a model/group before returning a terminal error. When true, the retry
	// count is automatically derived from the number of candidate channels.
	//
	// Environment variable: CHANNEL_RETRY_EXHAUST_ALL
	// Default: true
	ChannelRetryExhaustAll = env.Bool("CHANNEL_RETRY_EXHAUST_ALL", true)
)

// =============================================================================
// RATE LIMITING
// =============================================================================
// Rate limiting settings protect the service from abuse and ensure fair usage.
// Different limits apply to different endpoint types (API, web UI, relay).
// Rate limits use sliding window counters in Redis (or memory if Redis unavailable).

var (
	// RateLimitDisabled turns off ALL rate limiting (global web/API/relay,
	// critical, upload/download, per-channel, low-balance relay, and TOTP) when
	// RATE_LIMIT_DISABLED=true. This is a behavioral toggle intended for local
	// development and tests; it is deliberately independent of DEBUG so that
	// DEBUG only affects logging and never changes runtime behavior.
	//
	// Environment variable: RATE_LIMIT_DISABLED
	// Default: false
	// WARNING: never enable in production; it removes all abuse protection.
	RateLimitDisabled = env.Bool("RATE_LIMIT_DISABLED", false)

	// GlobalApiRateLimitNum bounds the number of REST API requests per IP
	// within the GlobalApiRateLimitDuration window.
	//
	// Environment variable: GLOBAL_API_RATE_LIMIT
	// Default: 480 requests per 3 minutes
	GlobalApiRateLimitNum = env.Int("GLOBAL_API_RATE_LIMIT", 480)

	// GlobalApiRateLimitDuration sets the duration of the API rate limit window.
	// Combined with GlobalApiRateLimitNum to define the rate.
	//
	// Default: 180 seconds (3 minutes)
	// Unit: seconds
	GlobalApiRateLimitDuration int64 = 3 * 60

	// GlobalWebRateLimitNum bounds the number of dashboard/web UI requests per IP
	// within the GlobalWebRateLimitDuration window.
	//
	// Environment variable: GLOBAL_WEB_RATE_LIMIT
	// Default: 240 requests per 3 minutes
	GlobalWebRateLimitNum = env.Int("GLOBAL_WEB_RATE_LIMIT", 240)

	// GlobalWebRateLimitDuration sets the duration of the dashboard rate limit window.
	//
	// Default: 180 seconds (3 minutes)
	// Unit: seconds
	GlobalWebRateLimitDuration int64 = 3 * 60

	// GlobalRelayRateLimitNum bounds the number of relay API calls per token
	// within the GlobalRelayRateLimitDuration window. This limits AI API requests.
	//
	// Environment variable: GLOBAL_RELAY_RATE_LIMIT
	// Default: 480 requests per 3 minutes
	GlobalRelayRateLimitNum = env.Int("GLOBAL_RELAY_RATE_LIMIT", 480)

	// GlobalRelayRateLimitDuration sets the duration of the relay token rate limit window.
	//
	// Default: 180 seconds (3 minutes)
	// Unit: seconds
	GlobalRelayRateLimitDuration int64 = 3 * 60

	// LowBalanceThreshold is the account balance, in USD, below which a stricter
	// relay rate limit is applied. It targets free users who never top up but keep
	// hammering free models. Users whose balance is at or above this value keep the
	// standard GlobalRelayRateLimitNum limit.
	//
	// Environment variable: LOW_BALANCE_RATE_LIMIT_THRESHOLD
	// Default: 0.5 (USD)
	LowBalanceThreshold = env.Float64("LOW_BALANCE_RATE_LIMIT_THRESHOLD", 0.5)

	// LowBalanceRelayRateLimitNum bounds the number of relay API calls allowed for a
	// low-balance user (balance below LowBalanceThreshold) within
	// LowBalanceRelayRateLimitDuration. It defaults to GlobalRelayRateLimitNum so the
	// out-of-the-box behavior is identical to the standard relay limit; operators
	// lower it (or lengthen the window) to throttle users who have not topped up.
	// The limiter engages only when its effective rate (this count divided by
	// LowBalanceRelayRateLimitDuration) is stricter than the global relay rate;
	// otherwise it stays a transparent no-op.
	//
	// Environment variable: LOW_BALANCE_RELAY_RATE_LIMIT
	// Default: same as GlobalRelayRateLimitNum
	LowBalanceRelayRateLimitNum = env.Int("LOW_BALANCE_RELAY_RATE_LIMIT", GlobalRelayRateLimitNum)

	// LowBalanceRelayRateLimitDuration sets the window for the low-balance relay limit.
	// Together with LowBalanceRelayRateLimitNum it defines the effective rate compared
	// against the global relay rate to decide whether the low-balance limiter engages.
	//
	// Environment variable: LOW_BALANCE_RELAY_RATE_LIMIT_DURATION
	// Default: same as GlobalRelayRateLimitDuration
	// Unit: seconds
	LowBalanceRelayRateLimitDuration = int64(env.Int("LOW_BALANCE_RELAY_RATE_LIMIT_DURATION", int(GlobalRelayRateLimitDuration)))

	// ChannelRateLimitEnabled toggles per-channel rate limiting when true.
	// When enabled, each channel has its own request limit defined in channel settings.
	//
	// Environment variable: GLOBAL_CHANNEL_RATE_LIMIT
	// Default: false
	ChannelRateLimitEnabled = env.Bool("GLOBAL_CHANNEL_RATE_LIMIT", false)

	// ChannelRateLimitDuration sets the duration of the per-channel rate limit window.
	//
	// Default: 180 seconds (3 minutes)
	// Unit: seconds
	ChannelRateLimitDuration int64 = 3 * 60

	// CriticalRateLimitNum defines the burst control for high sensitivity endpoints
	// like password reset, registration, and other security-critical operations.
	//
	// Environment variable: CRITICAL_RATE_LIMIT
	// Default: 20 requests per 20 minutes
	CriticalRateLimitNum = env.Int("CRITICAL_RATE_LIMIT", 20)

	// CriticalRateLimitDuration sets the window for critical rate limiting.
	//
	// Default: 1200 seconds (20 minutes)
	// Unit: seconds
	CriticalRateLimitDuration int64 = 20 * 60

	// UploadRateLimitNum bounds the number of file uploads allowed per client
	// within UploadRateLimitDuration.
	//
	// Default: 10 uploads per minute
	UploadRateLimitNum = 10

	// UploadRateLimitDuration sets the upload rate limit window.
	//
	// Default: 60 seconds
	// Unit: seconds
	UploadRateLimitDuration int64 = 60

	// DownloadRateLimitNum bounds the number of file downloads allowed per client
	// within DownloadRateLimitDuration.
	//
	// Default: 10 downloads per minute
	DownloadRateLimitNum = 10

	// DownloadRateLimitDuration sets the download rate limit window.
	//
	// Default: 60 seconds
	// Unit: seconds
	DownloadRateLimitDuration int64 = 60
)

// =============================================================================
// BILLING & QUOTA
// =============================================================================
// Settings related to token consumption tracking, quota management, and billing.
// These control how usage is measured, reserved, and reconciled.

var (
	// PreconsumeTokenForBackgroundRequest reserves quota for asynchronous
	// background requests that only report usage after completion (e.g., video
	// generation tasks). Prevents quota overrun during long-running operations.
	//
	// Environment variable: PRECONSUME_TOKEN_FOR_BACKGROUND_REQUEST
	// Default: 15000 tokens
	PreconsumeTokenForBackgroundRequest = env.Int("PRECONSUME_TOKEN_FOR_BACKGROUND_REQUEST", 15000)

	// PreConsumedQuota sets the default quota reservation for requests to avoid
	// race conditions where multiple concurrent requests could exceed quota.
	//
	// Runtime variable (set via admin UI)
	// Default: 500
	PreConsumedQuota int64 = 500

	// BillingTimeoutSec is the maximum time allowed for billing reconciliation
	// before failing the request. Long timeouts support streaming responses.
	//
	// Environment variable: BILLING_TIMEOUT
	// Default: 300 (5 minutes)
	// Unit: seconds
	BillingTimeoutSec = env.Int("BILLING_TIMEOUT", 300)

	// StreamingBillingIntervalSec determines how frequently streaming sessions
	// checkpoint usage. More frequent checkpoints reduce data loss on disconnection.
	//
	// Environment variable: STREAMING_BILLING_INTERVAL
	// Default: 3 seconds
	// Unit: seconds
	StreamingBillingIntervalSec = env.Int("STREAMING_BILLING_INTERVAL", 3)

	// ExternalBillingDefaultTimeoutSec sets the default hold duration for external
	// billing reserves. Used when integrating with external billing systems.
	//
	// Environment variable: EXTERNAL_BILLING_DEFAULT_TIMEOUT
	// Default: 600 (10 minutes)
	// Unit: seconds
	ExternalBillingDefaultTimeoutSec = env.Int("EXTERNAL_BILLING_DEFAULT_TIMEOUT", 600)

	// ExternalBillingMaxTimeoutSec caps user-supplied external billing hold durations
	// to prevent indefinite quota locks.
	//
	// Environment variable: EXTERNAL_BILLING_MAX_TIMEOUT
	// Default: 3600 (1 hour)
	// Unit: seconds
	ExternalBillingMaxTimeoutSec = env.Int("EXTERNAL_BILLING_MAX_TIMEOUT", 3600)

	// EnforceIncludeUsage forces upstream adapters to return usage accounting.
	// Requests without usage information are rejected when true.
	//
	// Environment variable: ENFORCE_INCLUDE_USAGE
	// Default: true
	EnforceIncludeUsage = env.Bool("ENFORCE_INCLUDE_USAGE", true)

	// ApproximateTokenEnabled toggles approximate token counting when exact counts
	// are unavailable from upstream providers.
	//
	// Runtime variable (set via admin UI)
	// Default: false
	ApproximateTokenEnabled = false

	// QuotaRemindThreshold determines when low quota notifications are sent to users.
	//
	// Runtime variable (set via admin UI)
	// Default: 1000
	QuotaRemindThreshold int64 = 1000
)

// =============================================================================
// RELAY & TIMEOUT CONFIGURATION
// =============================================================================
// Settings controlling how one-api communicates with upstream AI providers.
// Includes timeout settings and proxy configuration for outbound requests.

var (
	// RelayTimeout bounds upstream HTTP requests before aborting them.
	// Set to 0 for no timeout (not recommended for production).
	//
	// Environment variable: RELAY_TIMEOUT
	// Default: 0 (no timeout)
	// Unit: seconds
	// Recommended: 300 for most use cases
	RelayTimeout = env.Int("RELAY_TIMEOUT", 0)

	// UpstreamDialTimeout bounds the TCP connect phase of outbound relay
	// requests. Without it a blackholed upstream rides the OS SYN-retry budget
	// (~127s on Linux with tcp_syn_retries=6), during which the caller usually
	// gives up first and the failure is misclassified as a caller cancellation
	// rather than a retryable channel failure. A dial timeout surfaces as a
	// net.Error (i/o timeout), which the relay retries and can suspend on.
	//
	// Environment variable: UPSTREAM_DIAL_TIMEOUT
	// Default: 5
	// Unit: seconds
	UpstreamDialTimeout = env.Int("UPSTREAM_DIAL_TIMEOUT", 5)

	// BufferedStreamModels lists client-facing model names (comma-separated,
	// case-insensitive) whose streaming responses are fully buffered before
	// anything is written to the client. A provider that returns HTTP 200 but
	// ends the stream without a terminal finish_reason ("stream cut", common on
	// free/rate-limited tiers and in tool-calling/agent workloads) can then be
	// treated as a retryable failure, so the relay falls back to another channel
	// within the same request instead of surfacing a truncated reply.
	// Trade-off: these models lose incremental streaming (time-to-first-token).
	//
	// Matched against both the requested (origin) model name and the mapped
	// (actual) model name, so aliases like "auto-agent" work.
	//
	// Environment variable: BUFFERED_STREAM_MODELS
	// Default: "" (disabled)
	// Example: "auto-agent,auto-gemini"
	BufferedStreamModels = splitCSVLower(strings.TrimSpace(env.String("BUFFERED_STREAM_MODELS", "")))

	// BufferedStreamMaxBytes caps how much of a buffered streaming response is
	// held in memory. Once exceeded, the relay flushes what it has and streams
	// the remainder directly (degrading to normal behavior; such a response can
	// no longer be retried within the request).
	//
	// Environment variable: BUFFERED_STREAM_MAX_BYTES
	// Default: 8388608 (8 MiB)
	BufferedStreamMaxBytes = env.Int("BUFFERED_STREAM_MAX_BYTES", 8*1024*1024)

	// BufferedStreamMaxWaitSec bounds how long a buffered stream may be held
	// before it is force-flushed to the client. Guards against indefinite
	// buffering of a slow upstream.
	//
	// Environment variable: BUFFERED_STREAM_MAX_WAIT_SEC
	// Default: 600 (10 minutes)
	// Unit: seconds
	BufferedStreamMaxWaitSec = env.Int("BUFFERED_STREAM_MAX_WAIT_SEC", 600)

	// StreamCutDisableThreshold is the number of consecutive upstream stream
	// cuts after which a channel is auto-disabled. The periodic auto-disabled
	// channel health test re-enables it once it passes. Set <= 0 to suspend the
	// ability only (never auto-disable) on stream cuts.
	//
	// Environment variable: STREAM_CUT_DISABLE_THRESHOLD
	// Default: 3
	StreamCutDisableThreshold = env.Int("STREAM_CUT_DISABLE_THRESHOLD", 3)

	// RerankTimeout bounds rerank upstream requests separately from the general
	// RelayTimeout. Reranking many documents on CPU-only devices can take several
	// minutes, so a longer (or disabled) timeout is useful while keeping a tight
	// timeout for chat/embedding traffic.
	//
	// When 0, falls back to RelayTimeout.
	//
	// Environment variable: RERANK_TIMEOUT
	// Default: 600 (10 minutes)
	// Unit: seconds
	RerankTimeout = env.Int("RERANK_TIMEOUT", 600)

	// FanOutMinBatchSize is the minimum number of input items (embedding texts or
	// rerank documents) required before a request is split across multiple
	// channels for parallel processing. Below this threshold the request is sent
	// to a single channel as usual.
	//
	// Environment variable: FAN_OUT_MIN_BATCH_SIZE
	// Default: 10
	FanOutMinBatchSize = env.Int("FAN_OUT_MIN_BATCH_SIZE", 10)

	// FanOutMaxShard caps texts per fan-out sub-request. Weight-proportional
	// splits can still hand a weak CPU node 16-24 texts (e.g. weight 10/17 of
	// a 32-batch), which OOMs/hangs llama.cpp. Larger shards are processed as
	// sequential sub-batches on the same channel, preserving input order.
	//
	// Environment variable: FAN_OUT_MAX_SHARD
	// Default: 8
	FanOutMaxShard = env.Int("FAN_OUT_MAX_SHARD", 8)

	// EmbeddingTimeout bounds embedding upstream requests separately from the
	// general RelayTimeout. CPU-only llama.cpp nodes need tens of seconds per
	// shard; 0 falls back to RelayTimeout.
	//
	// Environment variable: EMBEDDING_TIMEOUT
	// Default: 120
	// Unit: seconds
	EmbeddingTimeout = env.Int("EMBEDDING_TIMEOUT", 120)

	// IdleTimeout controls how long to keep streaming connections alive without
	// traffic before closing them. Prevents connection leaks from stalled streams.
	//
	// Environment variable: IDLE_TIMEOUT
	// Default: 30 seconds
	// Unit: seconds
	IdleTimeout = env.Int("IDLE_TIMEOUT", 30)

	// RequestInterval throttles billing/channel polling loops.
	// Set to 0 to disable throttling.
	//
	// Environment variable: POLLING_INTERVAL
	// Default: 0 (no throttling)
	// Unit: seconds
	RequestInterval = time.Duration(env.Int("POLLING_INTERVAL", 0)) * time.Second

	// RelayProxy provides an HTTP proxy for outbound relay requests to upstream
	// providers. Useful for environments that require proxy for external access.
	//
	// Environment variable: RELAY_PROXY
	// Default: "" (no proxy)
	// Example: "http://proxy.example.com:8080"
	RelayProxy = env.String("RELAY_PROXY", "")

	// MCPMaxToolRounds bounds how many MCP tool execution rounds are allowed per request.
	//
	// Environment variable: MCP_MAX_TOOL_ROUNDS
	// Default: 10
	// Unit: rounds
	MCPMaxToolRounds = env.Int("MCP_MAX_TOOL_ROUNDS", 10)

	// MCPToolCallTimeoutSec limits how long one-api will wait for a single MCP tool call.
	//
	// Environment variable: MCP_TOOL_CALL_TIMEOUT
	// Default: 60 seconds
	// Unit: seconds
	MCPToolCallTimeoutSec = env.Int("MCP_TOOL_CALL_TIMEOUT", 60)

	// UserContentRequestProxy provides an HTTP proxy when fetching user-supplied
	// assets like external images. Separate from relay proxy for security isolation.
	//
	// Environment variable: USER_CONTENT_REQUEST_PROXY
	// Default: "" (no proxy)
	// Example: "http://proxy.example.com:8080"
	UserContentRequestProxy = env.String("USER_CONTENT_REQUEST_PROXY", "")

	// UserContentRequestTimeout limits fetch time for user-supplied assets.
	// Prevents slow external resources from blocking requests.
	//
	// Environment variable: USER_CONTENT_REQUEST_TIMEOUT
	// Default: 30 seconds
	// Unit: seconds
	UserContentRequestTimeout = env.Int("USER_CONTENT_REQUEST_TIMEOUT", 30)

	// MaxInlineImageSizeMB limits the size of images that can be inlined as base64
	// to prevent oversized payloads from overwhelming upstream providers.
	//
	// Environment variable: MAX_INLINE_IMAGE_SIZE_MB
	// Default: 16 MB
	// Unit: megabytes
	MaxInlineImageSizeMB = func() int {
		v := env.Int("MAX_INLINE_IMAGE_SIZE_MB", 16)
		if v < 0 {
			panic("MAX_INLINE_IMAGE_SIZE_MB must not be negative")
		}
		return v
	}()
)

// =============================================================================
// BATCH UPDATE SYSTEM
// =============================================================================
// Settings for the background usage batch updater. When enabled, usage data
// is aggregated and flushed to the database periodically rather than per-request.
// This reduces database load at the cost of potential data loss on crash.

var (
	// BatchUpdateEnabled turns on the background usage batch updater.
	// When true, usage data is aggregated in memory and flushed periodically.
	//
	// Environment variable: BATCH_UPDATE_ENABLED
	// Default: false
	BatchUpdateEnabled = env.Bool("BATCH_UPDATE_ENABLED", false)

	// BatchUpdateInterval sets the flush cadence for the batch updater.
	// Lower values reduce data loss risk; higher values reduce database load.
	//
	// Environment variable: BATCH_UPDATE_INTERVAL
	// Default: 5 seconds
	// Unit: seconds
	BatchUpdateInterval = env.Int("BATCH_UPDATE_INTERVAL", 5)

	// BatchUpdateTimeoutSec is the maximum time allowed for a single batch
	// update cycle. Prevents batch updates from blocking indefinitely.
	//
	// Environment variable: BATCH_UPDATE_TIMEOUT
	// Default: 180 (3 minutes)
	// Unit: seconds
	BatchUpdateTimeoutSec = env.Int("BATCH_UPDATE_TIMEOUT", 180)
)

// =============================================================================
// METRICS & MONITORING
// =============================================================================
// Settings for health monitoring, Prometheus metrics export, and channel
// failure rate tracking. These help operators monitor service health.

var (
	// EnableMetric toggles the failure rate monitor that can automatically
	// disable unstable channels based on success rate.
	//
	// Environment variable: ENABLE_METRIC
	// Default: false
	EnableMetric = env.Bool("ENABLE_METRIC", false)

	// EnablePrometheusMetrics exposes the /metrics endpoint for Prometheus
	// scrapers when true. Provides detailed service metrics.
	//
	// Environment variable: ENABLE_PROMETHEUS_METRICS
	// Default: true
	EnablePrometheusMetrics = env.Bool("ENABLE_PROMETHEUS_METRICS", true)

	// MetricsToken is the Bearer token required to access the /metrics endpoint.
	// When empty (default), the endpoint returns 403 until configured.
	//
	// Environment variable: METRICS_TOKEN
	// Default: "" (metrics endpoint blocked)
	MetricsToken = strings.TrimSpace(env.String("METRICS_TOKEN", ""))

	// MetricQueueSize configures the buffered queue that aggregates success/failure
	// events before processing. Larger queues handle burst traffic better.
	//
	// Environment variable: METRIC_QUEUE_SIZE
	// Default: 10
	MetricQueueSize = env.Int("METRIC_QUEUE_SIZE", 10)

	// MetricSuccessRateThreshold defines the minimum acceptable success ratio
	// before a channel is flagged as unhealthy. Used with EnableMetric.
	//
	// Environment variable: METRIC_SUCCESS_RATE_THRESHOLD
	// Default: 0.8 (80%)
	// Range: 0.0 to 1.0
	MetricSuccessRateThreshold = env.Float64("METRIC_SUCCESS_RATE_THRESHOLD", 0.8)

	// MetricSuccessChanSize sizes the buffered success event channel.
	// Increase for high-throughput deployments.
	//
	// Environment variable: METRIC_SUCCESS_CHAN_SIZE
	// Default: 1024
	MetricSuccessChanSize = env.Int("METRIC_SUCCESS_CHAN_SIZE", 1024)

	// MetricFailChanSize sizes the buffered failure event channel.
	// Usually smaller than success channel as failures should be less common.
	//
	// Environment variable: METRIC_FAIL_CHAN_SIZE
	// Default: 128
	MetricFailChanSize = env.Int("METRIC_FAIL_CHAN_SIZE", 128)
)

// =============================================================================
// OPEN TELEMETRY
// =============================================================================
// Settings for exporting traces and metrics to an OpenTelemetry collector.
// When enabled, the application will create OTLP HTTP exporters for traces
// and metrics and attach middleware/instrumentation across Gin and GORM.

var (
	// OpenTelemetryEnabled toggles OpenTelemetry tracing and metrics export.
	// When true, OTLP exporters are initialized using the settings below.
	//
	// Environment variable: OTEL_ENABLED
	// Default: false
	OpenTelemetryEnabled = env.Bool("OTEL_ENABLED", false)

	// OpenTelemetryEndpoint sets the OTLP collector host:port for both traces
	// and metrics. Accepts host:port without scheme (e.g., "localhost:4318").
	//
	// Environment variable: OTEL_EXPORTER_OTLP_ENDPOINT
	// Default: ""
	// Example: "100.97.108.34:4318"
	OpenTelemetryEndpoint = func() string {
		endpoint := strings.TrimSpace(env.String("OTEL_EXPORTER_OTLP_ENDPOINT", ""))
		endpoint = strings.TrimPrefix(endpoint, "http://")
		endpoint = strings.TrimPrefix(endpoint, "https://")
		return endpoint
	}()

	// OpenTelemetryInsecure determines whether the OTLP exporters should skip
	// TLS. Set to true for plain HTTP collectors (common for internal clusters).
	//
	// Environment variable: OTEL_EXPORTER_OTLP_INSECURE
	// Default: true
	OpenTelemetryInsecure = env.Bool("OTEL_EXPORTER_OTLP_INSECURE", true)

	// OpenTelemetryServiceName labels emitted telemetry with the logical
	// service identifier. This appears in tracing backends and metrics UIs.
	//
	// Environment variable: OTEL_SERVICE_NAME
	// Default: "one-api"
	OpenTelemetryServiceName = strings.TrimSpace(env.String("OTEL_SERVICE_NAME", "one-api"))

	// OpenTelemetryEnvironment labels telemetry with the deployment environment
	// (e.g., production, staging). Useful for filtering dashboards.
	//
	// Environment variable: OTEL_ENVIRONMENT
	// Default: "debug"
	OpenTelemetryEnvironment = strings.TrimSpace(env.String("OTEL_ENVIRONMENT", "debug"))
)

// =============================================================================
// LOGGING CONFIGURATION
// =============================================================================
// Settings for application logging, log rotation, and log retention.
// Also includes settings for external log push integrations.

var (
	// OnlyOneLogFile merges all rotated logs into a single file when true.
	// Simplifies log management but loses rotation benefits.
	//
	// Environment variable: ONLY_ONE_LOG_FILE
	// Default: false
	OnlyOneLogFile = env.Bool("ONLY_ONE_LOG_FILE", false)

	// LogRotationInterval selects how frequently the application rotates log files.
	//
	// Environment variable: LOG_ROTATION_INTERVAL
	// Default: "daily"
	// Allowed values: "hourly", "daily", "weekly"
	LogRotationInterval = strings.TrimSpace(strings.ToLower(env.String("LOG_ROTATION_INTERVAL", "daily")))

	// LogRetentionDays determines how many days logs are kept before the
	// retention worker purges them. Set to 0 to disable cleanup.
	//
	// Environment variable: LOG_RETENTION_DAYS
	// Default: 0 (disabled)
	// Unit: days
	LogRetentionDays = func() int {
		v := env.Int("LOG_RETENTION_DAYS", 0)
		if v < 0 {
			return 0
		}
		return v
	}()

	// TraceRetentionDays controls how long trace records are kept before the
	// retention worker removes them. Set to 0 to disable cleanup.
	//
	// Environment variable: TRACE_RETENTION_DAYS
	// Default: 30 days
	// Unit: days
	TraceRetentionDays = func() int {
		v := env.Int("TRACE_RETENTION_DAYS", 30)
		if v < 0 {
			return 0
		}
		return v
	}()

	// AsyncTaskRetentionDays controls how long asynchronous task bindings
	// (e.g., video generation jobs) are retained before cleanup.
	// Set to 0 to disable cleanup.
	//
	// Environment variable: ASYNC_TASK_RETENTION_DAYS
	// Default: 7 days
	// Unit: days
	AsyncTaskRetentionDays = func() int {
		v := env.Int("ASYNC_TASK_RETENTION_DAYS", 7)
		if v < 0 {
			return 0
		}
		return v
	}()

	// LogPushAPI defines the webhook endpoint for escalated log alerts.
	// Leave empty to disable log push.
	//
	// Environment variable: LOG_PUSH_API
	// Default: "" (disabled)
	// Example: "https://gq.laisky.com/query/"
	//
	// More info: https://github.com/Laisky/laisky-blog-graphql/blob/master/internal/web/telegram/README.md
	LogPushAPI = env.String("LOG_PUSH_API", "")

	// LogPushType labels outbound log alerts so downstream processors can route them.
	//
	// Environment variable: LOG_PUSH_TYPE
	// Default: ""
	// Example: "one-api-alerts"
	LogPushType = env.String("LOG_PUSH_TYPE", "")

	// LogPushToken authenticates outbound log alert requests.
	//
	// Environment variable: LOG_PUSH_TOKEN
	// Default: "" (no authentication)
	LogPushToken = env.String("LOG_PUSH_TOKEN", "")
)

// =============================================================================
// PROVIDER-SPECIFIC SETTINGS
// =============================================================================
// Configuration specific to individual AI providers. These settings customize
// behavior when communicating with specific upstream services.

var (
	// GeminiSafetySetting defines the default Gemini safety preset applied to
	// requests without explicit overrides. Controls content filtering level.
	//
	// Environment variable: GEMINI_SAFETY_SETTING
	// Default: "BLOCK_NONE"
	// Allowed values: "BLOCK_NONE", "BLOCK_LOW_AND_ABOVE", "BLOCK_MEDIUM_AND_ABOVE", "BLOCK_ONLY_HIGH"
	GeminiSafetySetting = env.String("GEMINI_SAFETY_SETTING", "BLOCK_NONE")

	// GeminiVersion selects the default Gemini API version when callers omit it.
	//
	// Environment variable: GEMINI_VERSION
	// Default: "v1"
	// Allowed values: "v1", "v1beta"
	GeminiVersion = env.String("GEMINI_VERSION", "v1")

	// OpenrouterProviderSort selects the ordering strategy when listing
	// OpenRouter providers. Affects model selection priority.
	//
	// Environment variable: OPENROUTER_PROVIDER_SORT
	// Default: "" (use OpenRouter default)
	// Example: "price", "quality"
	OpenrouterProviderSort = env.String("OPENROUTER_PROVIDER_SORT", "")

	// DefaultMaxToken enforces a global max token value when model-specific
	// limits are unknown. Acts as a fallback limit.
	//
	// Environment variable: DEFAULT_MAX_TOKEN
	// Default: 2048
	DefaultMaxToken = env.Int("DEFAULT_MAX_TOKEN", 2048)

	// DefaultUseMinMaxTokensModel controls whether new channels use the
	// min/max token scheme by default for output token control.
	//
	// Environment variable: DEFAULT_USE_MIN_MAX_TOKENS_MODEL
	// Default: false
	DefaultUseMinMaxTokensModel = env.Bool("DEFAULT_USE_MIN_MAX_TOKENS_MODEL", false)
)

// =============================================================================
// UI & THEME CONFIGURATION
// =============================================================================
// Settings controlling the frontend appearance and user interface.

var (
	// Theme chooses which bundled frontend theme to render.
	//
	// Environment variable: THEME
	// Default: "modern"
	// Allowed values: "berry", "air", "modern"
	// Note: "default" is no longer supported and will be automatically
	// redirected to "modern" for backward compatibility.
	Theme = env.String("THEME", "modern")

	// ValidThemes enumerates the built-in frontend themes.
	// Used for validation when changing themes.
	ValidThemes = map[string]bool{
		"berry":  true,
		"air":    true,
		"modern": true,
	}
)

// =============================================================================
// TOKEN & API KEY CONFIGURATION
// =============================================================================
// Settings related to API token generation and management.

var (
	// TokenKeyPrefix configures the prefix returned when new API tokens are created.
	// Helps users identify one-api tokens.
	//
	// Environment variable: TOKEN_KEY_PREFIX
	// Default: "sk-"
	// Example: "oneapi-", "myservice-"
	TokenKeyPrefix = env.String("TOKEN_KEY_PREFIX", "sk-")

	// InitialRootToken seeds an initial personal token for the root user on first boot.
	// Useful for automated deployments that need immediate API access.
	//
	// Environment variable: INITIAL_ROOT_TOKEN
	// Default: "" (no initial token)
	// Example: "sk-root-token-12345"
	InitialRootToken = env.String("INITIAL_ROOT_TOKEN", "")

	// InitialRootAccessToken seeds an initial access token for the root user on first boot.
	// Access tokens have different permissions than personal tokens.
	//
	// Environment variable: INITIAL_ROOT_ACCESS_TOKEN
	// Default: "" (no initial access token)
	InitialRootAccessToken = env.String("INITIAL_ROOT_ACCESS_TOKEN", "")
)

// =============================================================================
// CHANNEL TESTING CONFIGURATION
// =============================================================================
// Settings for automated channel health testing and diagnostics.

var (
	// TestPrompt holds the default test prompt used in automated channel diagnostics.
	// Should be a simple prompt that all models can answer.
	//
	// Environment variable: TEST_PROMPT
	// Default: "2 + 2 = ?"
	TestPrompt = env.String("TEST_PROMPT", "2 + 2 = ?")

	// TestMaxTokens caps the tokens requested by the diagnostic test prompt.
	// Keep low to minimize test costs while ensuring response is complete.
	//
	// Environment variable: TEST_MAX_TOKENS
	// Default: 1024
	TestMaxTokens = env.Int("TEST_MAX_TOKENS", 1024)
)

// =============================================================================
// SMOKE TEST CONFIGURATION
// =============================================================================
// Settings used by the cmd/test smoke tester for end-to-end testing.
// These are typically only set in testing/CI environments.

var (
	// APIBase configures the base URL used by the cmd/test smoke tester.
	//
	// Environment variable: API_BASE
	// Default: "" (disabled)
	// Example: "http://localhost:3000"
	APIBase = strings.TrimSpace(env.String("API_BASE", ""))

	// APIToken configures the API token consumed by the cmd/test smoke tester.
	//
	// Environment variable: API_TOKEN
	// Default: "" (disabled)
	APIToken = strings.TrimSpace(env.String("API_TOKEN", ""))

	// OneAPITestModels lists comma-separated models exercised by the smoke tester.
	//
	// Environment variable: ONEAPI_TEST_MODELS
	// Default: "" (test all available models)
	// Example: "gpt-4,gpt-3.5-turbo,claude-3-opus"
	OneAPITestModels = strings.TrimSpace(env.String("ONEAPI_TEST_MODELS", ""))

	// OneAPITestVariants limits the smoke tester to specific API formats (variants).
	//
	// Environment variable: ONEAPI_TEST_VARIANTS
	// Default: "" (test all variants)
	// Example: "chat,response,claude"
	OneAPITestVariants = strings.TrimSpace(env.String("ONEAPI_TEST_VARIANTS", ""))
)

// =============================================================================
// RUNTIME VARIABLES (Modified at runtime via admin UI)
// =============================================================================
// These variables are typically modified at runtime through the admin interface.
// They have default values but can be changed without restarting the server.
// Changes are persisted to the database options table.

var (
	// SystemName is displayed in the dashboard header and email templates.
	//
	// Runtime variable (set via admin UI)
	// Default: "One API"
	SystemName = "One API"

	// ServerAddress forms absolute URLs in email templates and redirect flows.
	// Must include protocol (http:// or https://).
	//
	// Runtime variable (set via admin UI)
	// Default: "http://localhost:3000"
	// Example: "https://api.example.com"
	ServerAddress = "http://localhost:3000"

	// Footer supplies custom HTML appended to the dashboard footer.
	// Supports HTML for links and formatting.
	//
	// Runtime variable (set via admin UI)
	// Default: "" (no custom footer)
	Footer = ""

	// Logo provides the dashboard logo URL.
	// Can be an absolute URL or relative path.
	//
	// Runtime variable (set via admin UI)
	// Default: "" (use default logo)
	Logo = ""

	// TopUpLink points users to the recharge page referenced in quota notifications.
	//
	// Runtime variable (set via admin UI)
	// Default: "" (no top-up link)
	// Example: "https://billing.example.com/topup"
	TopUpLink = ""

	// ChatLink links to the default chat UI shown in the dashboard shortcuts.
	//
	// Runtime variable (set via admin UI)
	// Default: "" (no chat link)
	// Example: "https://chat.example.com"
	ChatLink = ""

	// QuotaPerUnit defines the legacy conversion rate between quota and USD for display.
	// Used to show users their balance in a more familiar currency format.
	//
	// Runtime variable (set via admin UI)
	// Default: 500000 (quota units per USD)
	QuotaPerUnit = 500 * 1000.0

	// DisplayInCurrencyEnabled toggles quota display in currency instead of raw tokens.
	// When true, quota is shown as currency using QuotaPerUnit conversion.
	//
	// Runtime variable (set via admin UI)
	// Default: true
	DisplayInCurrencyEnabled = true

	// DisplayTokenStatEnabled toggles the token statistics card on the dashboard.
	// Shows users their token consumption metrics when enabled.
	//
	// Runtime variable (set via admin UI)
	// Default: true
	DisplayTokenStatEnabled = true
)

// =============================================================================
// OPTIONS CACHE
// =============================================================================
// Internal state for caching database options. Used to reduce database queries
// for frequently accessed configuration values.

var (
	// OptionMap caches key/value pairs loaded from the database options table.
	// Updated periodically based on SyncFrequency.
	OptionMap map[string]string

	// OptionMapRWMutex guards concurrent reads/writes to OptionMap.
	OptionMapRWMutex sync.RWMutex
)

// =============================================================================
// PAGINATION DEFAULTS
// =============================================================================
// Default pagination settings for UI tables and API responses.

var (
	// DefaultItemsPerPage controls pagination defaults for tables that do not
	// override the value. Used when page size is not specified.
	//
	// Default: 10
	DefaultItemsPerPage = 10

	// MaxRecentItems limits the number of recent actions retained in memory
	// for widgets like Recent Logs on the dashboard.
	//
	// Default: 100
	MaxRecentItems = 100
)

// =============================================================================
// AUTHENTICATION PROVIDERS
// =============================================================================
// Settings controlling available login methods. These are typically configured
// via environment variables or the admin UI. Multiple providers can be enabled
// simultaneously.

var (
	// PasswordLoginEnabled toggles email/password login support.
	// When false, users must use OAuth or other methods.
	//
	// Runtime variable (set via admin UI)
	// Default: true
	PasswordLoginEnabled = true

	// PasswordRegisterEnabled toggles self-service registration with password.
	// Independent of PasswordLoginEnabled - can disable registration while
	// allowing existing users to log in.
	//
	// Runtime variable (set via admin UI)
	// Default: true
	PasswordRegisterEnabled = true

	// EmailVerificationEnabled forces email verification during registration.
	// New users must verify email before accessing the service.
	//
	// Runtime variable (set via admin UI)
	// Default: false
	EmailVerificationEnabled = false

	// GitHubOAuthEnabled toggles GitHub OAuth login.
	// Requires GitHubClientId and GitHubClientSecret to be set.
	//
	// Runtime variable (set via admin UI)
	// Default: false
	GitHubOAuthEnabled = false

	// OidcEnabled toggles generic OIDC login.
	// Supports any OIDC-compliant identity provider.
	//
	// Runtime variable (set via admin UI)
	// Default: false
	OidcEnabled = false

	// WeChatAuthEnabled toggles WeChat login support.
	// Requires WeChat server configuration.
	//
	// Runtime variable (set via admin UI)
	// Default: false
	WeChatAuthEnabled = false

	// TurnstileCheckEnabled toggles Cloudflare Turnstile verification on the login UI.
	// Helps prevent automated abuse of registration/login.
	//
	// Runtime variable (set via admin UI)
	// Default: false
	TurnstileCheckEnabled = false

	// RegisterEnabled disables all new-user registration when set to false.
	// Master switch that overrides all other registration settings.
	//
	// Runtime variable (set via admin UI)
	// Default: true
	RegisterEnabled = true
)

// =============================================================================
// EMAIL CONFIGURATION
// =============================================================================
// Settings for outbound email functionality including SMTP server configuration
// and email domain restrictions for registration.

var (
	// ForceEmailTLSVerify enforces SMTP TLS certificate validation when sending email.
	// Disable only for testing with self-signed certificates.
	//
	// Environment variable: FORCE_EMAIL_TLS_VERIFY
	// Default: false
	// WARNING: Disabling reduces security
	ForceEmailTLSVerify = env.Bool("FORCE_EMAIL_TLS_VERIFY", false)

	// EmailDomainRestrictionEnabled allows limiting registrations to EmailDomainWhitelist.
	// Useful for enterprise deployments to restrict to company domains.
	//
	// Runtime variable (set via admin UI)
	// Default: false
	EmailDomainRestrictionEnabled = false

	// EmailDomainWhitelist lists domains allowed when EmailDomainRestrictionEnabled is true.
	// Users can only register with email addresses from these domains.
	//
	// Runtime variable (set via admin UI)
	// Default: common email providers
	EmailDomainWhitelist = []string{
		"gmail.com",
		"163.com",
		"126.com",
		"qq.com",
		"outlook.com",
		"hotmail.com",
		"icloud.com",
		"yahoo.com",
		"foxmail.com",
	}
)

// SMTP server settings for outbound email (password reset, verification, alerts).
// All settings are runtime variables configured via admin UI.
var (
	// SMTPServer holds the SMTP hostname for outbound email.
	//
	// Runtime variable (set via admin UI)
	// Default: "" (email disabled)
	// Example: "smtp.gmail.com"
	SMTPServer = ""

	// SMTPPort holds the SMTP port.
	// Common ports: 25 (unencrypted), 465 (SSL), 587 (TLS/STARTTLS)
	//
	// Runtime variable (set via admin UI)
	// Default: 587
	SMTPPort = 587

	// SMTPAccount stores the SMTP username for authentication.
	//
	// Runtime variable (set via admin UI)
	// Default: "" (no authentication)
	SMTPAccount = ""

	// SMTPFrom defines the From address used in outbound email.
	// Should match SMTPAccount in most cases.
	//
	// Runtime variable (set via admin UI)
	// Default: ""
	// Example: "noreply@example.com"
	SMTPFrom = ""

	// SMTPToken stores the SMTP password or app-specific password.
	//
	// Runtime variable (set via admin UI)
	// Default: "" (no authentication)
	SMTPToken = ""
)

// =============================================================================
// GITHUB OAUTH CONFIGURATION
// =============================================================================
// Settings for GitHub OAuth login integration.

var (
	// GitHubClientId stores the OAuth client ID for GitHub login.
	// Obtain from GitHub Developer Settings.
	//
	// Runtime variable (set via admin UI)
	// Default: "" (disabled)
	GitHubClientId = ""

	// GitHubClientSecret stores the OAuth client secret for GitHub login.
	//
	// Runtime variable (set via admin UI)
	// Default: "" (disabled)
	GitHubClientSecret = ""
)

// =============================================================================
// LARK (FEISHU) OAUTH CONFIGURATION
// =============================================================================
// Settings for Lark/Feishu OAuth login integration.

var (
	// LarkClientId stores the OAuth client ID for Lark login.
	// Obtain from Lark Open Platform.
	//
	// Runtime variable (set via admin UI)
	// Default: "" (disabled)
	LarkClientId = ""

	// LarkClientSecret stores the OAuth client secret for Lark login.
	//
	// Runtime variable (set via admin UI)
	// Default: "" (disabled)
	LarkClientSecret = ""
)

// =============================================================================
// OIDC (OPENID CONNECT) CONFIGURATION
// =============================================================================
// Settings for generic OIDC login integration. Supports any OIDC-compliant
// identity provider (Okta, Auth0, Keycloak, Azure AD, etc.).

var (
	// OidcClientId stores the client ID for generic OIDC login.
	//
	// Runtime variable (set via admin UI)
	// Default: "" (disabled)
	OidcClientId = ""

	// OidcClientSecret stores the client secret for generic OIDC login.
	//
	// Runtime variable (set via admin UI)
	// Default: ""
	OidcClientSecret = ""

	// OidcWellKnown caches the OIDC discovery endpoint (.well-known/openid-configuration).
	// When set, endpoints are automatically discovered.
	//
	// Runtime variable (set via admin UI)
	// Default: ""
	// Example: "https://accounts.google.com/.well-known/openid-configuration"
	OidcWellKnown = ""

	// OidcAuthorizationEndpoint overrides the authorization endpoint when
	// discovery is unavailable or needs customization.
	//
	// Runtime variable (set via admin UI)
	// Default: "" (use discovery)
	OidcAuthorizationEndpoint = ""

	// OidcTokenEndpoint overrides the token endpoint when discovery is unavailable.
	//
	// Runtime variable (set via admin UI)
	// Default: "" (use discovery)
	OidcTokenEndpoint = ""

	// OidcUserinfoEndpoint overrides the userinfo endpoint when discovery is unavailable.
	//
	// Runtime variable (set via admin UI)
	// Default: "" (use discovery)
	OidcUserinfoEndpoint = ""
)

// =============================================================================
// WECHAT AUTHENTICATION CONFIGURATION
// =============================================================================
// Settings for WeChat login integration (primarily for China deployments).

var (
	// WeChatServerAddress stores the WeChat auth server URL.
	//
	// Runtime variable (set via admin UI)
	// Default: "" (disabled)
	WeChatServerAddress = ""

	// WeChatServerToken stores the WeChat auth token.
	//
	// Runtime variable (set via admin UI)
	// Default: ""
	WeChatServerToken = ""

	// WeChatAccountQRCodeImageURL points to the QR code image shown during
	// WeChat login onboarding for users to scan.
	//
	// Runtime variable (set via admin UI)
	// Default: ""
	WeChatAccountQRCodeImageURL = ""
)

// =============================================================================
// PUSH NOTIFICATIONS CONFIGURATION
// =============================================================================
// Settings for optional push notification integrations (e.g., message pusher services).

var (
	// MessagePusherAddress is the endpoint for optional push notification integrations.
	// Used for sending alerts and notifications to external services.
	//
	// Runtime variable (set via admin UI)
	// Default: "" (disabled)
	MessagePusherAddress = ""

	// MessagePusherToken authenticates optional push notification requests.
	//
	// Runtime variable (set via admin UI)
	// Default: "" (no authentication)
	MessagePusherToken = ""
)

// =============================================================================
// CLOUDFLARE TURNSTILE CONFIGURATION
// =============================================================================
// Settings for Cloudflare Turnstile bot protection on login/registration pages.

var (
	// TurnstileSiteKey holds the Cloudflare Turnstile site key for frontend validation.
	// Obtain from Cloudflare Dashboard.
	//
	// Runtime variable (set via admin UI)
	// Default: "" (disabled)
	TurnstileSiteKey = ""

	// TurnstileSecretKey holds the Cloudflare Turnstile secret for server-side verification.
	//
	// Runtime variable (set via admin UI)
	// Default: "" (disabled)
	TurnstileSecretKey = ""
)

// =============================================================================
// USER QUOTA & REFERRAL SETTINGS
// =============================================================================
// Settings for initial user quotas and referral program rewards.

var (
	// QuotaForNewUser awards quota when a new user registers.
	// Set to 0 to disable welcome quota.
	//
	// Runtime variable (set via admin UI)
	// Default: 0
	QuotaForNewUser int64 = 0

	// QuotaForInviter awards quota to the inviter when a referral activates.
	// Encourages users to invite others.
	//
	// Runtime variable (set via admin UI)
	// Default: 0
	QuotaForInviter int64 = 0

	// QuotaForInvitee awards quota to the invitee when they register via referral.
	// Provides incentive for new users to use referral links.
	//
	// Runtime variable (set via admin UI)
	// Default: 0
	QuotaForInvitee int64 = 0

	// RetryTimes configures default retry attempts for certain background jobs.
	//
	// Runtime variable (set via admin UI)
	// Default: 0 (no retries)
	RetryTimes = 0
)

// =============================================================================
// ROOT USER CONFIGURATION
// =============================================================================
// Settings for the built-in root/admin account.

var (
	// RootUserEmail records the email for the built-in root account when seeded manually.
	// Used for password reset and notifications.
	//
	// Runtime variable (set via admin UI)
	// Default: "" (no email set)
	RootUserEmail = ""
)

// =============================================================================
// INTERNAL STATE
// =============================================================================
// Internal variables for runtime state management. These are not directly
// configurable but are modified through API calls or admin actions.

var (
	// logConsumeEnabled toggles quota consumption logging and is mutated at
	// runtime via SetLogConsumeEnabled. When disabled, usage is still tracked
	// but not persisted to the log table.
	logConsumeEnabled atomic.Bool
)

// =============================================================================
// INITIALIZATION
// =============================================================================
// Package initialization that sets up session secrets, validates configuration,
// and initializes default states.

func init() {
	// Generate or normalize session secret
	if SessionSecretEnvValue == "" {
		fmt.Println("SESSION_SECRET not set, using random secret")
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			panic(fmt.Sprintf("failed to generate random secret: %v", err))
		}

		SessionSecret = base64.StdEncoding.EncodeToString(key)
	} else if !slices.Contains([]int{16, 24, 32}, len(SessionSecretEnvValue)) {
		// Hash non-standard length secrets to 32 bytes
		hashed := sha256.Sum256([]byte(SessionSecretEnvValue))
		SessionSecret = base64.StdEncoding.EncodeToString(hashed[:32])
	}

	// Enable consumption logging by default
	logConsumeEnabled.Store(true)

	// Validate all environment variables with constraints
	// This will panic if any validation fails, ensuring fast failure on misconfiguration
	MustValidateEnvVars()
}

// =============================================================================
// PUBLIC FUNCTIONS
// =============================================================================
// Functions for runtime configuration access and modification.

// IsLogConsumeEnabled reports whether consumption logging is enabled.
// Used to conditionally skip logging in high-throughput scenarios.
func IsLogConsumeEnabled() bool {
	return logConsumeEnabled.Load()
}

// SetLogConsumeEnabled toggles consumption logging in a concurrency-safe way.
// Can be called at runtime to enable/disable logging without restart.
func SetLogConsumeEnabled(enabled bool) {
	logConsumeEnabled.Store(enabled)
}

// splitCSVLower splits a comma-separated list, trimming surrounding whitespace
// from each entry, lowercasing it, and dropping empties. It is used to parse
// case-insensitive name allowlists from environment variables.
func splitCSVLower(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.ToLower(strings.TrimSpace(part))
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// IsBufferedStreamModel reports whether streaming responses for the given model
// name should be fully buffered before being sent to the client. Matching is
// case-insensitive. It is backed by BUFFERED_STREAM_MODELS.
func IsBufferedStreamModel(model string) bool {
	if len(BufferedStreamModels) == 0 {
		return false
	}
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return false
	}
	for _, candidate := range BufferedStreamModels {
		if candidate == name {
			return true
		}
	}
	return false
}
