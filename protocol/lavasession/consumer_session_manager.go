package lavasession

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdkerrors "cosmossdk.io/errors"
	"github.com/lavanet/lava/protocol/common"
	metrics "github.com/lavanet/lava/protocol/metrics"
	"github.com/lavanet/lava/protocol/provideroptimizer"
	"github.com/lavanet/lava/utils"
	"github.com/lavanet/lava/utils/rand"
	pairingtypes "github.com/lavanet/lava/x/pairing/types"
	spectypes "github.com/lavanet/lava/x/spec/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

const (
	debug = false
)

var DebugProbes = false

// created with NewConsumerSessionManager
type ConsumerSessionManager struct {
	rpcEndpoint    *RPCEndpoint // used to filter out endpoints
	lock           sync.RWMutex
	pairing        map[string]*ConsumerSessionsWithProvider // key == provider address
	currentEpoch   uint64
	numberOfResets uint64
	// pairingAddresses for Data reliability
	pairingAddresses       map[uint64]string // contains all addresses from the initial pairing. and the keys are the indexes
	pairingAddressesLength uint64

	validAddresses    []string // contains all addresses that are currently valid
	addonAddresses    map[RouterKey][]string
	reportedProviders ReportedProviders
	// pairingPurge - contains all pairings that are unwanted this epoch, keeps them in memory in order to avoid release.
	// (if a consumer session still uses one of them or we want to report it.)
	pairingPurge           map[string]*ConsumerSessionsWithProvider
	providerOptimizer      ProviderOptimizer
	consumerMetricsManager *metrics.ConsumerMetricsManager
}

// this is being read in multiple locations and but never changes so no need to lock.
func (csm *ConsumerSessionManager) RPCEndpoint() RPCEndpoint {
	return *csm.rpcEndpoint
}

func (csm *ConsumerSessionManager) UpdateAllProviders(epoch uint64, pairingList map[uint64]*ConsumerSessionsWithProvider) error {
	pairingListLength := len(pairingList)
	// TODO: we can block updating until some of the probing is done, this can prevent failed attempts on epoch change when we have no information on the providers,
	// and all of them are new (less effective on big pairing lists or a process that runs for a few epochs)
	defer func() {
		// run this after done updating pairing
		time.Sleep(time.Duration(rand.Intn(500)) * time.Millisecond) // sleep up to 500ms in order to scatter different chains probe triggers
		ctx := context.Background()
		go csm.probeProviders(ctx, pairingList, epoch) // probe providers to eliminate offline ones from affecting relays, pairingList is thread safe it's members are not (accessed through csm.pairing)
	}()
	csm.lock.Lock()         // start by locking the class lock.
	defer csm.lock.Unlock() // we defer here so in case we return an error it will unlock automatically.

	if epoch <= csm.atomicReadCurrentEpoch() { // sentry shouldn't update an old epoch or current epoch
		return utils.LavaFormatError("trying to update provider list for older epoch", nil, utils.Attribute{Key: "epoch", Value: epoch}, utils.Attribute{Key: "currentEpoch", Value: csm.atomicReadCurrentEpoch()})
	}
	// Update Epoch.
	csm.atomicWriteCurrentEpoch(epoch)

	// Reset States
	// csm.validAddresses length is reset in setValidAddressesToDefaultValue
	csm.pairingAddresses = make(map[uint64]string, 0)

	csm.reportedProviders.Reset()
	csm.pairingAddressesLength = uint64(pairingListLength)
	csm.numberOfResets = 0
	csm.RemoveAddonAddresses("", nil)
	// Reset the pairingPurge.
	// This happens only after an entire epoch. so its impossible to have session connected to the old purged list
	csm.closePurgedUnusedPairingsConnections() // this must be before updating csm.pairingPurge as we want to close the connections of older sessions (prev 2 epochs)
	csm.pairingPurge = csm.pairing
	csm.pairing = make(map[string]*ConsumerSessionsWithProvider, pairingListLength)
	for idx, provider := range pairingList {
		csm.pairingAddresses[idx] = provider.PublicLavaAddress
		csm.pairing[provider.PublicLavaAddress] = provider
	}
	csm.setValidAddressesToDefaultValue("", nil) // the starting point is that valid addresses are equal to pairing addresses.
	csm.resetMetricsManager()
	utils.LavaFormatDebug("updated providers", utils.Attribute{Key: "epoch", Value: epoch}, utils.Attribute{Key: "spec", Value: csm.rpcEndpoint.Key()})
	return nil
}

func (csm *ConsumerSessionManager) Initialized() bool {
	csm.lock.RLock()         // start by locking the class lock.
	defer csm.lock.RUnlock() // we defer here so in case we return an error it will unlock automatically.
	return len(csm.pairingAddresses) != 0
}

func (csm *ConsumerSessionManager) RemoveAddonAddresses(addon string, extensions []string) {
	if addon == "" && len(extensions) == 0 {
		// purge all
		csm.addonAddresses = make(map[RouterKey][]string)
	} else {
		routerKey := NewRouterKey(append(extensions, addon))
		if csm.addonAddresses == nil {
			csm.addonAddresses = make(map[RouterKey][]string)
		}
		csm.addonAddresses[routerKey] = []string{}
	}
}

// csm is Rlocked
func (csm *ConsumerSessionManager) CalculateAddonValidAddresses(addon string, extensions []string) (supportingProviderAddresses []string) {
	for _, providerAdress := range csm.validAddresses {
		providerEntry := csm.pairing[providerAdress]
		if providerEntry.IsSupportingAddon(addon) && providerEntry.IsSupportingExtensions(extensions) {
			supportingProviderAddresses = append(supportingProviderAddresses, providerAdress)
		}
	}
	return supportingProviderAddresses
}

// assuming csm is Rlocked
func (csm *ConsumerSessionManager) getValidAddresses(addon string, extensions []string) (addresses []string) {
	routerKey := NewRouterKey(append(extensions, addon))
	if csm.addonAddresses == nil || csm.addonAddresses[routerKey] == nil {
		return csm.CalculateAddonValidAddresses(addon, extensions)
	}
	return csm.addonAddresses[routerKey]
}

