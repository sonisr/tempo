package blockbuilder

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/backoff"
	"github.com/grafana/dskit/ring"
	"github.com/grafana/dskit/services"
	"github.com/grafana/tempo/modules/storage"
	"github.com/grafana/tempo/pkg/ingest"
	"github.com/grafana/tempo/tempodb"
	"github.com/grafana/tempo/tempodb/encoding"
	"github.com/grafana/tempo/tempodb/wal"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
)

const (
	blockBuilderServiceName = "block-builder"
	ConsumerGroup           = "block-builder"
	pollTimeout             = 2 * time.Second
)

var (
	metricPartitionLag = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "tempo",
		Subsystem: "block_builder",
		Name:      "partition_lag",
		Help:      "Lag of a partition.",
	}, []string{"partition"})
	metricPartitionLagSeconds = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "tempo",
		Subsystem: "block_builder",
		Name:      "partition_lag_seconds",
		Help:      "Lag of a partition in seconds.",
	}, []string{"partition"})
	metricConsumeCycleDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace:                   "tempo",
		Subsystem:                   "block_builder",
		Name:                        "consume_cycle_duration_seconds",
		Help:                        "Time spent consuming a full cycle.",
		NativeHistogramBucketFactor: 1.1,
	})
	metricProcessPartitionSectionDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:                   "tempo",
		Subsystem:                   "block_builder",
		Name:                        "process_partition_section_duration_seconds",
		Help:                        "Time spent processing one partition section.",
		NativeHistogramBucketFactor: 1.1,
	}, []string{"partition"})
	metricFetchErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "tempo",
		Subsystem: "block_builder",
		Name:      "fetch_errors_total",
		Help:      "Total number of errors while fetching by the consumer.",
	}, []string{"partition"})
)

type BlockBuilder struct {
	services.Service

	logger log.Logger
	cfg    Config

	kafkaClient   *kgo.Client
	kadm          *kadm.Client
	decoder       *ingest.Decoder
	partitionRing ring.PartitionRingReader

	overrides Overrides
	enc       encoding.VersionedEncoding
	wal       *wal.WAL // TODO - Shared between tenants, should be per tenant?
	writer    tempodb.Writer
}

func New(
	cfg Config,
	logger log.Logger,
	partitionRing ring.PartitionRingReader,
	overrides Overrides,
	store storage.Store,
) *BlockBuilder {
	b := &BlockBuilder{
		logger:        logger,
		cfg:           cfg,
		partitionRing: partitionRing,
		decoder:       ingest.NewDecoder(),
		overrides:     overrides,
		writer:        store,
	}

	b.Service = services.NewBasicService(b.starting, b.running, b.stopping)
	return b
}

func (b *BlockBuilder) starting(ctx context.Context) (err error) {
	level.Info(b.logger).Log("msg", "block builder starting")

	b.enc = encoding.DefaultEncoding()
	if version := b.cfg.BlockConfig.BlockCfg.Version; version != "" {
		b.enc, err = encoding.FromVersion(version)
		if err != nil {
			return fmt.Errorf("failed to create encoding: %w", err)
		}
	}

	b.wal, err = wal.New(&b.cfg.WAL)
	if err != nil {
		return fmt.Errorf("failed to create WAL: %w", err)
	}

	b.kafkaClient, err = ingest.NewReaderClient(
		b.cfg.IngestStorageConfig.Kafka,
		ingest.NewReaderClientMetrics(blockBuilderServiceName, prometheus.DefaultRegisterer),
		b.logger,
	)
	if err != nil {
		return fmt.Errorf("failed to create kafka reader client: %w", err)
	}

	boff := backoff.New(ctx, backoff.Config{
		MinBackoff: 100 * time.Millisecond,
		MaxBackoff: time.Minute, // If there is a network hiccup, we prefer to wait longer retrying, than fail the service.
		MaxRetries: 10,
	})

	for boff.Ongoing() {
		err := b.kafkaClient.Ping(ctx)
		if err == nil {
			break
		}
		level.Warn(b.logger).Log("msg", "ping kafka; will retry", "err", err)
		boff.Wait()
	}
	if err := boff.ErrCause(); err != nil {
		return fmt.Errorf("failed to ping kafka: %w", err)
	}

	b.kadm = kadm.NewClient(b.kafkaClient)

	go b.metricLag(ctx)

	return nil
}

