package shardkv

import (
	"time"

	logger "6.824/raft-logs"
)

//rpc handler
func (kv *ShardKV) Get(args *GetArgs, reply *GetReply) {
	//Your code here.
	request_arg := Command{
		OptType:  TYPE_GET,
		Key:      args.Key,
		ClientID: args.ClientID,
		Seq:      args.Seq,
		Num:      args.Num,
	}

	result := kv.doRequest(&request_arg).(*GetReply)
	reply.Err = result.Err
	reply.Value = result.Value
	kv.logger.L(logger.ShardKVReq, "[%3d--%d] get return result%#v\n",
		args.ClientID%1000, args.Seq, reply)

}

func (kv *ShardKV) PutAppend(args *PutAppendArgs, reply *PutAppendReply) {
	// Your code here.
	request_arg := Command{
		Key:      args.Key,
		Opt:      args.Value,
		ClientID: args.ClientID,
		Seq:      args.Seq,
		Num:      args.Num,
	}
	switch args.Op {
	case "Put":
		request_arg.OptType = TYPE_PUT
	case "Append":
		request_arg.OptType = TYPE_APPEND
	default:
		kv.logger.L(logger.ShardKVReq, "putAppend err type %d from [%3d--%d]\n",
			args.Op, args.ClientID%1000, args.Seq)
	}

	reply_arg := kv.doRequest(&request_arg).(*PutAppendReply) //wait

	reply.Err = reply_arg.Err

	kv.logger.L(logger.ShardKVReq, "[%3d--%d] putAppend return result%#v\n",
		args.ClientID%1000, args.Seq, reply)
}

//handler
func (kv *ShardKV) Migrate(args *MigrationArgs, reply *MigrationReply) {
	kv.mu.Lock()
	defer kv.mu.Unlock()

	kv.logger.L(logger.ShardKVMigration, "num %d handle num %d move rpc %v\n",
		kv.config.Num, args.Num, args.ShardsIndexes)
	reply.Num = kv.config.Num
	reply.Ok = false
	if args.Num > kv.config.Num {
		return
	}
	if args.Num < kv.config.Num {
		reply.Ok = true
		return
	}
	if kv.isMigrationDone(args) {
		reply.Ok = true
		return
	}
	command := &Command{
		OptType: TYPE_MIGRATE,
		Opt:     *args,
	}
	index, _, isLeader := kv.rf.Start(*command)
	if !isLeader {
		kv.logger.L(logger.ShardKVMigration, "move handle not leader\n")
		return
	} else {
		kv.logger.L(logger.ShardKVMigration,
			"move handle num %d shards %v as leader?\n", args.Num, args.ShardsIndexes)
	}
	wait := kv.getWaitChan(index)
	kv.mu.Unlock()
	timer := time.NewTimer(time.Millisecond * 100)

	select {
	case <-timer.C:
	case <-wait:
	}

	kv.mu.Lock()
	reply.Num = kv.config.Num
	if args.Num <= kv.config.Num && kv.isMigrationDone(args) {

		reply.Ok = true
	}

}

func (kv *ShardKV) isMigrationDone(args *MigrationArgs) bool {
	for _, shardIndex := range args.ShardsIndexes {
		if kv.pendingShards[shardIndex] {
			return false
		}
	}
	kv.logger.L(logger.ShardKVMigration, "num %d move %v is done yet\n", args.Num, args.ShardsIndexes)
	return true
}
