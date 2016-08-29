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

package discover

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"time"

	"github.com/aristanetworks/goarista/atime"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

const (
	ticketTimeBucketLen = time.Minute
	timeWindow          = 30 // * ticketTimeBucketLen
	keepTicketConst     = time.Minute * 10
	keepTicketExp       = time.Minute * 5
	maxRadius           = 0xffffffffffffffff
	minRadAverage       = 100
	minRadStableAfter   = 50
	targetWaitTime      = time.Minute * 10
	adjustRatio         = 0.002
	adjustCooldownStart = 0.1
	adjustCooldownStep  = 0.01
	radiusExtendRatio   = 1.5
)

// absTime represents absolute monotonic time in nanoseconds.
type absTime time.Duration

func monotonicTime() absTime {
	return absTime(atime.NanoTime())
}

// timeBucket represents absolute monotonic time in minutes.
// It is used as the index into the per-topic ticket buckets.
type timeBucket int

type ticket struct {
	topics  []Topic
	regTime []absTime // Per-topic local absolute time when the ticket can be used.

	// The serial number that was issued by the server.
	serial uint32
	// Used by registrar, tracks absolute time when the ticket was created.
	issueTime absTime

	// Fields used only by registrants
	node   *Node  // the registrar node that signed this ticket
	refCnt int    // tracks number of topics that will be registered using this ticket
	pong   []byte // encoded pong packet signed by the registrar
}

// ticketRef refers to a single topic in a ticket.
type ticketRef struct {
	t   *ticket
	idx int // index of the topic in t.topics and t.regTime
}

func (ref ticketRef) topic() Topic {
	return ref.t.topics[ref.idx]
}

func (ref ticketRef) topicRegTime() absTime {
	return ref.t.regTime[ref.idx]
}

func pongToTicket(localTime absTime, topics []Topic, node *Node, p *ingressPacket) (*ticket, error) {
	wps := p.data.(*pong).WaitPeriods
	if len(topics) != len(wps) {
		return nil, fmt.Errorf("bad wait period list: got %d values, want %d", len(topics), len(wps))
	}
	if rlpHash(topics) != p.data.(*pong).TopicHash {
		return nil, fmt.Errorf("bad topic hash")
	}
	t := &ticket{
		issueTime: localTime,
		node:      node,
		topics:    topics,
		pong:      p.rawData,
		regTime:   make([]absTime, len(wps)),
	}
	// Convert wait periods to local absolute time.
	for i, wp := range wps {
		t.regTime[i] = localTime + absTime(time.Second*time.Duration(wp))
	}
	return t, nil
}

func ticketToPong(t *ticket, pong *pong) {
	pong.Expiration = uint64(t.issueTime / absTime(time.Second))
	pong.TopicHash = rlpHash(t.topics)
	pong.TicketSerial = t.serial
	pong.WaitPeriods = make([]uint32, len(t.regTime))
	for i, regTime := range t.regTime {
		pong.WaitPeriods[i] = uint32(time.Duration(regTime-t.issueTime) / time.Second)
	}
}

type ticketStore struct {
	// radius detector and target address generator
	// exists for both searched and registered topics
	radius map[Topic]*topicRadius

	// Contains buckets (for each absolute minute) of tickets
	// that can be used in that minute.
	// This is only set if the topic is being registered.
	tickets     map[Topic]topicTickets
	regtopics   []Topic
	nodes       map[*Node]*ticket
	nodeLastReq map[*Node]reqInfo

	lastBucketFetched timeBucket
	nextTicketCached  *ticketRef
	nextTicketReg     absTime

	minRadCnt, minRadPtr uint64
	minRadius, minRadSum uint64
	lastMinRads          [minRadAverage]uint64

	log *logChn
}

type topicTickets map[timeBucket][]ticketRef

func newTicketStore() *ticketStore {
	return &ticketStore{
		radius:      make(map[Topic]*topicRadius),
		tickets:     make(map[Topic]topicTickets),
		nodes:       make(map[*Node]*ticket),
		nodeLastReq: make(map[*Node]reqInfo),
	}
}

