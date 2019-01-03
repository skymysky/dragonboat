// Copyright 2017-2019 Lei Ni (nilei81@gmail.com)
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/*
Package raft is a distributed consensus package that implements the Raft
protocol.

This package is internally used by Dragonboat, applications are not expected
to import this package.
*/
package raft

import (
	"fmt"
	"math"
	"sort"

	"github.com/lni/dragonboat/config"
	"github.com/lni/dragonboat/internal/settings"
	"github.com/lni/dragonboat/internal/utils/logutil"
	"github.com/lni/dragonboat/internal/utils/random"
	"github.com/lni/dragonboat/logger"
	pb "github.com/lni/dragonboat/raftpb"
)

var (
	plog = logger.GetLogger("raft")
)

const (
	// NoLeader is the flag used to indcate that there is no leader or the leader
	// is unknown.
	NoLeader uint64 = 0
	// NoNode is the flag used to indicate that the node id field is not set.
	NoNode          uint64 = 0
	noLimit         uint64 = math.MaxUint64
	numMessageTypes uint64 = 25
)

var (
	emptyState = pb.State{}
)

// State is the state of a raft node defined in the raft paper, possible states
// are leader, follower, candidate and observer. Observer is non-voting member
// node.
type State uint64

const (
	follower State = iota
	candidate
	leader
	observer
	numStates
)

var stateNames = [...]string{
	"Follower",
	"Candidate",
	"Leader",
	"Observer",
}

func (st State) String() string {
	return stateNames[uint64(st)]
}

// NodeID returns a human friendly form of NodeID for logging purposes.
func NodeID(nodeID uint64) string {
	return logutil.NodeID(nodeID)
}

// ClusterID returns a human friendly form of ClusterID for logging purposes.
func ClusterID(clusterID uint64) string {
	return logutil.ClusterID(clusterID)
}

type handlerFunc func(pb.Message)
type stepFunc func(*raft, pb.Message)

// Status is the struct that captures the status of a raft node.
type Status struct {
	NodeID    uint64
	ClusterID uint64
	Applied   uint64
	LeaderID  uint64
	NodeState State
	pb.State
}

// IsLeader returns a boolean value indicating whether the node is leader.
func (s *Status) IsLeader() bool {
	return s.NodeState == leader
}

// IsFollower returns a boolean value indicating whether the node is a follower.
func (s *Status) IsFollower() bool {
	return s.NodeState == follower
}

// getLocalStatus gets a copy of the current raft status.
func getLocalStatus(r *raft) Status {
	return Status{
		NodeID:    r.nodeID,
		ClusterID: r.clusterID,
		NodeState: r.state,
		Applied:   r.log.applied,
		LeaderID:  r.leaderID,
		State:     r.raftState(),
	}
}

type raft struct {
	applied                   uint64
	nodeID                    uint64
	clusterID                 uint64
	term                      uint64
	vote                      uint64
	log                       *entryLog
	remotes                   map[uint64]*remote
	observers                 map[uint64]*remote
	state                     State
	votes                     map[uint64]bool
	msgs                      []pb.Message
	leaderID                  uint64
	leaderTransferTarget      uint64
	isLeaderTransferTarget    bool
	pendingConfigChange       bool
	readIndex                 *readIndex
	readyToRead               []pb.ReadyToRead
	checkQuorum               bool
	tickCount                 uint64
	electionTick              uint64
	heartbeatTick             uint64
	heartbeatTimeout          uint64
	electionTimeout           uint64
	randomizedElectionTimeout uint64
	handlers                  [numStates][numMessageTypes]handlerFunc
	handle                    stepFunc
	matched                   []uint64
	hasNotAppliedConfigChange func() bool
	recordLeader              func(uint64)
}

func newRaft(c *config.Config, logdb ILogDB) *raft {
	if err := c.Validate(); err != nil {
		panic(err)
	}
	if logdb == nil {
		panic("logdb is nil")
	}
	r := &raft{
		clusterID:        c.ClusterID,
		nodeID:           c.NodeID,
		leaderID:         NoLeader,
		msgs:             make([]pb.Message, 0),
		log:              newEntryLog(logdb),
		remotes:          make(map[uint64]*remote),
		observers:        make(map[uint64]*remote),
		electionTimeout:  c.ElectionRTT,
		heartbeatTimeout: c.HeartbeatRTT,
		checkQuorum:      c.CheckQuorum,
		readIndex:        newReadIndex(),
	}
	st, members := logdb.NodeState()
	for p := range members.Addresses {
		r.remotes[p] = &remote{
			next: 1,
		}
	}
	for p := range members.Observers {
		r.observers[p] = &remote{
			next: 1,
		}
	}
	r.resetMatchValueArray()
	if !pb.IsEmptyState(st) {
		r.loadState(st)
	}
	if c.IsObserver {
		r.state = observer
		r.becomeObserver(r.term, NoLeader)
	} else {
		r.becomeFollower(r.term, NoLeader)
	}
	r.initializeHandlerMap()
	r.checkHandlerMap()
	r.handle = defaultHandle
	return r
}

func (r *raft) setTestPeers(peers []uint64) {
	if len(r.remotes) == 0 {
		for _, p := range peers {
			r.remotes[p] = &remote{next: 1}
		}
	}
}

func (r *raft) setApplied(applied uint64) {
	r.applied = applied
}

func (r *raft) getApplied() uint64 {
	return r.applied
}

func (r *raft) resetMatchValueArray() {
	r.matched = make([]uint64, len(r.remotes))
}

func (r *raft) describe() string {
	li := r.log.lastIndex()
	t, err := r.log.term(li)
	if err != nil && err != ErrCompacted {
		panic(err)
	}
	fmtstr := "[f-idx:%d,l-idx:%d,logterm:%d,commit:%d,applied:%d] %s term %d"
	return fmt.Sprintf(fmtstr,
		r.log.firstIndex(), r.log.lastIndex(), t, r.log.committed,
		r.log.applied, logutil.DescribeNode(r.clusterID, r.nodeID), r.term)
}

func (r *raft) isObserver() bool {
	return r.state == observer
}

