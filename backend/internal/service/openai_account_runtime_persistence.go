package service

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// The runtime statistics path is deliberately decoupled from request
// handling. A slow or unavailable Redis instance must never add latency to an
// upstream attempt or to failover selection.
const (
	openAIAccountRuntimePersistenceWriteQueueSize = 4096
	openAIAccountRuntimePersistenceLoadQueueSize  = 1024
	openAIAccountRuntimePersistenceReportTTL      = 2 * time.Hour
	openAIAccountRuntimePersistenceRefreshEvery   = 30 * time.Second
	openAIAccountRuntimePersistenceRedisTimeout   = 500 * time.Millisecond
	openAIAccountRuntimePersistenceErrorLogEvery  = time.Minute
	openAIAccountRuntimePersistenceMaxErrorKeys   = 256
)

type openAIAccountRuntimePersistenceOptions struct {
	writeQueueSize  int
	loadQueueSize   int
	reportTTL       time.Duration
	refreshInterval time.Duration
	redisTimeout    time.Duration
	errorLogEvery   time.Duration
	maxErrorKeys    int
}

func defaultOpenAIAccountRuntimePersistenceOptions() openAIAccountRuntimePersistenceOptions {
	return openAIAccountRuntimePersistenceOptions{
		writeQueueSize:  openAIAccountRuntimePersistenceWriteQueueSize,
		loadQueueSize:   openAIAccountRuntimePersistenceLoadQueueSize,
		reportTTL:       openAIAccountRuntimePersistenceReportTTL,
		refreshInterval: openAIAccountRuntimePersistenceRefreshEvery,
		redisTimeout:    openAIAccountRuntimePersistenceRedisTimeout,
		errorLogEvery:   openAIAccountRuntimePersistenceErrorLogEvery,
		maxErrorKeys:    openAIAccountRuntimePersistenceMaxErrorKeys,
	}
}

type openAIAccountRuntimePersistenceReport struct {
	accountID    int64
	model        string
	success      bool
	firstTokenMs *int
	observedAt   time.Time
}

type openAIAccountRuntimePersistenceLoad struct {
	accountID int64
	model     string
	apply     openAIAccountRuntimePersistenceLoadCallback
}

// The callback runs on the load worker. It must only perform a short, local
// merge; it must not wait on Redis or perform network I/O.
type openAIAccountRuntimePersistenceLoadCallback func(
	accountID int64,
	model string,
	accountSnapshot OpenAIAccountRuntimeStatSnapshot,
	modelSnapshot OpenAIAccountRuntimeStatSnapshot,
	err error,
)

type openAIAccountRuntimePersistenceKey struct {
	accountID int64
	model     string
}

type openAIAccountRuntimePersistenceLoadState struct {
	inFlight    bool
	initialized bool
	lastQueued  time.Time
}

type openAIAccountRuntimePersistence struct {
	store  OpenAIAccountRuntimeStatsStore
	opts   openAIAccountRuntimePersistenceOptions
	ctx    context.Context
	cancel context.CancelFunc

	writeQueue chan openAIAccountRuntimePersistenceReport
	loadQueue  chan openAIAccountRuntimePersistenceLoad
	writeDone  chan struct{}
	loadDone   chan struct{}

	// enqueueMu closes channels only after all concurrent senders have left the
	// read section. This avoids a send-on-closed-channel race during shutdown.
	enqueueMu sync.RWMutex
	stopOnce  sync.Once
	stopped   atomic.Bool

	loadMu     sync.Mutex
	loadStates map[openAIAccountRuntimePersistenceKey]openAIAccountRuntimePersistenceLoadState

	errorMu       sync.Mutex
	errorLast     map[string]time.Time
	droppedWrites atomic.Uint64
	droppedLoads  atomic.Uint64
}

func newOpenAIAccountRuntimePersistence(store OpenAIAccountRuntimeStatsStore) *openAIAccountRuntimePersistence {
	if store == nil {
		return nil
	}
	return newOpenAIAccountRuntimePersistenceWithOptions(store, defaultOpenAIAccountRuntimePersistenceOptions())
}

