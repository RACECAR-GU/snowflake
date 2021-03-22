package lib

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"git.torproject.org/pluggable-transports/snowflake.git/common/encapsulation"
	"git.torproject.org/pluggable-transports/snowflake.git/common/turbotunnel"
	"git.torproject.org/pluggable-transports/snowflake.git/common/websocketconn"
	"github.com/gorilla/websocket"
	"github.com/xtaci/kcp-go/v5"
	"github.com/xtaci/smux"
)

const requestTimeout = 10 * time.Second

// How long to remember outgoing packets for a client, when we don't currently
// have an active WebSocket connection corresponding to that client. Because a
// client session may span multiple WebSocket connections, we keep packets we
// aren't able to send immediately in memory, for a little while but not
// indefinitely.
const clientMapTimeout = 1 * time.Minute

// How big to make the map of ClientIDs to IP addresses. The map is used in
// turbotunnelMode to store a reasonable IP address for a client session that
// may outlive any single WebSocket connection.
const clientIDAddrMapCapacity = 1024

// How long to wait for ListenAndServe or ListenAndServeTLS to return an error
// before deciding that it's not going to return.
const listenAndServeErrorTimeout = 100 * time.Millisecond

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// clientIDAddrMap stores short-term mappings from ClientIDs to IP addresses.
// When we call pt.DialOr, tor wants us to provide a USERADDR string that
// represents the remote IP address of the client (for metrics purposes, etc.).
// This data structure bridges the gap between ServeHTTP, which knows about IP
// addresses, and handleStream, which is what calls pt.DialOr. The common piece
// of information linking both ends of the chain is the ClientID, which is
// attached to the WebSocket connection and every session.
var clientIDAddrMap = newClientIDMap(clientIDAddrMapCapacity)

// overrideReadConn is a net.Conn with an overridden Read method. Compare to
// recordingConn at
// https://dave.cheney.net/2015/05/22/struct-composition-with-go.
type overrideReadConn struct {
	net.Conn
	io.Reader
}

func (conn *overrideReadConn) Read(p []byte) (int, error) {
	return conn.Reader.Read(p)
}

type HTTPHandler struct {
	// pconn is the adapter layer between stream-oriented WebSocket
	// connections and the packet-oriented KCP layer.
	pconn *turbotunnel.QueuePacketConn
	ln    *SnowflakeListener
}

func (handler *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}

	conn := websocketconn.New(ws)
	defer conn.Close()

	// Pass the address of client as the remote address of incoming connection
	clientIPParam := r.URL.Query().Get("client_ip")
	addr := clientAddr(clientIPParam)
	if err != nil {
		log.Printf("Error resolving client ip: %s", err.Error())
		return
	}

	var token [len(turbotunnel.Token)]byte
	_, err = io.ReadFull(conn, token[:])
	if err != nil {
		// Don't bother logging EOF: that happens with an unused
		// connection, which clients make frequently as they maintain a
		// pool of proxies.
		if err != io.EOF {
			log.Printf("reading token: %v", err)
		}
		return
	}

	//Set the read deadline for a connection
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))

	switch {
	case bytes.Equal(token[:], turbotunnel.Token[:]):
		err = turbotunnelMode(conn, addr, handler.pconn, handler.ln)
	default:
		// We didn't find a matching token, which means that we are
		// dealing with a client that doesn't know about such things.
		// "Unread" the token by constructing a new Reader and pass it
		// to the old one-session-per-WebSocket mode.
		conn2 := &overrideReadConn{Conn: conn, Reader: io.MultiReader(bytes.NewReader(token[:]), conn)}
		handler.ln.queue <- &SnowflakeConn{conn: conn2, address: addr}
	}
	if err != nil {
		log.Println(err)
		return
	}
}

// acceptStreams layers an smux.Session on the KCP connection and awaits streams
// on it. Passes each stream to our SnowflakeListener.
func acceptStreams(conn *kcp.UDPSession, queue chan net.Conn) error {
	// Look up the IP address associated with this KCP session, via the
	// ClientID that is returned by the session's RemoteAddr method.
	addr, ok := clientIDAddrMap.Get(conn.RemoteAddr().(turbotunnel.ClientID))
	if !ok {
		// This means that the map is tending to run over capacity, not
		// just that there was not client_ip on the incoming connection.
		// We store "" in the map in the absence of client_ip. This log
		// message means you should increase clientIDAddrMapCapacity.
		log.Printf("no address in clientID-to-IP map (capacity %d)", clientIDAddrMapCapacity)
	}

	smuxConfig := smux.DefaultConfig()
	smuxConfig.Version = 2
	smuxConfig.KeepAliveTimeout = 1 * time.Minute
	smuxConfig.KeepAliveDisabled = true
	sess, err := smux.Server(conn, smuxConfig)
	if err != nil {
		return err
	}

	for {
		stream, err := sess.AcceptStream()
		if err != nil {
			if err, ok := err.(net.Error); ok && err.Temporary() {
				continue
			}
			return err
		}
		go func() {
			queue <- &SnowflakeConn{conn: stream, address: clientAddr(addr)}
		}()
	}
}

