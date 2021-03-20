package lib

import (
	"crypto/tls"
	"log"
	"net"
	"net/http"
	"time"

	"git.torproject.org/pluggable-transports/snowflake.git/common/turbotunnel"
	"github.com/xtaci/kcp-go/v5"
	"golang.org/x/net/http2"
)

type Transport struct {
	getCert func(*tls.ClientHelloInfo) (*tls.Certificate, error)
}

func NewSnowflakeTransportServer(getCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)) *Transport {

	return &Transport{getCert: getCertificate}
}

func (t *Transport) Listen(addr string) (*SnowflakeListener, error) {
	ipaddr, _ := net.ResolveIPAddr("tcp", addr)
	listener := &SnowflakeListener{addr: ipaddr, queue: make(chan net.Conn, 65534)}

	handler := HTTPHandler{
		// pconn is shared among all connections to this server. It
		// overlays packet-based client sessions on top of ephemeral
		// WebSocket connections.
		pconn: turbotunnel.NewQueuePacketConn(ipaddr, clientMapTimeout),
	}
	server := &http.Server{
		Addr:        addr,
		Handler:     &handler,
		ReadTimeout: requestTimeout,
	}
	// We need to override server.TLSConfig.GetCertificate--but first
	// server.TLSConfig needs to be non-nil. If we just create our own new
	// &tls.Config, it will lack the default settings that the net/http
	// package sets up for things like HTTP/2. Therefore we first call
	// http2.ConfigureServer for its side effect of initializing
	// server.TLSConfig properly. An alternative would be to make a dummy
	// net.Listener, call Serve on it, and let it return.
	// https://github.com/golang/go/issues/16588#issuecomment-237386446
	err := http2.ConfigureServer(server, nil)
	if err != nil {
		return nil, err
	}
	server.TLSConfig.GetCertificate = t.getCert

	// Another unfortunate effect of the inseparable net/http ListenAndServe
	// is that we can't check for Listen errors like "permission denied" and
	// "address already in use" without potentially entering the infinite
	// loop of Serve. The hack we apply here is to wait a short time,
	// listenAndServeErrorTimeout, to see if an error is returned (because
	// it's better if the error message goes to the tor log through
	// SMETHOD-ERROR than if it only goes to the snowflake log).
	errChan := make(chan error)
	go func() {
		if t.getCert == nil {
			// TLS is disabled
			log.Printf("listening with plain HTTP on %s", addr)
			err := server.ListenAndServe()
			if err != nil {
				log.Printf("error in ListenAndServe: %s", err)
			}
			errChan <- err
		} else {
			log.Printf("listening with HTTPS on %s", addr)
			err := server.ListenAndServeTLS("", "")
			if err != nil {
				log.Printf("error in ListenAndServeTLS: %s", err)
			}
			errChan <- err
		}
	}()

	select {
	case err = <-errChan:
		break
	case <-time.After(listenAndServeErrorTimeout):
		break
	}

	// Start a KCP engine, set up to read and write its packets over the
	// WebSocket connections that arrive at the web server.
	// handler.ServeHTTP is responsible for encapsulation/decapsulation of
	// packets on behalf of KCP. KCP takes those packets and turns them into
	// sessions which appear in the acceptSessions function.
	ln, err := kcp.ServeConn(nil, 0, 0, handler.pconn)
	if err != nil {
		server.Close()
		return nil, err
	}
	go func() {
		defer ln.Close()
		err := acceptSessions(ln, listener.queue)
		if err != nil {
			log.Printf("acceptSessions: %v", err)
		}
	}()

	return listener, nil

}

type SnowflakeListener struct {
	addr  net.Addr
	queue chan net.Conn
}

// Allows the caller to accept incoming Snowflake connections
func (l *SnowflakeListener) Accept() (net.Conn, error) {
	//FIXME: what if queue is closed
	conn := <-l.queue
	return conn, nil
}

func (l *SnowflakeListener) Addr() net.Addr {
	return l.addr
}

func (l *SnowflakeListener) Close() error {
	//FIXME: still need to do
	return nil
}

// A wrapper for the underlying oneshot or turbotunnel conn
// because we need to reference our mapping to determine the client
// address
type SnowflakeConn struct {
	address net.Addr
	conn    net.Conn
}

func (conn *SnowflakeConn) Write(b []byte) (n int, err error) {
	return conn.conn.Write(b)
}

func (conn *SnowflakeConn) Read(b []byte) (n int, err error) {
	return conn.conn.Read(b)
}

func (conn *SnowflakeConn) Close() error {
	return conn.conn.Close()
}

func (conn *SnowflakeConn) LocalAddr() net.Addr {
	return conn.conn.LocalAddr()
}

func (conn *SnowflakeConn) RemoteAddr() net.Addr {
	return conn.address
}

func (conn *SnowflakeConn) SetDeadline(t time.Time) error {
	return conn.conn.SetDeadline(t)
}

func (conn *SnowflakeConn) SetReadDeadline(t time.Time) error {
	return conn.conn.SetReadDeadline(t)
}

func (conn *SnowflakeConn) SetWriteDeadline(t time.Time) error {
	return conn.conn.SetWriteDeadline(t)
}
