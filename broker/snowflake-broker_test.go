package broker

import (
	"bytes"
	"container/heap"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"
)

func NullLogger() *log.Logger {
	logger := log.New(os.Stdout, "", 0)
	logger.SetOutput(ioutil.Discard)
	return logger
}

var promOnce sync.Once

func TestBroker(t *testing.T) {

	Convey("Context", t, func() {
		ctx := NewBrokerContext(NullLogger())

		Convey("Adds Snowflake", func() {
			So(ctx.snowflakes.Len(), ShouldEqual, 0)
			So(len(ctx.idToSnowflake), ShouldEqual, 0)
			ctx.AddSnowflake("foo", "", NATUnrestricted)
			So(ctx.snowflakes.Len(), ShouldEqual, 1)
			So(len(ctx.idToSnowflake), ShouldEqual, 1)
		})

		Convey("Broker goroutine matches clients with proxies", func() {
			p := new(ProxyPoll)
			p.id = "test"
			p.natType = "unrestricted"
			p.offerChannel = make(chan *ClientOffer)
			go func(ctx *BrokerContext) {
				ctx.proxyPolls <- p
				close(ctx.proxyPolls)
			}(ctx)
			ctx.Broker()
			So(ctx.snowflakes.Len(), ShouldEqual, 1)
			snowflake := heap.Pop(ctx.snowflakes).(*Snowflake)
			snowflake.offerChannel <- &ClientOffer{sdp: []byte("test offer")}
			offer := <-p.offerChannel
			So(ctx.idToSnowflake["test"], ShouldNotBeNil)
			So(offer.sdp, ShouldResemble, []byte("test offer"))
			So(ctx.snowflakes.Len(), ShouldEqual, 0)
		})

		Convey("Request an offer from the Snowflake Heap", func() {
			done := make(chan *ClientOffer)
			go func() {
				offer := ctx.RequestOffer("test", "", NATUnrestricted)
				done <- offer
			}()
			request := <-ctx.proxyPolls
			request.offerChannel <- &ClientOffer{sdp: []byte("test offer")}
			offer := <-done
			So(offer.sdp, ShouldResemble, []byte("test offer"))
		})

		Convey("Responds to client offers...", func() {
			w := httptest.NewRecorder()
			data := bytes.NewReader([]byte("test"))
			r, err := http.NewRequest("POST", "snowflake.broker/client", data)
			So(err, ShouldBeNil)

			Convey("with 503 when no snowflakes are available.", func() {
				clientOffers(ctx, w, r)
				So(w.Code, ShouldEqual, http.StatusServiceUnavailable)
				So(w.Body.String(), ShouldEqual, "")
			})

			Convey("with a proxy answer if available.", func() {
				done := make(chan bool)
				// Prepare a fake proxy to respond with.
				snowflake := ctx.AddSnowflake("fake", "", NATUnrestricted)
				go func() {
					clientOffers(ctx, w, r)
					done <- true
				}()
				offer := <-snowflake.offerChannel
				So(offer.sdp, ShouldResemble, []byte("test"))
				snowflake.answerChannel <- []byte("fake answer")
				<-done
				So(w.Body.String(), ShouldEqual, "fake answer")
				So(w.Code, ShouldEqual, http.StatusOK)
			})

			Convey("Times out when no proxy responds.", func() {
				if testing.Short() {
					return
				}
				done := make(chan bool)
				snowflake := ctx.AddSnowflake("fake", "", NATUnrestricted)
				go func() {
					clientOffers(ctx, w, r)
					// Takes a few seconds here...
					done <- true
				}()
				offer := <-snowflake.offerChannel
				So(offer.sdp, ShouldResemble, []byte("test"))
				<-done
				So(w.Code, ShouldEqual, http.StatusGatewayTimeout)
			})
		})

		Convey("Responds to proxy polls...", func() {
			done := make(chan bool)
			w := httptest.NewRecorder()
			data := bytes.NewReader([]byte(`{"Sid":"ymbcCMto7KHNGYlp","Version":"1.0"}`))
			r, err := http.NewRequest("POST", "snowflake.broker/proxy", data)
			So(err, ShouldBeNil)

			Convey("with a client offer if available.", func() {
				go func(ctx *BrokerContext) {
					proxyPolls(ctx, w, r)
					done <- true
				}(ctx)
				// Pass a fake client offer to this proxy
				p := <-ctx.proxyPolls
				So(p.id, ShouldEqual, "ymbcCMto7KHNGYlp")
				p.offerChannel <- &ClientOffer{sdp: []byte("fake offer")}
				<-done
				So(w.Code, ShouldEqual, http.StatusOK)
				So(w.Body.String(), ShouldEqual, `{"Status":"client match","Offer":"fake offer","NAT":""}`)
			})

			Convey("return empty 200 OK when no client offer is available.", func() {
				go func(ctx *BrokerContext) {
					proxyPolls(ctx, w, r)
					done <- true
				}(ctx)
				p := <-ctx.proxyPolls
				So(p.id, ShouldEqual, "ymbcCMto7KHNGYlp")
				// nil means timeout
				p.offerChannel <- nil
				<-done
				So(w.Body.String(), ShouldEqual, `{"Status":"no match","Offer":"","NAT":""}`)
				So(w.Code, ShouldEqual, http.StatusOK)
			})
		})

		Convey("Responds to proxy answers...", func() {
			s := ctx.AddSnowflake("test", "", NATUnrestricted)
			w := httptest.NewRecorder()
			data := bytes.NewReader([]byte(`{"Version":"1.0","Sid":"test","Answer":"test"}`))

			Convey("by passing to the client if valid.", func() {
				r, err := http.NewRequest("POST", "snowflake.broker/answer", data)
				So(err, ShouldBeNil)
				go func(ctx *BrokerContext) {
					proxyAnswers(ctx, w, r)
				}(ctx)
				answer := <-s.answerChannel
				So(w.Code, ShouldEqual, http.StatusOK)
				So(answer, ShouldResemble, []byte("test"))
			})

			Convey("with client gone status if the proxy is not recognized", func() {
				data = bytes.NewReader([]byte(`{"Version":"1.0","Sid":"invalid","Answer":"test"}`))
				r, err := http.NewRequest("POST", "snowflake.broker/answer", data)
				So(err, ShouldBeNil)
				proxyAnswers(ctx, w, r)
				So(w.Code, ShouldEqual, http.StatusOK)
				b, err := ioutil.ReadAll(w.Body)
				So(err, ShouldBeNil)
				So(b, ShouldResemble, []byte(`{"Status":"client gone"}`))

			})

			Convey("with error if the proxy gives invalid answer", func() {
				data := bytes.NewReader(nil)
				r, err := http.NewRequest("POST", "snowflake.broker/answer", data)
				So(err, ShouldBeNil)
				proxyAnswers(ctx, w, r)
				So(w.Code, ShouldEqual, http.StatusBadRequest)
			})

			Convey("with error if the proxy writes too much data", func() {
				data := bytes.NewReader(make([]byte, 100001))
				r, err := http.NewRequest("POST", "snowflake.broker/answer", data)
				So(err, ShouldBeNil)
				proxyAnswers(ctx, w, r)
				So(w.Code, ShouldEqual, http.StatusBadRequest)
			})

		})

	})

	Convey("End-To-End", t, func() {
		ctx := NewBrokerContext(NullLogger())

		Convey("Check for client/proxy data race", func() {
			proxy_done := make(chan bool)
			client_done := make(chan bool)

			go ctx.Broker()

			// Make proxy poll
			wp := httptest.NewRecorder()
			datap := bytes.NewReader([]byte(`{"Sid":"ymbcCMto7KHNGYlp","Version":"1.0"}`))
			rp, err := http.NewRequest("POST", "snowflake.broker/proxy", datap)
			So(err, ShouldBeNil)

			go func(ctx *BrokerContext) {
				proxyPolls(ctx, wp, rp)
				proxy_done <- true
			}(ctx)

			// Client offer
			wc := httptest.NewRecorder()
			datac := bytes.NewReader([]byte("test"))
			rc, err := http.NewRequest("POST", "snowflake.broker/client", datac)
			So(err, ShouldBeNil)

			go func() {
				clientOffers(ctx, wc, rc)
				client_done <- true
			}()

			<-proxy_done
			So(wp.Code, ShouldEqual, http.StatusOK)

			// Proxy answers
			wp = httptest.NewRecorder()
			datap = bytes.NewReader([]byte(`{"Version":"1.0","Sid":"ymbcCMto7KHNGYlp","Answer":"test"}`))
			rp, err = http.NewRequest("POST", "snowflake.broker/answer", datap)
			So(err, ShouldBeNil)
			go func(ctx *BrokerContext) {
				proxyAnswers(ctx, wp, rp)
				proxy_done <- true
			}(ctx)

			<-proxy_done
			<-client_done

		})

		Convey("Ensure correct snowflake brokering", func() {
			done := make(chan bool)
			polled := make(chan bool)

			// Proxy polls with its ID first...
			dataP := bytes.NewReader([]byte(`{"Sid":"ymbcCMto7KHNGYlp","Version":"1.0"}`))
			wP := httptest.NewRecorder()
			rP, err := http.NewRequest("POST", "snowflake.broker/proxy", dataP)
			So(err, ShouldBeNil)
			go func() {
				proxyPolls(ctx, wP, rP)
				polled <- true
			}()

			// Manually do the Broker goroutine action here for full control.
			p := <-ctx.proxyPolls
			So(p.id, ShouldEqual, "ymbcCMto7KHNGYlp")
			s := ctx.AddSnowflake(p.id, "", NATUnrestricted)
			go func() {
				offer := <-s.offerChannel
				p.offerChannel <- offer
			}()
			So(ctx.idToSnowflake["ymbcCMto7KHNGYlp"], ShouldNotBeNil)

			// Client request blocks until proxy answer arrives.
			dataC := bytes.NewReader([]byte("fake offer"))
			wC := httptest.NewRecorder()
			rC, err := http.NewRequest("POST", "snowflake.broker/client", dataC)
			So(err, ShouldBeNil)
			go func() {
				clientOffers(ctx, wC, rC)
				done <- true
			}()

			<-polled
			So(wP.Code, ShouldEqual, http.StatusOK)
			So(wP.Body.String(), ShouldResemble, `{"Status":"client match","Offer":"fake offer","NAT":"unknown"}`)
			So(ctx.idToSnowflake["ymbcCMto7KHNGYlp"], ShouldNotBeNil)
			// Follow up with the answer request afterwards
			wA := httptest.NewRecorder()
			dataA := bytes.NewReader([]byte(`{"Version":"1.0","Sid":"ymbcCMto7KHNGYlp","Answer":"test"}`))
			rA, err := http.NewRequest("POST", "snowflake.broker/answer", dataA)
			So(err, ShouldBeNil)
			proxyAnswers(ctx, wA, rA)
			So(wA.Code, ShouldEqual, http.StatusOK)

			<-done
			So(wC.Code, ShouldEqual, http.StatusOK)
			So(wC.Body.String(), ShouldEqual, "test")
		})
	})
}