func (r *raft) setLeaderID(leaderID uint64) {
	r.leaderID = leaderID
	if r.recordLeader != nil {
		r.recordLeader(r.leaderID)
	}
}

func (r *raft) leaderTransfering() bool {
	return r.leaderTransferTarget != NoNode && r.state == leader
}

func (r *raft) abortLeaderTransfer() {
	r.leaderTransferTarget = NoNode
}

func (r *raft) quorum() int {
	return len(r.remotes)/2 + 1
}

func (r *raft) isSingleNodeQuorum() bool {
	return r.quorum() == 1
}

func (r *raft) leaderHasQuorum() bool {
	c := 0
	for nid := range r.remotes {
		if nid == r.nodeID || r.remotes[nid].isActive() {
			c++
			r.remotes[nid].setNotActive()
		}
	}
	return c >= r.quorum()
}

func (r *raft) nodes() []uint64 {
	nodes := make([]uint64, 0, len(r.remotes)+len(r.observers))
	for id := range r.remotes {
		nodes = append(nodes, id)
	}
	for id := range r.observers {
		nodes = append(nodes, id)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i] < nodes[j] })
	return nodes
}

func (r *raft) raftState() pb.State {
	return pb.State{
		Term:   r.term,
		Vote:   r.vote,
		Commit: r.log.committed,
	}
}

func (r *raft) loadState(st pb.State) {
	if st.Commit < r.log.committed || st.Commit > r.log.lastIndex() {
		plog.Panicf("%s got out of range state, st.commit %d, range[%d,%d]",
			r.describe(), st.Commit, r.log.committed, r.log.lastIndex())
	}
	r.log.committed = st.Commit
	r.term = st.Term
	r.vote = st.Vote
}

func (r *raft) restore(ss pb.Snapshot) bool {
	if ss.Index <= r.log.committed {
		plog.Infof("%s, ss.Index <= committed", r.describe())
		return false
	}
	if !r.isObserver() {
		for nid := range ss.Membership.Observers {
			if nid == r.nodeID {
				plog.Panicf("%s converting to observer, index %d, committed %d, %+v",
					r.describe(), ss.Index, r.log.committed, ss)
			}
		}
	}
	// TODO (lni):  check this
	if r.log.matchTerm(ss.Index, ss.Term) {
		r.log.commitTo(ss.Index)
		return false
	}
	plog.Infof("%s starts to restore snapshot index %d term %d",
		r.describe(), ss.Index, ss.Term)
	r.log.restore(ss)
	return true
}

func (r *raft) restoreRemotes(ss pb.Snapshot) {
	r.remotes = make(map[uint64]*remote)
	for id := range ss.Membership.Addresses {
		_, ok := r.observers[id]
		if ok {
			r.becomeFollower(r.term, r.leaderID)
		}
		match := uint64(0)
		next := r.log.lastIndex() + 1
		if id == r.nodeID {
			match = next - 1
		}
		r.setRemote(id, match, next)
		plog.Infof("%s restored remote progress of %s [%s]",
			r.describe(), NodeID(id), r.remotes[id])
	}
	r.observers = make(map[uint64]*remote)
	for id := range ss.Membership.Observers {
		match := uint64(0)
		next := r.log.lastIndex() + 1
		if id == r.nodeID {
			match = next - 1
		}
		r.setObserver(id, match, next)
		plog.Infof("%s restored observer progress of %s [%s]",
			r.describe(), NodeID(id), r.observers[id])
	}
	r.resetMatchValueArray()
}

//
// tick related functions
//

func (r *raft) timeForElection() bool {
	return r.electionTick >= r.randomizedElectionTimeout
}

func (r *raft) timeForHearbeat() bool {
	return r.heartbeatTick >= r.heartbeatTimeout
}

func (r *raft) timeForCheckQuorum() bool {
	return r.electionTick >= r.electionTimeout
}

func (r *raft) timeToAbortLeaderTransfer() bool {
	return r.leaderTransfering() && r.electionTick >= r.electionTimeout
}

func (r *raft) tick() {
	r.tickCount++
	if r.state == leader {
		r.leaderTick()
	} else {
		r.nonLeaderTick()
	}
}

func (r *raft) nonLeaderTick() {
	if r.state == leader {
		panic("noleader tick called on leader node")
	}
	r.electionTick++
	if r.isObserver() {
		return
	}
	if !r.selfRemoved() && r.timeForElection() {
		r.electionTick = 0
		r.Handle(pb.Message{
			From: r.nodeID,
			Type: pb.Election,
		})
	}
}

func (r *raft) leaderTick() {
	if r.state != leader {
		panic("leaderTick called on a non-leader node")
	}
	r.electionTick++
	timeToAbortLeaderTransfer := r.timeToAbortLeaderTransfer()
	if r.timeForCheckQuorum() {
		r.electionTick = 0
		if r.checkQuorum {
			r.Handle(pb.Message{
				From: r.nodeID,
				Type: pb.CheckQuorum,
			})
		}
	}
	if timeToAbortLeaderTransfer {
		r.abortLeaderTransfer()
	}
	r.heartbeatTick++
	if r.timeForHearbeat() {
		r.heartbeatTick = 0
		r.Handle(pb.Message{
			From: r.nodeID,
			Type: pb.LeaderHeartbeat,
		})
	}
}

func (r *raft) quiescedTick() {
	r.electionTick++
}

func (r *raft) setRandomizedElectionTimeout() {
	randTime := random.LockGuardedRand.Uint64() % r.electionTimeout
	r.randomizedElectionTimeout = r.electionTimeout + randTime
}

//
// send and broadcast functions
//

func (r *raft) finalizeMessageTerm(m pb.Message) pb.Message {
	if m.Term == 0 && m.Type == pb.RequestVote {
		plog.Panicf("sending RequestVote with 0 term")
	}
	if m.Term > 0 && m.Type != pb.RequestVote {
		plog.Panicf("term unexpectedly set for message type %d", m.Type)
	}
	if !isRequestMessage(m.Type) {
		m.Term = r.term
	}
	return m
}

