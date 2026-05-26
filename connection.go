package steam

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/sicdex/go-steam-ws/cryptoutil"
	"github.com/sicdex/go-steam-ws/protocol"
)

type connection interface {
	Read() (*protocol.Packet, error)
	Write([]byte) error
	Close() error
	SetEncryptionKey([]byte)
	IsEncrypted() bool
}

// Dialer matches net.Dial's signature so callers can route Steam
// connections through a SOCKS5/HTTP-CONNECT proxy or any custom
// transport. Pass via Client.Dialer before calling Connect().
//
// When Dialer is set on the Client, dialTCP delegates to it instead of
// using net.DialTCP. The proxy is responsible for reaching the
// destination CM by host:port.
type Dialer func(network, address string) (net.Conn, error)

const tcpConnectionMagic uint32 = 0x31305456 // "VT01"

type tcpConnection struct {
	conn        net.Conn
	ciph        cipher.Block
	cipherMutex sync.RWMutex
}

// dialTCP opens a TCP connection to the CM. If dialer is non-nil, it
// takes precedence (typical for SOCKS5/HTTP-CONNECT proxies); laddr is
// then ignored because the proxy owns its own routing. With nil dialer
// and non-nil laddr we use net.DialTCP for source-IP binding; otherwise
// plain net.Dial.
func dialTCP(laddr, raddr *net.TCPAddr, dialer Dialer) (*tcpConnection, error) {
	var conn net.Conn
	var err error
	switch {
	case dialer != nil:
		conn, err = dialer("tcp", raddr.String())
	case laddr != nil:
		conn, err = net.DialTCP("tcp", laddr, raddr)
	default:
		conn, err = net.Dial("tcp", raddr.String())
	}
	if err != nil {
		return nil, err
	}
	return &tcpConnection{conn: conn}, nil
}

func (c *tcpConnection) Read() (*protocol.Packet, error) {
	// All packets begin with a packet length
	var packetLen uint32
	err := binary.Read(c.conn, binary.LittleEndian, &packetLen)
	if err != nil {
		return nil, err
	}

	// A magic value follows for validation
	var packetMagic uint32
	err = binary.Read(c.conn, binary.LittleEndian, &packetMagic)
	if err != nil {
		return nil, err
	}
	if packetMagic != tcpConnectionMagic {
		return nil, fmt.Errorf("Invalid connection magic! Expected %d, got %d!", tcpConnectionMagic, packetMagic)
	}

	buf := make([]byte, packetLen, packetLen)
	_, err = io.ReadFull(c.conn, buf)
	if err == io.ErrUnexpectedEOF {
		return nil, io.EOF
	}
	if err != nil {
		return nil, err
	}

	// Packets after ChannelEncryptResult are encrypted
	c.cipherMutex.RLock()
	if c.ciph != nil {
		buf = cryptoutil.SymmetricDecrypt(c.ciph, buf)
	}
	c.cipherMutex.RUnlock()

	return protocol.NewPacket(buf)
}

// Writes a message. This may only be used by one goroutine at a time.
func (c *tcpConnection) Write(message []byte) error {
	c.cipherMutex.RLock()
	if c.ciph != nil {
		message = cryptoutil.SymmetricEncrypt(c.ciph, message)
	}
	c.cipherMutex.RUnlock()

	err := binary.Write(c.conn, binary.LittleEndian, uint32(len(message)))
	if err != nil {
		return err
	}
	err = binary.Write(c.conn, binary.LittleEndian, tcpConnectionMagic)
	if err != nil {
		return err
	}

	_, err = c.conn.Write(message)
	return err
}

func (c *tcpConnection) Close() error {
	return c.conn.Close()
}

func (c *tcpConnection) SetEncryptionKey(key []byte) {
	c.cipherMutex.Lock()
	defer c.cipherMutex.Unlock()
	if key == nil {
		c.ciph = nil
		return
	}
	if len(key) != 32 {
		panic("Connection AES key is not 32 bytes long!")
	}

	var err error
	c.ciph, err = aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
}

func (c *tcpConnection) IsEncrypted() bool {
	c.cipherMutex.RLock()
	defer c.cipherMutex.RUnlock()
	return c.ciph != nil
}
