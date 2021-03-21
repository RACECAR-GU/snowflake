package lib

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"git.torproject.org/pluggable-transports/snowflake.git/common/messages"
	"git.torproject.org/pluggable-transports/snowflake.git/common/util"
	"github.com/pion/ice/v2"
	"github.com/pion/sdp/v2"
	"github.com/pion/webrtc/v3"
)

const defaultProbeURL = "https://snowflake-broker.torproject.net:8443/probe"

const pollInterval = 5 * time.Second
const (
	NATUnknown      = "unknown"
	NATRestricted   = "restricted"
	NATUnrestricted = "unrestricted"
)

//amount of time after sending an SDP answer before the proxy assumes the
//client is not going to connect
const dataChannelTimeout = 20 * time.Second

const readLimit = 100000 //Maximum number of bytes to be read from an HTTP request

var broker *SignalingServer
var relay string

var currentNATType = NATUnknown

const (
	sessionIDLength = 16
)

var (
	tokens chan bool
	config webrtc.Configuration
	client http.Client
)

var remoteIPPatterns = []*regexp.Regexp{
	/* IPv4 */
	regexp.MustCompile(`(?m)^c=IN IP4 ([\d.]+)(?:(?:\/\d+)?\/\d+)?(:? |\r?\n)`),
	/* IPv6 */
	regexp.MustCompile(`(?m)^c=IN IP6 ([0-9A-Fa-f:.]+)(?:\/\d+)?(:? |\r?\n)`),
}

// Checks whether an IP address is a remote address for the client
func isRemoteAddress(ip net.IP) bool {
	return !(util.IsLocal(ip) || ip.IsUnspecified() || ip.IsLoopback())
}

func remoteIPFromSDP(str string) net.IP {
	// Look for remote IP in "a=candidate" attribute fields
	// https://tools.ietf.org/html/rfc5245#section-15.1
	var desc sdp.SessionDescription
	err := desc.Unmarshal([]byte(str))
	if err != nil {
		log.Println("Error parsing SDP: ", err.Error())
		return nil
	}
	for _, m := range desc.MediaDescriptions {
		for _, a := range m.Attributes {
			if a.IsICECandidate() {
				c, err := ice.UnmarshalCandidate(a.Value)
				if err == nil {
					ip := net.ParseIP(c.Address())
					if ip != nil && isRemoteAddress(ip) {
						return ip
					}
				}
			}
		}
	}
	// Finally look for remote IP in "c=" Connection Data field
	// https://tools.ietf.org/html/rfc4566#section-5.7
	for _, pattern := range remoteIPPatterns {
		m := pattern.FindStringSubmatch(str)
		if m != nil {
			// Ignore parsing errors, ParseIP returns nil.
			ip := net.ParseIP(m[1])
			if ip != nil && isRemoteAddress(ip) {
				return ip
			}

		}
	}

	return nil
}

func getToken() {
	<-tokens
}

func retToken() {
	tokens <- true
}

func genSessionID() string {
	buf := make([]byte, sessionIDLength)
	_, err := rand.Read(buf)
	if err != nil {
		panic(err.Error())
	}
	return strings.TrimRight(base64.StdEncoding.EncodeToString(buf), "=")
}

func limitedRead(r io.Reader, limit int64) ([]byte, error) {
	p, err := ioutil.ReadAll(&io.LimitedReader{R: r, N: limit + 1})
	if err != nil {
		return p, err
	} else if int64(len(p)) == limit+1 {
		return p[0:limit], io.ErrUnexpectedEOF
	}
	return p, err
}

type SignalingServer struct {
	url                *url.URL
	transport          http.RoundTripper
	keepLocalAddresses bool
}

func (s *SignalingServer) Post(path string, payload io.Reader) ([]byte, error) {

	req, err := http.NewRequest("POST", path, payload)
	if err != nil {
		return nil, err
	}
	resp, err := s.transport.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("remote returned status code %d", resp.StatusCode)
	}

	defer resp.Body.Close()
	return limitedRead(resp.Body, readLimit)
}

func (s *SignalingServer) pollOffer(sid string) *webrtc.SessionDescription {
	brokerPath := s.url.ResolveReference(&url.URL{Path: "proxy"})
	timeOfNextPoll := time.Now()
	for {
		// Sleep until we're scheduled to poll again.
		now := time.Now()
		time.Sleep(timeOfNextPoll.Sub(now))
		// Compute the next time to poll -- if it's in the past, that
		// means that the POST took longer than pollInterval, so we're
		// allowed to do another one immediately.
		timeOfNextPoll = timeOfNextPoll.Add(pollInterval)
		if timeOfNextPoll.Before(now) {
			timeOfNextPoll = now
		}

		body, err := messages.EncodePollRequest(sid, "standalone", currentNATType)
		if err != nil {
			log.Printf("Error encoding poll message: %s", err.Error())
			return nil
		}
		resp, err := s.Post(brokerPath.String(), bytes.NewBuffer(body))
		if err != nil {
			log.Printf("error polling broker: %s", err.Error())
		}

		offer, _, err := messages.DecodePollResponse(resp)
		if err != nil {
			log.Printf("Error reading broker response: %s", err.Error())
			log.Printf("body: %s", resp)
			return nil
		}
		if offer != "" {
			offer, err := util.DeserializeSessionDescription(offer)
			if err != nil {
				log.Printf("Error processing session description: %s", err.Error())
				return nil
			}
			return offer

		}
	}
}

