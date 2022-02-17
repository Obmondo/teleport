/*
Copyright 2015-2019 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package reversetunnel

import (
	"context"
	"io"
	"net"
	"sync"
	"time"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/reversetunnel/track"
	"github.com/gravitational/teleport/lib/utils"

	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

// ServerHandler implements an interface which can handle a connection
// (perform a handshake then process). This is needed because importing
// lib/srv in lib/reversetunnel causes a circular import.
type ServerHandler interface {
	// HandleConnection performs a handshake then process the connection.
	HandleConnection(conn net.Conn)
}

// AgentPool manages a pool of reverse tunnel agents.
type AgentPool struct {
	AgentPoolConfig
	active  *agentStore
	tracker *track.Tracker

	// events receives agent state change events.
	events              chan *Agent
	proxyPeeringEnabled bool

	// wg waits for the pool and all agents to complete.
	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc

	// backoff limits the rate at which new agents are created.
	backoff utils.Retry
	log     logrus.FieldLogger
}

// AgentPoolConfig holds configuration parameters for the agent pool
type AgentPoolConfig struct {
	// Client is client to the auth server this agent connects to receive
	// a list of pools
	Client auth.ClientI
	// AccessPoint is a lightweight access point
	// that can optionally cache some values
	AccessPoint auth.AccessCache
	// HostSigner is a host signer this agent presents itself as
	HostSigner ssh.Signer
	// HostUUID is a unique ID of this host
	HostUUID string
	// LocalCluster is a cluster name this client is a member of.
	LocalCluster string
	// Clock is a clock used to get time, if not set,
	// system clock is used
	Clock clockwork.Clock
	// KubeDialAddr is an address of a kubernetes proxy
	KubeDialAddr utils.NetAddr
	// Server is either an SSH or application server. It can handle a connection
	// (perform handshake and handle request).
	Server ServerHandler
	// Component is the Teleport component this agent pool is running in. It can
	// either be proxy (trusted clusters) or node (dial back).
	Component string
	// ReverseTunnelServer holds all reverse tunnel connections.
	ReverseTunnelServer Server
	// Resolver retrieves the reverse tunnel address
	Resolver Resolver
	// Cluster is a cluster name of the proxy.
	Cluster string
	// FIPS indicates if Teleport was started in FIPS mode.
	FIPS bool
	// StateCallback is called for each agent event. This is an optional
	// parameter that is currently only used for testing.
	StateCallback AgentStateCallback
	// ConnectedProxies signals which proxy an agent is connected to. This is
	// only relevant when ProxyPeering is enabled for the cluster.
	ConnectedProxies *ConnectedProxies
}

// CheckAndSetDefaults checks and sets defaults
func (cfg *AgentPoolConfig) CheckAndSetDefaults() error {
	if cfg.Client == nil {
		return trace.BadParameter("missing 'Client' parameter")
	}
	if cfg.AccessPoint == nil {
		return trace.BadParameter("missing 'AccessPoint' parameter")
	}
	if cfg.HostSigner == nil {
		return trace.BadParameter("missing 'HostSigner' parameter")
	}
	if len(cfg.HostUUID) == 0 {
		return trace.BadParameter("missing 'HostUUID' parameter")
	}
	if cfg.Cluster == "" {
		return trace.BadParameter("missing 'Cluster' parameter")
	}
	if cfg.ConnectedProxies == nil {
		cfg.ConnectedProxies = NewConnectedProxies()
	}
	if cfg.Clock == nil {
		cfg.Clock = clockwork.NewRealClock()
	}
	return nil
}

// NewAgentPool returns new instance of the agent pool
func NewAgentPool(ctx context.Context, config AgentPoolConfig) (*AgentPool, error) {
	if err := config.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}
	retry, err := utils.NewLinear(utils.LinearConfig{
		Step:      time.Second,
		Max:       time.Second * 8,
		Jitter:    utils.NewJitter(),
		AutoReset: 4,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	pool := &AgentPool{
		AgentPoolConfig: config,
		active:          newAgentStore(),
		events:          make(chan *Agent),
		wg:              sync.WaitGroup{},
		backoff:         retry,
		log: logrus.WithFields(logrus.Fields{
			trace.Component: teleport.ComponentReverseTunnelAgent,
			trace.ComponentFields: logrus.Fields{
				"cluster": config.Cluster,
			},
		}),
	}

	pool.ctx, pool.cancel = context.WithCancel(ctx)
	pool.tracker, err = track.New(pool.ctx, track.Config{ClusterName: pool.Cluster})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	pool.tracker.Start()

	return pool, nil
}

func (p *AgentPool) ConnectedProxies() *ConnectedProxies {
	return p.AgentPoolConfig.ConnectedProxies
}

func (p *AgentPool) updateConnectedProxies() {
	if !p.proxyPeeringEnabled {
		p.AgentPoolConfig.ConnectedProxies.updateProxyIDs([]string{})
		return
	}

	agent, ok := p.active.last()
	if !ok {
		return
	}

	proxyID, ok := getIDFromPrincipals(agent.client.Principals())
	if !ok {
		p.log.Warningf("Unable to get proxy ID from principals %v", agent.client.Principals())
	}

	p.log.Debugf("Updating connected proxy: %s", proxyID)

	p.AgentPoolConfig.ConnectedProxies.updateProxyIDs([]string{proxyID})
}

func (p *AgentPool) Count() int {
	return p.active.len()
}

// Start starts the agent pool in the background.
func (p *AgentPool) Start() error {
	p.log.Debugf("Starting agent pool %s.%s...", p.HostUUID, p.Cluster)
	p.tracker.Start()

	p.wg.Add(1)
	go func() {
		err := p.start()
		p.log.WithError(err).Error("Agent pool stopped.")

		p.cancel()
		p.wg.Done()
	}()
	return nil
}

func (p *AgentPool) start() error {
	for {
		if p.ctx.Err() != nil {
			return trace.Wrap(p.ctx.Err())
		}

		err := p.handle(p.ctx, p.tracker.Acquire(), p.events)
		if err != nil {
			p.log.WithError(err).Debugf("Unexpected agent pool error.")
		}
		p.backoff.Inc()

		err = p.waitForBackoff(p.ctx, p.events)
		if err != nil {
			p.log.WithError(err).Debugf("Unexpected agent pool error.")
		}
	}
}

// handle is a single iteration of the agent pool loop. It manages backoff,
// lease acquisition, event processing, and agent connections.
func (p *AgentPool) handle(ctx context.Context, leases <-chan track.Lease, events <-chan *Agent) error {
	var agent *Agent

	lease, err := p.waitForLease(ctx, leases, events)
	if err != nil {
		return trace.Wrap(err)
	}

	// Wrap in closure so we can release the lease on error in one place.
	err = func() error {
		err = p.processEvents(ctx, events)
		if err != nil {
			return trace.Wrap(err)
		}

		agent, err = p.newAgent(ctx, p.tracker, lease)
		if err != nil {
			return trace.Wrap(err)
		}

		err = agent.Start(ctx)
		if err != nil {
			return trace.Wrap(err)
		}

		return nil
	}()
	if err != nil {
		lease.Release()
		return trace.Wrap(err)
	}

	p.wg.Add(1)
	p.active.add(agent)
	p.updateConnectedProxies()

	return nil
}

// processEvents handles all events in the queue. Unblocking when a new agent
// is required.
func (p *AgentPool) processEvents(ctx context.Context, events <-chan *Agent) error {
	// Unblock after processing any queued up events.
	err := func() error {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case agent := <-events:
				p.handleEvent(ctx, agent)
			default:
				return nil
			}
		}
	}()
	if err != nil {
		return trace.Wrap(err)
	}

	if p.isAgentRequired() {
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case agent := <-events:
			p.handleEvent(ctx, agent)

			if p.isAgentRequired() {
				return nil
			}
		}
	}
}

// isAgentRequired returns true if a new agent is required.
func (p *AgentPool) isAgentRequired() bool {
	if err := p.updateSettings(); err != nil {
		p.log.WithError(err).Warningf("Failed to update agent pool settings.")
	}

	p.log.Debugf("Proxy peering enabled: %v", p.proxyPeeringEnabled)

	// An agent is always required when proxy peering is not enabled.
	if !p.proxyPeeringEnabled {
		return true
	}

	p.disconnectAgents()

	return p.active.len() < 1
}

func (p *AgentPool) updateSettings() error {
	config, err := p.AccessPoint.GetClusterNetworkingConfig(p.ctx)
	if err != nil {
		return trace.Wrap(err)
	}

	if config.GetProxyPeering() == types.ProxyPeering_Enabled {
		p.proxyPeeringEnabled = true
		return nil
	}

	p.proxyPeeringEnabled = false
	return nil
}

// disconnectAgents handles disconnecting agents that are no longer required.
func (p *AgentPool) disconnectAgents() {
	for {
		agent, ok := p.active.poplen(1)
		if !ok {
			return
		}

		p.log.Debugf("Disconnecting agent %s.", agent)
		go agent.Stop()
	}
}

// waitForLease processes events while waiting to acquire a lease.
func (p *AgentPool) waitForLease(ctx context.Context, leases <-chan track.Lease, events <-chan *Agent) (track.Lease, error) {
	for {
		select {
		case <-ctx.Done():
			return track.Lease{}, ctx.Err()
		case lease := <-leases:
			return lease, nil
		case agent := <-events:
			p.handleEvent(ctx, agent)
		}
	}
}

// waitForBackoff processes events while waiting for the backoff.
func (p *AgentPool) waitForBackoff(ctx context.Context, events <-chan *Agent) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.backoff.After():
			return nil
		case agent := <-events:
			p.handleEvent(ctx, agent)
		}
	}
}

// handleEvent processes a single event.
func (p *AgentPool) handleEvent(ctx context.Context, agent *Agent) {
	state := agent.GetState()
	switch state {
	case AgentConnected:
	case AgentClosed:
		if ok := p.active.remove(agent); ok {
			p.wg.Done()
		}
	}

	p.log.Debugf("Active agent count: %d", p.active.len())
}

// stateCallback adds events to the queue for each agent state change.
func (p *AgentPool) stateCallback(agent *Agent) {
	if p.StateCallback != nil {
		go p.StateCallback(agent)
	}
	select {
	case <-p.ctx.Done():
		// Handle events directly when the pool is closing.
		p.handleEvent(p.ctx, agent)
	case p.events <- agent:
	}
}

// newAgent creates a new newAgent instance.
func (p *AgentPool) newAgent(ctx context.Context, tracker *track.Tracker, lease track.Lease) (*Agent, error) {
	netConfig, err := p.AccessPoint.GetClusterNetworkingConfig(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	addr, err := p.Resolver()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	dialer := &agentDialer{
		client:      p.Client,
		fips:        p.FIPS,
		authMethods: []ssh.AuthMethod{ssh.PublicKeys(p.HostSigner)},
		username:    p.HostUUID,
		log:         p.log,
	}

	agent, err := NewAgent(&agentConfig{
		addr:          *addr,
		keepAlive:     netConfig.GetKeepAliveInterval(),
		stateCallback: p.stateCallback,
		sshDialer:     dialer,
		transporter:   p,
		tracker:       tracker,
		lease:         lease,
		clock:         p.Clock,
		log:           p.log,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return agent, nil
}

// Wait blocks until the pool context is done.
func (p *AgentPool) Wait() {
	p.wg.Wait()
}

// Stop stops the pool and waits for all resources to be released.
func (p *AgentPool) Stop() {
	p.cancel()
	p.wg.Wait()
}

// transport creates a new transport instance.
func (p *AgentPool) transport(ctx context.Context, channel ssh.Channel, requests <-chan *ssh.Request, conn ssh.Conn) *transport {
	return &transport{
		closeContext:        ctx,
		component:           p.Component,
		localClusterName:    p.LocalCluster,
		kubeDialAddr:        p.KubeDialAddr,
		authClient:          p.Client,
		reverseTunnelServer: p.ReverseTunnelServer,
		server:              p.Server,
		emitter:             p.Client,
		sconn:               conn,
		channel:             channel,
		requestCh:           requests,
		log:                 p.log,
	}
}

// Make sure ServerHandlerToListener implements both interfaces.
var _ = net.Listener(ServerHandlerToListener{})
var _ = ServerHandler(ServerHandlerToListener{})

// ServerHandlerToListener is an adapter from ServerHandler to net.Listener. It
// can be used as a Server field in AgentPoolConfig, while also being passed to
// http.Server.Serve (or any other func Serve(net.Listener)).
type ServerHandlerToListener struct {
	connCh     chan net.Conn
	closeOnce  *sync.Once
	tunnelAddr string
}

// NewServerHandlerToListener creates a new ServerHandlerToListener adapter.
func NewServerHandlerToListener(tunnelAddr string) ServerHandlerToListener {
	return ServerHandlerToListener{
		connCh:     make(chan net.Conn),
		closeOnce:  new(sync.Once),
		tunnelAddr: tunnelAddr,
	}
}

func (l ServerHandlerToListener) HandleConnection(c net.Conn) {
	// HandleConnection must block as long as c is used.
	// Wrap c to only return after c.Close() has been called.
	cc := newConnCloser(c)
	l.connCh <- cc
	cc.wait()
}

func (l ServerHandlerToListener) Accept() (net.Conn, error) {
	c, ok := <-l.connCh
	if !ok {
		return nil, io.EOF
	}
	return c, nil
}

func (l ServerHandlerToListener) Close() error {
	l.closeOnce.Do(func() { close(l.connCh) })
	return nil
}

func (l ServerHandlerToListener) Addr() net.Addr {
	return reverseTunnelAddr(l.tunnelAddr)
}

type connCloser struct {
	net.Conn
	closeOnce *sync.Once
	closed    chan struct{}
}

func newConnCloser(c net.Conn) connCloser {
	return connCloser{Conn: c, closeOnce: new(sync.Once), closed: make(chan struct{})}
}

func (c connCloser) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func (c connCloser) wait() { <-c.closed }

// reverseTunnelAddr is a net.Addr implementation for a listener based on a
// reverse tunnel.
type reverseTunnelAddr string

func (reverseTunnelAddr) Network() string  { return "ssh-reversetunnel" }
func (a reverseTunnelAddr) String() string { return string(a) }