func (r *raft) send(m pb.Message) {
	m.From = r.nodeID
	m = r.finalizeMessageTerm(m)
	r.msgs = append(r.msgs, m)
}

func (r *raft) makeInstallSnapshotMessage(to uint64, m *pb.Message) uint64 {
	m.To = to
	m.Type = pb.InstallSnapshot
	snapshot := r.log.snapshot()
	if pb.IsEmptySnapshot(snapshot) {
		panic("got an empty snapshot")
	}
	m.Snapshot = snapshot
	return snapshot.Index
}

func (r *raft) makeReplicateMessage(to uint64,
	next uint64, maxSize uint64) (pb.Message, error) {
	term, err := r.log.term(next - 1)
	if err != nil {
		return pb.Message{}, err
	}
	entries, err := r.log.entries(next, maxSize)
	if err != nil {
		return pb.Message{}, err
	}
	if len(entries) > 0 {
		if entries[len(entries)-1].Index != next-1+uint64(len(entries)) {
			plog.Panicf("expected last index in Replicate %d, got %d",
				next-1+uint64(len(entries)), entries[len(entries)-1].Index)
		}
	}
	return pb.Message{
		To:       to,
		Type:     pb.Replicate,
		LogIndex: next - 1,
		LogTerm:  term,
		Entries:  entries,
		Commit:   r.log.committed,
	}, nil
}

func (r *raft) sendReplicateMessage(to uint64) {
	var rp *remote
	if v, ok := r.remotes[to]; ok {
		rp = v
	} else {
		rp, ok = r.observers[to]
		if !ok {
			panic("failed to get the remote instance")
		}
	}
	if rp.isPaused() {
		return
	}
	m, err := r.makeReplicateMessage(to, rp.next, settings.Soft.MaxEntrySize)
	if err != nil {
		// log not available due to compaction, send snapshot
		if !rp.isActive() {
			plog.Warningf("node %s is not active, sending snapshot is skipped",
				NodeID(to))
			return
		}
		index := r.makeInstallSnapshotMessage(to, &m)
		plog.Infof("%s is sending snapshot (%d) to %s, r.Next %d, r.Match %d, %v",
			r.describe(), index, NodeID(to), rp.next, rp.match, err)
		rp.becomeSnapshot(index)
	} else {
		if len(m.Entries) > 0 {
			lastIndex := m.Entries[len(m.Entries)-1].Index
			rp.progress(lastIndex)
		}
	}
	r.send(m)
}

func (r *raft) broadcastReplicateMessage() {
	for nid := range r.remotes {
		if nid != r.nodeID {
			r.sendReplicateMessage(nid)
		}
	}
	for nid := range r.observers {
		if nid == r.nodeID {
			panic("observer is trying to broadcast Replicate msg")
		}
		r.sendReplicateMessage(nid)
	}
}

func (r *raft) sendHeartbeatMessage(to uint64,
	hint pb.SystemCtx, toObserver bool) {
	var match uint64
	if toObserver {
		match = r.observers[to].match
	} else {
		match = r.remotes[to].match
	}
	commit := min(match, r.log.committed)
	r.send(pb.Message{
		To:       to,
		Type:     pb.Heartbeat,
		Commit:   commit,
		Hint:     hint.Low,
		HintHigh: hint.High,
	})
}

func (r *raft) broadcastHeartbeatMessage() {
	if r.readIndex.hasPendingRequest() {
		ctx := r.readIndex.peepCtx()
		r.broadcastHeartbeatMessageWithHint(ctx)
	} else {
		r.broadcastHeartbeatMessageWithHint(pb.SystemCtx{})
	}
}

func (r *raft) broadcastHeartbeatMessageWithHint(ctx pb.SystemCtx) {
	zeroCtx := pb.SystemCtx{}
	for id := range r.remotes {
		if id != r.nodeID {
			r.sendHeartbeatMessage(id, ctx, false)
		}
	}
	if ctx == zeroCtx {
		for id := range r.observers {
			r.sendHeartbeatMessage(id, zeroCtx, true)
		}
	}
}

func (r *raft) sendTimeoutNowMessage(target uint64) {
	r.send(pb.Message{
		Type: pb.TimeoutNow,
		To:   target,
	})
}

//
// log append and commit
//

func (r *raft) sortMatchValues() {
	// unrolled bubble sort, sort.Slice is not allocation free
	if len(r.matched) == 3 {
		if r.matched[0] > r.matched[1] {
			v := r.matched[0]
			r.matched[0] = r.matched[1]
			r.matched[1] = v
		}
		if r.matched[1] > r.matched[2] {
			v := r.matched[1]
			r.matched[1] = r.matched[2]
			r.matched[2] = v
		}
		if r.matched[0] > r.matched[1] {
			v := r.matched[0]
			r.matched[0] = r.matched[1]
			r.matched[1] = v
		}
	} else {
		sort.Slice(r.matched, func(i, j int) bool {
			return r.matched[i] < r.matched[j]
		})
	}
}

func (r *raft) tryCommit() bool {
	if len(r.remotes) != len(r.matched) {
		r.resetMatchValueArray()
	}
	idx := 0
	for _, v := range r.remotes {
		r.matched[idx] = v.match
		idx++
	}
	r.sortMatchValues()
	q := r.matched[len(r.remotes)-r.quorum()]
	// see p8 raft paper
	// "Raft never commits log entries from previous terms by counting replicas.
	// Only log entries from the leader’s current term are committed by counting
	// replicas"
	return r.log.tryCommit(q, r.term)
}

func (r *raft) appendEntries(entries []pb.Entry) {
	lastIndex := r.log.lastIndex()
	for i := range entries {
		entries[i].Term = r.term
		entries[i].Index = lastIndex + 1 + uint64(i)
	}
	r.log.append(entries)
	r.remotes[r.nodeID].tryUpdate(r.log.lastIndex())
	if r.isSingleNodeQuorum() {
		r.tryCommit()
	}
}

//
// state transition related functions
//

