/*
 *
 * Copyright 2014 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package grpc

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/balancer"
	_ "google.golang.org/grpc/balancer/roundrobin" // To register roundrobin.
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/internal/backoff"
	"google.golang.org/grpc/internal/channelz"
	"google.golang.org/grpc/internal/envconfig"
	"google.golang.org/grpc/internal/grpcsync"
	"google.golang.org/grpc/internal/transport"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/resolver"
	_ "google.golang.org/grpc/resolver/dns"         // To register dns resolver.
	_ "google.golang.org/grpc/resolver/passthrough" // To register passthrough resolver.
	"google.golang.org/grpc/serviceconfig"
	"google.golang.org/grpc/status"
)

const (
	// minimum time to give a connection to complete
	minConnectTimeout = 20 * time.Second
	// must match grpclbName in grpclb/grpclb.go
	grpclbName = "grpclb"
)

var (
	// ErrClientConnClosing indicates that the operation is illegal because
	// the ClientConn is closing.
	//
	// Deprecated: this error should not be relied upon by users; use the status
	// code of Canceled instead.
	ErrClientConnClosing = status.Error(codes.Canceled, "grpc: the client connection is closing")
	// errConnDrain indicates that the connection starts to be drained and does not accept any new RPCs.
	errConnDrain = errors.New("grpc: the connection is drained")
	// errConnClosing indicates that the connection is closing.
	errConnClosing = errors.New("grpc: the connection is closing")
	// errBalancerClosed indicates that the balancer is closed.
	errBalancerClosed = errors.New("grpc: balancer is closed")
	// invalidDefaultServiceConfigErrPrefix is used to prefix the json parsing error for the default
	// service config.
	invalidDefaultServiceConfigErrPrefix = "grpc: the provided default service config is invalid"
)

// The following errors are returned from Dial and DialContext
var (
	// errNoTransportSecurity indicates that there is no transport security
	// being set for ClientConn. Users should either set one or explicitly
	// call WithInsecure DialOption to disable security.
	errNoTransportSecurity = errors.New("grpc: no transport security set (use grpc.WithInsecure() explicitly or set credentials)")
	// errTransportCredsAndBundle indicates that creds bundle is used together
	// with other individual Transport Credentials.
	errTransportCredsAndBundle = errors.New("grpc: credentials.Bundle may not be used with individual TransportCredentials")
	// errTransportCredentialsMissing indicates that users want to transmit security
	// information (e.g., OAuth2 token) which requires secure connection on an insecure
	// connection.
	errTransportCredentialsMissing = errors.New("grpc: the credentials require transport level security (use grpc.WithTransportCredentials() to set)")
	// errCredentialsConflict indicates that grpc.WithTransportCredentials()
	// and grpc.WithInsecure() are both called for a connection.
	errCredentialsConflict = errors.New("grpc: transport credentials are set for an insecure connection (grpc.WithTransportCredentials() and grpc.WithInsecure() are both called)")
)

const (
	defaultClientMaxReceiveMessageSize = 1024 * 1024 * 4
	defaultClientMaxSendMessageSize    = math.MaxInt32
	// http2IOBufSize specifies the buffer size for sending frames.
	defaultWriteBufSize = 32 * 1024
	defaultReadBufSize  = 32 * 1024
)

// Dial creates a client connection to the given target.
func Dial(target string, opts ...DialOption) (*ClientConn, error) {
	return DialContext(context.Background(), target, opts...)
}

// DialContext creates a client connection to the given target. By default, it's
// a non-blocking dial (the function won't wait for connections to be
// established, and connecting happens in the background). To make it a blocking
// dial, use WithBlock() dial option.
//
// In the non-blocking case, the ctx does not act against the connection. It
// only controls the setup steps.
//
// In the blocking case, ctx can be used to cancel or expire the pending
// connection. Once this function returns, the cancellation and expiration of
// ctx will be noop. Users should call ClientConn.Close to terminate all the
// pending operations after this function returns.
//
// The target name syntax is defined in
// https://github.com/grpc/grpc/blob/master/doc/naming.md.
// e.g. to use dns resolver, a "dns:///" prefix should be applied to the target.

// 创建ClientConn
func DialContext(ctx context.Context, target string, opts ...DialOption) (conn *ClientConn, err error) {
	cc := &ClientConn{
		target:            target,                       // target
		csMgr:             &connectivityStateManager{},  // 连接状态管理器
		conns:             make(map[*addrConn]struct{}), // 连接
		dopts:             defaultDialOptions(),         // options
		blockingpicker:    newPickerWrapper(),           // balancer.Picker的wrapper
		czData:            new(channelzData),
		firstResolveEvent: grpcsync.NewEvent(),
	}

	// retry
	cc.retryThrottler.Store((*retryThrottler)(nil))
	cc.ctx, cc.cancel = context.WithCancel(context.Background())

	// 应用options
	for _, opt := range opts {
		opt.apply(&cc.dopts)
	}

	// 如果存在多个Interceptors，则chain成一个
	chainUnaryClientInterceptors(cc)
	chainStreamClientInterceptors(cc)

	defer func() {
		if err != nil {
			cc.Close()
		}
	}()

	if channelz.IsOn() {
		if cc.dopts.channelzParentID != 0 {
			cc.channelzID = channelz.RegisterChannel(&channelzChannel{cc}, cc.dopts.channelzParentID, target)
			channelz.AddTraceEvent(cc.channelzID, &channelz.TraceEventDesc{
				Desc:     "Channel Created",
				Severity: channelz.CtINFO,
				Parent: &channelz.TraceEventDesc{
					Desc:     fmt.Sprintf("Nested Channel(id:%d) created", cc.channelzID),
					Severity: channelz.CtINFO,
				},
			})
		} else {
			cc.channelzID = channelz.RegisterChannel(&channelzChannel{cc}, 0, target)
			channelz.AddTraceEvent(cc.channelzID, &channelz.TraceEventDesc{
				Desc:     "Channel Created",
				Severity: channelz.CtINFO,
			})
		}
		cc.csMgr.channelzID = cc.channelzID
	}

	// tls连接加密证书相关检查
	if !cc.dopts.insecure {
		if cc.dopts.copts.TransportCredentials == nil && cc.dopts.copts.CredsBundle == nil {
			return nil, errNoTransportSecurity
		}
		if cc.dopts.copts.TransportCredentials != nil && cc.dopts.copts.CredsBundle != nil {
			return nil, errTransportCredsAndBundle
		}
	} else {
		if cc.dopts.copts.TransportCredentials != nil || cc.dopts.copts.CredsBundle != nil {
			return nil, errCredentialsConflict
		}
		for _, cd := range cc.dopts.copts.PerRPCCredentials {
			if cd.RequireTransportSecurity() {
				return nil, errTransportCredentialsMissing
			}
		}
	}

	// 如果提供了服务配置
	if cc.dopts.defaultServiceConfigRawJSON != nil {
		sc, err := parseServiceConfig(*cc.dopts.defaultServiceConfigRawJSON)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", invalidDefaultServiceConfigErrPrefix, err)
		}
		// 设置默认服务配置
		cc.dopts.defaultServiceConfig = sc
	}

	cc.mkp = cc.dopts.copts.KeepaliveParams

	// 如果没有提供dial，则默认使用ProxyDialer，会根据系统环境变量的代理配置进行网络连接
	if cc.dopts.copts.Dialer == nil {
		cc.dopts.copts.Dialer = newProxyDialer(
			func(ctx context.Context, addr string) (net.Conn, error) {
				network, addr := parseDialTarget(addr)
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		)
	}

	// 用户代理添加grpcUA
	if cc.dopts.copts.UserAgent != "" {
		cc.dopts.copts.UserAgent += " " + grpcUA
	} else {
		cc.dopts.copts.UserAgent = grpcUA
	}

	// 如果options设置了timeout
	if cc.dopts.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cc.dopts.timeout)
		defer cancel()
	}
	defer func() {
		select {
		case <-ctx.Done():
			conn, err = nil, ctx.Err()
		default:
		}
	}()

	scSet := false
	// 如果提供了scChan，支持对serviceConfig进行热更
	if cc.dopts.scChan != nil {
		// Try to get an initial service config.
		select {
		// 尝试获取初始的serviceConfig
		case sc, ok := <-cc.dopts.scChan:
			if ok {
				cc.sc = &sc
				scSet = true // 成功获取初始的serviceConfig
			}
		default:
		}
	}
	// 提供retry时的退避算法
	if cc.dopts.bs == nil {
		cc.dopts.bs = backoff.Exponential{
			MaxDelay: DefaultBackoffConfig.MaxDelay,
		}
	}

	// resolverBuilder，用于解析target为目标服务列表
	// 如果没有指定resolverBuilder
	if cc.dopts.resolverBuilder == nil {
		// Only try to parse target when resolver builder is not already set.
		// 解析target，根据target的scheme获取对应的resolver
		cc.parsedTarget = parseTarget(cc.target)
		grpclog.Infof("parsed scheme: %q", cc.parsedTarget.Scheme)
		cc.dopts.resolverBuilder = resolver.Get(cc.parsedTarget.Scheme)
		// 如果没有的话，使用默认的resolver
		if cc.dopts.resolverBuilder == nil {
			// If resolver builder is still nil, the parsed target's scheme is
			// not registered. Fallback to default resolver and set Endpoint to
			// the original target.
			grpclog.Infof("scheme %q not registered, fallback to default scheme", cc.parsedTarget.Scheme)
			cc.parsedTarget = resolver.Target{
				Scheme:   resolver.GetDefaultScheme(),
				Endpoint: target,
			}
			cc.dopts.resolverBuilder = resolver.Get(cc.parsedTarget.Scheme)
		}
	} else {
		cc.parsedTarget = resolver.Target{Endpoint: target}
	}

	// 连接证书
	creds := cc.dopts.copts.TransportCredentials
	if creds != nil && creds.Info().ServerName != "" {
		cc.authority = creds.Info().ServerName
	} else if cc.dopts.insecure && cc.dopts.authority != "" {
		cc.authority = cc.dopts.authority
	} else {
		// Use endpoint from "scheme://authority/endpoint" as the default
		// authority for ClientConn.
		cc.authority = cc.parsedTarget.Endpoint
	}

	// 如果提供了scChan但是还没有获取到初始的serviceConfig，则阻塞等待serviceConfig
	if cc.dopts.scChan != nil && !scSet {
		// Blocking wait for the initial service config.
		select {
		case sc, ok := <-cc.dopts.scChan:
			if ok {
				cc.sc = &sc
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	// 启动子协程，监听scChan，进行serviceConfig的热更
	if cc.dopts.scChan != nil {
		go cc.scWatcher()
	}

	var credsClone credentials.TransportCredentials
	if creds := cc.dopts.copts.TransportCredentials; creds != nil {
		credsClone = creds.Clone()
	}
	// balancerBuild的options
	cc.balancerBuildOpts = balancer.BuildOptions{
		DialCreds:        credsClone,
		CredsBundle:      cc.dopts.copts.CredsBundle,
		Dialer:           cc.dopts.copts.Dialer,
		ChannelzParentID: cc.channelzID,
		Target:           cc.parsedTarget,
	}

	// Build the resolver.
	// 创建resovler，并包装成resolverWrapper
	rWrapper, err := newCCResolverWrapper(cc)
	if err != nil {
		return nil, fmt.Errorf("failed to build resolver: %v", err)
	}

	cc.mu.Lock()
	cc.resolverWrapper = rWrapper
	cc.mu.Unlock()
	// A blocking dial blocks until the clientConn is ready.
	// 默认Dial不会等待网络连接完成，如果指定了blcok，则会阻塞等待网络连接完成才返回
	if cc.dopts.block {
		for {
			s := cc.GetState()
			// 如果已经Ready
			if s == connectivity.Ready {
				break
			} else if cc.dopts.copts.FailOnNonTempDialError && s == connectivity.TransientFailure {
				if err = cc.blockingpicker.connectionError(); err != nil {
					terr, ok := err.(interface {
						Temporary() bool
					})
					if ok && !terr.Temporary() {
						return nil, err
					}
				}
			}
			// 等待状态变更
			if !cc.WaitForStateChange(ctx, s) {
				// ctx got timeout or canceled.
				return nil, ctx.Err()
			}
		}
	}

	return cc, nil
}

// chainUnaryClientInterceptors chains all unary client interceptors into one.
func chainUnaryClientInterceptors(cc *ClientConn) {
	interceptors := cc.dopts.chainUnaryInts
	// Prepend dopts.unaryInt to the chaining interceptors if it exists, since unaryInt will
	// be executed before any other chained interceptors.
	if cc.dopts.unaryInt != nil {
		interceptors = append([]UnaryClientInterceptor{cc.dopts.unaryInt}, interceptors...)
	}
	var chainedInt UnaryClientInterceptor
	if len(interceptors) == 0 {
		chainedInt = nil
	} else if len(interceptors) == 1 {
		chainedInt = interceptors[0]
	} else {
		chainedInt = func(ctx context.Context, method string, req, reply interface{}, cc *ClientConn, invoker UnaryInvoker, opts ...CallOption) error {
			return interceptors[0](ctx, method, req, reply, cc, getChainUnaryInvoker(interceptors, 0, invoker), opts...)
		}
	}
	cc.dopts.unaryInt = chainedInt
}

// getChainUnaryInvoker recursively generate the chained unary invoker.
func getChainUnaryInvoker(interceptors []UnaryClientInterceptor, curr int, finalInvoker UnaryInvoker) UnaryInvoker {
	if curr == len(interceptors)-1 {
		return finalInvoker
	}
	return func(ctx context.Context, method string, req, reply interface{}, cc *ClientConn, opts ...CallOption) error {
		return interceptors[curr+1](ctx, method, req, reply, cc, getChainUnaryInvoker(interceptors, curr+1, finalInvoker), opts...)
	}
}

// chainStreamClientInterceptors chains all stream client interceptors into one.
func chainStreamClientInterceptors(cc *ClientConn) {
	interceptors := cc.dopts.chainStreamInts
	// Prepend dopts.streamInt to the chaining interceptors if it exists, since streamInt will
	// be executed before any other chained interceptors.
	if cc.dopts.streamInt != nil {
		interceptors = append([]StreamClientInterceptor{cc.dopts.streamInt}, interceptors...)
	}
	var chainedInt StreamClientInterceptor
	if len(interceptors) == 0 {
		chainedInt = nil
	} else if len(interceptors) == 1 {
		chainedInt = interceptors[0]
	} else {
		chainedInt = func(ctx context.Context, desc *StreamDesc, cc *ClientConn, method string, streamer Streamer, opts ...CallOption) (ClientStream, error) {
			return interceptors[0](ctx, desc, cc, method, getChainStreamer(interceptors, 0, streamer), opts...)
		}
	}
	cc.dopts.streamInt = chainedInt
}

// getChainStreamer recursively generate the chained client stream constructor.
func getChainStreamer(interceptors []StreamClientInterceptor, curr int, finalStreamer Streamer) Streamer {
	if curr == len(interceptors)-1 {
		return finalStreamer
	}
	return func(ctx context.Context, desc *StreamDesc, cc *ClientConn, method string, opts ...CallOption) (ClientStream, error) {
		return interceptors[curr+1](ctx, desc, cc, method, getChainStreamer(interceptors, curr+1, finalStreamer), opts...)
	}
}

// connectivityStateManager keeps the connectivity.State of ClientConn.
// This struct will eventually be exported so the balancers can access it.
type connectivityStateManager struct {
	mu         sync.Mutex
	state      connectivity.State
	notifyChan chan struct{}
	channelzID int64
}

// updateState updates the connectivity.State of ClientConn.
// If there's a change it notifies goroutines waiting on state change to
// happen.
func (csm *connectivityStateManager) updateState(state connectivity.State) {
	csm.mu.Lock()
	defer csm.mu.Unlock()
	if csm.state == connectivity.Shutdown {
		return
	}
	if csm.state == state {
		return
	}
	csm.state = state
	if channelz.IsOn() {
		channelz.AddTraceEvent(csm.channelzID, &channelz.TraceEventDesc{
			Desc:     fmt.Sprintf("Channel Connectivity change to %v", state),
			Severity: channelz.CtINFO,
		})
	}
	if csm.notifyChan != nil {
		// There are other goroutines waiting on this channel.
		close(csm.notifyChan)
		csm.notifyChan = nil
	}
}

func (csm *connectivityStateManager) getState() connectivity.State {
	csm.mu.Lock()
	defer csm.mu.Unlock()
	return csm.state
}

func (csm *connectivityStateManager) getNotifyChan() <-chan struct{} {
	csm.mu.Lock()
	defer csm.mu.Unlock()
	if csm.notifyChan == nil {
		csm.notifyChan = make(chan struct{})
	}
	return csm.notifyChan
}

// ClientConn represents a client connection to an RPC server.
type ClientConn struct {
	ctx    context.Context
	cancel context.CancelFunc

	target       string
	parsedTarget resolver.Target
	authority    string
	dopts        dialOptions
	csMgr        *connectivityStateManager

	balancerBuildOpts balancer.BuildOptions
	blockingpicker    *pickerWrapper

	mu              sync.RWMutex
	resolverWrapper *ccResolverWrapper
	sc              *ServiceConfig
	conns           map[*addrConn]struct{}
	// Keepalive parameter can be updated if a GoAway is received.
	mkp             keepalive.ClientParameters
	curBalancerName string
	balancerWrapper *ccBalancerWrapper
	retryThrottler  atomic.Value

	firstResolveEvent *grpcsync.Event

	channelzID int64 // channelz unique identification number
	czData     *channelzData
}

// WaitForStateChange waits until the connectivity.State of ClientConn changes from sourceState or
// ctx expires. A true value is returned in former case and false in latter.
// This is an EXPERIMENTAL API.
func (cc *ClientConn) WaitForStateChange(ctx context.Context, sourceState connectivity.State) bool {
	ch := cc.csMgr.getNotifyChan()
	if cc.csMgr.getState() != sourceState {
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-ch:
		return true
	}
}

// GetState returns the connectivity.State of ClientConn.
// This is an EXPERIMENTAL API.
func (cc *ClientConn) GetState() connectivity.State {
	return cc.csMgr.getState()
}

func (cc *ClientConn) scWatcher() {
	for {
		select {
		case sc, ok := <-cc.dopts.scChan:
			if !ok {
				return
			}
			cc.mu.Lock()
			// TODO: load balance policy runtime change is ignored.
			// We may revisit this decision in the future.
			cc.sc = &sc
			cc.mu.Unlock()
		case <-cc.ctx.Done():
			return
		}
	}
}

// waitForResolvedAddrs blocks until the resolver has provided addresses or the
// context expires.  Returns nil unless the context expires first; otherwise
// returns a status error based on the context.
func (cc *ClientConn) waitForResolvedAddrs(ctx context.Context) error {
	// This is on the RPC path, so we use a fast path to avoid the
	// more-expensive "select" below after the resolver has returned once.
	if cc.firstResolveEvent.HasFired() {
		return nil
	}
	select {
	case <-cc.firstResolveEvent.Done():
		return nil
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	case <-cc.ctx.Done():
		return ErrClientConnClosing
	}
}

// 更新ClientConn的地址和ServiceConfig
func (cc *ClientConn) updateResolverState(s resolver.State) error {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	// Check if the ClientConn is already closed. Some fields (e.g.
	// balancerWrapper) are set to nil when closing the ClientConn, and could
	// cause nil pointer panic if we don't have this check.
	if cc.conns == nil {
		return nil
	}

	// state中的ServiceConfig为空或者ServiceConfig被disable
	if cc.dopts.disableServiceConfig || s.ServiceConfig == nil {
		// 如果存在defaultServiceConfig，则应用defaultServiceConfig
		if cc.dopts.defaultServiceConfig != nil && cc.sc == nil {
			cc.applyServiceConfig(cc.dopts.defaultServiceConfig)
		}
	} else if sc, ok := s.ServiceConfig.(*ServiceConfig); ok { // 应用新的config
		// 应用ServiceConfig
		cc.applyServiceConfig(sc)
	}

	// 负载均衡器变更
	var balCfg serviceconfig.LoadBalancingConfig
	// 如果options中没有指定balancerBuilder，则根据serviceConfig更新balancer
	if cc.dopts.balancerBuilder == nil {
		// Only look at balancer types and switch balancer if balancer dial
		// option is not set.
		var newBalancerName string
		// 如果serviceConfig中指定了负载均衡器配置
		if cc.sc != nil && cc.sc.lbConfig != nil {
			newBalancerName = cc.sc.lbConfig.name
			balCfg = cc.sc.lbConfig.cfg
		} else {
			var isGRPCLB bool
			for _, a := range s.Addresses {
				if a.Type == resolver.GRPCLB {
					isGRPCLB = true
					break
				}
			}
			// 存在grpclb类型的addr，使用grpclb负载均衡器
			if isGRPCLB {
				newBalancerName = grpclbName
			} else if cc.sc != nil && cc.sc.LB != nil { // 如果配置中指定了负载均衡器
				newBalancerName = *cc.sc.LB
			} else {
				newBalancerName = PickFirstBalancerName // 默认使用pickFirst
			}
		}
		// 使用新的负载均衡器
		cc.switchBalancer(newBalancerName)
	} else if cc.balancerWrapper == nil { // options指定了balancerBuilder但是还没有初始化
		// Balancer dial option was set, and this is the first time handling
		// resolved addresses. Build a balancer with dopts.balancerBuilder.
		cc.curBalancerName = cc.dopts.balancerBuilder.Name()
		cc.balancerWrapper = newCCBalancerWrapper(cc, cc.dopts.balancerBuilder, cc.balancerBuildOpts)
	}

	// 通知Balancer变更
	cc.balancerWrapper.updateClientConnState(&balancer.ClientConnState{ResolverState: s, BalancerConfig: balCfg})
	return nil
}

// switchBalancer starts the switching from current balancer to the balancer
// with the given name.
//
// It will NOT send the current address list to the new balancer. If needed,
// caller of this function should send address list to the new balancer after
// this function returns.
//
// Caller must hold cc.mu.
// 更换当前balancer
func (cc *ClientConn) switchBalancer(name string) {
	if strings.EqualFold(cc.curBalancerName, name) {
		return
	}

	grpclog.Infof("ClientConn switching balancer to %q", name)
	// 如果options指定了balancerBuilder，则以options的为主，直接返回
	if cc.dopts.balancerBuilder != nil {
		grpclog.Infoln("ignoring balancer switching: Balancer DialOption used instead")
		return
	}
	// close原来的balancer
	if cc.balancerWrapper != nil {
		cc.balancerWrapper.close()
	}

	// 获取新的balancer对应的builder
	builder := balancer.Get(name)
	if channelz.IsOn() {
		if builder == nil {
			channelz.AddTraceEvent(cc.channelzID, &channelz.TraceEventDesc{
				Desc:     fmt.Sprintf("Channel switches to new LB policy %q due to fallback from invalid balancer name", PickFirstBalancerName),
				Severity: channelz.CtWarning,
			})
		} else {
			channelz.AddTraceEvent(cc.channelzID, &channelz.TraceEventDesc{
				Desc:     fmt.Sprintf("Channel switches to new LB policy %q", name),
				Severity: channelz.CtINFO,
			})
		}
	}
	// 无对应的balancer，默认使用Pickfirse
	if builder == nil {
		grpclog.Infof("failed to get balancer builder for: %v, using pick_first instead", name)
		builder = newPickfirstBuilder()
	}

	cc.curBalancerName = builder.Name()                                          // 设置新的balancerName
	cc.balancerWrapper = newCCBalancerWrapper(cc, builder, cc.balancerBuildOpts) // 创建balancerWrapper
}

func (cc *ClientConn) handleSubConnStateChange(sc balancer.SubConn, s connectivity.State) {
	cc.mu.Lock()
	if cc.conns == nil {
		cc.mu.Unlock()
		return
	}
	// TODO(bar switching) send updates to all balancer wrappers when balancer
	// gracefully switching is supported.
	cc.balancerWrapper.handleSubConnStateChange(sc, s)
	cc.mu.Unlock()
}

// newAddrConn creates an addrConn for addrs and adds it to cc.conns.
//
// Caller needs to make sure len(addrs) > 0.
func (cc *ClientConn) newAddrConn(addrs []resolver.Address, opts balancer.NewSubConnOptions) (*addrConn, error) {
	ac := &addrConn{
		cc:           cc,    // ClientConn
		addrs:        addrs, // 地址
		scopts:       opts,
		dopts:        cc.dopts,
		czData:       new(channelzData),
		resetBackoff: make(chan struct{}),
	}
	// context和cancel
	ac.ctx, ac.cancel = context.WithCancel(cc.ctx)
	// Track ac in cc. This needs to be done before any getTransport(...) is called.
	cc.mu.Lock()
	if cc.conns == nil {
		cc.mu.Unlock()
		return nil, ErrClientConnClosing
	}
	if channelz.IsOn() {
		ac.channelzID = channelz.RegisterSubChannel(ac, cc.channelzID, "")
		channelz.AddTraceEvent(ac.channelzID, &channelz.TraceEventDesc{
			Desc:     "Subchannel Created",
			Severity: channelz.CtINFO,
			Parent: &channelz.TraceEventDesc{
				Desc:     fmt.Sprintf("Subchannel(id:%d) created", ac.channelzID),
				Severity: channelz.CtINFO,
			},
		})
	}
	// 保存到ClientConn中
	cc.conns[ac] = struct{}{}
	cc.mu.Unlock()
	return ac, nil
}

// removeAddrConn removes the addrConn in the subConn from clientConn.
// It also tears down the ac with the given error.
func (cc *ClientConn) removeAddrConn(ac *addrConn, err error) {
	cc.mu.Lock()
	if cc.conns == nil {
		cc.mu.Unlock()
		return
	}
	delete(cc.conns, ac)
	cc.mu.Unlock()
	ac.tearDown(err)
}

func (cc *ClientConn) channelzMetric() *channelz.ChannelInternalMetric {
	return &channelz.ChannelInternalMetric{
		State:                    cc.GetState(),
		Target:                   cc.target,
		CallsStarted:             atomic.LoadInt64(&cc.czData.callsStarted),
		CallsSucceeded:           atomic.LoadInt64(&cc.czData.callsSucceeded),
		CallsFailed:              atomic.LoadInt64(&cc.czData.callsFailed),
		LastCallStartedTimestamp: time.Unix(0, atomic.LoadInt64(&cc.czData.lastCallStartedTime)),
	}
}

// Target returns the target string of the ClientConn.
// This is an EXPERIMENTAL API.
func (cc *ClientConn) Target() string {
	return cc.target
}

func (cc *ClientConn) incrCallsStarted() {
	atomic.AddInt64(&cc.czData.callsStarted, 1)
	atomic.StoreInt64(&cc.czData.lastCallStartedTime, time.Now().UnixNano())
}

func (cc *ClientConn) incrCallsSucceeded() {
	atomic.AddInt64(&cc.czData.callsSucceeded, 1)
}

func (cc *ClientConn) incrCallsFailed() {
	atomic.AddInt64(&cc.czData.callsFailed, 1)
}

// connect starts creating a transport.
// It does nothing if the ac is not IDLE.
// TODO(bar) Move this to the addrConn section.
func (ac *addrConn) connect() error {
	ac.mu.Lock()
	// 如果状态是Shutdown，不允许连接，保错
	if ac.state == connectivity.Shutdown {
		ac.mu.Unlock()
		return errConnClosing
	}
	// 如果状态不是Idle，表示已经连接，直接返回
	if ac.state != connectivity.Idle {
		ac.mu.Unlock()
		return nil
	}
	// Update connectivity state within the lock to prevent subsequent or
	// concurrent calls from resetting the transport more than once.
	// 更新状态为Connecting
	ac.updateConnectivityState(connectivity.Connecting)
	ac.mu.Unlock()

	// Start a goroutine connecting to the server asynchronously.
	// 异步启动一个协程去连接服务
	go ac.resetTransport()
	return nil
}

// tryUpdateAddrs tries to update ac.addrs with the new addresses list.
//
// If ac is Connecting, it returns false. The caller should tear down the ac and
// create a new one. Note that the backoff will be reset when this happens.
//
// If ac is TransientFailure, it updates ac.addrs and returns true. The updated
// addresses will be picked up by retry in the next iteration after backoff.
//
// If ac is Shutdown or Idle, it updates ac.addrs and returns true.
//
// If ac is Ready, it checks whether current connected address of ac is in the
// new addrs list.
//  - If true, it updates ac.addrs and returns true. The ac will keep using
//    the existing connection.
//  - If false, it does nothing and returns false.
func (ac *addrConn) tryUpdateAddrs(addrs []resolver.Address) bool {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	grpclog.Infof("addrConn: tryUpdateAddrs curAddr: %v, addrs: %v", ac.curAddr, addrs)
	if ac.state == connectivity.Shutdown ||
		ac.state == connectivity.TransientFailure ||
		ac.state == connectivity.Idle {
		ac.addrs = addrs
		return true
	}

	if ac.state == connectivity.Connecting {
		return false
	}

	// ac.state is Ready, try to find the connected address.
	var curAddrFound bool
	for _, a := range addrs {
		if reflect.DeepEqual(ac.curAddr, a) {
			curAddrFound = true
			break
		}
	}
	grpclog.Infof("addrConn: tryUpdateAddrs curAddrFound: %v", curAddrFound)
	if curAddrFound {
		ac.addrs = addrs
	}

	return curAddrFound
}

// GetMethodConfig gets the method config of the input method.
// If there's an exact match for input method (i.e. /service/method), we return
// the corresponding MethodConfig.
// If there isn't an exact match for the input method, we look for the default config
// under the service (i.e /service/). If there is a default MethodConfig for
// the service, we return it.
// Otherwise, we return an empty MethodConfig.
func (cc *ClientConn) GetMethodConfig(method string) MethodConfig {
	// TODO: Avoid the locking here.
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	if cc.sc == nil {
		return MethodConfig{}
	}
	m, ok := cc.sc.Methods[method]
	if !ok {
		i := strings.LastIndex(method, "/")
		m = cc.sc.Methods[method[:i+1]]
	}
	return m
}

func (cc *ClientConn) healthCheckConfig() *healthCheckConfig {
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	if cc.sc == nil {
		return nil
	}
	return cc.sc.healthCheckConfig
}

func (cc *ClientConn) getTransport(ctx context.Context, failfast bool, method string) (transport.ClientTransport, func(balancer.DoneInfo), error) {
	// picker根据负载均衡策略选择一条连接
	// 这里的blockingpicker是balancer.Picker的wrapper
	t, done, err := cc.blockingpicker.pick(ctx, failfast, balancer.PickOptions{
		FullMethodName: method,
	})
	if err != nil {
		return nil, nil, toRPCErr(err)
	}
	return t, done, nil
}

func (cc *ClientConn) applyServiceConfig(sc *ServiceConfig) error {
	if sc == nil {
		// should never reach here.
		return fmt.Errorf("got nil pointer for service config")
	}
	cc.sc = sc

	if cc.sc.retryThrottling != nil {
		newThrottler := &retryThrottler{
			tokens: cc.sc.retryThrottling.MaxTokens,
			max:    cc.sc.retryThrottling.MaxTokens,
			thresh: cc.sc.retryThrottling.MaxTokens / 2,
			ratio:  cc.sc.retryThrottling.TokenRatio,
		}
		cc.retryThrottler.Store(newThrottler)
	} else {
		cc.retryThrottler.Store((*retryThrottler)(nil))
	}

	return nil
}

func (cc *ClientConn) resolveNow(o resolver.ResolveNowOption) {
	cc.mu.RLock()
	r := cc.resolverWrapper
	cc.mu.RUnlock()
	if r == nil {
		return
	}
	go r.resolveNow(o)
}

// ResetConnectBackoff wakes up all subchannels in transient failure and causes
// them to attempt another connection immediately.  It also resets the backoff
// times used for subsequent attempts regardless of the current state.
//
// In general, this function should not be used.  Typical service or network
// outages result in a reasonable client reconnection strategy by default.
// However, if a previously unavailable network becomes available, this may be
// used to trigger an immediate reconnect.
//
// This API is EXPERIMENTAL.
func (cc *ClientConn) ResetConnectBackoff() {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	for ac := range cc.conns {
		ac.resetConnectBackoff()
	}
}

// Close tears down the ClientConn and all underlying connections.
func (cc *ClientConn) Close() error {
	defer cc.cancel()

	cc.mu.Lock()
	if cc.conns == nil {
		cc.mu.Unlock()
		return ErrClientConnClosing
	}
	conns := cc.conns
	cc.conns = nil
	cc.csMgr.updateState(connectivity.Shutdown)

	rWrapper := cc.resolverWrapper
	cc.resolverWrapper = nil
	bWrapper := cc.balancerWrapper
	cc.balancerWrapper = nil
	cc.mu.Unlock()

	cc.blockingpicker.close()

	if rWrapper != nil {
		rWrapper.close()
	}
	if bWrapper != nil {
		bWrapper.close()
	}

	for ac := range conns {
		ac.tearDown(ErrClientConnClosing)
	}
	if channelz.IsOn() {
		ted := &channelz.TraceEventDesc{
			Desc:     "Channel Deleted",
			Severity: channelz.CtINFO,
		}
		if cc.dopts.channelzParentID != 0 {
			ted.Parent = &channelz.TraceEventDesc{
				Desc:     fmt.Sprintf("Nested channel(id:%d) deleted", cc.channelzID),
				Severity: channelz.CtINFO,
			}
		}
		channelz.AddTraceEvent(cc.channelzID, ted)
		// TraceEvent needs to be called before RemoveEntry, as TraceEvent may add trace reference to
		// the entity being deleted, and thus prevent it from being deleted right away.
		channelz.RemoveEntry(cc.channelzID)
	}
	return nil
}

// addrConn is a network connection to a given address.
type addrConn struct {
	ctx    context.Context
	cancel context.CancelFunc

	cc     *ClientConn
	dopts  dialOptions
	acbw   balancer.SubConn
	scopts balancer.NewSubConnOptions

	// transport is set when there's a viable transport (note: ac state may not be READY as LB channel
	// health checking may require server to report healthy to set ac to READY), and is reset
	// to nil when the current transport should no longer be used to create a stream (e.g. after GoAway
	// is received, transport is closed, ac has been torn down).
	transport transport.ClientTransport // The current transport.

	mu      sync.Mutex
	curAddr resolver.Address   // The current address.
	addrs   []resolver.Address // All addresses that the resolver resolved to.

	// Use updateConnectivityState for updating addrConn's connectivity state.
	state connectivity.State

	backoffIdx   int // Needs to be stateful for resetConnectBackoff.
	resetBackoff chan struct{}

	channelzID int64 // channelz unique identification number.
	czData     *channelzData
}

// Note: this requires a lock on ac.mu.
func (ac *addrConn) updateConnectivityState(s connectivity.State) {
	if ac.state == s {
		return
	}

	updateMsg := fmt.Sprintf("Subchannel Connectivity change to %v", s)
	// 状态变更
	ac.state = s
	if channelz.IsOn() {
		channelz.AddTraceEvent(ac.channelzID, &channelz.TraceEventDesc{
			Desc:     updateMsg,
			Severity: channelz.CtINFO,
		})
	}
	// 调用ClientConn的SubConn状态变更回调
	ac.cc.handleSubConnStateChange(ac.acbw, s)
}

// adjustParams updates parameters used to create transports upon
// receiving a GoAway.
func (ac *addrConn) adjustParams(r transport.GoAwayReason) {
	switch r {
	case transport.GoAwayTooManyPings:
		v := 2 * ac.dopts.copts.KeepaliveParams.Time
		ac.cc.mu.Lock()
		if v > ac.cc.mkp.Time {
			ac.cc.mkp.Time = v
		}
		ac.cc.mu.Unlock()
	}
}

func (ac *addrConn) resetTransport() {
	for i := 0; ; i++ {
		// 如果发生重试，则手动触发重新resolve
		if i > 0 {
			ac.cc.resolveNow(resolver.ResolveNowOption{})
		}

		ac.mu.Lock()
		// 如果ac的状态变更为Shutdown，Shutdown表示该addrConn已经移除掉了
		if ac.state == connectivity.Shutdown {
			ac.mu.Unlock()
			return
		}
		// 要连接的addr
		addrs := ac.addrs
		backoffFor := ac.dopts.bs.Backoff(ac.backoffIdx)
		// This will be the duration that dial gets to finish.
		dialDuration := minConnectTimeout
		if ac.dopts.minConnectTimeout != nil {
			dialDuration = ac.dopts.minConnectTimeout()
		}

		if dialDuration < backoffFor {
			// Give dial more time as we keep failing to connect.
			dialDuration = backoffFor
		}
		// We can potentially spend all the time trying the first address, and
		// if the server accepts the connection and then hangs, the following
		// addresses will never be tried.
		//
		// The spec doesn't mention what should be done for multiple addresses.
		// https://github.com/grpc/grpc/blob/master/doc/connection-backoff.md#proposed-backoff-algorithm
		connectDeadline := time.Now().Add(dialDuration)
		// 更新状态为Connecting
		ac.updateConnectivityState(connectivity.Connecting)
		ac.transport = nil
		ac.mu.Unlock()

		// 尝试创建连接，只要一个addr成功立即返回
		newTr, addr, reconnect, err := ac.tryAllAddrs(addrs, connectDeadline)
		if err != nil { // 创建失败
			// After exhausting all addresses, the addrConn enters
			// TRANSIENT_FAILURE.
			ac.mu.Lock()
			// 已经Shutdown
			if ac.state == connectivity.Shutdown {
				ac.mu.Unlock()
				return
			}
			// 更新状态为TransientFailure，表示出现错误，但是期待恢复
			ac.updateConnectivityState(connectivity.TransientFailure)

			// Backoff.
			b := ac.resetBackoff
			ac.mu.Unlock()
			// sleep一下，然后重试
			timer := time.NewTimer(backoffFor)
			select {
			case <-timer.C:
				ac.mu.Lock()
				ac.backoffIdx++
				ac.mu.Unlock()
			case <-b:
				timer.Stop()
			case <-ac.ctx.Done():
				timer.Stop()
				return
			}
			continue
		}

		// 这里表示已经创建连接成功

		ac.mu.Lock()
		// 双重检查，是否已经Shutdown
		if ac.state == connectivity.Shutdown {
			newTr.Close()
			ac.mu.Unlock()
			return
		}
		// 当前连接的addr
		ac.curAddr = addr
		// transport
		ac.transport = newTr
		ac.backoffIdx = 0
		// 健康检查配置
		healthCheckConfig := ac.cc.healthCheckConfig()
		// LB channel health checking is only enabled when all the four requirements below are met:
		// 1. it is not disabled by the user with the WithDisableHealthCheck DialOption,
		// 2. the internal.HealthCheckFunc is set by importing the grpc/healthcheck package,
		// 3. a service config with non-empty healthCheckConfig field is provided,
		// 4. the current load balancer allows it.
		// 创建用于健康检查的context
		hctx, hcancel := context.WithCancel(ac.ctx)
		healthcheckManagingState := false
		// 如果启用了健康检查
		if !ac.cc.dopts.disableHealthCheck && healthCheckConfig != nil && ac.scopts.HealthCheckEnabled {
			if ac.cc.dopts.healthCheckFunc == nil {
				// TODO: add a link to the health check doc in the error message.
				grpclog.Error("the client side LB channel health check function has not been set.")
			} else {
				// TODO(deklerk) refactor to just return transport
				// 开启监控检查
				go ac.startHealthCheck(hctx, newTr, addr, healthCheckConfig.ServiceName)
				healthcheckManagingState = true
			}
		}

		// 如果没有健康检查，则立即更新连接状态为Ready
		if !healthcheckManagingState {
			ac.updateConnectivityState(connectivity.Ready)
		}
		ac.mu.Unlock()

		// Block until the created transport is down. And when this happens,
		// we restart from the top of the addr list.
		// 阻塞等待连接down掉，重连
		<-reconnect.Done()
		// 连接已经down掉，取消健康检查
		hcancel()

		// Need to reconnect after a READY, the addrConn enters
		// TRANSIENT_FAILURE.
		//
		// This will set addrConn to TRANSIENT_FAILURE for a very short period
		// of time, and turns CONNECTING. It seems reasonable to skip this, but
		// READY-CONNECTING is not a valid transition.
		ac.mu.Lock()
		// 如果已经Shutdown
		if ac.state == connectivity.Shutdown {
			ac.mu.Unlock()
			return
		}
		// 更新状态为TransientFailure，重新连接
		ac.updateConnectivityState(connectivity.TransientFailure)
		ac.mu.Unlock()
	}
}

// tryAllAddrs tries to creates a connection to the addresses, and stop when at the
// first successful one. It returns the transport, the address and a Event in
// the successful case. The Event fires when the returned transport disconnects.
func (ac *addrConn) tryAllAddrs(addrs []resolver.Address, connectDeadline time.Time) (transport.ClientTransport, resolver.Address, *grpcsync.Event, error) {
	// 遍历addrs
	for _, addr := range addrs {
		ac.mu.Lock()
		// 如果addrConn已经Shutdown
		if ac.state == connectivity.Shutdown {
			ac.mu.Unlock()
			return nil, resolver.Address{}, nil, errConnClosing
		}

		ac.cc.mu.RLock()
		ac.dopts.copts.KeepaliveParams = ac.cc.mkp
		ac.cc.mu.RUnlock()

		copts := ac.dopts.copts
		if ac.scopts.CredsBundle != nil {
			copts.CredsBundle = ac.scopts.CredsBundle
		}
		ac.mu.Unlock()

		if channelz.IsOn() {
			channelz.AddTraceEvent(ac.channelzID, &channelz.TraceEventDesc{
				Desc:     fmt.Sprintf("Subchannel picks a new address %q to connect", addr.Addr),
				Severity: channelz.CtINFO,
			})
		}

		// 创建连接
		newTr, reconnect, err := ac.createTransport(addr, copts, connectDeadline)
		// 创建成功
		if err == nil {
			return newTr, addr, reconnect, nil
		}
		ac.cc.blockingpicker.updateConnectionError(err)
	}

	// Couldn't connect to any address.
	// 创建失败
	return nil, resolver.Address{}, nil, fmt.Errorf("couldn't connect to any address")
}

// createTransport creates a connection to addr. It returns the transport and a
// Event in the successful case. The Event fires when the returned transport
// disconnects.
func (ac *addrConn) createTransport(addr resolver.Address, copts transport.ConnectOptions, connectDeadline time.Time) (transport.ClientTransport, *grpcsync.Event, error) {
	prefaceReceived := make(chan struct{})
	onCloseCalled := make(chan struct{})
	reconnect := grpcsync.NewEvent()

	target := transport.TargetInfo{
		Addr:      addr.Addr,
		Metadata:  addr.Metadata,
		Authority: ac.cc.authority,
	}

	// goaway回调
	onGoAway := func(r transport.GoAwayReason) {
		ac.mu.Lock()
		ac.adjustParams(r)
		ac.mu.Unlock()
		// 通知触发重连
		reconnect.Fire()
	}

	// close回调
	onClose := func() {
		close(onCloseCalled)
		// 通知触发重连
		reconnect.Fire()
	}

	onPrefaceReceipt := func() {
		close(prefaceReceived)
	}

	connectCtx, cancel := context.WithDeadline(ac.ctx, connectDeadline)
	defer cancel()
	if channelz.IsOn() {
		copts.ChannelzParentID = ac.channelzID
	}

	// 创建transport
	newTr, err := transport.NewClientTransport(connectCtx, ac.cc.ctx, target, copts, onPrefaceReceipt, onGoAway, onClose)
	if err != nil {
		// newTr is either nil, or closed.
		grpclog.Warningf("grpc: addrConn.createTransport failed to connect to %v. Err :%v. Reconnecting...", addr, err)
		return nil, nil, err
	}

	// 要求握手
	if ac.dopts.reqHandshake == envconfig.RequireHandshakeOn {
		select {
		case <-time.After(connectDeadline.Sub(time.Now())):
			// We didn't get the preface in time.
			newTr.Close()
			grpclog.Warningf("grpc: addrConn.createTransport failed to connect to %v: didn't receive server preface in time. Reconnecting...", addr)
			return nil, nil, errors.New("timed out waiting for server handshake")
		case <-prefaceReceived:
			// We got the preface - huzzah! things are good.
		case <-onCloseCalled:
			// The transport has already closed - noop.
			return nil, nil, errors.New("connection closed")
			// TODO(deklerk) this should bail on ac.ctx.Done(). Add a test and fix.
		}
	}
	return newTr, reconnect, nil
}

// 开启健康检查
func (ac *addrConn) startHealthCheck(ctx context.Context, newTr transport.ClientTransport, addr resolver.Address, serviceName string) {
	// Set up the health check helper functions
	// 该方法用于创建新的stream
	newStream := func() (interface{}, error) {
		return ac.newClientStream(ctx, &StreamDesc{ServerStreams: true}, "/grpc.health.v1.Health/Watch", newTr)
	}
	firstReady := true
	// 报告健康检查结果
	reportHealth := func(ok bool) {
		ac.mu.Lock()
		defer ac.mu.Unlock()
		// 如果transport已经变更了，直接返回
		if ac.transport != newTr {
			return
		}
		// 健康检查成功
		if ok {
			if firstReady {
				firstReady = false
				ac.curAddr = addr
			}
			// 更新状态为Ready
			ac.updateConnectivityState(connectivity.Ready)
		} else {
			// 更新状态为TransientFailure
			ac.updateConnectivityState(connectivity.TransientFailure)
		}
	}
	// 执行健康检查
	err := ac.cc.dopts.healthCheckFunc(ctx, newStream, reportHealth, serviceName)
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			if channelz.IsOn() {
				channelz.AddTraceEvent(ac.channelzID, &channelz.TraceEventDesc{
					Desc:     "Subchannel health check is unimplemented at server side, thus health check is disabled",
					Severity: channelz.CtError,
				})
			}
			grpclog.Error("Subchannel health check is unimplemented at server side, thus health check is disabled")
		} else {
			grpclog.Errorf("HealthCheckFunc exits with unexpected error %v", err)
		}
	}
}

func (ac *addrConn) resetConnectBackoff() {
	ac.mu.Lock()
	close(ac.resetBackoff)
	ac.backoffIdx = 0
	ac.resetBackoff = make(chan struct{})
	ac.mu.Unlock()
}

// getReadyTransport returns the transport if ac's state is READY.
// Otherwise it returns nil, false.
// If ac's state is IDLE, it will trigger ac to connect.
func (ac *addrConn) getReadyTransport() (transport.ClientTransport, bool) {
	ac.mu.Lock()
	// 如果是Ready状态
	if ac.state == connectivity.Ready && ac.transport != nil {
		t := ac.transport
		ac.mu.Unlock()
		return t, true
	}
	var idle bool
	// 如果是Idle状态
	if ac.state == connectivity.Idle {
		idle = true
	}
	ac.mu.Unlock()
	// Trigger idle ac to connect.
	if idle {
		// 触发连接
		ac.connect()
	}
	return nil, false
}

// tearDown starts to tear down the addrConn.
// TODO(zhaoq): Make this synchronous to avoid unbounded memory consumption in
// some edge cases (e.g., the caller opens and closes many addrConn's in a
// tight loop.
// tearDown doesn't remove ac from ac.cc.conns.
func (ac *addrConn) tearDown(err error) {
	ac.mu.Lock()
	if ac.state == connectivity.Shutdown {
		ac.mu.Unlock()
		return
	}
	curTr := ac.transport
	ac.transport = nil
	// We have to set the state to Shutdown before anything else to prevent races
	// between setting the state and logic that waits on context cancelation / etc.
	ac.updateConnectivityState(connectivity.Shutdown)
	ac.cancel()
	ac.curAddr = resolver.Address{}
	if err == errConnDrain && curTr != nil {
		// GracefulClose(...) may be executed multiple times when
		// i) receiving multiple GoAway frames from the server; or
		// ii) there are concurrent name resolver/Balancer triggered
		// address removal and GoAway.
		// We have to unlock and re-lock here because GracefulClose => Close => onClose, which requires locking ac.mu.
		ac.mu.Unlock()
		curTr.GracefulClose()
		ac.mu.Lock()
	}
	if channelz.IsOn() {
		channelz.AddTraceEvent(ac.channelzID, &channelz.TraceEventDesc{
			Desc:     "Subchannel Deleted",
			Severity: channelz.CtINFO,
			Parent: &channelz.TraceEventDesc{
				Desc:     fmt.Sprintf("Subchanel(id:%d) deleted", ac.channelzID),
				Severity: channelz.CtINFO,
			},
		})
		// TraceEvent needs to be called before RemoveEntry, as TraceEvent may add trace reference to
		// the entity beng deleted, and thus prevent it from being deleted right away.
		channelz.RemoveEntry(ac.channelzID)
	}
	ac.mu.Unlock()
}

func (ac *addrConn) getState() connectivity.State {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	return ac.state
}

func (ac *addrConn) ChannelzMetric() *channelz.ChannelInternalMetric {
	ac.mu.Lock()
	addr := ac.curAddr.Addr
	ac.mu.Unlock()
	return &channelz.ChannelInternalMetric{
		State:                    ac.getState(),
		Target:                   addr,
		CallsStarted:             atomic.LoadInt64(&ac.czData.callsStarted),
		CallsSucceeded:           atomic.LoadInt64(&ac.czData.callsSucceeded),
		CallsFailed:              atomic.LoadInt64(&ac.czData.callsFailed),
		LastCallStartedTimestamp: time.Unix(0, atomic.LoadInt64(&ac.czData.lastCallStartedTime)),
	}
}

func (ac *addrConn) incrCallsStarted() {
	atomic.AddInt64(&ac.czData.callsStarted, 1)
	atomic.StoreInt64(&ac.czData.lastCallStartedTime, time.Now().UnixNano())
}

func (ac *addrConn) incrCallsSucceeded() {
	atomic.AddInt64(&ac.czData.callsSucceeded, 1)
}

func (ac *addrConn) incrCallsFailed() {
	atomic.AddInt64(&ac.czData.callsFailed, 1)
}

type retryThrottler struct {
	max    float64
	thresh float64
	ratio  float64

	mu     sync.Mutex
	tokens float64 // TODO(dfawley): replace with atomic and remove lock.
}

// throttle subtracts a retry token from the pool and returns whether a retry
// should be throttled (disallowed) based upon the retry throttling policy in
// the service config.
func (rt *retryThrottler) throttle() bool {
	if rt == nil {
		return false
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.tokens--
	if rt.tokens < 0 {
		rt.tokens = 0
	}
	return rt.tokens <= rt.thresh
}

func (rt *retryThrottler) successfulRPC() {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.tokens += rt.ratio
	if rt.tokens > rt.max {
		rt.tokens = rt.max
	}
}

type channelzChannel struct {
	cc *ClientConn
}

func (c *channelzChannel) ChannelzMetric() *channelz.ChannelInternalMetric {
	return c.cc.channelzMetric()
}

// ErrClientConnTimeout indicates that the ClientConn cannot establish the
// underlying connections within the specified timeout.
//
// Deprecated: This error is never returned by grpc and should not be
// referenced by users.
var ErrClientConnTimeout = errors.New("grpc: timed out when dialing")