// addTopic starts tracking a topic. If register is true,
// the local node will register the topic and tickets will be collected.
// It can be called even
func (s *ticketStore) addTopic(t Topic, register bool) {
	s.log.log(fmt.Sprintf(" addTopic(%v, %v)", t, register))
	if s.radius[t] == nil {
		s.radius[t] = newTopicRadius(t)
	}
	if register && s.tickets[t] == nil {
		s.tickets[t] = make(topicTickets)
	}
}

// removeRegisterTopic deletes all tickets for the given topic.
func (s *ticketStore) removeRegisterTopic(topic Topic) {
	s.log.log(fmt.Sprintf(" removeRegisterTopic(%v)", topic))
	for _, list := range s.tickets[topic] {
		for _, ref := range list {
			ref.t.refCnt--
			if ref.t.refCnt == 0 {
				delete(s.nodes, ref.t.node)
				delete(s.nodeLastReq, ref.t.node)
			}
		}
	}
	delete(s.tickets, topic)
}

func (s *ticketStore) regTopicSet() []Topic {
	topics := make([]Topic, 0, len(s.tickets))
	for topic := range s.tickets {
		topics = append(topics, topic)
	}
	return topics
}

// nextRegisterLookup returns the target of the next lookup for ticket collection.
func (s *ticketStore) nextRegisterLookup() (lookup lookupInfo, delay time.Duration) {
	s.log.log("nextRegisterLookup()")
	firstTopic, ok := s.iterRegTopics()
	for topic := firstTopic; ok; {
		s.log.log(fmt.Sprintf(" checking topic %v, len(s.tickets[topic]) = %d", topic, len(s.tickets[topic])))
		if s.tickets[topic] != nil && s.needMoreTickets(topic) {
			next := s.radius[topic].nextTarget()
			s.log.log(fmt.Sprintf(" %x 1s", next[:8]))
			return lookupInfo{target: next, topic: topic}, 1 * time.Second
		}
		topic, ok = s.iterRegTopics()
		if topic == firstTopic {
			break // We have checked all topics.
		}
	}
	s.log.log(" null, 40s")
	return lookupInfo{}, 40 * time.Second
}

// iterRegTopics returns topics to register in arbitrary order.
// The second return value is false if there are no topics.
func (s *ticketStore) iterRegTopics() (Topic, bool) {
	s.log.log("iterRegTopics()")
	if len(s.regtopics) == 0 {
		if len(s.tickets) == 0 {
			s.log.log(" false")
			return "", false
		}
		// Refill register list.
		for t := range s.tickets {
			s.regtopics = append(s.regtopics, t)
		}
	}
	topic := s.regtopics[len(s.regtopics)-1]
	s.regtopics = s.regtopics[:len(s.regtopics)-1]
	s.log.log(" " + string(topic) + " true")
	return topic, true
}

// ticketsInWindow returns the number of tickets in the registration window.
func (s *ticketStore) needMoreTickets(t Topic) bool {
	now := monotonicTime()
	ltBucket := timeBucket(now / absTime(ticketTimeBucketLen))
	var sum float64
	tickets := s.tickets[t]
	for g := ltBucket; g < ltBucket+timeWindow; g++ {
		for _, t := range tickets[g] {
			l := t.t.regTime[t.idx] - t.t.issueTime
			if l > absTime(ticketTimeBucketLen)*timeWindow {
				l = absTime(ticketTimeBucketLen) * timeWindow
			}
			if l < absTime(time.Minute) {
				l = absTime(time.Minute)
			}
			sum += float64(targetWaitTime) / float64(l)
		}
	}
	s.log.log(fmt.Sprintf("ticketsInWindow(%v) = %v", t, sum))
	return sum < 10
}

