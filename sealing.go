package sealing

import (
	"context"
	"io"

	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	"github.com/ipfs/go-datastore/namespace"
	logging "github.com/ipfs/go-log/v2"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-address"
	padreader "github.com/filecoin-project/go-padreader"
	statemachine "github.com/filecoin-project/go-statemachine"
	sectorstorage "github.com/filecoin-project/sector-storage"
	"github.com/filecoin-project/sector-storage/ffiwrapper"
	"github.com/filecoin-project/specs-actors/actors/abi"
	"github.com/filecoin-project/specs-actors/actors/abi/big"
	"github.com/filecoin-project/specs-actors/actors/builtin/market"
	"github.com/filecoin-project/specs-actors/actors/builtin/miner"
	"github.com/filecoin-project/specs-actors/actors/crypto"
)

const SectorStorePrefix = "/sectors"

var log = logging.Logger("sectors")

type SealingAPI interface {
	StateWaitMsg(context.Context, cid.Cid) (MsgLookup, error)
	StateComputeDataCommitment(ctx context.Context, maddr address.Address, sectorType abi.RegisteredSealProof, deals []abi.DealID, tok TipSetToken) (cid.Cid, error)
	StateSectorPreCommitInfo(ctx context.Context, maddr address.Address, sectorNumber abi.SectorNumber, tok TipSetToken) (*miner.SectorPreCommitOnChainInfo, error)
	StateSectorGetInfo(ctx context.Context, maddr address.Address, sectorNumber abi.SectorNumber, tok TipSetToken) (*miner.SectorOnChainInfo, error)
	StateMinerSectorSize(context.Context, address.Address, TipSetToken) (abi.SectorSize, error)
	StateMinerWorkerAddress(ctx context.Context, maddr address.Address, tok TipSetToken) (address.Address, error)
	StateMinerDeadlines(ctx context.Context, maddr address.Address, tok TipSetToken) (*miner.Deadlines, error)
	StateMinerInitialPledgeCollateral(context.Context, address.Address, abi.SectorNumber, TipSetToken) (big.Int, error)
	StateMarketStorageDeal(context.Context, abi.DealID, TipSetToken) (market.DealProposal, error)
	SendMsg(ctx context.Context, from, to address.Address, method abi.MethodNum, value, gasPrice big.Int, gasLimit int64, params []byte) (cid.Cid, error)
	ChainHead(ctx context.Context) (TipSetToken, abi.ChainEpoch, error)
	ChainGetRandomness(ctx context.Context, tok TipSetToken, personalization crypto.DomainSeparationTag, randEpoch abi.ChainEpoch, entropy []byte) (abi.Randomness, error)
	ChainReadObj(context.Context, cid.Cid) ([]byte, error)
}

type Sealing struct {
	api    SealingAPI
	events Events

	maddr address.Address

	sealer  sectorstorage.SectorManager
	sectors *statemachine.StateGroup
	sc      SectorIDCounter
	verif   ffiwrapper.Verifier

	unsealedInfos map[abi.SectorNumber]UnsealedSectorInfo
	pcp           PreCommitPolicy
}

type UnsealedSectorInfo struct {
	// stored should always equal sum of pieceSizes
	stored     uint64
	pieceSizes []abi.UnpaddedPieceSize
}

func New(api SealingAPI, events Events, maddr address.Address, ds datastore.Batching, sealer sectorstorage.SectorManager, sc SectorIDCounter, verif ffiwrapper.Verifier, pcp PreCommitPolicy) *Sealing {
	s := &Sealing{
		api:    api,
		events: events,

		maddr:         maddr,
		sealer:        sealer,
		sc:            sc,
		verif:         verif,
		unsealedInfos: make(map[abi.SectorNumber]UnsealedSectorInfo),
		pcp:           pcp,
	}

	s.sectors = statemachine.New(namespace.Wrap(ds, datastore.NewKey(SectorStorePrefix)), s, SectorInfo{})

	return s
}

func (m *Sealing) Run(ctx context.Context) error {
	if err := m.restartSectors(ctx); err != nil {
		log.Errorf("%+v", err)
		return xerrors.Errorf("failed load sector states: %w", err)
	}

	return nil
}

