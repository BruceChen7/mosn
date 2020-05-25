/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
package connpool

import (
	"context"
	"mosn.io/api"
	mosnctx "mosn.io/mosn/pkg/context"
	"mosn.io/mosn/pkg/log"
	"mosn.io/mosn/pkg/network"
	"mosn.io/mosn/pkg/protocol"
	"mosn.io/mosn/pkg/protocol/xprotocol"
	"mosn.io/mosn/pkg/stream"
	"mosn.io/mosn/pkg/types"
	"mosn.io/pkg/utils"
	"sync"
	"sync/atomic"
	"time"
)

// for xprotocol
const (
	Init = iota
	Connecting
	Connected
)

// RegisterProtoConnPoolFactory register a protocol connection pool factory
func RegisterProtoConnPoolFactory(proto api.Protocol) {
	network.RegisterNewPoolFactory(proto, NewConnPool)
	types.RegisterConnPoolFactory(proto, true)
}

// types.ConnectionPool
type connpool struct {
	// sub protocol -> activeClients
	// sub protocol of http is "", sub of http2 is ""
	idleClients map[api.Protocol][]*activeClient

	host       atomic.Value
	supportTLS bool
	clientMux  sync.Mutex

	totalClientCount uint64 // total clients
	protocol         api.Protocol
}

// NewConnPool init a connection pool
func NewConnPool(proto api.Protocol, host types.Host) types.ConnectionPool {
	p := &connpool{
		supportTLS:  host.SupportTLS(),
		protocol:    proto,
		idleClients: make(map[api.Protocol][]*activeClient),
	}

	p.host.Store(host)

	return p
}

// SupportTLS get whether the pool supports TLS
func (p *connpool) SupportTLS() bool {
	return p.supportTLS
}

// 1. default use async connect
// 2. the pool mode was set to multiplex
func (p *connpool) useAsyncConnect(proto api.Protocol, subproto types.ProtocolName) bool {
	// HACK LOGIC
	// if the pool mode was set to mutiplex
	// we should not use async connect
	if proto == protocol.Xprotocol &&
		xprotocol.GetProtocol(subproto).PoolMode() == types.Multiplex {
		return true
	}

	return false
}

func (p *connpool) Protocol() types.ProtocolName {
	return p.protocol
}

func (p *connpool) shouldMultiplex(subproto types.ProtocolName) bool {
	switch p.protocol {
	case protocol.HTTP1:
		return false
	case protocol.HTTP2:
		return true
	case protocol.Xprotocol:
		if xprotocol.GetProtocol(subproto).PoolMode() == types.Multiplex {
			return true
		}
	}
	return false
}

// CheckAndInit init the connection pool
func (p *connpool) CheckAndInit(ctx context.Context) bool {
	subProtocol := getSubProtocol(ctx)

	// set the pool's multiplex mode
	p.shouldMultiplex(subProtocol)

	// get whether async connect or not
	if !p.useAsyncConnect(p.protocol, subProtocol) {
		return true
	}

	var client *activeClient

	// async connect only support multiplex mode !!!
	p.clientMux.Lock()
	{
		if len(p.idleClients[subProtocol]) == 0 {
			p.idleClients[subProtocol] = []*activeClient{{state: Init}} // fake client
		}

		clients := p.idleClients[subProtocol]
		lastIdx := len(clients) - 1
		client = clients[lastIdx]
	}
	p.clientMux.Unlock()

	if atomic.LoadUint32(&client.state) == Connected {
		return true
	}

	// asynchronously connect to host
	// if there is a bad host, directly connect it may hang our request
	if atomic.CompareAndSwapUint32(&client.state, Init, Connecting) {
		utils.GoWithRecover(func() {
			if log.DefaultLogger.GetLogLevel() >= log.DEBUG {
				log.DefaultLogger.Debugf("[stream] [sofarpc] [connpool] init host %s", p.Host().AddressString())
			}

			p.clientMux.Lock()
			defer p.clientMux.Unlock()
			lastIdx := len(p.idleClients[subProtocol]) - 1
			client, _ := p.newActiveClient(context.Background(), subProtocol)
			if client != nil {
				client.state = Connected
				p.idleClients[subProtocol][lastIdx] = client
			} else {
				delete(p.idleClients, subProtocol)
			}
		}, nil)
	}

	return false

}

