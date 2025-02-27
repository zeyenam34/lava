package statetracker

import (
	"context"
	"fmt"
	"sync"
	"time"

	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/cosmos/cosmos-sdk/client"
	"github.com/lavanet/lava/protocol/rpcprovider/reliabilitymanager"
	"github.com/lavanet/lava/protocol/rpcprovider/rewardserver"
	"github.com/lavanet/lava/utils"
	conflicttypes "github.com/lavanet/lava/x/conflict/types"
	pairingtypes "github.com/lavanet/lava/x/pairing/types"
	spectypes "github.com/lavanet/lava/x/spec/types"
)

const (
	debug = false
)

type EventTracker struct {
	lock               sync.RWMutex
	clientCtx          client.Context
	blockResults       *ctypes.ResultBlockResults
	latestUpdatedBlock int64
}

func (et *EventTracker) updateBlockResults(latestBlock int64) (err error) {
	ctx := context.Background()

	if latestBlock == 0 {
		var res *ctypes.ResultStatus
		for i := 0; i < 3; i++ {
			timeoutCtx, cancel := context.WithTimeout(ctx, time.Second)
			res, err = et.clientCtx.Client.Status(timeoutCtx)
			cancel()
			if err == nil {
				break
			}
		}
		if err != nil {
			return utils.LavaFormatWarning("could not get latest block height and requested latestBlock = 0", err)
		}
		latestBlock = res.SyncInfo.LatestBlockHeight
	}
	brp, err := tryIntoTendermintRPC(et.clientCtx.Client)
	if err != nil {
		return utils.LavaFormatError("could not get block result provider", err)
	}
	var blockResults *ctypes.ResultBlockResults
	for i := 0; i < BlockResultRetry; i++ {
		timeoutCtx, cancel := context.WithTimeout(ctx, time.Second)
		blockResults, err = brp.BlockResults(timeoutCtx, &latestBlock)
		cancel()
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond * time.Duration(i+1)) // need this so it doesnt just spam the attempts, and tendermint fails getting block results pretty often
	}
	if err != nil {
		return utils.LavaFormatError("could not get block result", err)
	}
	// lock for update after successful block result query
	et.lock.Lock()
	defer et.lock.Unlock()
	if latestBlock > et.latestUpdatedBlock {
		et.latestUpdatedBlock = latestBlock
		et.blockResults = blockResults
	} else {
		utils.LavaFormatDebug("event tracker got an outdated block", utils.Attribute{Key: "block", Value: latestBlock}, utils.Attribute{Key: "latestUpdatedBlock", Value: et.latestUpdatedBlock})
	}
	return nil
}

func (et *EventTracker) getLatestPaymentEvents() (payments []*rewardserver.PaymentRequest, err error) {
	et.lock.RLock()
	defer et.lock.RUnlock()
	transactionResults := et.blockResults.TxsResults
	for _, tx := range transactionResults {
		events := tx.Events
		for _, event := range events {
			if event.Type == utils.EventPrefix+pairingtypes.RelayPaymentEventName {
				paymentList, err := rewardserver.BuildPaymentFromRelayPaymentEvent(event, et.latestUpdatedBlock)
				if err != nil {
					return nil, utils.LavaFormatError("failed relay_payment_event parsing", err, utils.Attribute{Key: "event", Value: event})
				}
				if debug {
					utils.LavaFormatDebug("relay_payment_event", utils.Attribute{Key: "payment", Value: paymentList})
				}
				payments = append(payments, paymentList...)
			}
		}
	}
	return payments, nil
}