// nextRegisterableTicket returns the next ticket that can be used
// to register.
//
// If the returned wait time <= zero the ticket can be used. For a positive
// wait time, the caller should requery the next ticket later.
//
// A ticket can be returned more than once with <= zero wait time in case
// the ticket contains multiple topics.
func (s *ticketStore) nextRegisterableTicket() (t *ticketRef, wait time.Duration) {
	defer func() {
		if t == nil {
			s.log.log(" nil")
		} else {
			s.log.log(fmt.Sprintf(" node = %x sn = %v wait = %v", t.t.node.ID[:8], t.t.serial, wait))
		}
	}()

	s.log.log("nextRegisterableTicket()")
	now := monotonicTime()
	if s.nextTicketCached != nil {
		return s.nextTicketCached, time.Duration(s.nextTicketCached.topicRegTime() - now)
	}

	for bucket := s.lastBucketFetched; ; bucket++ {
		var (
			empty      = true    // true if there are no tickets
			nextTicket ticketRef // uninitialized if this bucket is empty
		)
		for _, tickets := range s.tickets {
			if len(tickets) != 0 {
				empty = false
				if list := tickets[bucket]; list != nil {
					for _, ref := range list {
						//s.log.log(fmt.Sprintf(" nrt bucket = %d node = %x sn = %v wait = %v", bucket, ref.t.node.ID[:8], ref.t.serial, time.Duration(ref.topicRegTime()-now)))
						if nextTicket.t == nil || ref.topicRegTime() < nextTicket.topicRegTime() {
							nextTicket = ref
						}
					}
				}
			}
		}
		if empty {
			return nil, 0
		}
		if nextTicket.t != nil {
			wait = time.Duration(nextTicket.topicRegTime() - now)
			s.nextTicketCached = &nextTicket
			return &nextTicket, wait
		}
		s.lastBucketFetched = bucket
	}
}

// ticketRegistered is called when t has been used to register for a topic.
func (s *ticketStore) ticketRegistered(ref ticketRef) {
	s.log.log(fmt.Sprintf("ticketRegistered(node = %x sn = %v)", ref.t.node.ID[:8], ref.t.serial))
	topic := ref.topic()
	tickets := s.tickets[topic]
	if tickets == nil {
		return
	}
	bucket := timeBucket(ref.t.regTime[ref.idx] / absTime(ticketTimeBucketLen))
	list := tickets[bucket]
	idx := -1
	for i, bt := range list {
		if bt.t == ref.t {
			idx = i
			break
		}
	}
	if idx == -1 {
		panic(nil)
	}
	list = append(list[:idx], list[idx+1:]...)
	if len(list) != 0 {
		tickets[bucket] = list
	} else {
		delete(tickets, bucket)
	}
	ref.t.refCnt--
	if ref.t.refCnt == 0 {
		delete(s.nodes, ref.t.node)
		delete(s.nodeLastReq, ref.t.node)
	}

	// Make nextRegisterableTicket return the next available ticket.
	s.nextTicketCached = nil
}

type lookupInfo struct {
	target common.Hash
	topic  Topic
}

type reqInfo struct {
	pingHash []byte
	topic    Topic
}

// returns -1 if not found
func (t *ticket) findIdx(topic Topic) int {
	for i, tt := range t.topics {
		if tt == topic {
			return i
		}
	}
	return -1
}

func (s *ticketStore) registerLookupDone(lookup lookupInfo, nodes []*Node, ping func(n *Node) []byte) {
	now := monotonicTime()
	//fmt.Printf("registerLookupDone  target = %016x\n", target[:8])
	if len(nodes) > 0 {
		s.adjustMinRadius(lookup.target, nodes[0].sha)
	}
	for i, n := range nodes {
		if i == 0 || (binary.BigEndian.Uint64(n.sha[:8])^binary.BigEndian.Uint64(lookup.target[:8])) < s.minRadius {
			if t := s.nodes[n]; t != nil {
				// adjust radius with already stored ticket
				if idx := t.findIdx(lookup.topic); idx != -1 {
					s.adjustWithTicket(now, t, idx, false)
				}
			} else {
				// request a new pong packet
				s.nodeLastReq[n] = reqInfo{pingHash: ping(n), topic: lookup.topic}
			}
		}
	}
}

