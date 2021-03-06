package hh

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"strings"
	"sync/atomic"

	"github.com/angopher/chronus/services/meta"
	"github.com/influxdata/influxdb/models"
	"go.uber.org/zap"
	"golang.org/x/time/rate"
)

const (
	writeNodeReq       = "writeNodeReq"
	writeNodeReqFail   = "writeNodeReqFail"
	writeNodeReqPoints = "writeNodeReqPoints"
)

var (
	// for concurrency control
	maxActiveProcessorCount = int32(0)
	activeProcessorCount    = int32(0)
)

// NodeProcessor encapsulates a queue of hinted-handoff data for a node, and the
// transmission of the data to the node.
type NodeProcessor struct {
	PurgeInterval    time.Duration // Interval between periodic purge checks
	RetryInterval    time.Duration // Interval between periodic write-to-node attempts.
	RetryMaxInterval time.Duration // Max interval between periodic write-to-node attempts.
	MaxSize          int64         // Maximum size an underlying queue can get.
	MaxAge           time.Duration // Maximum age queue data can get before purging.
	RetryRateLimit   int           // Limits the rate data is sent to node.
	nodeID           uint64
	dir              string

	mu   sync.RWMutex
	wg   sync.WaitGroup
	done chan struct{}

	queue  *queue
	meta   metaClient
	writer shardWriter

	stats  *NodeProcessorStatistics
	Logger *zap.SugaredLogger
}

type NodeProcessorStatistics struct {
	WriteShardReq       int64
	WriteShardReqPoints int64
	WriteNodeReq        int64
	WriteNodeReqFail    int64
	WriteNodeReqPoints  int64
}

func SetMaxActiveProcessorCount(n int32) {
	maxActiveProcessorCount = n
}

// NewNodeProcessor returns a new NodeProcessor for the given node, using dir for
// the hinted-handoff data.
func NewNodeProcessor(nodeID uint64, dir string, w shardWriter, m metaClient) *NodeProcessor {
	return &NodeProcessor{
		PurgeInterval:    DefaultPurgeInterval,
		RetryInterval:    DefaultRetryInterval,
		RetryMaxInterval: DefaultRetryMaxInterval,
		MaxSize:          DefaultMaxSize,
		MaxAge:           DefaultMaxAge,
		nodeID:           nodeID,
		dir:              dir,
		writer:           w,
		meta:             m,
		stats:            &NodeProcessorStatistics{},
		Logger:           zap.NewNop().Sugar(),
	}
}

func (n *NodeProcessor) WithLogger(logger *zap.Logger) {
	n.Logger = logger.With(zap.String("service", "hh_processor")).Sugar()
}

// Open opens the NodeProcessor. It will read and write data present in dir, and
// start transmitting data to the node. A NodeProcessor must be opened before it
// can accept hinted data.
func (n *NodeProcessor) Open() error {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.done != nil {
		// Already open.
		return nil
	}
	n.done = make(chan struct{})

	// Create the queue directory if it doesn't already exist.
	if err := os.MkdirAll(n.dir, 0700); err != nil {
		return fmt.Errorf("mkdir all: %s", err)
	}

	// Create the queue of hinted-handoff data.
	queue, err := newQueue(n.dir, n.MaxSize)
	if err != nil {
		return err
	}
	if err := queue.Open(); err != nil {
		return err
	}
	n.queue = queue

	n.wg.Add(1)
	go n.run()

	return nil
}

// Close closes the NodeProcessor, terminating all data tranmission to the node.
// When closed it will not accept hinted-handoff data.
func (n *NodeProcessor) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.done == nil {
		// Already closed.
		return nil
	}

	close(n.done)
	n.wg.Wait()
	n.done = nil

	return n.queue.Close()
}

// Statistics returns statistics for periodic monitoring.
func (n *NodeProcessor) Statistics(tags map[string]string) []models.Statistic {
	name := strings.Join([]string{"hh_processor", n.dir}, ":")
	t := map[string]string{"node": fmt.Sprintf("%d", n.nodeID), "path": n.dir}
	for k, v := range tags {
		t[k] = v
	}
	return []models.Statistic{{
		Name: name,
		Tags: t,
		Values: map[string]interface{}{
			writeShardReq:       atomic.LoadInt64(&n.stats.WriteShardReq),
			writeShardReqPoints: atomic.LoadInt64(&n.stats.WriteShardReqPoints),
			writeNodeReq:        atomic.LoadInt64(&n.stats.WriteNodeReq),
			writeNodeReqFail:    atomic.LoadInt64(&n.stats.WriteNodeReqFail),
			writeNodeReqPoints:  atomic.LoadInt64(&n.stats.WriteShardReqPoints),
		},
	}}
}

// Purge deletes all hinted-handoff data under management by a NodeProcessor.
// The NodeProcessor should be in the closed state before calling this function.
func (n *NodeProcessor) Purge() error {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.done != nil {
		return fmt.Errorf("node processor is open")
	}

	return os.RemoveAll(n.dir)
}

// WriteShard writes hinted-handoff data for the given shard and node. Since it may manipulate
// hinted-handoff queues, and be called concurrently, it takes a lock during queue access.
func (n *NodeProcessor) WriteShard(shardID uint64, points []models.Point) error {
	n.mu.RLock()
	defer n.mu.RUnlock()

	if n.done == nil {
		return fmt.Errorf("node processor is closed")
	}

	atomic.AddInt64(&n.stats.WriteShardReq, 1)
	atomic.AddInt64(&n.stats.WriteShardReqPoints, int64(len(points)))

	b := marshalWrite(shardID, points)
	return n.queue.Append(b)
}

