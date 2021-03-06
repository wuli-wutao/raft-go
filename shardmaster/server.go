package shardmaster

import (
	"encoding/gob"
	"raft-go/labrpc"
	"raft-go/raft"
	"sync"
	"time"
)

const (
	JOIN = iota
	LEAVE
	MOVE
	QUERY
)

type ShardMaster struct {
	mu      sync.Mutex
	me      int
	rf      *raft.Raft
	applyCh chan raft.ApplyMsg

	// Your data here.
	request map[int64]int
	result  map[int]chan int

	configs []Config // indexed by config num
}

type Op struct {
	Type  int
	Index int
	Cid   int64
	Seq   int
	// join
	Servers map[int][]string
	// leave
	GIDs []int
	// move
	Shard int
	GID   int
	// query
	Num int
}

func (sm *ShardMaster) appendLog(op Op) bool {
	index, _, isLeader := sm.rf.Start(op)
	if !isLeader {
		return false
	}
	sm.mu.Lock()
	// note：can not use `make(chan Op)`
	sm.result[index] = make(chan int, 1)
	sm.mu.Unlock()

	select {
	case idx := <-sm.result[index]:
		sm.mu.Lock()
		sm.request[op.Cid] = op.Seq
		sm.mu.Unlock()
		return idx == op.Index
	case <-time.After(200 * time.Millisecond):
		return false
	}
}

func (sm *ShardMaster) Join(args *JoinArgs, reply *JoinReply) {
	// Your code here.
	op := Op{}
	op.Type = JOIN
	op.Cid = args.Cid
	op.Seq = args.Seq
	op.Servers = args.Servers

	if !sm.appendLog(op) {
		reply.WrongLeader = true
		reply.Err = "This node is not leader."
		return
	}
	reply.WrongLeader = false
}

func (sm *ShardMaster) Leave(args *LeaveArgs, reply *LeaveReply) {
	// Your code here.
	op := Op{}
	op.Type = LEAVE
	op.GIDs = args.GIDs
	op.Cid = args.Cid
	op.Seq = args.Seq

	if !sm.appendLog(op) {
		reply.WrongLeader = true
		reply.Err = "This node is not leader"
		return
	}
	reply.WrongLeader = false
}

func (sm *ShardMaster) Move(args *MoveArgs, reply *MoveReply) {
	// Your code here.
	op := Op{}
	op.Type = MOVE
	op.Shard = args.Shard
	op.GID = args.GID
	op.Cid = args.Cid
	op.Seq = args.Seq

	if !sm.appendLog(op) {
		reply.WrongLeader = true
		reply.Err = "This node is not leader"
		return
	}
	reply.WrongLeader = false
}

func (sm *ShardMaster) Query(args *QueryArgs, reply *QueryReply) {
	// Your code here.
	op := Op{}
	op.Type = QUERY
	op.Num = args.Num
	op.Cid = args.Cid
	op.Seq = args.Seq

	if !sm.appendLog(op) {
		reply.WrongLeader = true
		reply.Err = "This node is not leader"
		return
	}

	sm.mu.Lock()
	if args.Num < 0 || args.Num >= len(sm.configs) {
		reply.Config = sm.configs[len(sm.configs)-1]
	} else {
		reply.Config = sm.configs[args.Num]
	}
	sm.mu.Unlock()
	reply.WrongLeader = false
}

//
// the tester calls Kill() when a ShardMaster instance won't
// be needed again. you are not required to do anything
// in Kill(), but it might be convenient to (for example)
// turn off debug output from this instance.
//
func (sm *ShardMaster) Kill() {
	sm.rf.Kill()
	// Your code here, if desired.
}

// needed by shardkv tester
func (sm *ShardMaster) Raft() *raft.Raft {
	return sm.rf
}