// After 2 epochs we need to close all open connections.
// otherwise golang garbage collector is not closing network connections and they
// will remain open forever.
func (csm *ConsumerSessionManager) closePurgedUnusedPairingsConnections() {
	for _, purgedPairing := range csm.pairingPurge {
		for _, endpoint := range purgedPairing.Endpoints {
			if endpoint.connection != nil {
				endpoint.connection.Close()
			}
		}
	}
}

func (csm *ConsumerSessionManager) probeProviders(ctx context.Context, pairingList map[uint64]*ConsumerSessionsWithProvider, epoch uint64) error {
	guid := utils.GenerateUniqueIdentifier()
	ctx = utils.AppendUniqueIdentifier(ctx, guid)
	if DebugProbes {
		utils.LavaFormatInfo("providers probe initiated", utils.Attribute{Key: "endpoint", Value: csm.rpcEndpoint}, utils.Attribute{Key: "GUID", Value: ctx}, utils.Attribute{Key: "epoch", Value: epoch})
	}
	// Create a wait group to synchronize the goroutines
	wg := sync.WaitGroup{}
	wg.Add(len(pairingList)) // increment by this and not by 1 for each go routine because we don;t want a race finishing the go routine before the next invocation
	for _, consumerSessionWithProvider := range pairingList {
		// Start a new goroutine for each provider
		go func(consumerSessionsWithProvider *ConsumerSessionsWithProvider) {
			// Call the probeProvider function and defer the WaitGroup Done call
			defer wg.Done()
			latency, providerAddress, err := csm.probeProvider(ctx, consumerSessionsWithProvider, epoch)
			success := err == nil // if failure then regard it in availability
			csm.providerOptimizer.AppendProbeRelayData(providerAddress, latency, success)
		}(consumerSessionWithProvider)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		wg.Wait()
	}()

	select {
	case <-done:
		// all probes finished in time
		if DebugProbes {
			utils.LavaFormatDebug("providers probe done", utils.Attribute{Key: "endpoint", Value: csm.rpcEndpoint}, utils.Attribute{Key: "GUID", Value: ctx}, utils.Attribute{Key: "epoch", Value: epoch})
		}
		return nil
	case <-ctx.Done():
		utils.LavaFormatWarning("providers probe ran out of time", nil, utils.Attribute{Key: "endpoint", Value: csm.rpcEndpoint}, utils.Attribute{Key: "GUID", Value: ctx}, utils.Attribute{Key: "epoch", Value: epoch})
		// ran out of time
		return ctx.Err()
	}
}

// this code needs to be thread safe
func (csm *ConsumerSessionManager) probeProvider(ctx context.Context, consumerSessionsWithProvider *ConsumerSessionsWithProvider, epoch uint64) (latency time.Duration, providerAddress string, err error) {
	// TODO: fetch all endpoints not just one
	connected, endpoint, providerAddress, err := consumerSessionsWithProvider.fetchEndpointConnectionFromConsumerSessionWithProvider(ctx)
	if err != nil || !connected {
		if AllProviderEndpointsDisabledError.Is(err) {
			csm.blockProvider(providerAddress, true, epoch, MaxConsecutiveConnectionAttempts, 0, csm.GenerateReconnectCallback(consumerSessionsWithProvider)) // reporting and blocking provider this epoch
		}
		return 0, providerAddress, err
	}

	relaySentTime := time.Now()
	connectCtx, cancel := context.WithTimeout(ctx, common.AverageWorldLatency)
	defer cancel()
	guid, found := utils.GetUniqueIdentifier(connectCtx)
	if !found {
		return 0, providerAddress, utils.LavaFormatError("probeProvider failed fetching unique identifier from context when it's set", nil)
	}
	if endpoint.Client == nil {
		consumerSessionsWithProvider.Lock.Lock()
		defer consumerSessionsWithProvider.Lock.Unlock()
		return 0, providerAddress, utils.LavaFormatError("returned nil client in endpoint", nil, utils.Attribute{Key: "consumerSessionWithProvider", Value: consumerSessionsWithProvider})
	}
	client := *endpoint.Client
	probeReq := &pairingtypes.ProbeRequest{
		Guid:         guid,
		SpecId:       csm.rpcEndpoint.ChainID,
		ApiInterface: csm.rpcEndpoint.ApiInterface,
	}
	var trailer metadata.MD
	probeResp, err := client.Probe(connectCtx, probeReq, grpc.Trailer(&trailer))
	versions := trailer.Get(common.VersionMetadataKey)
	relayLatency := time.Since(relaySentTime)
	if err != nil {
		return 0, providerAddress, utils.LavaFormatError("probe call error", err, utils.Attribute{Key: "provider", Value: providerAddress})
	}
	providerGuid := probeResp.GetGuid()
	if providerGuid != guid {
		return 0, providerAddress, utils.LavaFormatWarning("mismatch probe response", nil, utils.Attribute{Key: "provider", Value: providerAddress}, utils.Attribute{Key: "provider Guid", Value: providerGuid}, utils.Attribute{Key: "sent guid", Value: guid})
	}
	if probeResp.LatestBlock == 0 {
		return 0, providerAddress, utils.LavaFormatWarning("provider returned 0 latest block", nil, utils.Attribute{Key: "provider", Value: providerAddress}, utils.Attribute{Key: "sent guid", Value: guid})
	}
	// public lava address is a value that is not changing, so it's thread safe
	if DebugProbes {
		utils.LavaFormatDebug("Probed provider successfully", utils.Attribute{Key: "latency", Value: relayLatency}, utils.Attribute{Key: "provider", Value: consumerSessionsWithProvider.PublicLavaAddress}, utils.LogAttr("version", strings.Join(versions, ",")))
	}
	return relayLatency, providerAddress, nil
}

// csm needs to be locked here
func (csm *ConsumerSessionManager) setValidAddressesToDefaultValue(addon string, extensions []string) {
	if addon == "" && len(extensions) == 0 {
		csm.validAddresses = make([]string, len(csm.pairingAddresses))
		index := 0
		for _, provider := range csm.pairingAddresses {
			csm.validAddresses[index] = provider
			index++
		}
	} else {
		// check if one of the pairing addresses supports the addon
	addingToValidAddresses:
		for _, provider := range csm.pairingAddresses {
			if csm.pairing[provider].IsSupportingAddon(addon) && csm.pairing[provider].IsSupportingExtensions(extensions) {
				for _, validAddress := range csm.validAddresses {
					if validAddress == provider {
						// it exists, no need to add it again
						continue addingToValidAddresses
					}
				}
				// get here only it found a supporting provider that is not valid
				csm.validAddresses = append(csm.validAddresses, provider)
			}
		}
		csm.RemoveAddonAddresses(addon, extensions) // refresh the list
		csm.addonAddresses[NewRouterKey(append(extensions, addon))] = csm.CalculateAddonValidAddresses(addon, extensions)
	}
}

