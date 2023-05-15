package prover

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/taikoxyz/taiko-client/bindings"
	"github.com/taikoxyz/taiko-client/bindings/encoding"
	"github.com/taikoxyz/taiko-client/metrics"
	eventIterator "github.com/taikoxyz/taiko-client/pkg/chain_iterator/event_iterator"
	"github.com/taikoxyz/taiko-client/pkg/rpc"
	txListValidator "github.com/taikoxyz/taiko-client/pkg/tx_list_validator"
	proofProducer "github.com/taikoxyz/taiko-client/prover/proof_producer"
	proofSubmitter "github.com/taikoxyz/taiko-client/prover/proof_submitter"
	"github.com/urfave/cli/v2"
)

type cancelFunc func()

// Prover keep trying to prove new proposed blocks valid/invalid.
type Prover struct {
	// Configurations
	cfg                 *Config
	proverAddress       common.Address
	oracleProverAddress common.Address
	systemProverAddress common.Address

	// Clients
	rpc *rpc.Client

	// Contract configurations
	txListValidator *txListValidator.TxListValidator
	protocolConfigs *bindings.TaikoDataConfig

	// States
	latestVerifiedL1Height uint64
	lastHandledBlockID     uint64
	l1Current              uint64

	// Proof submitters
	validProofSubmitter proofSubmitter.ProofSubmitter

	// Subscriptions
	blockProposedCh  chan *bindings.TaikoL1ClientBlockProposed
	blockProposedSub event.Subscription
	blockProvenCh    chan *bindings.TaikoL1ClientBlockProven
	blockProvenSub   event.Subscription
	blockVerifiedCh  chan *bindings.TaikoL1ClientBlockVerified
	blockVerifiedSub event.Subscription
	proveNotify      chan struct{}

	// Proof related
	proveValidProofCh   chan *proofProducer.ProofWithHeader
	proveInvalidProofCh chan *proofProducer.ProofWithHeader

	// Concurrency guards
	proposeConcurrencyGuard     chan struct{}
	submitProofConcurrencyGuard chan struct{}
	submitProofTxMutex          *sync.Mutex

	currentBlocksBeingProven      map[uint64]cancelFunc
	currentBlocksBeingProvenMutex *sync.Mutex

	ctx context.Context
	wg  sync.WaitGroup
}

// New initializes the given prover instance based on the command line flags.
func (p *Prover) InitFromCli(ctx context.Context, c *cli.Context) error {
	cfg, err := NewConfigFromCliContext(c)
	if err != nil {
		return err
	}

	return InitFromConfig(ctx, p, cfg)
}

