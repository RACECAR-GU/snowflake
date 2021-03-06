package lib

import (
	"context"
	"errors"
	"log"
	"math/rand"
	"net"
	"strings"
	"time"

	"github.com/RACECAR-GU/snowflake/common/nat"
	"github.com/RACECAR-GU/snowflake/common/turbotunnel"
	"github.com/pion/webrtc/v3"
	"github.com/xtaci/kcp-go/v5"
	"github.com/xtaci/smux"
)

const (
	ReconnectTimeout = 10 * time.Second
	SnowflakeTimeout = 20 * time.Second
	// How long to wait for the OnOpen callback on a DataChannel.
	DataChannelTimeout = 10 * time.Second
)

type dummyAddr struct{}

func (addr dummyAddr) Network() string { return "dummy" }
func (addr dummyAddr) String() string  { return "dummy" }

// Transport is a structure with methods that conform to the Go PT v2.1 API
// https://github.com/Pluggable-Transports/Pluggable-Transports-spec/blob/master/releases/PTSpecV2.1/Pluggable%20Transport%20Specification%20v2.1%20-%20Go%20Transport%20API.pdf
type Transport struct {
	dialer *WebRTCDialer
}

// Create a new Snowflake transport client that can spawn multiple Snowflake connections.
// brokerURL and frontDomain are the urls for the broker host and domain fronting host
// iceAddresses are the STUN/TURN urls needed for WebRTC negotiation
// keepLocalAddresses is a flag to enable sending local network addresses (for testing purposes)
// max is the maximum number of snowflakes the client should gather for each SOCKS connection
func NewSnowflakeClient(brokerURL, frontDomain string, iceAddresses []string, keepLocalAddresses bool, max int) (*Transport, error) {

	log.Println("\n\n\n --- Starting Snowflake Client ---")

	iceServers := parseIceServers(iceAddresses)
	// chooses a random subset of servers from inputs
	rand.Seed(time.Now().UnixNano())
	rand.Shuffle(len(iceServers), func(i, j int) {
		iceServers[i], iceServers[j] = iceServers[j], iceServers[i]
	})
	if len(iceServers) > 2 {
		iceServers = iceServers[:(len(iceServers)+1)/2]
	}
	log.Printf("Using ICE servers:")
	for _, server := range iceServers {
		log.Printf("url: %v", strings.Join(server.URLs, " "))
	}

	// Use potentially domain-fronting broker to rendezvous.
	broker, err := NewBrokerChannel(
		brokerURL, frontDomain, CreateBrokerTransport(),
		keepLocalAddresses)
	if err != nil {
		return nil, err
	}
	go updateNATType(iceServers, broker)

	transport := &Transport{dialer: NewWebRTCDialer(broker, iceServers, max)}

	return transport, nil
}

// Create a new Snowflake connection. Starts the collection of snowflakes and returns a
// smux Stream.
func (t *Transport) Dial() (net.Conn, error) {
	// Cleanup functions to run before returning, in case of an error.
	var cleanup []func()
	defer func() {
		// Run cleanup in reverse order, as defer does.
		for i := len(cleanup) - 1; i >= 0; i-- {
			cleanup[i]()
		}
	}()

	// Prepare to collect remote WebRTC peers.
	snowflakes, err := NewPeers(t.dialer)
	if err != nil {
		return nil, err
	}
	cleanup = append(cleanup, func() { snowflakes.End() })

	// Use a real logger to periodically output how much traffic is happening.
	snowflakes.BytesLogger = NewBytesSyncLogger()

	log.Printf("---- SnowflakeConn: begin collecting snowflakes ---")
	go connectLoop(snowflakes)

	// Create a new smux session
	log.Printf("---- SnowflakeConn: starting a new session ---")
	pconn, sess, err := newSession(snowflakes)
	if err != nil {
		return nil, err
	}
	cleanup = append(cleanup, func() {
		pconn.Close()
		sess.Close()
	})

	// On the smux session we overlay a stream.
	stream, err := sess.OpenStream()
	if err != nil {
		return nil, err
	}
	// Begin exchanging data.
	log.Printf("---- SnowflakeConn: begin stream %v ---", stream.ID())
	cleanup = append(cleanup, func() { stream.Close() })

	// All good, clear the cleanup list.
	cleanup = nil
	return &SnowflakeConn{Stream: stream, sess: sess, pconn: pconn, snowflakes: snowflakes}, nil
}

type SnowflakeConn struct {
	*smux.Stream
	sess       *smux.Session
	pconn      net.PacketConn
	snowflakes *Peers
}