// reads cs.currentEpoch atomically
func (csm *ConsumerSessionManager) atomicWriteCurrentEpoch(epoch uint64) {
	atomic.StoreUint64(&csm.currentEpoch, epoch)
}

// reads cs.currentEpoch atomically
func (csm *ConsumerSessionManager) atomicReadCurrentEpoch() (epoch uint64) {
	return atomic.LoadUint64(&csm.currentEpoch)
}

func (csm *ConsumerSessionManager) atomicReadNumberOfResets() (resets uint64) {
	return atomic.LoadUint64(&csm.numberOfResets)
}

// reset the valid addresses list and increase numberOfResets
func (csm *ConsumerSessionManager) resetValidAddresses(addon string, extensions []string) uint64 {
	csm.lock.Lock() // lock write
	defer csm.lock.Unlock()
	if len(csm.getValidAddresses(addon, extensions)) == 0 { // re verify it didn't change while waiting for lock.
		utils.LavaFormatWarning("Provider pairing list is empty, resetting state.", nil, utils.Attribute{Key: "addon", Value: addon}, utils.Attribute{Key: "extensions", Value: extensions})
		csm.setValidAddressesToDefaultValue(addon, extensions)
		csm.numberOfResets += 1
	}
	// if len(csm.validAddresses) != 0 meaning we had a reset (or an epoch change), so we need to return the numberOfResets which is currently in csm
	return csm.numberOfResets
}

func (csm *ConsumerSessionManager) cacheAddonAddresses(addon string, extensions []string) []string {
	csm.lock.Lock() // lock to set validAddresses[addon] if it's not cached
	defer csm.lock.Unlock()
	routerKey := NewRouterKey(append(extensions, addon))
	if csm.addonAddresses == nil || csm.addonAddresses[routerKey] == nil {
		csm.RemoveAddonAddresses(addon, extensions)
		csm.addonAddresses[routerKey] = csm.CalculateAddonValidAddresses(addon, extensions)
	}
	return csm.addonAddresses[routerKey]
}

// validating we still have providers, otherwise reset valid addresses list
// also caches validAddresses for an addon to save on compute
func (csm *ConsumerSessionManager) validatePairingListNotEmpty(addon string, extensions []string) uint64 {
	numberOfResets := csm.atomicReadNumberOfResets()
	validAddresses := csm.cacheAddonAddresses(addon, extensions)
	if len(validAddresses) == 0 {
		numberOfResets = csm.resetValidAddresses(addon, extensions)
	}
	return numberOfResets
}

