package polygon

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/cornerstone-labs/acorus/common/global_const"
	"github.com/cornerstone-labs/acorus/database/worker"
	"log"
	"math/big"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/cornerstone-labs/acorus/common/bigint"
	"github.com/cornerstone-labs/acorus/common/tasks"
	"github.com/cornerstone-labs/acorus/config"
	"github.com/cornerstone-labs/acorus/database"
	common2 "github.com/cornerstone-labs/acorus/database/common"
	"github.com/cornerstone-labs/acorus/database/event"
	"github.com/cornerstone-labs/acorus/event/polygon/abi"
	"github.com/cornerstone-labs/acorus/event/polygon/bridge"
	"github.com/cornerstone-labs/acorus/event/polygon/utils"
)

type PolygonEventProcessor struct {
	db              *database.DB
	cfgRpc          *config.RPC
	resourceCtx     context.Context
	resourceCancel  context.CancelFunc
	tasks           tasks.Group
	L1PolygonBridge *abi.Polygonzkevmbridge
	L2PolygonBridge *abi.Polygonzkevmbridge
	loopInterval    time.Duration
	l1StartHeight   *big.Int
	l2StartHeight   *big.Int
	epoch           uint64
}

func NewBridgeProcessor(db *database.DB,
	l1cfg *config.RPC, l2cfg *config.RPC, shutdown context.CancelCauseFunc, loopInterval time.Duration, epoch uint64) (*PolygonEventProcessor, error) {
	resCtx, resCancel := context.WithCancel(context.Background())
	ethClient, _ := ethclient.Dial(l1cfg.RpcUrl)
	zkEVMClient, _ := ethclient.Dial(l2cfg.RpcUrl)

	l1PolygonBridge, err := abi.NewPolygonzkevmbridge(utils.L1PolygonZKEVMBridgeAddr, ethClient)
	if err != nil {
		return nil, err
	}
	l2PolygonBridge, err := abi.NewPolygonzkevmbridge(utils.L2PolygonZKEVMBridgeAddr, zkEVMClient)
	if err != nil {
		return nil, err
	}
	return &PolygonEventProcessor{
		db:             db,
		cfgRpc:         l2cfg,
		resourceCtx:    resCtx,
		resourceCancel: resCancel,
		tasks: tasks.Group{HandleCrit: func(err error) {
			shutdown(fmt.Errorf("critical error in bridge processor: %w", err))
		}},
		L1PolygonBridge: l1PolygonBridge,
		L2PolygonBridge: l2PolygonBridge,
		loopInterval:    loopInterval,
		epoch:           epoch,
	}, nil
}

func (pp *PolygonEventProcessor) StartUnpack() error {
	tickerSyncer := time.NewTicker(pp.loopInterval)
	log.Println("starting polygon bridge processor...")
	pp.tasks.Go(func() error {
		for range tickerSyncer.C {
			err := pp.onL1Data()
			if err != nil {
				log.Println("no more l1 etl updates. shutting down l1 task")
				return err
			}
		}
		return nil
	})
	// start L2 worker
	pp.tasks.Go(func() error {
		for range tickerSyncer.C {
			err := pp.onL2Data()
			if err != nil {
				log.Println("no more l2 etl updates. shutting down l2 task")
				return err
			}
		}
		return nil
	})
	// start relation worker
	pp.tasks.Go(func() error {
		for range tickerSyncer.C {
			err := pp.relationL1L2()
			if err != nil {
				log.Println("shutting down relation task")
				return err
			}
		}
		return nil
	})
	return nil
}

func (pp *PolygonEventProcessor) ClosetUnpack() error {
	pp.resourceCancel()
	return pp.tasks.Wait()
}