func TestSnowflakeHeap(t *testing.T) {
	Convey("SnowflakeHeap", t, func() {
		h := new(SnowflakeHeap)
		heap.Init(h)
		So(h.Len(), ShouldEqual, 0)
		s1 := new(Snowflake)
		s2 := new(Snowflake)
		s3 := new(Snowflake)
		s4 := new(Snowflake)
		s1.clients = 4
		s2.clients = 5
		s3.clients = 3
		s4.clients = 1

		heap.Push(h, s1)
		So(h.Len(), ShouldEqual, 1)
		heap.Push(h, s2)
		So(h.Len(), ShouldEqual, 2)
		heap.Push(h, s3)
		So(h.Len(), ShouldEqual, 3)
		heap.Push(h, s4)
		So(h.Len(), ShouldEqual, 4)

		heap.Remove(h, 0)
		So(h.Len(), ShouldEqual, 3)

		r := heap.Pop(h).(*Snowflake)
		So(h.Len(), ShouldEqual, 2)
		So(r.clients, ShouldEqual, 3)
		So(r.index, ShouldEqual, -1)

		r = heap.Pop(h).(*Snowflake)
		So(h.Len(), ShouldEqual, 1)
		So(r.clients, ShouldEqual, 4)
		So(r.index, ShouldEqual, -1)

		r = heap.Pop(h).(*Snowflake)
		So(h.Len(), ShouldEqual, 0)
		So(r.clients, ShouldEqual, 5)
		So(r.index, ShouldEqual, -1)
	})
}