// GetSessions will return a ConsumerSession, given cu needed for that session.
// The user can also request specific providers to not be included in the search for a session.
func (csm *ConsumerSessionManager) GetSessions(ctx context.Context, cuNeededForSession uint64, initUnwantedProviders map[string]struct{}, requestedBlock int64, addon string, extensions []*spectypes.Extension, stateful uint32, virtualEpoch uint64) (
	consumerSessionMap ConsumerSessionsMap, errRet error,
) {
	extensionNames := common.GetExtensionNames(extensions)
	// if pairing list is empty we reset the state.
	numberOfResets := csm.validatePairingListNotEmpty(addon, extensionNames)

	// verify initUnwantedProviders is not nil
	if initUnwantedProviders == nil {
		initUnwantedProviders = make(map[string]struct{})
	}

	// providers that we don't try to connect this iteration.
	tempIgnoredProviders := &ignoredProviders{
		providers:    initUnwantedProviders,
		currentEpoch: csm.atomicReadCurrentEpoch(),
	}

	// Get a valid consumerSessionsWithProvider
	sessionWithProviderMap, err := csm.getValidConsumerSessionsWithProvider(tempIgnoredProviders, cuNeededForSession, requestedBlock, addon, extensionNames, stateful, virtualEpoch)
	if err != nil {
		return nil, err
	}

	// Save how many sessions we are aiming to have
	wantedSession := len(sessionWithProviderMap)
	// Save sessions to return
	sessions := make(ConsumerSessionsMap, wantedSession)
	for {
		for providerAddress, sessionWithProvider := range sessionWithProviderMap {
			// Extract values from session with provider
			consumerSessionsWithProvider := sessionWithProvider.SessionsWithProvider
			sessionEpoch := sessionWithProvider.CurrentEpoch

			// Get a valid Endpoint from the provider chosen
			connected, endpoint, _, err := consumerSessionsWithProvider.fetchEndpointConnectionFromConsumerSessionWithProvider(ctx)
			if err != nil {
				// verify err is AllProviderEndpointsDisabled and report.
				if AllProviderEndpointsDisabledError.Is(err) {
					err = csm.blockProvider(providerAddress, true, sessionEpoch, MaxConsecutiveConnectionAttempts, 0, csm.GenerateReconnectCallback(consumerSessionsWithProvider)) // reporting and blocking provider this epoch
					if err != nil {
						if !EpochMismatchError.Is(err) {
							// only acceptable error is EpochMismatchError so if different, throw fatal
							utils.LavaFormatFatal("Unsupported Error", err)
						}
					}
					continue
				} else {
					utils.LavaFormatFatal("Unsupported Error", err)
				}
			} else if !connected {
				// If failed to connect we ignore this provider for this get session request only
				// and try again getting a random provider to pick from
				tempIgnoredProviders.providers[providerAddress] = struct{}{}
				continue
			}

			// we get the reported providers here after we try to connect, so if any provider didn't respond he will already be added to the list.
			reportedProviders := csm.GetReportedProviders(sessionEpoch)

			// Get session from endpoint or create new or continue. if more than 10 connections are open.
			consumerSession, pairingEpoch, err := consumerSessionsWithProvider.GetConsumerSessionInstanceFromEndpoint(endpoint, numberOfResets)
			if err != nil {
				utils.LavaFormatDebug("Error on consumerSessionWithProvider.getConsumerSessionInstanceFromEndpoint", utils.Attribute{Key: "Error", Value: err.Error()})
				if MaximumNumberOfSessionsExceededError.Is(err) {
					// we can get a different provider, adding this provider to the list of providers to skip on.
					tempIgnoredProviders.providers[providerAddress] = struct{}{}
				} else if MaximumNumberOfBlockListedSessionsError.Is(err) {
					// provider has too many block listed sessions. we block it until the next epoch.
					err = csm.blockProvider(providerAddress, false, sessionEpoch, 0, 0, nil)
					if err != nil {
						utils.LavaFormatError("Failed to block provider: ", err)
					}
				} else {
					utils.LavaFormatFatal("Unsupported Error", err)
				}

				continue
			}

			if pairingEpoch != sessionEpoch {
				// pairingEpoch and SessionEpoch must be the same, we validate them here if they are different we raise an error and continue with pairingEpoch
				utils.LavaFormatError("sessionEpoch and pairingEpoch mismatch", nil, utils.Attribute{Key: "sessionEpoch", Value: sessionEpoch}, utils.Attribute{Key: "pairingEpoch", Value: pairingEpoch})
				sessionEpoch = pairingEpoch
			}

			// If we successfully got a consumerSession we can apply the current CU to the consumerSessionWithProvider.UsedComputeUnits
			err = consumerSessionsWithProvider.addUsedComputeUnits(cuNeededForSession, virtualEpoch)
			if err != nil {
				utils.LavaFormatDebug("consumerSessionWithProvider.addUsedComputeUnit", utils.Attribute{Key: "Error", Value: err.Error()})
				if MaxComputeUnitsExceededError.Is(err) {
					tempIgnoredProviders.providers[providerAddress] = struct{}{}
					// We must unlock the consumer session before continuing.
					consumerSession.lock.Unlock()
					continue
				} else {
					utils.LavaFormatFatal("Unsupported Error", err)
				}
			} else {
				// consumer session is locked and valid, we need to set the relayNumber and the relay cu. before returning.
				consumerSession.LatestRelayCu = cuNeededForSession // set latestRelayCu
				consumerSession.RelayNum += RelayNumberIncrement   // increase relayNum
				// Successfully created/got a consumerSession.
				if debug {
					utils.LavaFormatDebug("Consumer get session",
						utils.Attribute{Key: "provider", Value: providerAddress},
						utils.Attribute{Key: "sessionEpoch", Value: sessionEpoch},
						utils.Attribute{Key: "consumerSession.CUSum", Value: consumerSession.CuSum},
						utils.Attribute{Key: "consumerSession.RelayNum", Value: consumerSession.RelayNum},
						utils.Attribute{Key: "consumerSession.SessionId", Value: consumerSession.SessionId},
					)
				}
				// If no error, add provider session map
				sessions[providerAddress] = &SessionInfo{
					Session:           consumerSession,
					Epoch:             sessionEpoch,
					ReportedProviders: reportedProviders,
				}

				if consumerSession.RelayNum > 1 {
					// we only set excellence for sessions with more than one successful relays, this guarantees data within the epoch exists
					consumerSession.QoSInfo.LastExcellenceQoSReport = csm.providerOptimizer.GetExcellenceQoSReportForProvider(providerAddress)
				}
				// We successfully added provider, we should ignore it if we need to fetch new
				tempIgnoredProviders.providers[providerAddress] = struct{}{}

				if len(sessions) == wantedSession {
					return sessions, nil
				}
				continue
			}
		}

		// If we do not have enough fetch more
		sessionWithProviderMap, err = csm.getValidConsumerSessionsWithProvider(tempIgnoredProviders, cuNeededForSession, requestedBlock, addon, extensionNames, stateful, virtualEpoch)

		// If error exists but we have sessions, return them
		if err != nil && len(sessions) != 0 {
			return sessions, nil
		}

		// If error happens, and we do not have any sessions return error
		if err != nil {
			return nil, err
		}
	}
}

// Get a valid provider address.
func (csm *ConsumerSessionManager) getValidProviderAddresses(ignoredProvidersList map[string]struct{}, cu uint64, requestedBlock int64, addon string, extensions []string, stateful uint32) (addresses []string, err error) {
	// cs.Lock must be Rlocked here.
	ignoredProvidersListLength := len(ignoredProvidersList)
	validAddresses := csm.getValidAddresses(addon, extensions)
	validAddressesLength := len(validAddresses)
	totalValidLength := validAddressesLength - ignoredProvidersListLength
	if totalValidLength <= 0 {
		// check all ignored are actually valid addresses
		ignoredProvidersListLength = 0
		for _, address := range validAddresses {
			if _, ok := ignoredProvidersList[address]; ok {
				ignoredProvidersListLength++
			}
		}
		if validAddressesLength-ignoredProvidersListLength <= 0 {
			utils.LavaFormatDebug("Pairing list empty", utils.Attribute{Key: "Provider list", Value: validAddresses}, utils.Attribute{Key: "IgnoredProviderList", Value: ignoredProvidersList}, utils.Attribute{Key: "addon", Value: addon}, utils.Attribute{Key: "extensions", Value: extensions})
			err = PairingListEmptyError
			return addresses, err
		}
	}
	var providers []string
	if stateful == common.CONSISTENCY_SELECT_ALLPROVIDERS && csm.providerOptimizer.Strategy() != provideroptimizer.STRATEGY_COST {
		providers = GetAllProviders(validAddresses, ignoredProvidersList)
	} else {
		providers = csm.providerOptimizer.ChooseProvider(validAddresses, ignoredProvidersList, cu, requestedBlock, OptimizerPerturbation)
	}
	if debug {
		utils.LavaFormatDebug("choosing providers",
			utils.Attribute{Key: "validAddresses", Value: validAddresses},
			utils.Attribute{Key: "ignoredProvidersList", Value: ignoredProvidersList},
			utils.Attribute{Key: "chosenProviders", Value: providers},
			utils.Attribute{Key: "addon", Value: addon},
			utils.Attribute{Key: "extensions", Value: extensions},
			utils.Attribute{Key: "stateful", Value: stateful},
		)
	}

	// make sure we have at least 1 valid provider
	if len(providers) == 0 || providers[0] == "" {
		utils.LavaFormatDebug("No providers returned by the optimizer", utils.Attribute{Key: "Provider list", Value: validAddresses}, utils.Attribute{Key: "IgnoredProviderList", Value: ignoredProvidersList})
		err = PairingListEmptyError
		return addresses, err
	}

	return providers, nil
}

