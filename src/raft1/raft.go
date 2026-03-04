package raft

import (
	"bytes"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"6.5840/labgob"
	"6.5840/labrpc"
	"6.5840/raftapi"
	"6.5840/tester1"
)

type Raft struct {
	mu        sync.Mutex
	peers     []*labrpc.ClientEnd
	persister *tester.Persister
	me        int
	dead      int32

	// Persistent state
	currentTerm   int
	votedFor      int
	log           []LogEntry
	snapshotIndex int // global index of last entry included in snapshot; log[0] is dummy at this index

	// Volatile state
	commitIndex int
	lastApplied int

	// Leader volatile state
	nextIndex  []int
	matchIndex []int

	// Other
	role          int // 0=follower, 1=candidate, 2=leader
	lastHeartbeat time.Time
	applyCh       chan raftapi.ApplyMsg
	snapshot      []byte
}

type LogEntry struct {
	Term    int
	Command interface{}
}

// lastLogIndex returns the global index of the last log entry.
func (rf *Raft) lastLogIndex() int {
	return rf.snapshotIndex + len(rf.log) - 1
}

// lastLogTerm returns the term of the last log entry.
func (rf *Raft) lastLogTerm() int {
	return rf.log[len(rf.log)-1].Term
}

// termAt returns the term at global index i (must be >= snapshotIndex).
func (rf *Raft) termAt(i int) int {
	return rf.log[i-rf.snapshotIndex].Term
}

func (rf *Raft) GetState() (int, bool) {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	return rf.currentTerm, rf.role == 2
}

func (rf *Raft) persist() {
	w := new(bytes.Buffer)
	e := labgob.NewEncoder(w)
	e.Encode(rf.currentTerm)
	e.Encode(rf.votedFor)
	e.Encode(rf.log)
	e.Encode(rf.snapshotIndex)
	raftstate := w.Bytes()
	rf.persister.Save(raftstate, rf.snapshot)
}

func (rf *Raft) readPersist(data []byte) {
	if data == nil || len(data) < 1 {
		return
	}
	r := bytes.NewBuffer(data)
	d := labgob.NewDecoder(r)
	var currentTerm int
	var votedFor int
	var log []LogEntry
	var snapshotIndex int
	if d.Decode(&currentTerm) != nil ||
		d.Decode(&votedFor) != nil ||
		d.Decode(&log) != nil ||
		d.Decode(&snapshotIndex) != nil {
		return
	}
	rf.currentTerm = currentTerm
	rf.votedFor = votedFor
	rf.log = log
	rf.snapshotIndex = snapshotIndex
}

func (rf *Raft) PersistBytes() int {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	return rf.persister.RaftStateSize()
}

func (rf *Raft) Snapshot(index int, snapshot []byte) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	if index <= rf.snapshotIndex {
		return
	}

	localIdx := index - rf.snapshotIndex
	newLog := make([]LogEntry, 1)
	newLog[0] = LogEntry{Term: rf.log[localIdx].Term}
	if localIdx+1 < len(rf.log) {
		newLog = append(newLog, rf.log[localIdx+1:]...)
	}
	rf.log = newLog
	rf.snapshotIndex = index
	rf.snapshot = snapshot
	rf.persist()
}

type RequestVoteArgs struct {
	Term         int
	CandidateId  int
	LastLogIndex int
	LastLogTerm  int
}

type RequestVoteReply struct {
	Term        int
	VoteGranted bool
}

type AppendEntriesArgs struct {
	Term         int
	LeaderId     int
	PrevLogIndex int
	PrevLogTerm  int
	Entries      []LogEntry
	LeaderCommit int
}

type AppendEntriesReply struct {
	Term          int
	Success       bool
	ConflictTerm  int
	ConflictIndex int
}

type InstallSnapshotArgs struct {
	Term              int
	LeaderId          int
	LastIncludedIndex int
	LastIncludedTerm  int
	Data              []byte
}

type InstallSnapshotReply struct {
	Term int
}

