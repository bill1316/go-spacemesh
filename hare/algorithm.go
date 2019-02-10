package hare

import (
	"encoding/binary"
	"errors"
	"github.com/gogo/protobuf/proto"
	"github.com/spacemeshos/go-spacemesh/hare/config"
	"github.com/spacemeshos/go-spacemesh/hare/pb"
	"github.com/spacemeshos/go-spacemesh/log"
	"github.com/spacemeshos/go-spacemesh/p2p/service"
	"hash/fnv"
	"time"
)

const ProtoName = "HARE_PROTOCOL"

type Byteable interface {
	Bytes() []byte
}

type NetworkService interface {
	RegisterGossipProtocol(protocol string) chan service.GossipMessage
	Broadcast(protocol string, payload []byte) error
}

type procOutput struct {
	id  InstanceId
	set *Set
}

func (cpo procOutput) Id() []byte {
	return cpo.id.Bytes()
}

func (cpo procOutput) Set() *Set {
	return cpo.set
}

var _ TerminationOutput = (*procOutput)(nil)

type State struct {
	k           uint32          // the round counter (r%4 is the round number)
	ki          int32           // indicates when S was first committed upon
	s           *Set            // the set of values
	certificate *pb.Certificate // the certificate
}

type ConsensusProcess struct {
	State
	Closer            // the consensus is closeable
	instanceId        InstanceId
	oracle            Rolacle // roles oracle
	signing           Signing
	network           NetworkService
	startTime         time.Time // TODO: needed?
	inbox             chan Message
	terminationReport chan TerminationOutput
	validator         messageValidator
	preRoundTracker   *PreRoundTracker
	statusesTracker   *StatusTracker
	proposalTracker   proposalTracker
	commitTracker     commitTracker
	notifyTracker     *NotifyTracker
	terminating       bool
	cfg               config.Config
	notifySent        bool
	pending           map[string]Message
}

func NewConsensusProcess(cfg config.Config, instanceId InstanceId, s *Set, oracle Rolacle, signing Signing, p2p NetworkService, terminationReport chan TerminationOutput) *ConsensusProcess {
	proc := &ConsensusProcess{}
	proc.State = State{0, -1, s.Clone(), nil}
	proc.Closer = NewCloser()
	proc.instanceId = instanceId
	proc.oracle = oracle
	proc.signing = signing
	proc.network = p2p
	proc.validator = NewMessageValidator(signing, cfg.F+1, cfg.N, proc.statusValidator())
	proc.preRoundTracker = NewPreRoundTracker(cfg.F+1, cfg.N)
	proc.statusesTracker = NewStatusTracker(cfg.F+1, cfg.N)
	proc.proposalTracker = NewProposalTracker(cfg.N)
	proc.commitTracker = NewCommitTracker(cfg.F+1, cfg.N, nil)
	proc.notifyTracker = NewNotifyTracker(cfg.N)
	proc.terminating = false
	proc.cfg = cfg
	proc.notifySent = false
	proc.terminationReport = terminationReport
	proc.pending = make(map[string]Message, cfg.N)

	return proc
}

func (proc *ConsensusProcess) Id() uint32 {
	return proc.instanceId.Id()
}

// Returns the iteration number from a given round counter
func iterationFromCounter(roundCounter uint32) uint32 {
	return roundCounter / 4
}

func (proc *ConsensusProcess) Start() error {
	if !proc.startTime.IsZero() { // called twice on same instance
		log.Error("ConsensusProcess has already been started.")
		return StartInstanceError(errors.New("instance already started"))
	}

	proc.startTime = time.Now()

	go proc.eventLoop()

	return nil
}

func (proc *ConsensusProcess) createInbox(size uint32) chan Message {
	proc.inbox = make(chan Message, size)
	return proc.inbox
}