func (sm *ShardMaster) runServer() {
	for {
		msg := <-sm.applyCh
		op, _ := msg.Command.(Op)
		if !sm.checkDuplicate(op.Cid, op.Seq) {
			switch op.Type {
			case JOIN:
				sm.mu.Lock()
				config := sm.newConfig()
				for gid, servers := range op.Servers {
					config.Groups[gid] = servers
				}
				sm.updateConfig(config)
				sm.success(msg.Index, op)
				sm.mu.Unlock()
			case LEAVE:
				sm.mu.Lock()
				config := sm.newConfig()
				for _, gid := range op.GIDs {
					delete(config.Groups, gid)
				}
				sm.updateConfig(config)
				sm.success(msg.Index, op)
				sm.mu.Unlock()
			case MOVE:
				sm.mu.Lock()
				config := sm.newConfig()
				config.Shards[op.Shard] = op.GID
				sm.configs = append(sm.configs, config)
				sm.success(msg.Index, op)
				sm.mu.Unlock()
			case QUERY:
				sm.mu.Lock()
				sm.success(msg.Index, op)
				sm.mu.Unlock()
			}
		}
	}
}

func (sm *ShardMaster) newConfig() Config {
	oldConfig := sm.configs[len(sm.configs)-1]
	newConfig := Config{}
	newConfig.Num = oldConfig.Num + 1
	newConfig.Groups = make(map[int][]string)
	newConfig.Shards = [NShards]int{}
	for k, v := range oldConfig.Groups {
		servers := make([]string, len(v))
		copy(servers, v)
		newConfig.Groups[k] = servers
	}
	for i, v := range oldConfig.Shards {
		newConfig.Shards[i] = v
	}
	return newConfig
}

func (sm *ShardMaster) success(index int, op Op) {
	ch, ok := sm.result[index]
	if ok {
		ch <- op.Index
	}
}

func (sm *ShardMaster) updateConfig(config Config) {
	var gids []int
	for k := range config.Groups {
		gids = append(gids, k)
	}

	var neededMove []int       // shards needed to reassign
	g2s := make(map[int][]int) // gid --> shards
	for sid, gid := range config.Shards {
		if _, ok := config.Groups[gid]; ok {
			g2s[gid] = append(g2s[gid], sid)
		} else {
			neededMove = append(neededMove, sid)
		}
	}
	expected := len(config.Shards) / len(gids)
	if expected == 0 {
		expected = 1
	}
	for _, sids := range g2s {
		if len(sids) > expected {
			neededMove = append(neededMove, sids[expected:]...)
		}
	}

	var candidateGids []int
	for _, gid := range gids {
		if _, ok := g2s[gid]; !ok {
			candidateGids = append(candidateGids, gid)
		} else if len(g2s[gid]) < expected {
			candidateGids = append(candidateGids, gid)
		}
	}

	nIndex := 0
	for _, gid := range candidateGids {
		if sids, ok := g2s[gid]; !ok {
			end := nIndex + expected
			for ; nIndex < len(neededMove) && nIndex < end; nIndex++ {
				config.Shards[neededMove[nIndex]] = gid
			}
		} else if len(sids) < expected {
			diff := expected - len(sids)
			end := nIndex + diff
			for ; nIndex < len(neededMove) && nIndex < end; nIndex++ {
				config.Shards[neededMove[nIndex]] = gid
			}
		}
	}

	gIndex := 0
	for nIndex < len(neededMove) && len(candidateGids) != 0 {
		if gIndex == len(candidateGids) {
			gIndex = 0
		}
		config.Shards[neededMove[nIndex]] = candidateGids[gIndex]
		gIndex++
		nIndex++
	}

	sm.configs = append(sm.configs, config)
}

func (sm *ShardMaster) checkDuplicate(cid int64, seq int) bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if lastSeq, ok := sm.request[cid]; ok {
		return seq <= lastSeq
	}
	return false
}

//
// servers[] contains the ports of the set of
// servers that will cooperate via Paxos to
// form the fault-tolerant shardmaster service.
// me is the index of the current server in servers[].
//
func StartServer(servers []*labrpc.ClientEnd, me int, persister *raft.Persister) *ShardMaster {
	sm := new(ShardMaster)
	sm.me = me

	sm.configs = make([]Config, 1)
	sm.configs[0].Groups = map[int][]string{}

	gob.Register(Op{})
	sm.applyCh = make(chan raft.ApplyMsg)
	sm.result = make(map[int]chan int)
	sm.request = make(map[int64]int)
	sm.rf = raft.Make(servers, me, persister, sm.applyCh)

	// Your code here.
	go sm.runServer()

	return sm
}