func (rf *Raft) RequestVote(args *RequestVoteArgs, reply *RequestVoteReply) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	reply.Term = rf.currentTerm
	reply.VoteGranted = false

	if args.Term < rf.currentTerm {
		return
	}

	if args.Term > rf.currentTerm {
		rf.currentTerm = args.Term
		rf.role = 0
		rf.votedFor = -1
		rf.persist()
	}

	lastIndex := rf.lastLogIndex()
	lastTerm := rf.lastLogTerm()

	logOk := args.LastLogTerm > lastTerm ||
		(args.LastLogTerm == lastTerm && args.LastLogIndex >= lastIndex)

	if (rf.votedFor == -1 || rf.votedFor == args.CandidateId) && logOk {
		rf.votedFor = args.CandidateId
		rf.lastHeartbeat = time.Now()
		reply.VoteGranted = true
		rf.persist()
	}
}

func (rf *Raft) sendRequestVote(server int, args *RequestVoteArgs, reply *RequestVoteReply) bool {
	ok := rf.peers[server].Call("Raft.RequestVote", args, reply)
	return ok
}

func (rf *Raft) AppendEntries(args *AppendEntriesArgs, reply *AppendEntriesReply) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	reply.Term = rf.currentTerm
	reply.Success = false

	if args.Term < rf.currentTerm {
		return
	}

	if args.Term > rf.currentTerm {
		rf.currentTerm = args.Term
		rf.votedFor = -1
		rf.persist()
	}

	rf.role = 0
	rf.lastHeartbeat = time.Now()

	// prevLogIndex is behind our snapshot — nudge leader to send InstallSnapshot
	if args.PrevLogIndex < rf.snapshotIndex {
		reply.ConflictIndex = rf.snapshotIndex
		reply.ConflictTerm = -1
		return
	}

	// prevLogIndex is beyond our log
	if args.PrevLogIndex >= rf.snapshotIndex+len(rf.log) {
		reply.ConflictIndex = rf.snapshotIndex + len(rf.log)
		reply.ConflictTerm = -1
		return
	}

	localPrev := args.PrevLogIndex - rf.snapshotIndex
	if rf.log[localPrev].Term != args.PrevLogTerm {
		reply.ConflictTerm = rf.log[localPrev].Term
		localStart := localPrev
		for localStart > 1 && rf.log[localStart-1].Term == reply.ConflictTerm {
			localStart--
		}
		reply.ConflictIndex = localStart + rf.snapshotIndex
		return
	}

	for i, entry := range args.Entries {
		localIdx := localPrev + 1 + i
		if localIdx < len(rf.log) {
			if rf.log[localIdx].Term != entry.Term {
				rf.log = rf.log[:localIdx]
				rf.log = append(rf.log, args.Entries[i:]...)
				break
			}
		} else {
			rf.log = append(rf.log, args.Entries[i:]...)
			break
		}
	}

	rf.persist()

	if args.LeaderCommit > rf.commitIndex {
		lastNewIndex := args.PrevLogIndex + len(args.Entries)
		if args.LeaderCommit < lastNewIndex {
			rf.commitIndex = args.LeaderCommit
		} else {
			rf.commitIndex = lastNewIndex
		}
	}

	reply.Success = true
}

func (rf *Raft) sendAppendEntries(server int, args *AppendEntriesArgs, reply *AppendEntriesReply) bool {
	ok := rf.peers[server].Call("Raft.AppendEntries", args, reply)
	return ok
}

func (rf *Raft) InstallSnapshot(args *InstallSnapshotArgs, reply *InstallSnapshotReply) {
	rf.mu.Lock()

	reply.Term = rf.currentTerm

	if args.Term < rf.currentTerm {
		rf.mu.Unlock()
		return
	}

	if args.Term > rf.currentTerm {
		rf.currentTerm = args.Term
		rf.votedFor = -1
		rf.persist()
	}

	rf.role = 0
	rf.lastHeartbeat = time.Now()

	if args.LastIncludedIndex <= rf.snapshotIndex {
		rf.mu.Unlock()
		return
	}

	localIdx := args.LastIncludedIndex - rf.snapshotIndex
	newLog := make([]LogEntry, 1)
	newLog[0] = LogEntry{Term: args.LastIncludedTerm}
	if localIdx+1 < len(rf.log) {
		newLog = append(newLog, rf.log[localIdx+1:]...)
	}
	rf.log = newLog
	rf.snapshotIndex = args.LastIncludedIndex
	rf.snapshot = args.Data

	if args.LastIncludedIndex > rf.commitIndex {
		rf.commitIndex = args.LastIncludedIndex
	}
	if args.LastIncludedIndex > rf.lastApplied {
		rf.lastApplied = args.LastIncludedIndex
	}

	rf.persist()

	msg := raftapi.ApplyMsg{
		SnapshotValid: true,
		Snapshot:      args.Data,
		SnapshotTerm:  args.LastIncludedTerm,
		SnapshotIndex: args.LastIncludedIndex,
	}
	rf.mu.Unlock()
	rf.applyCh <- msg
}

