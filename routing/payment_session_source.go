package routing

import (
	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/zpay32"
)

// A compile time assertion to ensure MissionControl meets the
// PaymentSessionSource interface.
var _ PaymentSessionSource = (*SessionSource)(nil)

// SessionSource defines a source for the router to retrieve new payment
// sessions.
type SessionSource struct {
	// Graph is the channel graph that will be used to gather metrics from
	// and also to carry out path finding queries.
	Graph *channeldb.ChannelGraph

	// QueryBandwidth is a method that allows querying the lower link layer
	// to determine the up to date available bandwidth at a prospective link
	// to be traversed. If the link isn't available, then a value of zero
	// should be returned. Otherwise, the current up to date knowledge of
	// the available bandwidth of the link should be returned.
	QueryBandwidth func(*channeldb.ChannelEdgeInfo) lnwire.MilliSatoshi

	// SelfNode is our own node.
	SelfNode *channeldb.LightningNode

	// MissionControl is a shared memory of sorts that executions of payment
	// path finding use in order to remember which vertexes/edges were
	// pruned from prior attempts. During payment execution, errors sent by
	// nodes are mapped into a vertex or edge to be pruned. Each run will
	// then take into account this set of pruned vertexes/edges to reduce
	// route failure and pass on graph information gained to the next
	// execution.
	MissionControl MissionController

	// PaymentAttemptPenalty is the virtual cost in path finding weight
	// units of executing a payment attempt that fails. It is used to trade
	// off potentially better routes against their probability of
	// succeeding.
	PaymentAttemptPenalty lnwire.MilliSatoshi

	// MinProbability defines the minimum success probability of the
	// returned route.
	MinRouteProbability float64
}

// NewPaymentSession creates a new payment session backed by the latest prune
// view from Mission Control. An optional set of routing hints can be provided
// in order to populate additional edges to explore when finding a path to the
// payment's destination.
func (m *SessionSource) NewPaymentSession(routeHints [][]zpay32.HopHint,
	target route.Vertex) (PaymentSession, error) {

	edges := make(map[route.Vertex][]*channeldb.ChannelEdgePolicy)

	// Traverse through all of the available hop hints and include them in
	// our edges map, indexed by the public key of the channel's starting
	// node.
	for _, routeHint := range routeHints {
		// If multiple hop hints are provided within a single route
		// hint, we'll assume they must be chained together and sorted
		// in forward order in order to reach the target successfully.
		for i, hopHint := range routeHint {
			// In order to determine the end node of this hint,
			// we'll need to look at the next hint's start node. If
			// we've reached the end of the hints list, we can
			// assume we've reached the destination.
			endNode := &channeldb.LightningNode{}
			if i != len(routeHint)-1 {
				endNode.AddPubKey(routeHint[i+1].NodeID)
			} else {
				targetPubKey, err := btcec.ParsePubKey(
					target[:], btcec.S256(),
				)
				if err != nil {
					return nil, err
				}
				endNode.AddPubKey(targetPubKey)
			}

			// Finally, create the channel edge from the hop hint
			// and add it to list of edges corresponding to the node
			// at the start of the channel.
			edge := &channeldb.ChannelEdgePolicy{
				Node:      endNode,
				ChannelID: hopHint.ChannelID,
				FeeBaseMSat: lnwire.MilliSatoshi(
					hopHint.FeeBaseMSat,
				),
				FeeProportionalMillionths: lnwire.MilliSatoshi(
					hopHint.FeeProportionalMillionths,
				),
				TimeLockDelta: hopHint.CLTVExpiryDelta,
			}

			v := route.NewVertex(hopHint.NodeID)
			edges[v] = append(edges[v], edge)
		}
	}

	// We'll also obtain a set of bandwidthHints from the lower layer for
	// each of our outbound channels. This will allow the path finding to
	// skip any links that aren't active or just don't have enough
	// bandwidth to carry the payment.
	sourceNode, err := m.Graph.SourceNode()
	if err != nil {
		return nil, err
	}
	bandwidthHints, err := generateBandwidthHints(
		sourceNode, m.QueryBandwidth,
	)
	if err != nil {
		return nil, err
	}

	return &paymentSession{
		additionalEdges: edges,
		bandwidthHints:  bandwidthHints,
		sessionSource:   m,
		pathFinder:      findPath,
	}, nil
}

// NewPaymentSessionForRoute creates a new paymentSession instance that is just
// used for failure reporting to missioncontrol.
func (m *SessionSource) NewPaymentSessionForRoute(preBuiltRoute *route.Route) PaymentSession {
	return &paymentSession{
		sessionSource: m,
		preBuiltRoute: preBuiltRoute,
	}
}

// NewPaymentSessionEmpty creates a new paymentSession instance that is empty,
// and will be exhausted immediately. Used for failure reporting to
// missioncontrol for resumed payment we don't want to make more attempts for.
func (m *SessionSource) NewPaymentSessionEmpty() PaymentSession {
	return &paymentSession{
		sessionSource:      m,
		preBuiltRoute:      &route.Route{},
		preBuiltRouteTried: true,
	}
}
