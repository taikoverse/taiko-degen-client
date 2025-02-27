package proposer

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/taikoxyz/taiko-client/bindings"
	"github.com/taikoxyz/taiko-client/bindings/encoding"
	"github.com/taikoxyz/taiko-client/metrics"
	"github.com/taikoxyz/taiko-client/pkg/rpc"
	"github.com/taikoxyz/taiko-client/proposer/chain_syncer/calldata"
	"github.com/taikoxyz/taiko-client/proposer/chain_syncer/beaconsync"
	
	// "github.com/taikoxyz/taiko-client/proposer/state"
	"github.com/urfave/cli/v2"
	"golang.org/x/sync/errgroup"
)

var (
	errNoNewTxs        = errors.New("no new transactions")
	waitReceiptTimeout = 1 * time.Minute
)

// Proposer keep proposing new transactions from L2 execution engine's tx pool at a fixed interval.
type Proposer struct {
	// RPC clients
	rpc *rpc.Client

	// Private keys and account addresses
	l1ProposerPrivKey       *ecdsa.PrivateKey
	l1ProposerAddress       common.Address
	l2SuggestedFeeRecipient common.Address

	// Proposing configurations
	proposingInterval          *time.Duration
	proposeEmptyBlocksInterval *time.Duration
	proposingTimer             *time.Timer
	commitSlot                 uint64
	locals                     []common.Address
	minBlockGasLimit           *uint64
	maxProposedTxListsPerEpoch uint64
	proposeBlockTxGasLimit     *uint64

	// Protocol configurations
	protocolConfigs *bindings.TaikoDataConfig

	// Only for testing purposes
	CustomProposeOpHook func() error
	AfterCommitHook     func() error

	ctx context.Context
	wg  sync.WaitGroup

	// Syncers
	// state          *state.State
	// l2ChainSyncer  *chainSyncer.L2ChainSyncer
	calldataSyncer *calldata.Syncer
}

// New initializes the given proposer instance based on the command line flags.
func (p *Proposer) InitFromCli(ctx context.Context, c *cli.Context) error {
	cfg, err := NewConfigFromCliContext(c)
	if err != nil {
		return err
	}

	return InitFromConfig(ctx, p, cfg)
}

// InitFromConfig initializes the proposer instance based on the given configurations.
func InitFromConfig(ctx context.Context, p *Proposer, cfg *Config) (err error) {
	p.l1ProposerPrivKey = cfg.L1ProposerPrivKey
	p.l1ProposerAddress = crypto.PubkeyToAddress(cfg.L1ProposerPrivKey.PublicKey)
	p.l2SuggestedFeeRecipient = cfg.L2SuggestedFeeRecipient
	p.proposingInterval = cfg.ProposeInterval
	p.proposeEmptyBlocksInterval = cfg.ProposeEmptyBlocksInterval
	p.proposeBlockTxGasLimit = cfg.ProposeBlockTxGasLimit
	p.wg = sync.WaitGroup{}
	p.locals = cfg.LocalAddresses
	p.commitSlot = cfg.CommitSlot
	p.maxProposedTxListsPerEpoch = cfg.MaxProposedTxListsPerEpoch
	p.ctx = ctx

	// RPC clients
	if p.rpc, err = rpc.NewClient(p.ctx, &rpc.ClientConfig{
		L1Endpoint:       cfg.L1Endpoint,
		L2Endpoint:       cfg.L2Endpoint,
		TaikoL1Address:   cfg.TaikoL1Address,
		TaikoL2Address:   cfg.TaikoL2Address,
		L2EngineEndpoint: cfg.L2EngineEndpoint,
		JwtSecret:        cfg.JwtSecret,
		RetryInterval:    cfg.BackOffRetryInterval,
	}); err != nil {
		return fmt.Errorf("initialize rpc clients error: %w", err)
	}

	// Protocol configs
	protocolConfigs, err := p.rpc.TaikoL1.GetConfig(nil)
	if err != nil {
		return fmt.Errorf("failed to get protocol configs: %w", err)
	}
	p.protocolConfigs = &protocolConfigs

	if cfg.MinBlockGasLimit != 0 {
		if cfg.MinBlockGasLimit > p.protocolConfigs.BlockMaxGasLimit {
			return fmt.Errorf(
				"minimal block gas limit too large, set: %d, limit: %d",
				cfg.MinBlockGasLimit,
				p.protocolConfigs.BlockMaxGasLimit,
			)
		}
		p.minBlockGasLimit = &cfg.MinBlockGasLimit
	}

	// if p.state, err = state.New(p.ctx, p.rpc); err != nil {
	// 	return err
	// }

	signalServiceAddress, err := p.rpc.TaikoL1.Resolve0(nil, rpc.StringToBytes32("signal_service"), false)
	if err != nil {
		return err
	}

	tracker := beaconsync.NewSyncProgressTracker(p.rpc.L2, cfg.P2PSyncTimeout)
	go tracker.Track(ctx)

	p.calldataSyncer, err = calldata.NewSyncer(ctx, p.rpc, tracker, signalServiceAddress)
	if err != nil {
		return err
	}

	log.Info("Protocol configs", "configs", p.protocolConfigs)

	return nil
}