func newOpenAIAccountRuntimePersistenceWithOptions(
	store OpenAIAccountRuntimeStatsStore,
	opts openAIAccountRuntimePersistenceOptions,
) *openAIAccountRuntimePersistence {
	if store == nil {
		return nil
	}
	defaults := defaultOpenAIAccountRuntimePersistenceOptions()
	if opts.writeQueueSize <= 0 {
		opts.writeQueueSize = defaults.writeQueueSize
	}
	if opts.loadQueueSize <= 0 {
		opts.loadQueueSize = defaults.loadQueueSize
	}
	if opts.reportTTL <= 0 {
		opts.reportTTL = defaults.reportTTL
	}
	if opts.refreshInterval <= 0 {
		opts.refreshInterval = defaults.refreshInterval
	}
	if opts.redisTimeout <= 0 {
		opts.redisTimeout = defaults.redisTimeout
	}
	if opts.errorLogEvery <= 0 {
		opts.errorLogEvery = defaults.errorLogEvery
	}
	if opts.maxErrorKeys <= 0 {
		opts.maxErrorKeys = defaults.maxErrorKeys
	}

	workerCtx, workerCancel := context.WithCancel(context.Background())
	p := &openAIAccountRuntimePersistence{
		store:      store,
		opts:       opts,
		ctx:        workerCtx,
		cancel:     workerCancel,
		writeQueue: make(chan openAIAccountRuntimePersistenceReport, opts.writeQueueSize),
		loadQueue:  make(chan openAIAccountRuntimePersistenceLoad, opts.loadQueueSize),
		writeDone:  make(chan struct{}),
		loadDone:   make(chan struct{}),
		loadStates: make(map[openAIAccountRuntimePersistenceKey]openAIAccountRuntimePersistenceLoadState),
		errorLast:  make(map[string]time.Time),
	}
	go p.writeLoop()
	go p.loadLoop()
	return p
}

