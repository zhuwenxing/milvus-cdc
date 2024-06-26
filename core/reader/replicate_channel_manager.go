/*
 * Licensed to the LF AI & Data foundation under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 * //
 *     http://www.apache.org/licenses/LICENSE-2.0
 * //
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package reader

import (
	"context"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/samber/lo"
	"github.com/sasha-s/go-deadlock"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/msgpb"
	"github.com/milvus-io/milvus/pkg/mq/msgstream"
	"github.com/milvus-io/milvus/pkg/mq/msgstream/mqwrapper"
	"github.com/milvus-io/milvus/pkg/util/commonpbutil"
	"github.com/milvus-io/milvus/pkg/util/conc"
	"github.com/milvus-io/milvus/pkg/util/retry"

	"github.com/zilliztech/milvus-cdc/core/api"
	"github.com/zilliztech/milvus-cdc/core/config"
	"github.com/zilliztech/milvus-cdc/core/log"
	"github.com/zilliztech/milvus-cdc/core/model"
	"github.com/zilliztech/milvus-cdc/core/pb"
	"github.com/zilliztech/milvus-cdc/core/util"
)

var _ api.ChannelManager = (*replicateChannelManager)(nil)

var replicatePool = conc.NewPool[struct{}](10, conc.WithExpiryDuration(time.Minute))

type replicateChannelManager struct {
	replicateCtx context.Context
	factory      msgstream.Factory
	targetClient api.TargetAPI
	metaOp       api.MetaOp

	retryOptions          []retry.Option
	startReadRetryOptions []retry.Option
	messageBufferSize     int
	ttInterval            int

	channelLock       deadlock.Mutex
	channelHandlerMap map[string]*replicateChannelHandler
	channelForwardMap map[string]struct{}

	collectionLock       deadlock.RWMutex
	replicateCollections map[int64]chan struct{}

	partitionLock       deadlock.Mutex
	replicatePartitions map[int64]map[int64]chan struct{}

	channelChan             chan string
	apiEventChan            chan *api.ReplicateAPIEvent
	forwardReplicateChannel chan string

	msgPackCallback func(string, *msgstream.MsgPack)

	droppedCollections util.Map[int64, struct{}]
	droppedPartitions  util.Map[int64, struct{}]
}

func NewReplicateChannelManager(mqConfig config.MQConfig,
	factoryCreator FactoryCreator,
	client api.TargetAPI,
	readConfig config.ReaderConfig,
	metaOp api.MetaOp,
	msgPackCallback func(string, *msgstream.MsgPack),
) (api.ChannelManager, error) {
	var factory msgstream.Factory
	switch {
	case mqConfig.Pulsar.Address != "":
		factory = factoryCreator.NewPmsFactory(&mqConfig.Pulsar)
	case mqConfig.Kafka.Address != "":
		factory = factoryCreator.NewKmsFactory(&mqConfig.Kafka)
	default:
		log.Warn("mqConfig is empty")
		return nil, errors.New("fail to get the msg stream, check the mqConfig param")
	}

	return &replicateChannelManager{
		factory:      factory,
		targetClient: client,
		metaOp:       metaOp,
		retryOptions: util.GetRetryOptions(readConfig.Retry),
		startReadRetryOptions: util.GetRetryOptions(config.RetrySettings{
			RetryTimes:  readConfig.Retry.RetryTimes,
			InitBackOff: readConfig.Retry.InitBackOff,
			MaxBackOff:  readConfig.Retry.InitBackOff,
		}),
		messageBufferSize:       readConfig.MessageBufferSize,
		ttInterval:              readConfig.TTInterval,
		channelHandlerMap:       make(map[string]*replicateChannelHandler),
		channelForwardMap:       make(map[string]struct{}),
		replicateCollections:    make(map[int64]chan struct{}),
		replicatePartitions:     make(map[int64]map[int64]chan struct{}),
		channelChan:             make(chan string, 10),
		apiEventChan:            make(chan *api.ReplicateAPIEvent, 10),
		forwardReplicateChannel: make(chan string),
		msgPackCallback:         msgPackCallback,
	}, nil
}

func (r *replicateChannelManager) SetCtx(ctx context.Context) {
	r.replicateCtx = ctx
}

func (r *replicateChannelManager) getCtx() context.Context {
	if r.replicateCtx == nil {
		return context.Background()
	}
	return r.replicateCtx
}

func IsCollectionNotFoundError(err error) bool {
	return strings.Contains(err.Error(), "collection not found") || strings.Contains(err.Error(), "can't find collection")
}

func IsDatabaseNotFoundError(err error) bool {
	return err == util.NotFoundDatabase
}

func (r *replicateChannelManager) AddDroppedCollection(ids []int64) {
	for _, id := range ids {
		r.droppedCollections.Store(id, struct{}{})
	}
	log.Info("has added dropped collection", zap.Int64s("ids", ids))
}

func (r *replicateChannelManager) AddDroppedPartition(ids []int64) {
	for _, id := range ids {
		r.droppedPartitions.Delete(id)
	}
	log.Info("has removed dropped partitions", zap.Int64s("ids", ids))
}

func (r *replicateChannelManager) StartReadCollection(ctx context.Context, info *pb.CollectionInfo, seekPositions []*msgpb.MsgPosition) error {
	sourceDBInfo := r.metaOp.GetDatabaseInfoForCollection(ctx, info.ID)

	var err error
	retryErr := retry.Do(ctx, func() error {
		_, err = r.targetClient.GetCollectionInfo(ctx, info.Schema.GetName(), sourceDBInfo.Name)
		if err != nil &&
			!IsCollectionNotFoundError(err) && !IsDatabaseNotFoundError(err) {
			return err
		}
		r.collectionLock.RLock()
		_, ok := r.replicateCollections[info.ID]
		r.collectionLock.RUnlock()
		if ok {
			return errors.Newf("the collection has been replicated, wait it [collection name: %s] to drop...", info.Schema.Name)
		}
		// collection not found will exit the retry
		return nil
	}, r.startReadRetryOptions...)

	if retryErr != nil {
		return retryErr
	}

	if err != nil {
		// the collection is not existed in the target and source collection has dropped, skip it
		if info.State == pb.CollectionState_CollectionDropped || info.State == pb.CollectionState_CollectionDropping {
			r.droppedCollections.Store(info.ID, struct{}{})
			log.Info("the collection is dropped in the target instance",
				zap.Int64("collection_id", info.ID), zap.String("collection_name", info.Schema.Name))
			return nil
		}
		if IsDatabaseNotFoundError(err) {
			log.Panic("the database has been dropped but the collection is existed in the source instance",
				zap.String("collection_name", info.Schema.Name))
			return nil
		}
		select {
		case <-ctx.Done():
			log.Warn("context is done in the start read collection")
			return ctx.Err()
		default:
			r.apiEventChan <- &api.ReplicateAPIEvent{
				EventType:      api.ReplicateCreateCollection,
				CollectionInfo: info,
				ReplicateInfo: &commonpb.ReplicateInfo{
					IsReplicate:  true,
					MsgTimestamp: info.CreateTime,
				},
				ReplicateParam: api.ReplicateParam{Database: sourceDBInfo.Name},
			}
		}
	}

	var targetInfo *model.CollectionInfo

	err = retry.Do(ctx, func() error {
		targetInfo, err = r.targetClient.GetCollectionInfo(ctx, info.Schema.Name, sourceDBInfo.Name)
		return err
	}, r.startReadRetryOptions...)
	if err != nil {
		log.Warn("failed to get target collection info", zap.Error(err))
		return err
	}
	log.Info("success to get the collection info in the target instance", zap.String("collection_name", targetInfo.CollectionName))
	if info.State == pb.CollectionState_CollectionDropped || info.State == pb.CollectionState_CollectionDropping {
		targetInfo.Dropped = true
		log.Info("the collection is dropped in the source instance and it's existed in the target instance", zap.String("collection_name", info.Schema.Name))
	}

	getSeekPosition := func(channelName string) *msgpb.MsgPosition {
		for _, seekPosition := range seekPositions {
			if seekPosition.ChannelName == channelName {
				return seekPosition
			}
		}
		return nil
	}

	r.collectionLock.Lock()
	if _, ok := r.replicateCollections[info.ID]; ok {
		r.collectionLock.Unlock()
		log.Info("the collection is already replicated", zap.String("collection_name", info.Schema.Name))
		return nil
	}

	targetMsgCount := len(info.StartPositions)
	barrier := NewBarrier(targetMsgCount, func(msgTs uint64, b *Barrier) {
		select {
		case <-b.CloseChan:
		case r.apiEventChan <- &api.ReplicateAPIEvent{
			EventType:      api.ReplicateDropCollection,
			CollectionInfo: info,
			ReplicateInfo: &commonpb.ReplicateInfo{
				IsReplicate:  true,
				MsgTimestamp: msgTs,
			},
			ReplicateParam: api.ReplicateParam{Database: targetInfo.DatabaseName},
		}:
			r.droppedCollections.Store(info.ID, struct{}{})
			for _, name := range info.PhysicalChannelNames {
				r.stopReadChannel(name, info.ID)
			}
		}
	})
	r.replicateCollections[info.ID] = barrier.CloseChan
	r.collectionLock.Unlock()

	var successChannels []string
	var channelHandlers []*replicateChannelHandler
	err = ForeachChannel(info.PhysicalChannelNames, targetInfo.PChannels, func(sourcePChannel, targetPChannel string) error {
		channelHandler, err := r.startReadChannel(&model.SourceCollectionInfo{
			PChannelName: sourcePChannel,
			CollectionID: info.ID,
			SeekPosition: getSeekPosition(sourcePChannel),
		}, &model.TargetCollectionInfo{
			DatabaseName:         targetInfo.DatabaseName,
			CollectionID:         targetInfo.CollectionID,
			CollectionName:       info.Schema.Name,
			PartitionInfo:        targetInfo.Partitions,
			PChannel:             targetPChannel,
			VChannel:             GetVChannelByPChannel(targetPChannel, targetInfo.VChannels),
			BarrierChan:          util.NewOnceWriteChan(barrier.BarrierChan),
			PartitionBarrierChan: make(map[int64]*util.OnceWriteChan[uint64]),
			Dropped:              targetInfo.Dropped,
			DroppedPartition:     make(map[int64]struct{}),
		})
		if err != nil {
			log.Warn("start read channel failed", zap.String("channel", sourcePChannel), zap.Int64("collection_id", info.ID), zap.Error(err))
			for _, channel := range successChannels {
				r.stopReadChannel(channel, info.ID)
			}
			return err
		}
		if channelHandler != nil {
			channelHandlers = append(channelHandlers, channelHandler)
		}
		successChannels = append(successChannels, sourcePChannel)
		log.Info("start read channel",
			zap.String("channel", sourcePChannel),
			zap.String("target_channel", targetPChannel),
			zap.Int64("collection_id", info.ID))
		return nil
	})

	for _, channelHandler := range channelHandlers {
		channelHandler.startReadChannel()
		log.Info("start read the source channel", zap.String("channel_name", channelHandler.pChannelName))
	}
	return err
}

func GetVChannelByPChannel(pChannel string, vChannels []string) string {
	for _, vChannel := range vChannels {
		if strings.Contains(vChannel, pChannel) {
			return vChannel
		}
	}
	return ""
}

func ForeachChannel(sourcePChannels, targetPChannels []string, f func(sourcePChannel, targetPChannel string) error) error {
	if len(sourcePChannels) != len(targetPChannels) {
		return errors.New("the lengths of source and target channels are not equal")
	}
	sources := make([]string, len(sourcePChannels))
	targets := make([]string, len(targetPChannels))
	copy(sources, sourcePChannels)
	copy(targets, targetPChannels)
	sort.Strings(sources)
	sort.Strings(targets)

	for i, source := range sources {
		if err := f(source, targets[i]); err != nil {
			return err
		}
	}
	return nil
}

func (r *replicateChannelManager) AddPartition(ctx context.Context, collectionInfo *pb.CollectionInfo, partitionInfo *pb.PartitionInfo) error {
	var handlers []*replicateChannelHandler
	collectionID := collectionInfo.ID
	partitionLog := log.With(zap.Int64("partition_id", partitionInfo.PartitionID), zap.Int64("collection_id", collectionID),
		zap.String("collection_name", collectionInfo.Schema.Name), zap.String("partition_name", partitionInfo.PartitionName))
	sourceDBInfo := r.metaOp.GetDatabaseInfoForCollection(ctx, collectionID)

	if sourceDBInfo.Dropped {
		partitionLog.Warn("the database has been dropped when add partition")
		return nil
	}

	_ = retry.Do(ctx, func() error {
		if _, dropped := r.droppedCollections.Load(collectionID); dropped {
			partitionLog.Info("the collection is dropped when add partition")
			return nil
		}
		r.channelLock.Lock()
		for _, handler := range r.channelHandlerMap {
			handler.recordLock.RLock()
			if _, ok := handler.collectionRecords[collectionID]; ok {
				handlers = append(handlers, handler)
			}
			handler.recordLock.RUnlock()
		}
		r.channelLock.Unlock()
		if len(handlers) == 0 {
			partitionLog.Info("waiting handler", zap.Int64("collection_id", collectionID))
			return errors.New("no handler found")
		}
		return nil
	}, r.retryOptions...)
	if _, dropped := r.droppedCollections.Load(collectionID); dropped {
		return nil
	}

	if len(handlers) == 0 {
		partitionLog.Warn("no handler found", zap.Int64("collection_id", collectionID))
		return errors.New("no handler found")
	}

	firstHandler := handlers[0]
	targetInfo, err := firstHandler.getCollectionTargetInfo(collectionID)
	if err != nil {
		return err
	}
	if targetInfo == nil || targetInfo.Dropped {
		partitionLog.Info("the collection is dropping or dropped, skip the partition")
		return nil
	}
	firstHandler.recordLock.RLock()
	partitionRecord := targetInfo.PartitionInfo
	_, ok := partitionRecord[partitionInfo.PartitionName]
	firstHandler.recordLock.RUnlock()
	if !ok {
		if partitionInfo.State == pb.PartitionState_PartitionDropping ||
			partitionInfo.State == pb.PartitionState_PartitionDropped {
			r.droppedPartitions.Store(partitionInfo.PartitionID, struct{}{})
			partitionLog.Warn("the partition is dropped in the source and target")
			return nil
		}
		select {
		case r.apiEventChan <- &api.ReplicateAPIEvent{
			EventType:      api.ReplicateCreatePartition,
			CollectionInfo: collectionInfo,
			PartitionInfo:  partitionInfo,
			ReplicateInfo: &commonpb.ReplicateInfo{
				IsReplicate:  true,
				MsgTimestamp: partitionInfo.PartitionCreatedTimestamp,
			},
			ReplicateParam: api.ReplicateParam{Database: sourceDBInfo.Name},
		}:
		case <-ctx.Done():
			partitionLog.Warn("context is done when adding partition")
			return ctx.Err()
		}
	}
	partitionLog.Info("start to add partition", zap.Int("num", len(handlers)))
	barrier := NewBarrier(len(handlers), func(msgTs uint64, b *Barrier) {
		select {
		case <-b.CloseChan:
			log.Info("close barrier")
		case r.apiEventChan <- &api.ReplicateAPIEvent{
			EventType:      api.ReplicateDropPartition,
			CollectionInfo: collectionInfo,
			PartitionInfo:  partitionInfo,
			ReplicateInfo: &commonpb.ReplicateInfo{
				IsReplicate:  true,
				MsgTimestamp: msgTs,
			},
			ReplicateParam: api.ReplicateParam{Database: sourceDBInfo.Name},
		}:
			r.droppedPartitions.Store(partitionInfo.PartitionID, struct{}{})
			for _, handler := range handlers {
				handler.RemovePartitionInfo(collectionID, partitionInfo.PartitionName, partitionInfo.PartitionID)
			}
		}
	})
	r.partitionLock.Lock()
	if _, ok := r.replicatePartitions[collectionID]; !ok {
		r.replicatePartitions[collectionID] = make(map[int64]chan struct{})
	}
	r.replicatePartitions[collectionID][partitionInfo.PartitionID] = barrier.CloseChan
	r.partitionLock.Unlock()
	for _, handler := range handlers {
		err = handler.AddPartitionInfo(collectionInfo, partitionInfo, barrier.BarrierChan)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *replicateChannelManager) StopReadCollection(ctx context.Context, info *pb.CollectionInfo) error {
	for _, position := range info.StartPositions {
		r.stopReadChannel(position.GetKey(), info.ID)
	}
	r.collectionLock.Lock()
	closeChan, ok := r.replicateCollections[info.ID]
	if ok {
		select {
		case <-closeChan:
		default:
			close(closeChan)
		}
		delete(r.replicateCollections, info.ID)
	}
	r.collectionLock.Unlock()
	r.partitionLock.Lock()
	partitions, ok := r.replicatePartitions[info.ID]
	if ok {
		for _, closeChan := range partitions {
			select {
			case <-closeChan:
			default:
				close(closeChan)
			}
		}
		delete(r.replicatePartitions, info.ID)
	}
	r.partitionLock.Unlock()
	return nil
}

func (r *replicateChannelManager) GetChannelChan() <-chan string {
	return r.channelChan
}

func (r *replicateChannelManager) GetMsgChan(pChannel string) <-chan *msgstream.MsgPack {
	r.channelLock.Lock()
	defer r.channelLock.Unlock()

	handler := r.channelHandlerMap[pChannel]
	if handler != nil {
		return handler.msgPackChan
	}
	return nil
}

func (r *replicateChannelManager) GetEventChan() <-chan *api.ReplicateAPIEvent {
	return r.apiEventChan
}

// startReadChannel start read channel
// pChannelName: source milvus channel name, collectionID: source milvus collection id, startPosition: start position of the source milvus collection
// targetInfo: target collection info, it will be used to replace the message info in the source milvus channel
func (r *replicateChannelManager) startReadChannel(sourceInfo *model.SourceCollectionInfo, targetInfo *model.TargetCollectionInfo) (*replicateChannelHandler, error) {
	r.channelLock.Lock()
	defer r.channelLock.Unlock()
	var err error

	channelLog := log.With(
		zap.Int64("collection_id", sourceInfo.CollectionID),
		zap.String("collection_name", targetInfo.CollectionName),
		zap.String("source_channel", sourceInfo.PChannelName),
		zap.String("target_channel", targetInfo.PChannel),
	)

	channelHandler, ok := r.channelHandlerMap[sourceInfo.PChannelName]
	if !ok {
		channelHandler, err = newReplicateChannelHandler(r.getCtx(),
			sourceInfo, targetInfo,
			r.targetClient, r.metaOp, r.apiEventChan,
			&model.HandlerOpts{
				MessageBufferSize: r.messageBufferSize,
				TTInterval:        r.ttInterval,
				Factory:           r.factory,
				RetryOptions:      r.retryOptions,
			})
		if err != nil {
			channelLog.Warn("fail to new replicate channel handler", zap.Error(err))
			return nil, err
		}
		channelHandler.msgPackCallback = r.msgPackCallback
		channelHandler.forwardMsgFunc = r.forwardMsg
		channelHandler.isDroppedCollection = r.isDroppedCollection
		channelHandler.isDroppedPartition = r.isDroppedPartition
		hasReplicateForTargetChannel := false
		for _, handler := range r.channelHandlerMap {
			handler.recordLock.RLock()
			if handler.targetPChannel == targetInfo.PChannel {
				hasReplicateForTargetChannel = true
			}
			handler.recordLock.RUnlock()
		}
		if hasReplicateForTargetChannel {
			channelLog.Info("channel already has replicate for target channel", zap.String("channel_name", sourceInfo.PChannelName))
			r.waitChannel(sourceInfo, targetInfo, channelHandler)
		} else {
			r.channelForwardMap[channelHandler.targetPChannel] = struct{}{}
		}
		r.channelHandlerMap[sourceInfo.PChannelName] = channelHandler
		r.channelChan <- sourceInfo.PChannelName
		if !hasReplicateForTargetChannel {
			return channelHandler, nil
		}
		return nil, nil
	}
	if channelHandler.targetPChannel != targetInfo.PChannel {
		log.Info("diff target pchannel", zap.String("target_channel", targetInfo.PChannel), zap.String("handler_channel", channelHandler.targetPChannel))
		r.forwardChannel(targetInfo)
	}
	channelHandler.AddCollection(sourceInfo.CollectionID, targetInfo)
	return nil, nil
}

func (r *replicateChannelManager) waitChannel(sourceInfo *model.SourceCollectionInfo, targetInfo *model.TargetCollectionInfo, channelHandler *replicateChannelHandler) {
	go func() {
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				log.Info("wait the new replicate channel", zap.String("target_pchannel", targetInfo.PChannel))
			case targetChannel := <-r.forwardReplicateChannel:
				isRepeatedChannel := false
				r.channelLock.Lock()
				for _, handler := range r.channelHandlerMap {
					handler.recordLock.RLock()
					if handler.targetPChannel == targetChannel {
						isRepeatedChannel = true
					}
					handler.recordLock.RUnlock()
				}
				if isRepeatedChannel {
					r.channelLock.Unlock()
					continue
				}
				log.Info("success to get the new replicate channel", zap.String("source_pchannel", sourceInfo.PChannelName), zap.String("target_pchannel", targetInfo.PChannel))
				channelHandler.targetPChannel = targetChannel
				r.channelForwardMap[channelHandler.targetPChannel] = struct{}{}
				channelHandler.startReadChannel()
				r.channelLock.Unlock()
				return
			}
		}
	}()
}

func (r *replicateChannelManager) forwardChannel(targetInfo *model.TargetCollectionInfo) {
	go func() {
		r.channelLock.Lock()
		_, hasForward := r.channelForwardMap[targetInfo.PChannel]
		if !hasForward {
			r.channelForwardMap[targetInfo.PChannel] = struct{}{}
		}
		r.channelLock.Unlock()
		if !hasForward {
			tick := time.NewTicker(5 * time.Second)
			defer tick.Stop()
			for {
				select {
				case <-tick.C:
					log.Info("forward the diff replicate channel", zap.String("target_channel", targetInfo.PChannel))
				case r.forwardReplicateChannel <- targetInfo.PChannel:
					log.Info("success to forward the diff replicate channel", zap.String("target_channel", targetInfo.PChannel))
					return
				}
			}
		}
	}()
}

func (r *replicateChannelManager) forwardMsg(targetPChannel string, msg *msgstream.MsgPack) {
	var handler *replicateChannelHandler

	_ = retry.Do(r.replicateCtx, func() error {
		r.channelLock.Lock()
		defer r.channelLock.Unlock()

		for _, channelHandler := range r.channelHandlerMap {
			if channelHandler.targetPChannel == targetPChannel {
				handler = channelHandler
				break
			}
		}
		if handler == nil {
			select {
			case r.forwardReplicateChannel <- targetPChannel:
				log.Info("success to forward replicate channel", zap.String("target_pchannel", targetPChannel))
			default:
			}
			return errors.Newf("channel %s not found when forward the msg", targetPChannel)
		}
		return nil
	}, r.retryOptions...)

	if handler == nil {
		r.apiEventChan <- &api.ReplicateAPIEvent{
			EventType: api.ReplicateError,
			Error:     errors.Newf("channel %s not found when forward the msg", targetPChannel),
		}
		log.Warn("channel not found when forward the msg",
			zap.String("target_pchannel", targetPChannel), zap.Strings("channels", lo.Keys(r.channelHandlerMap)))
		return
	}
	handler.forwardPackChan <- msg
}

func (r *replicateChannelManager) isDroppedCollection(collection int64) bool {
	_, ok := r.droppedCollections.Load(collection)
	return ok
}

func (r *replicateChannelManager) isDroppedPartition(partition int64) bool {
	_, ok := r.droppedPartitions.Load(partition)
	return ok
}

func (r *replicateChannelManager) stopReadChannel(pChannelName string, collectionID int64) {
	r.channelLock.Lock()
	defer r.channelLock.Unlock()
	channelHandler, ok := r.channelHandlerMap[pChannelName]
	if !ok {
		return
	}
	channelHandler.RemoveCollection(collectionID)
	// because the channel maybe be repeated to use for the forward message, NOT CLOSE
	// if channelHandler.IsEmpty() {
	//	channelHandler.Close()
	// }
}

type replicateChannelHandler struct {
	replicateCtx   context.Context
	pChannelName   string
	targetPChannel string
	// TODO make it single stream when there are many tasks
	stream       msgstream.MsgStream
	targetClient api.TargetAPI
	metaOp       api.MetaOp

	// key: source milvus collectionID value: *model.TargetCollectionInfo
	recordLock        deadlock.RWMutex
	collectionRecords map[int64]*model.TargetCollectionInfo // key is suorce collection id
	collectionNames   map[string]int64

	msgPackChan      chan *msgstream.MsgPack
	apiEventChan     chan *api.ReplicateAPIEvent
	forwardPackChan  chan *msgstream.MsgPack
	generatePackChan chan *msgstream.MsgPack

	msgPackCallback     func(string, *msgstream.MsgPack)
	forwardMsgFunc      func(string, *msgstream.MsgPack)
	isDroppedCollection func(int64) bool
	isDroppedPartition  func(int64) bool

	retryOptions   []retry.Option
	lastSendTTTime time.Time
	ttPeriod       time.Duration

	sourceSeekPosition *msgstream.MsgPosition
}

func (r *replicateChannelHandler) AddCollection(collectionID int64, targetInfo *model.TargetCollectionInfo) {
	r.recordLock.Lock()
	r.collectionRecords[collectionID] = targetInfo
	r.collectionNames[targetInfo.CollectionName] = collectionID
	GetTSManager().AddRef(r.pChannelName)
	r.recordLock.Unlock()
	log.Info("add collection to channel handler",
		zap.Int64("collection_id", collectionID), zap.String("collection_name", targetInfo.CollectionName))

	if targetInfo.Dropped {
		replicatePool.Submit(func() (struct{}, error) {
			dropCollectionLog := log.With(zap.Int64("collection_id", collectionID), zap.String("collection_name", targetInfo.CollectionName))
			dropCollectionLog.Info("generate msg for dropped collection")
			generatePosition := r.sourceSeekPosition
			if generatePosition == nil || generatePosition.Timestamp == 0 {
				dropCollectionLog.Warn("drop collection, but seek timestamp is 0")
				return struct{}{}, nil
			}
			r.generatePackChan <- &msgstream.MsgPack{
				BeginTs: generatePosition.Timestamp,
				EndTs:   generatePosition.Timestamp,
				StartPositions: []*msgstream.MsgPosition{
					generatePosition,
				},
				EndPositions: []*msgstream.MsgPosition{
					generatePosition,
				},
				Msgs: []msgstream.TsMsg{
					&msgstream.DropCollectionMsg{
						BaseMsg: msgstream.BaseMsg{
							BeginTimestamp: generatePosition.Timestamp,
							EndTimestamp:   generatePosition.Timestamp,
							HashValues:     []uint32{uint32(0)},
							MsgPosition:    generatePosition,
						},
						DropCollectionRequest: msgpb.DropCollectionRequest{
							Base: &commonpb.MsgBase{
								MsgType:   commonpb.MsgType_DropCollection,
								Timestamp: generatePosition.Timestamp,
							},
							DbName:         targetInfo.DatabaseName,
							CollectionID:   collectionID,
							CollectionName: targetInfo.CollectionName,
						},
					},
				},
			}
			dropCollectionLog.Info("has generate msg for dropped collection")
			return struct{}{}, nil
		})
	}
}

func (r *replicateChannelHandler) RemoveCollection(collectionID int64) {
	r.recordLock.Lock()
	defer r.recordLock.Unlock()
	collectionRecord, ok := r.collectionRecords[collectionID]
	if !ok {
		return
	}
	delete(r.collectionRecords, collectionID)
	if collectionRecord != nil {
		delete(r.collectionNames, collectionRecord.CollectionName)
	}
	GetTSManager().RemoveRef(r.pChannelName)
	log.Info("remove collection from handler", zap.Int64("collection_id", collectionID))
}

func (r *replicateChannelHandler) AddPartitionInfo(collectionInfo *pb.CollectionInfo, partitionInfo *pb.PartitionInfo, barrierChan chan<- uint64) error {
	collectionID := collectionInfo.ID
	partitionID := partitionInfo.PartitionID
	collectionName := collectionInfo.Schema.Name
	partitionName := partitionInfo.PartitionName

	partitionLog := log.With(
		zap.String("collection_name", collectionName),
		zap.Int64("collection_id", collectionID),
		zap.Int64("partition_id", partitionID),
		zap.String("partition_name", partitionName),
	)

	targetInfo, err := r.getCollectionTargetInfo(collectionID)
	if err != nil {
		partitionLog.Warn("fail to get collection target info", zap.Error(err))
		return err
	}
	if targetInfo == nil || targetInfo.Dropped {
		partitionLog.Info("the collection is dropping or dropped, skip the partition")
		return nil
	}
	r.recordLock.Lock()
	if targetInfo.PartitionBarrierChan[partitionID] != nil {
		partitionLog.Info("the partition barrier chan is not nil")
		r.recordLock.Unlock()
		return nil
	}
	targetInfo.PartitionBarrierChan[partitionID] = util.NewOnceWriteChan(barrierChan)
	partitionLog.Info("add partition info done")
	r.recordLock.Unlock()

	if partitionInfo.State == pb.PartitionState_PartitionDropping ||
		partitionInfo.State == pb.PartitionState_PartitionDropped {
		targetInfo.DroppedPartition[partitionID] = struct{}{}
		partitionLog.Info("the partition is dropped")
		replicatePool.Submit(func() (struct{}, error) {
			partitionLog.Info("generate msg for dropped partition")
			generatePosition := r.sourceSeekPosition
			if generatePosition == nil || generatePosition.Timestamp == 0 {
				partitionLog.Warn("drop partition, but seek timestamp is 0")
				return struct{}{}, nil
			}
			r.generatePackChan <- &msgstream.MsgPack{
				BeginTs: generatePosition.Timestamp,
				EndTs:   generatePosition.Timestamp,
				StartPositions: []*msgstream.MsgPosition{
					generatePosition,
				},
				EndPositions: []*msgstream.MsgPosition{
					generatePosition,
				},
				Msgs: []msgstream.TsMsg{
					&msgstream.DropPartitionMsg{
						BaseMsg: msgstream.BaseMsg{
							BeginTimestamp: generatePosition.Timestamp,
							EndTimestamp:   generatePosition.Timestamp,
							HashValues:     []uint32{uint32(0)},
							MsgPosition:    generatePosition,
						},
						DropPartitionRequest: msgpb.DropPartitionRequest{
							Base: &commonpb.MsgBase{
								MsgType:   commonpb.MsgType_DropPartition,
								Timestamp: generatePosition.Timestamp,
							},
							DbName:         targetInfo.DatabaseName,
							CollectionID:   collectionID,
							CollectionName: targetInfo.CollectionName,
							PartitionName:  partitionName,
							PartitionID:    partitionID,
						},
					},
				},
			}
			partitionLog.Info("has generate msg for dropped partition")
			return struct{}{}, nil
		})
	}
	return nil
}

func (r *replicateChannelHandler) updateTargetPartitionInfo(collectionID int64, collectionName string, partitionName string) int64 {
	ctx, cancelFunc := context.WithTimeout(r.replicateCtx, 10*time.Second)
	defer cancelFunc()
	sourceDBInfo := r.metaOp.GetDatabaseInfoForCollection(ctx, collectionID)

	collectionInfo, err := r.targetClient.GetPartitionInfo(ctx, collectionName, sourceDBInfo.Name)
	if err != nil {
		log.Warn("fail to get partition info", zap.String("collection_name", collectionName), zap.Error(err))
		return 0
	}
	r.recordLock.Lock()
	defer r.recordLock.Unlock()
	targetInfo, ok := r.collectionRecords[collectionID]
	if !ok {
		log.Warn("not found the collection id", zap.Int64("collection_id", collectionID), zap.Any("collections", r.collectionNames))
		return 0
	}
	targetInfo.PartitionInfo = collectionInfo.Partitions
	return targetInfo.PartitionInfo[partitionName]
}

func (r *replicateChannelHandler) RemovePartitionInfo(collectionID int64, name string, id int64) {
	targetInfo, err := r.getCollectionTargetInfo(collectionID)
	if err != nil {
		log.Warn("fail to get collection target info", zap.Int64("collection_id", collectionID), zap.Error(err))
		return
	}
	if targetInfo == nil {
		log.Info("the collection is dropping or dropped, skip the partition",
			zap.Int64("collection_id", collectionID), zap.String("partition_name", name))
		return
	}
	r.recordLock.Lock()
	defer r.recordLock.Unlock()
	if targetInfo.PartitionInfo[name] == id {
		delete(targetInfo.PartitionInfo, name)
	}
	delete(targetInfo.PartitionBarrierChan, id)
	targetInfo.DroppedPartition[id] = struct{}{}

	log.Info("remove partition info", zap.String("collection_name", targetInfo.CollectionName), zap.Int64("collection_id", collectionID),
		zap.String("partition_name", name), zap.Int64("partition_id", id))
}

func (r *replicateChannelHandler) IsEmpty() bool {
	r.recordLock.RLock()
	defer r.recordLock.RUnlock()
	return len(r.collectionRecords) == 0
}

func (r *replicateChannelHandler) Close() {
	// r.stream.Close()
}

func (r *replicateChannelHandler) startReadChannel() {
	go func() {
		innerHandlePack := func(msgPack *msgstream.MsgPack) {
			p := r.handlePack(false, msgPack)
			if p == util.EmptyMsgPack {
				return
			}
			r.msgPackChan <- p
		}

		for {
			select {
			case <-r.replicateCtx.Done():
				log.Warn("replicate channel handler closed")
				return
			case msgPack := <-r.forwardPackChan:
				r.msgPackChan <- r.handlePack(true, msgPack)
			case msgPack := <-r.generatePackChan:
				innerHandlePack(msgPack)
			case msgPack, ok := <-r.stream.Chan():
				if !ok {
					log.Warn("replicate channel closed", zap.String("channel_name", r.pChannelName))
					continue
				}
				GreedyConsumeChan(r.generatePackChan, innerHandlePack)
				GreedyConsumeChan(r.forwardPackChan, func(pack *msgstream.MsgPack) {
					r.msgPackChan <- r.handlePack(true, pack)
				})

				innerHandlePack(msgPack)
			}
		}
	}()
}

func GreedyConsumeChan(packChan chan *msgstream.MsgPack, f func(*msgstream.MsgPack)) {
	for i := 0; i < 10; i++ {
		select {
		case pack := <-packChan:
			f(pack)
		default:
			return
		}
	}
}

func (r *replicateChannelHandler) getCollectionTargetInfo(collectionID int64) (*model.TargetCollectionInfo, error) {
	if r.isDroppedCollection(collectionID) {
		return nil, nil
	}
	r.recordLock.RLock()
	targetInfo, ok := r.collectionRecords[collectionID]
	r.recordLock.RUnlock()
	if ok {
		return targetInfo, nil
	}

	// Sleep for a short time to avoid invalid requests
	time.Sleep(500 * time.Millisecond)
	err := retry.Do(r.replicateCtx, func() error {
		r.recordLock.RLock()
		var xs []int64
		for x := range r.collectionRecords {
			xs = append(xs, x)
		}
		log.Info("wait collection info", zap.Int64("msg_collection_id", collectionID), zap.Any("records", xs))
		// TODO it needs to be considered when supporting the specific collection in a task
		targetInfo, ok = r.collectionRecords[collectionID]
		r.recordLock.RUnlock()
		if ok {
			return nil
		}
		if r.isDroppedCollection(collectionID) {
			return nil
		}
		return errors.Newf("not found the collection [%d]", collectionID)
	}, r.retryOptions...)
	if err != nil {
		log.Warn("fail to find the collection info", zap.Error(err))
		return nil, err
	}
	return targetInfo, nil
}

func (r *replicateChannelHandler) containCollection(collectionName string) bool {
	r.recordLock.RLock()
	defer r.recordLock.RUnlock()
	return r.collectionNames[collectionName] != 0
}

func (r *replicateChannelHandler) getPartitionID(sourceCollectionID, sourcePartitionID int64, info *model.TargetCollectionInfo, name string) (int64, error) {
	r.recordLock.RLock()
	id, ok := info.PartitionInfo[name]
	r.recordLock.RUnlock()
	if ok {
		return id, nil
	}

	// Sleep for a short time to avoid invalid requests
	time.Sleep(500 * time.Millisecond)
	err := retry.Do(r.replicateCtx, func() error {
		log.Warn("wait partition info", zap.Int64("collection_id", info.CollectionID), zap.String("partition_name", name))
		id = r.updateTargetPartitionInfo(sourceCollectionID, info.CollectionName, name)
		if id != 0 {
			return nil
		}
		if r.isDroppedCollection(sourceCollectionID) ||
			r.isDroppedPartition(sourcePartitionID) {
			id = -1
			return nil
		}
		return errors.Newf("not found the partition [%s]", name)
	}, r.retryOptions...)
	if err != nil {
		log.Warn("fail to find the partition id", zap.Int64("source_collection", sourceCollectionID), zap.Any("target_collection", info.CollectionID), zap.String("partition_name", name))
		return 0, err
	}
	return id, nil
}

func (r *replicateChannelHandler) isDroppingPartition(partition int64, info *model.TargetCollectionInfo) bool {
	r.recordLock.RLock()
	defer r.recordLock.RUnlock()
	_, ok := info.DroppedPartition[partition]
	return ok
}

func (r *replicateChannelHandler) handlePack(forward bool, pack *msgstream.MsgPack) *msgstream.MsgPack {
	GetTSManager().CollectTS(r.pChannelName, pack.EndTs)
	if r.msgPackCallback != nil {
		r.msgPackCallback(r.pChannelName, pack)
	}
	newPack := &msgstream.MsgPack{
		BeginTs:        pack.BeginTs,
		EndTs:          pack.EndTs,
		StartPositions: pack.StartPositions,
		EndPositions:   pack.EndPositions,
		Msgs:           make([]msgstream.TsMsg, 0),
	}
	beginTS := pack.BeginTs

	minTS, resetTS := GetTSManager().GetMinTS(r.pChannelName)
	if minTS == 0 {
		r.sendErrEvent(errors.Newf("fail to get channel ts, channel: %s", r.pChannelName))
		log.Warn("fail to get channel ts", zap.String("channel", r.pChannelName))
		return nil
	}
	endTS := minTS
	if beginTS > endTS {
		beginTS = endTS
	}

	needTsMsg := false
	pChannel := r.targetPChannel
	for _, msg := range pack.Msgs {
		if forward {
			newPack.Msgs = append(newPack.Msgs, msg)
			if msg.Type() == commonpb.MsgType_DropCollection {
				needTsMsg = true
			}
			continue
		}
		if msg.Type() == commonpb.MsgType_CreateCollection ||
			msg.Type() == commonpb.MsgType_CreatePartition ||
			msg.Type() == commonpb.MsgType_TimeTick {
			continue
		}
		if msg.Type() != commonpb.MsgType_Insert && msg.Type() != commonpb.MsgType_Delete &&
			msg.Type() != commonpb.MsgType_DropCollection && msg.Type() != commonpb.MsgType_DropPartition {
			log.Warn("not support msg type", zap.Any("msg", msg))
			continue
		}

		if y, ok := msg.(interface{ GetCollectionID() int64 }); ok {
			sourceCollectionID := y.GetCollectionID()
			info, err := r.getCollectionTargetInfo(sourceCollectionID)
			if err != nil {
				r.sendErrEvent(err)
				log.Warn("fail to get collection info", zap.Int64("collection_id", sourceCollectionID), zap.Error(err))
				return nil
			}
			if info == nil {
				log.Info("collection has been dropped in the source and target", zap.Int64("collection_id", sourceCollectionID))
				continue
			}
			var dataLen int
			switch realMsg := msg.(type) {
			case *msgstream.InsertMsg:
				if info.Dropped {
					log.Info("skip insert msg because collection has been dropped", zap.Int64("collection_id", sourceCollectionID))
					continue
				}
				realMsg.CollectionID = info.CollectionID
				partitionID := realMsg.PartitionID
				realMsg.PartitionID, err = r.getPartitionID(sourceCollectionID, partitionID, info, realMsg.PartitionName)
				if realMsg.PartitionID == -1 {
					log.Info("skip insert msg because partition has been dropped in the source and target",
						zap.Int64("partition_id", partitionID), zap.String("partition_name", realMsg.PartitionName))
					continue
				}
				realMsg.ShardName = info.VChannel
				dataLen = int(realMsg.GetNumRows())
				if resetTS {
					realMsg.EndTimestamp = endTS
				}
			case *msgstream.DeleteMsg:
				if info.Dropped {
					log.Info("skip delete msg because collection has been dropped", zap.Int64("collection_id", sourceCollectionID))
					continue
				}
				if r.isDroppedPartition(realMsg.PartitionID) || r.isDroppingPartition(realMsg.PartitionID, info) {
					log.Info("skip delete msg because partition has been dropped in the source and target",
						zap.Int64("partition_id", realMsg.PartitionID), zap.String("partition_name", realMsg.PartitionName))
					continue
				}
				realMsg.CollectionID = info.CollectionID
				if realMsg.PartitionName != "" {
					partitionID := realMsg.PartitionID
					realMsg.PartitionID, err = r.getPartitionID(sourceCollectionID, realMsg.PartitionID, info, realMsg.PartitionName)
					if realMsg.PartitionID == -1 {
						log.Info("skip delete msg because partition has been dropped in the source and target",
							zap.Int64("partition_id", partitionID), zap.String("partition_name", realMsg.PartitionName))
						continue
					}
				}
				realMsg.ShardName = info.VChannel
				dataLen = int(realMsg.GetNumRows())
				if resetTS {
					realMsg.EndTimestamp = endTS
				}
			case *msgstream.DropCollectionMsg:
				collectionID := realMsg.CollectionID
				realMsg.CollectionID = info.CollectionID
				info.BarrierChan.Write(msg.EndTs())
				needTsMsg = true
				r.RemoveCollection(collectionID)
				if resetTS {
					realMsg.EndTimestamp = endTS
				}
			case *msgstream.DropPartitionMsg:
				if info.Dropped {
					log.Info("skip drop partition msg because collection has been dropped",
						zap.Int64("collection_id", sourceCollectionID),
						zap.String("collection_name", info.CollectionName),
						zap.String("partition_name", realMsg.PartitionName))
					continue
				}
				if r.isDroppedPartition != nil && r.isDroppedPartition(realMsg.PartitionID) {
					log.Info("skip drop partition msg because partition has been dropped",
						zap.Int64("partition_id", realMsg.PartitionID),
						zap.String("partition_name", realMsg.PartitionName))
					continue
				}
				realMsg.CollectionID = info.CollectionID
				if realMsg.PartitionName == "" {
					log.Warn("invalid drop partition message, empty partition name", zap.Any("msg", msg))
					continue
				}
				var partitionBarrierChan *util.OnceWriteChan[uint64]
				retryErr := retry.Do(r.replicateCtx, func() error {
					err = nil
					r.recordLock.RLock()
					partitionBarrierChan = info.PartitionBarrierChan[realMsg.PartitionID]
					if partitionBarrierChan == nil {
						err = errors.Newf("not found the partition info [%d]", realMsg.PartitionID)
						log.Warn("invalid drop partition message", zap.Any("msg", msg))
					}
					r.recordLock.RUnlock()
					if r.isDroppedCollection(realMsg.CollectionID) ||
						r.isDroppedPartition(realMsg.PartitionID) {
						return nil
					}
					return err
				}, r.retryOptions...)
				if retryErr != nil && err == nil {
					err = retryErr
				}
				if r.isDroppedCollection(realMsg.CollectionID) ||
					r.isDroppedPartition(realMsg.PartitionID) {
					log.Info("skip drop partition msg because partition or collection is dropping",
						zap.Int64("partition_id", realMsg.PartitionID), zap.String("partition_name", realMsg.PartitionName))
					continue
				}
				if err == nil {
					partitionID := realMsg.PartitionID
					realMsg.PartitionID, err = r.getPartitionID(sourceCollectionID, partitionID, info, realMsg.PartitionName)
					if realMsg.PartitionID == -1 {
						log.Warn("skip drop partition msg because partition or collection is dropping",
							zap.Int64("partition_id", partitionID), zap.String("partition_name", realMsg.PartitionName))
						continue
					}
					partitionBarrierChan.Write(msg.EndTs())
					r.RemovePartitionInfo(sourceCollectionID, realMsg.PartitionName, partitionID)
				}
				if resetTS {
					realMsg.EndTimestamp = endTS
				}
			}
			if err != nil {
				r.sendErrEvent(err)
				log.Warn("fail to process the msg info", zap.Any("msg", msg.Type()), zap.Error(err))
				return nil
			}
			originPosition := msg.Position()
			msg.SetPosition(&msgpb.MsgPosition{
				ChannelName: info.PChannel,
				MsgID:       originPosition.GetMsgID(),
				MsgGroup:    originPosition.GetMsgGroup(),
				Timestamp:   msg.EndTs(),
			})
			logFields := []zap.Field{
				zap.String("msg", msg.Type().String()),
			}
			if dataLen != 0 {
				logFields = append(logFields, zap.Int("data_len", dataLen))
			}
			log.Info("receive msg", logFields...)
			if pChannel != info.PChannel {
				log.Info("forward the msg", zap.Any("msg_type", msg.Type().String()), zap.String("pChannel", pChannel), zap.String("info_pChannel", info.PChannel))
				r.forwardMsgFunc(info.PChannel, &msgstream.MsgPack{
					BeginTs:        msg.BeginTs(),
					EndTs:          msg.EndTs(),
					StartPositions: []*msgpb.MsgPosition{originPosition},
					EndPositions:   []*msgpb.MsgPosition{originPosition},
					Msgs: []msgstream.TsMsg{
						msg,
					},
				})
				continue
			}
			newPack.Msgs = append(newPack.Msgs, msg)
		} else {
			log.Warn("not support msg type", zap.Any("msg", msg))
		}
	}

	if !needTsMsg && len(newPack.Msgs) == 0 && time.Since(r.lastSendTTTime) < r.ttPeriod {
		return util.EmptyMsgPack
	}

	for _, position := range newPack.StartPositions {
		position.ChannelName = pChannel
		position.Timestamp = beginTS
	}
	for _, position := range newPack.EndPositions {
		position.ChannelName = pChannel
		position.Timestamp = endTS
	}
	newPack.BeginTs = beginTS
	newPack.EndTs = endTS

	needTsMsg = needTsMsg || len(newPack.Msgs) == 0
	if needTsMsg {
		timeTickResult := msgpb.TimeTickMsg{
			Base: commonpbutil.NewMsgBase(
				commonpbutil.WithMsgType(commonpb.MsgType_TimeTick),
				commonpbutil.WithMsgID(0),
				commonpbutil.WithTimeStamp(endTS),
				commonpbutil.WithSourceID(-1),
			),
		}
		timeTickMsg := &msgstream.TimeTickMsg{
			BaseMsg: msgstream.BaseMsg{
				BeginTimestamp: endTS,
				EndTimestamp:   endTS,
				HashValues:     []uint32{0},
				MsgPosition:    newPack.EndPositions[0],
			},
			TimeTickMsg: timeTickResult,
		}
		newPack.Msgs = append(newPack.Msgs, timeTickMsg)
	}
	return newPack
}

func (r *replicateChannelHandler) sendErrEvent(err error) {
	r.apiEventChan <- &api.ReplicateAPIEvent{
		EventType: api.ReplicateError,
		Error:     err,
	}
}

func newReplicateChannelHandler(ctx context.Context,
	sourceInfo *model.SourceCollectionInfo, targetInfo *model.TargetCollectionInfo,
	targetClient api.TargetAPI, metaOp api.MetaOp,
	apiEventChan chan *api.ReplicateAPIEvent, opts *model.HandlerOpts,
) (*replicateChannelHandler, error) {
	stream, err := opts.Factory.NewMsgStream(ctx)
	log := log.With(zap.String("channel_name", sourceInfo.PChannelName), zap.Int64("collection_id", sourceInfo.CollectionID))
	if err != nil {
		log.Warn("fail to new the msg stream", zap.Error(err))
		return nil, err
	}
	subPositionType := mqwrapper.SubscriptionPositionUnknown
	if sourceInfo.SeekPosition == nil {
		subPositionType = mqwrapper.SubscriptionPositionLatest
	}
	err = stream.AsConsumer(ctx, []string{sourceInfo.PChannelName}, sourceInfo.PChannelName+strconv.Itoa(rand.Int()), subPositionType)
	if err != nil {
		log.Warn("fail to consume the channel", zap.Error(err))
		stream.Close()
		return nil, err
	}
	if sourceInfo.SeekPosition != nil {
		err = stream.Seek(ctx, []*msgstream.MsgPosition{sourceInfo.SeekPosition}, false)
		if err != nil {
			log.Warn("fail to seek the msg stream", zap.Error(err))
			stream.Close()
			return nil, err
		}
		if sourceInfo.SeekPosition.Timestamp == 0 {
			log.Info("the seek timestamp is zero")
		}
		log.Info("success to seek the msg stream")
	}
	if opts.TTInterval <= 0 {
		opts.TTInterval = util.DefaultReplicateTTInterval
	}
	channelHandler := &replicateChannelHandler{
		replicateCtx:       ctx,
		pChannelName:       sourceInfo.PChannelName,
		targetPChannel:     targetInfo.PChannel,
		stream:             stream,
		targetClient:       targetClient,
		metaOp:             metaOp,
		collectionRecords:  make(map[int64]*model.TargetCollectionInfo),
		collectionNames:    make(map[string]int64),
		msgPackChan:        make(chan *msgstream.MsgPack, opts.MessageBufferSize),
		apiEventChan:       apiEventChan,
		forwardPackChan:    make(chan *msgstream.MsgPack, opts.MessageBufferSize),
		generatePackChan:   make(chan *msgstream.MsgPack, 30),
		retryOptions:       opts.RetryOptions,
		lastSendTTTime:     time.Now(),
		ttPeriod:           time.Duration(opts.TTInterval) * time.Second,
		sourceSeekPosition: sourceInfo.SeekPosition,
	}
	channelHandler.lastSendTTTime = time.Now().Add(-channelHandler.ttPeriod)
	channelHandler.AddCollection(sourceInfo.CollectionID, targetInfo)
	GetTSManager().CollectTS(channelHandler.pChannelName, math.MaxUint64)
	return channelHandler, nil
}