func (m *Sealing) Stop(ctx context.Context) error {
	return m.sectors.Stop(ctx)
}
func (m *Sealing) AddPieceToAnySector(ctx context.Context, size abi.UnpaddedPieceSize, r io.Reader, d DealInfo) (abi.SectorNumber, uint64, error) {
	log.Infof("Adding piece for deal %d", d.DealID)
	if (padreader.PaddedSize(uint64(size))) != size {
		return 0, 0, xerrors.Errorf("cannot allocate unpadded piece")
	}

	if size > abi.UnpaddedPieceSize(m.sealer.SectorSize()) {
		return 0, 0, xerrors.Errorf("piece cannot fit into a sector")
	}

	sid, err := m.getAvailableSector(size)
	if err != nil {
		return 0, 0, xerrors.Errorf("creating new sector: %w", err)
	}

	offset := m.unsealedInfos[sid].stored
	ppi, err := m.sealer.AddPiece(sectorstorage.WithPriority(ctx, DealSectorPriority), m.minerSector(sid), m.unsealedInfos[sid].pieceSizes, size, r)
	if err != nil {
		return 0, 0, xerrors.Errorf("writing piece: %w", err)
	}

	err = m.addPiece(sid, Piece{
		Piece:    ppi,
		DealInfo: &d,
	})

	if err != nil {
		return 0, 0, xerrors.Errorf("adding piece to sector: %w", err)
	}

	return sid, offset, nil
}

func (m *Sealing) addPiece(sectorID abi.SectorNumber, piece Piece) error {
	log.Infof("Adding piece to sector %d", sectorID)
	err := m.sectors.Send(uint64(sectorID), SectorAddPiece{NewPiece: piece})
	if err != nil {
		return err
	}

	ui := m.unsealedInfos[sectorID]
	m.unsealedInfos[sectorID] = UnsealedSectorInfo{
		stored:     ui.stored + uint64(piece.Piece.Size.Unpadded()),
		pieceSizes: append(ui.pieceSizes, piece.Piece.Size.Unpadded()),
	}

	return nil
}

func (m *Sealing) Remove(ctx context.Context, sid abi.SectorNumber) error {
	return m.sectors.Send(uint64(sid), SectorRemove{})
}

func (m *Sealing) StartPacking(sectorID abi.SectorNumber) error {
	log.Infof("Starting packing sector %d", sectorID)
	err := m.sectors.Send(uint64(sectorID), SectorStartPacking{})
	if err != nil {
		return err
	}

	delete(m.unsealedInfos, sectorID)

	return nil
}

func (m *Sealing) getAvailableSector(size abi.UnpaddedPieceSize) (abi.SectorNumber, error) {
	ss := m.sealer.SectorSize()
	for k, v := range m.unsealedInfos {
		if v.stored+uint64(size) <= uint64(ss) {
			// TODO: Support multiple deal sizes in the same sector
			if len(v.pieceSizes) == 0 || v.pieceSizes[0] == size {
				return k, nil
			}
		}
	}

	return m.newSector()
}

// newSector creates a new sector for deal storage
func (m *Sealing) newSector() (abi.SectorNumber, error) {
	sid, err := m.sc.Next()
	if err != nil {
		return 0, xerrors.Errorf("getting sector number: %w", err)
	}

	err = m.sealer.NewSector(context.TODO(), m.minerSector(sid))
	if err != nil {
		return 0, xerrors.Errorf("initializing sector: %w", err)
	}

	rt, err := ffiwrapper.SealProofTypeFromSectorSize(m.sealer.SectorSize())
	if err != nil {
		return 0, xerrors.Errorf("bad sector size: %w", err)
	}

	log.Infof("Creating sector %d", sid)
	err = m.sectors.Send(uint64(sid), SectorStart{
		ID:         sid,
		SectorType: rt,
	})

	if err != nil {
		return 0, xerrors.Errorf("starting the sector fsm: %w", err)
	}

	m.unsealedInfos[sid] = UnsealedSectorInfo{
		stored:     0,
		pieceSizes: nil,
	}

	return sid, nil
}

// newSectorCC accepts a slice of pieces with no deal (junk data)
func (m *Sealing) newSectorCC(sid abi.SectorNumber, pieces []Piece) error {
	rt, err := ffiwrapper.SealProofTypeFromSectorSize(m.sealer.SectorSize())
	if err != nil {
		return xerrors.Errorf("bad sector size: %w", err)
	}

	log.Infof("Creating CC sector %d", sid)
	return m.sectors.Send(uint64(sid), SectorStartCC{
		ID:         sid,
		Pieces:     pieces,
		SectorType: rt,
	})
}

func (m *Sealing) minerSector(num abi.SectorNumber) abi.SectorID {
	mid, err := address.IDFromAddress(m.maddr)
	if err != nil {
		panic(err)
	}

	return abi.SectorID{
		Number: num,
		Miner:  abi.ActorID(mid),
	}
}

func (m *Sealing) Address() address.Address {
	return m.maddr
}