func TestGeoip(t *testing.T) {
	Convey("Geoip", t, func() {
		tv4 := new(GeoIPv4Table)
		err := GeoIPLoadFile(tv4, "test_geoip")
		So(err, ShouldEqual, nil)
		tv6 := new(GeoIPv6Table)
		err = GeoIPLoadFile(tv6, "test_geoip6")
		So(err, ShouldEqual, nil)

		Convey("IPv4 Country Mapping Tests", func() {
			for _, test := range []struct {
				addr, cc string
				ok       bool
			}{
				{
					"129.97.208.23", //uwaterloo
					"CA",
					true,
				},
				{
					"127.0.0.1",
					"",
					false,
				},
				{
					"255.255.255.255",
					"",
					false,
				},
				{
					"0.0.0.0",
					"",
					false,
				},
				{
					"223.252.127.255", //test high end of range
					"JP",
					true,
				},
				{
					"223.252.127.255", //test low end of range
					"JP",
					true,
				},
			} {
				country, ok := GetCountryByAddr(tv4, net.ParseIP(test.addr))
				So(country, ShouldEqual, test.cc)
				So(ok, ShouldResemble, test.ok)
			}
		})

		Convey("IPv6 Country Mapping Tests", func() {
			for _, test := range []struct {
				addr, cc string
				ok       bool
			}{
				{
					"2620:101:f000:0:250:56ff:fe80:168e", //uwaterloo
					"CA",
					true,
				},
				{
					"fd00:0:0:0:0:0:0:1",
					"",
					false,
				},
				{
					"0:0:0:0:0:0:0:0",
					"",
					false,
				},
				{
					"ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff",
					"",
					false,
				},
				{
					"2a07:2e47:ffff:ffff:ffff:ffff:ffff:ffff", //test high end of range
					"FR",
					true,
				},
				{
					"2a07:2e40::", //test low end of range
					"FR",
					true,
				},
			} {
				country, ok := GetCountryByAddr(tv6, net.ParseIP(test.addr))
				So(country, ShouldEqual, test.cc)
				So(ok, ShouldResemble, test.ok)
			}
		})

		// Make sure things behave properly if geoip file fails to load
		ctx := NewBrokerContext(NullLogger())
		if err := ctx.metrics.LoadGeoipDatabases("invalid_filename", "invalid_filename6"); err != nil {
			log.Printf("loading geo ip databases returned error: %v", err)
		}
		ctx.metrics.UpdateCountryStats("127.0.0.1", "", NATUnrestricted)
		So(ctx.metrics.tablev4, ShouldEqual, nil)

	})
}