func (p *connpool) Host() types.Host {
	h := p.host.Load()
	if host, ok := h.(types.Host); ok {
		return host
	}

	return nil
}

// StreamSender Create a client stream and call's by proxy
func (p *connpool) StreamSender(ctx context.Context, receiver types.StreamReceiveListener) (types.PoolFailureReason, types.Host, types.StreamSender) {
	host := p.Host()

	c, reason := p.GetActiveClient(ctx, getSubProtocol(ctx))
	if reason != "" {
		return reason, host, nil
	}

	var streamEncoder = c.StreamClient().NewStream(ctx, receiver)

	// FIXME one way
	// is there any need to skip the metrics?
	if receiver == nil {
		return "", host, streamEncoder
	}

	streamEncoder.GetStream().AddEventListener(c)

	host.HostStats().UpstreamRequestActive.Inc(1)
	host.ClusterInfo().Stats().UpstreamRequestActive.Inc(1)
	host.ClusterInfo().ResourceManager().Requests().Increase()

	return "", host, streamEncoder
}

// GetActiveClient get a avail client
func (p *connpool) GetActiveClient(ctx context.Context, subProtocol types.ProtocolName) (types.PooledClient, types.PoolFailureReason) {

	host := p.Host()
	if !host.ClusterInfo().ResourceManager().Requests().CanCreate() {
		host.HostStats().UpstreamRequestPendingOverflow.Inc(1)
		host.ClusterInfo().Stats().UpstreamRequestPendingOverflow.Inc(1)
		return nil, types.Overflow
	}

	// FIXME, http1 加锁建连会有性能问题
	p.clientMux.Lock()
	defer p.clientMux.Unlock()

	if p.useAsyncConnect(p.protocol, subProtocol) && len(p.idleClients[subProtocol]) > 0 {
		// the client was inited in the CheckAndInit function
		lastIdx := len(p.idleClients[subProtocol]) - 1
		return p.idleClients[subProtocol][lastIdx], ""
	}

	n := len(p.idleClients[subProtocol])

	// max conns is 0 means no limit
	maxConns := host.ClusterInfo().ResourceManager().Connections().Max()
	// no available client
	var (
		c *activeClient
		reason types.PoolFailureReason
	)

	if n == 0 {
		if maxConns == 0 || p.totalClientCount < maxConns {
			c, reason = p.newActiveClient(ctx, subProtocol)
			if c != nil && reason == "" {
				p.totalClientCount++

				if p.shouldMultiplex(subProtocol) {
					// HTTP/2 && xprotocol
					// should put this conn to pool
					p.idleClients[subProtocol] = append(p.idleClients[subProtocol], c)
				}
			}

			goto RET
		} else {
			host.HostStats().UpstreamRequestPendingOverflow.Inc(1)
			host.ClusterInfo().Stats().UpstreamRequestPendingOverflow.Inc(1)
			c, reason = nil, types.Overflow

			goto RET
		}
	} else {
		var lastIdx = n - 1
		if p.shouldMultiplex(subProtocol) {
			// HTTP/2 && xprotocol
			var reason types.PoolFailureReason
			c = p.idleClients[subProtocol][lastIdx]
			if c == nil || atomic.LoadUint32(&c.goaway) == 1 {
				c, reason = p.newActiveClient(ctx, subProtocol)
				if reason == "" && c != nil {
					p.idleClients[subProtocol][lastIdx] = c
				}
			}

			goto RET
		} else {
			// Only refuse extra connection, keepalive-connection is closed by timeout
			usedConns := p.totalClientCount - uint64(n) + 1
			if maxConns != 0 && usedConns > host.ClusterInfo().ResourceManager().Connections().Max() {
				host.HostStats().UpstreamRequestPendingOverflow.Inc(1)
				host.ClusterInfo().Stats().UpstreamRequestPendingOverflow.Inc(1)
				c, reason = nil, types.Overflow
				goto RET
			}

			c = p.idleClients[subProtocol][lastIdx]
			p.idleClients[subProtocol][lastIdx] = nil
			p.idleClients[subProtocol] = p.idleClients[subProtocol][:lastIdx]

			goto RET
		}
	}

RET:
	if p.useAsyncConnect(p.protocol, subProtocol) && c != nil && atomic.LoadUint32(&c.state) != Connected {
		return nil, types.ConnectionFailure
	}

	if c != nil && p.shouldMultiplex(subProtocol) {
		atomic.AddUint64(&c.totalStream, 1)
	}

	if c != nil && reason == "" {
		host.HostStats().UpstreamRequestTotal.Inc(1)
		host.ClusterInfo().Stats().UpstreamRequestTotal.Inc(1)
	}

	return c, reason
}