// Report queues one immutable copy of a request outcome. It returns false
// when the queue is full or the coordinator is stopping; callers should still
// treat the in-memory report as authoritative.
func (p *openAIAccountRuntimePersistence) Report(
	accountID int64,
	model string,
	success bool,
	firstTokenMs *int,
	observedAt time.Time,
) bool {
	if p == nil || accountID <= 0 {
		return false
	}
	model = NormalizeOpenAIAccountRuntimeModel(model)
	if model == "" {
		return false
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	// Never hand an asynchronous worker a pointer owned by the request path.
	var copiedTTFT *int
	if firstTokenMs != nil && *firstTokenMs > 0 {
		value := *firstTokenMs
		copiedTTFT = &value
	}
	report := openAIAccountRuntimePersistenceReport{
		accountID:    accountID,
		model:        model,
		success:      success,
		firstTokenMs: copiedTTFT,
		observedAt:   observedAt,
	}

	p.enqueueMu.RLock()
	defer p.enqueueMu.RUnlock()
	if p.stopped.Load() {
		return false
	}
	select {
	case p.writeQueue <- report:
		return true
	default:
		p.droppedWrites.Add(1)
		return false
	}
}

// EnsureLoaded queues the first load for account+model. Completion (including
// a Redis error) marks the key initialized, so a broken Redis does not turn
// every request into a synchronous retry storm.
func (p *openAIAccountRuntimePersistence) EnsureLoaded(
	accountID int64,
	model string,
	apply openAIAccountRuntimePersistenceLoadCallback,
) bool {
	return p.queueLoad(accountID, model, apply, false)
}

// Refresh queues an initial load when needed, or a throttled refresh after the
// configured interval. Only one load for a key can be in flight at a time.
func (p *openAIAccountRuntimePersistence) Refresh(
	accountID int64,
	model string,
	apply openAIAccountRuntimePersistenceLoadCallback,
) bool {
	return p.queueLoad(accountID, model, apply, true)
}

func (p *openAIAccountRuntimePersistence) queueLoad(
	accountID int64,
	model string,
	apply openAIAccountRuntimePersistenceLoadCallback,
	allowRefresh bool,
) bool {
	if p == nil || accountID <= 0 {
		return false
	}
	model = NormalizeOpenAIAccountRuntimeModel(model)
	if model == "" {
		return false
	}
	key := openAIAccountRuntimePersistenceKey{accountID: accountID, model: model}
	now := time.Now()
	p.loadMu.Lock()
	state := p.loadStates[key]
	if state.inFlight || (state.initialized && (!allowRefresh || now.Sub(state.lastQueued) < p.opts.refreshInterval)) {
		p.loadMu.Unlock()
		return false
	}
	state.inFlight = true
	state.lastQueued = now
	p.loadStates[key] = state
	p.loadMu.Unlock()

	load := openAIAccountRuntimePersistenceLoad{accountID: accountID, model: model, apply: apply}
	p.enqueueMu.RLock()
	if p.stopped.Load() {
		p.enqueueMu.RUnlock()
		p.markLoadDropped(key)
		return false
	}
	select {
	case p.loadQueue <- load:
		p.enqueueMu.RUnlock()
		return true
	default:
		p.enqueueMu.RUnlock()
		p.droppedLoads.Add(1)
		p.markLoadDropped(key)
		return false
	}
}

func (p *openAIAccountRuntimePersistence) markLoadDropped(key openAIAccountRuntimePersistenceKey) {
	p.loadMu.Lock()
	state, ok := p.loadStates[key]
	if ok {
		state.inFlight = false
		p.loadStates[key] = state
	}
	p.loadMu.Unlock()
}

func (p *openAIAccountRuntimePersistence) writeLoop() {
	defer close(p.writeDone)
	for {
		if p.ctx.Err() != nil {
			return
		}
		var report openAIAccountRuntimePersistenceReport
		select {
		case <-p.ctx.Done():
			return
		case report = <-p.writeQueue:
		}
		ctx, cancel := context.WithTimeout(p.ctx, p.opts.redisTimeout)
		err := p.store.Report(ctx, report.accountID, report.model, report.success, report.firstTokenMs, report.observedAt, p.opts.reportTTL)
		cancel()
		if err != nil {
			p.logStoreError("report", err)
		}
	}
}

func (p *openAIAccountRuntimePersistence) loadLoop() {
	defer close(p.loadDone)
	for {
		if p.ctx.Err() != nil {
			return
		}
		var load openAIAccountRuntimePersistenceLoad
		select {
		case <-p.ctx.Done():
			return
		case load = <-p.loadQueue:
		}
		ctx, cancel := context.WithTimeout(p.ctx, p.opts.redisTimeout)
		accountSnapshot, modelSnapshot, err := p.store.Load(ctx, load.accountID, load.model)
		cancel()
		p.completeLoad(load.accountID, load.model)
		if err != nil {
			p.logStoreError("load", err)
		}
		if load.apply != nil {
			p.invokeLoadCallback(load.apply, load.accountID, load.model, accountSnapshot, modelSnapshot, err)
		}
	}
}

func (p *openAIAccountRuntimePersistence) completeLoad(accountID int64, model string) {
	key := openAIAccountRuntimePersistenceKey{accountID: accountID, model: model}
	p.loadMu.Lock()
	state, ok := p.loadStates[key]
	if ok {
		state.inFlight = false
		state.initialized = true
		p.loadStates[key] = state
	}
	p.loadMu.Unlock()
}

func (p *openAIAccountRuntimePersistence) invokeLoadCallback(
	apply openAIAccountRuntimePersistenceLoadCallback,
	accountID int64,
	model string,
	accountSnapshot OpenAIAccountRuntimeStatSnapshot,
	modelSnapshot OpenAIAccountRuntimeStatSnapshot,
	err error,
) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("OpenAI runtime stats load callback panicked", "account_id", accountID, "model", model, "panic", recovered)
		}
	}()
	apply(accountID, model, accountSnapshot, modelSnapshot, err)
}

func (p *openAIAccountRuntimePersistence) logStoreError(operation string, err error) {
	if p == nil || err == nil {
		return
	}
	now := time.Now()
	p.errorMu.Lock()
	if previous, ok := p.errorLast[operation]; ok && now.Sub(previous) < p.opts.errorLogEvery {
		p.errorMu.Unlock()
		return
	}
	if len(p.errorLast) >= p.opts.maxErrorKeys {
		// Keep the limiter bounded. Replacing the oldest entry is unnecessary
		// for correctness; clearing the small map is deterministic and cheap.
		p.errorLast = make(map[string]time.Time)
	}
	p.errorLast[operation] = now
	p.errorMu.Unlock()
	slog.Warn("OpenAI runtime stats persistence failed", "operation", operation, "error", err)
}

// Snapshot returns queue-drop counters for diagnostics and tests.
type OpenAIAccountRuntimePersistenceSnapshot struct {
	DroppedWrites uint64
	DroppedLoads  uint64
}

