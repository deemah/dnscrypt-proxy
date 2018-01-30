package main

import (
	"bytes"
	"context"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jedisct1/dlog"
	"golang.org/x/crypto/curve25519"
)

type CachedIPs struct {
	sync.RWMutex
	cache map[string]string
}

type Proxy struct {
	proxyPublicKey               [32]byte
	proxySecretKey               [32]byte
	questionSizeEstimator        QuestionSizeEstimator
	serversInfo                  ServersInfo
	timeout                      time.Duration
	certRefreshDelay             time.Duration
	certRefreshDelayAfterFailure time.Duration
	certIgnoreTimestamp          bool
	mainProto                    string
	listenAddresses              []string
	daemonize                    bool
	registeredServers            []RegisteredServer
	pluginBlockIPv6              bool
	cache                        bool
	cacheSize                    int
	cacheNegTTL                  uint32
	cacheMinTTL                  uint32
	cacheMaxTTL                  uint32
	queryLogFile                 string
	queryLogFormat               string
	queryLogIgnoredQtypes        []string
	nxLogFile                    string
	nxLogFormat                  string
	blockNameFile                string
	blockNameLogFile             string
	blockNameFormat              string
	blockIPFile                  string
	blockIPLogFile               string
	blockIPFormat                string
	forwardFile                  string
	pluginsGlobals               PluginsGlobals
	urlsToPrefetch               []URLToPrefetch
	clientsCount                 uint32
	maxClients                   uint32
	httpTransport                *http.Transport
	cachedIPs                    CachedIPs
}

func (proxy *Proxy) StartProxy() {
	proxy.questionSizeEstimator = NewQuestionSizeEstimator()
	if _, err := rand.Read(proxy.proxySecretKey[:]); err != nil {
		dlog.Fatal(err)
	}
	curve25519.ScalarBaseMult(&proxy.proxyPublicKey, &proxy.proxySecretKey)
	for _, registeredServer := range proxy.registeredServers {
		proxy.serversInfo.registerServer(proxy, registeredServer.name, registeredServer.stamp)
	}
	dialer := &net.Dialer{
		Timeout:   proxy.timeout,
		KeepAlive: proxy.timeout,
		DualStack: true,
	}
	proxy.httpTransport = &http.Transport{
		DisableKeepAlives:      false,
		DisableCompression:     true,
		MaxIdleConns:           1,
		IdleConnTimeout:        proxy.timeout,
		ResponseHeaderTimeout:  proxy.timeout,
		ExpectContinueTimeout:  proxy.timeout,
		MaxResponseHeaderBytes: 4096,
		DialContext: func(ctx context.Context, network, addrStr string) (net.Conn, error) {
			host := addrStr[:strings.LastIndex(addrStr, ":")]
			ipOnly := host
			proxy.cachedIPs.RLock()
			cachedIP := proxy.cachedIPs.cache[host]
			proxy.cachedIPs.RUnlock()
			if len(cachedIP) > 0 {
				ipOnly = cachedIP
			} else {
				dlog.Debugf("[%s] IP address was not cached", host)
			}
			addrStr = ipOnly + addrStr[strings.LastIndex(addrStr, ":"):]
			return dialer.DialContext(ctx, network, addrStr)
		},
	}
	for _, listenAddrStr := range proxy.listenAddresses {
		listenUDPAddr, err := net.ResolveUDPAddr("udp", listenAddrStr)
		if err != nil {
			dlog.Fatal(err)
		}
		listenTCPAddr, err := net.ResolveTCPAddr("tcp", listenAddrStr)
		if err != nil {
			dlog.Fatal(err)
		}
		if err := proxy.udpListenerFromAddr(listenUDPAddr); err != nil {
			dlog.Fatal(err)
		}
		if err := proxy.tcpListenerFromAddr(listenTCPAddr); err != nil {
			dlog.Fatal(err)
		}
	}
	if err := proxy.SystemDListeners(); err != nil {
		dlog.Fatal(err)
	}
	liveServers, err := proxy.serversInfo.refresh(proxy)
	if liveServers > 0 {
		dlog.Noticef("dnscrypt-proxy is ready - live servers: %d", liveServers)
		SystemDNotify()
	} else if err != nil {
		dlog.Error(err)
		dlog.Notice("dnscrypt-proxy is waiting for at least one server to be reachable")
	}
	proxy.prefetcher(&proxy.urlsToPrefetch)
	go func() {
		for {
			delay := proxy.certRefreshDelay
			if proxy.serversInfo.liveServers() == 0 {
				delay = proxy.certRefreshDelayAfterFailure
			}
			time.Sleep(delay)
			proxy.serversInfo.refresh(proxy)
		}
	}()
}