func (b *BlockBuilder) running(ctx context.Context) error {
	// Initial delay
	waitTime := 0 * time.Second
	for {
		select {
		case <-time.After(waitTime):
			err := b.consume(ctx)
			if err != nil {
				level.Error(b.logger).Log("msg", "consumeCycle failed", "err", err)
			}

			// Real delay on subsequent
			waitTime = b.cfg.ConsumeCycleDuration
		case <-ctx.Done():
			return nil
		}
	}
}

func (b *BlockBuilder) consume(ctx context.Context) error {
	var (
		end        = time.Now()
		partitions = b.getAssignedActivePartitions()
	)

	level.Info(b.logger).Log("msg", "starting consume cycle", "cycle_end", end, "active_partitions", partitions)
	defer func(t time.Time) { metricConsumeCycleDuration.Observe(time.Since(t).Seconds()) }(time.Now())

	for _, partition := range partitions {
		// Consume partition while data remains.
		// TODO - round-robin one consumption per partition instead to equalize catch-up time.
		for {
			more, err := b.consumePartition(ctx, partition, end)
			if err != nil {
				return err
			}

			if !more {
				break
			}
		}
	}

	return nil
}

func (b *BlockBuilder) consumePartition(ctx context.Context, partition int32, overallEnd time.Time) (more bool, err error) {
	defer func(t time.Time) {
		metricProcessPartitionSectionDuration.WithLabelValues(strconv.Itoa(int(partition))).Observe(time.Since(t).Seconds())
	}(time.Now())

	var (
		dur         = b.cfg.ConsumeCycleDuration
		topic       = b.cfg.IngestStorageConfig.Kafka.Topic
		group       = b.cfg.IngestStorageConfig.Kafka.ConsumerGroup
		startOffset kgo.Offset
		init        bool
		writer      *writer
		lastRec     *kgo.Record
		end         time.Time
	)

	commits, err := b.kadm.FetchOffsetsForTopics(ctx, group, topic)
	if err != nil {
		return false, err
	}

	lastCommit, ok := commits.Lookup(topic, partition)
	if ok && lastCommit.At >= 0 {
		startOffset = kgo.NewOffset().At(lastCommit.At)
	} else {
		startOffset = kgo.NewOffset().AtStart()
	}

	level.Info(b.logger).Log(
		"msg", "consuming partition",
		"partition", partition,
		"commit_offset", lastCommit.At,
		"start_offset", startOffset,
	)

	// We always rewind the partition's offset to the commit offset by reassigning the partition to the client (this triggers partition assignment).
	// This is so the cycle started exactly at the commit offset, and not at what was (potentially over-) consumed previously.
	// In the end, we remove the partition from the client (refer to the defer below) to guarantee the client always consumes
	// from one partition at a time. I.e. when this partition is consumed, we start consuming the next one.
	b.kafkaClient.AddConsumePartitions(map[string]map[int32]kgo.Offset{
		topic: {
			partition: startOffset,
		},
	})
	defer b.kafkaClient.RemoveConsumePartitions(map[string][]int32{topic: {partition}})

outer:
	for {
		fetches := func() kgo.Fetches {
			ctx2, cancel := context.WithTimeout(ctx, pollTimeout)
			defer cancel()
			return b.kafkaClient.PollFetches(ctx2)
		}()
		err = fetches.Err()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				// No more data
				break
			}
			metricFetchErrors.WithLabelValues(strconv.Itoa(int(partition))).Inc()
			return false, err
		}

		if fetches.Empty() {
			break
		}

		for iter := fetches.RecordIter(); !iter.Done(); {
			rec := iter.Next()

			level.Debug(b.logger).Log(
				"msg", "processing record",
				"partition", rec.Partition,
				"offset", rec.Offset,
				"timestamp", rec.Timestamp,
			)

			// Initialize on first record
			if !init {
				end = rec.Timestamp.Add(dur) // When block will be cut
				metricPartitionLagSeconds.WithLabelValues(strconv.Itoa(int(partition))).Set(time.Since(rec.Timestamp).Seconds())
				writer = newPartitionSectionWriter(b.logger, uint64(partition), uint64(rec.Offset), b.cfg.BlockConfig, b.overrides, b.wal, b.enc)
				init = true
			}

			if rec.Timestamp.After(end) {
				// Cut this block but continue only if we have at least another full cycle
				if overallEnd.Sub(rec.Timestamp) >= dur {
					more = true
				}
				break outer
			}

			if rec.Timestamp.After(overallEnd) {
				break outer
			}

			err := b.pushTraces(rec.Key, rec.Value, writer)
			if err != nil {
				return false, err
			}

			lastRec = rec
		}
	}

	if lastRec == nil {
		// Received no data
		level.Info(b.logger).Log(
			"msg", "no data",
			"partition", partition,
		)
		return false, nil
	}

	err = writer.flush(ctx, b.writer)
	if err != nil {
		return false, err
	}

	// TODO - Retry commit
	resp, err := b.kadm.CommitOffsets(ctx, group, kadm.OffsetsFromRecords(*lastRec))
	if err != nil {
		return false, err
	}
	if err := resp.Error(); err != nil {
		return false, err
	}

	level.Info(b.logger).Log(
		"msg", "successfully committed offset to kafka",
		"partition", partition,
		"last_record", lastRec.Offset,
	)

	return more, nil
}