func (pp *PolygonEventProcessor) onL1Data() error {
	chainId := pp.cfgRpc.ChainId
	chainIdStr := strconv.Itoa(int(chainId))
	if pp.l1StartHeight == nil {
		lastBlockHeard, err := pp.db.L1ToL2.L1LatestBlockHeader(chainIdStr)
		if err != nil {
			log.Println("l1 failed to get last block heard", "err", err)
			return err
		}
		if lastBlockHeard == nil {
			lastBlockHeard = &common2.BlockHeader{
				Number: big.NewInt(0),
			}
		}
		pp.l1StartHeight = lastBlockHeard.Number
		if pp.l1StartHeight.Cmp(big.NewInt(int64(pp.cfgRpc.L1EventUnpackBlock))) == -1 {
			pp.l1StartHeight = big.NewInt(int64(pp.cfgRpc.L1EventUnpackBlock))
		}
	} else {
		pp.l1StartHeight = new(big.Int).Add(pp.l1StartHeight, bigint.One)
	}
	fromL1Height := pp.l1StartHeight
	toL1Height := new(big.Int).Add(fromL1Height, big.NewInt(int64(pp.epoch)))

	chainLatestBlockHeader, err := pp.db.Blocks.ChainLatestBlockHeader(strconv.FormatUint(global_const.EthereumChainId, 10))
	if err != nil {
		return err
	}
	if chainLatestBlockHeader == nil {
		return nil
	}
	if chainLatestBlockHeader.Number.Cmp(fromL1Height) == -1 {
		return nil
	} else {
		if chainLatestBlockHeader.Number.Cmp(toL1Height) == -1 {
			toL1Height = new(big.Int).Add(fromL1Height, chainLatestBlockHeader.Number)
		}
	}

	if err := pp.db.Transaction(func(tx *database.DB) error {
		l1EventsFetchErr := pp.l1EventsFetch(fromL1Height, toL1Height)
		if l1EventsFetchErr != nil {
			return l1EventsFetchErr
		}
		return nil
	}); err != nil {
		return err
	}
	pp.l1StartHeight = toL1Height
	return nil
}

func (pp *PolygonEventProcessor) onL2Data() error {
	chainId := pp.cfgRpc.ChainId
	chainIdStr := strconv.Itoa(int(chainId))
	if pp.l2StartHeight == nil {
		lastBlockHeard, err := pp.db.L2ToL1.L2LatestBlockHeader(chainIdStr)
		if err != nil {
			log.Println("l2 failed to get last block heard", "err", err)
			return err
		}
		if lastBlockHeard == nil {
			lastBlockHeard = &common2.BlockHeader{
				Number: big.NewInt(0),
			}
		}
		pp.l2StartHeight = lastBlockHeard.Number
	} else {
		pp.l2StartHeight = new(big.Int).Add(pp.l2StartHeight, bigint.One)
	}
	fromL2Height := pp.l2StartHeight
	toL2Height := new(big.Int).Add(fromL2Height, big.NewInt(int64(pp.epoch)))
	chainLatestBlockHeader, err := pp.db.Blocks.ChainLatestBlockHeader(chainIdStr)
	if err != nil {
		return err
	}
	if chainLatestBlockHeader == nil {
		return nil
	}
	if chainLatestBlockHeader.Number.Cmp(fromL2Height) == -1 {
		return nil
	} else {
		if chainLatestBlockHeader.Number.Cmp(toL2Height) == -1 {
			toL2Height = new(big.Int).Add(fromL2Height, chainLatestBlockHeader.Number)
		}
	}
	if err := pp.db.Transaction(func(tx *database.DB) error {
		l2EventsFetchErr := pp.l2EventsFetch(fromL2Height, toL2Height)
		if l2EventsFetchErr != nil {
			return l2EventsFetchErr
		}
		return nil
	}); err != nil {
		return err
	}
	pp.l2StartHeight = toL2Height
	return nil
}

func (pp *PolygonEventProcessor) l1EventsFetch(fromL1Height, toL1Height *big.Int) error {
	l1Contracts := pp.cfgRpc.Contracts
	for _, l1contract := range l1Contracts {
		contractEventFilter := event.ContractEvent{ContractAddress: common.HexToAddress(l1contract)}
		events, err := pp.db.ContractEvents.ContractEventsWithFilter("1", contractEventFilter, fromL1Height, toL1Height)
		if err != nil {
			log.Println("failed to index L1ContractEventsWithFilter ", "err", err)
			return err
		}
		for _, contractEvent := range events {
			unpackErr := pp.l1EventUnpack(contractEvent)
			if unpackErr != nil {
				log.Println("failed to index L1 bridge events", "unpackErr", unpackErr)
				return unpackErr
			}
		}
	}
	return nil
}

