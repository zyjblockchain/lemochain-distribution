package network

import (
	"errors"
	"fmt"
	"github.com/LemoFoundationLtd/lemochain-core/chain/types"
	"github.com/LemoFoundationLtd/lemochain-core/common"
	"github.com/LemoFoundationLtd/lemochain-core/common/log"
	"github.com/LemoFoundationLtd/lemochain-core/common/subscribe"
	coreNetwork "github.com/LemoFoundationLtd/lemochain-core/network"
	"github.com/LemoFoundationLtd/lemochain-core/network/p2p"
	"github.com/LemoFoundationLtd/lemochain-distribution/chain/params"
	"strconv"
	"sync"
	"time"
)

const (
	ForceSyncInterval = 10 * time.Second
	ReconnectInterval = 5 * time.Second
)

type ProtocolManager struct {
	chainID        uint16
	nodeVersion    uint32
	chain          coreNetwork.BlockChain
	blockCache     *coreNetwork.BlockCache
	txCh           chan *types.Transaction
	rcvBlocksCh    chan types.Blocks
	corePeer       *peer
	dialManager    *DialManager
	isStopping     bool
	newPeerCh      chan p2p.IPeer
	dialCh         chan struct{}
	forceSyncTimer *time.Timer
	wg             sync.WaitGroup
	quitCh         chan struct{}
}

func NewProtocolManager(chainID uint16, coreNodeID *p2p.NodeID, coreNodeEndpoint string, chain coreNetwork.BlockChain) *ProtocolManager {
	pm := &ProtocolManager{
		chainID:     chainID,
		nodeVersion: params.VersionUint(),
		chain:       chain,
		dialManager: NewDialManager(coreNodeID, coreNodeEndpoint),
		blockCache:  coreNetwork.NewBlockCache(),
		txCh:        make(chan *types.Transaction),
		rcvBlocksCh: make(chan types.Blocks),
		newPeerCh:   make(chan p2p.IPeer),
		dialCh:      make(chan struct{}),
		quitCh:      make(chan struct{}),
	}
	pm.sub()
	return pm
}

// sub subscribe channel
func (pm *ProtocolManager) sub() {
	subscribe.Sub(AddNewCorePeer, pm.newPeerCh)
	subscribe.Sub(GetNewTx, pm.txCh)
}

// unSub unsubscribe channel
func (pm *ProtocolManager) unSub() {
	subscribe.UnSub(AddNewCorePeer, pm.newPeerCh)
	subscribe.UnSub(GetNewTx, pm.txCh)
}

// Start
func (pm *ProtocolManager) Start() {
	pm.forceSyncTimer = time.NewTimer(ForceSyncInterval)
	pm.isStopping = false

	go pm.dialLoop()
	go pm.txLoop()
	go pm.rcvBlockLoop()
	go pm.reqStatusLoop()

	pm.dialCh <- struct{}{}
}

// Stop
func (pm *ProtocolManager) Stop() {
	if !pm.isStopping {
		log.Infof("ProtocolManager not start")
		return
	}
	pm.forceSyncTimer.Stop()
	pm.forceSyncTimer = nil
	pm.isStopping = true
	pm.stopCoreNode()
	close(pm.quitCh)
	pm.unSub()
	pm.wg.Wait()
	log.Info("ProtocolManager has stopped")
}

// stopCoreNode stop net connection of core node
func (pm *ProtocolManager) stopCoreNode() {
	if pm.corePeer != nil {
		pm.corePeer.NormalClose()
		pm.corePeer = nil
	}
}

// resetDialTask reset dial task
func (pm *ProtocolManager) resetDialTask() {
	if !pm.isStopping && GetConnectResult() {
		log.Debug("start reconnect...")
		pm.corePeer = nil
		pm.dialCh <- struct{}{}
	}
}

var needReconnect bool // need reconnect == true
var m sync.Mutex

func GetConnectResult() bool {
	m.Lock()
	defer m.Unlock()
	ok := needReconnect
	return ok
}
func SetConnectResult(success bool) {
	m.Lock()
	needReconnect = !success
	m.Unlock()
	log.Debugf("need reconnect: %s", strconv.FormatBool(!success))
}

