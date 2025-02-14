package keeper

import (
	"strconv"

	"cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"
	legacyerrors "github.com/cosmos/cosmos-sdk/types/errors"
	"github.com/lavanet/lava/utils"
	epochstoragetypes "github.com/lavanet/lava/x/epochstorage/types"
	"github.com/lavanet/lava/x/subscription/types"
)

const LIMIT_TOKEN_PER_CU = 100

// GetTrackedCu gets the tracked CU counter (with QoS influence) and the trackedCu entry's block
func (k Keeper) GetTrackedCu(ctx sdk.Context, sub string, provider string, chainID string, subBlock uint64) (cu uint64, found bool, key string) {
	cuTrackerKey := types.CuTrackerKey(sub, provider, chainID)
	var trackedCu types.TrackedCu
	entryBlock, _, _, found := k.cuTrackerFS.FindEntryDetailed(ctx, cuTrackerKey, subBlock, &trackedCu)
	if !found || entryBlock != subBlock {
		// entry not found/deleted -> this is the first, so not an error. return CU=0
		return 0, false, cuTrackerKey
	}
	return trackedCu.Cu, found, cuTrackerKey
}

// AddTrackedCu adds CU to the CU counters in relevant trackedCu entry
func (k Keeper) AddTrackedCu(ctx sdk.Context, sub string, provider string, chainID string, cuToAdd uint64, block uint64) error {
	cu, found, key := k.GetTrackedCu(ctx, sub, provider, chainID, block)

	// Note that the trackedCu entry usually has one version since we used
	// the subscription's block which is constant during a specific month
	// (updating an entry using append in the same block acts as ModifyEntry).
	// At most, there can be two trackedCu entries. Two entries occur
	// in the time period after a month has passed but before the payment
	// timer ended (in this time, a provider can still request payment for the previous month)
	if found {
		k.cuTrackerFS.ModifyEntry(ctx, key, block, &types.TrackedCu{Cu: cu + cuToAdd})
	} else {
		err := k.cuTrackerFS.AppendEntry(ctx, key, block, &types.TrackedCu{Cu: cuToAdd})
		if err != nil {
			return utils.LavaFormatError("cannot create new tracked CU entry", err,
				utils.Attribute{Key: "tracked_cu_key", Value: key},
				utils.Attribute{Key: "sub_block", Value: strconv.FormatUint(block, 10)},
				utils.Attribute{Key: "current_cu", Value: strconv.FormatUint(cu, 10)},
				utils.Attribute{Key: "cu_to_be_added", Value: strconv.FormatUint(cuToAdd, 10)})
		}
	}

	return nil
}

// GetAllSubTrackedCuIndices gets all the trackedCu entries that are related to a specific subscription
func (k Keeper) GetAllSubTrackedCuIndices(ctx sdk.Context, sub string) []string {
	return k.cuTrackerFS.GetAllEntryIndicesWithPrefix(ctx, sub)
}

// removeCuTracker removes a trackedCu entry
func (k Keeper) resetCuTracker(ctx sdk.Context, sub string, info trackedCuInfo, subBlock uint64) error {
	key := types.CuTrackerKey(sub, info.provider, info.chainID)
	var trackedCu types.TrackedCu
	_, _, isLatest, _ := k.cuTrackerFS.FindEntryDetailed(ctx, key, subBlock, &trackedCu)
	if isLatest {
		return k.cuTrackerFS.DelEntry(ctx, key, uint64(ctx.BlockHeight()))
	}
	return nil
}

type trackedCuInfo struct {
	provider  string
	chainID   string
	trackedCu uint64
	block     uint64
}

func (k Keeper) GetSubTrackedCuInfo(ctx sdk.Context, sub string, subBlockStr string) (trackedCuList []trackedCuInfo, totalCuTracked uint64) {
	keys := k.GetAllSubTrackedCuIndices(ctx, sub)

	for _, key := range keys {
		_, provider, chainID := types.DecodeCuTrackerKey(key)
		block, err := strconv.ParseUint(subBlockStr, 10, 64)
		if err != nil {
			utils.LavaFormatError("cannot remove cu tracker", err,
				utils.Attribute{Key: "sub", Value: sub},
				utils.Attribute{Key: "provider", Value: provider},
				utils.Attribute{Key: "chain_id", Value: chainID},
				utils.Attribute{Key: "block_str", Value: subBlockStr},
			)
			continue
		}

		cu, found, _ := k.GetTrackedCu(ctx, sub, provider, chainID, block)
		if !found {
			utils.LavaFormatWarning("cannot remove cu tracker", legacyerrors.ErrKeyNotFound,
				utils.Attribute{Key: "sub", Value: sub},
				utils.Attribute{Key: "provider", Value: provider},
				utils.Attribute{Key: "chain_id", Value: chainID},
				utils.Attribute{Key: "block", Value: subBlockStr},
			)
			continue
		}
		trackedCuList = append(trackedCuList, trackedCuInfo{
			provider:  provider,
			trackedCu: cu,
			chainID:   chainID,
			block:     block,
		})
		totalCuTracked += cu
	}

	return trackedCuList, totalCuTracked
}