func (pp *PolygonEventProcessor) l2EventsFetch(fromL1Height, toL1Height *big.Int) error {
	chainId := pp.cfgRpc.ChainId
	chainIdStr := strconv.Itoa(int(chainId))
	l2Contracts := pp.cfgRpc.EventContracts
	for _, l2contract := range l2Contracts {
		contractEventFilter := event.ContractEvent{ContractAddress: common.HexToAddress(l2contract)}
		events, err := pp.db.ContractEvents.ContractEventsWithFilter(chainIdStr, contractEventFilter, fromL1Height, toL1Height)
		if err != nil {
			log.Println("failed to index L2ContractEventsWithFilter ", "err", err)
			return err
		}
		for _, contractEvent := range events {
			unpackErr := pp.l2EventUnpack(contractEvent)
			if unpackErr != nil {
				log.Println("failed to index L2 bridge events", "unpackErr", unpackErr)
				return unpackErr
			}
		}
	}
	return nil
}

func (pp *PolygonEventProcessor) l1EventUnpack(event event.ContractEvent) error {
	chainId := pp.cfgRpc.ChainId
	chainIdStr := strconv.Itoa(int(chainId))
	switch event.EventSignature.String() {
	case utils.DepositEventSignatureHash.String():
		err := bridge.L1Deposit(chainIdStr, pp.L1PolygonBridge, event, pp.db)
		return err
	case utils.ClaimEventSignatureHash.String():
		err := bridge.L1WithdrawClaimed(chainIdStr, pp.L1PolygonBridge, event, pp.db)
		return err
	}
	return nil
}

func (pp *PolygonEventProcessor) l2EventUnpack(event event.ContractEvent) error {
	chainId := pp.cfgRpc.ChainId
	chainIdStr := strconv.Itoa(int(chainId))
	switch event.EventSignature.String() {
	case utils.WithdrawEventSignatureHash.String():
		err := bridge.L2Withdraw(chainIdStr, pp.L1PolygonBridge, event, pp.db)
		return err
	case utils.ClaimEventSignatureHash.String():
		err := bridge.L2WithdrawClaimed(chainIdStr, pp.L1PolygonBridge, event, pp.db)
		return err
	}
	return nil
}

func (pp *PolygonEventProcessor) relationL1L2() error {
	chainId := pp.cfgRpc.ChainId
	chainIdStr := strconv.Itoa(int(chainId))

	if err := pp.db.Transaction(func(tx *database.DB) error {
		// step 1
		if err := pp.db.MsgSentRelation.RelayRelation(chainIdStr); err != nil {
			return err
		}
		// step 2
		if canSaveDataList, err := pp.db.MsgSentRelation.GetCanSaveDataList(chainIdStr); err != nil {
			return err
		} else {
			l1ToL2s := make([]worker.L1ToL2, 0)
			l2ToL1s := make([]worker.L2ToL1, 0)
			for _, data := range canSaveDataList {
				l1l2Data := data.Data
				if data.LayerType == global_const.LayerTypeOne {
					var l1Tol2 worker.L1ToL2
					if unMarErr := json.Unmarshal([]byte(l1l2Data), &l1Tol2); unMarErr != nil {
						return unMarErr
					}
					l1Tol2.MessageHash = data.MsgHash
					l1Tol2.L2BlockNumber = data.LayerBlockNumber
					l1Tol2.L2TransactionHash = data.LayerHash
					l1Tol2.Status = 1
					l1ToL2s = append(l1ToL2s, l1Tol2)
				}
				if data.LayerType == global_const.LayerTypeTwo {
					var l2Tol1 worker.L2ToL1
					if unMarErr := json.Unmarshal([]byte(l1l2Data), &l2Tol1); unMarErr != nil {
						return unMarErr
					}
					l2Tol1.MessageHash = data.MsgHash
					l2Tol1.L1BlockNumber = data.LayerBlockNumber
					l2Tol1.L1FinalizeTxHash = data.LayerHash
					l2Tol1.Status = 1
					l2ToL1s = append(l2ToL1s, l2Tol1)
				}

			}

			if len(l1ToL2s) > 0 {
				saveErr := pp.db.L1ToL2.StoreL1ToL2Transactions(chainIdStr, l1ToL2s)
				if saveErr != nil {
					log.Println("failed to StoreL1ToL2Transactions", "saveErr", saveErr)
					return saveErr
				}
			}

			if len(l2ToL1s) > 0 {
				saveErr := pp.db.L2ToL1.StoreL2ToL1Transactions(chainIdStr, l2ToL1s)
				if saveErr != nil {
					log.Println("failed to StoreL2ToL1Transactions", "saveErr", saveErr)
					return saveErr
				}
			}

		}
		if err := pp.db.MsgSentRelation.L1RelationClear(chainIdStr); err != nil {
			return err
		}
		if err := pp.db.MsgSentRelation.L2RelationClear(chainIdStr); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}