func (r *raft) becomeObserver(term uint64, leaderID uint64) {
	if r.state != observer {
		panic("transitioning to observer state from non-observer")
	}
	r.reset(term)
	r.setLeaderID(leaderID)
	plog.Infof("%s became an observer", r.describe())
}

func (r *raft) becomeFollower(term uint64, leaderID uint64) {
	r.state = follower
	r.reset(term)
	r.setLeaderID(leaderID)
	plog.Infof("%s became a follower", r.describe())
}

func (r *raft) becomeCandidate() {
	if r.state == leader {
		panic("transitioning to candidate state from leader")
	}
	if r.state == observer {
		panic("observer is becoming candidate")
	}
	r.state = candidate
	r.reset(r.term + 1)
	r.vote = r.nodeID
	plog.Infof("%s became a candidate", r.describe())
}

func (r *raft) becomeLeader() {
	if r.state == follower {
		panic("transitioning to leader state from follower")
	}
	if r.state == observer {
		panic("observer is become leader")
	}
	r.state = leader
	r.reset(r.term)
	r.setLeaderID(r.nodeID)
	r.preLeaderPromotionHandleConfigChange()
	r.appendEntries([]pb.Entry{{Type: pb.ApplicationEntry, Cmd: nil}})
	plog.Infof("%s became the leader", r.describe())
}

func (r *raft) reset(term uint64) {
	if r.term != term {
		r.term = term
		r.vote = NoLeader
	}
	r.setLeaderID(NoLeader)
	r.votes = make(map[uint64]bool)
	r.electionTick = 0
	r.heartbeatTick = 0
	r.setRandomizedElectionTimeout()
	r.readIndex = newReadIndex()
	r.clearPendingConfigChange()
	r.abortLeaderTransfer()
	r.resetRemotes()
	r.resetObservers()
	r.resetMatchValueArray()
}

func (r *raft) preLeaderPromotionHandleConfigChange() {
	n := r.getPendingConfigChangeCount()
	if n > 1 {
		panic("multiple uncommitted config change entries")
	} else if n == 1 {
		plog.Infof("%s is becoming a leader with pending Config Change",
			r.describe())
		r.setPendingConfigChange()
	}
}

func (r *raft) resetRemotes() {
	for id := range r.remotes {
		r.remotes[id] = &remote{
			next: r.log.lastIndex() + 1,
		}
		if id == r.nodeID {
			r.remotes[id].match = r.log.lastIndex()
		}
	}
}

func (r *raft) resetObservers() {
	for id := range r.observers {
		r.observers[id] = &remote{
			next: r.log.lastIndex() + 1,
		}
		if id == r.nodeID {
			r.observers[id].match = r.log.lastIndex()
		}
	}
}

//
// election related functions
//

func (r *raft) handleVoteResp(from uint64, rejected bool) int {
	if rejected {
		plog.Infof("%s received RequestVoteResp rejection from %s at term %d",
			r.describe(), NodeID(from), r.term)
	} else {
		plog.Infof("%s received RequestVoteResp from %s at term %d",
			r.describe(), NodeID(from), r.term)
	}
	votedFor := 0
	if _, ok := r.votes[from]; !ok {
		r.votes[from] = !rejected
	}
	for _, v := range r.votes {
		if v {
			votedFor++
		}
	}
	return votedFor
}

func (r *raft) campaign() {
	plog.Infof("%s campaign called, remotes len: %d", r.describe(), len(r.remotes))
	r.becomeCandidate()
	term := r.term
	r.handleVoteResp(r.nodeID, false)
	if r.isSingleNodeQuorum() {
		r.becomeLeader()
		return
	}
	var hint uint64
	if r.isLeaderTransferTarget {
		hint = r.nodeID
		r.isLeaderTransferTarget = false
	}
	for k := range r.remotes {
		if k == r.nodeID {
			continue
		}
		r.send(pb.Message{
			Term:     term,
			To:       k,
			Type:     pb.RequestVote,
			LogIndex: r.log.lastIndex(),
			LogTerm:  r.log.lastTerm(),
			Hint:     hint,
		})
		plog.Infof("%s sent RequestVote to node %s", r.describe(), NodeID(k))
	}
}

//
// membership management
//

func (r *raft) selfRemoved() bool {
	if r.state == observer {
		_, ok := r.observers[r.nodeID]
		return !ok
	}
	_, ok := r.remotes[r.nodeID]
	return !ok
}

func (r *raft) addNode(nodeID uint64) {
	r.clearPendingConfigChange()
	if _, ok := r.remotes[nodeID]; ok {
		// already a voting member
		return
	}
	if rp, ok := r.observers[nodeID]; ok {
		// promoting to full member with inheriated progress info
		r.deleteObserver(nodeID)
		r.remotes[nodeID] = rp
		// local peer promoted, become follower
		if nodeID == r.nodeID {
			r.becomeFollower(r.term, r.leaderID)
		}
	} else {
		r.setRemote(nodeID, 0, r.log.lastIndex()+1)
	}
}

func (r *raft) addObserver(nodeID uint64) {
	r.clearPendingConfigChange()
	if _, ok := r.observers[nodeID]; ok {
		return
	}
	r.setObserver(nodeID, 0, r.log.lastIndex()+1)
}

func (r *raft) removeNode(nodeID uint64) {
	r.deleteRemote(nodeID)
	r.deleteObserver(nodeID)
	r.clearPendingConfigChange()
	if r.leaderTransfering() && r.leaderTransferTarget == nodeID {
		r.abortLeaderTransfer()
	}
	if len(r.remotes) > 0 {
		if r.tryCommit() {
			r.broadcastReplicateMessage()
		}
	}
}

func (r *raft) deleteRemote(nodeID uint64) {
	delete(r.remotes, nodeID)
	r.resetMatchValueArray()
}

func (r *raft) deleteObserver(nodeID uint64) {
	delete(r.observers, nodeID)
}

func (r *raft) setRemote(nodeID uint64, match uint64, next uint64) {
	plog.Infof("%s set remote, id %s, match %d, next %d",
		r.describe(), NodeID(nodeID), match, next)
	r.remotes[nodeID] = &remote{
		next:  next,
		match: match,
	}
	r.resetMatchValueArray()
}

