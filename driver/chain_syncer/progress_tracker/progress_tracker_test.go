package progressTracker

import (
	"context"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/suite"
	"github.com/taikoxyz/taiko-client/testutils"
)

type BeaconSyncProgressTrackerTestSuite struct {
	testutils.ClientTestSuite
	t *BeaconSyncProgressTracker
}

func (s *BeaconSyncProgressTrackerTestSuite) SetupTest() {
	s.ClientTestSuite.SetupTest()

	s.t = New(s.RpcClient.L2, 30*time.Second)
}

func (s *BeaconSyncProgressTrackerTestSuite) TestSyncProgressed() {
	s.False(syncProgressed(nil, &ethereum.SyncProgress{}), nil)
	s.False(syncProgressed(&ethereum.SyncProgress{}, &ethereum.SyncProgress{}))

	// Block
	s.True(syncProgressed(&ethereum.SyncProgress{CurrentBlock: 0}, &ethereum.SyncProgress{CurrentBlock: 1}))
	s.False(syncProgressed(&ethereum.SyncProgress{CurrentBlock: 0}, &ethereum.SyncProgress{CurrentBlock: 0}))
	s.False(syncProgressed(&ethereum.SyncProgress{CurrentBlock: 1}, &ethereum.SyncProgress{CurrentBlock: 1}))

	// Fast sync fields
	s.True(syncProgressed(&ethereum.SyncProgress{PulledStates: 0}, &ethereum.SyncProgress{PulledStates: 1}))

	// Snap sync fields
	s.True(syncProgressed(&ethereum.SyncProgress{SyncedAccounts: 0}, &ethereum.SyncProgress{SyncedAccounts: 1}))
	s.True(syncProgressed(&ethereum.SyncProgress{SyncedAccountBytes: 0}, &ethereum.SyncProgress{SyncedAccountBytes: 1}))
	s.True(syncProgressed(&ethereum.SyncProgress{SyncedBytecodes: 0}, &ethereum.SyncProgress{SyncedBytecodes: 1}))
	s.True(syncProgressed(&ethereum.SyncProgress{SyncedBytecodeBytes: 0}, &ethereum.SyncProgress{SyncedBytecodeBytes: 1}))
	s.True(syncProgressed(&ethereum.SyncProgress{SyncedStorage: 0}, &ethereum.SyncProgress{SyncedStorage: 1}))
	s.True(syncProgressed(&ethereum.SyncProgress{SyncedStorageBytes: 0}, &ethereum.SyncProgress{SyncedStorageBytes: 1}))
	s.True(syncProgressed(&ethereum.SyncProgress{HealedTrienodes: 0}, &ethereum.SyncProgress{HealedTrienodes: 1}))
	s.True(syncProgressed(&ethereum.SyncProgress{HealedTrienodeBytes: 0}, &ethereum.SyncProgress{HealedTrienodeBytes: 1}))
	s.True(syncProgressed(&ethereum.SyncProgress{HealedBytecodes: 0}, &ethereum.SyncProgress{HealedBytecodes: 1}))
	s.True(syncProgressed(&ethereum.SyncProgress{HealedBytecodeBytes: 0}, &ethereum.SyncProgress{HealedBytecodeBytes: 1}))
	s.True(syncProgressed(&ethereum.SyncProgress{HealingTrienodes: 0}, &ethereum.SyncProgress{HealingTrienodes: 1}))
	s.True(syncProgressed(&ethereum.SyncProgress{HealingBytecode: 0}, &ethereum.SyncProgress{HealingBytecode: 1}))
}

func (s *BeaconSyncProgressTrackerTestSuite) TestTrack() {
	// Not triggered
	ctx, cancel := context.WithCancel(context.Background())
	go s.t.Track(ctx)
	time.Sleep(syncProgressCheckInterval + 5*time.Second)
	cancel()

	// Triggered
	ctx, cancel = context.WithCancel(context.Background())
	s.t.UpdateMeta(common.Big256, common.Big256, testutils.RandomHash())
	go s.t.Track(ctx)
	time.Sleep(syncProgressCheckInterval + 5*time.Second)
	cancel()
}

func (s *BeaconSyncProgressTrackerTestSuite) TestClearMeta() {
	s.t.triggered = true
	s.t.ClearMeta()
	s.False(s.t.triggered)
}

func (s *BeaconSyncProgressTrackerTestSuite) TestHeadChanged() {
	s.True(s.t.HeadChanged(common.Big256))
	s.t.triggered = true
	s.False(s.t.HeadChanged(common.Big256))
}

func (s *BeaconSyncProgressTrackerTestSuite) TestOutOfSync() {
	s.False(s.t.OutOfSync())
}

func (s *BeaconSyncProgressTrackerTestSuite) TestTriggered() {
	s.False(s.t.Triggered())
}

func (s *BeaconSyncProgressTrackerTestSuite) TestLastSyncedVerifiedBlockID() {
	s.Nil(s.t.LastSyncedVerifiedBlockID())
	s.t.lastSyncedVerifiedBlockID = common.Big1
	s.Equal(common.Big1.Uint64(), s.t.LastSyncedVerifiedBlockID().Uint64())
}

func (s *BeaconSyncProgressTrackerTestSuite) TestLastSyncedVerifiedBlockHeight() {
	s.Nil(s.t.LastSyncedVerifiedBlockHeight())
	s.t.lastSyncedVerifiedBlockHeight = common.Big1
	s.Equal(common.Big1.Uint64(), s.t.LastSyncedVerifiedBlockHeight().Uint64())
}

func (s *BeaconSyncProgressTrackerTestSuite) TestLastSyncedVerifiedBlockHash() {
	s.Equal(
		common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000000"),
		s.t.LastSyncedVerifiedBlockHash(),
	)
	randomHash := testutils.RandomHash()
	s.t.lastSyncedVerifiedBlockHash = randomHash
	s.Equal(randomHash, s.t.LastSyncedVerifiedBlockHash())
}

func TestBeaconSyncProgressTrackerTestSuite(t *testing.T) {
	suite.Run(t, new(BeaconSyncProgressTrackerTestSuite))
}