// InitFromConfig initializes the prover instance based on the given configurations.
func InitFromConfig(ctx context.Context, p *Prover, cfg *Config) (err error) {
	p.cfg = cfg
	p.ctx = ctx
	p.currentBlocksBeingProven = make(map[uint64]cancelFunc)
	p.currentBlocksBeingProvenMutex = &sync.Mutex{}

	// Clients
	if p.rpc, err = rpc.NewClient(p.ctx, &rpc.ClientConfig{
		L1Endpoint:     cfg.L1WsEndpoint,
		L2Endpoint:     cfg.L2WsEndpoint,
		TaikoL1Address: cfg.TaikoL1Address,
		TaikoL2Address: cfg.TaikoL2Address,
	}); err != nil {
		return err
	}

	// Configs
	protocolConfigs, err := p.rpc.TaikoL1.GetConfig(nil)
	if err != nil {
		return fmt.Errorf("failed to get protocol configs: %w", err)
	}
	p.protocolConfigs = &protocolConfigs

	log.Info("Protocol configs", "configs", p.protocolConfigs)

	p.submitProofTxMutex = &sync.Mutex{}
	p.txListValidator = txListValidator.NewTxListValidator(
		p.protocolConfigs.BlockMaxGasLimit.Uint64(),
		p.protocolConfigs.MaxTransactionsPerBlock.Uint64(),
		p.protocolConfigs.MaxBytesPerTxList.Uint64(),
		p.protocolConfigs.MinTxGasLimit.Uint64(),
		p.rpc.L2ChainID,
	)
	p.proverAddress = crypto.PubkeyToAddress(p.cfg.L1ProverPrivKey.PublicKey)

	chBufferSize := p.protocolConfigs.MaxNumProposedBlocks.Uint64()
	p.blockProposedCh = make(chan *bindings.TaikoL1ClientBlockProposed, chBufferSize)
	p.blockVerifiedCh = make(chan *bindings.TaikoL1ClientBlockVerified, chBufferSize)
	p.blockProvenCh = make(chan *bindings.TaikoL1ClientBlockProven, chBufferSize)
	p.proveValidProofCh = make(chan *proofProducer.ProofWithHeader, chBufferSize)
	p.proveInvalidProofCh = make(chan *proofProducer.ProofWithHeader, chBufferSize)
	p.proveNotify = make(chan struct{}, 1)
	if err := p.initL1Current(cfg.StartingBlockID); err != nil {
		return fmt.Errorf("initialize L1 current cursor error: %w", err)
	}

	// Concurrency guards
	p.proposeConcurrencyGuard = make(chan struct{}, cfg.MaxConcurrentProvingJobs)
	p.submitProofConcurrencyGuard = make(chan struct{}, cfg.MaxConcurrentProvingJobs)

	oracleProverAddress, err := p.rpc.TaikoL1.Resolve(nil, p.rpc.L1ChainID, rpc.StringToBytes32("oracle_prover"), true)
	if err != nil {
		return err
	}

	p.oracleProverAddress = oracleProverAddress

	systemProverAddress, err := p.rpc.TaikoL1.Resolve(nil, p.rpc.L1ChainID, rpc.StringToBytes32("system_prover"), true)
	if err != nil {
		return err
	}

	p.systemProverAddress = systemProverAddress

	var producer proofProducer.ProofProducer

	isSystemProver := cfg.SystemProver
	isOracleProver := cfg.OracleProver

	if isSystemProver || isOracleProver {
		var specialProverAddress common.Address
		var privateKey *ecdsa.PrivateKey
		if isSystemProver {
			specialProverAddress = systemProverAddress
			privateKey = p.cfg.SystemProverPrivateKey
		} else {
			specialProverAddress = oracleProverAddress
			privateKey = p.cfg.OracleProverPrivateKey
		}

		if producer, err = proofProducer.NewSpecialProofProducer(
			p.rpc,
			privateKey,
			p.cfg.TaikoL2Address,
			time.Duration(p.protocolConfigs.ProofTimeTarget)*time.Second,
			specialProverAddress,
			p.cfg.Graffiti,
			isSystemProver,
		); err != nil {
			return err
		}
	} else if cfg.Dummy {
		producer = &proofProducer.DummyProofProducer{
			RandomDummyProofDelayLowerBound: p.cfg.RandomDummyProofDelayLowerBound,
			RandomDummyProofDelayUpperBound: p.cfg.RandomDummyProofDelayUpperBound,
		}
	} else {
		if producer, err = proofProducer.NewZkevmRpcdProducer(
			cfg.ZKEvmRpcdEndpoint,
			cfg.ZkEvmRpcdParamsPath,
			cfg.L1HttpEndpoint,
			cfg.L2HttpEndpoint,
			true,
		); err != nil {
			return err
		}
	}

	// Proof submitter
	if p.validProofSubmitter, err = proofSubmitter.NewValidProofSubmitter(
		p.rpc,
		producer,
		p.proveValidProofCh,
		p.cfg.TaikoL2Address,
		p.cfg.L1ProverPrivKey,
		p.submitProofTxMutex,
		p.cfg.OracleProver,
		p.cfg.SystemProver,
		p.cfg.Graffiti,
	); err != nil {
		return err
	}

	return nil
}

// Start starts the main loop of the L2 block prover.
func (p *Prover) Start() error {
	p.wg.Add(1)
	p.initSubscription()
	go p.eventLoop()

	return nil
}

