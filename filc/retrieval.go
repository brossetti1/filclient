package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/application-research/filclient"
	"github.com/application-research/filclient/retrievehelper"
	"github.com/dustin/go-humanize"
	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-fil-markets/retrievalmarket"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/ipfs/go-blockservice"
	"github.com/ipfs/go-cid"
	ipldformat "github.com/ipfs/go-ipld-format"
	"github.com/ipfs/go-merkledag"
	"github.com/ipld/go-ipld-prime"
	"golang.org/x/term"
	"golang.org/x/xerrors"
)

type RetrievalCandidate struct {
	Miner   address.Address
	RootCid cid.Cid
	DealID  uint
}

type CandidateSelectionConfig struct {
	// Whether retrieval over IPFS is preferred if available
	tryIPFS bool

	// If true, candidates will be tried in the order they're passed in
	// unchanged (and all other sorting-related options will be ignored)
	noSort bool
}

type RetrievalResults struct {
}

func (node *Node) GetRetrievalCandidates(endpoint string, c cid.Cid) ([]RetrievalCandidate, error) {

	endpointURL, err := url.Parse(endpoint)
	if err != nil {
		return nil, xerrors.Errorf("endpoint %s is not a valid url", endpoint)
	}
	endpointURL.Path = path.Join(endpointURL.Path, c.String())

	resp, err := http.Get(endpointURL.String())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http request to endpoint %s got status %v", endpointURL, resp.StatusCode)
	}

	var res []RetrievalCandidate

	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, xerrors.Errorf("could not unmarshal http response for cid %s", c)
	}

	return res, nil
}

type RetrievalStats interface {
	GetByteSize() uint64
	GetDuration() time.Duration
	GetAverageBytesPerSecond() uint64
}

type FILRetrievalStats struct {
	filclient.RetrievalStats
}

func (stats *FILRetrievalStats) GetByteSize() uint64 {
	return stats.Size
}

func (stats *FILRetrievalStats) GetDuration() time.Duration {
	return stats.Duration
}

func (stats *FILRetrievalStats) GetAverageBytesPerSecond() uint64 {
	return stats.AverageSpeed
}

type IPFSRetrievalStats struct {
	ByteSize uint64
	Duration time.Duration
}

func (stats *IPFSRetrievalStats) GetByteSize() uint64 {
	return stats.ByteSize
}

func (stats *IPFSRetrievalStats) GetDuration() time.Duration {
	return stats.Duration
}

func (stats *IPFSRetrievalStats) GetAverageBytesPerSecond() uint64 {
	return uint64(float64(stats.ByteSize) / stats.Duration.Seconds())
}

func (node *Node) RetrieveFromBestCandidate(
	ctx context.Context,
	fc *filclient.FilClient,
	c cid.Cid,
	selNode ipld.Node,
	candidates []RetrievalCandidate,
	cfg CandidateSelectionConfig,
) (RetrievalStats, error) {
	// Try IPFS first, if requested
	if cfg.tryIPFS && (selNode == nil || selNode.IsNull()) {
		stats, err := node.tryRetrieveFromIPFS(ctx, c)
		if err != nil {
			// If IPFS failed, log the error and continue to FIL attempt
			log.Error(err) // TODO: handle errors specifically
		} else {
			return stats, err
		}
	}

	stats, err := node.tryRetrieveFromFIL(ctx, fc, c, selNode, candidates, cfg)
	if err != nil {
		log.Error(err) // TODO
	} else {
		return stats, err
	}

	return nil, fmt.Errorf("all retrieval attempts failed")
}

