package bindings

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/blang/semver/v4"
	"github.com/containers/common/pkg/ssh"
	"github.com/containers/podman/v5/version"
	"github.com/kevinburke/ssh_config"
	"github.com/sirupsen/logrus"
	ssh2 "golang.org/x/crypto/ssh"
	"golang.org/x/net/proxy"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/net/swarm"
	"github.com/libp2p/go-libp2p/p2p/protocol/ping"
	"github.com/multiformats/go-multiaddr"
	ma "github.com/multiformats/go-multiaddr"
)

type APIResponse struct {
	*http.Response
	Request *http.Request
}

type Connection struct {
	URI    *url.URL
	Client *http.Client
}

type valueKey string

const (
	clientKey  = valueKey("Client")
	versionKey = valueKey("ServiceVersion")
)

type ConnectError struct {
	Err error
}

func (c ConnectError) Error() string {
	return "unable to connect to Podman socket: " + c.Err.Error()
}

func (c ConnectError) Unwrap() error {
	return c.Err
}

func newConnectError(err error) error {
	return ConnectError{Err: err}
}

const protocolID = "/p2pdao/libp2p-ssh/1.0.0"
const serviceName = "p2pdao.libp2p-proxy"

type MultiaddrAsNetAddr struct {
	maddr multiaddr.Multiaddr
}

// Network returns a fixed string identifying the network.
func (m MultiaddrAsNetAddr) Network() string {
	return "libp2p"
}

// String returns the string representation of the multiaddr.
func (m MultiaddrAsNetAddr) String() string {
	return m.maddr.String()
}

// Libp2pConn wraps a libp2p stream to implement net.Conn
type Libp2pConn struct {
	stream network.Stream
}

func (c *Libp2pConn) Read(b []byte) (int, error) {
	return c.stream.Read(b)
}

func (c *Libp2pConn) Write(b []byte) (int, error) {
	return c.stream.Write(b)
}

func (c *Libp2pConn) Close() error {
	return c.stream.Close()
}

func (c *Libp2pConn) LocalAddr() net.Addr {
	return MultiaddrAsNetAddr{maddr: c.stream.Conn().LocalMultiaddr()}
}

func (c *Libp2pConn) RemoteAddr() net.Addr {
	return MultiaddrAsNetAddr{maddr: c.stream.Conn().RemoteMultiaddr()}
}

func (c *Libp2pConn) SetDeadline(t time.Time) error      { return nil }
func (c *Libp2pConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *Libp2pConn) SetWriteDeadline(t time.Time) error { return nil }

// createLibp2pHost creates a libp2p host
func createLibp2pHost() (host.Host, error) {
	return libp2p.New(
		libp2p.NoListenAddrs,
		libp2p.UserAgent(serviceName),
		// Usually EnableRelay() is not required as it is enabled by default
		// but NoListenAddrs overrides this, so we're adding it in explicitly again.
		libp2p.EnableRelay(),
	)
}