// Start starts the proposer's main loop.
func (p *Proposer) Start() error {
	p.wg.Add(1)
	go p.eventLoop()
	return nil
}

// eventLoop starts the main loop of Taiko proposer.
func (p *Proposer) eventLoop() {
	defer func() {
		p.proposingTimer.Stop()
		p.wg.Done()
	}()

	var lastNonEmptyBlockProposedAt = time.Now()
	for {
		p.updateProposingTicker()

		select {
		case <-p.ctx.Done():
			return
		case <-p.proposingTimer.C:
			metrics.ProposerProposeEpochCounter.Inc(1)

			if err := p.ProposeOp(p.ctx); err != nil {
				if !errors.Is(err, errNoNewTxs) {
					log.Error("Proposing operation error", "error", err)
					continue
				}

				// code unreachable here if proposeEmptyBlocksInterval is not set
				if p.proposeEmptyBlocksInterval != nil {
					if time.Now().Before(lastNonEmptyBlockProposedAt.Add(*p.proposeEmptyBlocksInterval)) {
						continue
					}

					if err := p.ProposeEmptyBlockOp(p.ctx); err != nil {
						log.Error("Proposing an empty block operation error", "error", err)
					}

					lastNonEmptyBlockProposedAt = time.Now()
				}

				continue
			}

			lastNonEmptyBlockProposedAt = time.Now()
		}
	}
}

// Close closes the proposer instance.
func (p *Proposer) Close() {
	p.wg.Wait()
}

// ProposeOp performs a proposing operation, fetching transactions
// from L2 execution engine's tx pool, splitting them by proposing constraints,
// and then proposing them to TaikoL1 contract.
func (p *Proposer) ProposeOp(ctx context.Context) error {
	if p.CustomProposeOpHook != nil {
		return p.CustomProposeOpHook()
	}

	// log.Info("Comparing proposer TKO balance to block fee", "proposer", p.l1ProposerAddress.Hex())

	// if err := p.checkTaikoTokenBalance(); err != nil {
	// 	return fmt.Errorf("failed to check Taiko token balance: %w", err)
	// }

	// // Wait until L2 execution engine is synced at first.
	// if err := p.rpc.WaitTillL2ExecutionEngineSynced(ctx); err != nil {
	// 	return fmt.Errorf("failed to wait until L2 execution engine synced: %w", err)
	// }

	log.Info("Start fetching L2 execution engine's transaction pool content")

	txLists, err := p.rpc.GetPoolContent(
		ctx,
		p.protocolConfigs.MaxTransactionsPerBlock,
		p.protocolConfigs.BlockMaxGasLimit,
		p.protocolConfigs.MaxBytesPerTxList,
		p.locals,
	)
	if err != nil {
		return fmt.Errorf("failed to fetch transaction pool content: %w", err)
	}

	log.Info("Transactions lists count", "count", len(txLists))

	if len(txLists) == 0 {
		return errNoNewTxs
	}

	// head, err := p.rpc.L1.BlockNumber(ctx)
	// if err != nil {
	// 	return err
	// }
	// nonce, err := p.rpc.L1.NonceAt(
	// 	ctx,
	// 	crypto.PubkeyToAddress(p.l1ProposerPrivKey.PublicKey),
	// 	new(big.Int).SetUint64(head),
	// )
	// if err != nil {
	// 	return err
	// }

	// log.Info("Proposer account information", "chainHead", head, "nonce", nonce)

	g := new(errgroup.Group)
	// -----------------
	// block insert code can be put here
	for i, txs := range txLists {
		func(i int, txs types.Transactions) {

			g.Go(func() error {
				if i >= int(p.maxProposedTxListsPerEpoch) {
					return nil
				}

				txListBytes, err := rlp.EncodeToBytes(txs)
				if err != nil {
					return fmt.Errorf("failed to encode transactions: %w", err)
				}

				// txNonce := nonce + uint64(i)
				taikoL1BlockMetadataInput := encoding.TaikoL1BlockMetadataInput{
					Beneficiary:     p.l2SuggestedFeeRecipient,
					GasLimit:        uint32(sumTxsGasLimit(txs)),
					TxListHash:      crypto.Keccak256Hash(txListBytes),
					TxListByteStart: common.Big0,
					TxListByteEnd:   new(big.Int).SetUint64(uint64(len(txListBytes))),
					CacheTxListInfo: 0,
				}

				start1 := time.Now()
				// block insert code can be put here
				currentId, err := p.calldataSyncer.OnBlockProposed(ctx, &taikoL1BlockMetadataInput, txListBytes)
				if err != nil {
					return fmt.Errorf("failed to insert the block: %w", err)
				}
				fmt.Printf("%s took %v\n", "1", time.Since(start1))

				start2 := time.Now()
				if err := p.ProposeTxList(ctx, &taikoL1BlockMetadataInput, txListBytes, uint(txs.Len()), currentId); err != nil {
					return fmt.Errorf("failed to propose transactions: %w", err)
				}
				fmt.Printf("%s took %v\n", "2", time.Since(start2))


				return nil
			})
		}(i, txs)
	}

	if err := g.Wait(); err != nil {
		return fmt.Errorf("failed to propose transactions: %w", err)
	}

	if p.AfterCommitHook != nil {
		if err := p.AfterCommitHook(); err != nil {
			log.Error("Run AfterCommitHook error", "error", err)
		}
	}

	return nil
}