func (conn *SnowflakeConn) Close() error {
	log.Printf("---- SnowflakeConn: closed stream %v ---", conn.ID())
	conn.Stream.Close()
	log.Printf("---- SnowflakeConn: end collecting snowflakes ---")
	conn.snowflakes.End()
	conn.pconn.Close()
	log.Printf("---- SnowflakeConn: discarding finished session ---")
	conn.sess.Close()
	return nil //TODO: return errors if any of the above do
}

// loop through all provided STUN servers until we exhaust the list or find
// one that is compatable with RFC 5780
func updateNATType(servers []webrtc.ICEServer, broker *BrokerChannel) {

	var restrictedNAT bool
	var err error
	for _, server := range servers {
		addr := strings.TrimPrefix(server.URLs[0], "stun:")
		restrictedNAT, err = nat.CheckIfRestrictedNAT(addr)
		if err == nil {
			if restrictedNAT {
				broker.SetNATType(nat.NATRestricted)
			} else {
				broker.SetNATType(nat.NATUnrestricted)
			}
			break
		}
	}
	if err != nil {
		broker.SetNATType(nat.NATUnknown)
	}
}

// Returns a slice of webrtc.ICEServer given a slice of addresses
func parseIceServers(addresses []string) []webrtc.ICEServer {
	var servers []webrtc.ICEServer
	if len(addresses) == 0 {
		return nil
	}
	for _, url := range addresses {
		url = strings.TrimSpace(url)
		servers = append(servers, webrtc.ICEServer{
			URLs: []string{url},
		})
	}
	return servers
}

// newSession returns a new smux.Session and the net.PacketConn it is running
// over. The net.PacketConn successively connects through Snowflake proxies
// pulled from snowflakes.
func newSession(snowflakes SnowflakeCollector) (net.PacketConn, *smux.Session, error) {
	clientID := turbotunnel.NewClientID()

	// We build a persistent KCP session on a sequence of ephemeral WebRTC
	// connections. This dialContext tells RedialPacketConn how to get a new
	// WebRTC connection when the previous one dies. Inside each WebRTC
	// connection, we use EncapsulationPacketConn to encode packets into a
	// stream.
	dialContext := func(ctx context.Context) (net.PacketConn, error) {
		log.Printf("redialing on same connection")
		// Obtain an available WebRTC remote. May block.
		conn := snowflakes.Pop()
		if conn == nil {
			return nil, errors.New("handler: Received invalid Snowflake")
		}
		log.Println("---- Handler: snowflake assigned ----")
		// Send the magic Turbo Tunnel token.
		_, err := conn.Write(turbotunnel.Token[:])
		if err != nil {
			return nil, err
		}
		// Send ClientID prefix.
		_, err = conn.Write(clientID[:])
		if err != nil {
			return nil, err
		}
		return NewEncapsulationPacketConn(dummyAddr{}, dummyAddr{}, conn), nil
	}
	pconn := turbotunnel.NewRedialPacketConn(dummyAddr{}, dummyAddr{}, dialContext)

	// conn is built on the underlying RedialPacketConn—when one WebRTC
	// connection dies, another one will be found to take its place. The
	// sequence of packets across multiple WebRTC connections drives the KCP
	// engine.
	conn, err := kcp.NewConn2(dummyAddr{}, nil, 0, 0, pconn)
	if err != nil {
		pconn.Close()
		return nil, nil, err
	}
	// Permit coalescing the payloads of consecutive sends.
	conn.SetStreamMode(true)
	// Set the maximum send and receive window sizes to a high number
	// Removes KCP bottlenecks: https://gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/-/issues/40026
	conn.SetWindowSize(65535, 65535)
	// Disable the dynamic congestion window (limit only by the
	// maximum of local and remote static windows).
	conn.SetNoDelay(
		0, // default nodelay
		0, // default interval
		0, // default resend
		1, // nc=1 => congestion window off
	)
	// On the KCP connection we overlay an smux session and stream.
	smuxConfig := smux.DefaultConfig()
	smuxConfig.Version = 2
	smuxConfig.KeepAliveTimeout = 1 * time.Minute
	smuxConfig.KeepAliveDisabled = true
	sess, err := smux.Client(conn, smuxConfig)
	if err != nil {
		conn.Close()
		pconn.Close()
		return nil, nil, err
	}

	return pconn, sess, err
}

// Maintain |SnowflakeCapacity| number of available WebRTC connections, to
// transfer to the Tor SOCKS handler when needed.
func connectLoop(snowflakes SnowflakeCollector) {
	for {
		timer := time.After(ReconnectTimeout)
		_, err := snowflakes.Collect()
		if err != nil {
			log.Printf("WebRTC: %v  Retrying...", err)
		}
		select {
		case <-timer:
			continue
		case <-snowflakes.Melted():
			log.Println("ConnectLoop: stopped.")
			return
		}
	}
}
