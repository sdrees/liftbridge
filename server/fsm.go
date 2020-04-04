package server

import (
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"
	"strconv"

	"github.com/dustin/go-humanize/english"
	"github.com/hashicorp/raft"
	"github.com/pkg/errors"

	proto "github.com/liftbridge-io/liftbridge/server/protocol"
)

// recoverLatestCommittedFSMLog returns the last committed Raft FSM log entry.
// It returns nil if there are no entries in the Raft log.
func (s *Server) recoverLatestCommittedFSMLog(applyIndex uint64) (*raft.Log, error) {
	raftNode := s.getRaft()
	commitIndex, err := strconv.ParseUint(raftNode.Stats()["commit_index"], 10, 64)
	if err != nil {
		return nil, err
	}
	firstIndex, err := raftNode.store.FirstIndex()
	if err != nil {
		return nil, err
	}
	if firstIndex == 0 {
		// No entries.
		return nil, nil
	}
	log := &raft.Log{}
	for i := commitIndex; i >= firstIndex; i-- {
		if i == applyIndex && applyIndex == commitIndex {
			// We are committing the first FSM log.
			return nil, nil
		}
		if err := raftNode.store.GetLog(i, log); err != nil {
			return nil, err
		}
		if log.Type == raft.LogCommand {
			return log, nil
		}
	}
	return nil, nil
}

// Apply applies a Raft log entry to the controller FSM. This is invoked by
// Raft once a log entry is committed. It returns a value which will be made
// available on the ApplyFuture returned by Raft.Apply if that method was
// called on the same Raft node as the FSM.
//
// Note that, on restart, this can be called for entries that have already been
// committed to Raft as part of the recovery process. As such, this should be
// an idempotent call.
func (s *Server) Apply(l *raft.Log) interface{} {
	// If recoveryStarted is false, the server was just started. We are going
	// to recover the last committed Raft FSM log entry, if any, to determine
	// the recovery high watermark. Once we apply all entries up to that point,
	// we know we've completed the recovery process and subsequent entries are
	// newly committed operations. During the recovery process, any recovered
	// streams will not be started until recovery is finished to avoid starting
	// streams in an intermediate state. When recovery completes, we'll call
	// finishedRecovery() to start the recovered streams.
	if !s.recoveryStarted {
		lastCommittedLog, err := s.recoverLatestCommittedFSMLog(l.Index)
		// If this returns an error, something is very wrong.
		if err != nil {
			panic(err)
		}
		s.latestRecoveredLog = lastCommittedLog
		s.recoveryStarted = true
		if s.latestRecoveredLog != nil {
			s.logger.Debug("fsm: Replaying Raft log...")
			s.startedRecovery()
		}
	}

	// Check if this is a "recovered" Raft entry, meaning we are still applying
	// logs up to and including the latest recovered log.
	recovered := false
	if s.latestRecoveredLog != nil && l.Index <= s.latestRecoveredLog.Index {
		recovered = true
		if l.Index == s.latestRecoveredLog.Index {
			// We've applied all entries up to the latest recovered log, so
			// recovery is finished. Call finishedRecovery() to start any
			// recovered streams.
			defer func() {
				count, err := s.finishedRecovery()
				if err != nil {
					panic(fmt.Sprintf("failed to recover from Raft log: %v", err))
				}
				s.logger.Debugf("fsm: Finished replaying Raft log, recovered %s",
					english.Plural(count, "stream", ""))
			}()
			s.latestRecoveredLog = nil
		}
	}

	// Unmarshal the log data and apply the operation to the FSM.
	log := &proto.RaftLog{}
	if err := log.Unmarshal(l.Data); err != nil {
		panic(err)
	}
	value, err := s.apply(log, l.Index, recovered)
	if err != nil {
		if s.isShutdown() {
			// Don't panic if the server is shutting down, just return the
			// error.
			return err
		}
		panic(err)
	}
	return value
}