func (p *connpool) Close() {
	p.clientMux.Lock()
	defer p.clientMux.Unlock()

	for _, clients := range p.idleClients {
		for _, c := range clients {
			// c.codecClient.Close()
			c.host.Connection.Close(api.NoFlush, api.LocalClose)
		}
	}
}

func (p *connpool) Shutdown() {
	//TODO: http2 connpool do nothing for shutdown ?
	if p.protocol == protocol.HTTP2 {
		return
	}

	p.clientMux.Lock()
	defer p.clientMux.Unlock()

	for _, clients := range p.idleClients {
		for _, c := range clients {
			c.OnGoAway()
			if c.keepAlive != nil {
				c.keepAlive.keepAlive.Stop()
			}
		}
	}
}

// return client to pool
func (p *connpool) putClientToPool(client *activeClient) {
	subProto := client.subProtocol
	if p.shouldMultiplex(subProto) {
		// do nothing
		return
	}

	p.clientMux.Lock()
	defer p.clientMux.Unlock()

	if !client.closed {
		p.idleClients[subProto] = append(p.idleClients[subProto], client)
	}
}

// TODO
/*
type activeCodecClient struct {
	codecClient types.StreamClient
	activeClient *activeClient
}
 */

// types.StreamEventListener
// types.ConnectionEventListener
// types.StreamConnectionEventListener
type activeClient struct {
	pool               *connpool
	// close connid 都可以从 host 中获取
	codecClient        types.StreamClient
	host               types.CreateConnectionData
	totalStream        uint64
	closeWithActiveReq bool

	// -----http1 start, for ping pong mode
	closed          bool
	shouldCloseConn bool
	// -----http1 end

	// -----http2 start
	goaway uint32
	// -----http2 end

	// -----xprotocol start
	subProtocol types.ProtocolName
	keepAlive   *keepAliveListener
	state       uint32 // for async connection
	// -----xprotocol end
}