// remove only before the sub is deleted
func (k Keeper) RewardAndResetCuTracker(ctx sdk.Context, cuTrackerTimerKeyBytes []byte, cuTrackerTimerData []byte) {
	sub := string(cuTrackerTimerKeyBytes)
	blockStr := string(cuTrackerTimerData)
	_, err := strconv.ParseUint(blockStr, 10, 64)
	if err != nil {
		utils.LavaFormatError(types.ErrCuTrackerPayoutFailed.Error(), err,
			utils.Attribute{Key: "blockStr", Value: blockStr},
		)
		return
	}
	trackedCuList, totalCuTracked := k.GetSubTrackedCuInfo(ctx, sub, blockStr)

	var block uint64
	if len(trackedCuList) == 0 {
		// no tracked CU for this sub, nothing to do
		return
	}

	// note: there is an implicit assumption here that the subscription's
	// plan didn't change throughout the month. Currently there is no way
	// of altering the subscription's plan after being bought, but if there
	// might be in the future, this code should change
	block = trackedCuList[0].block
	plan, err := k.GetPlanFromSubscription(ctx, sub, block)
	if err != nil {
		utils.LavaFormatError("cannot find subscription's plan", types.ErrCuTrackerPayoutFailed,
			utils.Attribute{Key: "sub_consumer", Value: sub},
		)
		return
	}

	totalTokenAmount := plan.Price.Amount
	if plan.Price.Amount.Quo(sdk.NewIntFromUint64(totalCuTracked)).GT(sdk.NewIntFromUint64(LIMIT_TOKEN_PER_CU)) {
		totalTokenAmount = sdk.NewIntFromUint64(LIMIT_TOKEN_PER_CU * totalCuTracked)
	}

	for _, trackedCuInfo := range trackedCuList {
		trackedCu := trackedCuInfo.trackedCu
		provider := trackedCuInfo.provider
		chainID := trackedCuInfo.chainID

		err := k.resetCuTracker(ctx, sub, trackedCuInfo, block)
		if err != nil {
			utils.LavaFormatError("removing/reseting tracked CU entry failed", err,
				utils.Attribute{Key: "provider", Value: provider},
				utils.Attribute{Key: "tracked_cu", Value: trackedCu},
				utils.Attribute{Key: "chain_id", Value: chainID},
				utils.Attribute{Key: "sub", Value: sub},
				utils.Attribute{Key: "block", Value: ctx.BlockHeight()},
			)
			return
		}

		// provider monthly reward = (tracked_CU / total_CU_used_in_sub_this_month) * plan_price
		// TODO: deal with the reward's remainder (uint division...)

		totalMonthlyReward := k.CalcTotalMonthlyReward(ctx, totalTokenAmount, trackedCu, totalCuTracked)

		// calculate the provider reward (smaller than totalMonthlyReward
		// because it's shared with delegators)
		providerAddr, err := sdk.AccAddressFromBech32(provider)
		if err != nil {
			utils.LavaFormatError("invalid provider address", err,
				utils.Attribute{Key: "provider", Value: provider},
			)
			return
		}

		// Note: if the reward function doesn't reward the provider
		// because he was unstaked, we only print an error and not returning
		providerReward, err := k.dualstakingKeeper.RewardProvidersAndDelegators(ctx, providerAddr, chainID, totalMonthlyReward, types.ModuleName, false, false, false)
		if err == epochstoragetypes.ErrProviderNotStaked || err == epochstoragetypes.ErrStakeStorageNotFound {
			utils.LavaFormatWarning("sending provider reward with delegations failed", err,
				utils.Attribute{Key: "provider", Value: provider},
				utils.Attribute{Key: "chain_id", Value: chainID},
				utils.Attribute{Key: "block", Value: strconv.FormatInt(ctx.BlockHeight(), 10)},
			)
		} else if err != nil {
			utils.LavaFormatError("sending provider reward with delegations failed", err,
				utils.Attribute{Key: "provider", Value: provider},
				utils.Attribute{Key: "tracked_cu", Value: trackedCu},
				utils.Attribute{Key: "chain_id", Value: chainID},
				utils.Attribute{Key: "sub", Value: sub},
				utils.Attribute{Key: "sub_total_used_cu", Value: totalCuTracked},
				utils.Attribute{Key: "block", Value: ctx.BlockHeight()},
			)
			return
		} else {
			utils.LogLavaEvent(ctx, k.Logger(ctx), types.MonthlyCuTrackerProviderRewardEventName, map[string]string{
				"provider":   provider,
				"sub":        sub,
				"plan":       plan.Index,
				"tracked_cu": strconv.FormatUint(trackedCu, 10),
				"plan_price": plan.Price.String(),
				"reward":     providerReward.String(),
				"block":      strconv.FormatInt(ctx.BlockHeight(), 10),
			}, "Provider got monthly reward successfully")
		}
	}
}

func (k Keeper) CalcTotalMonthlyReward(ctx sdk.Context, totalAmount math.Int, trackedCu uint64, totalCuUsedBySub uint64) math.Int {
	// TODO: deal with the reward's remainder (uint division...)
	// monthly reward = (tracked_CU / total_CU_used_in_sub_this_month) * plan_price
	if totalCuUsedBySub == 0 {
		return math.ZeroInt()
	}
	totalMonthlyReward := totalAmount.MulRaw(int64(trackedCu)).QuoRaw(int64(totalCuUsedBySub))
	return totalMonthlyReward
}
