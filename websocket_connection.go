package steam

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sicdex/go-steam-ws/protocol"
)

type wsConnection struct {
	ws         *websocket.Conn
	writeMutex sync.Mutex // gorilla/websocket needs serialized writes
}

func dialWebSocket(host string, dialer Dialer, handshakeTimeout time.Duration) (*wsConnection, error) {
	if handshakeTimeout <= 0 {
		handshakeTimeout = 45 * time.Second
	}
	u := url.URL{
		Scheme: "wss",
		Host:   host,
		Path:   "/cmsocket/",
	}
	d := &websocket.Dialer{
		HandshakeTimeout: handshakeTimeout,
		ReadBufferSize:   64 * 1024,
		WriteBufferSize:  64 * 1024,
	}
	if dialer != nil {
		d.NetDial = func(network, addr string) (net.Conn, error) {
			return dialer(network, addr)
		}
	}
	headers := http.Header{}
	headers.Set("User-Agent", "Valve/Steam HTTP Client 1.0")

	ws, resp, err := d.Dial(u.String(), headers)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("websocket dial %s: %w (HTTP %s)", u.String(), err, resp.Status)
		}
		return nil, fmt.Errorf("websocket dial %s: %w", u.String(), err)
	}
	return &wsConnection{ws: ws}, nil
}

func (c *wsConnection) Read() (*protocol.Packet, error) {
	for {
		msgType, data, err := c.ws.ReadMessage()
		if err != nil {
			return nil, err
		}
		if msgType != websocket.BinaryMessage {
			continue
		}
		return protocol.NewPacket(data)
	}
}

func (c *wsConnection) Write(message []byte) error {
	c.writeMutex.Lock()
	defer c.writeMutex.Unlock()
	return c.ws.WriteMessage(websocket.BinaryMessage, message)
}

func (c *wsConnection) Close() error {
	c.writeMutex.Lock()
	_ = c.ws.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(2*time.Second),
	)
	c.writeMutex.Unlock()
	return c.ws.Close()
}

func (c *wsConnection) SetEncryptionKey([]byte) {}

func (c *wsConnection) IsEncrypted() bool { return true }