// ProposeEmptyBlockOp performs a proposing one empty block operation.
func (p *Proposer) ProposeEmptyBlockOp(ctx context.Context) error {

	

	txs := types.Transactions{}
	txListBytes := []byte{}
	
	taikoL1BlockMetadataInput := encoding.TaikoL1BlockMetadataInput{
		Beneficiary:     p.l2SuggestedFeeRecipient,
		GasLimit:        uint32(sumTxsGasLimit(txs)),
		TxListHash:      crypto.Keccak256Hash(txListBytes),
		TxListByteStart: common.Big0,
		TxListByteEnd:   common.Big0,
		CacheTxListInfo: 0,
	}

	start1 := time.Now()
	// block insert code can be put here
	currentId, err := p.calldataSyncer.OnBlockProposed(ctx, &taikoL1BlockMetadataInput, txListBytes)
	if err != nil {
		return fmt.Errorf("failed to insert the block: %w", err)
	}
	fmt.Printf("%s took %v\n", "1", time.Since(start1))

	start2 := time.Now()
	if err := p.ProposeTxList(ctx, &taikoL1BlockMetadataInput, txListBytes, uint(txs.Len()), currentId); err != nil {
		return fmt.Errorf("failed to propose transactions: %w", err)
	}
	fmt.Printf("%s took %v\n", "2", time.Since(start2))

	return nil

	// return p.ProposeTxList(ctx, &encoding.TaikoL1BlockMetadataInput{
	// 	TxListHash:      crypto.Keccak256Hash([]byte{}),
	// 	Beneficiary:     p.L2SuggestedFeeRecipient(),
	// 	GasLimit:        21000,
	// 	TxListByteStart: common.Big0,
	// 	TxListByteEnd:   common.Big0,
	// 	CacheTxListInfo: 0,
	// }, []byte{}, 0, nil, 0)
}

