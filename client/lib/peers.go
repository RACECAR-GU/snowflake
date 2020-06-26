package lib

import (
	"container/list"
	"errors"
	"fmt"
	"log"
)

// Container which keeps track of multiple WebRTC remote peers.
// Implements |SnowflakeCollector|.
//
// Maintaining a set of pre-connected Peers with fresh but inactive datachannels
// allows allows rapid recovery when the current WebRTC Peer disconnects.
//
// Note: For now, only one remote can be active at any given moment.
// This is a property of Tor circuits & its current multiplexing constraints,
// but could be updated if that changes.
// (Also, this constraint does not necessarily apply to the more generic PT
// version of Snowflake)
type Peers struct {
	Tongue
	BytesLogger BytesLogger

	snowflakeChan chan *WebRTCPeer
	activePeers   *list.List
	capacity      int

	serverAddr string

	melt chan struct{}
}

// Construct a fresh container of remote peers.
func NewPeers(max int, serverAddr string) *Peers {
	p := &Peers{capacity: max}
	// Use buffered go channel to pass snowflakes onwards to the SOCKS handler.
	p.snowflakeChan = make(chan *WebRTCPeer, max)
	p.activePeers = list.New()
	p.melt = make(chan struct{})
	p.serverAddr = serverAddr
	return p
}

// As part of |SnowflakeCollector| interface.
func (p *Peers) Collect() (*WebRTCPeer, error) {
	cnt := p.Count()
	s := fmt.Sprintf("Currently at [%d/%d]", cnt, p.capacity)
	if cnt >= p.capacity {
		return nil, fmt.Errorf("At capacity [%d/%d]", cnt, p.capacity)
	}
	log.Println("WebRTC: Collecting a new Snowflake.", s)
	// Engage the Snowflake Catching interface, which must be available.
	if nil == p.Tongue {
		return nil, errors.New("missing Tongue to catch Snowflakes with")
	}
	// BUG: some broker conflict here.
	connection, err := p.Tongue.Catch(p.serverAddr)
	if nil != err {
		return nil, err
	}
	// Track new valid Snowflake in internal collection and pass along.
	p.activePeers.PushBack(connection)
	p.snowflakeChan <- connection
	return connection, nil
}

// Pop blocks until an available, valid snowflake appears. Returns nil after End
// has been called.
func (p *Peers) Pop() *WebRTCPeer {
	for {
		snowflake, ok := <-p.snowflakeChan
		if !ok {
			return nil
		}
		if snowflake.closed {
			continue
		}
		// Set to use the same rate-limited traffic logger to keep consistency.
		snowflake.BytesLogger = p.BytesLogger
		return snowflake
	}
}

// As part of |SnowflakeCollector| interface.
func (p *Peers) Melted() <-chan struct{} {
	return p.melt
}

// Returns total available Snowflakes (including the active one)
// The count only reduces when connections themselves close, rather than when
// they are popped.
func (p *Peers) Count() int {
	p.purgeClosedPeers()
	return p.activePeers.Len()
}

func (p *Peers) purgeClosedPeers() {
	for e := p.activePeers.Front(); e != nil; {
		next := e.Next()
		conn := e.Value.(*WebRTCPeer)
		// Purge those marked for deletion.
		if conn.closed {
			p.activePeers.Remove(e)
		}
		e = next
	}
}

// Close all Peers contained here.
func (p *Peers) End() {
	close(p.snowflakeChan)
	close(p.melt)
	cnt := p.Count()
	for e := p.activePeers.Front(); e != nil; {
		next := e.Next()
		conn := e.Value.(*WebRTCPeer)
		conn.Close()
		p.activePeers.Remove(e)
		e = next
	}
	log.Printf("WebRTC: melted all %d snowflakes.", cnt)
}
