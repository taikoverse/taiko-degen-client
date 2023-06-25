package beaconsync

import (
	"context"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/log"
)

var (
	syncProgressCheckInterval = 10 * time.Second
)

// SyncProgressTracker is responsible for tracking the L2 execution engine's sync progress, after
// a beacon sync is triggered in it, and check whether the L2 execution is not able to sync through P2P (due to no
// connected peer or some other reasons).
type SyncProgressTracker struct {
	// RPC client
	client *ethclient.Client

	// Meta data
	triggered                     bool
	lastSyncedVerifiedBlockID     *big.Int
	lastSyncedVerifiedBlockHeight *big.Int
	lastSyncedVerifiedBlockHash   common.Hash

	// Out-of-sync check related
	lastSyncProgress   *ethereum.SyncProgress
	lastProgressedTime time.Time
	timeout            time.Duration
	outOfSync          bool
	ticker             *time.Ticker

	// Read-write mutex
	mutex sync.RWMutex
}

// NewSyncProgressTracker creates a new SyncProgressTracker instance.
func NewSyncProgressTracker(c *ethclient.Client, timeout time.Duration) *SyncProgressTracker {
	return &SyncProgressTracker{client: c, timeout: timeout, ticker: time.NewTicker(syncProgressCheckInterval)}
}

// Track starts the inner event loop, to monitor the sync progress.
func (t *SyncProgressTracker) Track(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.ticker.C:
			t.track(ctx)
		}
	}
}

// track is the internal implementation of MonitorSyncProgress, tries to
// track the L2 execution engine's beacon sync progress.
func (t *SyncProgressTracker) track(ctx context.Context) {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	if !t.triggered {
		log.Debug("Beacon sync not triggered")
		return
	}

	if t.outOfSync {
		return
	}

	progress, err := t.client.SyncProgress(ctx)
	if err != nil {
		log.Error("Get L2 execution engine sync progress error", "error", err)
		return
	}

	log.Info(
		"L2 execution engine sync progress",
		"progress", progress,
		"lastProgressedTime", t.lastProgressedTime,
		"timeout", t.timeout,
	)

	if progress == nil {
		headHeight, err := t.client.BlockNumber(ctx)
		if err != nil {
			log.Error("Get L2 execution engine head height error", "error", err)
			return
		}

		if new(big.Int).SetUint64(headHeight).Cmp(t.lastSyncedVerifiedBlockHeight) >= 0 {
			t.lastProgressedTime = time.Now()
			log.Info("L2 execution engine has finished the P2P sync work, all verified blocks synced, "+
				"will switch to insert pending blocks one by one",
				"lastSyncedVerifiedBlockID", t.lastSyncedVerifiedBlockID,
				"lastSyncedVerifiedBlockHeight", t.lastSyncedVerifiedBlockHeight,
				"lastSyncedVerifiedBlockHash", t.lastSyncedVerifiedBlockHash,
			)
			return
		}

		log.Warn("L2 execution engine has not started P2P syncing yet")
	}

	defer func() { t.lastSyncProgress = progress }()

	// Check whether the L2 execution engine has synced any new block through P2P since last event loop.
	if syncProgressed(t.lastSyncProgress, progress) {
		t.outOfSync = false
		t.lastProgressedTime = time.Now()
		return
	}

	// Has not synced any new block since last loop, check whether reaching the timeout.
	if time.Since(t.lastProgressedTime) > t.timeout {
		// Mark the L2 execution engine out of sync.
		t.outOfSync = true

		log.Warn(
			"L2 execution engine is not able to sync through P2P",
			"lastProgressedTime", t.lastProgressedTime,
			"timeout", t.timeout,
		)
	}
}

