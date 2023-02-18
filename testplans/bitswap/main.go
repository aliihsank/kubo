package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/testground/sdk-go/network"
	"github.com/testground/sdk-go/run"
	"github.com/testground/sdk-go/runtime"
	"github.com/testground/sdk-go/sync"

	bitswap "github.com/ipfs/go-libipfs/bitswap"
	bsnet "github.com/ipfs/go-libipfs/bitswap/network"
	block "github.com/ipfs/go-libipfs/blocks"
	"github.com/ipfs/go-cid"
	datastore "github.com/ipfs/go-datastore"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	exchange "github.com/ipfs/go-ipfs-exchange-interface"
	bstats "github.com/ipfs/go-ipfs-regression/bitswap"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
	"github.com/multiformats/go-multihash"
)

var (
	testcases = map[string]interface{}{
		"speed-test": run.InitializedTestCaseFn(runSpeedTest),
	}
	networkState  = sync.State("network-configured")
	readyState    = sync.State("ready-to-publish")
	readyDLState  = sync.State("ready-to-download")
	doneState     = sync.State("done")
	providerTopic = sync.NewTopic("provider", &peer.AddrInfo{})
	blockTopic    = sync.NewTopic("blocks", &multihash.Multihash{})
)

func main() {
	run.InvokeMap(testcases)
}

func runSpeedTest(runenv *runtime.RunEnv, initCtx *run.InitContext) error {
	runenv.RecordMessage("running speed-test")
	ctx := context.Background()

	netclient := initCtx.NetClient

	linkShape := network.LinkShape{}
	// linkShape := network.LinkShape{
	// 	Latency:   50 * time.Millisecond,
	// 	Jitter:    20 * time.Millisecond,
	// 	Bandwidth: 3e6,
	// 	// Filter: (not implemented)
	// 	Loss:          0.02,
	// 	Corrupt:       0.01,
	// 	CorruptCorr:   0.1,
	// 	Reorder:       0.01,
	// 	ReorderCorr:   0.1,
	// 	Duplicate:     0.02,
	// 	DuplicateCorr: 0.1,
	// }
	netclient.MustConfigureNetwork(ctx, &network.Config{
		Network:        "default",
		Enable:         true,
		Default:        linkShape,
		CallbackState:  networkState,
		CallbackTarget: runenv.TestGroupInstanceCount,
		RoutingPolicy:  network.AllowAll,
	})
	listen, err := multiaddr.NewMultiaddr(fmt.Sprintf("/ip4/%s/tcp/3333", netclient.MustGetDataNetworkIP().String()))
	if err != nil {
		return err
	}
	h, err := libp2p.New(libp2p.ListenAddrs(listen))
	if err != nil {
		return err
	}
	defer h.Close()
	kad, err := dht.New(ctx, h)
	if err != nil {
		return err
	}
	for _, a := range h.Addrs() {
		runenv.RecordMessage("listening on addr: %s", a.String())
	}
	bstore := blockstore.NewBlockstore(datastore.NewMapDatastore())
	ex := bitswap.New(ctx, bsnet.NewFromIpfsHost(h, kad), bstore)
	switch runenv.TestGroupID {
	case "providers":
		runenv.RecordMessage("running provider")
		err = runProvide(ctx, runenv, h, bstore, ex, initCtx)
	case "requestors":
		runenv.RecordMessage("running requestor")
		err = runRequest(ctx, runenv, h, bstore, ex, initCtx)
	default:
		runenv.RecordMessage("not part of a group")
		err = errors.New("unknown test group id")
	}
	return err
}

func runProvide(ctx context.Context, runenv *runtime.RunEnv, h host.Host, bstore blockstore.Blockstore, ex exchange.Interface, initCtx *run.InitContext) error {
	client := initCtx.SyncClient

	ai := peer.AddrInfo{
		ID:    h.ID(),
		Addrs: h.Addrs(),
	}
	client.MustPublish(ctx, providerTopic, &ai)
	_ = client.MustSignalAndWait(ctx, readyState, runenv.TestInstanceCount)

	size := runenv.SizeParam("size")
	blockCount := runenv.IntParam("block_count")
	for i := 0; i <= blockCount - 1; i++ {
		runenv.RecordMessage("generating %d-sized random block[%d] ", size, i)
		buf := make([]byte, size)
		rand.Read(buf)
		blk := block.NewBlock(buf)
		err := bstore.Put(ctx, blk)
		if err != nil {
			return err
		}
		mh := blk.Multihash()
		runenv.RecordMessage("publishing block %s", mh.String())
		client.MustPublish(ctx, blockTopic, &mh)
	}
	_ = client.MustSignalAndWait(ctx, readyDLState, runenv.TestInstanceCount)
	_ = client.MustSignalAndWait(ctx, doneState, runenv.TestInstanceCount)
	return nil
}

func runRequest(ctx context.Context, runenv *runtime.RunEnv, h host.Host, bstore blockstore.Blockstore, ex exchange.Interface, initCtx *run.InitContext) error {
	client := initCtx.SyncClient

	providers := make(chan *peer.AddrInfo)
	blkmhs := make(chan *multihash.Multihash)
	providerSub, err := client.Subscribe(ctx, providerTopic, providers)
	if err != nil {
		return err
	}

	providerSub.Done()

	providerCount := runenv.IntParam("provider_count")
	for i := 0; i <= providerCount - 1; i++ {
		ai := <-providers
		runenv.RecordMessage("connecting to provider provider[%d]: %s", i, fmt.Sprint(*ai))

		err = h.Connect(ctx, *ai)
		if err != nil {
			return fmt.Errorf("could not connect to provider: %w", err)
		}
		runenv.RecordMessage("requester connected to provider[%d]: %s", i, fmt.Sprint(*ai))
	}

	runenv.RecordMessage("connected to all providers")

	// tell the provider that we're ready for it to publish blocks
	_ = client.MustSignalAndWait(ctx, readyState, runenv.TestInstanceCount)
	// wait until the provider is ready for us to start downloading
	_ = client.MustSignalAndWait(ctx, readyDLState, runenv.TestInstanceCount)

	blockmhSub, err := client.Subscribe(ctx, blockTopic, blkmhs)
	if err != nil {
		return fmt.Errorf("could not subscribe to block sub: %w", err)
	}
	defer blockmhSub.Done()

	begin := time.Now()
	blockCount := runenv.IntParam("block_count")
	for i := 0; i <= blockCount - 1; i++ {
		mh := <-blkmhs
		runenv.RecordMessage("downloading block[%d] %s", i, mh.String())
		dlBegin := time.Now()
		blk, err := ex.GetBlock(ctx, cid.NewCidV0(*mh))
		if err != nil {
			return fmt.Errorf("could not download get block[%d] %s: %w", i, mh.String(), err)
		}
		dlDuration := time.Since(dlBegin)
		s := &bstats.BitswapStat{
			SingleDownloadSpeed: &bstats.SingleDownloadSpeed{
				Cid:              blk.Cid().String(),
				DownloadDuration: dlDuration,
			},
		}
		runenv.RecordMessage(bstats.Marshal(s))
	}
	duration := time.Since(begin)
	s := &bstats.BitswapStat{
		MultipleDownloadSpeed: &bstats.MultipleDownloadSpeed{
			BlockCount:    blockCount,
			TotalDuration: duration,
		},
	}
	runenv.RecordMessage(bstats.Marshal(s))
	_ = client.MustSignalEntry(ctx, doneState)
	return nil
}