// eventLoop starts the main loop of Taiko prover.
func (p *Prover) eventLoop() {
	defer func() {
		p.wg.Done()
	}()

	// reqProving requests performing a proving operation, won't block
	// if we are already proving.
	reqProving := func() {
		select {
		case p.proveNotify <- struct{}{}:
		default:
		}
	}

	// If there is too many (TaikoData.Config.maxNumBlocks) pending blocks in TaikoL1 contract, there will be no new
	// BlockProposed temporarily, so except the BlockProposed subscription, we need another trigger to start
	// fetching the proposed blocks.
	forceProvingTicker := time.NewTicker(15 * time.Second)
	defer forceProvingTicker.Stop()

	// Call reqProving() right away to catch up with the latest state.
	reqProving()

	for {
		select {
		case <-p.ctx.Done():
			return
		case proofWithHeader := <-p.proveValidProofCh:
			p.submitProofOp(p.ctx, proofWithHeader, true)
		case proofWithHeader := <-p.proveInvalidProofCh:
			p.submitProofOp(p.ctx, proofWithHeader, false)
		case <-p.proveNotify:
			if err := p.proveOp(); err != nil {
				log.Error("Prove new blocks error", "error", err)
			}
		case <-p.blockProposedCh:
			reqProving()
		case e := <-p.blockVerifiedCh:
			if err := p.onBlockVerified(p.ctx, e); err != nil {
				log.Error("Handle BlockVerified event error", "error", err)
			}
		case e := <-p.blockProvenCh:
			if err := p.onBlockProven(p.ctx, e); err != nil {
				log.Error("Handle BlockProven event error", "error", err)
			}
		case <-forceProvingTicker.C:
			reqProving()
		}
	}
}

// Close closes the prover instance.
func (p *Prover) Close() {
	p.closeSubscription()
	p.wg.Wait()
}

// proveOp performs a proving operation, find current unproven blocks, then
// request generating proofs for them.
func (p *Prover) proveOp() error {
	iter, err := eventIterator.NewBlockProposedIterator(p.ctx, &eventIterator.BlockProposedIteratorConfig{
		Client:               p.rpc.L1,
		TaikoL1:              p.rpc.TaikoL1,
		StartHeight:          new(big.Int).SetUint64(p.l1Current),
		OnBlockProposedEvent: p.onBlockProposed,
	})
	if err != nil {
		return err
	}

	return iter.Iter()
}

// onBlockProposed tries to prove that the newly proposed block is valid/invalid.
func (p *Prover) onBlockProposed(
	ctx context.Context,
	event *bindings.TaikoL1ClientBlockProposed,
	end eventIterator.EndBlockProposedEventIterFunc,
) error {
	// If there is newly generated proofs, we need to submit them as soon as possible.
	if len(p.proveValidProofCh) > 0 || len(p.proveInvalidProofCh) > 0 {
		end()
		return nil
	}
	if event.Id.Uint64() <= p.lastHandledBlockID {
		return nil
	}
	log.Info("Proposed block", "blockID", event.Id)
	metrics.ProverReceivedProposedBlockGauge.Update(event.Id.Int64())

	handleBlockProposedEvent := func() error {
		defer func() { <-p.proposeConcurrencyGuard }()

		// Check whether the block has been verified.
		isVerified, err := p.isBlockVerified(event.Id)
		if err != nil {
			return err
		}

		if isVerified {
			log.Info("📋 Block has been verified", "blockID", event.Id)
			return nil
		}

		needNewProof, err := p.NeedNewProof(event.Id)
		if err != nil {
			return fmt.Errorf("failed to check whether the L2 block needs a new proof: %w", err)
		}

		if !needNewProof {
			return nil
		}

		ctx, cancelCtx := context.WithCancel(ctx)
		p.currentBlocksBeingProvenMutex.Lock()
		p.currentBlocksBeingProven[event.Id.Uint64()] = cancelFunc(func() {
			defer cancelCtx()
			if err := p.validProofSubmitter.CancelProof(ctx, event.Id); err != nil {
				log.Error("error cancelling proof", "error", err, "blockID", event.Id)
			}
		})
		p.currentBlocksBeingProvenMutex.Unlock()

		return p.validProofSubmitter.RequestProof(ctx, event)
	}

	p.proposeConcurrencyGuard <- struct{}{}

	p.l1Current = event.Raw.BlockNumber
	p.lastHandledBlockID = event.Id.Uint64()

	go func() {
		if err := handleBlockProposedEvent(); err != nil {
			p.currentBlocksBeingProvenMutex.Lock()
			delete(p.currentBlocksBeingProven, event.Id.Uint64())
			p.currentBlocksBeingProvenMutex.Unlock()
			log.Error("Handle new BlockProposed event error", "error", err)
		}
	}()

	return nil
}

