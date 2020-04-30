package storagemarket_test

import (
	"bytes"
	"context"
	"io/ioutil"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/filecoin-project/go-address"
	datatransfer "github.com/filecoin-project/go-data-transfer"
	graphsync "github.com/filecoin-project/go-data-transfer/impl/graphsync"
	"github.com/filecoin-project/specs-actors/actors/abi"
	"github.com/filecoin-project/specs-actors/actors/abi/big"
	"github.com/filecoin-project/specs-actors/actors/builtin/market"
	"github.com/ipfs/go-cid"
	"github.com/ipld/go-ipld-prime"
	cidlink "github.com/ipld/go-ipld-prime/linking/cid"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/go-fil-markets/filestore"
	"github.com/filecoin-project/go-fil-markets/pieceio"
	"github.com/filecoin-project/go-fil-markets/pieceio/cario"
	"github.com/filecoin-project/go-fil-markets/piecestore"
	"github.com/filecoin-project/go-fil-markets/retrievalmarket/discovery"
	"github.com/filecoin-project/go-fil-markets/shared_testutil"
	"github.com/filecoin-project/go-fil-markets/storagemarket"
	storageimpl "github.com/filecoin-project/go-fil-markets/storagemarket/impl"
	"github.com/filecoin-project/go-fil-markets/storagemarket/impl/requestvalidation"
	"github.com/filecoin-project/go-fil-markets/storagemarket/network"
	"github.com/filecoin-project/go-fil-markets/storagemarket/testnodes"
)

func TestMakeDeal(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.Client.Run(ctx)
	err := h.Provider.Start(ctx)
	assert.NoError(t, err)

	// set up a subscriber
	dealChan := make(chan storagemarket.MinerDeal)
	subscriber := func(event storagemarket.ProviderEvent, deal storagemarket.MinerDeal) {
		dealChan <- deal
	}
	_ = h.Provider.SubscribeToEvents(subscriber)

	result := h.ProposeStorageDeal(t, &storagemarket.DataRef{TransferType: storagemarket.TTGraphsync, Root: h.PayloadCid})
	proposalCid := result.ProposalCid

	time.Sleep(time.Millisecond * 200)

	ctx, canc := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer canc()
	var seenDeal storagemarket.MinerDeal
	var actualStates []storagemarket.StorageDealStatus
	for seenDeal.State != storagemarket.StorageDealCompleted {
		select {
		case seenDeal = <-dealChan:
			actualStates = append(actualStates, seenDeal.State)
		case <-ctx.Done():
			t.Fatalf("never saw event")
		}
	}

	expectedStates := []storagemarket.StorageDealStatus{
		storagemarket.StorageDealValidating,
		storagemarket.StorageDealProposalAccepted,
		storagemarket.StorageDealTransferring,
		storagemarket.StorageDealVerifyData,
		storagemarket.StorageDealEnsureProviderFunds,
		storagemarket.StorageDealPublish,
		storagemarket.StorageDealPublishing,
		storagemarket.StorageDealStaged,
		storagemarket.StorageDealSealing,
		storagemarket.StorageDealActive,
		storagemarket.StorageDealCompleted,
	}
	assert.Equal(t, expectedStates, actualStates)

	// check a couple of things to make sure we're getting the whole deal
	assert.Equal(t, h.TestData.Host1.ID(), seenDeal.Client)
	assert.Empty(t, seenDeal.Message)
	assert.Equal(t, proposalCid, seenDeal.ProposalCid)
	assert.Equal(t, h.ProviderAddr, seenDeal.ClientDealProposal.Proposal.Provider)

	cd, err := h.Client.GetLocalDeal(ctx, proposalCid)
	assert.NoError(t, err)
	assert.Equal(t, storagemarket.StorageDealActive, cd.State)

	providerDeals, err := h.Provider.ListLocalDeals()
	assert.NoError(t, err)

	pd := providerDeals[0]
	assert.Equal(t, pd.ProposalCid, proposalCid)
	assert.Equal(t, storagemarket.StorageDealCompleted, pd.State)
}