// connectToLibp2pPeer connects to a libp2p peer and opens a stream
func connectToLibp2pPeer(ctx context.Context, host host.Host, relayAddress string, peerStr string) (*Libp2pConn, error) {

	// The multiaddress string
	multiAddrStr := "/ip4/64.176.227.5/tcp/4001/p2p/12D3KooWLzi9E1oaHLhWrgTPnPa3aUjNkM8vvC8nYZp1gk9RjTV1"

	// Parse the multiaddress
	multiAddr, err := multiaddr.NewMultiaddr(multiAddrStr)
	if err != nil {
		log.Fatalf("Failed to parse multiaddress: %v", err)
	}

	// Extract AddrInfo from the multiaddress
	relay1info, err := peer.AddrInfoFromP2pAddr(multiAddr)
	if err != nil {
		log.Fatalf("Failed to extract AddrInfo: %v", err)
	}

	serverPeer1, err := peer.AddrInfoFromString("/ip4/64.176.227.5/tcp/11212/ws/p2p/12D3KooWJJqoWuC2CVAuUfEfdLHguh1bPbKsLwQY4SoC2Vw695ry")
	if err != nil {
		fmt.Println(err)
	}

	// Register a connection notification handler
	host.Network().Notify(&network.NotifyBundle{
		ConnectedF: func(n network.Network, conn network.Conn) {
			addr := conn.RemoteMultiaddr()

			if addr.String() == "" {
				fmt.Println("No multiaddr found for connection.")
				return
			}

			// Check if the connection is using a relay

			fmt.Printf("Connected via relay: %s\n", addr)

		},
		DisconnectedF: func(_ network.Network, conn network.Conn) {
			if conn.RemotePeer() == relay1info.ID {
				fmt.Println("Lost connection to relay. Reconnecting...")
			}
		},
	})

	// Connect both unreachable1 and unreachable2 to relay1
	if err := host.Connect(context.Background(), *relay1info); err != nil {
		log.Printf("Failed to connect unreachable1 and relay1: %v", err)
	}

	fmt.Printf("Peer ID: %s\n", host.ID())

	// Now create a new address for unreachable2 that specifies to communicate via
	// relay1 using a circuit relay
	serverPeer, err := ma.NewMultiaddr("/p2p/" + relay1info.ID.String() + "/p2p-circuit/p2p/" + serverPeer1.ID.String())
	if err != nil {
		log.Println(err)
	} else {
		log.Println(serverPeer)
	}

	// Since we just tried and failed to dial, the dialer system will, by default
	// prevent us from redialing again so quickly. Since we know what we're doing, we
	// can use this ugly hack (it's on our TODO list to make it a little cleaner)
	// to tell the dialer "no, its okay, let's try this again"
	host.Network().(*swarm.Swarm).Backoff().Clear(serverPeer1.ID)

	log.Println("Now let's attempt to connect the hosts via the relay node")

	// Open a connection to the previously unreachable host via the relay address
	unreachable2relayinfo := peer.AddrInfo{
		ID:    serverPeer1.ID,
		Addrs: []ma.Multiaddr{serverPeer},
	}
	// host.Peerstore().AddAddrs(serverPeer.ID, serverPeer.Addrs, peerstore.PermanentAddrTTL)
	ctxt, cancel := context.WithTimeout(ctx, time.Second*15)

	if err := host.Connect(context.Background(), unreachable2relayinfo); err != nil {
		log.Printf("Unexpected error here. Failed to connect unreachable1 and unreachable2: %v", err)
	}

	log.Println("Yep, that worked!")

	res := <-ping.Ping(ctxt, host, serverPeer1.ID)
	if res.Error != nil {
		log.Fatalf("ping error: %v", res.Error)
	} else {
		log.Printf("ping RTT: %s", res.RTT)
	}
	cancel()
	host.ConnManager().Protect(serverPeer1.ID, "proxy")

	stream, err := host.NewStream(network.WithAllowLimitedConn(ctx, protocolID), serverPeer1.ID, protocolID)
	if err != nil {
		return nil, fmt.Errorf("failed to open stream: %v", err)
	}

	return &Libp2pConn{stream: stream}, nil
}

// createSSHClient creates an SSH client using the libp2p connection
func createSSHClient(libp2pConn net.Conn, username string, privateKey []byte) (*ssh2.Client, error) {
	signer, err := ssh2.ParsePrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to parse private key: %v", err)
	}

	clientConfig := &ssh2.ClientConfig{
		User: username,
		Auth: []ssh2.AuthMethod{
			ssh2.PublicKeys(signer),
		},
		HostKeyCallback: ssh2.InsecureIgnoreHostKey(),
	}

	clientConn, chans, reqs, err := ssh2.NewClientConn(libp2pConn, "ssh://root@127.0.0.1:56503/run/podman/podman.sock", clientConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create SSH client connection: %v", err)
	}

	return ssh2.NewClient(clientConn, chans, reqs), nil
}

// GetClient from context build by NewConnection()
func GetClient(ctx context.Context) (*Connection, error) {
	if c, ok := ctx.Value(clientKey).(*Connection); ok {
		return c, nil
	}
	return nil, fmt.Errorf("%s not set in context", clientKey)
}

// ServiceVersion from context build by NewConnection()
func ServiceVersion(ctx context.Context) *semver.Version {
	if v, ok := ctx.Value(versionKey).(*semver.Version); ok {
		return v
	}
	return new(semver.Version)
}

// JoinURL elements with '/'
func JoinURL(elements ...string) string {
	return "/" + strings.Join(elements, "/")
}