func (s *ticketStore) adjustWithTicket(localTime absTime, t *ticket, idx int, onlyConverging bool) {
	if onlyConverging {
		for i, topic := range t.topics {
			if tt, ok := s.radius[topic]; ok && !tt.converged && tt.isInRadius(t, true) {
				tt.adjust(localTime, ticketRef{t, i}, s.minRadius, s.minRadCnt >= minRadStableAfter)
				s.log.log(fmt.Sprintf("adjust converging topic: %v, rad: %v, cd: %v, converged: %v", topic, float64(tt.radius)/maxRadius, tt.adjustCooldown, tt.converged))
			}
		}
	} else {
		topic := t.topics[idx]
		if tt, ok := s.radius[topic]; ok && tt.isInRadius(t, true) {
			tt.adjust(localTime, ticketRef{t, idx}, s.minRadius, s.minRadCnt >= minRadStableAfter)
			s.log.log(fmt.Sprintf("adjust topic: %v, rad: %v, cd: %v, converged: %v", topic, float64(tt.radius)/maxRadius, tt.adjustCooldown, tt.converged))
		}
	}
}

func (s *ticketStore) addTicket(localTime absTime, pingHash []byte, t *ticket) {
	s.log.log(fmt.Sprintf("add(node = %x sn = %v)", t.node.ID[:8], t.serial))

	if s.nodes[t.node] != nil {
		return
	}

	lastReq, ok := s.nodeLastReq[t.node]
	if !(ok && bytes.Equal(pingHash, lastReq.pingHash)) {
		s.adjustWithTicket(localTime, t, -1, true)
		return
	}
	topic := lastReq.topic
	topicIdx := t.findIdx(topic)
	if topicIdx == -1 {
		return
	}

	s.adjustWithTicket(localTime, t, topicIdx, false)
	bucket := timeBucket(localTime / absTime(ticketTimeBucketLen))
	if s.lastBucketFetched == 0 || bucket < s.lastBucketFetched {
		s.lastBucketFetched = bucket
	}

	for topicIdx, topic := range t.topics {
		if tt, ok := s.radius[topic]; ok && tt.isInRadius(t, false) && s.needMoreTickets(topic) {
			if tickets, ok := s.tickets[topic]; ok && tt.converged {
				wait := t.regTime[topicIdx] - localTime
				rnd := rand.ExpFloat64()
				if rnd > 10 {
					rnd = 10
				}
				if float64(wait) < float64(keepTicketConst)+float64(keepTicketExp)*rnd {
					// use the ticket to register this topic
					bucket := timeBucket(t.regTime[topicIdx] / absTime(ticketTimeBucketLen))
					tickets[bucket] = append(tickets[bucket], ticketRef{t, topicIdx})
					t.refCnt++
				}
			}
		}
	}

	if t.refCnt > 0 {
		s.nextTicketCached = nil
		s.nodes[t.node] = t
	}
}

func (s *ticketStore) getNodeTicket(node *Node) *ticket {
	if s.nodes[node] == nil {
		s.log.log(fmt.Sprintf("getNodeTicket(%x) sn = nil", node.ID[:8]))
	} else {
		s.log.log(fmt.Sprintf("getNodeTicket(%x) sn = %v", node.ID[:8], s.nodes[node].serial))
	}
	return s.nodes[node]
}

func (s *ticketStore) adjustMinRadius(target, found common.Hash) {
	tp := binary.BigEndian.Uint64(target[0:8])
	fp := binary.BigEndian.Uint64(found[0:8])
	dist := tp ^ fp

	var mr uint64
	if dist < maxRadius/16 {
		mr = dist * 16
	} else {
		mr = maxRadius
	}
	mr /= minRadAverage

	s.minRadSum -= s.lastMinRads[s.minRadPtr]
	s.lastMinRads[s.minRadPtr] = mr
	s.minRadSum += mr
	s.minRadPtr++
	if s.minRadPtr == minRadAverage {
		s.minRadPtr = 0
	}
	s.minRadCnt++

	if s.minRadCnt < minRadAverage {
		s.minRadius = (s.minRadSum / s.minRadCnt) * minRadAverage
	} else {
		s.minRadius = s.minRadSum
	}
	s.log.log(fmt.Sprintf("adjustMinRadius() %v", float64(s.minRadius)/maxRadius))
}