// dialLoop
func (pm *ProtocolManager) dialLoop() {
	pm.wg.Add(1)
	defer func() {
		pm.wg.Done()
		log.Debugf("dialLoop finished")
	}()
	reconnectTicker := time.NewTicker(ReconnectInterval)
	defer reconnectTicker.Stop()
	for {
		select {
		case <-pm.quitCh:
			return
		case <-pm.dialCh:
			go pm.dialManager.Dial()
		case p := <-pm.newPeerCh:
			log.Debugf("recv connection")
			pm.corePeer = newPeer(p)
			go pm.dialManager.runPeer(p)
			go pm.handlePeer()
		case <-reconnectTicker.C:
			go pm.resetDialTask()
		}
	}
}

// txConfirmLoop receive transactions and confirm and then broadcast them
func (pm *ProtocolManager) txLoop() {
	pm.wg.Add(1)
	defer func() {
		pm.wg.Done()
		log.Debugf("txConfirmLoop finished")
	}()

	for {
		select {
		case <-pm.quitCh:
			log.Info("txConfirmLoop finished")
			return
		case tx := <-pm.txCh:
			if pm.corePeer != nil {
				log.Infof("send txs to core peer,tx: %s", tx.String())
				go pm.corePeer.SendTxs(types.Transactions{tx})
			}
		}
	}
}

// blockLoop receive special type block event
func (pm *ProtocolManager) rcvBlockLoop() {
	pm.wg.Add(1)
	proInterval := 500 * time.Millisecond
	queueTimer := time.NewTimer(proInterval)
	defer func() {
		pm.wg.Done()
		queueTimer.Stop()
		log.Debugf("rcvBlockLoop finished")
	}()

	for {
		select {
		case <-pm.quitCh:
			log.Info("blockLoop finished")
			return
		case blocks := <-pm.rcvBlocksCh:
			pm.forceSyncTimer.Reset(ForceSyncInterval)

			if pm.corePeer == nil {
				log.Debug("drop connect peer")
				break
			}
			// peer's latest height
			pLstHeight := pm.corePeer.LatestStatus().StaHeight

			for _, b := range blocks {
				// block is exist
				if pm.chain.StableBlock() != nil && (b.Height() <= pm.chain.StableBlock().Height() || pm.chain.HasBlock(b.Hash())) {
					continue
				}
				// update latest status
				if b.Height() > pLstHeight && pm.corePeer != nil {
					pm.corePeer.UpdateStatus(b.Height(), b.Hash())
				}
				// parent block exist
				if b.Height() == 0 || pm.chain.HasBlock(b.ParentHash()) {
					pm.insertBlock(b)
				} else {
					pm.blockCache.Add(b)
					log.Debugf("receive a new block (height = %d), but its parent block is not found.", b.Height())
				}
			}
		case <-queueTimer.C:
			processBlock := func(block *types.Block) bool {
				if pm.chain.HasBlock(block.ParentHash()) {
					pm.insertBlock(block)
					return true
				}
				return false
			}
			pm.blockCache.Iterate(processBlock)
			queueTimer.Reset(proInterval)
			// output cache size
			cacheSize := pm.blockCache.Size()
			if cacheSize > 0 && pm.corePeer != nil {
				go pm.corePeer.RequestBlocks(pm.blockCache.FirstHeight()-1, pm.blockCache.FirstHeight()-1)
				log.Debugf("blockCache's size: %d", cacheSize)
			}
		}
	}
}

func (pm *ProtocolManager) reqStatusLoop() {
	pm.wg.Add(1)
	defer func() {
		pm.wg.Done()
		log.Debugf("reqStatusLoop finished")
	}()

	for {
		select {
		case <-pm.quitCh:
			return
		case <-pm.forceSyncTimer.C:
			log.Info("reqStatusLoop: start forceSync block")
			if pm.corePeer != nil {
				if pm.chain.StableBlock() == nil || pm.corePeer.LatestStatus().StaHeight > pm.chain.StableBlock().Height() {
					sta := pm.corePeer.LatestStatus()
					pm.forceSyncBlock(&sta, pm.corePeer)
				} else {
					pm.corePeer.SendReqLatestStatus()
				}
				pm.forceSyncTimer.Reset(ForceSyncInterval)
			}
		}
	}
}