func (r *raft) setObserver(nodeID uint64, match uint64, next uint64) {
	plog.Infof("%s set observer, id %s, match %d, next %d",
		r.describe(), NodeID(nodeID), match, next)
	r.observers[nodeID] = &remote{
		next:  next,
		match: match,
	}
}

func (r *raft) setPendingConfigChange() {
	r.pendingConfigChange = true
}

func (r *raft) hasPendingConfigChange() bool {
	return r.pendingConfigChange
}

func (r *raft) clearPendingConfigChange() {
	r.pendingConfigChange = false
}

func (r *raft) getPendingConfigChangeCount() int {
	idx := r.log.committed + 1
	count := 0
	for {
		ents, err := r.log.entries(idx, maxEntriesToApplySize)
		if err != nil {
			plog.Panicf("failed to get entries %v", err)
		}
		if len(ents) == 0 {
			return count
		}
		count += countConfigChange(ents)
		idx = ents[len(ents)-1].Index + 1
	}
}

//
// handler for various message types
//

func (r *raft) handleHeartbeatMessage(m pb.Message) {
	r.log.commitTo(m.Commit)
	r.send(pb.Message{
		To:       m.From,
		Type:     pb.HeartbeatResp,
		Hint:     m.Hint,
		HintHigh: m.HintHigh,
	})
}

func (r *raft) handleInstallSnapshotMessage(m pb.Message) {
	plog.Infof("%s called handleInstallSnapshotMessage with snapshot from %s",
		r.describe(), NodeID(m.From))
	index, term := m.Snapshot.Index, m.Snapshot.Term
	resp := pb.Message{
		To:   m.From,
		Type: pb.ReplicateResp,
	}
	if r.restore(m.Snapshot) {
		plog.Infof("%s restored snapshot index %d term %d",
			r.describe(), index, term)
		resp.LogIndex = r.log.lastIndex()
	} else {
		plog.Infof("%s ignored snapshot index %d term %d",
			r.describe(), index, term)
		resp.LogIndex = r.log.committed
	}
	r.send(resp)
}

func (r *raft) handleReplicateMessage(m pb.Message) {
	resp := pb.Message{
		To:   m.From,
		Type: pb.ReplicateResp,
	}
	if m.LogIndex < r.log.committed {
		resp.LogIndex = r.log.committed
		r.send(resp)
		return
	}
	if r.log.matchTerm(m.LogIndex, m.LogTerm) {
		r.log.tryAppend(m.LogIndex, m.Entries)
		lastIdx := m.LogIndex + uint64(len(m.Entries))
		r.log.commitTo(min(lastIdx, m.Commit))
		resp.LogIndex = lastIdx
	} else {
		plog.Warningf("%s rejected Replicate index %d term %d from %s",
			r.describe(), m.LogIndex, m.Term, NodeID(m.From))
		resp.Reject = true
		resp.LogIndex = m.LogIndex
		resp.Hint = r.log.lastIndex()
	}
	r.send(resp)
}

//
// Step related functions
//

func isRequestMessage(t pb.MessageType) bool {
	return t == pb.Propose || t == pb.ReadIndex
}

func isLeaderMessage(t pb.MessageType) bool {
	return t == pb.Replicate || t == pb.InstallSnapshot ||
		t == pb.Heartbeat || t == pb.TimeoutNow || t == pb.ReadIndexResp
}

func (r *raft) dropRequestVoteFromHighTermNode(m pb.Message) bool {
	if m.Type != pb.RequestVote || !r.checkQuorum || m.Term <= r.term {
		return false
	}
	// we got a RequestVote with higher term, but we recently had heartbeat msg
	// from leader within the minimum election timeout and that leader is known
	// to have quorum. we thus drop such RequestVote to minimize interruption by
	// network partitioned nodes with higher term.
	// this idea is from the last paragraph of the section 6 of the raft paper
	if m.Hint == m.From {
		plog.Infof("%s, RequestVote with leader transfer hint received from %d",
			r.describe(), m.From)
		return false
	}
	if r.leaderID != NoLeader && r.electionTick < r.electionTimeout {
		return true
	}
	return false
}

// onMessageTermNotMatched handles the situation in which the incoming
// message has a term value different from local node's term. it returns a
// boolean flag indicating whether the message should be ignored.
func (r *raft) onMessageTermNotMatched(m pb.Message) bool {
	if m.Term == 0 || m.Term == r.term {
		return false
	}
	if r.dropRequestVoteFromHighTermNode(m) {
		return true
	}
	if m.Term > r.term {
		plog.Infof("%s received a %s with higher term (%d) from %s",
			r.describe(), m.Type, m.Term, NodeID(m.From))
		leaderID := NoLeader
		if isLeaderMessage(m.Type) {
			leaderID = m.From
		}
		if r.isObserver() {
			r.becomeObserver(m.Term, leaderID)
		} else {
			r.becomeFollower(m.Term, leaderID)
		}
	} else if m.Term < r.term {
		if isLeaderMessage(m.Type) && r.checkQuorum {
			// see TestFreeStuckCandidateWithCheckQuorum for details
			r.send(pb.Message{To: m.From, Type: pb.NoOP})
		} else {
			plog.Infof("%s ignored a %s with lower term (%d) from %s",
				r.describe(), m.Type, m.Term, NodeID(m.From))
		}
		return true
	}
	return false
}

func (r *raft) Handle(m pb.Message) {
	if !r.onMessageTermNotMatched(m) {
		r.doubleCheckTermMatched(m.Term)
		r.handle(r, m)
	} else {
		plog.Infof("term not matched")
	}
}

func (r *raft) hasConfigChangeToApply() bool {
	if r.hasNotAppliedConfigChange != nil {
		plog.Infof("using test-only hasConfigChangeToApply()")
		return r.hasNotAppliedConfigChange()
	}
	return r.log.committed > r.getApplied()
}

func (r *raft) canGrantVote(m pb.Message) bool {
	return r.vote == NoNode ||
		r.vote == m.From ||
		m.Term > r.term
}