func TestMakeDealOffline(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.Client.Run(ctx)

	carBuf := new(bytes.Buffer)

	err := cario.NewCarIO().WriteCar(ctx, h.TestData.Bs1, h.PayloadCid, h.TestData.AllSelector, carBuf)
	require.NoError(t, err)

	commP, size, err := pieceio.GeneratePieceCommitment(abi.RegisteredProof_StackedDRG2KiBPoSt, carBuf, uint64(carBuf.Len()))
	assert.NoError(t, err)

	dataRef := &storagemarket.DataRef{
		TransferType: storagemarket.TTManual,
		Root:         h.PayloadCid,
		PieceCid:     &commP,
		PieceSize:    size,
	}

	result := h.ProposeStorageDeal(t, dataRef)
	proposalCid := result.ProposalCid

	time.Sleep(time.Millisecond * 100)

	cd, err := h.Client.GetLocalDeal(ctx, proposalCid)
	assert.NoError(t, err)
	assert.Equal(t, storagemarket.StorageDealValidating, cd.State)

	providerDeals, err := h.Provider.ListLocalDeals()
	assert.NoError(t, err)

	pd := providerDeals[0]
	assert.True(t, pd.ProposalCid.Equals(proposalCid))
	assert.Equal(t, storagemarket.StorageDealWaitingForData, pd.State)

	err = cario.NewCarIO().WriteCar(ctx, h.TestData.Bs1, h.PayloadCid, h.TestData.AllSelector, carBuf)
	require.NoError(t, err)
	err = h.Provider.ImportDataForDeal(ctx, pd.ProposalCid, carBuf)
	require.NoError(t, err)

	time.Sleep(time.Millisecond * 100)

	cd, err = h.Client.GetLocalDeal(ctx, proposalCid)
	assert.NoError(t, err)
	assert.Equal(t, storagemarket.StorageDealActive, cd.State)

	providerDeals, err = h.Provider.ListLocalDeals()
	assert.NoError(t, err)

	pd = providerDeals[0]
	assert.True(t, pd.ProposalCid.Equals(proposalCid))
	assert.Equal(t, storagemarket.StorageDealCompleted, pd.State)
}

func TestMakeDealNonBlocking(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	testCids := shared_testutil.GenerateCids(2)

	h.ClientNode.AddFundsCid = testCids[0]
	h.Client.Run(ctx)

	h.ProviderNode.WaitForMessageBlocks = true
	h.ProviderNode.AddFundsCid = testCids[1]
	err := h.Provider.Start(ctx)
	assert.NoError(t, err)

	result := h.ProposeStorageDeal(t, &storagemarket.DataRef{TransferType: storagemarket.TTGraphsync, Root: h.PayloadCid})

	time.Sleep(time.Millisecond * 500)

	cd, err := h.Client.GetLocalDeal(ctx, result.ProposalCid)
	assert.NoError(t, err)
	assert.Equal(t, storagemarket.StorageDealValidating, cd.State)

	providerDeals, err := h.Provider.ListLocalDeals()
	assert.NoError(t, err)

	// Provider should be blocking on waiting for funds to appear on chain
	pd := providerDeals[0]
	assert.Equal(t, result.ProposalCid, pd.ProposalCid)
	assert.Equal(t, storagemarket.StorageDealProviderFunding, pd.State)
}

type harness struct {
	Ctx          context.Context
	Epoch        abi.ChainEpoch
	PayloadCid   cid.Cid
	ProviderAddr address.Address
	Client       storagemarket.StorageClient
	ClientNode   *testnodes.FakeClientNode
	Provider     storagemarket.StorageProvider
	ProviderNode *testnodes.FakeProviderNode
	ProviderInfo storagemarket.StorageProviderInfo
	TestData     *shared_testutil.Libp2pTestData
}

