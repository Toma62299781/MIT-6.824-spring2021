package raft

import (
	"bytes"
	"sync"
	"sync/atomic"
	"time"

	"6.824/labgob"
	"6.824/labrpc"
	logger "6.824/raft-logs"
)

const (
	FOLLOWER  = 0
	CANDIDATE = 1
	LEADER    = 2

	ELECTION_TIMEOUT = 800 * time.Millisecond
	RANDOM_PLUS      = 200 * time.Millisecond
	HEART_INTERVAL   = 300 * time.Millisecond
)

type ApplyMsg struct {
	CommandValid bool
	Command      interface{}
	CommandIndex int

	// For 2D:
	SnapshotValid bool
	Snapshot      []byte
	SnapshotTerm  int
	SnapshotIndex int
}
type LogEntry struct {
	Term    int
	Command interface{}
}

type Raft struct {
	//initialized, and won't change until restart
	mu             sync.Mutex          // Lock to protect shared access to this peer's state
	peers          []*labrpc.ClientEnd // RPC end points of all peers
	persister      *Persister          // Object to hold this peer's persisted state
	me             int                 // this peer's index into peers[]
	major          int
	peerCnt        int
	applyCh        chan ApplyMsg
	applyCond      *sync.Cond
	wakeLeaderCond *sync.Cond
	timerLock      sync.Mutex
	logger         logger.TopicLogger
	//protected by atomic
	dead int32 // set by Kill()

	//volatile
	snapshot    []byte
	role        int //FOLLOWER = 0, CANDIDATE = 1, LEADER = 2
	commitIndex int
	lastApplied int
	timeDdl     time.Time //time to timeout
	votes       int
	//volatile for leader's every election, which means re-init
	nextIndex  []int
	matchIndex []int

	//persisted
	currentTerm  int
	votedFor     int
	logs         []LogEntry //log[0].Term saves lastIncludedTerm
	offset       int        // = lastIncludedIndex + 1
	lastLogIndex int
}

// return currentTerm and whether this server
// believes it is the leader.
func (rf *Raft) GetState() (int, bool) {

	var term int
	var isleader bool
	// Your code here (2A).
	rf.mu.Lock()
	term = rf.currentTerm
	isleader = rf.role == LEADER
	rf.mu.Unlock()
	return term, isleader
}

//hold lock means:
//before enter the function we alredy got rf.mu.Lock
//and leave the function without Unlock

//hold lock
func (rf *Raft) doPersistRaftAndSnap(index, term int, snapshot []byte) {

	rf.logger.L(logger.Snap, "term %d apply snapshot offset %d->%d \n",
		rf.currentTerm, rf.offset, index)

	if index > rf.commitIndex {
		rf.commitIndex = index
	}
	if index > rf.lastApplied {
		rf.lastApplied = index
	}
	length_after_trim := rf.lastLogIndex - index + 1
	if length_after_trim < 1 {
		length_after_trim = 1
		rf.logs = make([]LogEntry, 1)
	} else {
		// newLogs := make([]LogEntry, length_after_trim)
		// copy(newLogs, rf.logs[index-rf.offset:]) //log[0].Term saves lastIncludedTerm
		rf.logs = append([]LogEntry{}, rf.logs[index-rf.offset:]...)
	}
	if index > rf.lastLogIndex {
		rf.lastLogIndex = index
	}
	rf.logs[0].Term = term
	rf.logs[0].Command = nil
	rf.offset = index
	state := rf.getRaftState()
	rf.persister.SaveStateAndSnapshot(state, snapshot)
	rf.snapshot = snapshot
	rf.logger.L(logger.SnapSize, "raft apply snapshot offset %d ,lastApplied %d,total log %d, size %d,log cap:%d\n",
		index, rf.lastApplied, rf.lastLogIndex, len(state), cap(rf.logs))
	if rf.lastApplied != index {
		rf.logger.L(logger.SnapSize, "last applied not snapshoted %#v\n", rf.logs[rf.lastApplied-rf.offset])
	}
}

