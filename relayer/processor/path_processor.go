package processor

import (
	"context"
	"fmt"
	"time"

	"github.com/cosmos/relayer/v2/relayer/provider"
	"go.uber.org/zap"
)

const (
	// durationErrorRetry determines how long to wait before retrying
	// in the case of failure to send transactions with IBC messages.
	durationErrorRetry         = 5 * time.Second
	blocksToRetryAssemblyAfter = 1
	blocksToRetrySendAfter     = 2
	maxMessageSendRetries      = 10

	ibcHeadersToCache = 10
)

// PathProcessor is a process that handles incoming IBC messages from a pair of chains.
// It determines what messages need to be relayed, and sends them.
type PathProcessor struct {
	log *zap.Logger

	pathEnd1 *pathEndRuntime
	pathEnd2 *pathEndRuntime

	// Signals to retry.
	retryProcess chan struct{}

	sentInitialMsg bool
}

// PathProcessors is a slice of PathProcessor instances
type PathProcessors []*PathProcessor

func (p PathProcessors) IsRelayedChannel(k ChannelKey, chainID string) bool {
	for _, pp := range p {
		if pp.IsRelayedChannel(chainID, k) {
			return true
		}
	}
	return false
}

func NewPathProcessor(log *zap.Logger, pathEnd1 PathEnd, pathEnd2 PathEnd) *PathProcessor {
	return &PathProcessor{
		log:          log,
		pathEnd1:     newPathEndRuntime(log, pathEnd1),
		pathEnd2:     newPathEndRuntime(log, pathEnd2),
		retryProcess: make(chan struct{}, 8),
	}
}

// TEST USE ONLY
func (pp *PathProcessor) PathEnd1Messages(channelKey ChannelKey, message string) PacketSequenceCache {
	return pp.pathEnd1.messageCache.PacketFlow[channelKey][message]
}

// TEST USE ONLY
func (pp *PathProcessor) PathEnd2Messages(channelKey ChannelKey, message string) PacketSequenceCache {
	return pp.pathEnd2.messageCache.PacketFlow[channelKey][message]
}

type channelPair struct {
	pathEnd1ChannelKey ChannelKey
	pathEnd2ChannelKey ChannelKey
}

// RelevantClientID returns the relevant client ID or panics
func (pp *PathProcessor) RelevantClientID(chainID string) string {
	if pp.pathEnd1.info.ChainID == chainID {
		return pp.pathEnd1.info.ClientID
	}
	if pp.pathEnd2.info.ChainID == chainID {
		return pp.pathEnd2.info.ClientID
	}
	panic(fmt.Errorf("no relevant client ID for chain ID: %s", chainID))
}

// OnConnectionMessage allows the caller to handle connection handshake messages with a callback.
func (pp *PathProcessor) OnConnectionMessage(chainID string, action string, onMsg func(provider.ConnectionInfo)) {
	if pp.pathEnd1.info.ChainID == chainID {
		pp.pathEnd1.connectionMessageSubscribers[action] = append(pp.pathEnd1.connectionMessageSubscribers[action], onMsg)
	} else if pp.pathEnd2.info.ChainID == chainID {
		pp.pathEnd2.connectionMessageSubscribers[action] = append(pp.pathEnd2.connectionMessageSubscribers[action], onMsg)
	}
}

// OnChannelMessage allows the caller to handle channel handshake messages with a callback.
func (pp *PathProcessor) OnChannelMessage(chainID string, action string, onMsg func(provider.ChannelInfo)) {
	if pp.pathEnd1.info.ChainID == chainID {
		pp.pathEnd1.channelMessageSubscribers[action] = append(pp.pathEnd1.channelMessageSubscribers[action], onMsg)
	} else if pp.pathEnd2.info.ChainID == chainID {
		pp.pathEnd2.channelMessageSubscribers[action] = append(pp.pathEnd2.channelMessageSubscribers[action], onMsg)
	}
}

// OnPacketMessage allows the caller to handle packet flow messages with a callback.
func (pp *PathProcessor) OnPacketMessage(chainID string, action string, onMsg func(provider.PacketInfo)) {
	if pp.pathEnd1.info.ChainID == chainID {
		pp.pathEnd1.packetMessageSubscribers[action] = append(pp.pathEnd1.packetMessageSubscribers[action], onMsg)
	} else if pp.pathEnd2.info.ChainID == chainID {
		pp.pathEnd2.packetMessageSubscribers[action] = append(pp.pathEnd2.packetMessageSubscribers[action], onMsg)
	}
}

func (pp *PathProcessor) channelPairs() []channelPair {
	// Channel keys are from pathEnd1's perspective
	channels := make(map[ChannelKey]bool)
	for k, open := range pp.pathEnd1.channelStateCache {
		channels[k] = open
	}
	for k, open := range pp.pathEnd2.channelStateCache {
		channels[k.Counterparty()] = open
	}
	pairs := make([]channelPair, len(channels))
	i := 0
	for k, open := range channels {
		if !open {
			continue
		}
		pairs[i] = channelPair{
			pathEnd1ChannelKey: k,
			pathEnd2ChannelKey: k.Counterparty(),
		}
		i++
	}
	return pairs
}