// NewConnection creates a new service connection without an identity
func NewConnection(ctx context.Context, uri string) (context.Context, error) {
	return NewConnectionWithIdentity(ctx, uri, "", false)
}

// NewConnectionWithIdentity takes a URI as a string and returns a context with the
// Connection embedded as a value.  This context needs to be passed to each
// endpoint to work correctly.
//
// A valid URI connection should be scheme://
// For example tcp://localhost:<port>
// or unix:///run/podman/podman.sock
// or ssh://<user>@<host>[:port]/run/podman/podman.sock
func NewConnectionWithIdentity(ctx context.Context, uri string, identity string, machine bool) (context.Context, error) {
	var (
		err error
	)
	if v, found := os.LookupEnv("CONTAINER_HOST"); found && uri == "" {
		uri = v
	}

	if v, found := os.LookupEnv("CONTAINER_SSHKEY"); found && len(identity) == 0 {
		identity = v
	}

	_url, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("value of CONTAINER_HOST is not a valid url: %s: %w", uri, err)
	}

	// Now we set up the http Client to use the connection above
	var connection Connection
	switch _url.Scheme {
	case "ssh":
		conn, err := sshClient(_url, uri, identity, machine)
		if err != nil {
			return nil, err
		}
		connection = conn
	case "p2p":
		conn, err := p2pClient(_url, uri, identity, machine)
		if err != nil {
			return nil, err
		}
		connection = conn
	case "unix":
		if !strings.HasPrefix(uri, "unix:///") {
			// autofix unix://path_element vs unix:///path_element
			_url.Path = JoinURL(_url.Host, _url.Path)
			_url.Host = ""
		}
		connection = unixClient(_url)
	case "tcp":
		if !strings.HasPrefix(uri, "tcp://") {
			return nil, errors.New("tcp URIs should begin with tcp://")
		}
		conn, err := tcpClient(_url)
		if err != nil {
			return nil, newConnectError(err)
		}
		connection = conn
	default:
		return nil, fmt.Errorf("unable to create connection. %q is not a supported schema", _url.Scheme)
	}

	ctx = context.WithValue(ctx, clientKey, &connection)
	serviceVersion, err := pingNewConnection(ctx)
	if err != nil {
		return nil, newConnectError(err)
	}
	ctx = context.WithValue(ctx, versionKey, serviceVersion)
	return ctx, nil
}