//hold lock
func (rf *Raft) getRaftState() []byte {
	w := new(bytes.Buffer)
	e := labgob.NewEncoder(w)
	e.Encode(rf.currentTerm)
	e.Encode(rf.votedFor)
	e.Encode(rf.offset)
	e.Encode(rf.lastLogIndex)
	e.Encode(rf.logs)
	return w.Bytes()
}

//hold lock
func (rf *Raft) persist() {
	rf.persister.SaveRaftState(rf.getRaftState())
}

func (rf *Raft) readPersist(data []byte) {
	if data == nil || len(data) < 1 { // bootstrap without any state?
		return
	}

	r := bytes.NewBuffer(data)
	d := labgob.NewDecoder(r)
	var currentTerm, votedFor, offset, lastLogIndex int

	if d.Decode(&currentTerm) != nil ||
		d.Decode(&votedFor) != nil ||
		d.Decode(&offset) != nil ||
		d.Decode(&lastLogIndex) != nil {
		rf.logger.L(logger.Persist, "read persist error\n")
	} else {
		logs := make([]LogEntry, lastLogIndex-offset)
		if d.Decode(&logs) != nil {
			rf.logger.L(logger.Persist, "read persist logs error\n")
		} else {
			rf.currentTerm = currentTerm
			rf.votedFor = votedFor
			rf.offset = offset
			rf.lastLogIndex = lastLogIndex
			rf.logs = logs
			rf.commitIndex = rf.offset
			rf.lastApplied = rf.offset
			rf.logger.L(logger.Persist, "read persist ok\n")
		}
	}
}

//
// A service wants to switch to snapshot.  Only do so if Raft hasn't
// have more recent info since it communicate the snapshot on applyCh.
//
func (rf *Raft) CondInstallSnapshot(lastIncludedTerm int, lastIncludedIndex int, snapshot []byte) bool {

	//Your code here (2D).
	rf.mu.Lock()
	defer rf.mu.Unlock()

	if rf.lastApplied >= lastIncludedIndex {
		rf.logger.L(logger.Snap, "cond install false lastApplied:%d includeIndex:%d\n",
			rf.lastApplied, lastIncludedIndex)
		return false
	}
	rf.logger.L(logger.Snap, "cond install return true lastApplied:%d includeIndex:%d\n",
		rf.lastApplied, lastIncludedIndex)
	rf.doPersistRaftAndSnap(lastIncludedIndex, lastIncludedTerm, snapshot)
	return true
}

type InstallSnapArgs struct {
	//2a
	Term int
	//other
	LastIncludedIndex int
	LastIncludedTerm  int
	//log entries TODO
	Snap []byte
}
type InstallSnapReply struct {
	Term int
}

func (rf *Raft) InstallSnapshot(args *InstallSnapArgs, reply *InstallSnapReply) {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	defer func() { reply.Term = rf.currentTerm }()
	rf.logger.L(logger.Snap, "term %d recv term %d installSnap:%d, myLast:%d\n",
		rf.currentTerm, args.Term, args.LastIncludedIndex, rf.lastLogIndex)
	if args.Term < rf.currentTerm {
		return
	}
	if args.Term > rf.currentTerm {
		rf.toHigherTermWithLock(args.Term)
	}
	rf.role = FOLLOWER
	rf.freshTimer()

	reply.Term = rf.currentTerm
	if rf.lastApplied >= args.LastIncludedIndex {

		rf.logger.L(logger.Snap, "ignore install index %d for applied %d\n",
			rf.lastApplied, args.LastIncludedIndex)
		return
	}

	rf.freshTimer()
	go func() {
		rf.logger.L(logger.Snap, "write snap to chan index %d\n",
			args.LastIncludedIndex)
		rf.applyCh <- ApplyMsg{
			CommandValid:  false,
			SnapshotValid: true,
			Snapshot:      args.Snap,
			SnapshotTerm:  args.LastIncludedTerm,
			SnapshotIndex: args.LastIncludedIndex,
		}
	}()

}