// insertBlock insert block
func (pm *ProtocolManager) insertBlock(b *types.Block) {
	if err := pm.chain.InsertBlock(b); err != nil {
		log.Errorf("insertBlock [%d] failed: %v", b.Height(), err)
	}
	pm.blockCache.Remove(b)
}

// handlePeer handle about peer
func (pm *ProtocolManager) handlePeer() {
	p := pm.corePeer
	// handshake
	rStatus, err := pm.handshake(p)
	if err != nil {
		log.Warnf("protocol handshake failed: %v", err)
		pm.corePeer.conn.Close()
		SetConnectResult(false)
		return
	}

	// sync block
	from, err := pm.findSyncFrom(&rStatus.LatestStatus)
	if err != nil {
		log.Warnf("find sync from error: %v", err)
		p.HardForkClose()
		return
	}
	p.RequestBlocks(from, rStatus.LatestStatus.StaHeight)
	log.Debugf("start handle msg")
	// set first sync height
	if pm.corePeer != nil {
		pm.corePeer.SetFirstSyncHeight(rStatus.LatestStatus.StaHeight)
	}
	SetConnectResult(true)

	for {
		// handle peer net message
		if err := pm.handleMsg(p); err != nil {
			log.Debugf("handle message failed: %v", err)
			if pm.corePeer != nil {
				pm.corePeer.conn.Close()
			}
			SetConnectResult(false)
			return
		}
	}
}

// handshake protocol handshake
func (pm *ProtocolManager) handshake(p *peer) (*ProtocolHandshake, error) {
	phs := &ProtocolHandshake{
		ChainID:     pm.chainID,
		GenesisHash: common.Hash{},
		NodeVersion: pm.nodeVersion,
		LatestStatus: LatestStatus{
			CurHash:   common.Hash{},
			CurHeight: 0,
			StaHash:   common.Hash{},
			StaHeight: 0,
		},
	}
	content := phs.Bytes()
	if content == nil {
		return nil, errors.New("rlp encode error")
	}
	if pm.corePeer == nil {
		return nil, errors.New("peer has closed")
	}
	remoteStatus, err := p.Handshake(content)
	if err != nil {
		return nil, err
	}
	return remoteStatus, nil
}

// forceSyncBlock force to sync block
func (pm *ProtocolManager) forceSyncBlock(status *LatestStatus, p *peer) {
	if pm.chain.StableBlock() != nil && status.StaHeight <= pm.chain.StableBlock().Height() {
		return
	}
	from, err := pm.findSyncFrom(status)
	if err != nil {
		log.Warnf("find sync from error: %v", err)
		p.HardForkClose()
		return
	}
	p.RequestBlocks(from, status.StaHeight)
}

// findSyncFrom find height of which sync from
func (pm *ProtocolManager) findSyncFrom(rStatus *LatestStatus) (uint32, error) {
	var from uint32
	curBlock := pm.chain.StableBlock()
	// staBlock := pm.chain.StableBlock()
	if curBlock == nil {
		return 0, nil
	}

	if curBlock.Height() >= rStatus.StaHeight {
		if !pm.chain.HasBlock(rStatus.StaHash) {
			return 0, errors.New("core peer is not on the same chain")
		}
	}
	from = curBlock.Height() + 1
	return from, nil
}

// handleMsg handle net received message
func (pm *ProtocolManager) handleMsg(p *peer) error {
	msg, err := p.ReadMsg()
	if err != nil {
		return err
	}
	defer func() {
		if msg.Code == coreNetwork.BlocksMsg {
			log.Debugf("receive blocks msg: %d", msg.Code)
		}
	}()

	switch msg.Code {
	case coreNetwork.LstStatusMsg:
		return pm.handleLstStatusMsg(msg, p)
	case coreNetwork.GetLstStatusMsg:
		return pm.handleGetLstStatusMsg(msg, p)
	case coreNetwork.BlockHashMsg:
		return pm.handleBlockHashMsg(msg, p)
	case coreNetwork.TxsMsg:
		return pm.handleTxsMsg(msg)
	case coreNetwork.BlocksMsg:
		return pm.handleBlocksMsg(msg, p)
	case coreNetwork.GetBlocksMsg:
		return pm.handleGetBlocksMsg(msg, p)
	case coreNetwork.GetConfirmsMsg:
		return pm.handleGetConfirmsMsg(msg, p)
	case coreNetwork.ConfirmsMsg:
		return pm.handleConfirmsMsg(msg)
	case coreNetwork.ConfirmMsg:
		return pm.handleConfirmMsg(msg)
	case coreNetwork.DiscoverReqMsg:
		return pm.handleDiscoverReqMsg(msg, p)
	case coreNetwork.DiscoverResMsg:
		return pm.handleDiscoverResMsg(msg)
	case coreNetwork.GetBlocksWithChangeLogMsg:
		return pm.handleGetBlocksWithChangeLogMsg(msg, p)
	default:
		log.Debugf("invalid code: %d, from: %s", msg.Code, common.ToHex(p.NodeID()[:8]))
		return coreNetwork.ErrInvalidCode
	}
}