func (p *connpool) newActiveClient(ctx context.Context, subProtocol api.Protocol) (*activeClient, types.PoolFailureReason) {
	ac := &activeClient{
		pool:        p,
		subProtocol: subProtocol,
		host:        p.Host().CreateConnection(ctx),
	}

	host := p.Host()
	connCtx := ctx

	if p.shouldMultiplex(subProtocol) {
		connCtx = mosnctx.WithValue(ctx, types.ContextKeyConnectionID, ac.host.Connection.ID())
	}

	if len(subProtocol) > 0 {
		connCtx = mosnctx.WithValue(ctx, types.ContextSubProtocol, string(subProtocol))
	}

	codecClient := stream.NewStreamClient(connCtx, p.protocol, ac.host.Connection, host)
	// this should be equal to
	// ac.host.Connection.AddConnectionEventListener(ac)
	codecClient.AddConnectionEventListener(ac) // ac.OnEvent

	codecClient.SetStreamConnectionEventListener(ac) // ac.OnGoAway

	ac.codecClient = codecClient

	if subProtocol != "" {
		// Add Keep Alive
		// protocol is from onNewDetectStream
		// check heartbeat enable, hack: judge trigger result of Heartbeater
		proto := xprotocol.GetProtocol(subProtocol)
		if heartbeater, ok := proto.(xprotocol.Heartbeater); ok && heartbeater.Trigger(0) != nil {
			// create keepalive
			rpcKeepAlive := NewKeepAlive(codecClient, subProtocol, time.Second, 6)
			rpcKeepAlive.StartIdleTimeout()
			ac.keepAlive = &keepAliveListener{
				keepAlive: rpcKeepAlive,
			}
			// this should be equal to
			// ac.host.Connection.AddConnectionEventListener(ac.keepAlive)
			ac.codecClient.AddConnectionEventListener(ac.keepAlive)
		}
	}

	if err := ac.host.Connection.Connect(); err != nil {
		return nil, types.ConnectionFailure
	}

	atomic.StoreUint32(&ac.state, Connected)

	// stats
	host.HostStats().UpstreamConnectionTotal.Inc(1)
	host.HostStats().UpstreamConnectionActive.Inc(1)
	host.ClusterInfo().Stats().UpstreamConnectionTotal.Inc(1)
	host.ClusterInfo().Stats().UpstreamConnectionActive.Inc(1)

	// bytes total adds all connections data together
	codecClient.SetConnectionCollector(host.ClusterInfo().Stats().UpstreamBytesReadTotal, host.ClusterInfo().Stats().UpstreamBytesWriteTotal)
	return ac, ""
}

// Close return this client back to pool
func (ac *activeClient) Close(err error) {
	if err != nil {
		ac.removeFromPool()
		return
	}

	if !ac.pool.shouldMultiplex(ac.subProtocol) && (!ac.closed && ac.shouldCloseConn) {
		// HTTP1 && xprotocol ping pong
		// xprotocol may also use ping pong now, so we need to close the conn
		// ac.codecClient.Close()
		ac.host.Connection.Close(api.NoFlush, api.LocalClose)
	}

	// return to pool
	ac.pool.putClientToPool(ac)
}

// removeFromPool removes this client from connection pool
func (ac *activeClient) removeFromPool() {
	p := ac.pool
	subProtocol := ac.subProtocol
	p.clientMux.Lock()

	defer p.clientMux.Unlock()
	p.totalClientCount--
	for idx, c := range p.idleClients[subProtocol] {
		if c == ac {
			// remove this element
			lastIdx := len(p.idleClients[subProtocol]) - 1
			// 	1. swap this with the last
			p.idleClients[subProtocol][idx], p.idleClients[subProtocol][lastIdx] =
				p.idleClients[subProtocol][lastIdx], p.idleClients[subProtocol][idx]
			// 	2. set last to nil
			p.idleClients[subProtocol][lastIdx] = nil
			// 	3. remove the last
			p.idleClients[subProtocol] = p.idleClients[subProtocol][:lastIdx]
		}
	}
	ac.closed = true
}

func (ac *activeClient) StreamClient() types.StreamClient {
	return ac.codecClient
}