// apply the given RaftLog to the FSM. This returns a value, if any, which
// should be made available on the ApplyFuture returned by Raft.Apply if that
// method was called on the same Raft node as the FSM. An error is returned if
// the operation could not be applied. The index parameter is the index of the
// entry in the Raft log. The recovered parameter indicates if this entry is
// being applied during the recovery process.
func (s *Server) apply(log *proto.RaftLog, index uint64, recovered bool) (interface{}, error) {
	switch log.Op {
	case proto.Op_CREATE_PARTITION:
		partition := log.CreatePartitionOp.Partition
		// Make sure to set the leader epoch on the stream.
		partition.LeaderEpoch = index
		partition.Epoch = index
		err := s.applyCreatePartition(partition, recovered)
		// If err is ErrPartitionExists, we want to return this value back to
		// the caller.
		if err == ErrPartitionExists {
			return err, nil
		}
		if err != nil {
			return nil, err
		}
	case proto.Op_SHRINK_ISR:
		var (
			stream    = log.ShrinkISROp.Stream
			replica   = log.ShrinkISROp.ReplicaToRemove
			partition = log.ShrinkISROp.Partition
		)
		if err := s.applyShrinkISR(stream, replica, partition, index); err != nil {
			return nil, err
		}
	case proto.Op_CHANGE_LEADER:
		var (
			stream    = log.ChangeLeaderOp.Stream
			leader    = log.ChangeLeaderOp.Leader
			partition = log.ChangeLeaderOp.Partition
		)
		if err := s.applyChangeStreamLeader(stream, leader, partition, index); err != nil {
			return nil, err
		}
	case proto.Op_EXPAND_ISR:
		var (
			stream    = log.ExpandISROp.Stream
			replica   = log.ExpandISROp.ReplicaToAdd
			partition = log.ExpandISROp.Partition
		)
		if err := s.applyExpandISR(stream, replica, partition, index); err != nil {
			return nil, err
		}
	case proto.Op_DELETE_STREAM:
		var (
			stream = log.DeleteStreamOp.Stream
		)
		err := s.applyDeleteStream(stream)
		// If err is ErrStreamNotFound, we want to return this value back to the
		// caller.
		if err == ErrStreamNotFound {
			return err, nil
		}
		if err != nil {
			return nil, err
		}
	case proto.Op_PAUSE_STREAM:
		var (
			stream     = log.PauseStreamOp.Stream
			partitions = log.PauseStreamOp.Partitions
			resumeAll  = log.PauseStreamOp.ResumeAll
		)
		err := s.applyPauseStream(stream, partitions, resumeAll)
		// If err is ErrStreamNotFound or ErrPartitionNotFound, we want to
		// return this value back to the caller.
		if err == ErrStreamNotFound || err == ErrPartitionNotFound {
			return err, nil
		}
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("Unknown Raft operation: %s", log.Op)
	}
	return nil, nil
}

// startedRecovery should be called when the FSM has started replaying any
// unapplied log entries.
func (s *Server) startedRecovery() {
	if s.config.LogRecovery {
		return
	}
	// If LogRecovery is disabled, we need to suppress logs while replaying the
	// Raft log. Do this by discarding the log output.
	s.loggerOut = s.logger.Writer()
	s.logger.SetWriter(ioutil.Discard)
}

// finishedRecovery should be called when the FSM has finished replaying any
// unapplied log entries. This will start any stream partitions recovered
// during the replay. It returns the number of streams which had partitions
// that were recovered.
func (s *Server) finishedRecovery() (int, error) {
	// If LogRecovery is disabled, we need to restore the previous log output.
	if !s.config.LogRecovery {
		s.logger.SetWriter(s.loggerOut)
	}
	recoveredStreams := make(map[string]struct{})
	for _, stream := range s.metadata.GetStreams() {
		for _, partition := range stream.GetPartitions() {
			recovered, err := partition.StartRecovered()
			if err != nil {
				return 0, err
			}
			if recovered {
				recoveredStreams[stream.GetName()] = struct{}{}
			}
		}
	}
	return len(recoveredStreams), nil
}