// LastModified returns the time the NodeProcessor last receieved hinted-handoff data.
func (n *NodeProcessor) LastModified() (time.Time, error) {
	t, err := n.queue.LastModified()
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// run attempts to send any existing hinted handoff data to the target node. It also purges
// any hinted handoff data older than the configured time.
func (n *NodeProcessor) run() {
	defer n.wg.Done()

	waitTime := time.Duration(n.RetryInterval)
	if waitTime > time.Duration(n.RetryMaxInterval) {
		waitTime = time.Duration(n.RetryMaxInterval)
	}
	purgeTimer := time.NewTimer(n.PurgeInterval)
	defer purgeTimer.Stop()
	sendingTimer := time.NewTimer(waitTime)
	defer sendingTimer.Stop()

	for {
		select {
		case <-n.done:
			return

		case <-purgeTimer.C:
			if err := n.queue.PurgeOlderThan(time.Now().Add(-n.MaxAge)); err != nil {
				n.Logger.Warnf("failed to purge for node %d: %s", n.nodeID, err.Error())
			}
			purgeTimer.Reset(n.PurgeInterval)

		case <-sendingTimer.C:
			waitTime = n.sendingLoop(waitTime)
			sendingTimer.Reset(waitTime)

		}
	}
}

func concurrencyAllow() bool {
	if maxActiveProcessorCount < 1 {
		return true
	}
	waiter := time.NewTimer(time.Second)
	defer waiter.Stop()
	for {
		select {
		case <-waiter.C:
			// timeout
			return false
		default:
			if atomic.AddInt32(&activeProcessorCount, 1) <= maxActiveProcessorCount {
				return true
			}
			// restore & next
			atomic.AddInt32(&activeProcessorCount, -1)
		}
	}
}

func (n *NodeProcessor) sendingLoop(curDelay time.Duration) (nextDelay time.Duration) {
	var (
		sent int
		err  error
	)

	// concurrency check
	if maxActiveProcessorCount > 0 {
		if !concurrencyAllow() {
			n.Logger.Info("concurrency control, skip scheduling once")
			return n.RetryInterval
		}
		defer atomic.AddInt32(&activeProcessorCount, -1)
	}

	// Bytes rate limit
	if n.RetryRateLimit > 0 {
		bytesLimiter := rate.NewLimiter(rate.Limit(n.RetryRateLimit), 10*n.RetryRateLimit)
		defer func() {
			if sent > 0 {
				n.Logger.Infof("write to %d with %d bytes", n.nodeID, sent)
				bytesLimiter.WaitN(context.Background(), sent)
			}
		}()
	}

	sent, err = n.SendWrite()
	if err == nil {
		// Success! Ensure backoff is cancelled.
		nextDelay = n.RetryInterval
		return
	}

	if err == io.EOF {
		// No more data, return to configured interval
		nextDelay = n.RetryInterval
	} else {
		// backoff
		nextDelay = 2 * curDelay
		if nextDelay > n.RetryMaxInterval {
			nextDelay = n.RetryMaxInterval
		}
	}
	return
}

// SendWrite attempts to sent the current block of hinted data to the target node. If successful,
// it returns the number of bytes it sent and advances to the next block. Otherwise returns EOF
// when there is no more data or the node is inactive.
func (n *NodeProcessor) SendWrite() (int, error) {
	n.mu.RLock()
	defer n.mu.RUnlock()

	active, err := n.Active()
	if err != nil {
		return 0, err
	}
	if !active {
		return 0, io.EOF
	}

	// Get the current block from the queue
	buf, err := n.queue.Current()
	if err != nil {
		return 0, err
	}

	// unmarshal the byte slice back to shard ID and points
	shardID, points, err := unmarshalWrite(buf)
	if err != nil {
		n.Logger.Warnf("unmarshal write failed: %v", err)
		// Try to skip it.
		if err := n.queue.Advance(); err != nil {
			n.Logger.Warnf("failed to advance queue for node %d: %s", n.nodeID, err.Error())
		}
		return 0, err
	}

	if err := n.writer.WriteShard(shardID, n.nodeID, points); err != nil {
		atomic.AddInt64(&n.stats.WriteNodeReqFail, 1)
		return 0, err
	}
	atomic.AddInt64(&n.stats.WriteNodeReq, 1)
	atomic.AddInt64(&n.stats.WriteNodeReqPoints, int64(len(points)))

	if err := n.queue.Advance(); err != nil {
		n.Logger.Warnf("failed to advance queue for node %d: %s", n.nodeID, err.Error())
	}

	return len(buf), nil
}

// Head returns the head of the processor's queue.
func (n *NodeProcessor) Head() string {
	qp, err := n.queue.Position()
	if err != nil {
		return ""
	}
	return qp.head
}

// Tail returns the tail of the processor's queue.
func (n *NodeProcessor) Tail() string {
	qp, err := n.queue.Position()
	if err != nil {
		return ""
	}
	return qp.tail
}

// Active returns whether this node processor is for a currently active node.
func (n *NodeProcessor) Active() (bool, error) {
	nio, err := n.meta.DataNode(n.nodeID)
	if err != nil && err != meta.ErrNodeNotFound {
		n.Logger.Warnf("failed to determine if node %d is active: %s", n.nodeID, err.Error())
		return false, err
	}
	return nio != nil, nil
}

func marshalWrite(shardID uint64, points []models.Point) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, shardID)
	for _, p := range points {
		b = append(b, []byte(p.String())...)
		b = append(b, '\n')
	}
	return b
}

func unmarshalWrite(b []byte) (uint64, []models.Point, error) {
	if len(b) < 8 {
		return 0, nil, fmt.Errorf("too short: len = %d", len(b))
	}
	ownerID := binary.BigEndian.Uint64(b[:8])
	points, err := models.ParsePoints(b[8:])
	return ownerID, points, err
}