func (proc *ConsensusProcess) eventLoop() {
	log.Info("Start listening for instance %v ", proc.instanceId.Bytes())

	// set pre-round message and send
	m := proc.initDefaultBuilder(proc.s).SetType(PreRound).Sign(proc.signing).Build()
	proc.sendMessage(m)

	// listen to pre-round messages
	timer := time.NewTimer(proc.cfg.RoundDuration)
PreRound:
	for {
		select {
		case msg := <-proc.inbox:
			proc.handleMessage(msg)
		case <-timer.C:
			break PreRound
		case <-proc.CloseChannel():
			return
		}
	}
	proc.preRoundTracker.FilterSet(proc.s)
	if proc.s.Size() == 0 {
		log.Error("Fatal: PreRound ended with empty set")
	}

	// start first iteration
	proc.onRoundBegin()
	ticker := time.NewTicker(proc.cfg.RoundDuration)
	for {
		select {
		case msg := <-proc.inbox: // msg event
			proc.handleMessage(msg)
			if proc.terminating {
				log.Info("Detected terminating on. Exiting.")
				return
			}
		case <-ticker.C: // next round event
			proc.onRoundEnd()
			proc.advanceToNextRound()
			proc.onRoundBegin()
		case <-proc.CloseChannel(): // close event
			log.Info("Stop event loop, terminating")
			return
		}
	}
}

func roleFromRoundCounter(k uint32) Role {
	switch k % 4 {
	case Round2:
		return Leader
	case Round4:
		return Passive
	default:
		return Active
	}
}

func (proc *ConsensusProcess) onEarlyMessage(m Message) {
	if m.msg == nil {
		log.Error("onEarlyMessage called with nil")
		return
	}

	verifier, err := NewVerifier(m.msg.PubKey)
	if err != nil {
		log.Warning("Could not construct verifier: ", err)
		return
	}

	if _, exist := proc.pending[verifier.String()]; exist { // ignore, already received
		log.Warning("Already received message from sender %v", verifier.String())
		return
	}

	proc.pending[verifier.String()] = m
}

func (proc *ConsensusProcess) expectedCommitteeSize(k uint32) int {
	if k%4 == Round2 {
		return 1 // 5 leaders
	}

	// N actives
	return proc.cfg.N
}

func hashInstanceAndK(instanceID InstanceId, K uint32) uint32 {
	h := newHasherU32()
	val := h.Hash(append(instanceID.Bytes(), byte(K)))
	return val
}

func (proc *ConsensusProcess) validateRole(m *pb.HareMessage) bool {
	if m == nil {
		log.Error("validateRole called with nil")
		return false
	}

	if m.Message == nil {
		log.Warning("Role validation failed: message is nil")
		return false
	}

	// TODO: validate role proof sig

	verifier, err := NewVerifier(m.PubKey)
	if err != nil {
		log.Error("Could not build verifier")
		return false
	}

	// validate role
	if !proc.oracle.Eligible(hashInstanceAndK(proc.instanceId, m.Message.K), proc.expectedCommitteeSize(m.Message.K), verifier.String(), Signature(m.Message.RoleProof)) {
		log.Warning("Role validation failed")
		return false
	}

	return true
}

func (proc *ConsensusProcess) handleMessage(msg Message) {
	// Note: instanceId is already verified by the broker
	m := msg.msg
	// first validate role
	if !proc.validateRole(m) {
		log.Warning("Role validation failed, pubkey %v", m.PubKey)
		msg.reportValidationResult(false)
		return
	}

	if !proc.validator.SyntacticallyValidateMessage(m) {
		log.Warning("Syntactically validation failed, pubkey %v", m.PubKey)
		msg.reportValidationResult(false)
		return
	}

	// Not including the contextual validity in the report since it might have different results between two different peers
	msg.reportValidationResult(true)

	// validate message for this or next round
	if !proc.validator.ContextuallyValidateMessage(m, proc.k) {
		if !proc.validator.ContextuallyValidateMessage(m, proc.k+1) {
			// TODO: should return error from message validation to indicate what failed, should retry only for contextual failure
			log.Warning("Message is not valid for either round, pubkey %v", m.PubKey)
			return
		} else { // a valid early message, keep it for later
			log.Info("Early message detected. Keeping message, pubkey %v", m.PubKey)
			proc.onEarlyMessage(msg)
			return
		}
	}

	// continue process msg by type
	proc.processMsg(m)
}

func (proc *ConsensusProcess) processMsg(m *pb.HareMessage) {
	switch MessageType(m.Message.Type) {
	case PreRound:
		proc.processPreRoundMsg(m)
	case Status: // end of round 1
		proc.processStatusMsg(m)
	case Proposal: // end of round 2
		proc.processProposalMsg(m)
	case Commit: // end of round 3
		proc.processCommitMsg(m)
	case Notify: // end of round 4
		proc.processNotifyMsg(m)
	default:
		log.Warning("Unknown message type: %v , pubkey %v", m.Message.Type, m.PubKey)
	}
}