// fsmSnapshot is returned by an FSM in response to a Snapshot. It must be safe
// to invoke fsmSnapshot methods with concurrent calls to Apply.
type fsmSnapshot struct {
	*proto.MetadataSnapshot
}

// Persist should dump all necessary state to the WriteCloser sink and call
// sink.Close() when finished or call sink.Cancel() on error.
func (f *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	err := func() error {
		// Encode data.
		b, err := f.Marshal()
		if err != nil {
			return err
		}

		// Write size and data to sink.
		sizeBuf := make([]byte, 4)
		binary.BigEndian.PutUint32(sizeBuf, uint32(len(b)))
		if _, err := sink.Write(sizeBuf); err != nil {
			return err
		}
		if _, err := sink.Write(b); err != nil {
			return err
		}

		// Close the sink.
		return sink.Close()
	}()

	if err != nil {
		sink.Cancel()
	}

	return err
}

// Release is invoked when we are finished with the snapshot.
func (f *fsmSnapshot) Release() {}

// Snapshot is used to support log compaction. This call should return an
// FSMSnapshot which can be used to save a point-in-time snapshot of the FSM.
// Apply and Snapshot are not called in multiple threads, but Apply will be
// called concurrently with Persist. This means the FSM should be implemented
// in a fashion that allows for concurrent updates while a snapshot is
// happening.
func (s *Server) Snapshot() (raft.FSMSnapshot, error) {
	var (
		streams    = s.metadata.GetStreams()
		partitions = make([]*proto.Partition, 0, len(streams))
	)
	for _, stream := range streams {
		for _, partition := range stream.GetPartitions() {
			partitions = append(partitions, partition.Partition)
		}
	}
	return &fsmSnapshot{&proto.MetadataSnapshot{Partitions: partitions}}, nil
}

// Restore is used to restore an FSM from a snapshot. It is not called
// concurrently with any other command. The FSM must discard all previous
// state.
func (s *Server) Restore(snapshot io.ReadCloser) error {
	s.logger.Debug("fsm: Restoring Raft state from snapshot...")
	defer snapshot.Close()

	// Read snapshot size.
	sizeBuf := make([]byte, 4)
	if _, err := io.ReadFull(snapshot, sizeBuf); err != nil {
		return err
	}
	// Read snapshot.
	size := binary.BigEndian.Uint32(sizeBuf)
	buf := make([]byte, size)
	if _, err := io.ReadFull(snapshot, buf); err != nil {
		return err
	}
	snap := &proto.MetadataSnapshot{}
	if err := snap.Unmarshal(buf); err != nil {
		return err
	}

	// Drop state and restore.
	if err := s.metadata.Reset(); err != nil {
		return err
	}
	recoveredStreams := make(map[string]struct{})
	for _, partition := range snap.Partitions {
		if err := s.applyCreatePartition(partition, false); err != nil {
			return err
		}
		recoveredStreams[partition.Stream] = struct{}{}
	}
	s.logger.Debugf("fsm: Finished restoring Raft state from snapshot, recovered %s",
		english.Plural(len(recoveredStreams), "stream", ""))
	return nil
}

// applyCreatePartition adds the given stream partition to the metadata store.
// If the partition is being recovered, it will not be started until after the
// recovery process completes. If it is not being recovered, the partition will
// be started as a leader or follower if applicable. ErrPartitionExists is
// returned if the partition already exists.
func (s *Server) applyCreatePartition(protoPartition *proto.Partition, recovered bool) error {
	// QUESTION: If this broker is not a replica for the stream, can we just
	// store a "lightweight" representation of the stream (i.e. the protobuf)
	// for recovery purposes? There is no need to initialize a commit log for
	// it.
	partition, err := s.metadata.AddPartition(protoPartition, recovered)
	if err == ErrPartitionExists {
		return err
	}
	if err != nil {
		return errors.Wrap(err, "failed to add partition to metadata store")
	}
	s.logger.Debugf("fsm: Created partition %s", partition)
	return nil
}

