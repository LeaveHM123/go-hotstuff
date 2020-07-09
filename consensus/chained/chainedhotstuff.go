package chained

import (
	"bytes"
	"context"
	"github.com/golang/protobuf/proto"
	"github.com/niclabs/tcrsa"
	"github.com/sirupsen/logrus"
	go_hotstuff "github.com/wjbbig/go-hotstuff"
	"github.com/wjbbig/go-hotstuff/config"
	"github.com/wjbbig/go-hotstuff/consensus"
	"github.com/wjbbig/go-hotstuff/logging"
	pb "github.com/wjbbig/go-hotstuff/proto"
	"strconv"
)

var logger *logrus.Logger

func init() {
	logger = logging.GetLogger()
}

type ChainedHotStuff struct {
	consensus.HotStuffImpl
	genericQC *pb.QuorumCert
	lockQC    *pb.QuorumCert
	cancel    context.CancelFunc
}

func NewChainedHotStuff(id int, handleMethod func(string) string) *ChainedHotStuff {
	msgEntrance := make(chan *pb.Msg)
	chs := &ChainedHotStuff{}
	chs.MsgEntrance = msgEntrance
	chs.ID = uint32(id)
	chs.View = consensus.NewView(1, 1)
	logger.Debugf("[HOTSTUFF] Init block storage, replica id: %d", id)
	chs.BlockStorage = go_hotstuff.NewBlockStorageImpl(strconv.Itoa(id))
	logger.Debugf("[HOTSTUFF] Generate genesis block")
	genesisBlock := consensus.GenerateGenesisBlock()
	err := chs.BlockStorage.Put(genesisBlock)
	if err != nil {
		logger.Fatal("generate genesis block failed")
	}
	chs.genericQC = &pb.QuorumCert{
		BlockHash: genesisBlock.Hash,
		Type:      pb.MsgType_PREPARE_VOTE,
		ViewNum:   0,
		Signature: nil,
	}
	chs.lockQC = &pb.QuorumCert{
		BlockHash: genesisBlock.Hash,
		Type:      pb.MsgType_PREPARE_VOTE,
		ViewNum:   0,
		Signature: nil,
	}
	logger.Debugf("[HOTSTUFF] Init command set, replica id: %d", id)
	chs.CmdSet = go_hotstuff.NewCmdSet()

	// read config
	chs.Config = config.HotStuffConfig{}
	chs.Config.ReadConfig()

	// init timer and stop it
	chs.TimeChan = go_hotstuff.NewTimer(chs.Config.Timeout)
	chs.TimeChan.Init()

	chs.BatchTimeChan = go_hotstuff.NewTimer(chs.Config.BatchTimeout)
	chs.BatchTimeChan.Init()

	chs.CurExec = &consensus.CurProposal{
		Node:          nil,
		DocumentHash:  nil,
		PrepareVote:   make([]*tcrsa.SigShare, 0),
		HighQC:        make([]*pb.QuorumCert, 0),
	}
	privateKey, err := go_hotstuff.ReadThresholdPrivateKeyFromFile(chs.GetSelfInfo().PrivateKey)
	if err != nil {
		logger.Fatal(err)
	}
	chs.Config.PrivateKey = privateKey
	chs.ProcessMethod = handleMethod
	ctx, cancel := context.WithCancel(context.Background())
	chs.cancel = cancel
	go chs.receiveMsg(ctx)
	return chs
}

func (chs *ChainedHotStuff) receiveMsg(ctx context.Context) {
	for {
		select {
		case msg := <-chs.MsgEntrance:
			chs.handleMsg(msg)
		case <-ctx.Done():
			return
		}
	}
}