func TestMetrics(t *testing.T) {
	Convey("Test metrics...", t, func() {
		done := make(chan bool)
		buf := new(bytes.Buffer)
		ctx := NewBrokerContext(log.New(buf, "", 0))

		err := ctx.metrics.LoadGeoipDatabases("test_geoip", "test_geoip6")
		So(err, ShouldEqual, nil)

		//Test addition of proxy polls
		Convey("for proxy polls", func() {
			w := httptest.NewRecorder()
			data := bytes.NewReader([]byte("{\"Sid\":\"ymbcCMto7KHNGYlp\",\"Version\":\"1.0\"}"))
			r, err := http.NewRequest("POST", "snowflake.broker/proxy", data)
			r.RemoteAddr = "129.97.208.23:8888" //CA geoip
			So(err, ShouldBeNil)
			go func(ctx *BrokerContext) {
				proxyPolls(ctx, w, r)
				done <- true
			}(ctx)
			p := <-ctx.proxyPolls //manually unblock poll
			p.offerChannel <- nil
			<-done

			w = httptest.NewRecorder()
			data = bytes.NewReader([]byte(`{"Sid":"ymbcCMto7KHNGYlp","Version":"1.0","Type":"standalone"}`))
			r, err = http.NewRequest("POST", "snowflake.broker/proxy", data)
			r.RemoteAddr = "129.97.208.23:8888" //CA geoip
			So(err, ShouldBeNil)
			go func(ctx *BrokerContext) {
				proxyPolls(ctx, w, r)
				done <- true
			}(ctx)
			p = <-ctx.proxyPolls //manually unblock poll
			p.offerChannel <- nil
			<-done

			w = httptest.NewRecorder()
			data = bytes.NewReader([]byte(`{"Sid":"ymbcCMto7KHNGYlp","Version":"1.0","Type":"badge"}`))
			r, err = http.NewRequest("POST", "snowflake.broker/proxy", data)
			r.RemoteAddr = "129.97.208.23:8888" //CA geoip
			So(err, ShouldBeNil)
			go func(ctx *BrokerContext) {
				proxyPolls(ctx, w, r)
				done <- true
			}(ctx)
			p = <-ctx.proxyPolls //manually unblock poll
			p.offerChannel <- nil
			<-done

			w = httptest.NewRecorder()
			data = bytes.NewReader([]byte(`{"Sid":"ymbcCMto7KHNGYlp","Version":"1.0","Type":"webext"}`))
			r, err = http.NewRequest("POST", "snowflake.broker/proxy", data)
			r.RemoteAddr = "129.97.208.23:8888" //CA geoip
			So(err, ShouldBeNil)
			go func(ctx *BrokerContext) {
				proxyPolls(ctx, w, r)
				done <- true
			}(ctx)
			p = <-ctx.proxyPolls //manually unblock poll
			p.offerChannel <- nil
			<-done
			ctx.metrics.printMetrics()
			So(buf.String(), ShouldResemble, "snowflake-stats-end "+time.Now().UTC().Format("2006-01-02 15:04:05")+" (86400 s)\nsnowflake-ips CA=4\nsnowflake-ips-total 4\nsnowflake-ips-standalone 1\nsnowflake-ips-badge 1\nsnowflake-ips-webext 1\nsnowflake-idle-count 8\nclient-denied-count 0\nclient-restricted-denied-count 0\nclient-unrestricted-denied-count 0\nclient-snowflake-match-count 0\nsnowflake-ips-nat-restricted 0\nsnowflake-ips-nat-unrestricted 0\nsnowflake-ips-nat-unknown 1\n")

		})

		//Test addition of client failures
		Convey("for no proxies available", func() {
			w := httptest.NewRecorder()
			data := bytes.NewReader([]byte("test"))
			r, err := http.NewRequest("POST", "snowflake.broker/client", data)
			So(err, ShouldBeNil)

			clientOffers(ctx, w, r)

			ctx.metrics.printMetrics()
			So(buf.String(), ShouldContainSubstring, "client-denied-count 8\nclient-restricted-denied-count 8\nclient-unrestricted-denied-count 0\nclient-snowflake-match-count 0")

			// Test reset
			buf.Reset()
			ctx.metrics.zeroMetrics()
			ctx.metrics.printMetrics()
			So(buf.String(), ShouldContainSubstring, "snowflake-ips \nsnowflake-ips-total 0\nsnowflake-ips-standalone 0\nsnowflake-ips-badge 0\nsnowflake-ips-webext 0\nsnowflake-idle-count 0\nclient-denied-count 0\nclient-restricted-denied-count 0\nclient-unrestricted-denied-count 0\nclient-snowflake-match-count 0\nsnowflake-ips-nat-restricted 0\nsnowflake-ips-nat-unrestricted 0\nsnowflake-ips-nat-unknown 0\n")
		})
		//Test addition of client matches
		Convey("for client-proxy match", func() {
			w := httptest.NewRecorder()
			data := bytes.NewReader([]byte("test"))
			r, err := http.NewRequest("POST", "snowflake.broker/client", data)
			So(err, ShouldBeNil)

			// Prepare a fake proxy to respond with.
			snowflake := ctx.AddSnowflake("fake", "", NATUnrestricted)
			go func() {
				clientOffers(ctx, w, r)
				done <- true
			}()
			offer := <-snowflake.offerChannel
			So(offer.sdp, ShouldResemble, []byte("test"))
			snowflake.answerChannel <- []byte("fake answer")
			<-done

			ctx.metrics.printMetrics()
			So(buf.String(), ShouldContainSubstring, "client-denied-count 0\nclient-restricted-denied-count 0\nclient-unrestricted-denied-count 0\nclient-snowflake-match-count 8")
		})
		//Test rounding boundary
		Convey("binning boundary", func() {
			w := httptest.NewRecorder()
			data := bytes.NewReader([]byte("test"))
			r, err := http.NewRequest("POST", "snowflake.broker/client", data)
			So(err, ShouldBeNil)

			clientOffers(ctx, w, r)
			clientOffers(ctx, w, r)
			clientOffers(ctx, w, r)
			clientOffers(ctx, w, r)
			clientOffers(ctx, w, r)
			clientOffers(ctx, w, r)
			clientOffers(ctx, w, r)
			clientOffers(ctx, w, r)

			ctx.metrics.printMetrics()
			So(buf.String(), ShouldContainSubstring, "client-denied-count 8\nclient-restricted-denied-count 8\nclient-unrestricted-denied-count 0\n")

			clientOffers(ctx, w, r)
			buf.Reset()
			ctx.metrics.printMetrics()
			So(buf.String(), ShouldContainSubstring, "client-denied-count 16\nclient-restricted-denied-count 16\nclient-unrestricted-denied-count 0\n")
		})

		//Test unique ip
		Convey("proxy counts by unique ip", func() {
			w := httptest.NewRecorder()
			data := bytes.NewReader([]byte(`{"Sid":"ymbcCMto7KHNGYlp","Version":"1.0"}`))
			r, err := http.NewRequest("POST", "snowflake.broker/proxy", data)
			r.RemoteAddr = "129.97.208.23:8888" //CA geoip
			So(err, ShouldBeNil)
			go func(ctx *BrokerContext) {
				proxyPolls(ctx, w, r)
				done <- true
			}(ctx)
			p := <-ctx.proxyPolls //manually unblock poll
			p.offerChannel <- nil
			<-done

			data = bytes.NewReader([]byte(`{"Sid":"ymbcCMto7KHNGYlp","Version":"1.0"}`))
			r, err = http.NewRequest("POST", "snowflake.broker/proxy", data)
			if err != nil {
				log.Printf("unable to get NewRequest with error: %v", err)
			}
			r.RemoteAddr = "129.97.208.23:8888" //CA geoip
			go func(ctx *BrokerContext) {
				proxyPolls(ctx, w, r)
				done <- true
			}(ctx)
			p = <-ctx.proxyPolls //manually unblock poll
			p.offerChannel <- nil
			<-done

			ctx.metrics.printMetrics()
			So(buf.String(), ShouldContainSubstring, "snowflake-ips CA=1\nsnowflake-ips-total 1")
		})
		//Test NAT types
		Convey("proxy counts by NAT type", func() {
			w := httptest.NewRecorder()
			data := bytes.NewReader([]byte(`{"Sid":"ymbcCMto7KHNGYlp","Version":"1.2","Type":"unknown","NAT":"restricted"}`))
			r, err := http.NewRequest("POST", "snowflake.broker/proxy", data)
			r.RemoteAddr = "129.97.208.23:8888" //CA geoip
			So(err, ShouldBeNil)
			go func(ctx *BrokerContext) {
				proxyPolls(ctx, w, r)
				done <- true
			}(ctx)
			p := <-ctx.proxyPolls //manually unblock poll
			p.offerChannel <- nil
			<-done

			ctx.metrics.printMetrics()
			So(buf.String(), ShouldContainSubstring, "snowflake-ips-nat-restricted 1\nsnowflake-ips-nat-unrestricted 0\nsnowflake-ips-nat-unknown 0")

			data = bytes.NewReader([]byte(`{"Sid":"ymbcCMto7KHNGYlp","Version":"1.2","Type":"unknown","NAT":"unrestricted"}`))
			r, err = http.NewRequest("POST", "snowflake.broker/proxy", data)
			if err != nil {
				log.Printf("unable to get NewRequest with error: %v", err)
			}
			r.RemoteAddr = "129.97.208.24:8888" //CA geoip
			go func(ctx *BrokerContext) {
				proxyPolls(ctx, w, r)
				done <- true
			}(ctx)
			p = <-ctx.proxyPolls //manually unblock poll
			p.offerChannel <- nil
			<-done

			ctx.metrics.printMetrics()
			So(buf.String(), ShouldContainSubstring, "snowflake-ips-nat-restricted 1\nsnowflake-ips-nat-unrestricted 1\nsnowflake-ips-nat-unknown 0")
		})
		//Test client failures by NAT type
		Convey("client failures by NAT type", func() {
			w := httptest.NewRecorder()
			data := bytes.NewReader([]byte("test"))
			r, err := http.NewRequest("POST", "snowflake.broker/client", data)
			r.Header.Set("Snowflake-NAT-TYPE", "restricted")
			So(err, ShouldBeNil)

			clientOffers(ctx, w, r)

			ctx.metrics.printMetrics()
			So(buf.String(), ShouldContainSubstring, "client-denied-count 8\nclient-restricted-denied-count 8\nclient-unrestricted-denied-count 0\nclient-snowflake-match-count 0")

			buf.Reset()
			ctx.metrics.zeroMetrics()

			r, err = http.NewRequest("POST", "snowflake.broker/client", data)
			r.Header.Set("Snowflake-NAT-TYPE", "unrestricted")
			So(err, ShouldBeNil)

			clientOffers(ctx, w, r)

			ctx.metrics.printMetrics()
			So(buf.String(), ShouldContainSubstring, "client-denied-count 8\nclient-restricted-denied-count 0\nclient-unrestricted-denied-count 8\nclient-snowflake-match-count 0")

			buf.Reset()
			ctx.metrics.zeroMetrics()

			r, err = http.NewRequest("POST", "snowflake.broker/client", data)
			r.Header.Set("Snowflake-NAT-TYPE", "unknown")
			So(err, ShouldBeNil)

			clientOffers(ctx, w, r)

			ctx.metrics.printMetrics()
			So(buf.String(), ShouldContainSubstring, "client-denied-count 8\nclient-restricted-denied-count 8\nclient-unrestricted-denied-count 0\nclient-snowflake-match-count 0")
		})
		Convey("for country stats order", func() {

			stats := map[string]int{
				"IT": 50,
				"FR": 200,
				"TZ": 100,
				"CN": 250,
				"RU": 150,
				"CA": 1,
				"BE": 1,
				"PH": 1,
			}
			ctx.metrics.countryStats.counts = stats
			So(ctx.metrics.countryStats.Display(), ShouldEqual, "CN=250,FR=200,RU=150,TZ=100,IT=50,BE=1,CA=1,PH=1")
		})
	})
}