func p2pClient(_url *url.URL, uri string, identity string, machine bool) (Connection, error) {
	var (
		err   error
		port  int
		alias string
	)
	connection := Connection{
		URI: _url,
	}
	userinfo := _url.User

	if _url.Port() != "" {
		port, err = strconv.Atoi(_url.Port())
		if err != nil {
			return connection, err
		}
	}

	alias = _url.Hostname()
	// only parse ssh_config when we are not connecting to a machine
	// For machine connections we always have the full URL in the
	// system connection so reading the file is just unnecessary.
	if !machine {
		cfg := ssh_config.DefaultUserSettings
		cfg.IgnoreErrors = true
		found := false

		if userinfo == nil {
			if val := cfg.Get(alias, "User"); val != "" {
				userinfo = url.User(val)
				found = true
			}
		}
		// not in url or ssh_config so default to current user
		if userinfo == nil {
			u, err := user.Current()
			if err != nil {
				return connection, fmt.Errorf("current user could not be determined: %w", err)
			}
			userinfo = url.User(u.Username)
		}

		if val := cfg.Get(alias, "Hostname"); val != "" {
			uri = val
			found = true
		}

		if port == 0 {
			if val := cfg.Get(alias, "Port"); val != "" {
				if val != ssh_config.Default("Port") {
					port, err = strconv.Atoi(val)
					if err != nil {
						return connection, fmt.Errorf("port is not an int: %s: %w", val, err)
					}
					found = true
				}
			}
		}
		// not in ssh config or url so use default 22 port
		if port == 0 {
			port = 22
		}

		if identity == "" {
			if val := cfg.Get(alias, "IdentityFile"); val != "" {
				identity = strings.Trim(val, "\"")
				if strings.HasPrefix(identity, "~/") {
					homedir, err := os.UserHomeDir()
					if err != nil {
						return connection, fmt.Errorf("failed to find home dir: %w", err)
					}
					identity = filepath.Join(homedir, identity[2:])
				}
				found = true
			}
		}

		if found {
			logrus.Debugf("ssh_config alias found: %s", alias)
			logrus.Debugf("  User: %s", userinfo.Username())
			logrus.Debugf("  Hostname: %s", uri)
			logrus.Debugf("  Port: %d", port)
			logrus.Debugf("  IdentityFile: %q", identity)
		}
	}

	// use libp2p connect as underlying network stream
	ctx := context.Background()

	// Step 1: Create the libp2p host
	host, err := createLibp2pHost()
	if err != nil {
		log.Fatalf("Failed to create libp2p host: %v", err)
	}
	defer host.Close()

	// Replace with your server's libp2p multiaddress
	serverMultiAddr := "/ip4/64.176.227.5/tcp/4001/p2p/12D3KooWLzi9E1oaHLhWrgTPnPa3aUjNkM8vvC8nYZp1gk9RjTV1"

	// Step 2: Connect to the libp2p peer
	libp2pConn, err := connectToLibp2pPeer(ctx, host, serverMultiAddr, _url.Hostname())
	if err != nil {
		log.Fatalf("Failed to connect to libp2p peer: %v", err)
	}

	// Step 3: Create an SSH client
	key, err := os.ReadFile(identity)
	if err != nil {
		return connection, err
	}

	conn, err := createSSHClient(libp2pConn, "core", key)
	if err != nil {
		log.Fatalf("Failed to create SSH client: %v", err)
	}
	defer conn.Close()
	if err != nil {
		return connection, newConnectError(err)
	}
	if _url.Path == "" {
		session, err := conn.NewSession()
		if err != nil {
			return connection, err
		}
		defer session.Close()

		var b bytes.Buffer
		session.Stdout = &b
		if err := session.Run(
			"podman info --format '{{.Host.RemoteSocket.Path}}'"); err != nil {
			return connection, err
		}
		val := strings.TrimSuffix(b.String(), "\n")
		_url.Path = val
	}
	dialContext := func(ctx context.Context, _, _ string) (net.Conn, error) {
		return ssh.DialNet(conn, "unix", _url)
	}
	connection.Client = &http.Client{
		Transport: &http.Transport{
			DialContext: dialContext,
		}}
	return connection, nil
}

func sshClient(_url *url.URL, uri string, identity string, machine bool) (Connection, error) {
	var (
		err  error
		port int
	)
	connection := Connection{
		URI: _url,
	}
	userinfo := _url.User

	if _url.Port() != "" {
		port, err = strconv.Atoi(_url.Port())
		if err != nil {
			return connection, err
		}
	}

	// only parse ssh_config when we are not connecting to a machine
	// For machine connections we always have the full URL in the
	// system connection so reading the file is just unnecessary.
	if !machine {
		alias := _url.Hostname()
		cfg := ssh_config.DefaultUserSettings
		cfg.IgnoreErrors = true
		found := false

		if userinfo == nil {
			if val := cfg.Get(alias, "User"); val != "" {
				userinfo = url.User(val)
				found = true
			}
		}
		// not in url or ssh_config so default to current user
		if userinfo == nil {
			u, err := user.Current()
			if err != nil {
				return connection, fmt.Errorf("current user could not be determined: %w", err)
			}
			userinfo = url.User(u.Username)
		}

		if val := cfg.Get(alias, "Hostname"); val != "" {
			uri = val
			found = true
		}

		if port == 0 {
			if val := cfg.Get(alias, "Port"); val != "" {
				if val != ssh_config.Default("Port") {
					port, err = strconv.Atoi(val)
					if err != nil {
						return connection, fmt.Errorf("port is not an int: %s: %w", val, err)
					}
					found = true
				}
			}
		}
		// not in ssh config or url so use default 22 port
		if port == 0 {
			port = 22
		}

		if identity == "" {
			if val := cfg.Get(alias, "IdentityFile"); val != "" {
				identity = strings.Trim(val, "\"")
				if strings.HasPrefix(identity, "~/") {
					homedir, err := os.UserHomeDir()
					if err != nil {
						return connection, fmt.Errorf("failed to find home dir: %w", err)
					}
					identity = filepath.Join(homedir, identity[2:])
				}
				found = true
			}
		}

		if found {
			logrus.Debugf("ssh_config alias found: %s", alias)
			logrus.Debugf("  User: %s", userinfo.Username())
			logrus.Debugf("  Hostname: %s", uri)
			logrus.Debugf("  Port: %d", port)
			logrus.Debugf("  IdentityFile: %q", identity)
		}
	}
	conn, err := ssh.Dial(&ssh.ConnectionDialOptions{
		Host:                        uri,
		Identity:                    identity,
		User:                        userinfo,
		Port:                        port,
		InsecureIsMachineConnection: machine,
	}, ssh.GolangMode)
	if err != nil {
		return connection, newConnectError(err)
	}
	if _url.Path == "" {
		session, err := conn.NewSession()
		if err != nil {
			return connection, err
		}
		defer session.Close()

		var b bytes.Buffer
		session.Stdout = &b
		if err := session.Run(
			"podman info --format '{{.Host.RemoteSocket.Path}}'"); err != nil {
			return connection, err
		}
		val := strings.TrimSuffix(b.String(), "\n")
		_url.Path = val
	}
	dialContext := func(ctx context.Context, _, _ string) (net.Conn, error) {
		return ssh.DialNet(conn, "unix", _url)
	}
	connection.Client = &http.Client{
		Transport: &http.Transport{
			DialContext: dialContext,
		}}
	return connection, nil
}