func (csm *ConsumerSessionManager) getValidConsumerSessionsWithProvider(ignoredProviders *ignoredProviders, cuNeededForSession uint64, requestedBlock int64, addon string, extensions []string, stateful uint32, virtualEpoch uint64) (sessionWithProviderMap SessionWithProviderMap, err error) {
	csm.lock.RLock()
	defer csm.lock.RUnlock()
	if debug {
		utils.LavaFormatDebug("called getValidConsumerSessionsWithProvider", utils.Attribute{Key: "ignoredProviders", Value: ignoredProviders})
	}
	currentEpoch := csm.atomicReadCurrentEpoch() // reading the epoch here while locked, to get the epoch of the pairing.
	if ignoredProviders.currentEpoch < currentEpoch {
		utils.LavaFormatDebug("ignoredProviders epoch is not the current epoch, resetting ignoredProviders", utils.Attribute{Key: "ignoredProvidersEpoch", Value: ignoredProviders.currentEpoch}, utils.Attribute{Key: "currentEpoch", Value: currentEpoch})
		ignoredProviders.providers = make(map[string]struct{}) // reset the old providers as epochs changed so we have a new pairing list.
		ignoredProviders.currentEpoch = currentEpoch
	}

	// Fetch provider addresses
	providerAddresses, err := csm.getValidProviderAddresses(ignoredProviders.providers, cuNeededForSession, requestedBlock, addon, extensions, stateful)
	if err != nil {
		utils.LavaFormatError("could not get a provider addresses", err)
		return nil, err
	}

	// save how many providers we are aiming to return
	wantedProviderNumber := len(providerAddresses)

	// Create map to save sessions with providers
	sessionWithProviderMap = make(SessionWithProviderMap, wantedProviderNumber)

	// Iterate till we fill map or do not have more
	for {
		// Iterate over providers
		for _, providerAddress := range providerAddresses {
			consumerSessionsWithProvider := csm.pairing[providerAddress]
			if consumerSessionsWithProvider == nil {
				utils.LavaFormatFatal("invalid provider address returned from csm.getValidProviderAddresses", nil,
					utils.Attribute{Key: "providerAddress", Value: providerAddress},
					utils.Attribute{Key: "all_providerAddresses", Value: providerAddresses},
					utils.Attribute{Key: "pairing", Value: csm.pairing},
					utils.Attribute{Key: "epochAtStart", Value: currentEpoch},
					utils.Attribute{Key: "currentEpoch", Value: csm.atomicReadCurrentEpoch()},
					utils.Attribute{Key: "validAddresses", Value: csm.getValidAddresses(addon, extensions)},
					utils.Attribute{Key: "wantedProviderNumber", Value: wantedProviderNumber},
				)
			}
			if err := consumerSessionsWithProvider.validateComputeUnits(cuNeededForSession, virtualEpoch); err != nil {
				// Add to ignored
				ignoredProviders.providers[providerAddress] = struct{}{}
				continue
			}

			// If no error, add provider session map
			sessionWithProviderMap[providerAddress] = &SessionWithProvider{
				SessionsWithProvider: consumerSessionsWithProvider,
				CurrentEpoch:         currentEpoch,
			}
			// Add to ignored
			ignoredProviders.providers[providerAddress] = struct{}{}

			// If we have enough providers return
			if len(sessionWithProviderMap) == wantedProviderNumber {
				return sessionWithProviderMap, nil
			}
		}

		// If we do not have enough fetch more
		providerAddresses, err = csm.getValidProviderAddresses(ignoredProviders.providers, cuNeededForSession, requestedBlock, addon, extensions, stateful)

		// If error exists but we have providers, return them
		if err != nil && len(sessionWithProviderMap) != 0 {
			return sessionWithProviderMap, nil
		}

		// If error happens, and we do not have any provider return error
		if err != nil {
			utils.LavaFormatError("could not get a provider addresses", err)
			return nil, err
		}
	}
}

// removes a given address from the valid addresses list.
func (csm *ConsumerSessionManager) removeAddressFromValidAddresses(address string) error {
	// cs Must be Locked here.
	for idx, addr := range csm.validAddresses {
		if addr == address {
			// remove the index from the valid list.
			csm.validAddresses = append(csm.validAddresses[:idx], csm.validAddresses[idx+1:]...)
			csm.RemoveAddonAddresses("", nil)
			return nil
		}
	}
	return AddressIndexWasNotFoundError
}

// Blocks a provider making him unavailable for pick this epoch, will also report him as unavailable if reportProvider is set to true.
// Validates that the sessionEpoch is equal to cs.currentEpoch otherwise doesn't take effect.
func (csm *ConsumerSessionManager) blockProvider(address string, reportProvider bool, sessionEpoch uint64, disconnections uint64, errors uint64, reconnectCallback func() error) error {
	// find Index of the address
	if sessionEpoch != csm.atomicReadCurrentEpoch() { // we read here atomically so cs.currentEpoch cant change in the middle, so we can save time if epochs mismatch
		return EpochMismatchError
	}

	csm.lock.Lock() // we lock RW here because we need to make sure nothing changes while we verify validAddresses/addedToPurgeAndReport
	defer csm.lock.Unlock()
	if sessionEpoch != csm.atomicReadCurrentEpoch() { // After we lock we need to verify again that the epoch didn't change while we waited for the lock.
		return EpochMismatchError
	}

	err := csm.removeAddressFromValidAddresses(address)
	if err != nil {
		if AddressIndexWasNotFoundError.Is(err) {
			// in case index wasnt found just continue with the method
			utils.LavaFormatError("address was not found in valid addresses", err, utils.Attribute{Key: "address", Value: address}, utils.Attribute{Key: "validAddresses", Value: csm.validAddresses})
		} else {
			return err
		}
	}

	if reportProvider { // Report provider flow
		csm.reportedProviders.ReportProvider(address, errors, disconnections, reconnectCallback)
	}

	return nil
}