func (b *BlockBuilder) metricLag(ctx context.Context) {
	var (
		waitTime = time.Second * 15
		topic    = b.cfg.IngestStorageConfig.Kafka.Topic
		group    = b.cfg.IngestStorageConfig.Kafka.ConsumerGroup
	)

	for {
		select {
		case <-time.After(waitTime):
			lag, err := getGroupLag(ctx, b.kadm, topic, group)
			if err != nil {
				level.Error(b.logger).Log("msg", "metric lag failed:", "err", err)
				continue
			}
			for _, p := range b.getAssignedActivePartitions() {
				l, ok := lag.Lookup(topic, p)
				if ok {
					metricPartitionLag.WithLabelValues(strconv.Itoa(int(p))).Set(float64(l.Lag))
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

func (b *BlockBuilder) stopping(err error) error {
	if b.kafkaClient != nil {
		b.kafkaClient.Close()
	}
	return err
}

func (b *BlockBuilder) pushTraces(tenantBytes, reqBytes []byte, p partitionSectionWriter) error {
	req, err := b.decoder.Decode(reqBytes)
	if err != nil {
		return fmt.Errorf("failed to decode trace: %w", err)
	}
	defer b.decoder.Reset()

	return p.pushBytes(string(tenantBytes), req)
}

func (b *BlockBuilder) getAssignedActivePartitions() []int32 {
	activePartitionsCount := b.partitionRing.PartitionRing().ActivePartitionsCount()
	assignedActivePartitions := make([]int32, 0, activePartitionsCount)
	for _, partition := range b.cfg.AssignedPartitions[b.cfg.InstanceID] {
		if partition > int32(activePartitionsCount) {
			break
		}
		assignedActivePartitions = append(assignedActivePartitions, partition)
	}
	return assignedActivePartitions
}

// getGroupLag is similar to `kadm.Client.Lag` but works when the group doesn't have live participants.
// Similar to `kadm.CalculateGroupLagWithStartOffsets`, it takes into account that the group may not have any commits.
//
// The lag is the difference between the last produced offset (high watermark) and an offset in the "past".
// If the block builder committed an offset for a given partition to the consumer group at least once, then
// the lag is the difference between the last produced offset and the offset committed in the consumer group.
// Otherwise, if the block builder didn't commit an offset for a given partition yet (e.g. block builder is
// running for the first time), then the lag is the difference between the last produced offset and fallbackOffsetMillis.
func getGroupLag(ctx context.Context, admClient *kadm.Client, topic, group string) (kadm.GroupLag, error) {
	offsets, err := admClient.FetchOffsets(ctx, group)
	if err != nil {
		if !errors.Is(err, kerr.GroupIDNotFound) {
			return nil, fmt.Errorf("fetch offsets: %w", err)
		}
	}
	if err := offsets.Error(); err != nil {
		return nil, fmt.Errorf("fetch offsets got error in response: %w", err)
	}

	startOffsets, err := admClient.ListStartOffsets(ctx, topic)
	if err != nil {
		return nil, err
	}
	endOffsets, err := admClient.ListEndOffsets(ctx, topic)
	if err != nil {
		return nil, err
	}

	descrGroup := kadm.DescribedGroup{
		// "Empty" is the state that indicates that the group doesn't have active consumer members; this is always the case for block-builder,
		// because we don't use group consumption.
		State: "Empty",
	}
	return kadm.CalculateGroupLagWithStartOffsets(descrGroup, offsets, startOffsets, endOffsets), nil
}
