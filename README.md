# Raft Consensus Implementation

A complete implementation of the Raft distributed consensus algorithm in Go, built on top of the MIT 6.5840 Distributed Systems lab framework. All 28 tests pass across the four lab components, including the race detector.

## What is Raft?

Raft is a consensus algorithm designed to be understandable. It allows a cluster of servers to agree on a sequence of log entries even in the presence of failures. A Raft cluster tolerates up to (n-1)/2 server failures, meaning a 3-node cluster survives 1 failure and a 5-node cluster survives 2.

This implementation is based directly on the paper:
**[In Search of an Understandable Consensus Algorithm](https://raft.github.io/raft.pdf)** by Diego Ongaro and John Ousterhout (USENIX ATC 2014).

## Architecture

A Raft cluster consists of servers that are always in one of three states:

```
           timeout, no heartbeat
Follower  -----------------------> Candidate
   ^                                   |
   |  discover higher term             |  receives majority votes
   |                                   v
   +-------------------------------- Leader
         step down on higher term
```

The leader handles all client requests, replicates log entries to followers, and sends periodic heartbeats to prevent new elections. If a follower does not hear from a leader within a randomized election timeout, it starts a new election.

## What is Implemented

**Lab 3A: Leader Election**

Servers elect a leader using randomized election timeouts to avoid split votes. A candidate wins by collecting votes from a majority of the cluster. The log completeness check ensures a candidate with a stale log cannot win an election, which is the core safety guarantee of the algorithm.

**Lab 3B: Log Replication**

The leader accepts commands from clients, appends them to its log, and replicates them to followers via AppendEntries RPCs. An entry is committed once a majority of servers have written it. Committed entries are applied to the state machine in order via an apply channel.

Log repair after failures uses accelerated backtracking. Rather than decrementing nextIndex by one per round trip, followers return conflict term information so the leader can skip back an entire term at once. This matters significantly under a lossy network.

**Lab 3C: Persistence**

The three fields that must survive a crash are written to stable storage before any RPC response: currentTerm, votedFor, and the log. This prevents a restarted server from voting twice in the same term or forgetting entries it acknowledged.

**Lab 3D: Log Compaction**

The log cannot grow without bound. Once the state machine has applied entries up to some index, it takes a snapshot and instructs Raft to discard everything before that point. When a follower is so far behind that the leader has already discarded the entries it needs, the leader sends an InstallSnapshot RPC instead of AppendEntries.

Implementing snapshots required rethinking every index in the codebase. The in-memory log is now a window into the global log, and all index arithmetic translates between local array positions and global log indices through a set of helper methods.

## Test Results

All 28 tests pass with the Go race detector enabled (`-race`):

| Lab | Tests | Description |
|-----|-------|-------------|
| 3A  | 3     | Leader election on reliable and unreliable networks |
| 3B  | 10    | Log replication, leader/follower failures, concurrent clients |
| 3C  | 8     | Persistence across crashes, Figure 8 scenarios, churn |
| 3D  | 7     | Snapshot creation, InstallSnapshot, crash and restart |

```
go test -run "3A|3B|3C|3D" -race
...
ok   6.5840/raft1   453.524s
```

## Project Structure

```
src/raft1/
  raft.go       # The full Raft implementation
  server.go     # Test harness state machine (lab scaffolding)
  raft_test.go  # Test suite
```

## Running the Tests

Requires Go 1.21 or later.

```bash
cd src/raft1

# Run a specific lab
go test -run 3A -race -v
go test -run 3B -race -v
go test -run 3C -race -v
go test -run 3D -race -v

# Run everything
go test -run "3A|3B|3C|3D" -race
```

## Key Design Decisions

**Single mutex over the entire Raft state.** Raft's state transitions are complex and interdependent. Fine-grained locking adds implementation complexity without meaningful performance benefit in this setting, since the bottleneck is network latency, not lock contention.

**Heartbeat and replication in the same loop.** Rather than maintaining a separate replication trigger, the heartbeat loop always sends the entries a follower is missing. An empty Entries slice is a heartbeat; a non-empty one is replication. This simplifies the code path significantly.

**Stale RPC detection.** Because RPCs can return arbitrarily late, every RPC callback checks that the server's role and term have not changed since the RPC was sent before acting on the response. This prevents a delayed vote response from incorrectly crowning a server that has already moved on to a new term.

**Snapshot index as a base offset.** The snapshotIndex field tracks the global index of the last compacted entry. log[0] is always a dummy entry whose term equals the snapshot term, preserving the invariant that prevLogTerm can always be looked up for any entry in the current log window.

## References

- [Raft Paper](https://raft.github.io/raft.pdf)
- [Raft Visualization](https://raft.github.io)
- [MIT 6.5840 Distributed Systems](https://pdos.csail.mit.edu/6.824)