// Verify the consumerSession is locked when getting to this function, if its not locked throw an error
func (csm *ConsumerSessionManager) verifyLock(consumerSession *SingleConsumerSession) error {
	if consumerSession.lock.TryLock() { // verify.
		// if we managed to lock throw an error for misuse.
		defer consumerSession.lock.Unlock()
		// if failed to lock we should block session as it seems like a very rare case.
		consumerSession.BlockListed = true // block this session from future usages
		utils.LavaFormatError("Verify Lock failed on session Failure, blocking session", nil, utils.LogAttr("consumerSession", consumerSession))
		return LockMisUseDetectedError
	}
	return nil
}

// A Session can be created but unused if consumer found the response in the cache.
// So we need to unlock the session and decrease the cu that were applied
func (csm *ConsumerSessionManager) OnSessionUnUsed(consumerSession *SingleConsumerSession) error {
	if err := csm.verifyLock(consumerSession); err != nil {
		return sdkerrors.Wrapf(err, "OnSessionUnUsed, consumerSession.lock must be locked before accessing this method, additional info:")
	}
	cuToDecrease := consumerSession.LatestRelayCu
	consumerSession.LatestRelayCu = 0                            // making sure no one uses it in a wrong way
	parentConsumerSessionsWithProvider := consumerSession.Parent // must read this pointer before unlocking
	// finished with consumerSession here can unlock.
	consumerSession.lock.Unlock()                                                    // we unlock before we change anything in the parent ConsumerSessionsWithProvider
	err := parentConsumerSessionsWithProvider.decreaseUsedComputeUnits(cuToDecrease) // change the cu in parent
	if err != nil {
		return err
	}
	return nil
}

// Report session failure, mark it as blocked from future usages, report if timeout happened.
func (csm *ConsumerSessionManager) OnSessionFailure(consumerSession *SingleConsumerSession, errorReceived error) error {
	// consumerSession must be locked when getting here.
	if err := csm.verifyLock(consumerSession); err != nil {
		return sdkerrors.Wrapf(err, "OnSessionFailure, consumerSession.lock must be locked before accessing this method, additional info:")
	}

	// consumer Session should be locked here. so we can just apply the session failure here.
	if consumerSession.BlockListed {
		// if consumer session is already blocklisted return an error.
		return sdkerrors.Wrapf(SessionIsAlreadyBlockListedError, "trying to report a session failure of a blocklisted consumer session")
	}

	consumerSession.QoSInfo.TotalRelays++
	consumerSession.ConsecutiveNumberOfFailures += 1 // increase number of failures for this session
	consumerSession.errosCount += 1
	// if this session failed more than MaximumNumberOfFailuresAllowedPerConsumerSession times or session went out of sync we block it.
	var consumerSessionBlockListed bool
	if consumerSession.ConsecutiveNumberOfFailures > MaximumNumberOfFailuresAllowedPerConsumerSession || IsSessionSyncLoss(errorReceived) {
		utils.LavaFormatDebug("Blocking consumer session", utils.LogAttr("ConsecutiveNumberOfFailures", consumerSession.ConsecutiveNumberOfFailures), utils.LogAttr("errosCount", consumerSession.errosCount), utils.Attribute{Key: "id", Value: consumerSession.SessionId})
		consumerSession.BlockListed = true // block this session from future usages
		consumerSessionBlockListed = true
	}
	cuToDecrease := consumerSession.LatestRelayCu
	// latency, isHangingApi, syncScore arent updated when there is a failure
	go csm.providerOptimizer.AppendRelayFailure(consumerSession.Parent.PublicLavaAddress)
	consumerSession.LatestRelayCu = 0 // making sure no one uses it in a wrong way

	parentConsumerSessionsWithProvider := consumerSession.Parent // must read this pointer before unlocking
	reportErrors := consumerSession.ConsecutiveNumberOfFailures
	// finished with consumerSession here can unlock.
	csm.updateMetricsManager(consumerSession)
	consumerSession.lock.Unlock() // we unlock before we change anything in the parent ConsumerSessionsWithProvider

	err := parentConsumerSessionsWithProvider.decreaseUsedComputeUnits(cuToDecrease) // change the cu in parent
	if err != nil {
		return err
	}

	// check if need to block & report
	var blockProvider, reportProvider bool
	if ReportAndBlockProviderError.Is(errorReceived) {
		blockProvider = true
		reportProvider = true
	} else if BlockProviderError.Is(errorReceived) {
		blockProvider = true
	}

	// if BlockListed is true here meaning we had a ConsecutiveNumberOfFailures > MaximumNumberOfFailuresAllowedPerConsumerSession or out of sync
	// we will check the total number of cu for this provider and decide if we need to report it.
	if consumerSessionBlockListed {
		if parentConsumerSessionsWithProvider.atomicReadUsedComputeUnits() == 0 { // if we had 0 successful relays and we reached block session we need to report this provider
			blockProvider = true
			reportProvider = true
		}
	}

	if blockProvider {
		publicProviderAddress, pairingEpoch := parentConsumerSessionsWithProvider.getPublicLavaAddressAndPairingEpoch()
		err = csm.blockProvider(publicProviderAddress, reportProvider, pairingEpoch, 0, reportErrors, nil)
		if err != nil {
			if EpochMismatchError.Is(err) {
				return nil // no effects this epoch has been changed
			}
			return err
		}
	}
	return nil
}

// On a successful DataReliability session we don't need to increase and update any field, we just need to unlock the session.
func (csm *ConsumerSessionManager) OnDataReliabilitySessionDone(consumerSession *SingleConsumerSession,
	latestServicedBlock int64,
	specComputeUnits uint64,
	currentLatency time.Duration,
	expectedLatency time.Duration,
	expectedBH int64,
	numOfProviders int,
	providersCount uint64,
) error {
	if err := csm.verifyLock(consumerSession); err != nil {
		return sdkerrors.Wrapf(err, "OnDataReliabilitySessionDone, consumerSession.lock must be locked before accessing this method")
	}

	defer consumerSession.lock.Unlock()               // we need to be locked here, if we didn't get it locked we try lock anyway
	consumerSession.ConsecutiveNumberOfFailures = 0   // reset failures.
	consumerSession.LatestBlock = latestServicedBlock // update latest serviced block
	if expectedBH-latestServicedBlock > 1000 {
		utils.LavaFormatWarning("identified block gap", nil,
			utils.Attribute{Key: "expectedBH", Value: expectedBH},
			utils.Attribute{Key: "latestServicedBlock", Value: latestServicedBlock},
			utils.Attribute{Key: "session_id", Value: consumerSession.SessionId},
			utils.Attribute{Key: "provider_address", Value: consumerSession.Parent.PublicLavaAddress},
		)
	}
	consumerSession.CalculateQoS(currentLatency, expectedLatency, expectedBH-latestServicedBlock, numOfProviders, int64(providersCount))
	return nil
}