func (s *SignalingServer) sendAnswer(sid string, pc *webrtc.PeerConnection) error {
	brokerPath := s.url.ResolveReference(&url.URL{Path: "answer"})
	ld := pc.LocalDescription()
	if !s.keepLocalAddresses {
		ld = &webrtc.SessionDescription{
			Type: ld.Type,
			SDP:  util.StripLocalAddresses(ld.SDP),
		}
	}
	answer, err := util.SerializeSessionDescription(ld)
	if err != nil {
		return err
	}
	body, err := messages.EncodeAnswerRequest(answer, sid)
	if err != nil {
		return err
	}
	resp, err := s.Post(brokerPath.String(), bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("error sending answer to broker: %s", err.Error())
	}

	success, err := messages.DecodeAnswerResponse(resp)
	if err != nil {
		return err
	}
	if !success {
		return fmt.Errorf("broker returned client timeout")
	}

	return nil
}

func CopyLoop(c1 io.ReadWriteCloser, c2 io.ReadWriteCloser) {
	var wg sync.WaitGroup
	copyer := func(dst io.ReadWriteCloser, src io.ReadWriteCloser) {
		defer wg.Done()
		// Ignore io.ErrClosedPipe because it is likely caused by the
		// termination of copyer in the other direction.
		if _, err := io.Copy(dst, src); err != nil && err != io.ErrClosedPipe {
			log.Printf("io.Copy inside CopyLoop generated an error: %v", err)
		}
		dst.Close()
		src.Close()
	}
	wg.Add(2)
	go copyer(c1, c2)
	go copyer(c2, c1)
	wg.Wait()
}

func runSession(sid string) {
	offer := broker.pollOffer(sid)
	if offer == nil {
		log.Printf("bad offer from broker")
		retToken()
		return
	}
	dataChan := make(chan struct{})
	pc, err := makePeerConnectionFromOffer(offer, config, dataChan, datachannelHandler)
	if err != nil {
		log.Printf("error making WebRTC connection: %s", err)
		retToken()
		return
	}
	err = broker.sendAnswer(sid, pc)
	if err != nil {
		log.Printf("error sending answer to client through broker: %s", err)
		if inerr := pc.Close(); inerr != nil {
			log.Printf("error calling pc.Close: %v", inerr)
		}
		retToken()
		return
	}
	// Set a timeout on peerconnection. If the connection state has not
	// advanced to PeerConnectionStateConnected in this time,
	// destroy the peer connection and return the token.
	select {
	case <-dataChan:
		log.Println("Connection successful.")
	case <-time.After(dataChannelTimeout):
		log.Println("Timed out waiting for client to open data channel.")
		if err := pc.Close(); err != nil {
			log.Printf("error calling pc.Close: %v", err)
		}
		retToken()
	}
}

func Proxy(capacity uint, rawBrokerURL, relayURL, stunURL string,
	keepLocalAddresses bool) error {

	var err error

	relay = relayURL
	broker = new(SignalingServer)
	broker.keepLocalAddresses = keepLocalAddresses
	broker.url, err = url.Parse(rawBrokerURL)
	if err != nil {
		log.Fatalf("invalid broker url: %s", err)
	}
	_, err = url.Parse(stunURL)
	if err != nil {
		log.Fatalf("invalid stun url: %s", err)
	}
	_, err = url.Parse(relay)
	if err != nil {
		log.Fatalf("invalid relay url: %s", err)
	}

	broker.transport = http.DefaultTransport.(*http.Transport)
	broker.transport.(*http.Transport).ResponseHeaderTimeout = 15 * time.Second
	config = webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{stunURL},
			},
		},
	}
	tokens = make(chan bool, capacity)
	for i := uint(0); i < capacity; i++ {
		tokens <- true
	}

	// use probetest to determine NAT compatability
	checkNATType(config, defaultProbeURL)
	log.Printf("NAT type: %s", currentNATType)

	for {
		getToken()
		sessionID := genSessionID()
		runSession(sessionID)
	}
}

func checkNATType(config webrtc.Configuration, probeURL string) {

	var err error

	probe := new(SignalingServer)
	probe.transport = http.DefaultTransport.(*http.Transport)
	probe.transport.(*http.Transport).ResponseHeaderTimeout = 30 * time.Second
	probe.url, err = url.Parse(probeURL)
	if err != nil {
		log.Printf("Error parsing url: %s", err.Error())
	}

	// create offer
	dataChan := make(chan struct{})
	pc, err := makeNewPeerConnection(config, dataChan)
	if err != nil {
		log.Printf("error making WebRTC connection: %s", err)
		return
	}

	offer := pc.LocalDescription()
	sdp, err := util.SerializeSessionDescription(offer)
	log.Printf("Offer: %s", sdp)
	if err != nil {
		log.Printf("Error encoding probe message: %s", err.Error())
		return
	}

	// send offer
	body, err := messages.EncodePollResponse(sdp, true, "")
	if err != nil {
		log.Printf("Error encoding probe message: %s", err.Error())
		return
	}
	resp, err := probe.Post(probe.url.String(), bytes.NewBuffer(body))
	if err != nil {
		log.Printf("error polling probe: %s", err.Error())
		return
	}

	sdp, _, err = messages.DecodeAnswerRequest(resp)
	if err != nil {
		log.Printf("Error reading probe response: %s", err.Error())
		return
	}
	answer, err := util.DeserializeSessionDescription(sdp)
	if err != nil {
		log.Printf("Error setting answer: %s", err.Error())
		return
	}
	err = pc.SetRemoteDescription(*answer)
	if err != nil {
		log.Printf("Error setting answer: %s", err.Error())
		return
	}

	select {
	case <-dataChan:
		currentNATType = NATUnrestricted
	case <-time.After(dataChannelTimeout):
		currentNATType = NATRestricted
	}
	if err := pc.Close(); err != nil {
		log.Printf("error calling pc.Close: %v", err)
	}

}