// the service says it has created a snapshot that has
// all info up to and including index. this means the
// service no longer needs the log through (and including)
// that index. Raft should now trim its log as much as possible.
func (rf *Raft) Snapshot(index int, snapshot []byte) {
	// Your code here (2D).
	rf.mu.Lock()
	rf.logger.L(logger.Snap, "term %d service snap, logs -> [%d->%d]\n", rf.currentTerm, index+1, rf.lastLogIndex)

	term := rf.logs[index-rf.offset].Term
	rf.doPersistRaftAndSnap(index, term, snapshot)
	rf.mu.Unlock()
}

//
// example RequestVote RPC arguments structure.
// field names must start with capital letters!
//
type RequestVoteArgs struct {
	// Your data here (2A, 2B).
	Term         int //2a
	CandidateId  int //2a
	LastLogIndex int
	LastLogTerm  int
}

//
// example RequestVote RPC reply structure.
// field names must start with capital letters!
//
type RequestVoteReply struct {
	// Your data here (2A).
	Term        int  //2a
	VoteGranted bool //2a
}

func isNewEnough(selfTerm, otherTerm, selfIndex, otherIndex int) bool {
	if otherTerm > selfTerm {
		return true
	} else if otherTerm < selfTerm {
		return false
	} else {
		return otherIndex >= selfIndex
	}
}

//hold lock
func (rf *Raft) deleteTailLogs(from int) {
	rf.AssertTrue(from > 0 && from <= rf.lastLogIndex,
		"from:%d lastLog:%d\n", from, rf.lastLogIndex)

	rf.logger.L(logger.LogModify, "term %d delete logs [%d->%d]\n", rf.currentTerm, rf.lastLogIndex, from-1)
	rf.logs = append([]LogEntry{}, rf.logs[:from-rf.offset]...)
	rf.lastLogIndex = from - 1
	rf.persist()
}

//hold lock,
func (rf *Raft) appendManyLogs(logs []LogEntry) {

	rf.lastLogIndex += len(logs)
	rf.logger.L(logger.LogModify, "term %d ++%d logs [tail->%d]\n", rf.currentTerm, len(logs), rf.lastLogIndex)
	rf.logs = append(rf.logs, logs...)
	rf.persist()
}

//hold lock
func (rf *Raft) appendOneLog(logEntry LogEntry) {
	rf.lastLogIndex += 1
	rf.logger.L(logger.LogModify, "term %d ++1 log [tail->%d]\n", rf.currentTerm, rf.lastLogIndex)
	rf.logs = append(rf.logs, logEntry)
	rf.persist()
}

func (rf *Raft) RequestVote(args *RequestVoteArgs, reply *RequestVoteReply) {
	// Your code here (2A, 2B).
	rf.mu.Lock()
	defer rf.mu.Unlock()
	defer func() { reply.Term = rf.currentTerm }()
	rf.logger.L(logger.Vote, "term %d recv term %d voteReq to S%d \n",
		rf.currentTerm, args.Term, args.CandidateId)

	if args.Term < rf.currentTerm {
		reply.VoteGranted = false
		return
	}

	if args.Term > rf.currentTerm {
		rf.toHigherTermWithLock(args.Term)
	}
	if rf.role == LEADER {
		reply.VoteGranted = false
		rf.logger.L(logger.Vote, "term %d as leader reject to vote S%d\n",
			rf.currentTerm, args.CandidateId)
		return
	}

	reply.Term = rf.currentTerm
	selfIndex := rf.lastLogIndex
	selfTerm := rf.logs[selfIndex-rf.offset].Term

	if rf.votedFor == args.CandidateId ||
		(rf.votedFor == -1 &&
			isNewEnough(selfTerm, args.LastLogTerm, selfIndex, args.LastLogIndex)) {
		reply.VoteGranted = true
		rf.freshTimer()
		rf.votedFor = args.CandidateId
		rf.logger.L(logger.Vote, "term %d vote to S%d \n",
			rf.currentTerm, args.CandidateId)
		rf.persist()
	} else {
		reply.VoteGranted = false
		if rf.votedFor != -1 {
			rf.logger.L(logger.Vote, "term %d reject to vote S%d for voted S%d\n",
				rf.currentTerm, args.CandidateId, rf.votedFor)
		} else {
			rf.logger.L(logger.Vote, "term %dreject to vote S%d for log cmp, [t%d,i%d] > [t%d,i%d] \n",
				rf.currentTerm, args.CandidateId,
				selfTerm, selfIndex, args.LastLogTerm, args.LastLogIndex)
		}
	}
}