// ProposeTxList proposes the given transactions list to TaikoL1 smart contract.
func (p *Proposer) ProposeTxList(
	ctx context.Context,
	meta *encoding.TaikoL1BlockMetadataInput,
	txListBytes []byte,
	txNum uint,
	// nonce *uint64,
	currentId uint64,
) error {
	// propose every 100 blocks
	if currentId % 100 != 0 {
		return nil
	}

	// get chain head and nonce
	head, err := p.rpc.L1.BlockNumber(ctx)
	if err != nil {
		return err
	}

	nonce, err := p.rpc.L1.NonceAt(
		ctx,
		crypto.PubkeyToAddress(p.l1ProposerPrivKey.PublicKey),
		new(big.Int).SetUint64(head),
	)
	if err != nil {
		return err
	}

	log.Info("Proposer account information", "chainHead", head, "nonce", nonce)

	

	if p.minBlockGasLimit != nil && meta.GasLimit < uint32(*p.minBlockGasLimit) {
		meta.GasLimit = uint32(*p.minBlockGasLimit)
	}

	// Propose the transactions list
	inputs, err := encoding.EncodeProposeBlockInput(meta)
	if err != nil {
		return err
	}

	opts, err := getTxOpts(ctx, p.rpc.L1, p.l1ProposerPrivKey, p.rpc.L1ChainID)
	if err != nil {
		return err
	}
	opts.Nonce = new(big.Int).SetUint64(nonce)
	if p.proposeBlockTxGasLimit != nil {
		opts.GasLimit = *p.proposeBlockTxGasLimit
	}

	_, err = p.rpc.TaikoL1.ProposeBlock(opts, inputs, txListBytes)
	if err != nil {
		return encoding.TryParsingCustomError(err)
	}

	// TODO:
	// Commented the following code temporarily.
	// In the near future, submit this tx in another goroutine.
	// If submission failed, just backoff with reorg.
	// Thus high speed is achieved.
	// _, cancel := context.WithTimeout(ctx, waitReceiptTimeout)
	// defer cancel()

	// if _, err := rpc.WaitReceipt(ctxWithTimeout, p.rpc.L1, proposeTx); err != nil {
	// 	return err
	// }

	// proposeTx, err := p.rpc.TaikoL1.ProposeBlock(opts, inputs, txListBytes)
	// if err != nil {
	// 	return encoding.TryParsingCustomError(err)
	// }

	// ctxWithTimeout, cancel := context.WithTimeout(ctx, waitReceiptTimeout)
	// defer cancel()

	// if _, err := rpc.WaitReceipt(ctxWithTimeout, p.rpc.L1, proposeTx); err != nil {
	// 	return err
	// }

	log.Info("📝 Propose transactions succeeded", "txs", txNum)

	metrics.ProposerProposedTxListsCounter.Inc(1)
	metrics.ProposerProposedTxsCounter.Inc(int64(txNum))

	return nil
}


// updateProposingTicker updates the internal proposing timer.
func (p *Proposer) updateProposingTicker() {
	if p.proposingTimer != nil {
		p.proposingTimer.Stop()
	}

	var duration time.Duration
	if p.proposingInterval != nil {
		duration = *p.proposingInterval
	} else {
		// Random number between 120 - 300
		randomSeconds := rand.Intn(300-119) + 120
		duration = time.Duration(randomSeconds) * time.Second
	}

	p.proposingTimer = time.NewTimer(duration)
}

// Name returns the application name.
func (p *Proposer) Name() string {
	return "proposer"
}

// L2SuggestedFeeRecipient returns the L2 suggested fee recipient of the current proposer.
func (p *Proposer) L2SuggestedFeeRecipient() common.Address {
	return p.l2SuggestedFeeRecipient
}

// sumTxsGasLimit calculates the accumulated gas limit of all transactions in the list.
func sumTxsGasLimit(txs []*types.Transaction) uint64 {
	var total uint64
	for i := range txs {
		total += txs[i].Gas()
	}
	return total
}

// getTxOpts creates a bind.TransactOpts instance using the given private key.
func getTxOpts(
	ctx context.Context,
	cli *ethclient.Client,
	privKey *ecdsa.PrivateKey,
	chainID *big.Int,
) (*bind.TransactOpts, error) {
	opts, err := bind.NewKeyedTransactorWithChainID(privKey, chainID)
	if err != nil {
		return nil, fmt.Errorf("failed to generate prepareBlock transaction options: %w", err)
	}

	gasTipCap, err := cli.SuggestGasTipCap(ctx)
	if err != nil {
		if rpc.IsMaxPriorityFeePerGasNotFoundError(err) {
			gasTipCap = rpc.FallbackGasTipCap
		} else {
			return nil, err
		}
	}

	opts.GasTipCap = gasTipCap

	return opts, nil
}

func (p *Proposer) checkTaikoTokenBalance() error {
	fee, err := p.rpc.TaikoL1.GetBlockFee(nil)
	if err != nil {
		return fmt.Errorf("failed to get block fee: %w", err)
	}

	log.Info("GetBlockFee", "fee", fee)

	if fee > math.MaxInt64 {
		metrics.ProposerBlockFeeGauge.Update(math.MaxInt64)
	} else {
		metrics.ProposerBlockFeeGauge.Update(int64(fee))
	}

	balance, err := p.rpc.TaikoL1.GetTaikoTokenBalance(nil, p.l1ProposerAddress)
	if err != nil {
		return fmt.Errorf("failed to get tko balance: %w", err)
	}

	if balance.Cmp(new(big.Int).SetUint64(fee)) == -1 {
		return fmt.Errorf("proposer does not have enough tko balance to propose, balance: %d, fee: %d", balance, fee)
	}

	return nil
}