func tcpClient(_url *url.URL) (Connection, error) {
	connection := Connection{
		URI: _url,
	}
	dialContext := func(ctx context.Context, _, _ string) (net.Conn, error) {
		return net.Dial("tcp", _url.Host)
	}
	// use proxy if env `CONTAINER_PROXY` set
	if proxyURI, found := os.LookupEnv("CONTAINER_PROXY"); found {
		proxyURL, err := url.Parse(proxyURI)
		if err != nil {
			return connection, fmt.Errorf("value of CONTAINER_PROXY is not a valid url: %s: %w", proxyURI, err)
		}
		proxyDialer, err := proxy.FromURL(proxyURL, proxy.Direct)
		if err != nil {
			return connection, fmt.Errorf("unable to dial to proxy %s, %w", proxyURI, err)
		}
		dialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			logrus.Debugf("use proxy %s, but proxy dialer does not support dial timeout", proxyURI)
			return proxyDialer.Dial("tcp", _url.Host)
		}
		if f, ok := proxyDialer.(proxy.ContextDialer); ok {
			dialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
				// the default tcp dial timeout seems to be 75s, podman-remote will retry 3 times before exit.
				// here we change proxy dial timeout to 3s
				logrus.Debugf("use proxy %s with dial timeout 3s", proxyURI)
				ctx, cancel := context.WithTimeout(ctx, time.Second*3)
				defer cancel() // It's safe to cancel, `f.DialContext` only use ctx for returning the Conn, not the lifetime of the Conn.
				return f.DialContext(ctx, "tcp", _url.Host)
			}
		}
	}
	connection.Client = &http.Client{
		Transport: &http.Transport{
			DialContext:        dialContext,
			DisableCompression: true,
		},
	}
	return connection, nil
}

// pingNewConnection pings to make sure the RESTFUL service is up
// and running. it should only be used when initializing a connection
func pingNewConnection(ctx context.Context) (*semver.Version, error) {
	client, err := GetClient(ctx)
	if err != nil {
		return nil, err
	}
	// the ping endpoint sits at / in this case
	response, err := client.DoRequest(ctx, nil, http.MethodGet, "/_ping", nil, nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusOK {
		versionHdr := response.Header.Get("Libpod-API-Version")
		if versionHdr == "" {
			logrus.Warn("Service did not provide Libpod-API-Version Header")
			return new(semver.Version), nil
		}
		versionSrv, err := semver.ParseTolerant(versionHdr)
		if err != nil {
			return nil, err
		}

		switch version.APIVersion[version.Libpod][version.MinimalAPI].Compare(versionSrv) {
		case -1, 0:
			// Server's job when Client version is equal or older
			return &versionSrv, nil
		case 1:
			return nil, fmt.Errorf("server API version is too old. Client %q server %q",
				version.APIVersion[version.Libpod][version.MinimalAPI].String(), versionSrv.String())
		}
	}
	return nil, fmt.Errorf("ping response was %d", response.StatusCode)
}

func unixClient(_url *url.URL) Connection {
	connection := Connection{URI: _url}
	connection.Client = &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", _url.Path)
			},
			DisableCompression: true,
		},
	}
	return connection
}