func (p *openAIAccountRuntimePersistence) Snapshot() OpenAIAccountRuntimePersistenceSnapshot {
	if p == nil {
		return OpenAIAccountRuntimePersistenceSnapshot{}
	}
	return OpenAIAccountRuntimePersistenceSnapshot{
		DroppedWrites: p.droppedWrites.Load(),
		DroppedLoads:  p.droppedLoads.Load(),
	}
}

// Stop cancels in-flight Redis work and waits for both workers. Calls after the
// first one are idempotent. Pending best-effort reports may be dropped during
// shutdown; request handling has already updated the process-local stats.
func (p *openAIAccountRuntimePersistence) Stop() {
	if p == nil {
		return
	}
	p.stopOnce.Do(func() {
		p.enqueueMu.Lock()
		p.stopped.Store(true)
		p.cancel()
		p.enqueueMu.Unlock()
		<-p.writeDone
		<-p.loadDone
	})
}

// StopOpenAIAccountRuntimePersistence is the gateway lifecycle hook. It is
// intentionally separate from CloseOpenAIWSPool so callers can order cleanup
// before closing Redis.
func (s *OpenAIGatewayService) StopOpenAIAccountRuntimePersistence() {
	if s != nil && s.openaiRuntimePersistence != nil {
		s.openaiRuntimePersistence.Stop()
	}
}

// mergeOpenAIAccountRuntimeStatSnapshot merges a Redis snapshot into the
// process-local atomics without allowing an older snapshot to overwrite newer
// request observations. It returns true when at least one value advanced.
func mergeOpenAIAccountRuntimeStatSnapshot(
	stat *openAIAccountRuntimeStat,
	snapshot OpenAIAccountRuntimeStatSnapshot,
) bool {
	if stat == nil {
		return false
	}
	stat.mergeMu.Lock()
	defer stat.mergeMu.Unlock()
	remoteLatest := latestOpenAIAccountRuntimeSnapshotTime(snapshot)
	if remoteLatest.IsZero() {
		return false
	}
	localLatest := latestOpenAIAccountRuntimeStatTime(stat)
	advanced := false
	if !remoteLatest.Before(localLatest) {
		if !math.IsNaN(snapshot.ErrorRateEWMA) && snapshot.ErrorRateEWMA >= 0 && snapshot.ErrorRateEWMA <= 1 {
			remoteBits := math.Float64bits(snapshot.ErrorRateEWMA)
			if stat.errorRateEWMABits.Load() != remoteBits {
				stat.errorRateEWMABits.Store(remoteBits)
				advanced = true
			}
		}
		beforeFailure := stat.lastFailureAt.Load()
		beforeSuccess := stat.lastSuccessAt.Load()
		storeAtomicMax(&stat.lastFailureAt, snapshot.LastFailureAt.UnixNano())
		storeAtomicMax(&stat.lastSuccessAt, snapshot.LastSuccessAt.UnixNano())
		advanced = advanced || stat.lastFailureAt.Load() != beforeFailure || stat.lastSuccessAt.Load() != beforeSuccess
	}
	localTTFTUpdatedAt := timeFromUnixNano(stat.ttftUpdatedAt.Load())
	if snapshot.TTFTUpdatedAt.After(localTTFTUpdatedAt) ||
		(snapshot.TTFTUpdatedAt.Equal(localTTFTUpdatedAt) && snapshot.SampleCount >= stat.ttftSampleCount.Load()) {
		if snapshot.TTFTEWMA > 0 && !math.IsNaN(snapshot.TTFTEWMA) && !math.IsInf(snapshot.TTFTEWMA, 0) {
			beforeTTFTBits := stat.ttftEWMABits.Load()
			beforeSamples := stat.ttftSampleCount.Load()
			beforeUpdatedAt := stat.ttftUpdatedAt.Load()
			stat.ttftEWMABits.Store(math.Float64bits(snapshot.TTFTEWMA))
			if snapshot.SampleCount >= 0 {
				stat.ttftSampleCount.Store(snapshot.SampleCount)
			}
			storeAtomicMax(&stat.ttftUpdatedAt, snapshot.TTFTUpdatedAt.UnixNano())
			advanced = advanced || stat.ttftEWMABits.Load() != beforeTTFTBits ||
				stat.ttftSampleCount.Load() != beforeSamples || stat.ttftUpdatedAt.Load() != beforeUpdatedAt
		}
	}
	return advanced
}