// On a successful session this function will update all necessary fields in the consumerSession. and unlock it when it finishes
func (csm *ConsumerSessionManager) OnSessionDone(
	consumerSession *SingleConsumerSession,
	latestServicedBlock int64,
	specComputeUnits uint64,
	currentLatency time.Duration,
	expectedLatency time.Duration,
	expectedBH int64,
	numOfProviders int,
	providersCount uint64,
	isHangingApi bool,
) error {
	// release locks, update CU, relaynum etc..
	if err := csm.verifyLock(consumerSession); err != nil {
		return sdkerrors.Wrapf(err, "OnSessionDone, consumerSession.lock must be locked before accessing this method")
	}

	defer consumerSession.lock.Unlock()                    // we need to be locked here, if we didn't get it locked we try lock anyway
	consumerSession.CuSum += consumerSession.LatestRelayCu // add CuSum to current cu usage.
	consumerSession.LatestRelayCu = 0                      // reset cu just in case
	consumerSession.ConsecutiveNumberOfFailures = 0        // reset failures.
	consumerSession.LatestBlock = latestServicedBlock      // update latest serviced block
	// calculate QoS
	consumerSession.CalculateQoS(currentLatency, expectedLatency, expectedBH-latestServicedBlock, numOfProviders, int64(providersCount))
	go csm.providerOptimizer.AppendRelayData(consumerSession.Parent.PublicLavaAddress, currentLatency, isHangingApi, specComputeUnits, uint64(latestServicedBlock))
	csm.updateMetricsManager(consumerSession)
	return nil
}

// func ()

// consumerSession should still be locked when accessing this method as it fetches information from the session it self
func (csm *ConsumerSessionManager) updateMetricsManager(consumerSession *SingleConsumerSession) {
	if csm.consumerMetricsManager == nil {
		return
	}
	info := csm.RPCEndpoint()
	apiInterface := info.ApiInterface
	chainId := info.ChainID
	var lastQos *pairingtypes.QualityOfServiceReport
	var lastQosExcellence *pairingtypes.QualityOfServiceReport
	if consumerSession.QoSInfo.LastQoSReport != nil {
		qos := *consumerSession.QoSInfo.LastQoSReport
		lastQos = &qos
	}
	if consumerSession.QoSInfo.LastExcellenceQoSReport != nil {
		qosEx := *consumerSession.QoSInfo.LastExcellenceQoSReport
		lastQosExcellence = &qosEx
	}

	go csm.consumerMetricsManager.SetQOSMetrics(chainId, apiInterface, consumerSession.Parent.PublicLavaAddress, lastQos, lastQosExcellence, consumerSession.LatestBlock, consumerSession.RelayNum)
}

// consumerSession should still be locked when accessing this method as it fetches information from the session it self
func (csm *ConsumerSessionManager) resetMetricsManager() {
	if csm.consumerMetricsManager == nil {
		return
	}
	csm.consumerMetricsManager.ResetQOSMetrics()
}

// Get the reported providers currently stored in the session manager.
func (csm *ConsumerSessionManager) GetReportedProviders(epoch uint64) []*pairingtypes.ReportedProvider {
	if epoch != csm.atomicReadCurrentEpoch() {
		return nil // if epochs are not equal, we will return an empty list.
	}
	return csm.reportedProviders.GetReportedProviders()
}

// Data Reliability Section:

// Atomically read csm.pairingAddressesLength for data reliability.
func (csm *ConsumerSessionManager) GetAtomicPairingAddressesLength() uint64 {
	return atomic.LoadUint64(&csm.pairingAddressesLength)
}

func (csm *ConsumerSessionManager) getDataReliabilityProviderIndex(unAllowedAddress string, index uint64) (cswp *ConsumerSessionsWithProvider, providerAddress string, epoch uint64, err error) {
	csm.lock.RLock()
	defer csm.lock.RUnlock()
	currentEpoch := csm.atomicReadCurrentEpoch()
	pairingAddressesLength := csm.GetAtomicPairingAddressesLength()
	if index >= pairingAddressesLength {
		utils.LavaFormatInfo(DataReliabilityIndexOutOfRangeError.Error(), utils.Attribute{Key: "index", Value: index}, utils.Attribute{Key: "pairingAddressesLength", Value: pairingAddressesLength})
		return nil, "", currentEpoch, DataReliabilityIndexOutOfRangeError
	}
	providerAddress = csm.pairingAddresses[index]
	if providerAddress == unAllowedAddress {
		return nil, "", currentEpoch, DataReliabilityIndexRequestedIsOriginalProviderError
	}
	// if address is valid return the ConsumerSessionsWithProvider
	return csm.pairing[providerAddress], providerAddress, currentEpoch, nil
}

func (csm *ConsumerSessionManager) fetchEndpointFromConsumerSessionsWithProviderWithRetry(ctx context.Context, consumerSessionsWithProvider *ConsumerSessionsWithProvider, sessionEpoch uint64) (endpoint *Endpoint, err error) {
	var connected bool
	var providerAddress string
	for idx := 0; idx < MaxConsecutiveConnectionAttempts; idx++ { // try to connect to the endpoint 3 times
		connected, endpoint, providerAddress, err = consumerSessionsWithProvider.fetchEndpointConnectionFromConsumerSessionWithProvider(ctx)
		if err != nil {
			// verify err is AllProviderEndpointsDisabled and report.
			if AllProviderEndpointsDisabledError.Is(err) {
				err = csm.blockProvider(providerAddress, true, sessionEpoch, MaxConsecutiveConnectionAttempts, 0, csm.GenerateReconnectCallback(consumerSessionsWithProvider)) // reporting and blocking provider this epoch
				if err != nil {
					if !EpochMismatchError.Is(err) {
						// only acceptable error is EpochMismatchError so if different, throw fatal
						utils.LavaFormatFatal("Unsupported Error", err)
					}
				}
				break // all endpoints are disabled, no reason to continue with this provider.
			} else {
				utils.LavaFormatFatal("Unsupported Error", err)
			}
		}
		if connected {
			// if we are connected we can stop trying and return the endpoint
			break
		} else {
			continue
		}
	}
	if !connected { // if we are not connected at the end
		// failed to get an endpoint connection from that provider. return an error.
		return nil, utils.LavaFormatError("Not Connected", FailedToConnectToEndPointForDataReliabilityError, utils.Attribute{Key: "provider", Value: providerAddress})
	}
	return endpoint, nil
}

