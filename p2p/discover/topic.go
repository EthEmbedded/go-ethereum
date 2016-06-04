// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Package discover implements the Node Discovery Protocol.
//
// The Node Discovery protocol provides a way to find RLPx nodes that
// can be connected to. It uses a Kademlia-like protocol to maintain a
// distributed database of the IDs and endpoints of all listening
// nodes.
package discover

import (
	"container/heap"
	"math"
	"math/rand"
	"time"

	"github.com/aristanetworks/goarista/atime"
)

const MaxEntries = 10000
const MaxEntriesPerTopic = 50

type Topic string

type topicEntry struct {
	topic   Topic
	fifoIdx uint64
	node    *Node
	expire  uint64 // nanotime
}

type topicInfo struct {
	entries            map[uint64]*topicEntry
	fifoHead, fifoTail uint64
	rqItem             *topicRequestQueueItem
	wcl                waitControlLoop
}

// removes tail element from the fifo
func (t *topicInfo) getFifoTail() *topicEntry {
	for t.entries[t.fifoTail] == nil {
		t.fifoTail++
	}
	tail := t.entries[t.fifoTail]
	t.fifoTail++
	return tail
}

type nodeInfo struct {
	entries                          map[Topic]*topicEntry
	noTicketUntil                    uint64 // nanotime
	lastIssuedTicket, lastUsedTicket uint32
}

type TopicTable struct {
	db                    *nodeDB
	nodes                 map[*Node]*nodeInfo
	topics                map[Topic]*topicInfo
	globalEntries         uint64
	requested             topicRequestQueue
	requestCnt            uint64
	lastGarbageCollection uint64 // nanotime
}

func NewTopicTable(db *nodeDB) *TopicTable {
	return &TopicTable{
		db:     db,
		nodes:  make(map[*Node]*nodeInfo),
		topics: make(map[Topic]*topicInfo),
	}
}

func (t *TopicTable) Stop() {
}

func (t *TopicTable) getOrNewTopic(topic Topic) *topicInfo {
	ti := t.topics[topic]
	if ti == nil {
		rqItem := &topicRequestQueueItem{
			topic:    topic,
			priority: t.requestCnt,
		}
		ti = &topicInfo{
			entries: make(map[uint64]*topicEntry),
			rqItem:  rqItem,
		}
		t.topics[topic] = ti
		heap.Push(&t.requested, rqItem)
	}
	return ti
}

// This function assumes that topic is in the topics table.
func (t *TopicTable) checkDeleteTopic(topic Topic) {
	ti := t.topics[topic]
	if len(ti.entries) == 0 && ti.wcl.hasMinimumWaitPeriod() {
		delete(t.topics, topic)
	}
}

func (t *TopicTable) getOrNewNode(node *Node) *nodeInfo {
	n := t.nodes[node]
	if n == nil {
		issued, used := t.db.fetchTopicRegTickets(node.ID)
		n = &nodeInfo{
			entries:          make(map[Topic]*topicEntry),
			lastIssuedTicket: issued,
			lastUsedTicket:   used,
		}
		t.nodes[node] = n
	}
	return n
}

// This function assumes that node is in the nodes table.
func (t *TopicTable) checkDeleteNode(node *Node) {
	n := t.nodes[node]
	if len(n.entries) == 0 && n.noTicketUntil < atime.NanoTime() {
		delete(t.nodes, node)
	}
}

func (t *TopicTable) storeTicket(node *Node) {
	n := t.getOrNewNode(node)
	t.db.updateTopicRegTickets(node.ID, n.lastIssuedTicket, n.lastUsedTicket)
}

func (t *TopicTable) GetEntries(topic Topic) []*Node {
	t.collectGarbage()

	te := t.topics[topic]
	if te == nil {
		return nil
	}
	nodes := make([]*Node, len(te.entries))
	i := 0
	for _, e := range te.entries {
		nodes[i] = e.node
		i++
	}
	t.requestCnt++
	t.requested.update(te.rqItem, t.requestCnt)
	return nodes
}

func (t *TopicTable) AddEntries(node *Node, topics []Topic, expiry time.Duration) {
	n := t.getOrNewNode(node)
	// clear previous entries by the same node
	for _, e := range n.entries {
		t.deleteEntry(e)
	}

	tm := atime.NanoTime()
	for _, topic := range topics {
		te := t.getOrNewTopic(topic)

		if len(te.entries) == MaxEntriesPerTopic {
			t.deleteEntry(te.getFifoTail())
		}

		if t.globalEntries == MaxEntries {
			t.deleteEntry(t.leastRequested()) // not empty, no need to check for nil
		}

		fifoIdx := te.fifoHead
		te.fifoHead++
		entry := &topicEntry{
			topic:   topic,
			fifoIdx: fifoIdx,
			node:    node,
			expire:  tm + uint64(expiry),
		}
		te.entries[fifoIdx] = entry
		n.entries[topic] = entry
		t.globalEntries++
		te.wcl.registered(tm)
	}
}

// removes least requested element from the fifo
func (t *TopicTable) leastRequested() *topicEntry {
	for t.requested.Len() > 0 && t.topics[t.requested[0].topic] == nil {
		heap.Pop(&t.requested)
	}
	if t.requested.Len() == 0 {
		return nil
	}
	return t.topics[t.requested[0].topic].getFifoTail()
}