//with lock and hold lock
func (rf *Raft) toHigherTermWithLock(term int) {
	rf.logger.L(logger.Term, "term change %d-->%d\n", rf.currentTerm, term)
	rf.role = FOLLOWER
	rf.votes = 0
	//persist 2c
	rf.currentTerm = term
	rf.votedFor = -1
	rf.persist()
}

type AppendArgs struct {
	//2a
	Term     int
	LeaderId int
	//other
	PrevLogIndex int
	PrevLogTerm  int
	//log entries TODO
	Entries      []LogEntry
	LeaderCommit int
}
type AppendReply struct {
	Term           int
	Success        bool
	RejectedByTerm bool
	NextIndex      int
	XTerm          int
	XIndex         int
	XLen           int
}

//hold lock
func (rf *Raft) findTermFirstIndex(from int) int {
	i := from - 1
	term := rf.logs[from-rf.offset].Term
	for i > rf.offset {
		if rf.logs[i-rf.offset].Term != term {
			break
		}
		i--
	}
	i++
	rf.AssertTrue(i >= rf.offset && rf.logs[i-rf.offset].Term == rf.logs[from-rf.offset].Term,
		"must equal,found i:%d Term:%d, from i:%d, Term:%d\n",
		i, rf.logs[i-rf.offset].Term, from, rf.logs[from-rf.offset].Term)
	return i
}