func (et *EventTracker) getLatestVersionEvents(latestBlock int64) (updated bool, err error) {
	et.lock.RLock()
	defer et.lock.RUnlock()
	if et.latestUpdatedBlock != latestBlock {
		return false, utils.LavaFormatWarning("event results are different than expected", nil, utils.Attribute{Key: "requested latestBlock", Value: latestBlock}, utils.Attribute{Key: "current latestBlock", Value: et.latestUpdatedBlock})
	}
	for _, event := range et.blockResults.EndBlockEvents {
		if event.Type == utils.EventPrefix+"param_change" {
			for _, attribute := range event.Attributes {
				if attribute.Key == "param" && attribute.Value == "Version" {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

func (et *EventTracker) getLatestDowntimeParamsUpdateEvents(latestBlock int64) (updated bool, err error) {
	// check DowntimeParams change proposal results
	et.lock.RLock()
	defer et.lock.RUnlock()
	if et.latestUpdatedBlock != latestBlock {
		return false, utils.LavaFormatWarning("event results are different than expected", nil, utils.Attribute{Key: "requested latestBlock", Value: latestBlock}, utils.Attribute{Key: "current latestBlock", Value: et.latestUpdatedBlock})
	}
	for _, event := range et.blockResults.EndBlockEvents {
		if event.Type == utils.EventPrefix+"param_change" {
			for _, attribute := range event.Attributes {
				if attribute.Key == "param" && (attribute.Value == "DowntimeDuration" || attribute.Value == "EpochDuration") {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

func (et *EventTracker) getLatestSpecModifyEvents(latestBlock int64) (updated bool, err error) {
	// SpecModifyEventName
	et.lock.RLock()
	defer et.lock.RUnlock()
	if et.latestUpdatedBlock != latestBlock {
		return false, utils.LavaFormatWarning("event results are different than expected", nil, utils.Attribute{Key: "requested latestBlock", Value: latestBlock}, utils.Attribute{Key: "current latestBlock", Value: et.latestUpdatedBlock})
	}
	for _, event := range et.blockResults.EndBlockEvents {
		if event.Type == utils.EventPrefix+spectypes.SpecModifyEventName {
			return true, nil
		}
	}
	return false, nil
}

func (et *EventTracker) getLatestVoteEvents(latestBlock int64) (votes []*reliabilitymanager.VoteParams, err error) {
	et.lock.RLock()
	defer et.lock.RUnlock()
	if et.latestUpdatedBlock != latestBlock {
		return nil, utils.LavaFormatWarning("event results are different than expected", nil, utils.Attribute{Key: "requested latestBlock", Value: latestBlock}, utils.Attribute{Key: "current latestBlock", Value: et.latestUpdatedBlock})
	}
	transactionResults := et.blockResults.TxsResults
	for _, tx := range transactionResults {
		events := tx.Events
		for _, event := range events {
			if event.Type == utils.EventPrefix+conflicttypes.ConflictVoteDetectionEventName {
				vote, err := reliabilitymanager.BuildVoteParamsFromDetectionEvent(event)
				if err != nil {
					return nil, utils.LavaFormatError("failed conflict_vote_detection_event parsing", err, utils.Attribute{Key: "event", Value: event})
				}
				utils.LavaFormatDebug("conflict_vote_detection_event", utils.Attribute{Key: "voteID", Value: vote.VoteID})
				votes = append(votes, vote)
			}
		}
	}

	beginBlockEvents := et.blockResults.BeginBlockEvents
	for _, event := range beginBlockEvents {
		if event.Type == utils.EventPrefix+conflicttypes.ConflictVoteRevealEventName {
			voteID, voteDeadline, err := reliabilitymanager.BuildBaseVoteDataFromEvent(event)
			if err != nil {
				return nil, utils.LavaFormatError("failed conflict_vote_reveal_event parsing", err, utils.Attribute{Key: "event", Value: event})
			}
			vote_reveal := &reliabilitymanager.VoteParams{VoteID: voteID, VoteDeadline: voteDeadline, ParamsType: reliabilitymanager.RevealVoteType}
			utils.LavaFormatDebug("conflict_vote_reveal_event", utils.Attribute{Key: "voteID", Value: voteID})
			votes = append(votes, vote_reveal)
		}
		if event.Type == utils.EventPrefix+conflicttypes.ConflictVoteResolvedEventName {
			voteID, _, err := reliabilitymanager.BuildBaseVoteDataFromEvent(event)
			if err != nil {
				if !reliabilitymanager.NoVoteDeadline.Is(err) {
					return nil, utils.LavaFormatError("failed conflict_vote_resolved_event parsing", err, utils.Attribute{Key: "event", Value: event})
				}
			}
			vote_resolved := &reliabilitymanager.VoteParams{VoteID: voteID, VoteDeadline: 0, ParamsType: reliabilitymanager.CloseVoteType, CloseVote: true}
			votes = append(votes, vote_resolved)
			utils.LavaFormatDebug("conflict_vote_resolved_event", utils.Attribute{Key: "voteID", Value: voteID})
		}
	}

	return votes, err
}

type tendermintRPC interface {
	BlockResults(
		ctx context.Context,
		height *int64,
	) (*ctypes.ResultBlockResults, error)
	ConsensusParams(
		ctx context.Context,
		height *int64,
	) (*ctypes.ResultConsensusParams, error)
}

func tryIntoTendermintRPC(cl client.TendermintRPC) (tendermintRPC, error) {
	brp, ok := cl.(tendermintRPC)
	if !ok {
		return nil, fmt.Errorf("client does not implement tendermintRPC: %T", cl)
	}
	return brp, nil
}