func (proxy *Proxy) prefetcher(urlsToPrefetch *[]URLToPrefetch) {
	go func() {
		for {
			now := time.Now()
			for i := range *urlsToPrefetch {
				urlToPrefetch := &(*urlsToPrefetch)[i]
				if now.After(urlToPrefetch.when) {
					dlog.Debugf("Prefetching [%s]", urlToPrefetch.url)
					if err := PrefetchSourceURL(urlToPrefetch); err != nil {
						dlog.Debugf("Prefetching [%s] failed: %s", err)
					} else {
						dlog.Debugf("Prefetching [%s] succeeded. Next refresh scheduled for %v", urlToPrefetch.url, urlToPrefetch.when)
					}
				}
			}
			time.Sleep(60 * time.Second)
		}
	}()
}

func (proxy *Proxy) udpListener(clientPc *net.UDPConn) {
	defer clientPc.Close()
	for {
		buffer := make([]byte, MaxDNSPacketSize-1)
		length, clientAddr, err := clientPc.ReadFrom(buffer)
		if err != nil {
			return
		}
		packet := buffer[:length]
		go func() {
			if !proxy.clientsCountInc() {
				dlog.Warnf("Too many connections (max=%d)", proxy.maxClients)
				return
			}
			defer proxy.clientsCountDec()
			proxy.processIncomingQuery(proxy.serversInfo.getOne(), "udp", proxy.mainProto, packet, &clientAddr, clientPc)
		}()
	}
}

func (proxy *Proxy) udpListenerFromAddr(listenAddr *net.UDPAddr) error {
	clientPc, err := net.ListenUDP("udp", listenAddr)
	if err != nil {
		return err
	}
	dlog.Noticef("Now listening to %v [UDP]", listenAddr)
	go proxy.udpListener(clientPc)
	return nil
}

func (proxy *Proxy) tcpListener(acceptPc *net.TCPListener) {
	defer acceptPc.Close()
	for {
		clientPc, err := acceptPc.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer clientPc.Close()
			if !proxy.clientsCountInc() {
				dlog.Warnf("Too many connections (max=%d)", proxy.maxClients)
				return
			}
			defer proxy.clientsCountDec()
			clientPc.SetDeadline(time.Now().Add(proxy.timeout))
			packet, err := ReadPrefixed(clientPc.(*net.TCPConn))
			if err != nil || len(packet) < MinDNSPacketSize {
				return
			}
			clientAddr := clientPc.RemoteAddr()
			proxy.processIncomingQuery(proxy.serversInfo.getOne(), "tcp", "tcp", packet, &clientAddr, clientPc)
		}()
	}
}

func (proxy *Proxy) tcpListenerFromAddr(listenAddr *net.TCPAddr) error {
	acceptPc, err := net.ListenTCP("tcp", listenAddr)
	if err != nil {
		return err
	}
	dlog.Noticef("Now listening to %v [TCP]", listenAddr)
	go proxy.tcpListener(acceptPc)
	return nil
}

func (proxy *Proxy) exchangeWithUDPServer(serverInfo *ServerInfo, encryptedQuery []byte, clientNonce []byte) ([]byte, error) {
	pc, err := net.DialUDP("udp", nil, serverInfo.UDPAddr)
	if err != nil {
		return nil, err
	}
	pc.SetDeadline(time.Now().Add(serverInfo.Timeout))
	pc.Write(encryptedQuery)
	encryptedResponse := make([]byte, MaxDNSPacketSize)
	length, err := pc.Read(encryptedResponse)
	pc.Close()
	if err != nil {
		return nil, err
	}
	encryptedResponse = encryptedResponse[:length]
	return proxy.Decrypt(serverInfo, encryptedResponse, clientNonce)
}

