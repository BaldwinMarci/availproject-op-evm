package block

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"time"

	"github.com/0xPolygon/polygon-edge/blockchain"
	"github.com/0xPolygon/polygon-edge/consensus"
	"github.com/0xPolygon/polygon-edge/state"
	"github.com/0xPolygon/polygon-edge/types"
	"github.com/hashicorp/go-hclog"
)

var (
	ErrInvalidHash    = errors.New("invalid hash")
	ErrSignKeyMissing = errors.New("signing key missing")
)

// Builder provides a builder interface for constructing blocks.
type Builder interface {
	SetBlockNumber(number uint64) Builder
	SetCoinbaseAddress(coinbaseAddr types.Address) Builder
	SetGasLimit(limit uint64) Builder
	SetParentStateRoot(parentRoot types.Hash) Builder

	AddTransactions(txs ...*types.Transaction) Builder

	SignWith(signKey *ecdsa.PrivateKey) Builder

	Build() (*types.Block, error)
	Write(src string) error
}

type blockBuilder struct {
	blockchain *blockchain.Blockchain
	executor   *state.Executor
	logger     hclog.Logger

	coinbase   *types.Address
	parentRoot *types.Hash
	gasLimit   *uint64

	header *types.Header
	parent *types.Header

	transition   *state.Transition
	transactions []*types.Transaction
	signKey      *ecdsa.PrivateKey
}

type BlockBuilderFactory interface {
	FromParentHash(hash types.Hash) (Builder, error)
}

type blockBuilderFactory struct {
	blockchain *blockchain.Blockchain
	executor   *state.Executor
	logger     hclog.Logger
}

func NewBlockBuilderFactory(blockchain *blockchain.Blockchain, executor *state.Executor, logger hclog.Logger) BlockBuilderFactory {
	return &blockBuilderFactory{
		blockchain: blockchain,
		executor:   executor,
		logger:     logger.ResetNamed("block_builder_factory"),
	}
}

func (bbf *blockBuilderFactory) FromParentHash(parent types.Hash) (Builder, error) {
	hdr, found := bbf.blockchain.GetHeaderByHash(parent)
	if !found {
		return nil, fmt.Errorf("%w: not found", ErrInvalidHash)
	}

	return bbf.FromParentHeader(hdr)
}

func (bbf *blockBuilderFactory) FromParentHeader(parent *types.Header) (Builder, error) {
	bb := &blockBuilder{
		blockchain: bbf.blockchain,
		executor:   bbf.executor,
		logger:     bbf.logger.ResetNamed("block_builder"),

		header: &types.Header{
			ParentHash: parent.Hash,
			Number:     parent.Number + 1,
			GasLimit:   parent.GasLimit,
		},
		parent: parent,
	}

	return bb, nil
}

func (bb *blockBuilder) SetBlockNumber(n uint64) Builder {
	bb.header.Number = n
	return bb
}

func (bb *blockBuilder) SetCoinbaseAddress(coinbaseAddr types.Address) Builder {
	bb.coinbase = &coinbaseAddr
	return bb
}

func (bb *blockBuilder) SetGasLimit(limit uint64) Builder {
	bb.gasLimit = &limit
	return bb
}

func (bb *blockBuilder) SetParentStateRoot(parentRoot types.Hash) Builder {
	bb.parentRoot = &parentRoot
	return bb
}

func (bb *blockBuilder) AddTransactions(tx ...*types.Transaction) Builder {
	bb.transactions = append(bb.transactions, tx...)
	return bb
}

func (bb *blockBuilder) SignWith(signKey *ecdsa.PrivateKey) Builder {
	bb.signKey = signKey
	return bb
}

func (bb *blockBuilder) Write(src string) error {
	blk, err := bb.Build()
	if err != nil {
		return err
	}

	err = bb.blockchain.WriteBlock(blk, src)
	if err != nil {
		return err
	}

	return nil
}

func (bb *blockBuilder) setDefaults() {
	if bb.coinbase == nil {
		bb.coinbase = new(types.Address)
		*bb.coinbase = types.BytesToAddress(bb.parent.Miner)
	}

	if bb.parentRoot == nil {
		bb.parentRoot = new(types.Hash)
		*bb.parentRoot = bb.parent.StateRoot
	}

	if bb.gasLimit == nil {
		bb.gasLimit = new(uint64)
		*bb.gasLimit = 0
	}
}

func (bb *blockBuilder) Build() (*types.Block, error) {
	var err error

	// ASSERTIONS
	if bb.signKey == nil {
		return nil, ErrSignKeyMissing
	}

	// Set defaults for missing unset parameters.
	bb.setDefaults()

	// Finalize header details before transaction processing.
	bb.header.GasLimit = *bb.gasLimit
	bb.header.Miner = bb.coinbase.Bytes()
	bb.header.Timestamp = uint64(time.Now().Unix())

	// Set arbitrary gas limit for the first block if not set yet.
	if bb.header.GasLimit == 0 && bb.parent.Number == 0 {
		// This arbitrary gas limit comes from early unit tests that run with
		// empty block.
		bb.header.GasLimit = 4_715_000
	}

	// Check if the gas limit needs to be calculated.
	if bb.header.GasLimit == 0 {
		// Calculate gas limit based on parent header.
		bb.header.GasLimit, err = bb.blockchain.CalculateGasLimit(bb.parent.Number)
		if err != nil {
			return nil, err
		}
	}

	// Create a block transition.
	bb.transition, err = bb.executor.BeginTxn(*bb.parentRoot, bb.header, *bb.coinbase)
	if err != nil {
		return nil, err
	}

	// Write all transactions in-order.
	for _, tx := range bb.transactions {
		err := bb.transition.Write(tx)
		if err != nil {
			return nil, err
		}
	}
	// Commit the changes.
	_, root := bb.transition.Commit()

	// Update the headers.
	bb.header.StateRoot = root
	bb.header.GasUsed = bb.transition.TotalGas()

	// Build the actual block.
	// The header hash is computed inside BuildBlock().
	blk := consensus.BuildBlock(consensus.BuildBlockParams{
		Header:   bb.header,
		Txns:     bb.transactions,
		Receipts: bb.transition.Receipts(),
	})

	// Initialize the block header's `ExtraData`.
	err = PutValidatorExtra(blk.Header, &ValidatorExtra{Validators: []types.Address{types.BytesToAddress(bb.header.Miner)}})
	if err != nil {
		return nil, err
	}

	// ...and sign the block.
	blk.Header, err = WriteSeal(bb.signKey, blk.Header)
	if err != nil {
		return nil, err
	}

	// Compute the hash, this is only a provisional hash since the final one
	// is sealed after all the committed seals.
	blk.Header.ComputeHash()

	return blk, nil
}