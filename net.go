package netem

//
// Full replacement for [net]
//

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// Net is a drop-in replacement for the [net] package. The zero
// value is invalid; please init all the MANDATORY fields.
type Net struct {
	// Stack is the MANDATORY underlying stack.
	Stack UnderlyingNetwork
}

// ErrDial contains all the errors occurred during a [DialContext] operation.
type ErrDial struct {
	// Errors contains the list of errors.
	Errors []error
}

var _ error = &ErrDial{}

// Error implements error
func (e *ErrDial) Error() string {
	var b strings.Builder
	b.WriteString("dial failed: ")
	for index, err := range e.Errors {
		b.WriteString(err.Error())
		if index < len(e.Errors)-1 {
			b.WriteString("; ")
		}
	}
	return b.String()
}

// Is allows errors.Is predicates to match child errors.
func (e *ErrDial) Is(target error) bool {
	for _, child := range e.Errors {
		if errors.Is(child, target) {
			return true
		}
	}
	return false
}

// DialContext is a drop-in replacement for [net.Dialer.DialContext].
func (n *Net) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	// determine the domain or IP address we're connecting to
	domain, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}

	// make sure we have IP addresses to try
	var addresses []string
	switch v := net.ParseIP(domain); v {
	default:
		addresses = append(addresses, domain)
	case nil:
		addresses, err = n.LookupHost(ctx, domain)
		if err != nil {
			return nil, err
		}
	}

	// try each available address
	errlist := &ErrDial{}
	for _, ip := range addresses {
		endpoint := net.JoinHostPort(ip, port)
		conn, err := n.Stack.DialContext(ctx, network, endpoint)
		if err != nil {
			errlist.Errors = append(errlist.Errors, fmt.Errorf("%s: %w", endpoint, err))
			continue
		}
		return conn, nil
	}

	return nil, errlist
}

// DialTLSContext is like [Net.DialContext] but also performs a TLS handshake.
func (n *Net) DialTLSContext(ctx context.Context, network, address string) (net.Conn, error) {
	hostname, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	conn, err := n.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	config := &tls.Config{
		RootCAs:    n.Stack.DefaultCertPool(),
		NextProtos: nil, // TODO(bassosimone): automatically generate the right ALPN
		ServerName: hostname,
	}
	tc := tls.Client(conn, config)
	if err := n.tlsHandshake(ctx, tc); err != nil {
		conn.Close() // closing the conn here unblocks the background goroutine
		return nil, err
	}
	return tc, nil
}

// tlsHandshake ensures we honour the context's deadline and cancellation
func (n *Net) tlsHandshake(ctx context.Context, tc *tls.Conn) error {
	if deadline, ok := ctx.Deadline(); ok {
		tc.SetDeadline(deadline)
		defer tc.SetDeadline(time.Time{})
	}
	errch := make(chan error, 1)
	go func() {
		errch <- tc.HandshakeContext(ctx)
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errch:
		return err
	}
}

// LookupHost is a drop-in replacement for [net.Resolver.LookupHost].
func (n *Net) LookupHost(ctx context.Context, domain string) ([]string, error) {
	addrs, _, err := n.Stack.GetaddrinfoLookupANY(ctx, domain)
	return addrs, err
}

// LookupCNAME is a drop-in replacement for [net.Resolver.LookupCNAME].
func (n *Net) LookupCNAME(ctx context.Context, domain string) (string, error) {
	_, cname, err := n.Stack.GetaddrinfoLookupANY(ctx, domain)
	return cname, err
}

// ListenTCP is a drop-in replacement for [net.ListenTCP].
func (n *Net) ListenTCP(network string, addr *net.TCPAddr) (net.Listener, error) {
	return n.Stack.ListenTCP(network, addr)
}

// ListenUDP is a drop-in replacement for [net.ListenUDP].
func (n *Net) ListenUDP(network string, addr *net.UDPAddr) (UDPLikeConn, error) {
	return n.Stack.ListenUDP(network, addr)
}

// ListenTLS is a replacement for [tls.Listen] that uses the underlying
// stack's TLS MITM capabilities during the TLS handshake.
func (n *Net) ListenTLS(network string, laddr *net.TCPAddr, config *tls.Config) (net.Listener, error) {
	listener, err := n.ListenTCP(network, laddr)
	if err != nil {
		return nil, err
	}
	lw := &netListenerTLS{
		config:   config,
		listener: listener,
		stack:    n.Stack,
	}
	return lw, nil
}

// netListenerTLS is a TLS listener.
type netListenerTLS struct {
	config   *tls.Config
	listener net.Listener
	stack    UnderlyingNetwork
}

var _ net.Listener = &netListenerTLS{}

// Accept implements net.Listener
func (lw *netListenerTLS) Accept() (net.Conn, error) {
	conn, err := lw.listener.Accept()
	if err != nil {
		return nil, err
	}
	tc := tls.Server(conn, lw.config)
	// make sure there is a maximum timeout for the handshake
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := tc.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, err
	}
	return tc, nil
}

// Addr implements net.Listener
func (lw *netListenerTLS) Addr() net.Addr {
	return lw.listener.Addr()
}

// Close implements net.Listener
func (lw *netListenerTLS) Close() error {
	return lw.listener.Close()
}