type topicRadius struct {
	topic           Topic
	topicHashPrefix uint64
	radius          uint64
	adjustCooldown  float64 // only for convergence detection
	converged       bool
	intExtBalance   float64
}

func newTopicRadius(t Topic) *topicRadius {
	topicHash := crypto.Keccak256Hash([]byte(t))
	topicHashPrefix := binary.BigEndian.Uint64(topicHash[0:8])

	return &topicRadius{
		topic:           t,
		topicHashPrefix: topicHashPrefix,
		radius:          maxRadius,
		adjustCooldown:  adjustCooldownStart,
		converged:       false,
	}
}

func (r *topicRadius) isInRadius(t *ticket, extRadius bool) bool {
	nodePrefix := binary.BigEndian.Uint64(t.node.sha[0:8])
	dist := nodePrefix ^ r.topicHashPrefix
	if extRadius {
		return float64(dist) < float64(r.radius)*radiusExtendRatio
	}
	return dist < r.radius
}

func randUint64n(n uint64) uint64 { // don't care about lowest bit, 63 bit randomness is more than enough
	if n < 4 {
		return 0
	}
	return uint64(rand.Int63n(int64(n/2))) * 2
}

func (r *topicRadius) nextTarget() common.Hash {
	var rnd uint64
	if r.intExtBalance < 0 {
		// select target from inner region
		rnd = randUint64n(r.radius)
	} else {
		// select target from outer region
		e := float64(r.radius) * radiusExtendRatio
		extRadius := uint64(maxRadius)
		if e < maxRadius {
			extRadius = uint64(e)
		}
		rnd = r.radius + randUint64n(extRadius-r.radius)
	}
	prefix := r.topicHashPrefix ^ rnd
	var target common.Hash
	binary.BigEndian.PutUint64(target[0:8], prefix)
	return target
}

func (r *topicRadius) adjust(localTime absTime, t ticketRef, minRadius uint64, minRadStable bool) {
	var balanceStep, stepSign float64
	if r.isInRadius(t.t, false) {
		balanceStep = radiusExtendRatio - 1
		stepSign = 1
	} else {
		balanceStep = -1
		stepSign = -1
	}

	if r.intExtBalance*stepSign > 3 {
		return
	}
	r.intExtBalance += balanceStep

	wait := t.t.regTime[t.idx] - t.t.issueTime // localTime
	/*	adjust := (float64(wait)/float64(targetWaitTime) - 1) * 2
		if adjust > 1 {
			adjust = 1
		}
		if adjust < -1 {
			adjust = -1
		}*/
	var adjust float64
	if wait > absTime(targetWaitTime) {
		adjust = 1
	} else {
		adjust = -1
	}

	if r.converged {
		adjust *= adjustRatio
	} else {
		adjust *= r.adjustCooldown
	}

	/*if adjust > 0 {
		adjust *= radiusExtendRatio*2 - 1
	}*/

	radius := float64(r.radius) * (1 + adjust)
	if radius > float64(maxRadius) {
		r.radius = maxRadius
	} else {
		r.radius = uint64(radius)
		if r.radius < minRadius {
			r.radius = minRadius
		}
	}

	if !r.converged && (adjust > 0 || (r.radius == minRadius && minRadStable)) {
		r.adjustCooldown *= (1 - adjustCooldownStep)
		if r.adjustCooldown <= adjustRatio {
			r.converged = true
		}
	}

}

type logChn struct {
	list []string
}

func (c *logChn) log(s string) {
	if c != nil {
		fmt.Println(time.Now().String() + " : " + s)
		//c.list = append(c.list, time.Now().String()+" : "+s)
	}
}

func (c *logChn) printLogs() {
	if c != nil {
		for _, s := range c.list {
			fmt.Println(s)
		}
	}
}

func newlogChn() *logChn {
	return &logChn{}
}