func (chs *ChainedHotStuff) handleMsg(msg *pb.Msg) {
	switch msg.Payload.(type) {
	case *pb.Msg_Request:
		request := msg.GetRequest()
		logger.Debugf("[HOTSTUFF] Get request msg, content:%s", request.String())
		// put the cmd into the cmdset
		chs.CmdSet.Add(request.Cmd)
		if chs.GetLeader() != chs.ID {
			// redirect to the leader
			chs.Unicast(chs.GetNetworkInfo()[chs.GetLeader()], msg)
			return
		}
		if chs.CurExec.Node != nil {
			return
		}
		chs.BatchTimeChan.SoftStartTimer()
		// if the length of unprocessed cmd equals to batch size, stop timer and call handleMsg to send prepare msg
		logger.Debugf("cmd set size: %d", len(chs.CmdSet.GetFirst(int(chs.Config.BatchSize))))
		cmds := chs.CmdSet.GetFirst(int(chs.Config.BatchSize))
		if len(cmds) == int(chs.Config.BatchSize) {
			// stop timer
			chs.BatchTimeChan.Stop()
			// create prepare msg
			chs.batchEvent(cmds)
		}
		break
	case *pb.Msg_Prepare:
		break
	case *pb.Msg_NewView:
		break
	}
}

func (chs *ChainedHotStuff) update(block *pb.Block) {
	// block = b*, block1 = b'', block2 = b', block3 = b
	block1, err := chs.BlockStorage.BlockOf(block.Justify)
	if err != nil {
		logger.Fatal(err)
	}
	if block1 == nil || block1.Committed {
		return
	}
	if bytes.Equal(block.ParentHash, block1.Hash) {
		chs.genericQC = block.Justify
	}
	
	block2, err := chs.BlockStorage.BlockOf(block1.Justify)
	if err != nil {
		logger.Fatal(err)
	}
	if block2 == nil || block2.Committed {
		return
	}
	if bytes.Equal(block.ParentHash, block1.Hash) && bytes.Equal(block1.ParentHash, block2.Hash) {
		chs.lockQC = block1.Justify
	}

	block3, err := chs.BlockStorage.BlockOf(block2.Justify)
	if err != nil {
		logger.Fatal(err)
	}
	if block3 == nil || block3.Committed {
		return
	}
	if bytes.Equal(block.ParentHash, block1.Hash) && bytes.Equal(block1.ParentHash, block2.Hash) &&
		bytes.Equal(block2.ParentHash, block3.Hash) {
		//decide
		chs.processProposal()
	}
}

func (chs *ChainedHotStuff) SafeExit() {
	chs.cancel()
	close(chs.MsgEntrance)
	chs.BlockStorage.Close()
}

func (chs *ChainedHotStuff) batchEvent(cmds []string) {
	// if batch timeout, check size
	if len(cmds) == 0 {
		chs.BatchTimeChan.SoftStartTimer()
		return
	}
	// create prepare msg
	node := chs.CreateLeaf(chs.BlockStorage.GetLastBlockHash(), cmds, nil)
	chs.CurExec.Node = node
	chs.CmdSet.MarkProposed(cmds...)
	if chs.HighQC == nil {
		chs.HighQC = chs.PrepareQC
	}
	prepareMsg := chs.Msg(pb.MsgType_PREPARE, node, chs.HighQC)
	// vote self
	marshal, _ := proto.Marshal(prepareMsg)
	chs.CurExec.DocumentHash, _ = go_hotstuff.CreateDocumentHash(marshal, chs.Config.PublicKey)
	partSig, _ := go_hotstuff.TSign(chs.CurExec.DocumentHash, chs.Config.PrivateKey, chs.Config.PublicKey)
	chs.CurExec.PrepareVote = append(chs.CurExec.PrepareVote, partSig)
	// broadcast prepare msg
	chs.Broadcast(prepareMsg)
	chs.TimeChan.SoftStartTimer()
}

func (chs *ChainedHotStuff) processProposal() {
	// process proposal
	go chs.ProcessProposal(chs.CurExec.Node.Commands)
	// store block
	chs.CurExec.Node.Committed = true
}