func (rf *Raft) Append(args *AppendArgs, reply *AppendReply) {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	defer func() { reply.Term = rf.currentTerm }()
	leaderSendLastIndex := args.PrevLogIndex + len(args.Entries)

	if len(args.Entries) == 0 {
		rf.logger.L(logger.Append, "term %d recv term %d append []\n", rf.currentTerm,
			args.Term)
	} else if len(args.Entries) == 1 {
		rf.logger.L(logger.Append, "term %d recv term %d append [%d]\n", rf.currentTerm,
			args.Term,
			args.PrevLogIndex+1)
	} else {
		rf.logger.L(logger.Append, "term %d recv term %d append [%d->%d]\n", rf.currentTerm,
			args.Term,
			args.PrevLogIndex+1, leaderSendLastIndex)
	}

	reply.Success = false
	reply.RejectedByTerm = false
	if args.Term < rf.currentTerm {
		reply.RejectedByTerm = true
		return
	}

	defer rf.freshTimer()
	if args.Term > rf.currentTerm {
		rf.toHigherTermWithLock(args.Term)
	} else {
		rf.role = FOLLOWER
	}

	reply.NextIndex = leaderSendLastIndex + 1

	//my logs is too short, using quick rollback case 3
	if args.PrevLogIndex > rf.lastLogIndex {
		reply.XTerm = -1
		reply.XLen = rf.lastLogIndex + 1
		rf.logger.L(logger.Append, "term %d lastIndex %d, %d's pre [t%d,i%d] lacking pre\n",
			rf.currentTerm, rf.lastLogIndex, args.LeaderId, args.PrevLogTerm, args.PrevLogIndex)
		return
	}

	//prefix conflict,must d0 rollback, quick rollback case 1 && case 2
	if args.PrevLogIndex > rf.offset && rf.logs[args.PrevLogIndex-rf.offset].Term != args.PrevLogTerm {

		rf.logger.L(logger.Append, "term %d log decline S%d term %d pre[t%d,i%d],for last log's term:%d i:%d\n",
			rf.currentTerm, args.LeaderId, args.Term, args.PrevLogTerm, args.PrevLogIndex,
			rf.logs[rf.lastLogIndex-rf.offset].Term, rf.lastLogIndex)

		reply.XLen = rf.lastLogIndex + 1
		reply.XTerm = rf.logs[args.PrevLogIndex-rf.offset].Term
		reply.XIndex = rf.findTermFirstIndex(args.PrevLogIndex)

		rf.logger.L(logger.Append, "term %d conflict xlen%d xterm%d xindex%d\n",
			rf.currentTerm, reply.XLen, reply.XTerm, reply.XIndex)
		return
	}

	//prefix matched
	reply.Success = true

	reply.NextIndex = leaderSendLastIndex + 1
	if leaderSendLastIndex <= rf.lastApplied {
		rf.logger.L(logger.Append, "term %d log %d already applied\n",
			rf.currentTerm, leaderSendLastIndex)
		return
	}
	//scan logs, delete logs that succeeds the first unmatched log
	scan_end := rf.lastLogIndex
	if scan_end > leaderSendLastIndex {
		scan_end = leaderSendLastIndex
	}
	scan_from := args.PrevLogIndex + 1
	if scan_from <= rf.offset {
		scan_from = rf.offset + 1
	}
	if scan_from <= scan_end {
		for scan_from <= scan_end {
			if rf.logs[scan_from-rf.offset].Term !=
				args.Entries[scan_from-args.PrevLogIndex-1].Term {
				rf.deleteTailLogs(scan_from)
				break
			}
			scan_from++
		}
	}
	//append the left logs
	if scan_from <= leaderSendLastIndex {
		rf.appendManyLogs(args.Entries[scan_from-args.PrevLogIndex-1:])
	}

	//try to commit
	to_commit := args.LeaderCommit
	if to_commit > rf.lastLogIndex {
		to_commit = rf.lastLogIndex
	}
	if to_commit > rf.commitIndex {
		if to_commit == rf.commitIndex+1 {
			rf.logger.L(logger.Commit, "term %d commit [%d]\n",
				rf.currentTerm, to_commit)
		} else {
			rf.logger.L(logger.Commit, "term %d commit [%d->%d]\n",
				rf.currentTerm, rf.commitIndex+1, to_commit)
		}

		rf.commitIndex = to_commit
		if rf.commitIndex > rf.lastApplied {
			rf.applyCond.Signal()
		}

	}

	rf.logger.L(logger.Append, "term %d expect nextindex %d\n", rf.currentTerm, reply.NextIndex)

}

//
// the service using Raft (e.g. a k/v server) wants to start
// agreement on the next command to be appended to Raft's log. if this
// server isn't the leader, returns false. otherwise start the
// agreement and return immediately. there is no guarantee that this
// command will ever be committed to the Raft log, since the leader
// may fail or lose an election. even if the Raft instance has been killed,
// this function should return gracefully.
//
// the first return value is the index that the command will appear at
// if it's ever committed. the second return value is the current
// term. the third return value is true if this server believes it is
// the leader.
//
func (rf *Raft) Start(command interface{}) (int, int, bool) {
	index := -1
	term := -1
	isLeader := true

	rf.mu.Lock()

	term = rf.currentTerm
	isLeader = rf.role == LEADER
	if !isLeader || rf.killed() {
		rf.mu.Unlock()
		return 0, term, false
	}
	term = rf.currentTerm

	rf.appendOneLog(LogEntry{
		Term:    term,
		Command: command,
	})
	index = rf.lastLogIndex
	rf.logger.L(logger.Start, "term %d request of index %d %#v\n", term, index, command)
	rf.mu.Unlock()

	go func() {
		//hang on a while, there might be several Start() calls after this
		//one signal is enough for all Start() calls
		time.Sleep(3 * time.Millisecond)
		rf.wakeLeaderCond.Broadcast()
	}()

	return index, term, true
}

