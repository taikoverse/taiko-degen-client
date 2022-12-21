package prover

import (
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/taikoxyz/taiko-client/cmd/flags"
	"github.com/urfave/cli/v2"
)

// Config contains the configurations to initialize a Taiko prover.
type Config struct {
	L1Endpoint                      string
	L2Endpoint                      string
	TaikoL1Address                  common.Address
	TaikoL2Address                  common.Address
	L1ProverPrivKey                 *ecdsa.PrivateKey
	ZKEvmRpcdEndpoint               string
	ZkEvmRpcdParamsPath             string
	StartingBlockID                 *big.Int
	MaxConcurrentProvingJobs        uint
	Dummy                           bool
	RandomDummyProofDelayLowerBound *time.Duration
	RandomDummyProofDelayUpperBound *time.Duration
}

// NewConfigFromCliContext creates a new config instance from command line flags.
func NewConfigFromCliContext(c *cli.Context) (*Config, error) {
	l1ProverPrivKeyStr := c.String(flags.L1ProverPrivKey.Name)

	l1ProverPrivKey, err := crypto.ToECDSA(common.Hex2Bytes(l1ProverPrivKeyStr))
	if err != nil {
		return nil, fmt.Errorf("invalid L1 prover private key: %w", err)
	}

	var (
		randomDummyProofDelayLowerBound *time.Duration
		randomDummyProofDelayUpperBound *time.Duration
	)
	if c.IsSet(flags.RandomDummyProofDelay.Name) {
		flagValue := c.String(flags.RandomDummyProofDelay.Name)
		splitted := strings.Split(flagValue, "-")
		if len(splitted) != 2 {
			return nil, fmt.Errorf("invalid random dummy proof delay value: %s", flagValue)
		}

		lower, err := time.ParseDuration(splitted[0])
		if err != nil {
			return nil, fmt.Errorf("invalid random dummy proof delay value: %s, err: %w", flagValue, err)
		}
		upper, err := time.ParseDuration(splitted[1])
		if err != nil {
			return nil, fmt.Errorf("invalid random dummy proof delay value: %s, err: %w", flagValue, err)
		}
		if lower > upper {
			return nil, fmt.Errorf("invalid random dummy proof delay value (lower > upper): %s", flagValue)
		}

		if upper != time.Duration(0) {
			randomDummyProofDelayLowerBound = &lower
			randomDummyProofDelayUpperBound = &upper
		}
	}

	var startingBlockID *big.Int
	if c.IsSet(flags.StartingBlockID.Name) {
		startingBlockID = new(big.Int).SetUint64(c.Uint64(flags.StartingBlockID.Name))
	}

	return &Config{
		L1Endpoint:                      c.String(flags.L1WSEndpoint.Name),
		L2Endpoint:                      c.String(flags.L2WSEndpoint.Name),
		TaikoL1Address:                  common.HexToAddress(c.String(flags.TaikoL1Address.Name)),
		TaikoL2Address:                  common.HexToAddress(c.String(flags.TaikoL2Address.Name)),
		L1ProverPrivKey:                 l1ProverPrivKey,
		ZKEvmRpcdEndpoint:               c.String(flags.ZkEvmRpcdEndpoint.Name),
		ZkEvmRpcdParamsPath:             c.String(flags.ZkEvmRpcdParamsPath.Name),
		StartingBlockID:                 startingBlockID,
		MaxConcurrentProvingJobs:        c.Uint(flags.MaxConcurrentProvingJobs.Name),
		Dummy:                           c.Bool(flags.Dummy.Name),
		RandomDummyProofDelayLowerBound: randomDummyProofDelayLowerBound,
		RandomDummyProofDelayUpperBound: randomDummyProofDelayUpperBound,
	}, nil
}