//
// handlers for nodes in any state
//

func (r *raft) handleNodeElection(m pb.Message) {
	if r.state != leader {
		if r.hasConfigChangeToApply() {
			plog.Warningf("%s campaign skipped due to pending Config Change",
				r.describe())
			return
		}
		plog.Infof("%s will campaign at term %d", r.describe(), r.term)
		r.campaign()
	} else {
		plog.Infof("leader node %s ignored Election",
			r.describe())
	}
}

func (r *raft) handleNodeRequestVote(m pb.Message) {
	resp := pb.Message{
		To:   m.From,
		Type: pb.RequestVoteResp,
	}
	canGrant := r.canGrantVote(m)
	isUpToDate := r.log.upToDate(m.LogIndex, m.LogTerm)
	if canGrant && isUpToDate {
		plog.Infof("%s cast vote from %s index %d term %d, log term: %d",
			r.describe(), NodeID(m.From), m.LogIndex, m.Term, m.LogTerm)
		r.electionTick = 0
		r.vote = m.From
	} else {
		plog.Infof("%s rejected vote %s index%d term%d,logterm%d,grant%v,utd%v",
			r.describe(), NodeID(m.From), m.LogIndex, m.Term,
			m.LogTerm, canGrant, isUpToDate)
		resp.Reject = true
	}
	r.send(resp)
}

//
// message handler functions used by leader
//

func (r *raft) handleLeaderLeaderHeartbeat(m pb.Message) {
	r.broadcastHeartbeatMessage()
}

func (r *raft) handleLeaderCheckQuorum(m pb.Message) {
	if !r.leaderHasQuorum() {
		plog.Warningf("%s stepped down, no longer has quorum",
			r.describe())
		r.becomeFollower(r.term, NoLeader)
	}
}

func (r *raft) handleLeaderPropose(m pb.Message) {
	if r.selfRemoved() {
		plog.Warningf("dropping a proposal, local node has been removed")
		return
	}
	if r.leaderTransfering() {
		plog.Warningf("dropping a proposal, leader transfer is ongoing")
		return
	}
	for i, e := range m.Entries {
		if e.Type == pb.ConfigChangeEntry {
			if r.hasPendingConfigChange() {
				plog.Warningf("%s dropped a config change, one is pending",
					r.describe())
				m.Entries[i] = pb.Entry{Type: pb.ApplicationEntry}
			}
			r.setPendingConfigChange()
		}
	}
	r.appendEntries(m.Entries)
	r.broadcastReplicateMessage()
}

func (r *raft) hasCommittedEntryAtCurrentTerm() bool {
	if r.term == 0 {
		panic("not suppose to reach here")
	}
	lastCommittedTerm, err := r.log.term(r.log.committed)
	if err != nil && err != ErrCompacted {
		panic(err)
	}
	return lastCommittedTerm == r.term
}

func (r *raft) clearReadyToRead() {
	r.readyToRead = r.readyToRead[:0]
}

func (r *raft) addReadyToRead(index uint64, ctx pb.SystemCtx) {
	r.readyToRead = append(r.readyToRead,
		pb.ReadyToRead{
			Index:     index,
			SystemCtx: ctx,
		})
}

func (r *raft) handleLeaderReadIndex(m pb.Message) {
	if r.selfRemoved() {
		plog.Warningf("dropping a read index request, local node removed")
	}
	ctx := pb.SystemCtx{
		High: m.HintHigh,
		Low:  m.Hint,
	}
	if !r.isSingleNodeQuorum() {
		if !r.hasCommittedEntryAtCurrentTerm() {
			// leader doesn't know the commit value of the cluster
			// see raft thesis section 6.4, this is the first step of the ReadIndex
			// protocol.
			plog.Warningf("ReadIndex request ignored, no entry committed")
			return
		}
		r.readIndex.addRequest(r.log.committed, ctx, m.From)
		r.broadcastHeartbeatMessageWithHint(ctx)
	} else {
		r.addReadyToRead(r.log.committed, ctx)
		_, ok := r.observers[m.From]
		if m.From != r.nodeID && ok {
			r.send(pb.Message{
				To:       m.From,
				Type:     pb.ReadIndexResp,
				LogIndex: r.log.committed,
				Hint:     m.Hint,
				HintHigh: m.HintHigh,
				Commit:   m.Commit,
			})
		}
	}
}

func (r *raft) handleLeaderReplicateResp(m pb.Message, rp *remote) {
	rp.setActive()
	if !m.Reject {
		paused := rp.isPaused()
		if rp.tryUpdate(m.LogIndex) {
			rp.respondedTo()
			if r.tryCommit() {
				r.broadcastReplicateMessage()
			} else if paused {
				r.sendReplicateMessage(m.From)
			}
			if r.leaderTransfering() && m.From == r.leaderTransferTarget &&
				r.log.lastIndex() == rp.match {
				r.sendTimeoutNowMessage(r.leaderTransferTarget)
			}
		}
	} else {
		if rp.decreaseTo(m.LogIndex, m.Hint) {
			r.enterRetryState(rp)
			r.sendReplicateMessage(m.From)
		}
	}
}

func (r *raft) handleLeaderHeartbeatResp(m pb.Message, rp *remote) {
	rp.setActive()
	rp.waitToRetry()
	if rp.match < r.log.lastIndex() {
		r.sendReplicateMessage(m.From)
	}
	// heartbeat response contains leadership confirmation requested as part of
	// the ReadIndex protocol.
	if m.Hint != 0 {
		r.handleReadIndexLeaderConfirmation(m)
	}
}

func (r *raft) handleLeaderLeaderTransfer(m pb.Message, rp *remote) {
	target := m.Hint
	plog.Infof("handleLeaderLeaderTransfer called on cluster %d, target %d",
		r.clusterID, target)
	if target == NoNode {
		panic("leader transfer target not set")
	}
	if r.leaderTransfering() {
		plog.Warningf("LeaderTransfer ignored, leader transfer is ongoing")
		return
	}
	if r.nodeID == target {
		plog.Warningf("received LeaderTransfer with target pointing to itself")
		return
	}
	r.leaderTransferTarget = target
	r.electionTick = 0
	if rp.match == r.log.lastIndex() {
		r.sendTimeoutNowMessage(target)
	}
}