func latestOpenAIAccountRuntimeSnapshotTime(snapshot OpenAIAccountRuntimeStatSnapshot) time.Time {
	latest := snapshot.TTFTUpdatedAt
	if snapshot.LastFailureAt.After(latest) {
		latest = snapshot.LastFailureAt
	}
	if snapshot.LastSuccessAt.After(latest) {
		latest = snapshot.LastSuccessAt
	}
	return latest
}

func latestOpenAIAccountRuntimeStatTime(stat *openAIAccountRuntimeStat) time.Time {
	if stat == nil {
		return time.Time{}
	}
	latest := stat.ttftUpdatedAt.Load()
	if failure := stat.lastFailureAt.Load(); failure > latest {
		latest = failure
	}
	if success := stat.lastSuccessAt.Load(); success > latest {
		latest = success
	}
	return timeFromUnixNano(latest)
}

func timeFromUnixNano(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	return time.Unix(0, value)
}

// mergeOpenAIAccountRuntimeSnapshots hydrates both account and model scopes.
func mergeOpenAIAccountRuntimeSnapshots(
	stats *openAIAccountRuntimeStats,
	accountID int64,
	model string,
	accountSnapshot OpenAIAccountRuntimeStatSnapshot,
	modelSnapshot OpenAIAccountRuntimeStatSnapshot,
) bool {
	if stats == nil || accountID <= 0 {
		return false
	}
	merged := mergeOpenAIAccountRuntimeStatSnapshot(stats.loadOrCreate(accountID), accountSnapshot)
	if normalizedModel := NormalizeOpenAIAccountRuntimeModel(model); normalizedModel != "" {
		merged = mergeOpenAIAccountRuntimeStatSnapshot(stats.loadOrCreateModel(accountID, normalizedModel), modelSnapshot) || merged
	}
	return merged
}

// EnsureLoadedAndMerge and RefreshAndMerge are convenience hooks for ranking
// callers. The callback only touches local atomics and is therefore safe to run
// on the coordinator's load worker.
func (p *openAIAccountRuntimePersistence) EnsureLoadedAndMerge(
	stats *openAIAccountRuntimeStats,
	accountID int64,
	model string,
) bool {
	if p == nil {
		return false
	}
	return p.EnsureLoaded(accountID, model, func(
		loadedAccountID int64,
		loadedModel string,
		accountSnapshot OpenAIAccountRuntimeStatSnapshot,
		modelSnapshot OpenAIAccountRuntimeStatSnapshot,
		err error,
	) {
		if err == nil {
			mergeOpenAIAccountRuntimeSnapshots(stats, loadedAccountID, loadedModel, accountSnapshot, modelSnapshot)
		}
	})
}

func (p *openAIAccountRuntimePersistence) RefreshAndMerge(
	stats *openAIAccountRuntimeStats,
	accountID int64,
	model string,
) bool {
	if p == nil {
		return false
	}
	return p.Refresh(accountID, model, func(
		loadedAccountID int64,
		loadedModel string,
		accountSnapshot OpenAIAccountRuntimeStatSnapshot,
		modelSnapshot OpenAIAccountRuntimeStatSnapshot,
		err error,
	) {
		if err == nil {
			mergeOpenAIAccountRuntimeSnapshots(stats, loadedAccountID, loadedModel, accountSnapshot, modelSnapshot)
		}
	})
}

func (s *OpenAIGatewayService) refreshOpenAIAccountRuntimeStats(
	stats *openAIAccountRuntimeStats,
	account *Account,
) {
	s.refreshOpenAIAccountRuntimeStatsForModel(stats, account, "")
}

// refreshOpenAIAccountRuntimeStatsForModel loads the model-scoped runtime
// sample used by the current request. The probe model is only the fallback for
// generic leaderboard views; routing must refresh the mapped request model.
func (s *OpenAIGatewayService) refreshOpenAIAccountRuntimeStatsForModel(
	stats *openAIAccountRuntimeStats,
	account *Account,
	requestedUpstreamModel string,
) {
	if s == nil || s.openaiRuntimePersistence == nil || stats == nil || account == nil {
		return
	}
	snapshot := decodeUpstreamBillingProbeSnapshot(account.Extra)
	model := NormalizeOpenAIAccountRuntimeModel(requestedUpstreamModel)
	if model == "" && snapshot != nil {
		model = NormalizeOpenAIAccountRuntimeModel(snapshot.ModelProbeModel)
	}
	if model == "" {
		return
	}
	s.openaiRuntimePersistence.RefreshAndMerge(stats, account.ID, model)
}