func (rf *Raft) sendInstallSnapshot(server int, args *InstallSnapshotArgs, reply *InstallSnapshotReply) bool {
	ok := rf.peers[server].Call("Raft.InstallSnapshot", args, reply)
	return ok
}

func (rf *Raft) Start(command interface{}) (int, int, bool) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	if rf.role != 2 {
		return -1, -1, false
	}

	entry := LogEntry{
		Term:    rf.currentTerm,
		Command: command,
	}
	rf.log = append(rf.log, entry)
	rf.persist()
	index := rf.lastLogIndex()
	term := rf.currentTerm

	return index, term, true
}

func (rf *Raft) Kill() {
	atomic.StoreInt32(&rf.dead, 1)
}

func (rf *Raft) killed() bool {
	z := atomic.LoadInt32(&rf.dead)
	return z == 1
}

func (rf *Raft) ticker() {
	for rf.killed() == false {
		rf.mu.Lock()
		role := rf.role
		elapsed := time.Since(rf.lastHeartbeat)
		rf.mu.Unlock()

		if role != 2 && elapsed > rf.electionTimeout() {
			rf.startElection()
		}

		time.Sleep(10 * time.Millisecond)
	}
}

func (rf *Raft) electionTimeout() time.Duration {
	return time.Duration(300+rand.Intn(200)) * time.Millisecond
}

func (rf *Raft) startElection() {
	rf.mu.Lock()
	rf.role = 1
	rf.currentTerm++
	rf.votedFor = rf.me
	rf.lastHeartbeat = time.Now()
	rf.persist()
	votes := 1
	term := rf.currentTerm
	lastLogIndex := rf.lastLogIndex()
	lastLogTerm := rf.lastLogTerm()
	rf.mu.Unlock()

	for i := range rf.peers {
		if i == rf.me {
			continue
		}
		go func(server int) {
			args := &RequestVoteArgs{
				Term:         term,
				CandidateId:  rf.me,
				LastLogIndex: lastLogIndex,
				LastLogTerm:  lastLogTerm,
			}
			reply := &RequestVoteReply{}
			if !rf.sendRequestVote(server, args, reply) {
				return
			}
			rf.mu.Lock()
			defer rf.mu.Unlock()

			if reply.Term > rf.currentTerm {
				rf.currentTerm = reply.Term
				rf.role = 0
				rf.votedFor = -1
				rf.persist()
				return
			}
			if rf.role != 1 || rf.currentTerm != term {
				return
			}
			if reply.VoteGranted {
				votes++
				if votes > len(rf.peers)/2 {
					rf.role = 2
					rf.initLeader()
				}
			}
		}(i)
	}
}

func (rf *Raft) initLeader() {
	for i := range rf.peers {
		rf.nextIndex[i] = rf.lastLogIndex() + 1
		rf.matchIndex[i] = 0
	}
	go rf.heartbeatLoop()
}