func (r *raft) handleReadIndexLeaderConfirmation(m pb.Message) {
	ctx := pb.SystemCtx{
		Low:  m.Hint,
		High: m.HintHigh,
	}
	ris := r.readIndex.confirm(ctx, m.From, r.quorum())
	for _, s := range ris {
		if s.from == NoNode || s.from == r.nodeID {
			r.addReadyToRead(s.index, s.ctx)
		} else {
			// FIXME (lni): add tests for this case
			r.send(pb.Message{
				To:       s.from,
				Type:     pb.ReadIndexResp,
				LogIndex: s.index,
				Hint:     m.Hint,
				HintHigh: m.HintHigh,
			})
		}
	}
}

func (r *raft) handleLeaderSnapshotStatus(m pb.Message, rp *remote) {
	if rp.state != remoteSnapshot {
		return
	}
	if m.Reject {
		rp.clearPendingSnapshot()
		plog.Infof("%s snapshot failed, %s is now in wait state",
			r.describe(), NodeID(m.From))
	} else {
		plog.Infof("%s snapshot succeeded, %s in wait state now, next %d",
			r.describe(), NodeID(m.From), rp.next)
	}
	rp.becomeWait()
}

func (r *raft) handleLeaderUnreachable(m pb.Message, rp *remote) {
	plog.Infof("%s received Unreachable, %s entered retry state",
		r.describe(), NodeID(m.From))
	r.enterRetryState(rp)
}

func (r *raft) enterRetryState(rp *remote) {
	if rp.state == remoteReplicate {
		rp.becomeRetry()
	}
}

//
// message handlers used by observer
// re-route them to follower handlers for now
//

func (r *raft) handleObserverReplicate(m pb.Message) {
	r.handleFollowerReplicate(m)
}

func (r *raft) handleObserverHeartbeat(m pb.Message) {
	r.handleFollowerHeartbeat(m)
}

func (r *raft) handleObserverSnapshot(m pb.Message) {
	r.handleFollowerInstallSnapshot(m)
}

func (r *raft) handleObserverPropose(m pb.Message) {
	r.handleFollowerPropose(m)
}

func (r *raft) handleObserverReadIndex(m pb.Message) {
	r.handleFollowerReadIndex(m)
}

func (r *raft) handleObserverReadIndexResp(m pb.Message) {
	r.handleFollowerReadIndexResp(m)
}

//
// message handlers used by follower
//

func (r *raft) handleFollowerPropose(m pb.Message) {
	if r.leaderID == NoLeader {
		plog.Warningf("%s dropping proposal as there is no leader", r.describe())
		return
	}
	m.To = r.leaderID
	// the message might be queued by the transport layer, this violates the
	// requirement of the entryQueue.get() func. copy the m.Entries to its
	// own space.
	m.Entries = newEntrySlice(m.Entries)
	r.send(m)
}

func (r *raft) handleFollowerReplicate(m pb.Message) {
	r.electionTick = 0
	r.setLeaderID(m.From)
	r.handleReplicateMessage(m)
}

func (r *raft) handleFollowerHeartbeat(m pb.Message) {
	r.electionTick = 0
	r.setLeaderID(m.From)
	r.handleHeartbeatMessage(m)
}

func (r *raft) handleFollowerReadIndex(m pb.Message) {
	if r.leaderID == NoLeader {
		plog.Warningf("%s dropped ReadIndex as no leader", r.describe())
		return
	}
	m.To = r.leaderID
	r.send(m)
}

func (r *raft) handleFollowerLeaderTransfer(m pb.Message) {
	if r.leaderID == NoLeader {
		plog.Warningf("%s dropped LeaderTransfer as no leader", r.describe())
		return
	}
	plog.Infof("rerouting LeaderTransfer for %d to %d",
		r.clusterID, r.leaderID)
	m.To = r.leaderID
	r.send(m)
}

func (r *raft) handleFollowerReadIndexResp(m pb.Message) {
	ctx := pb.SystemCtx{
		Low:  m.Hint,
		High: m.HintHigh,
	}
	r.electionTick = 0
	r.setLeaderID(m.From)
	r.addReadyToRead(m.LogIndex, ctx)
}

func (r *raft) handleFollowerInstallSnapshot(m pb.Message) {
	r.electionTick = 0
	r.setLeaderID(m.From)
	r.handleInstallSnapshotMessage(m)
}

func (r *raft) handleFollowerTimeoutNow(m pb.Message) {
	// as mentioned by the raft paper, this is nothing different from the clock
	// moving forward faster.
	plog.Infof("TimeoutNow received on %d:%d", r.clusterID, r.nodeID)
	r.electionTick = r.randomizedElectionTimeout
	r.isLeaderTransferTarget = true
	r.tick()
	if r.isLeaderTransferTarget {
		r.isLeaderTransferTarget = false
	}
}

//
// handler functions used by candidate
//

func (r *raft) doubleCheckTermMatched(msgTerm uint64) {
	if msgTerm != 0 && r.term != msgTerm {
		panic("mismatched term found")
	}
}

func (r *raft) handleCandidatePropose(m pb.Message) {
	plog.Warningf("%s dropping proposal, no leader", r.describe())
}

func (r *raft) handleCandidateReplicate(m pb.Message) {
	plog.Infof("r.handleCandidateReplicate invoked")
	r.becomeFollower(r.term, m.From)
	r.handleReplicateMessage(m)
}

func (r *raft) handleCandidateInstallSnapshot(m pb.Message) {
	r.becomeFollower(r.term, m.From)
	r.handleInstallSnapshotMessage(m)
}

func (r *raft) handleCandidateHeartbeat(m pb.Message) {
	r.becomeFollower(r.term, m.From)
	r.handleHeartbeatMessage(m)
}