// UpdateMeta updates the inner beacon sync meta data.
func (t *SyncProgressTracker) UpdateMeta(id, height *big.Int, blockHash common.Hash) {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	log.Debug("Update sync progress tracker meta", "id", id, "height", height, "hash", blockHash)

	if !t.triggered {
		t.lastProgressedTime = time.Now()
	}

	t.triggered = true
	t.lastSyncedVerifiedBlockID = id
	t.lastSyncedVerifiedBlockHeight = height
	t.lastSyncedVerifiedBlockHash = blockHash
}

// ClearMeta cleans the inner beacon sync meta data.
func (t *SyncProgressTracker) ClearMeta() {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	log.Debug("Clear sync progress tracker meta")

	t.triggered = false
	t.lastSyncedVerifiedBlockID = nil
	t.lastSyncedVerifiedBlockHash = common.Hash{}
	t.outOfSync = false
}

// HeadChanged checks if a new beacon sync request will be needed.
func (t *SyncProgressTracker) HeadChanged(newID *big.Int) bool {
	t.mutex.RLock()
	defer t.mutex.RUnlock()

	if !t.triggered {
		return true
	}

	return t.lastSyncedVerifiedBlockID != nil && t.lastSyncedVerifiedBlockID != newID
}

// OutOfSync tells whether the L2 execution engine is marked as out of sync.
func (t *SyncProgressTracker) OutOfSync() bool {
	t.mutex.RLock()
	defer t.mutex.RUnlock()

	return t.outOfSync
}

// Triggered returns tracker.triggered.
func (t *SyncProgressTracker) Triggered() bool {
	t.mutex.RLock()
	defer t.mutex.RUnlock()

	return t.triggered
}

// LastSyncedVerifiedBlockID returns tracker.lastSyncedVerifiedBlockID.
func (t *SyncProgressTracker) LastSyncedVerifiedBlockID() *big.Int {
	t.mutex.RLock()
	defer t.mutex.RUnlock()

	if t.lastSyncedVerifiedBlockID == nil {
		return nil
	}

	return new(big.Int).Set(t.lastSyncedVerifiedBlockID)
}

// LastSyncedVerifiedBlockHeight returns tracker.lastSyncedVerifiedBlockHeight.
func (t *SyncProgressTracker) LastSyncedVerifiedBlockHeight() *big.Int {
	t.mutex.RLock()
	defer t.mutex.RUnlock()

	if t.lastSyncedVerifiedBlockHeight == nil {
		return nil
	}

	return new(big.Int).Set(t.lastSyncedVerifiedBlockHeight)
}

// LastSyncedVerifiedBlockHash returns tracker.lastSyncedVerifiedBlockHash.
func (t *SyncProgressTracker) LastSyncedVerifiedBlockHash() common.Hash {
	t.mutex.RLock()
	defer t.mutex.RUnlock()

	return t.lastSyncedVerifiedBlockHash
}

// syncProgressed checks whether there is any new progress since last sync progress check.
func syncProgressed(last *ethereum.SyncProgress, new *ethereum.SyncProgress) bool {
	if last == nil {
		return false
	}

	if new == nil {
		return true
	}

	// Block
	if new.CurrentBlock > last.CurrentBlock {
		return true
	}

	// Fast sync fields
	if new.PulledStates > last.PulledStates {
		return true
	}

	// Snap sync fields
	if new.SyncedAccounts > last.SyncedAccounts ||
		new.SyncedAccountBytes > last.SyncedAccountBytes ||
		new.SyncedBytecodes > last.SyncedBytecodes ||
		new.SyncedBytecodeBytes > last.SyncedBytecodeBytes ||
		new.SyncedStorage > last.SyncedStorage ||
		new.SyncedStorageBytes > last.SyncedStorageBytes ||
		new.HealedTrienodes > last.HealedTrienodes ||
		new.HealedTrienodeBytes > last.HealedTrienodeBytes ||
		new.HealedBytecodes > last.HealedBytecodes ||
		new.HealedBytecodeBytes > last.HealedBytecodeBytes ||
		new.HealingTrienodes > last.HealingTrienodes ||
		new.HealingBytecode > last.HealingBytecode {
		return true
	}

	return false
}