// Get a Data Reliability Session
func (csm *ConsumerSessionManager) GetDataReliabilitySession(ctx context.Context, originalProviderAddress string, index int64, sessionEpoch uint64) (singleConsumerSession *SingleConsumerSession, providerAddress string, epoch uint64, err error) {
	consumerSessionWithProvider, providerAddress, currentEpoch, err := csm.getDataReliabilityProviderIndex(originalProviderAddress, uint64(index))
	if err != nil {
		return nil, "", 0, err
	}
	if sessionEpoch != currentEpoch { // validate we are in the same epoch.
		return nil, "", currentEpoch, DataReliabilityEpochMismatchError
	}

	// after choosing a provider, try to see if it already has an existing data reliability session.
	consumerSession, pairingEpoch, err := consumerSessionWithProvider.verifyDataReliabilitySessionWasNotAlreadyCreated()
	if NoDataReliabilitySessionWasCreatedError.Is(err) { // need to create a new data reliability session
		// We can get an endpoint now and create a data reliability session.
		endpoint, err := csm.fetchEndpointFromConsumerSessionsWithProviderWithRetry(ctx, consumerSessionWithProvider, currentEpoch)
		if err != nil {
			return nil, "", currentEpoch, err
		}

		// get data reliability session from endpoint
		consumerSession, pairingEpoch, err = consumerSessionWithProvider.getDataReliabilitySingleConsumerSession(endpoint)
		if err != nil {
			return nil, "", currentEpoch, err
		}
	} else if err != nil {
		return nil, "", currentEpoch, err
	}

	if currentEpoch != pairingEpoch { // validate they are the same, if not print an error and set currentEpoch to pairingEpoch.
		utils.LavaFormatError("currentEpoch and pairingEpoch mismatch", nil, utils.Attribute{Key: "sessionEpoch", Value: currentEpoch}, utils.Attribute{Key: "pairingEpoch", Value: pairingEpoch})
		currentEpoch = pairingEpoch
	}

	// DR consumer session is locked, we can increment data reliability relay number.
	consumerSession.RelayNum += 1

	return consumerSession, providerAddress, currentEpoch, nil
}

// On a successful Subscribe relay
func (csm *ConsumerSessionManager) OnSessionDoneIncreaseCUOnly(consumerSession *SingleConsumerSession) error {
	if err := csm.verifyLock(consumerSession); err != nil {
		return sdkerrors.Wrapf(err, "OnSessionDoneIncreaseRelayAndCu consumerSession.lock must be locked before accessing this method")
	}

	defer consumerSession.lock.Unlock()                    // we need to be locked here, if we didn't get it locked we try lock anyway
	consumerSession.CuSum += consumerSession.LatestRelayCu // add CuSum to current cu usage.
	consumerSession.LatestRelayCu = 0                      // reset cu just in case
	consumerSession.ConsecutiveNumberOfFailures = 0        // reset failures.
	return nil
}

// On a failed DataReliability session we don't decrease the cu unlike a normal session, we just unlock and verify if we need to block this session or provider.
func (csm *ConsumerSessionManager) OnDataReliabilitySessionFailure(consumerSession *SingleConsumerSession, errorReceived error) error {
	// consumerSession must be locked when getting here.
	if err := csm.verifyLock(consumerSession); err != nil {
		return sdkerrors.Wrapf(err, "OnDataReliabilitySessionFailure consumerSession.lock must be locked before accessing this method")
	}
	// consumer Session should be locked here. so we can just apply the session failure here.
	if consumerSession.BlockListed {
		// if consumer session is already blocklisted return an error.
		return sdkerrors.Wrapf(SessionIsAlreadyBlockListedError, "trying to report a session failure of a blocklisted client session")
	}
	consumerSession.QoSInfo.TotalRelays++
	consumerSession.ConsecutiveNumberOfFailures += 1 // increase number of failures for this session
	consumerSession.RelayNum -= 1                    // upon data reliability failure, decrease the relay number so we can try again.

	// if this session failed more than MaximumNumberOfFailuresAllowedPerConsumerSession times we block list it.
	if consumerSession.ConsecutiveNumberOfFailures > MaximumNumberOfFailuresAllowedPerConsumerSession {
		consumerSession.BlockListed = true // block this session from future usages
	} else if SessionOutOfSyncError.Is(errorReceived) { // this is an error that we must block the session due to.
		consumerSession.BlockListed = true
	}

	var blockProvider, reportProvider bool
	if ReportAndBlockProviderError.Is(errorReceived) {
		blockProvider = true
		reportProvider = true
	} else if BlockProviderError.Is(errorReceived) {
		blockProvider = true
	}

	parentConsumerSessionsWithProvider := consumerSession.Parent
	consumerSession.lock.Unlock()

	if blockProvider {
		publicProviderAddress, pairingEpoch := parentConsumerSessionsWithProvider.getPublicLavaAddressAndPairingEpoch()
		err := csm.blockProvider(publicProviderAddress, reportProvider, pairingEpoch, 0, 0, nil)
		if err != nil {
			if EpochMismatchError.Is(err) {
				return nil // no effects this epoch has been changed
			}
			return err
		}
	}

	return nil
}

func (csm *ConsumerSessionManager) GenerateReconnectCallback(consumerSessionsWithProvider *ConsumerSessionsWithProvider) func() error {
	return func() error {
		_, _, err := csm.probeProvider(context.Background(), consumerSessionsWithProvider, csm.atomicReadCurrentEpoch())
		return err
	}
}

func NewConsumerSessionManager(rpcEndpoint *RPCEndpoint, providerOptimizer ProviderOptimizer, consumerMetricsManager *metrics.ConsumerMetricsManager) *ConsumerSessionManager {
	csm := &ConsumerSessionManager{
		reportedProviders:      *NewReportedProviders(),
		consumerMetricsManager: consumerMetricsManager,
	}
	csm.rpcEndpoint = rpcEndpoint
	csm.providerOptimizer = providerOptimizer
	return csm
}
