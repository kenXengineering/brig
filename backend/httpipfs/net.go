package httpipfs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path"
	"sync"
	"time"

	shell "github.com/ipfs/go-ipfs-api"
	netBackend "github.com/sahib/brig/net/backend"
	log "github.com/sirupsen/logrus"
)

// TODO: Move this to util.
func findFreePort() int {
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0
	}

	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

//////////////////////////

type connWrapper struct {
	net.Conn

	peer       string
	protocol   string
	targetAddr string
	sh         *shell.Shell
}

func (cw *connWrapper) LocalAddr() net.Addr {
	return &addrWrapper{
		protocol: cw.protocol,
		peer:     "",
	}
}

func (cw *connWrapper) RemoteAddr() net.Addr {
	return &addrWrapper{
		protocol: cw.protocol,
		peer:     cw.peer,
	}
}

func (cw *connWrapper) Close() error {
	defer cw.Conn.Close()
	return closeStream(cw.sh, cw.protocol, "", cw.targetAddr)
}

func (nd *Node) Dial(peerHash, protocol string) (net.Conn, error) {
	if !nd.allowNetOps {
		return nil, ErrOffline
	}

	protocol = path.Join(protocol, peerHash)

	port := findFreePort()
	addr := fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", port)
	if err := forward(nd.sh, protocol, addr, peerHash); err != nil {
		return nil, err
	}

	tcpAddr := fmt.Sprintf("127.0.0.1:%d", port)
	log.Debugf("dial to »%s« over port %d", peerHash, port)
	conn, err := net.Dial("tcp", tcpAddr)
	if err != nil {
		return nil, err
	}

	return &connWrapper{
		Conn:       conn,
		peer:       peerHash,
		protocol:   protocol,
		targetAddr: addr,
		sh:         nd.sh,
	}, nil
}

//////////////////////////

func forward(sh *shell.Shell, protocol, targetAddr, peerID string) error {
	ctx := context.Background()
	peerID = "/ipfs/" + peerID

	rb := sh.Request("p2p/forward", protocol, targetAddr, peerID)
	rb.Option("allow-custom-protocol", true)
	resp, err := rb.Send(ctx)
	if err != nil {
		return err
	}

	defer resp.Close()
	if resp.Error != nil {
		return resp.Error
	}

	return nil
}

func openListener(sh *shell.Shell, protocol, targetAddr string) error {
	ctx := context.Background()
	rb := sh.Request("p2p/listen", protocol, targetAddr)
	rb.Option("allow-custom-protocol", true)
	resp, err := rb.Send(ctx)
	if err != nil {
		return err
	}

	defer resp.Close()
	if err := resp.Error; err != nil {
		return err
	}

	return nil
}

func closeStream(sh *shell.Shell, protocol, targetAddr, listenAddr string) error {
	ctx := context.Background()
	rb := sh.Request("p2p/close")
	rb.Option("protocol", protocol)

	if targetAddr != "" {
		rb.Option("target-address", targetAddr)
	}

	if listenAddr != "" {
		rb.Option("listen-address", listenAddr)
	}

	resp, err := rb.Send(ctx)
	if err != nil {
		return err
	}

	defer resp.Close()
	if resp.Error != nil {
		return resp.Error
	}

	return nil
}

type addrWrapper struct {
	protocol string
	peer     string
}

func (sa *addrWrapper) Network() string {
	return sa.protocol
}

func (sa *addrWrapper) String() string {
	return sa.peer
}

type listenerWrapper struct {
	net.Listener
	protocol   string
	peer       string
	targetAddr string
	sh         *shell.Shell
}

func (lw *listenerWrapper) Addr() net.Addr {
	return &addrWrapper{
		protocol: lw.protocol,
		peer:     lw.peer,
	}
}

func (lw *listenerWrapper) Close() error {
	defer lw.Listener.Close()
	return closeStream(lw.sh, lw.protocol, lw.targetAddr, "")
}

func (nd *Node) Listen(protocol string) (net.Listener, error) {
	if !nd.allowNetOps {
		return nil, ErrOffline
	}

	self, err := nd.Identity()
	if err != nil {
		return nil, err
	}

	// Append the id to the protocol:
	protocol = path.Join(protocol, self.Addr)

	port := findFreePort()
	addr := fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", port)

	// Prevent errors by closing any previously opened listeners:
	if err := closeStream(nd.sh, protocol, "", ""); err != nil {
		return nil, err
	}

	log.Debugf("backend: listening for %s over port %d", protocol, port)
	if err := openListener(nd.sh, protocol, addr); err != nil {
		return nil, err
	}

	localAddr := fmt.Sprintf("127.0.0.1:%d", port)
	lst, err := net.Listen("tcp", localAddr)
	if err != nil {
		return nil, err
	}

	return &listenerWrapper{
		Listener:   lst,
		protocol:   protocol,
		peer:       self.Addr,
		targetAddr: addr,
		sh:         nd.sh,
	}, nil
}

/////////////////////////////////

type pinger struct {
	lastSeen  time.Time
	roundtrip time.Duration
	err       error

	mu     sync.Mutex
	cancel func()
	nd     *Node
}

// LastSeen returns the time we pinged the remote last time.
func (p *pinger) LastSeen() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.lastSeen
}

// Roundtrip returns the time needed send a single package to
// the remote and receive the answer.
func (p *pinger) Roundtrip() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.roundtrip
}

// Err will return a non-nil error when the current ping did not succeed.
func (p *pinger) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.err
}

// Close will clean up the pinger.
func (p *pinger) Close() error {
	if p.cancel != nil {
		p.cancel()
	}

	p.cancel = nil
	return nil
}

func (p *pinger) Run(ctx context.Context, addr string) error {
	self, err := p.nd.Identity()
	if err != nil {
		return err
	}

	tckr := time.NewTicker(10 * time.Second)

	for {
		select {
		case <-ctx.Done():
			break
		case <-tckr.C:
			// Edge case: test setups where we ping ourselves.
			if self.Addr == addr {
				p.mu.Lock()
				p.err = nil
				p.lastSeen = time.Now()
				p.roundtrip = time.Duration(0)
				p.mu.Unlock()
				continue
			}

			log.Debugf("backend: do ping »%s«", addr)
			roundtrip, err := ping(p.nd.sh, addr)
			p.mu.Lock()
			log.Debugf("backend: got »%s«: %v %v", addr, roundtrip, err)

			if err != nil {
				p.err = err
			} else {
				p.err = nil
				p.lastSeen = time.Now()
				p.roundtrip = roundtrip
			}

			p.mu.Unlock()
		}
	}
}

// TODO: Make a PR with those functions.
func ping(sh *shell.Shell, peerID string) (time.Duration, error) {
	ctx := context.Background()
	resp, err := sh.Request("ping", peerID).Send(ctx)
	if err != nil {
		return 0, err
	}

	defer resp.Close()

	if resp.Error != nil {
		return 0, resp.Error
	}

	raw := struct {
		Success bool
		Time    int64
	}{}

	if err := json.NewDecoder(resp.Output).Decode(&raw); err != nil {
		return 0, err
	}

	if raw.Success {
		return time.Duration(raw.Time), nil
	}

	return 0, fmt.Errorf("no ping")
}

var ErrWaiting = errors.New("waiting for route")

func (nd *Node) Ping(addr string) (netBackend.Pinger, error) {
	if !nd.allowNetOps {
		return nil, ErrOffline
	}

	log.Debugf("backend: start ping »%s«", addr)
	p := &pinger{
		nd:  nd,
		err: ErrWaiting,
	}

	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	go p.Run(ctx, addr)
	return p, nil
}