// entry should exist
func (t *TopicTable) deleteEntry(e *topicEntry) {
	ne := t.nodes[e.node].entries
	delete(ne, e.topic)
	if len(ne) == 0 {
		t.checkDeleteNode(e.node)
	}
	te := t.topics[e.topic]
	delete(te.entries, e.fifoIdx)
	if len(te.entries) == 0 {
		t.checkDeleteTopic(e.topic)
		heap.Remove(&t.requested, te.rqItem.index)
	}
	t.globalEntries--
}

// It is assumed that topics and waitPeriods have the same length.
func (t *TopicTable) useTicket(node *Node, serialNo uint32, topics []Topic, waitPeriods []uint32, expiry time.Duration) (registered bool) {
	t.collectGarbage()

	n := t.getOrNewNode(node)
	if serialNo < n.lastUsedTicket {
		return false
	}
	if serialNo != n.lastUsedTicket {
		n.lastUsedTicket = serialNo
		t.storeTicket(node)
	}

	tm := atime.NanoTime()
	currTime := uint32(tm / 1000000000)
	var regTopics []Topic
	for i, w := range waitPeriods {
		relTime := int32(currTime - w)                    // make it safe even if time turns around
		if relTime >= -1 && relTime <= regTimeWindow+1 && // give clients a little security margin on both ends
			n.entries[topics[i]] == nil { // don't register again if there is an active entry
			regTopics = append(regTopics, topics[i])
		}
	}
	if regTopics != nil {
		t.AddEntries(node, regTopics, expiry)
		n.noTicketUntil = tm + noTicketTimeout()
		return true
	} else {
		return false
	}
}

func (t *TopicTable) getTicket(node *Node, topics []Topic) (serialNo, currTime uint32, waitUntil []uint32) {
	t.collectGarbage()

	tm := atime.NanoTime()
	currTime = uint32(tm / 1000000000)
	n := t.getOrNewNode(node)
	if n.noTicketUntil > tm {
		return
	}
	n.lastIssuedTicket++
	t.storeTicket(node)
	serialNo = n.lastIssuedTicket
	waitUntil = make([]uint32, len(topics))
	for i, topic := range topics {
		var w uint64
		if te := t.topics[topic]; te != nil {
			w = te.wcl.waitPeriod
		} else {
			w = minWaitPeriod
		}
		waitUntil[i] = currTime + uint32(w/1000000000)
	}
	return
}

const gcInterval = uint64(time.Minute)

func (t *TopicTable) collectGarbage() {
	tm := atime.NanoTime()
	if tm-t.lastGarbageCollection < gcInterval {
		return
	}
	t.lastGarbageCollection = tm

	for node, n := range t.nodes {
		for _, e := range n.entries {
			if e.expire <= tm {
				t.deleteEntry(e)
			}
		}

		t.checkDeleteNode(node)
	}

	for topic, _ := range t.topics {
		t.checkDeleteTopic(topic)
	}
}

const (
	minWaitPeriod      = uint64(time.Minute)
	regTimeWindow      = 10 // seconds
	avgNoTicketTimeout = uint64(time.Minute) * 10
	// target average interval between two incoming ad requests
	wcTargetRegInterval = uint64(time.Minute) * 10 / MaxEntriesPerTopic
	//
	wcTimeConst = uint64(time.Minute) * 10
)

// initialization is not required, will set to minWaitPeriod at first registration
type waitControlLoop struct {
	lastIncoming, waitPeriod uint64
}

func (w *waitControlLoop) registered(tm uint64) {
	w.waitPeriod = w.nextWaitPeriod(tm)
	w.lastIncoming = tm
}

func (w *waitControlLoop) nextWaitPeriod(tm uint64) uint64 {
	period := tm - w.lastIncoming
	wp := uint64(float64(w.waitPeriod) * math.Exp((float64(wcTargetRegInterval)-float64(period))/float64(wcTimeConst)))
	if wp < minWaitPeriod {
		wp = minWaitPeriod
	}
	return wp
}

func (w *waitControlLoop) hasMinimumWaitPeriod() bool {
	return w.nextWaitPeriod(atime.NanoTime()) == minWaitPeriod
}

func noTicketTimeout() uint64 {
	e := rand.ExpFloat64()
	if e > 100 {
		e = 100
	}
	return uint64(float64(avgNoTicketTimeout) * e)
}

type topicRequestQueueItem struct {
	topic    Topic
	priority uint64
	index    int
}

// A topicRequestQueue implements heap.Interface and holds topicRequestQueueItems.
type topicRequestQueue []*topicRequestQueueItem

func (tq topicRequestQueue) Len() int { return len(tq) }

func (tq topicRequestQueue) Less(i, j int) bool {
	return tq[i].priority < tq[j].priority
}

func (tq topicRequestQueue) Swap(i, j int) {
	tq[i], tq[j] = tq[j], tq[i]
	tq[i].index = i
	tq[j].index = j
}

func (tq *topicRequestQueue) Push(x interface{}) {
	n := len(*tq)
	item := x.(*topicRequestQueueItem)
	item.index = n
	*tq = append(*tq, item)
}

func (tq *topicRequestQueue) Pop() interface{} {
	old := *tq
	n := len(old)
	item := old[n-1]
	item.index = -1
	*tq = old[0 : n-1]
	return item
}

func (tq *topicRequestQueue) update(item *topicRequestQueueItem, priority uint64) {
	item.priority = priority
	heap.Fix(tq, item.index)
}