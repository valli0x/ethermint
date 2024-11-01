package stream

import (
	"context"
	"fmt"
	"sync"

	"cosmossdk.io/log"
	cmtquery "github.com/cometbft/cometbft/libs/pubsub/query"
	rpcclient "github.com/cometbft/cometbft/rpc/client"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	tmtypes "github.com/cometbft/cometbft/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/evmos/ethermint/rpc/types"
	ethermint "github.com/evmos/ethermint/types"
	evmtypes "github.com/evmos/ethermint/x/evm/types"
	"google.golang.org/grpc"
)

const (
	streamSubscriberName = "ethermint-json-rpc"
	subscribBufferSize   = 1024

	headerStreamSegmentSize = 128
	headerStreamCapacity    = 128 * 32
	txStreamSegmentSize     = 1024
	txStreamCapacity        = 1024 * 32
	logStreamSegmentSize    = 2048
	logStreamCapacity       = 2048 * 32
)

var (
	evmEvents = cmtquery.MustCompile(fmt.Sprintf("%s='%s' AND %s.%s='%s'",
		tmtypes.EventTypeKey,
		tmtypes.EventTx,
		sdk.EventTypeMessage,
		sdk.AttributeKeyModule, evmtypes.ModuleName)).String()
	blockEvents  = tmtypes.QueryForEvent(tmtypes.EventNewBlock).String()
	evmTxHashKey = fmt.Sprintf("%s.%s", evmtypes.TypeMsgEthereumTx, evmtypes.AttributeKeyEthereumTxHash)
)

type RPCHeader struct {
	EthHeader *ethtypes.Header
	Hash      common.Hash
}

type validatorAccountFunc func(
	ctx context.Context, in *evmtypes.QueryValidatorAccountRequest, opts ...grpc.CallOption,
) (*evmtypes.QueryValidatorAccountResponse, error)

// RPCStream provides data streams for newHeads, logs, and pendingTransactions.
// it's only started on demand, so there's no overhead if the filter apis are not called at all.
type RPCStream struct {
	evtClient rpcclient.EventsClient
	logger    log.Logger
	txDecoder sdk.TxDecoder

	// headerStream/logStream are backed by cometbft event subscription
	headerStream *Stream[RPCHeader]
	logStream    *Stream[*ethtypes.Log]

	// pendingTxStream is backed by check-tx ante handler
	pendingTxStream *Stream[common.Hash]

	wg               sync.WaitGroup
	validatorAccount validatorAccountFunc
}

func NewRPCStreams(
	evtClient rpcclient.EventsClient,
	logger log.Logger,
	txDecoder sdk.TxDecoder,
	validatorAccount validatorAccountFunc,
) *RPCStream {
	return &RPCStream{
		evtClient:        evtClient,
		logger:           logger,
		txDecoder:        txDecoder,
		validatorAccount: validatorAccount,
		pendingTxStream:  NewStream[common.Hash](txStreamSegmentSize, txStreamCapacity),
	}
}

func (s *RPCStream) initSubscriptions() {
	if s.headerStream != nil {
		// already initialized
		return
	}

	s.headerStream = NewStream[RPCHeader](headerStreamSegmentSize, headerStreamCapacity)
	s.logStream = NewStream[*ethtypes.Log](logStreamSegmentSize, logStreamCapacity)

	ctx := context.Background()

	chBlocks, err := s.evtClient.Subscribe(ctx, streamSubscriberName, blockEvents, subscribBufferSize)
	if err != nil {
		panic(err)
	}

	chLogs, err := s.evtClient.Subscribe(ctx, streamSubscriberName, evmEvents, subscribBufferSize)
	if err != nil {
		if err := s.evtClient.UnsubscribeAll(context.Background(), streamSubscriberName); err != nil {
			s.logger.Error("failed to unsubscribe", "err", err)
		}
		panic(err)
	}

	go s.start(&s.wg, chBlocks, chLogs)
}

func (s *RPCStream) Close() error {
	if s.headerStream == nil {
		// not initialized
		return nil
	}

	if err := s.evtClient.UnsubscribeAll(context.Background(), streamSubscriberName); err != nil {
		return err
	}
	s.wg.Wait()
	return nil
}

func (s *RPCStream) HeaderStream() *Stream[RPCHeader] {
	s.initSubscriptions()
	return s.headerStream
}

func (s *RPCStream) PendingTxStream() *Stream[common.Hash] {
	return s.pendingTxStream
}

func (s *RPCStream) LogStream() *Stream[*ethtypes.Log] {
	s.initSubscriptions()
	return s.logStream
}

// ListenPendingTx is a callback passed to application to listen for pending transactions in CheckTx.
func (s *RPCStream) ListenPendingTx(hash common.Hash) {
	s.PendingTxStream().Add(hash)
}

func (s *RPCStream) start(
	wg *sync.WaitGroup,
	chBlocks <-chan coretypes.ResultEvent,
	chLogs <-chan coretypes.ResultEvent,
) {
	wg.Add(1)
	defer func() {
		wg.Done()
		if err := s.evtClient.UnsubscribeAll(context.Background(), streamSubscriberName); err != nil {
			s.logger.Error("failed to unsubscribe", "err", err)
		}
	}()

	for {
		select {
		case ev, ok := <-chBlocks:
			if !ok {
				chBlocks = nil
				break
			}

			data, ok := ev.Data.(tmtypes.EventDataNewBlock)
			if !ok {
				s.logger.Error("event data type mismatch", "type", fmt.Sprintf("%T", ev.Data))
				continue
			}

			baseFee := types.BaseFeeFromEvents(data.ResultFinalizeBlock.Events)
			res, err := s.validatorAccount(
				types.ContextWithHeight(data.Block.Height),
				&evmtypes.QueryValidatorAccountRequest{
					ConsAddress: sdk.ConsAddress(data.Block.Header.ProposerAddress).String(),
				},
			)
			if err != nil {
				s.logger.Error("failed to get validator account", "err", err)
				continue
			}
			validator, err := sdk.AccAddressFromBech32(res.AccountAddress)
			if err != nil {
				s.logger.Error("failed to convert validator account", "err", err)
				continue
			}
			// TODO: fetch bloom from events
			header := types.EthHeaderFromTendermint(data.Block.Header, ethtypes.Bloom{}, baseFee, validator)
			s.headerStream.Add(RPCHeader{EthHeader: header, Hash: common.BytesToHash(data.Block.Header.Hash())})

		case ev, ok := <-chLogs:
			if !ok {
				chLogs = nil
				break
			}

			if _, ok := ev.Events[evmTxHashKey]; !ok {
				// ignore transaction as it's not from the evm module
				continue
			}

			// get transaction result data
			dataTx, ok := ev.Data.(tmtypes.EventDataTx)
			if !ok {
				s.logger.Error("event data type mismatch", "type", fmt.Sprintf("%T", ev.Data))
				continue
			}
			height, err := ethermint.SafeUint64(dataTx.TxResult.Height)
			if err != nil {
				continue
			}
			txLogs, err := evmtypes.DecodeTxLogsFromEvents(dataTx.TxResult.Result.Data, dataTx.TxResult.Result.Events, height)
			if err != nil {
				s.logger.Error("fail to decode evm tx response", "error", err.Error())
				continue
			}

			s.logStream.Add(txLogs...)
		}

		if chBlocks == nil && chLogs == nil {
			break
		}
	}
}