// DoRequest assembles the http request and returns the response.
// The caller must close the response body.
func (c *Connection) DoRequest(ctx context.Context, httpBody io.Reader, httpMethod, endpoint string, queryParams url.Values, headers http.Header, pathValues ...string) (*APIResponse, error) {
	var (
		err      error
		response *http.Response
	)

	params := make([]interface{}, len(pathValues)+1)

	if v := headers.Values("API-Version"); len(v) > 0 {
		params[0] = v[0]
	} else {
		// Including the semver suffices breaks older services... so do not include them
		v := version.APIVersion[version.Libpod][version.CurrentAPI]
		params[0] = fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	}

	for i, pv := range pathValues {
		// url.URL lacks the semantics for escaping embedded path parameters... so we manually
		//   escape each one and assume the caller included the correct formatting in "endpoint"
		params[i+1] = url.PathEscape(pv)
	}

	baseURL := "http://d"
	if c.URI.Scheme == "tcp" {
		// Allow path prefixes for tcp connections to match Docker behavior
		baseURL = "http://" + c.URI.Host + c.URI.Path
	}
	uri := fmt.Sprintf(baseURL+"/v%s/libpod"+endpoint, params...)
	logrus.Debugf("DoRequest Method: %s URI: %v", httpMethod, uri)

	req, err := http.NewRequestWithContext(ctx, httpMethod, uri, httpBody)
	if err != nil {
		return nil, err
	}
	if len(queryParams) > 0 {
		req.URL.RawQuery = queryParams.Encode()
	}

	for key, val := range headers {
		if key == "API-Version" {
			continue
		}

		for _, v := range val {
			req.Header.Add(key, v)
		}
	}

	// Give the Do three chances in the case of a comm/service hiccup
	for i := 1; i <= 3; i++ {
		response, err = c.Client.Do(req) //nolint:bodyclose // The caller has to close the body.
		if err == nil {
			break
		}
		time.Sleep(time.Duration(i*100) * time.Millisecond)
	}
	return &APIResponse{response, req}, err
}

// GetDialer returns raw Transport.DialContext from client
func (c *Connection) GetDialer(ctx context.Context) (net.Conn, error) {
	client := c.Client
	transport := client.Transport.(*http.Transport)
	if transport.DialContext != nil && transport.TLSClientConfig == nil {
		return transport.DialContext(ctx, c.URI.Scheme, c.URI.String())
	}

	return nil, errors.New("unable to get dial context")
}

// IsInformational returns true if the response code is 1xx
func (h *APIResponse) IsInformational() bool {
	//nolint:usestdlibvars // linter wants to use http.StatusContinue over 100 but that makes less readable IMO
	return h.Response.StatusCode/100 == 1
}

// IsSuccess returns true if the response code is 2xx
func (h *APIResponse) IsSuccess() bool {
	//nolint:usestdlibvars // linter wants to use http.StatusContinue over 100 but that makes less readable IMO
	return h.Response.StatusCode/100 == 2
}

// IsRedirection returns true if the response code is 3xx
func (h *APIResponse) IsRedirection() bool {
	//nolint:usestdlibvars // linter wants to use http.StatusContinue over 100 but that makes less readable IMO
	return h.Response.StatusCode/100 == 3
}

// IsClientError returns true if the response code is 4xx
func (h *APIResponse) IsClientError() bool {
	//nolint:usestdlibvars // linter wants to use http.StatusContinue over 100 but that makes less readable IMO
	return h.Response.StatusCode/100 == 4
}

// IsConflictError returns true if the response code is 409
func (h *APIResponse) IsConflictError() bool {
	return h.Response.StatusCode == http.StatusConflict
}

// IsServerError returns true if the response code is 5xx
func (h *APIResponse) IsServerError() bool {
	//nolint:usestdlibvars // linter wants to use http.StatusContinue over 100 but that makes less readable IMO
	return h.Response.StatusCode/100 == 5
}