func (proxy *Proxy) exchangeWithTCPServer(serverInfo *ServerInfo, encryptedQuery []byte, clientNonce []byte) ([]byte, error) {
	pc, err := net.DialTCP("tcp", nil, serverInfo.TCPAddr)
	if err != nil {
		return nil, err
	}
	pc.SetDeadline(time.Now().Add(serverInfo.Timeout))
	encryptedQuery, err = PrefixWithSize(encryptedQuery)
	if err != nil {
		return nil, err
	}
	pc.Write(encryptedQuery)

	encryptedResponse, err := ReadPrefixed(pc)
	pc.Close()
	if err != nil {
		return nil, err
	}
	return proxy.Decrypt(serverInfo, encryptedResponse, clientNonce)
}

func (proxy *Proxy) clientsCountInc() bool {
	for {
		count := proxy.clientsCount
		if count >= proxy.maxClients {
			return false
		}
		if atomic.CompareAndSwapUint32(&proxy.clientsCount, count, count+1) {
			return true
		}
	}
}

func (proxy *Proxy) clientsCountDec() {
	for {
		if count := proxy.clientsCount; count == 0 || atomic.CompareAndSwapUint32(&proxy.clientsCount, count, count-1) {
			break
		}
	}
}

func (proxy *Proxy) processIncomingQuery(serverInfo *ServerInfo, clientProto string, serverProto string, query []byte, clientAddr *net.Addr, clientPc net.Conn) {
	if len(query) < MinDNSPacketSize || serverInfo == nil {
		return
	}
	pluginsState := NewPluginsState(proxy, clientProto, clientAddr)
	query, _ = pluginsState.ApplyQueryPlugins(&proxy.pluginsGlobals, query)
	var response []byte
	var err error
	if pluginsState.action != PluginsActionForward {
		if pluginsState.synthResponse != nil {
			response, err = pluginsState.synthResponse.PackBuffer(response)
			if err != nil {
				return
			}
		}
		if pluginsState.action == PluginsActionDrop {
			return
		}
	}
	if len(response) == 0 {
		if serverInfo.Proto == StampProtoTypeDNSCrypt {
			encryptedQuery, clientNonce, err := proxy.Encrypt(serverInfo, query, serverProto)
			if err != nil {
				return
			}
			serverInfo.noticeBegin(proxy)
			if serverProto == "udp" {
				response, err = proxy.exchangeWithUDPServer(serverInfo, encryptedQuery, clientNonce)
			} else {
				response, err = proxy.exchangeWithTCPServer(serverInfo, encryptedQuery, clientNonce)
			}
			if err != nil {
				serverInfo.noticeFailure(proxy)
				return
			}
		} else if serverInfo.Proto == StampProtoTypeDoH {
			req := &http.Request{
				Method: "POST",
				URL:    serverInfo.URL,
				Host:   serverInfo.HostName,
				Header: map[string][]string{
					"Accept":       {"application/dns-udpwireformat"},
					"Content-Type": {"application/dns-udpwireformat"},
					"User-Agent":   {"dnscrypt-proxy"},
				},
				Close: false,
				Body:  ioutil.NopCloser(bytes.NewReader(query)),
			}
			client := http.Client{
				Transport: proxy.httpTransport,
				Timeout:   proxy.timeout,
			}
			resp, err := client.Do(req)
			if (err == nil && resp != nil && (resp.StatusCode < 200 || resp.StatusCode > 299)) ||
				err != nil || resp == nil {
				return
			}
			response, err = ioutil.ReadAll(resp.Body)
			if err != nil {
				return
			}
		} else {
			dlog.Fatal("Unsupported protocol")
		}
		if err != nil {
			serverInfo.noticeFailure(proxy)
			return
		}
		response, _ = pluginsState.ApplyResponsePlugins(&proxy.pluginsGlobals, response)
	}
	if len(response) < MinDNSPacketSize || len(response) > MaxDNSPacketSize {
		serverInfo.noticeFailure(proxy)
		return
	}
	if clientProto == "udp" {
		if len(response) > MaxDNSUDPPacketSize {
			response, err = TruncatedResponse(response)
			if err != nil {
				return
			}
		}
		clientPc.(net.PacketConn).WriteTo(response, *clientAddr)
		if HasTCFlag(response) {
			proxy.questionSizeEstimator.blindAdjust()
		} else {
			proxy.questionSizeEstimator.adjust(ResponseOverhead + len(response))
		}
	} else {
		response, err = PrefixWithSize(response)
		if err != nil {
			serverInfo.noticeFailure(proxy)
			return
		}
		clientPc.Write(response)
	}
	serverInfo.noticeSuccess(proxy)
}