// acceptSessions listens for incoming KCP connections and passes them to
// acceptStreams. It is handler.ServeHTTP that provides the network interface
// that drives this function.
func acceptSessions(ln *kcp.Listener, queue chan net.Conn) error {
	for {
		conn, err := ln.AcceptKCP()
		if err != nil {
			if err, ok := err.(net.Error); ok && err.Temporary() {
				continue
			}
			return err
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
		go func() {
			defer conn.Close()
			err := acceptStreams(conn, queue)
			if err != nil && err != io.ErrClosedPipe {
				log.Printf("acceptStreams: %v", err)
			}
		}()
	}
}

// turbotunnelMode handles clients that sent turbotunnel.Token at the start of
// their stream. These clients expect to send and receive encapsulated packets,
// with a long-lived session identified by ClientID.
func turbotunnelMode(conn net.Conn, addr net.Addr, pconn *turbotunnel.QueuePacketConn,
	ln *SnowflakeListener) error {
	// Read the ClientID prefix. Every packet encapsulated in this WebSocket
	// connection pertains to the same ClientID.
	var clientID turbotunnel.ClientID
	_, err := io.ReadFull(conn, clientID[:])
	if err != nil {
		return fmt.Errorf("reading ClientID: %v", err)
	}

	// Store a a short-term mapping from the ClientID to the client IP
	// address attached to this WebSocket connection. tor will want us to
	// provide a client IP address when we call pt.DialOr. But a KCP session
	// does not necessarily correspond to any single IP address--it's
	// composed of packets that are carried in possibly multiple WebSocket
	// streams. We apply the heuristic that the IP address of the most
	// recent WebSocket connection that has had to do with a session, at the
	// time the session is established, is the IP address that should be
	// credited for the entire KCP session.
	clientIDAddrMap.Set(clientID, addr.String())

	errCh := make(chan error)

	// The remainder of the WebSocket stream consists of encapsulated
	// packets. We read them one by one and feed them into the
	// QueuePacketConn on which kcp.ServeConn was set up, which eventually
	// leads to KCP-level sessions in the acceptSessions function.
	go func() {
		for {
			p, err := encapsulation.ReadData(conn)
			if err != nil {
				errCh <- err
				break
			}
			pconn.QueueIncoming(p, clientID)
		}
	}()

	// At the same time, grab packets addressed to this ClientID and
	// encapsulate them into the downstream.
	go func() {
		// Buffer encapsulation.WriteData operations to keep length
		// prefixes in the same send as the data that follows.
		bw := bufio.NewWriter(conn)
		for p := range pconn.OutgoingQueue(clientID) {
			_, err := encapsulation.WriteData(bw, p)
			if err == nil {
				err = bw.Flush()
			}
			if err != nil {
				errCh <- err
				break
			}
		}
	}()

	// Wait until one of the above loops terminates. The closing of the
	// WebSocket connection will terminate the other one.
	<-errCh

	return nil
}

type ClientAddress struct {
	addr net.TCPAddr
}

func (addr ClientAddress) Network() string {
	return "snowflake"
}

func (addr ClientAddress) String() string {
	return addr.addr.String()
}

// Return a client address
func clientAddr(clientIPParam string) net.Addr {
	if clientIPParam == "" {
		return ClientAddress{}
	}
	// Check if client addr is a valid IP
	clientIP := net.ParseIP(clientIPParam)
	if clientIP == nil {
		return ClientAddress{}
	}
	// Check if client addr is 0.0.0.0 or [::]. Some proxies erroneously
	// report an address of 0.0.0.0: https://bugs.torproject.org/33157.
	if clientIP.IsUnspecified() {
		return ClientAddress{}
	}
	// Add a dummy port number. USERADDR requires a port number.
	return ClientAddress{net.TCPAddr{IP: clientIP, Port: 1, Zone: ""}}
}