func (r *raft) handleCandidateRequestVoteResp(m pb.Message) {
	_, ok := r.observers[m.From]
	if ok {
		plog.Warningf("dropping a RequestVoteResp from observer")
		return
	}
	count := r.handleVoteResp(m.From, m.Reject)
	plog.Infof("%s received %d votes and %d rejections, quorum is %d",
		r.describe(), count, len(r.votes)-count, r.quorum())
	if count == r.quorum() {
		r.becomeLeader()
		r.broadcastReplicateMessage()
	} else if len(r.votes)-count == r.quorum() {
		r.becomeFollower(r.term, NoLeader)
	}
}

func lw(r *raft, f func(m pb.Message, rp *remote)) handlerFunc {
	w := func(nm pb.Message) {
		if npr, ok := r.remotes[nm.From]; ok {
			f(nm, npr)
		} else if nob, ok := r.observers[nm.From]; ok {
			f(nm, nob)
		} else {
			plog.Infof("%s no remote available for %s",
				r.describe(), NodeID(nm.From))
			return
		}
	}
	return w
}

func defaultHandle(r *raft, m pb.Message) {
	f := r.handlers[r.state][m.Type]
	if f != nil {
		f(m)
	}
}

func (r *raft) initializeHandlerMap() {
	// candidate
	r.handlers[candidate][pb.Heartbeat] = r.handleCandidateHeartbeat
	r.handlers[candidate][pb.Propose] = r.handleCandidatePropose
	r.handlers[candidate][pb.Replicate] = r.handleCandidateReplicate
	r.handlers[candidate][pb.InstallSnapshot] = r.handleCandidateInstallSnapshot
	r.handlers[candidate][pb.RequestVoteResp] = r.handleCandidateRequestVoteResp
	r.handlers[candidate][pb.Election] = r.handleNodeElection
	r.handlers[candidate][pb.RequestVote] = r.handleNodeRequestVote
	// follower
	r.handlers[follower][pb.Propose] = r.handleFollowerPropose
	r.handlers[follower][pb.Replicate] = r.handleFollowerReplicate
	r.handlers[follower][pb.Heartbeat] = r.handleFollowerHeartbeat
	r.handlers[follower][pb.ReadIndex] = r.handleFollowerReadIndex
	r.handlers[follower][pb.LeaderTransfer] = r.handleFollowerLeaderTransfer
	r.handlers[follower][pb.ReadIndexResp] = r.handleFollowerReadIndexResp
	r.handlers[follower][pb.InstallSnapshot] = r.handleFollowerInstallSnapshot
	r.handlers[follower][pb.Election] = r.handleNodeElection
	r.handlers[follower][pb.RequestVote] = r.handleNodeRequestVote
	r.handlers[follower][pb.TimeoutNow] = r.handleFollowerTimeoutNow
	// leader
	r.handlers[leader][pb.LeaderHeartbeat] = r.handleLeaderLeaderHeartbeat
	r.handlers[leader][pb.CheckQuorum] = r.handleLeaderCheckQuorum
	r.handlers[leader][pb.Propose] = r.handleLeaderPropose
	r.handlers[leader][pb.ReadIndex] = r.handleLeaderReadIndex
	r.handlers[leader][pb.ReplicateResp] = lw(r, r.handleLeaderReplicateResp)
	r.handlers[leader][pb.HeartbeatResp] = lw(r, r.handleLeaderHeartbeatResp)
	r.handlers[leader][pb.SnapshotStatus] = lw(r, r.handleLeaderSnapshotStatus)
	r.handlers[leader][pb.Unreachable] = lw(r, r.handleLeaderUnreachable)
	r.handlers[leader][pb.LeaderTransfer] = lw(r, r.handleLeaderLeaderTransfer)
	r.handlers[leader][pb.Election] = r.handleNodeElection
	r.handlers[leader][pb.RequestVote] = r.handleNodeRequestVote
	// observer
	r.handlers[observer][pb.Heartbeat] = r.handleObserverHeartbeat
	r.handlers[observer][pb.Replicate] = r.handleObserverReplicate
	r.handlers[observer][pb.InstallSnapshot] = r.handleObserverSnapshot
	r.handlers[observer][pb.Propose] = r.handleObserverPropose
	r.handlers[observer][pb.ReadIndex] = r.handleObserverReadIndex
	r.handlers[observer][pb.ReadIndexResp] = r.handleObserverReadIndexResp
}

func (r *raft) checkHandlerMap() {
	// following states/types are not suppose to have handler filled in
	checks := []struct {
		stateType State
		msgType   pb.MessageType
	}{
		{leader, pb.Heartbeat},
		{leader, pb.Replicate},
		{leader, pb.InstallSnapshot},
		{leader, pb.ReadIndexResp},
		{follower, pb.ReplicateResp},
		{follower, pb.HeartbeatResp},
		{follower, pb.SnapshotStatus},
		{follower, pb.Unreachable},
		{candidate, pb.ReplicateResp},
		{candidate, pb.HeartbeatResp},
		{candidate, pb.SnapshotStatus},
		{candidate, pb.Unreachable},
		{observer, pb.Election},
		{observer, pb.RequestVote},
		{observer, pb.RequestVoteResp},
		{observer, pb.ReplicateResp},
		{observer, pb.HeartbeatResp},
	}
	for _, tt := range checks {
		f := r.handlers[tt.stateType][tt.msgType]
		if f != nil {
			panic("unexpected msg handler")
		}
	}
}

//
// debugging related functions
//

func (r *raft) dumpRaftInfoToLog(addrMap map[uint64]string) {
	var flag string
	if r.leaderID != NoLeader && r.leaderID == r.nodeID {
		flag = "***"
	} else {
		flag = "###"
	}
	plog.Infof("%s Raft node %s, %d remote nodes", flag, r.describe(), len(r.remotes))
	for id, rp := range r.remotes {
		v, ok := addrMap[id]
		if !ok {
			v = "!missing!"
		}
		plog.Infof(" %s,addr:%s,match:%d,next:%d,state:%s,paused:%v,ra:%v,ps:%d",
			NodeID(id), v, rp.match, rp.next, rp.state, rp.isPaused(),
			rp.isActive(), rp.snapshotIndex)
	}
}