//
// the tester doesn't halt goroutines created by Raft after each test,
// but it does call the Kill() method. your code can use killed() to
// check whether Kill() has been called. the use of atomic avoids the
// need for a lock.
//
// the issue is that long-running goroutines use memory and may chew
// up CPU time, perhaps causing later tests to fail and generating
// confusing debug output. any goroutine with a long-running loop
// should call killed() to check whether it should stop.
//
func (rf *Raft) Kill() {
	rf.logger.L(logger.RaftShutdown, "raft killed #######\n")
	atomic.StoreInt32(&rf.dead, 1)
	// Your code here, if desired.
}

func (rf *Raft) killed() bool {
	z := atomic.LoadInt32(&rf.dead)
	return z == 1
}

// The ticker go routine starts a new election if this peer hasn't received
// heartsbeats recently.
func (rf *Raft) ticker() {
	for !rf.killed() {
		if rf.checkTimeOut() {
			rf.freshTimer()
			go rf.newElection()
		}
		rf.sleepTimeout()
	}
}

//apply logs
func (rf *Raft) applier() {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	for !rf.killed() {
		if rf.lastApplied < rf.commitIndex {
			length := rf.commitIndex - rf.lastApplied
			applyMsgs := make([]*ApplyMsg, length)

			if length == 1 {
				rf.logger.L(logger.Apply, "term %d apply [%d]\n",
					rf.currentTerm, rf.commitIndex)
			} else {
				rf.logger.L(logger.Apply, "term %d apply [%d->%d]\n",
					rf.currentTerm, rf.lastApplied+1, rf.commitIndex)
			}

			for i := 0; i < length; i++ {
				rf.lastApplied++
				applyMsgs[i] = &ApplyMsg{
					CommandValid:  true,
					SnapshotValid: false,
					Command:       rf.logs[rf.lastApplied-rf.offset].Command,
					CommandIndex:  rf.lastApplied,
				}
			}
			rf.mu.Unlock()

			for _, msg := range applyMsgs {
				rf.applyCh <- *msg
			}

			rf.mu.Lock()
		} else {
			rf.applyCond.Wait()
		}
	}

}

//
// the service or tester wants to create a Raft server. the ports
// of all the Raft servers (including this one) are in peers[]. this
// server's port is peers[me]. all the servers' peers[] arrays
// have the same order. persister is a place for this server to
// save its persistent state, and also initially holds the most
// recent saved state, if any. applyCh is a channel on which the
// tester or service expects Raft to send ApplyMsg messages.
// Make() must return quickly, so it should start goroutines
// for any long-running work.
//
func Make(peers []*labrpc.ClientEnd, me int,
	persister *Persister, applyCh chan ApplyMsg) *Raft {
	//init
	labgob.Register(LogEntry{})
	rf := &Raft{
		me: me,
		logger: logger.TopicLogger{
			Me: me,
		},
		peers:        peers,
		persister:    persister,
		peerCnt:      len(peers),
		major:        (len(peers) + 1) / 2,
		role:         FOLLOWER,
		applyCh:      applyCh,
		logs:         make([]LogEntry, 1),
		currentTerm:  0,
		votedFor:     -1,
		offset:       0,
		lastLogIndex: 0,
		commitIndex:  0,
		lastApplied:  0,
		snapshot:     persister.ReadSnapshot(),
	}
	rf.mu.Lock()
	defer rf.mu.Unlock()
	rf.nextIndex = make([]int, rf.peerCnt)
	rf.matchIndex = make([]int, rf.peerCnt)
	rf.applyCond = sync.NewCond(&rf.mu)
	rf.wakeLeaderCond = sync.NewCond(&rf.mu)
	rf.initTimer()
	rf.readPersist(persister.ReadRaftState())
	rf.logger.L(logger.Persist, "init from snap offset %d\n", rf.offset)
	// start ticker goroutine to start elections
	go rf.applier()
	go rf.ticker()
	return rf
}
