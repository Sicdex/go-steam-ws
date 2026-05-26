package main

import (
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/sicdex/go-steam-ws"
	"github.com/sicdex/go-steam-ws/protocol/steamlang"
	"golang.org/x/net/proxy"

	"bufio"
)

func main() {
	var (
		username string
		password string
		proxyURL string
		timeout  time.Duration
		forceCM  string
		listOnly bool
		only443  bool
	)
	flag.StringVar(&username, "user", "", "Steam username (required for login)")
	flag.StringVar(&password, "pass", "", "Steam password (required for login)")
	flag.StringVar(&proxyURL, "proxy", "", "optional SOCKS5/HTTP(S) proxy URL — same format as gc-game-parser uses")
	flag.DurationVar(&timeout, "timeout", 60*time.Second, "abort if LoggedOnEvent hasn't arrived by then")
	flag.StringVar(&forceCM, "cm", "", "skip the directory lookup and use this host:port CM (must be a websocket-type CM)")
	flag.BoolVar(&listOnly, "list", false, "just fetch and print the websocket-capable CM list, then exit")
	flag.BoolVar(&only443, "only-443", false, "restrict CM selection to port 443 (mandatory when going through HTTP-CONNECT proxies that only allow CONNECT :443)")
	flag.Parse()

	var cmEndpoint string
	if forceCM != "" {
		cmEndpoint = forceCM
	} else {
		cms, err := steam.FetchCMListForConnect(0)
		if err != nil {
			log.Fatalf("FetchCMListForConnect: %v", err)
		}
		ws := steam.FilterByType(cms, "websockets")
		if only443 {
			out := ws[:0:0]
			for _, cm := range ws {
				if strings.HasSuffix(cm.Endpoint, ":443") {
					out = append(out, cm)
				}
			}
			ws = out
		}
		if listOnly {
			fmt.Printf("websocket CMs (%d total):\n", len(ws))
			for i, cm := range ws {
				if i >= 30 {
					fmt.Println("  ...")
					break
				}
				fmt.Printf("  %-50s dc=%-6s load=%d\n", cm.Endpoint, cm.DC, cm.Load)
			}
			return
		}
		if len(ws) == 0 {
			log.Fatal("no websocket-type CMs returned by directory")
		}
		cm := steam.PickRandom(ws)
		cmEndpoint = cm.Endpoint
		log.Printf("picked CM: %s (dc=%s load=%d, %d candidates)", cm.Endpoint, cm.DC, cm.Load, len(ws))
	}
	if username == "" || password == "" {
		log.Fatal("-user and -pass are required (use -list to skip auth)")
	}

	client := steam.NewClient()
	if proxyURL != "" {
		dialer, err := makeDialer(proxyURL)
		if err != nil {
			log.Fatalf("proxy dialer: %v", err)
		}
		client.Dialer = dialer
		log.Printf("proxy: %s", proxyURL)
	}

	log.Printf("dialing wss://%s/cmsocket/ ...", cmEndpoint)
	t0 := time.Now()
	if err := client.ConnectToWebSocket(cmEndpoint); err != nil {
		log.Fatalf("ConnectToWebSocket: %v", err)
	}
	log.Printf("WS tunnel up after %s; waiting for Steam channel-encryption handshake", time.Since(t0).Round(time.Millisecond))

	deadline := time.After(timeout)
	for {
		select {
		case evt := <-client.Events():
			switch e := evt.(type) {
			case *steam.ConnectedEvent:
				log.Printf("=> ConnectedEvent (channel encrypted) at %s — sending LogOn", time.Since(t0).Round(time.Millisecond))
				client.Auth.LogOn(&steam.LogOnDetails{
					Username: username,
					Password: password,
				})
			case *steam.LoggedOnEvent:
				if e.Result == steamlang.EResult_OK {
					log.Printf("=> LoggedOnEvent OK at %s, steam_id=%d session_id=%d cell=%d",
						time.Since(t0).Round(time.Millisecond), e.ClientSteamId, client.SessionId(), e.CellId)
					log.Println("SUCCESS — go-steam-ws WebSocket path works end-to-end")
					client.Disconnect()
					return
				}
				log.Fatalf("=> LoggedOnEvent FAILED result=%v (extended=%v)", e.Result, e.ExtendedResult)
			case *steam.LogOnFailedEvent:
				log.Fatalf("=> LogOnFailedEvent: %v", e.Result)
			case *steam.LoggedOffEvent:
				log.Fatalf("=> LoggedOffEvent: %v", e.Result)
			case *steam.DisconnectedEvent:
				log.Fatal("=> DisconnectedEvent before login completed")
			case steam.FatalErrorEvent:
				log.Fatalf("=> FatalErrorEvent: %v", error(e))
			case error:
				log.Printf("(non-fatal) %v", e)
			default:
				log.Printf("(event) %T: %+v", e, e)
			}
		case <-deadline:
			log.Fatalf("timed out after %s waiting for LoggedOnEvent", timeout)
		}
	}
}

func makeDialer(proxyURL string) (steam.Dialer, error) {
	if proxyURL == "" {
		return nil, nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("parse proxy_url: %w", err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("proxy_url has no host")
	}
	switch strings.ToLower(u.Scheme) {
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if u.User != nil {
			pass, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: pass}
		}
		sd, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("socks5 setup: %w", err)
		}
		return func(network, addr string) (net.Conn, error) {
			return sd.Dial(network, addr)
		}, nil
	case "http", "https":
		return httpConnectDialer(u), nil
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
}

func httpConnectDialer(u *url.URL) steam.Dialer {
	proxyHost := u.Host
	var authHeader string
	if u.User != nil {
		user := u.User.Username()
		pass, _ := u.User.Password()
		authHeader = "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
	}
	return func(network, addr string) (net.Conn, error) {
		log.Printf("proxy: CONNECT %s via %s", addr, proxyHost)
		conn, err := net.DialTimeout(network, proxyHost, 30*time.Second)
		if err != nil {
			return nil, fmt.Errorf("dial proxy: %w", err)
		}
		var req strings.Builder
		fmt.Fprintf(&req, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", addr, addr)
		if authHeader != "" {
			fmt.Fprintf(&req, "Proxy-Authorization: %s\r\n", authHeader)
		}
		req.WriteString("\r\n")
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
		if _, err := conn.Write([]byte(req.String())); err != nil {
			conn.Close()
			return nil, fmt.Errorf("write CONNECT: %w", err)
		}
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("read CONNECT response: %w", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			brdErr := resp.Header.Get("x-brd-err-code")
			conn.Close()
			if brdErr != "" {
				return nil, fmt.Errorf("proxy CONNECT %s failed: %s (brd-err=%s)", addr, resp.Status, brdErr)
			}
			return nil, fmt.Errorf("proxy CONNECT %s failed: %s", addr, resp.Status)
		}
		_ = conn.SetDeadline(time.Time{})
		if br.Buffered() > 0 {
			return &bufConn{Reader: br, Conn: conn}, nil
		}
		return conn, nil
	}
}

type bufConn struct {
	*bufio.Reader
	net.Conn
}

func (b *bufConn) Read(p []byte) (int, error) { return b.Reader.Read(p) }

// keep os imported even if unused for the explicit -h handler one day
var _ = os.Args