func (proc *ConsensusProcess) sendMessage(msg *pb.HareMessage) {
	// invalid msg
	if msg == nil {
		return
	}

	// check eligibility
	if !proc.isEligible() {
		log.Info("%v not eligible round %v", proc.signing.Verifier().String(), proc.k)
		return
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		log.Error("failed marshaling message")
		panic("could not marshal message before send")
	}

	if err := proc.network.Broadcast(ProtoName, data); err != nil {
		log.Error("Could not broadcast round message ", err.Error())
		return
	}

}

func (proc *ConsensusProcess) onRoundEnd() {
	log.Info("End of round: %d", proc.k)

	// reset trackers
	switch proc.currentRound() {
	case Round1:
		proc.endOfRound1()
	case Round3:
		proc.endOfRound3()
	}
}

func (proc *ConsensusProcess) advanceToNextRound() {
	proc.k++
}

func (proc *ConsensusProcess) beginRound1() {
	proc.statusesTracker = NewStatusTracker(proc.cfg.F+1, proc.cfg.N)
	statusMsg := proc.initDefaultBuilder(proc.s).SetType(Status).Sign(proc.signing).Build()
	proc.sendMessage(statusMsg)
}

func (proc *ConsensusProcess) beginRound2() {
	proc.proposalTracker = NewProposalTracker(proc.cfg.N)

	if proc.isEligible() && proc.statusesTracker.IsSVPReady() {
		builder := proc.initDefaultBuilder(proc.statusesTracker.ProposalSet(proc.cfg.SetSize))
		svp := proc.statusesTracker.BuildSVP()
		if svp != nil {
			proposalMsg := builder.SetType(Proposal).SetSVP(svp).Sign(proc.signing).Build()
			proc.sendMessage(proposalMsg)
		} else {
			log.Error("Failed to build SVP (nil) after verifying SVP is ready ")
		}
	}

	// done with building proposal, reset statuses tracking
	proc.statusesTracker = nil
}

func (proc *ConsensusProcess) beginRound3() {
	proposedSet := proc.proposalTracker.ProposedSet()

	// proposedSet may be nil, in such case the tracker will ignore messages
	proc.commitTracker = NewCommitTracker(proc.cfg.F+1, proc.cfg.N, proposedSet) // track commits for proposed set

	if proposedSet != nil { // has proposal to send
		builder := proc.initDefaultBuilder(proposedSet).SetType(Commit).Sign(proc.signing)
		commitMsg := builder.Build()
		proc.sendMessage(commitMsg)
	}

	// proposedSet may be nil, in such case the tracker will ignore messages
	proc.commitTracker = NewCommitTracker(proc.cfg.F+1, proc.cfg.N, proposedSet) // track commits for proposed set
}

func (proc *ConsensusProcess) beginRound4() {
	proc.commitTracker = nil
	proc.proposalTracker = nil
}

func (proc *ConsensusProcess) handlePending(pending map[string]Message) {
	for _, m := range pending {
		proc.inbox <- m
	}
}

func (proc *ConsensusProcess) onRoundBegin() {
	// reset trackers
	switch proc.currentRound() {
	case Round1:
		proc.beginRound1()
	case Round2:
		proc.beginRound2()
	case Round3:
		proc.beginRound3()
	case Round4:
		proc.beginRound4()
	default:
		log.Error("Current round out of bounds. Expected: 0-4, Found: ", proc.currentRound())
		panic("Current round out of bounds")
	}

	pendingProcess := proc.pending
	proc.pending = make(map[string]Message, proc.cfg.N)
	go proc.handlePending(pendingProcess)
}

func (proc *ConsensusProcess) roleProof() Signature {
	kInBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(kInBytes, uint32(proc.k))
	hash := fnv.New32()
	hash.Write(proc.signing.Verifier().Bytes())
	hash.Write(kInBytes)

	hashBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(hashBytes, uint32(hash.Sum32()))

	return proc.signing.Sign(hashBytes)
}

func (proc *ConsensusProcess) initDefaultBuilder(s *Set) *MessageBuilder {
	builder := NewMessageBuilder().SetPubKey(proc.signing.Verifier().Bytes()).SetInstanceId(proc.instanceId)
	builder = builder.SetRoundCounter(proc.k).SetKi(proc.ki).SetValues(s)
	builder.SetRoleProof(proc.roleProof())

	return builder
}