// submitProofOp performs a (valid block / invalid block) proof submission operation.
func (p *Prover) submitProofOp(ctx context.Context, proofWithHeader *proofProducer.ProofWithHeader, isValidProof bool) {
	p.submitProofConcurrencyGuard <- struct{}{}
	go func() {
		defer func() {
			<-p.submitProofConcurrencyGuard
			p.currentBlocksBeingProvenMutex.Lock()
			delete(p.currentBlocksBeingProven, proofWithHeader.Meta.Id)
			p.currentBlocksBeingProvenMutex.Unlock()
		}()

		if err := p.validProofSubmitter.SubmitProof(p.ctx, proofWithHeader); err != nil {
			log.Error("Submit proof error", "isValidProof", isValidProof, "error", err)
		}
	}()
}

// onBlockVerified update the latestVerified block in current state, and cancels
// the block being proven if it's verified.
func (p *Prover) onBlockVerified(ctx context.Context, event *bindings.TaikoL1ClientBlockVerified) error {
	metrics.ProverLatestVerifiedIDGauge.Update(event.Id.Int64())
	p.latestVerifiedL1Height = event.Raw.BlockNumber

	if event.BlockHash == (common.Hash{}) {
		log.Info("New verified invalid block", "blockID", event.Id)
		return nil
	}

	log.Info("New verified valid block", "blockID", event.Id, "hash", common.BytesToHash(event.BlockHash[:]))

	// cancel any proofs being generated for this block
	p.cancelProof(ctx, event.Id.Uint64())

	return nil
}

// onBlockProven cancels proof generation if the proof is being generated by this prover,
// and the proof is not the oracle proof address.
func (p *Prover) onBlockProven(ctx context.Context, event *bindings.TaikoL1ClientBlockProven) error {
	metrics.ProverReceivedProvenBlockGauge.Update(event.Id.Int64())
	// if this proof is submitted by an oracle prover or a system prover, dont cancel proof.
	if event.Prover == p.oracleProverAddress ||
		event.Prover == p.systemProverAddress ||
		event.Prover == common.HexToAddress("0x0000000000000000000000000000000000000000") ||
		event.Prover == common.HexToAddress("0x0000000000000000000000000000000000000001") {
		return nil
	}

	// cancel any proofs being generated for this block
	if err := p.cancelProofIfValid(ctx, event.Id.Uint64(), uint64(event.ParentGasUsed), event.ParentHash); err != nil {
		return err
	}

	return nil
}

// Name returns the application name.
func (p *Prover) Name() string {
	return "prover"
}

// initL1Current initializes prover's L1Current cursor.
func (p *Prover) initL1Current(startingBlockID *big.Int) error {
	if err := p.rpc.WaitTillL2Synced(p.ctx); err != nil {
		return err
	}

	if startingBlockID == nil {
		stateVars, err := p.rpc.GetProtocolStateVariables(nil)
		if err != nil {
			return err
		}

		if stateVars.LastVerifiedBlockId == 0 {
			p.l1Current = stateVars.GenesisHeight
			return nil
		}

		startingBlockID = new(big.Int).SetUint64(stateVars.LastVerifiedBlockId)
	}

	latestVerifiedHeaderL1Origin, err := p.rpc.L2.L1OriginByID(p.ctx, startingBlockID)
	if err != nil {
		return err
	}

	p.l1Current = latestVerifiedHeaderL1Origin.L1BlockHeight.Uint64()
	return nil
}

