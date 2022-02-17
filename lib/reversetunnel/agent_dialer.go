/*
Copyright 2022 Gravitational, Inc.

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

	"github.com/gravitational/teleport/api/client/webclient"
	apidefaults "github.com/gravitational/teleport/api/defaults"
	"github.com/gravitational/teleport/api/types"
	apisshutils "github.com/gravitational/teleport/api/utils/sshutils"
	"github.com/gravitational/teleport/lib"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/sshutils"
	"github.com/gravitational/teleport/lib/utils"
	"github.com/gravitational/teleport/lib/utils/proxy"
	"github.com/gravitational/trace"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

// agentDialer dials an ssh server on behalf of an agent.
type agentDialer struct {
	client      auth.AccessCache
	username    string
	authMethods []ssh.AuthMethod
	fips        bool
	log         logrus.FieldLogger
}

// DialContext creates an ssh connection to the given address.
func (d *agentDialer) DialContext(ctx context.Context, addr utils.NetAddr) (SSHClient, error) {
	proxyOptions := make([]proxy.DialerOptionFunc, 0)

	pong, err := webclient.Find(ctx, addr.String(), lib.IsInsecureDevMode(), nil)
	if err == nil && pong.Proxy.TLSRoutingEnabled {
		proxyOptions = append(proxyOptions, proxy.WithALPNDialer())
	}

	for _, authMethod := range d.authMethods {
		// Create a dialer (that respects HTTP proxies) and connect to remote host.
		dialer := proxy.DialerFromEnvironment(addr.Addr, proxyOptions...)
		pconn, err := dialer.DialTimeout(addr.AddrNetwork, addr.Addr, apidefaults.DefaultDialTimeout)
		if err != nil {
			d.log.WithError(err).Debugf("Failed to dial %s.", addr.Addr)
			continue
		}

		principals := make([]string, 0)
		callback, err := apisshutils.NewHostKeyCallback(
			apisshutils.HostKeyCallbackConfig{
				GetHostCheckers: d.getHostCheckers,
				OnCheckCert: func(c *ssh.Certificate) {
					principals = c.ValidPrincipals
				},
				FIPS: d.fips,
			})
		if err != nil {
			d.log.Debugf("Failed to create host key callback for %v: %v.", addr.Addr, err)
			continue
		}

		// Build a new client connection. This is done to get access to incoming
		// global requests which dialer.Dial would not provide.
		conn, chans, reqs, err := ssh.NewClientConn(pconn, addr.Addr, &ssh.ClientConfig{
			User:            d.username,
			Auth:            []ssh.AuthMethod{authMethod},
			HostKeyCallback: callback,
			Timeout:         apidefaults.DefaultDialTimeout,
		})
		if err != nil {
			d.log.WithError(err).Debugf("Failed to create client to %v.", addr.Addr)
			continue
		}

		emptyRequests := make(chan *ssh.Request)
		close(emptyRequests)

		client := ssh.NewClient(conn, chans, emptyRequests)

		return &sshClient{
			Client:      client,
			requests:    reqs,
			newChannels: chans,
			principals:  principals,
		}, nil
	}

	return nil, trace.BadParameter("failed to dial: all auth methods failed")
}

// getHostCheckers fetches the CA public keys.
func (d *agentDialer) getHostCheckers() ([]ssh.PublicKey, error) {
	cas, err := d.client.GetCertAuthorities(types.HostCA, false)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	var keys []ssh.PublicKey
	for _, ca := range cas {
		checkers, err := sshutils.GetCheckers(ca)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		keys = append(keys, checkers...)
	}
	return keys, nil
}

type SSHClient interface {
	ssh.Conn
	Principals() []string
	GlobalRequests() <-chan *ssh.Request
	HandleChannelOpen(channelType string) <-chan ssh.NewChannel
	Reply(*ssh.Request, bool, []byte) error
}

type sshClient struct {
	*ssh.Client
	requests    <-chan *ssh.Request
	newChannels <-chan ssh.NewChannel
	principals  []string
}

func (c *sshClient) NewChannels() <-chan ssh.NewChannel {
	return c.newChannels
}

func (c *sshClient) GlobalRequests() <-chan *ssh.Request {
	return c.requests
}

func (c *sshClient) Principals() []string {
	return c.principals
}

func (c *sshClient) Reply(request *ssh.Request, ok bool, payload []byte) error {
	return request.Reply(ok, payload)
}