func (proc *ConsensusProcess) processPreRoundMsg(msg *pb.HareMessage) {
	proc.preRoundTracker.OnPreRound(msg)
}

func (proc *ConsensusProcess) processStatusMsg(msg *pb.HareMessage) {
	// record status
	proc.statusesTracker.RecordStatus(msg)
}

func (proc *ConsensusProcess) processProposalMsg(msg *pb.HareMessage) {
	if proc.currentRound() == Round2 { // regular proposal
		proc.proposalTracker.OnProposal(msg)
	} else { // late proposal
		proc.proposalTracker.OnLateProposal(msg)
	}
}

func (proc *ConsensusProcess) processCommitMsg(msg *pb.HareMessage) {
	proc.commitTracker.OnCommit(msg)
}

func (proc *ConsensusProcess) processNotifyMsg(msg *pb.HareMessage) {
	s := NewSet(msg.Message.Values)

	if ignored := proc.notifyTracker.OnNotify(msg); ignored {
		log.Warning("Ignoring notification sent from %v", msg.PubKey)
		return
	}

	if proc.currentRound() == Round4 { // not necessary to update otherwise
		// we assume that this expression was checked before
		if int32(msg.Cert.AggMsgs.Messages[0].Message.K) >= proc.ki { // update state iff K >= ki
			proc.s = s
			proc.certificate = msg.Cert
			proc.ki = msg.Message.Ki
		}
	}

	if proc.notifyTracker.NotificationsCount(s) < proc.cfg.F+1 { // not enough
		//log.Info("Not enough notifications for termination. Expected: %v Actual: %v",
		//	proc.cfg.F+1, proc.notifyTracker.NotificationsCount(s))
		return
	}

	// enough notifications, should terminate
	proc.s = s // update to the agreed set
	log.Info("Consensus process terminated for %v with output set: ", proc.signing.Verifier().Bytes(), proc.s)
	proc.terminationReport <- procOutput{proc.instanceId, proc.s}
	proc.Close()
	proc.terminating = true // ensures immediate termination
}

func (proc *ConsensusProcess) currentRound() int {
	return int(proc.k % 4)
}

func (proc *ConsensusProcess) statusValidator() func(m *pb.HareMessage) bool {
	validate := func(m *pb.HareMessage) bool {
		s := NewSet(m.Message.Values)
		if m.Message.Ki == -1 { // no certificates, validate by pre-round msgs
			if proc.preRoundTracker.CanProveSet(s) { // can prove s
				return true
			}
		} else { // ki>=0, we should have received a certificate for that set
			if proc.notifyTracker.HasCertificate(m.Message.Ki, s) { // can prove s
				return true
			}
		}
		return false
	}

	return validate
}

func (proc *ConsensusProcess) endOfRound1() {
	proc.statusesTracker.AnalyzeStatuses(proc.statusValidator())
}

func (proc *ConsensusProcess) endOfRound3() {
	// notify already sent after committing, only one should be sent
	if proc.notifySent {
		return
	}

	if proc.proposalTracker.IsConflicting() {
		return
	}

	if !proc.commitTracker.HasEnoughCommits() {
		return
	}

	cert := proc.commitTracker.BuildCertificate()
	if cert == nil {
		log.Error("Build certificate returned nil")
		return
	}

	s := proc.proposalTracker.ProposedSet()
	if s == nil {
		return
	}

	// commit & send notification message
	proc.s = s
	proc.certificate = cert
	builder := proc.initDefaultBuilder(proc.s).SetType(Notify).SetCertificate(proc.certificate).Sign(proc.signing)
	notifyMsg := builder.Build()
	proc.sendMessage(notifyMsg)
	proc.notifySent = true
}

func (proc *ConsensusProcess) isEligible() bool {
	return proc.currentRole() != Passive
}

// Returns the role matching the current round if eligible for this round, false otherwise
func (proc *ConsensusProcess) currentRole() Role {
	if proc.oracle.Eligible(hashInstanceAndK(proc.instanceId, proc.k), proc.expectedCommitteeSize(proc.k), proc.signing.Verifier().String(), proc.roleProof()) {
		if proc.currentRound() == Round2 {
			return Leader
		}
		return Active
	}

	return Passive
}