// handleLstStatusMsg handle latest remote status message
func (pm *ProtocolManager) handleLstStatusMsg(msg *p2p.Msg, p *peer) error {
	var status LatestStatus
	if err := msg.Decode(&status); err != nil {
		return fmt.Errorf("handleLstStatusMsg error: %v", err)
	}
	go pm.forceSyncBlock(&status, p)
	return nil
}

// handleGetLstStatusMsg handle request of latest status
func (pm *ProtocolManager) handleGetLstStatusMsg(msg *p2p.Msg, p *peer) error {
	return nil
}

// handleBlockHashMsg handle receiving block's hash message
func (pm *ProtocolManager) handleBlockHashMsg(msg *p2p.Msg, p *peer) error {
	var hashMsg coreNetwork.BlockHashData
	if err := msg.Decode(&hashMsg); err != nil {
		return fmt.Errorf("handleBlockHashMsg error: %v", err)
	}
	if pm.chain.HasBlock(hashMsg.Hash) {
		return nil
	}
	// judge whether the init synchronization has been completed
	var firstSyncHeight uint32
	if pm.corePeer != nil {
		firstSyncHeight = pm.corePeer.GetFirstSyncHeight()
	} else {
		return errors.New("corePeer == nil")
	}
	if pm.chain.GetBlockByHeight(firstSyncHeight) == nil && hashMsg.Height > firstSyncHeight {
		return nil
	}

	currentHeight := pm.chain.StableBlock().Height()
	// update status
	p.UpdateStatus(hashMsg.Height, hashMsg.Hash)
	go p.RequestBlocks(currentHeight+1, hashMsg.Height)
	return nil
}

// handleTxsMsg handle transactions message
func (pm *ProtocolManager) handleTxsMsg(msg *p2p.Msg) error {
	return nil
}

// handleBlocksMsg handle receiving blocks message
func (pm *ProtocolManager) handleBlocksMsg(msg *p2p.Msg, p *peer) error {
	var blocks types.Blocks
	if err := msg.Decode(&blocks); err != nil {
		return fmt.Errorf("handleBlocksMsg error: %v", err)
	}
	pm.rcvBlocksCh <- blocks
	return nil
}

// handleGetBlocksMsg handle get blocks message
func (pm *ProtocolManager) handleGetBlocksMsg(msg *p2p.Msg, p *peer) error {
	return nil
}

// handleConfirmsMsg handle received block's confirm package message
func (pm *ProtocolManager) handleConfirmsMsg(msg *p2p.Msg) error {
	return nil
}

// handleGetConfirmsMsg handle remote request of block's confirm package message
func (pm *ProtocolManager) handleGetConfirmsMsg(msg *p2p.Msg, p *peer) error {
	return nil
}

// handleConfirmMsg handle confirm broadcast info
func (pm *ProtocolManager) handleConfirmMsg(msg *p2p.Msg) error {
	return nil
}

// handleDiscoverReqMsg handle discover nodes request
func (pm *ProtocolManager) handleDiscoverReqMsg(msg *p2p.Msg, p *peer) error {
	return nil
}

// handleDiscoverResMsg handle discover nodes response
func (pm *ProtocolManager) handleDiscoverResMsg(msg *p2p.Msg) error {
	return nil
}

// handleGetBlocksWithChangeLogMsg for
func (pm *ProtocolManager) handleGetBlocksWithChangeLogMsg(msg *p2p.Msg, p *peer) error {
	return nil
}
