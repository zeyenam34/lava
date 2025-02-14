package lvstatetracker

import (
	"context"
	"time"

	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/client/tx"
	"github.com/lavanet/lava/protocol/chaintracker"
	"github.com/lavanet/lava/protocol/statetracker"
	"github.com/lavanet/lava/utils"
	protocoltypes "github.com/lavanet/lava/x/protocol/types"
	spectypes "github.com/lavanet/lava/x/spec/types"
)

// Lava visor doesn't require complicated state tracker, it just needs to periodically fetch the protocol version.
type LavaVisorStateTracker struct {
	stateQuery       *statetracker.StateQuery
	averageBlockTime time.Duration
	ticker           *time.Ticker
	versionUpdater   *LavaVisorVersionUpdater
}

func NewLavaVisorStateTracker(ctx context.Context, txFactory tx.Factory, clientCtx client.Context, chainFetcher chaintracker.ChainFetcher) (lvst *LavaVisorStateTracker, err error) {
	// validate chainId
	status, err := clientCtx.Client.Status(ctx)
	if err != nil {
		return nil, utils.LavaFormatError("[Lavavisor] failed getting status", err)
	}
	if txFactory.ChainID() != status.NodeInfo.Network {
		return nil, utils.LavaFormatError("[Lavavisor] Chain ID mismatch", nil, utils.Attribute{Key: "--chain-id", Value: txFactory.ChainID()}, utils.Attribute{Key: "Node chainID", Value: status.NodeInfo.Network})
	}
	specQueryClient := spectypes.NewQueryClient(clientCtx)
	specResponse, err := specQueryClient.Spec(ctx, &spectypes.QueryGetSpecRequest{
		ChainID: "LAV1",
	})
	if err != nil {
		utils.LavaFormatFatal("blockchain missing LAV1 spec, cant initialize lavavisor", err)
	}
	for i := 0; i < statetracker.BlockResultRetry && err != nil; i++ {
		specResponse, err = specQueryClient.Spec(ctx, &spectypes.QueryGetSpecRequest{
			ChainID: "LAV1",
		})
	}

	lst := &LavaVisorStateTracker{stateQuery: statetracker.NewStateQuery(ctx, clientCtx), averageBlockTime: time.Duration(specResponse.Spec.AverageBlockTime) * time.Millisecond}
	return lst, nil
}

func (lst *LavaVisorStateTracker) RegisterForVersionUpdates(ctx context.Context, version *protocoltypes.Version, versionValidator statetracker.VersionValidationInf) {
	lst.versionUpdater = &LavaVisorVersionUpdater{VersionUpdater: statetracker.VersionUpdater{
		VersionStateQuery:    lst.stateQuery,
		LastKnownVersion:     &statetracker.ProtocolVersionResponse{Version: version, BlockNumber: "uninitialized"},
		VersionValidationInf: versionValidator,
	}}
	lst.ticker = time.NewTicker(lst.averageBlockTime)
	lst.versionUpdater.Update()
	go func() {
		for {
			select {
			case <-lst.ticker.C:
				lst.versionUpdater.Update()
				lst.ticker = time.NewTicker(lst.averageBlockTime)
			case <-ctx.Done():
				lst.ticker.Stop()
				return
			}
		}
	}()
}

func (lst *LavaVisorStateTracker) GetProtocolVersion(ctx context.Context) (*statetracker.ProtocolVersionResponse, error) {
	return lst.stateQuery.GetProtocolVersion(ctx)
}

type LavaVisorVersionUpdater struct {
	statetracker.VersionUpdater
}

// monitor protocol version on each new block, this method overloads the update
// method of version updater in protocol because here we fetch the protocol version every block
// instead of listening to events which is not necessary in lava visor
func (vu *LavaVisorVersionUpdater) Update() {
	vu.Lock.Lock()
	defer vu.Lock.Unlock()
	// fetch updated version from consensus
	version, err := vu.VersionStateQuery.GetProtocolVersion(context.Background())
	if err != nil {
		utils.LavaFormatError("[Lavavisor] could not get version from node, its possible the node is down", err)
		return
	}
	vu.LastKnownVersion = version
	err = vu.ValidateProtocolVersion(vu.LastKnownVersion)
	if err != nil {
		utils.LavaFormatError("[Lavavisor] Validate Protocol Version Error", err)
	}
}