func (node *Node) tryRetrieveFromFIL(
	ctx context.Context,
	fc *filclient.FilClient,
	c cid.Cid,
	selNode ipld.Node,
	candidates []RetrievalCandidate,
	cfg CandidateSelectionConfig,
) (*FILRetrievalStats, error) {

	// If no miners are provided, there's nothing else we can do
	if len(candidates) == 0 {
		log.Info("No miners were provided, will not attempt FIL retrieval")
		return nil, xerrors.Errorf("retrieval failed: no miners were provided")
	}

	// If IPFS retrieval was unavailable, do a full FIL retrieval. Start with
	// querying all the candidates for sorting.

	log.Info("Querying FIL retrieval candidates...")

	type CandidateQuery struct {
		Candidate RetrievalCandidate
		Response  *retrievalmarket.QueryResponse
	}
	checked := 0
	var queries []CandidateQuery
	var queriesLk sync.Mutex

	var wg sync.WaitGroup
	wg.Add(len(candidates))

	for _, candidate := range candidates {

		// Copy into loop, cursed go
		candidate := candidate

		go func() {
			defer wg.Done()

			query, err := fc.RetrievalQuery(ctx, candidate.Miner, candidate.RootCid)
			if err != nil {
				log.Debugf("Retrieval query for miner %s failed: %v", candidate.Miner, err)
				return
			}

			queriesLk.Lock()
			queries = append(queries, CandidateQuery{Candidate: candidate, Response: query})
			checked++
			fmt.Fprintf(os.Stderr, "%v/%v\r", checked, len(candidates))
			queriesLk.Unlock()
		}()
	}

	wg.Wait()

	log.Infof("Got back %v retrieval query results of a total of %v candidates", len(queries), len(candidates))

	if len(queries) == 0 {
		return nil, xerrors.Errorf("retrieval failed: queries failed for all miners")
	}

	// After we got the query results, sort them with respect to the candidate
	// selection config as long as noSort isn't requested (TODO - more options)

	if !cfg.noSort {
		sort.Slice(queries, func(i, j int) bool {
			a := queries[i].Response
			b := queries[i].Response

			// Always prefer unsealed to sealed, no matter what
			if a.UnsealPrice.IsZero() && !b.UnsealPrice.IsZero() {
				return true
			}

			// Select lower price, or continue if equal
			aTotalPrice := totalCost(a)
			bTotalPrice := totalCost(b)
			if !aTotalPrice.Equals(bTotalPrice) {
				return aTotalPrice.LessThan(bTotalPrice)
			}

			// Select smaller size, or continue if equal
			if a.Size != b.Size {
				return a.Size < b.Size
			}

			return false
		})
	}

	// Now attempt retrievals in serial from first to last, until one works.
	// stats will get set if a retrieval succeeds - if no retrievals work, it
	// will still be nil after the loop finishes
	var stats *FILRetrievalStats = nil
	for _, query := range queries {
		log.Infof("Attempting FIL retrieval with miner %s from root CID %s (%s)", query.Candidate.Miner, query.Candidate.RootCid, types.FIL(totalCost(query.Response)))

		if selNode != nil && !selNode.IsNull() {
			log.Infof("Using selector %s", selNode)
		}

		proposal, err := retrievehelper.RetrievalProposalForAsk(query.Response, query.Candidate.RootCid, selNode)
		if err != nil {
			log.Debugf("Failed to create retrieval proposal with candidate miner %s: %v", query.Candidate.Miner, err)
			continue
		}

		var bytesReceived uint64
		stats_, err := fc.RetrieveContentWithProgressCallback(ctx, query.Candidate.Miner, proposal, func(bytesReceived_ uint64) {
			bytesReceived = bytesReceived_
			printProgress(bytesReceived)
		})
		if err != nil {
			log.Errorf("Failed to retrieve content with candidate miner %s: %v", query.Candidate.Miner, err)
			continue
		}

		stats = &FILRetrievalStats{RetrievalStats: *stats_}
		break
	}

	if stats == nil {
		return nil, xerrors.New("retrieval failed for all miners")
	}

	log.Info("FIL retrieval succeeded")

	return stats, nil
}

func (node *Node) tryRetrieveFromIPFS(ctx context.Context, c cid.Cid) (*IPFSRetrievalStats, error) {
	log.Info("Searching IPFS for CID...")

	providers := node.DHT.FindProvidersAsync(ctx, c, 20)

	ready := make(chan bool, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case provider := <-providers:
				if provider.ID == "" {
					continue
				}

				log.Infof("Provider candidate %s", provider)

				if err := node.Host.Connect(ctx, provider); err != nil {
					log.Warnf("Failed to connect to IPFS provider %s: %v", provider, err)
					continue
				}

				log.Infof("Connected to IPFS provider %s", provider)
				ready <- true
			}
		}
	}()

	select {
	// TODO: also add connection timeout
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-ready:
		// All we do on ready is stop blocking
	}

	// If we were able to connect to at least one of the providers, go ahead
	// with the retrieval

	var progressLk sync.Mutex
	var bytesRetrieved uint64 = 0
	startTime := time.Now()

	log.Info("Starting IPFS retrieval")

	bserv := blockservice.New(node.Blockstore, node.Bitswap)
	dserv := merkledag.NewDAGService(bserv)
	//dsess := dserv.Session(ctx)

	cset := cid.NewSet()
	if err := merkledag.Walk(ctx, func(ctx context.Context, c cid.Cid) ([]*ipldformat.Link, error) {
		node, err := dserv.Get(ctx, c)
		if err != nil {
			return nil, err
		}

		// Only count leaf nodes toward the total size
		if len(node.Links()) == 0 {
			progressLk.Lock()
			nodeSize, err := node.Size()
			if err != nil {
				nodeSize = 0
			}
			bytesRetrieved += nodeSize
			printProgress(bytesRetrieved)
			progressLk.Unlock()
		}

		if c.Type() == cid.Raw {
			return nil, nil
		}

		return node.Links(), nil
	}, c, cset.Visit, merkledag.Concurrent()); err != nil {
		return nil, err
	}

	log.Info("IPFS retrieval succeeded")

	return &IPFSRetrievalStats{
		ByteSize: bytesRetrieved,
		Duration: time.Since(startTime),
	}, nil
}

func totalCost(qres *retrievalmarket.QueryResponse) big.Int {
	return big.Add(big.Mul(qres.MinPricePerByte, big.NewIntUnsigned(qres.Size)), qres.UnsealPrice)
}

func printProgress(bytesReceived uint64) {
	str := fmt.Sprintf("%v (%v)", bytesReceived, humanize.IBytes(bytesReceived))

	termWidth, _, err := term.GetSize(int(os.Stdin.Fd()))
	strLen := len(str)
	if err == nil {

		if strLen < termWidth {
			// If the string is shorter than the terminal width, pad right side
			// with spaces to remove old text
			str = strings.Join([]string{str, strings.Repeat(" ", termWidth-strLen)}, "")
		} else if strLen > termWidth {
			// If the string doesn't fit in the terminal, cut it down to a size
			// that fits
			str = str[:termWidth]
		}
	}

	fmt.Fprintf(os.Stderr, "%s\r", str)
}