// isBlockVerified checks whether the given block has been verified by other provers.
func (p *Prover) isBlockVerified(id *big.Int) (bool, error) {
	stateVars, err := p.rpc.GetProtocolStateVariables(nil)
	if err != nil {
		return false, err
	}

	return id.Uint64() <= stateVars.LastVerifiedBlockId, nil
}

// NeedNewProof checks whether the L2 block still needs a new proof.
func (p *Prover) NeedNewProof(id *big.Int) (bool, error) {
	if !p.cfg.OracleProver && !p.cfg.SystemProver {
		conf, err := p.rpc.TaikoL1.GetConfig(nil)
		if err != nil {
			return false, err
		}

		if id.Uint64()%conf.RealProofSkipSize.Uint64() != 0 {
			log.Info(
				"Skipping valid block proof",
				"blockID", id.Uint64(),
				"skipSize", conf.RealProofSkipSize.Uint64(),
			)

			return false, nil
		}
	}

	var parent *types.Header
	if id.Cmp(common.Big1) == 0 {
		header, err := p.rpc.L2.HeaderByNumber(p.ctx, common.Big0)
		if err != nil {
			return false, err
		}

		parent = header
	} else {
		parentL1Origin, err := p.rpc.WaitL1Origin(p.ctx, new(big.Int).Sub(id, common.Big1))
		if err != nil {
			return false, err
		}

		if parent, err = p.rpc.L2.HeaderByHash(p.ctx, parentL1Origin.L2BlockHash); err != nil {
			return false, err
		}
	}

	fc, err := p.rpc.TaikoL1.GetForkChoice(nil, id, parent.Hash(), uint32(parent.GasUsed))
	if err != nil && !strings.Contains(encoding.TryParsingCustomError(err).Error(), "L1_FORK_CHOICE_NOT_FOUND") {
		return false, encoding.TryParsingCustomError(err)
	}

	if p.proverAddress == fc.Prover {
		log.Info("📬 Block's proof has already been submitted by current prover", "blockID", id)
		return false, nil
	}

	return true, nil
}

// initSubscription initializes all subscriptions in current prover instance.
func (p *Prover) initSubscription() {
	p.blockProposedSub = rpc.SubscribeBlockProposed(p.rpc.TaikoL1, p.blockProposedCh)
	p.blockVerifiedSub = rpc.SubscribeBlockVerified(p.rpc.TaikoL1, p.blockVerifiedCh)
	p.blockProvenSub = rpc.SubscribeBlockProven(p.rpc.TaikoL1, p.blockProvenCh)
}

// closeSubscription closes all subscriptions.
func (p *Prover) closeSubscription() {
	p.blockVerifiedSub.Unsubscribe()
	p.blockProposedSub.Unsubscribe()
}

// cancelProofIfValid cancels proof only if the parentGasUsed and parentHash in the proof match what
// is expected
func (p *Prover) cancelProofIfValid(
	ctx context.Context,
	blockID uint64,
	parentGasUsed uint64,
	parentHash common.Hash,
) error {
	parent, err := p.rpc.L2ParentByBlockId(ctx, new(big.Int).SetUint64(blockID))
	if err != nil {
		return err
	}

	if parent.GasUsed == parentGasUsed && parent.Hash() == parentHash {
		p.cancelProof(ctx, blockID)
	}

	return nil
}

// cancelProof cancels local proof generation
func (p *Prover) cancelProof(ctx context.Context, blockID uint64) {
	p.currentBlocksBeingProvenMutex.Lock()
	defer p.currentBlocksBeingProvenMutex.Unlock()

	if cancel, ok := p.currentBlocksBeingProven[blockID]; ok {
		cancel()
		delete(p.currentBlocksBeingProven, blockID)
		log.Info("Cancelled proof for ", "blockID", blockID)
	}
}