// applyShrinkISR removes the given replica from the partition and updates the
// partition epoch. If the partition epoch is greater than or equal to the
// specified epoch, this does nothing.
func (s *Server) applyShrinkISR(stream, replica string, partitionID int32, epoch uint64) error {
	partition := s.metadata.GetPartition(stream, partitionID)
	if partition == nil {
		return fmt.Errorf("No such partition [stream=%s, partition=%d]", stream, partitionID)
	}

	// Idempotency check.
	if partition.GetEpoch() >= epoch {
		return nil
	}

	if err := partition.RemoveFromISR(replica); err != nil {
		return errors.Wrap(err, fmt.Sprintf("failed to remove %s from ISR for partition %s",
			replica, partition))
	}

	partition.SetEpoch(epoch)

	s.logger.Warnf("fsm: Removed replica %s from ISR for partition %s", replica, partition)
	return nil
}

// applyExpandISR adds the given replica to the partition and updates the
// partition epoch. If the partition epoch is greater than or equal to the
// specified epoch, this does nothing.
func (s *Server) applyExpandISR(stream, replica string, partitionID int32, epoch uint64) error {
	partition := s.metadata.GetPartition(stream, partitionID)
	if partition == nil {
		return fmt.Errorf("No such partition [stream=%s, partition=%d]", stream, partitionID)
	}

	// Idempotency check.
	if partition.GetEpoch() >= epoch {
		return nil
	}

	if err := partition.AddToISR(replica); err != nil {
		return errors.Wrap(err, fmt.Sprintf("failed to add %s to ISR for partition %s",
			replica, partition))
	}

	partition.SetEpoch(epoch)

	s.logger.Infof("fsm: Added replica %s to ISR for partition %s", replica, partition)
	return nil
}

// applyChangeStreamLeader sets the partition's leader to the given replica and
// updates the partition epoch. If the partition epoch is greater than or equal
// to the specified epoch, this does nothing.
func (s *Server) applyChangeStreamLeader(stream, leader string, partitionID int32, epoch uint64) error {
	partition := s.metadata.GetPartition(stream, partitionID)
	if partition == nil {
		return fmt.Errorf("No such partition [stream=%s, partition=%d]", stream, partitionID)
	}

	// Idempotency check.
	if partition.GetEpoch() >= epoch {
		return nil
	}

	if err := partition.SetLeader(leader, epoch); err != nil {
		return errors.Wrap(err, "failed to change partition leader")
	}

	partition.SetEpoch(epoch)

	s.logger.Debugf("fsm: Changed leader for partition %s to %s", partition, leader)
	return nil
}

// applyDeleteStream deletes the given stream partition.
func (s *Server) applyDeleteStream(streamName string) error {
	stream := s.metadata.GetStream(streamName)
	if stream == nil {
		return ErrStreamNotFound
	}

	err := s.metadata.CloseAndDeleteStream(stream)
	if err != nil {
		return errors.Wrap(err, "failed to delete stream")
	}

	s.logger.Debugf("fsm: Deleted stream %s", streamName)
	return nil
}

// applyPauseStream pauses the given stream partitions.
func (s *Server) applyPauseStream(streamName string, partitions []int32, resumeAll bool) error {
	stream := s.metadata.GetStream(streamName)
	if stream == nil {
		return ErrStreamNotFound
	}

	err := stream.Pause(partitions, resumeAll)
	if err != nil {
		if err == ErrPartitionNotFound {
			return err
		}
		return errors.Wrap(err, "failed to pause stream")
	}

	s.logger.Debugf("fsm: Paused stream %s", streamName)
	return nil
}