func newHarness(t *testing.T, ctx context.Context) *harness {
	epoch := abi.ChainEpoch(100)
	td := shared_testutil.NewLibp2pTestData(ctx, t)
	rootLink := td.LoadUnixFSFile(t, "payload.txt", false)
	payloadCid := rootLink.(cidlink.Link).Cid

	smState := testnodes.NewStorageMarketState()
	clientNode := testnodes.FakeClientNode{
		FakeCommonNode: testnodes.FakeCommonNode{SMState: smState},
		ClientAddr:     address.TestAddress,
	}

	expDealID := abi.DealID(rand.Uint64())
	psdReturn := market.PublishStorageDealsReturn{IDs: []abi.DealID{expDealID}}
	psdReturnBytes := bytes.NewBuffer([]byte{})
	err := psdReturn.MarshalCBOR(psdReturnBytes)
	assert.NoError(t, err)

	providerAddr := address.TestAddress2
	tempPath, err := ioutil.TempDir("", "storagemarket_test")
	assert.NoError(t, err)
	ps := piecestore.NewPieceStore(td.Ds2)
	providerNode := &testnodes.FakeProviderNode{
		FakeCommonNode: testnodes.FakeCommonNode{
			SMState:                smState,
			WaitForMessageRetBytes: psdReturnBytes.Bytes(),
		},
		MinerAddr: providerAddr,
	}
	fs, err := filestore.NewLocalFileStore(filestore.OsPath(tempPath))
	assert.NoError(t, err)

	// create provider and client
	dt1 := graphsync.NewGraphSyncDataTransfer(td.Host1, td.GraphSync1, td.DTStoredCounter1)
	require.NoError(t, dt1.RegisterVoucherType(reflect.TypeOf(&requestvalidation.StorageDataTransferVoucher{}), &fakeDTValidator{}))

	client, err := storageimpl.NewClient(
		network.NewFromLibp2pHost(td.Host1),
		td.Bs1,
		dt1,
		discovery.NewLocal(td.Ds1),
		td.Ds1,
		&clientNode,
	)
	require.NoError(t, err)
	dt2 := graphsync.NewGraphSyncDataTransfer(td.Host2, td.GraphSync2, td.DTStoredCounter2)
	provider, err := storageimpl.NewProvider(
		network.NewFromLibp2pHost(td.Host2),
		td.Ds2,
		td.Bs2,
		fs,
		ps,
		dt2,
		providerNode,
		providerAddr,
		abi.RegisteredProof_StackedDRG2KiBPoSt,
	)
	assert.NoError(t, err)

	// set ask price where we'll accept any price
	err = provider.AddAsk(big.NewInt(0), 50_000)
	assert.NoError(t, err)

	err = provider.Start(ctx)
	assert.NoError(t, err)

	// Closely follows the MinerInfo struct in the spec
	providerInfo := storagemarket.StorageProviderInfo{
		Address:    providerAddr,
		Owner:      providerAddr,
		Worker:     providerAddr,
		SectorSize: 1 << 20,
		PeerID:     td.Host2.ID(),
	}

	return &harness{
		Ctx:          ctx,
		Epoch:        epoch,
		PayloadCid:   payloadCid,
		ProviderAddr: providerAddr,
		Client:       client,
		ClientNode:   &clientNode,
		Provider:     provider,
		ProviderNode: providerNode,
		ProviderInfo: providerInfo,
		TestData:     td,
	}
}

func (h *harness) ProposeStorageDeal(t *testing.T, dataRef *storagemarket.DataRef) *storagemarket.ProposeStorageDealResult {
	result, err := h.Client.ProposeStorageDeal(
		h.Ctx,
		h.ProviderAddr,
		&h.ProviderInfo,
		dataRef,
		h.Epoch+100,
		h.Epoch+20100,
		big.NewInt(1),
		big.NewInt(0),
		abi.RegisteredProof_StackedDRG2KiBPoSt,
	)
	assert.NoError(t, err)
	return result
}

type fakeDTValidator struct{}

func (v *fakeDTValidator) ValidatePush(sender peer.ID, voucher datatransfer.Voucher, baseCid cid.Cid, selector ipld.Node) error {
	return nil
}

func (v *fakeDTValidator) ValidatePull(receiver peer.ID, voucher datatransfer.Voucher, baseCid cid.Cid, selector ipld.Node) error {
	return nil
}

var _ datatransfer.RequestValidator = (*fakeDTValidator)(nil)