func (rf *Raft) heartbeatLoop() {
	for rf.killed() == false {
		rf.mu.Lock()
		if rf.role != 2 {
			rf.mu.Unlock()
			return
		}
		term := rf.currentTerm
		rf.mu.Unlock()

		for i := range rf.peers {
			if i == rf.me {
				continue
			}
			go func(server int) {
				rf.mu.Lock()
				if rf.role != 2 {
					rf.mu.Unlock()
					return
				}

				// Follower is behind the snapshot — send InstallSnapshot instead
				if rf.nextIndex[server] <= rf.snapshotIndex {
					args := &InstallSnapshotArgs{
						Term:              rf.currentTerm,
						LeaderId:          rf.me,
						LastIncludedIndex: rf.snapshotIndex,
						LastIncludedTerm:  rf.log[0].Term,
						Data:              rf.snapshot,
					}
					rf.mu.Unlock()

					reply := &InstallSnapshotReply{}
					if !rf.sendInstallSnapshot(server, args, reply) {
						return
					}

					rf.mu.Lock()
					defer rf.mu.Unlock()

					if reply.Term > rf.currentTerm {
						rf.currentTerm = reply.Term
						rf.role = 0
						rf.votedFor = -1
						rf.persist()
						return
					}
					if rf.role != 2 || rf.currentTerm != term {
						return
					}
					rf.nextIndex[server] = args.LastIncludedIndex + 1
					rf.matchIndex[server] = args.LastIncludedIndex
					return
				}

				prevLogIndex := rf.nextIndex[server] - 1 // global
				localPrev := prevLogIndex - rf.snapshotIndex
				prevLogTerm := rf.log[localPrev].Term
				entries := make([]LogEntry, len(rf.log[localPrev+1:]))
				copy(entries, rf.log[localPrev+1:])
				args := &AppendEntriesArgs{
					Term:         term,
					LeaderId:     rf.me,
					PrevLogIndex: prevLogIndex,
					PrevLogTerm:  prevLogTerm,
					Entries:      entries,
					LeaderCommit: rf.commitIndex,
				}
				rf.mu.Unlock()

				reply := &AppendEntriesReply{}
				if !rf.sendAppendEntries(server, args, reply) {
					return
				}

				rf.mu.Lock()
				defer rf.mu.Unlock()

				if reply.Term > rf.currentTerm {
					rf.currentTerm = reply.Term
					rf.role = 0
					rf.votedFor = -1
					rf.persist()
					return
				}
				if rf.role != 2 || rf.currentTerm != term {
					return
				}
				if reply.Success {
					rf.matchIndex[server] = prevLogIndex + len(entries)
					rf.nextIndex[server] = rf.matchIndex[server] + 1
					rf.advanceCommitIndex()
				} else {
					if reply.ConflictTerm == -1 {
						rf.nextIndex[server] = reply.ConflictIndex
					} else {
						newIndex := -1
						for i := rf.lastLogIndex(); i >= rf.snapshotIndex+1; i-- {
							if rf.termAt(i) == reply.ConflictTerm {
								newIndex = i + 1
								break
							}
						}
						if newIndex == -1 {
							rf.nextIndex[server] = reply.ConflictIndex
						} else {
							rf.nextIndex[server] = newIndex
						}
					}
				}
			}(i)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (rf *Raft) advanceCommitIndex() {
	for n := rf.lastLogIndex(); n > rf.commitIndex; n-- {
		if rf.termAt(n) != rf.currentTerm {
			continue
		}
		count := 1
		for i := range rf.peers {
			if i != rf.me && rf.matchIndex[i] >= n {
				count++
			}
		}
		if count > len(rf.peers)/2 {
			rf.commitIndex = n
			break
		}
	}
}

func (rf *Raft) applyLoop() {
	for rf.killed() == false {
		rf.mu.Lock()
		for rf.lastApplied < rf.commitIndex {
			rf.lastApplied++
			localIdx := rf.lastApplied - rf.snapshotIndex
			msg := raftapi.ApplyMsg{
				CommandValid: true,
				Command:      rf.log[localIdx].Command,
				CommandIndex: rf.lastApplied,
			}
			rf.mu.Unlock()
			rf.applyCh <- msg
			rf.mu.Lock()
		}
		rf.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
}

func Make(peers []*labrpc.ClientEnd, me int,
	persister *tester.Persister, applyCh chan raftapi.ApplyMsg) raftapi.Raft {
	rf := &Raft{}
	rf.peers = peers
	rf.persister = persister
	rf.me = me

	rf.currentTerm = 0
	rf.votedFor = -1
	rf.log = make([]LogEntry, 1)
	rf.snapshotIndex = 0
	rf.commitIndex = 0
	rf.lastApplied = 0
	rf.nextIndex = make([]int, len(peers))
	rf.matchIndex = make([]int, len(peers))
	rf.role = 0
	rf.lastHeartbeat = time.Now()
	rf.applyCh = applyCh
	rf.snapshot = nil

	rf.readPersist(persister.ReadRaftState())
	rf.snapshot = persister.ReadSnapshot()

	if rf.snapshotIndex > 0 {
		rf.lastApplied = rf.snapshotIndex
		rf.commitIndex = rf.snapshotIndex
	}

	go rf.applyLoop()
	go rf.ticker()

	return rf
}