// Path Processors are constructed before ChainProcessors, so reference needs to be added afterwards
// This can be done inside the ChainProcessor constructor for simplification
func (pp *PathProcessor) SetChainProviderIfApplicable(chainProvider provider.ChainProvider) bool {
	if chainProvider == nil {
		return false
	}
	if pp.pathEnd1.info.ChainID == chainProvider.ChainId() {
		pp.pathEnd1.chainProvider = chainProvider
		return true
	} else if pp.pathEnd2.info.ChainID == chainProvider.ChainId() {
		pp.pathEnd2.chainProvider = chainProvider
		return true
	}
	return false
}

func (pp *PathProcessor) IsRelayedChannel(chainID string, channelKey ChannelKey) bool {
	if pp.pathEnd1.info.ChainID == chainID {
		return pp.pathEnd1.info.ShouldRelayChannel(channelKey)
	} else if pp.pathEnd2.info.ChainID == chainID {
		return pp.pathEnd2.info.ShouldRelayChannel(channelKey)
	}
	return false
}

func (pp *PathProcessor) IsRelevantClient(chainID string, clientID string) bool {
	if pp.pathEnd1.info.ChainID == chainID {
		return pp.pathEnd1.info.ClientID == clientID
	} else if pp.pathEnd2.info.ChainID == chainID {
		return pp.pathEnd2.info.ClientID == clientID
	}
	return false
}

func (pp *PathProcessor) IsRelevantConnection(chainID string, connectionID string) bool {
	if pp.pathEnd1.info.ChainID == chainID {
		return pp.pathEnd1.isRelevantConnection(connectionID)
	} else if pp.pathEnd2.info.ChainID == chainID {
		return pp.pathEnd2.isRelevantConnection(connectionID)
	}
	return false
}

// ProcessBacklogIfReady gives ChainProcessors a way to trigger the path processor process
// as soon as they are in sync for the first time, even if they do not have new messages.
func (pp *PathProcessor) ProcessBacklogIfReady() {
	select {
	case pp.retryProcess <- struct{}{}:
		// All good.
	default:
		// Log that the channel is saturated;
		// something is wrong if we are retrying this quickly.
		pp.log.Info("Failed to enqueue path processor retry")
	}
}

// ChainProcessors call this method when they have new IBC messages
func (pp *PathProcessor) HandleNewData(chainID string, cacheData ChainProcessorCacheData) {
	if pp.pathEnd1.info.ChainID == chainID {
		pp.pathEnd1.incomingCacheData <- cacheData
	} else if pp.pathEnd2.info.ChainID == chainID {
		pp.pathEnd2.incomingCacheData <- cacheData
	}
}

// Run executes the main path process.
func (pp *PathProcessor) Run(ctx context.Context, ctxCancel func(), messageLifecycle MessageLifecycle) {
	var retryTimer *time.Timer
	for {
		// block until we have any signals to process
		select {
		case <-ctx.Done():
			pp.log.Debug("Context done, quitting PathProcessor",
				zap.String("chain_id_1", pp.pathEnd1.info.ChainID),
				zap.String("chain_id_2", pp.pathEnd2.info.ChainID),
				zap.String("client_id_1", pp.pathEnd1.info.ClientID),
				zap.String("client_id_2", pp.pathEnd2.info.ClientID),
				zap.Error(ctx.Err()),
			)
			return
		case d := <-pp.pathEnd1.incomingCacheData:
			// we have new data from ChainProcessor for pathEnd1
			pp.pathEnd1.MergeCacheData(ctx, ctxCancel, d, messageLifecycle)

		case d := <-pp.pathEnd2.incomingCacheData:
			// we have new data from ChainProcessor for pathEnd2
			pp.pathEnd2.MergeCacheData(ctx, ctxCancel, d, messageLifecycle)

		case <-pp.retryProcess:
			// No new data to merge in, just retry handling.
		}

		// Fully flush pathEnd incoming data before processing
		for len(pp.pathEnd1.incomingCacheData) > 0 {
			pp.pathEnd1.MergeCacheData(ctx, ctxCancel, <-pp.pathEnd1.incomingCacheData, messageLifecycle)
		}
		for len(pp.pathEnd2.incomingCacheData) > 0 {
			pp.pathEnd2.MergeCacheData(ctx, ctxCancel, <-pp.pathEnd2.incomingCacheData, messageLifecycle)
		}

		// flush retry process in case retries were scheduled
		for len(pp.retryProcess) > 0 {
			<-pp.retryProcess
		}

		// check context error here in case MergeCacheData found termination condition,
		// don't need to proceed to process messages if so.
		if ctx.Err() != nil {
			pp.log.Debug("Context cancelled, quitting PathProcessor",
				zap.String("chain_id_1", pp.pathEnd1.info.ChainID),
				zap.String("chain_id_2", pp.pathEnd2.info.ChainID),
				zap.String("client_id_1", pp.pathEnd1.info.ClientID),
				zap.String("client_id_2", pp.pathEnd2.info.ClientID),
				zap.Error(ctx.Err()),
			)
			return
		}

		if !pp.pathEnd1.inSync || !pp.pathEnd2.inSync {
			continue
		}

		// process latest message cache state from both pathEnds
		if err := pp.processLatestMessages(ctx, messageLifecycle); err != nil {
			// in case of IBC message send errors, schedule retry after durationErrorRetry
			if retryTimer != nil {
				retryTimer.Stop()
			}
			if ctx.Err() == nil {
				retryTimer = time.AfterFunc(durationErrorRetry, pp.ProcessBacklogIfReady)
			}
		}
	}
}