// types.ConnectionEventListener
func (ac *activeClient) OnEvent(event api.ConnectionEvent) {
	p := ac.pool
	if p.protocol == protocol.HTTP2 {
		log.DefaultLogger.Debugf("http2 connpool onConnectionEvent: %v", event)
	}

	host := p.Host()
	// all protocol should report the following metrics
	if ac.closeWithActiveReq {
		if event == api.LocalClose {
			host.HostStats().UpstreamConnectionLocalCloseWithActiveRequest.Inc(1)
			host.ClusterInfo().Stats().UpstreamConnectionLocalCloseWithActiveRequest.Inc(1)
		} else if event == api.RemoteClose {
			host.HostStats().UpstreamConnectionRemoteCloseWithActiveRequest.Inc(1)
			host.ClusterInfo().Stats().UpstreamConnectionRemoteCloseWithActiveRequest.Inc(1)
		}
	}

	switch {
	case event.IsClose():
		if p.protocol == protocol.Xprotocol {
			host.HostStats().UpstreamConnectionClose.Inc(1)
			host.HostStats().UpstreamConnectionActive.Dec(1)
			host.ClusterInfo().Stats().UpstreamConnectionClose.Inc(1)
			host.ClusterInfo().Stats().UpstreamConnectionActive.Dec(1)

			switch event {
			case api.LocalClose:
				host.HostStats().UpstreamConnectionLocalClose.Inc(1)
				host.ClusterInfo().Stats().UpstreamConnectionLocalClose.Inc(1)
			case api.RemoteClose:
				host.HostStats().UpstreamConnectionRemoteClose.Inc(1)
				host.ClusterInfo().Stats().UpstreamConnectionRemoteClose.Inc(1)
			}
		}
		ac.removeFromPool()

	case event == api.ConnectTimeout:
		host.HostStats().UpstreamRequestTimeout.Inc(1)
		host.ClusterInfo().Stats().UpstreamRequestTimeout.Inc(1)
		// ac.codecClient.Close()
		ac.host.Connection.Close(api.NoFlush, api.LocalClose)
	case event == api.ConnectFailed:
		host.HostStats().UpstreamConnectionConFail.Inc(1)
		host.ClusterInfo().Stats().UpstreamConnectionConFail.Inc(1)
	}
}

// types.StreamEventListener
func (ac *activeClient) OnDestroyStream() {
	host := ac.pool.Host()
	host.HostStats().UpstreamRequestActive.Dec(1)
	host.ClusterInfo().Stats().UpstreamRequestActive.Dec(1)
	host.ClusterInfo().ResourceManager().Requests().Decrease()

	ac.Close(nil)
}

func (ac *activeClient) OnResetStream(reason types.StreamResetReason) {
	host := ac.pool.Host()
	switch reason {
	case types.StreamConnectionTermination, types.StreamConnectionFailed:
		host.HostStats().UpstreamRequestFailureEject.Inc(1)
		host.ClusterInfo().Stats().UpstreamRequestFailureEject.Inc(1)
		ac.closeWithActiveReq = true
	case types.StreamLocalReset:
		host.HostStats().UpstreamRequestLocalReset.Inc(1)
		host.ClusterInfo().Stats().UpstreamRequestLocalReset.Inc(1)
	case types.StreamRemoteReset:
		host.HostStats().UpstreamRequestRemoteReset.Inc(1)
		host.ClusterInfo().Stats().UpstreamRequestRemoteReset.Inc(1)
	}

	if !ac.pool.shouldMultiplex(ac.subProtocol) && reason == types.StreamLocalReset && !ac.closed {
		// for http1 && xprotocol ping pong
		// ac.codecClient.ConnID() = ac.host.Connection.ID()
		log.DefaultLogger.Debugf("[stream] [http] stream local reset, blow codecClient away also, Connection = %d",
			ac.host.Connection.ID())
		//log.DefaultLogger.Debugf("[stream] [http] stream local reset, blow codecClient away also, Connection = %d",
		//	ac.codecClient.ConnID())
		ac.shouldCloseConn = true
	}
}

// types.StreamConnectionEventListener
func (ac *activeClient) OnGoAway() {
	if ac.pool.shouldMultiplex(ac.subProtocol) {
		atomic.StoreUint32(&ac.goaway, 1)
	} else {
		ac.shouldCloseConn = true
	}
}

func getSubProtocol(ctx context.Context) types.ProtocolName {
	if ctx != nil {
		if val := mosnctx.Get(ctx, types.ContextSubProtocol); val != nil {
			if code, ok := val.(string); ok {
				return types.ProtocolName(code)
			}
		}
	}
	return ""
}

// ----------xprotocol only
// keepAliveListener is a types.ConnectionEventListener
type keepAliveListener struct {
	keepAlive types.KeepAlive
}

func (l *keepAliveListener) OnEvent(event api.ConnectionEvent) {
	if event == api.OnReadTimeout {
		l.keepAlive.SendKeepAlive()
	}
}

// ----------xprotocol only
